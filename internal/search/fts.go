package search

import (
	"context"
	"fmt"
	"strings"

	"github.com/dorinpuscasu25/recapproject/internal/nlp"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
)

// PostgresFTS is the lexical fallback when OpenSearch is not configured. It queries the
// canonical tables directly (generated tsvector columns), so Index/Delete are no-ops.
type PostgresFTS struct{ DB *db.DB }

func (p *PostgresFTS) Name() string                                 { return "postgres_fts" }
func (p *PostgresFTS) Ensure(context.Context) error                 { return nil }
func (p *PostgresFTS) Index(context.Context, LexicalDoc) error      { return nil }
func (p *PostgresFTS) Delete(context.Context, string, string) error { return nil }

// tsQuery builds an OR-of-prefixes query from unaccented, lower-cased terms so that inflected
// Romanian/Russian forms still match (FTS uses the 'simple' configuration).
func tsQuery(text string) string {
	var parts []string
	seen := map[string]bool{}
	for _, t := range nlp.Tokens(nlp.StripDiacritics(text)) {
		if len([]rune(t)) < 3 || seen[t] {
			continue
		}
		seen[t] = true
		r := []rune(t)
		if len(r) > 5 {
			r = r[:len(r)-2] // crude stemming via prefix match
		}
		clean := strings.Map(func(c rune) rune {
			if c == '\'' || c == ':' || c == '&' || c == '|' || c == '!' || c == '(' || c == ')' || c == '\\' {
				return -1
			}
			return c
		}, string(r))
		if clean != "" {
			parts = append(parts, clean+":*")
		}
		if len(parts) >= 12 {
			break
		}
	}
	return strings.Join(parts, " | ")
}

func (p *PostgresFTS) Search(ctx context.Context, q LexicalQuery) ([]LexicalHit, error) {
	tq := tsQuery(q.Text)
	if tq == "" {
		return nil, nil
	}
	types := q.DocTypes
	if len(types) == 0 {
		types = []string{"event", "article", "claim"}
	}
	var out []LexicalHit
	for _, dt := range types {
		var sql string
		args := []any{tq, q.Limit}
		cond := ""
		addArr := func(expr string, vals []string) {
			if len(vals) > 0 {
				args = append(args, vals)
				cond += fmt.Sprintf(" AND %s && $%d::text[]", expr, len(args))
			}
		}
		addEq := func(expr string, vals []string) {
			if len(vals) > 0 {
				args = append(args, vals)
				cond += fmt.Sprintf(" AND %s = ANY($%d::text[])", expr, len(args))
			}
		}
		addTime := func(expr string) {
			if q.From != nil {
				args = append(args, *q.From)
				cond += fmt.Sprintf(" AND %s >= $%d", expr, len(args))
			}
			if q.To != nil {
				args = append(args, *q.To)
				cond += fmt.Sprintf(" AND %s <= $%d", expr, len(args))
			}
		}
		switch dt {
		case "event":
			addArr("e.languages", q.Languages)
			addArr("ARRAY(SELECT category_id::text FROM event_categories ec WHERE ec.event_id=e.id)", q.CategoryIDs)
			addArr("e.locations", q.Regions)
			addEq("e.state", q.Statuses)
			addArr("ARRAY(SELECT en.normalized_key FROM event_entities ee JOIN entities en ON en.id=ee.entity_id WHERE ee.event_id=e.id)", q.EntityKeys)
			addArr("ARRAY(SELECT a.publisher_id::text FROM event_article_links l JOIN articles a ON a.id=l.article_id WHERE l.event_id=e.id AND l.unlinked_at IS NULL)", q.PublisherIDs)
			addTime("e.last_material_update_at")
			sql = `SELECT e.id::text, ts_rank(e.tsv, to_tsquery('simple', $1)) AS r,
				ts_headline('simple', e.search_text, to_tsquery('simple', $1), 'MaxFragments=1,MaxWords=30,MinWords=10')
				FROM events e WHERE e.status='active' AND e.tsv @@ to_tsquery('simple', $1)` + cond + ` ORDER BY r DESC LIMIT $2`
		case "article":
			addEq("a.language", q.Languages)
			addArr("ARRAY(SELECT category_id::text FROM article_categories ac WHERE ac.article_id=a.id)", q.CategoryIDs)
			addEq("a.publisher_id::text", q.PublisherIDs)
			addArr("a.geographies", q.Regions)
			addArr("ARRAY(SELECT en.normalized_key FROM article_entities ae JOIN entities en ON en.id=ae.entity_id WHERE ae.article_id=a.id)", q.EntityKeys)
			addTime("a.published_at")
			sql = `SELECT a.id::text, ts_rank(a.tsv, to_tsquery('simple', $1)) AS r,
				ts_headline('simple', coalesce(a.description,'') || ' ' || left(coalesce(a.body_text,''), 3000), to_tsquery('simple', $1), 'MaxFragments=1,MaxWords=30,MinWords=10')
				FROM articles a WHERE a.status='enriched' AND a.tombstoned_at IS NULL AND a.tsv @@ to_tsquery('simple', $1)` + cond + ` ORDER BY r DESC LIMIT $2`
		case "claim":
			addEq("c.language", q.Languages)
			addEq("c.status", q.Statuses)
			if len(q.Verdicts) > 0 {
				args = append(args, q.Verdicts)
				cond += fmt.Sprintf(" AND (SELECT verdict FROM claim_verdicts v WHERE v.id=c.current_verdict_id) = ANY($%d::text[])", len(args))
			}
			addTime("c.last_seen_at")
			sql = `SELECT c.id::text, ts_rank(c.tsv, to_tsquery('simple', $1)) AS r, c.canonical_text
				FROM claims c WHERE c.tsv @@ to_tsquery('simple', $1)` + cond + ` ORDER BY r DESC LIMIT $2`
		default:
			continue
		}
		rows, err := p.DB.Pool.Query(ctx, sql, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var h LexicalHit
			if err := rows.Scan(&h.ID, &h.Score, &h.Snippet); err != nil {
				rows.Close()
				return nil, err
			}
			h.DocType = dt
			out = append(out, h)
		}
		rows.Close()
	}
	return out, nil
}
