package enrich

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dorinpuscasu25/recapproject/internal/ingest"
	"github.com/dorinpuscasu25/recapproject/internal/jobs"
	"github.com/dorinpuscasu25/recapproject/internal/platform/bus"
	"github.com/dorinpuscasu25/recapproject/internal/taxonomy"
)

// RunReclassify re-runs category classification over a scoped set of articles. In dry-run mode
// it only reports how assignments would change; otherwise it enqueues reclassification.
func (s *Service) RunReclassify(ctx context.Context, j *jobs.Job) (any, error) {
	var r taxonomy.ReclassifyRequest
	if err := json.Unmarshal(j.Payload, &r); err != nil {
		return nil, jobs.Permanent(err)
	}
	catIDs := r.CategoryIDs
	if catIDs == nil {
		catIDs = []uuid.UUID{}
	}
	rows, err := s.db.Pool.Query(ctx, `SELECT a.id, a.revision FROM articles a WHERE a.status='enriched'
		AND ($1::timestamptz IS NULL OR a.first_seen_at >= $1) AND ($2::timestamptz IS NULL OR a.first_seen_at < $2)
		AND ($3::uuid IS NULL OR a.publisher_id=$3)
		AND (cardinality($4::uuid[])=0 OR EXISTS (SELECT 1 FROM article_categories ac WHERE ac.article_id=a.id AND ac.category_id = ANY($4)))
		ORDER BY a.first_seen_at DESC LIMIT $5`, r.Since, r.Until, r.PublisherID, catIDs, r.Limit)
	if err != nil {
		return nil, err
	}
	type ar struct {
		id  uuid.UUID
		rev int
	}
	var list []ar
	for rows.Next() {
		var x ar
		if err := rows.Scan(&x.id, &x.rev); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, x)
	}
	rows.Close()
	if !r.DryRun {
		err := s.db.Tx(ctx, func(tx pgx.Tx) error {
			for _, x := range list {
				if _, err := bus.Publish(ctx, tx, bus.Draft{Type: bus.ArticleEnrichmentRequested, AggregateType: "article", AggregateID: x.id.String(),
					Payload: ingest.EnrichmentRequested{ArticleID: x.id, Revision: x.rev, Reason: "reclassify"}}); err != nil {
					return err
				}
			}
			return nil
		})
		return map[string]any{"dry_run": false, "enqueued": len(list)}, err
	}
	changed := 0
	var samples []map[string]any
	for _, x := range list {
		a, err := s.loadArticle(ctx, x.id)
		if err != nil {
			return nil, err
		}
		res, err := s.Enrich(ctx, a, true)
		if err != nil {
			return nil, err
		}
		var cur []string
		crow, err := s.db.Pool.Query(ctx, `SELECT c.slug::text FROM article_categories ac JOIN categories c ON c.id=ac.category_id
			WHERE ac.article_id=$1 AND ac.source='ai' AND ac.model_ref NOT LIKE '%+parent'`, x.id)
		if err != nil {
			return nil, err
		}
		cur, _ = pgx.CollectRows(crow, pgx.RowTo[string])
		var next []string
		for _, c := range res.cats {
			next = append(next, c.Slug)
		}
		sort.Strings(cur)
		sort.Strings(next)
		if !equal(cur, next) {
			changed++
			if len(samples) < 50 {
				samples = append(samples, map[string]any{"article_id": x.id, "current": cur, "proposed": next})
			}
		}
	}
	return map[string]any{"dry_run": true, "evaluated": len(list), "would_change": changed, "samples": samples}, nil
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
