package ingest

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dorinpuscasu25/recapproject/internal/audit"
	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/bus"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/principal"
)

type ArticleSummary struct {
	ID            uuid.UUID  `json:"id"`
	PublisherID   uuid.UUID  `json:"publisher_id"`
	Publisher     string     `json:"publisher"`
	Title         string     `json:"title"`
	CanonicalURL  string     `json:"canonical_url"`
	Language      string     `json:"language"`
	Status        string     `json:"status"`
	ContentType   string     `json:"content_type"`
	Revision      int        `json:"revision"`
	DuplicateOf   *uuid.UUID `json:"duplicate_of,omitempty"`
	DuplicateKind *string    `json:"duplicate_kind,omitempty"`
	PublishedAt   *time.Time `json:"published_at,omitempty"`
	FirstSeenAt   time.Time  `json:"first_seen_at"`
	LastError     *string    `json:"last_error,omitempty"`
	BodyChars     int        `json:"body_chars"`
	Categories    []string   `json:"categories"`
	EventID       *uuid.UUID `json:"event_id,omitempty"`
	categories    *string
}

func (s *Service) ListArticles(ctx context.Context, status string, publisher, endpoint *uuid.UUID, q string, limit int) ([]ArticleSummary, error) {
	rows, err := s.db.Pool.Query(ctx, `SELECT a.id, a.publisher_id, p.name, a.title, a.canonical_url, a.language, a.status, a.content_type, a.revision,
		a.duplicate_of, a.duplicate_kind, a.published_at, a.first_seen_at, a.last_error, a.body_chars,
		(SELECT string_agg(c.slug, ',') FROM article_categories ac JOIN categories c ON c.id=ac.category_id WHERE ac.article_id=a.id),
		(SELECT l.event_id FROM event_article_links l WHERE l.article_id=a.id AND l.unlinked_at IS NULL LIMIT 1)
		FROM articles a JOIN publishers p ON p.id=a.publisher_id
		WHERE ($1='' OR a.status=$1) AND ($2::uuid IS NULL OR a.publisher_id=$2) AND ($3='' OR a.title ILIKE '%'||$3||'%')
		  AND ($5::uuid IS NULL OR a.source_endpoint_id=$5)
		ORDER BY a.first_seen_at DESC LIMIT $4`, status, publisher, q, limit, endpoint)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ArticleSummary, error) {
		var x ArticleSummary
		err := r.Scan(&x.ID, &x.PublisherID, &x.Publisher, &x.Title, &x.CanonicalURL, &x.Language, &x.Status, &x.ContentType, &x.Revision,
			&x.DuplicateOf, &x.DuplicateKind, &x.PublishedAt, &x.FirstSeenAt, &x.LastError, &x.BodyChars, &x.categories, &x.EventID)
		if x.categories != nil {
			x.Categories = strings.Split(*x.categories, ",")
		}
		return x, err
	})
}

// ArticleDetail returns provenance-rich article data for editors.
func (s *Service) ArticleDetail(ctx context.Context, id uuid.UUID) (map[string]any, error) {
	var out map[string]any
	var raw []byte
	err := s.db.Pool.QueryRow(ctx, `SELECT row_to_json(x) FROM (
		SELECT a.id, a.publisher_id, a.source_endpoint_id, a.canonical_url, a.original_url, a.language, a.language_confidence, a.title, a.byline,
		  a.description, a.body_chars, a.content_type, a.published_at, a.modified_at, a.first_seen_at, a.fetched_at, a.extractor_version,
		  a.extraction_confidence, a.status, a.duplicate_of, a.duplicate_kind, a.revision, a.is_correction, a.retracted_at, a.tombstoned_at,
		  a.geographies, a.tags, a.enrichment, a.last_error, a.version,
		  (SELECT coalesce(json_agg(json_build_object('revision', revision, 'change_kind', change_kind, 'captured_at', captured_at, 'title', title) ORDER BY revision),'[]')
		     FROM article_versions v WHERE v.article_id=a.id) AS versions,
		  (SELECT coalesce(json_agg(json_build_object('category_id', ac.category_id, 'slug', c.slug, 'source', ac.source, 'is_primary', ac.is_primary,
		     'confidence', ac.confidence, 'model_ref', ac.model_ref, 'taxonomy_version', ac.taxonomy_version)),'[]')
		     FROM article_categories ac JOIN categories c ON c.id=ac.category_id WHERE ac.article_id=a.id) AS categories,
		  (SELECT coalesce(json_agg(json_build_object('id', e.id, 'kind', e.kind, 'name', e.canonical_name, 'mentions', ae.mention_count)),'[]')
		     FROM article_entities ae JOIN entities e ON e.id=ae.entity_id WHERE ae.article_id=a.id) AS entities,
		  (SELECT coalesce(json_agg(json_build_object('type', f.fact_type, 'key', f.fact_key, 'text', f.text, 'start', f.start_offset, 'end', f.end_offset)),'[]')
		     FROM article_facts f WHERE f.article_id=a.id) AS facts,
		  (SELECT coalesce(json_agg(json_build_object('claim_id', o.claim_id, 'quote', o.quote_text, 'start', o.start_offset, 'end', o.end_offset,
		     'attributed_to', o.attributed_to, 'model_ref', o.extraction_model_ref)),'[]') FROM claim_occurrences o WHERE o.article_id=a.id) AS claims,
		  (SELECT coalesce(json_agg(json_build_object('event_id', l.event_id, 'relation', l.relation, 'confidence', l.confidence, 'rationale', l.rationale,
		     'linked_at', l.linked_at, 'unlinked_at', l.unlinked_at)),'[]') FROM event_article_links l WHERE l.article_id=a.id) AS event_links
		FROM articles a WHERE a.id=$1) x`, id).Scan(&raw)
	if db.IsNoRows(err) {
		return nil, apperr.NotFound("article_not_found", "Article not found.")
	}
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(raw, &out)
	return out, err
}

func actorID(ctx context.Context) *uuid.UUID {
	if p := principal.From(ctx); p != nil && p.Kind == principal.KindUser {
		return &p.ID
	}
	return nil
}

// SetStatus quarantines or releases an article (release re-enters enrichment).
func (s *Service) SetArticleStatus(ctx context.Context, id uuid.UUID, action, reason string) error {
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		var status string
		var rev int
		err := tx.QueryRow(ctx, `SELECT status, revision FROM articles WHERE id=$1 FOR UPDATE`, id).Scan(&status, &rev)
		if db.IsNoRows(err) {
			return apperr.NotFound("article_not_found", "Article not found.")
		}
		if err != nil {
			return err
		}
		switch action {
		case "quarantine":
			if status == "withdrawn" {
				return apperr.Conflict("article_withdrawn", "Withdrawn articles cannot be quarantined.")
			}
			if _, err := tx.Exec(ctx, `UPDATE articles SET status='quarantined', last_error=$2, version=version+1, updated_at=now() WHERE id=$1`, id, reason); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE event_article_links SET unlinked_at=now() WHERE article_id=$1 AND unlinked_at IS NULL`, id); err != nil {
				return err
			}
			if _, err := bus.Publish(ctx, tx, bus.Draft{Type: bus.ContentDeleted, AggregateType: "article", AggregateID: id.String(),
				Payload: map[string]any{"doc_type": "article", "id": id, "force": true}}); err != nil {
				return err
			}
		case "release", "retry":
			if status != "quarantined" && status != "failed" && action == "release" {
				return apperr.Conflict("article_not_quarantined", "Only quarantined or failed articles can be released.")
			}
			if status == "withdrawn" || status == "duplicate" {
				return apperr.Conflict("article_not_processable", "This article cannot be reprocessed.")
			}
			if _, err := tx.Exec(ctx, `UPDATE articles SET status='extracted', last_error=NULL, version=version+1, updated_at=now() WHERE id=$1`, id); err != nil {
				return err
			}
			if _, err := bus.Publish(ctx, tx, bus.Draft{Type: bus.ArticleEnrichmentRequested, AggregateType: "article", AggregateID: id.String(),
				Payload: EnrichmentRequested{ArticleID: id, Revision: rev, Reason: "reprocess"}}); err != nil {
				return err
			}
		default:
			return apperr.ValidationMsg("invalid_action", "Unknown action.")
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "article." + action, ResourceType: "article", ResourceID: id.String(),
			Old: map[string]any{"status": status}, New: map[string]any{"reason": reason}})
	})
}

// Withdraw tombstones an article (legal/removal request): body and raw snapshot are deleted,
// it is unlinked from events and removed from search projections. Metadata remains for audit.
func (s *Service) Withdraw(ctx context.Context, id uuid.UUID, reason, legalBasis string) ([]uuid.UUID, error) {
	if reason == "" {
		return nil, apperr.Validation(apperr.FieldError{Field: "reason", Code: "required", Message: "A reason is required."})
	}
	var events []uuid.UUID
	var rawKey *string
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		var rev int
		err := tx.QueryRow(ctx, `SELECT revision, raw_object_key FROM articles WHERE id=$1 FOR UPDATE`, id).Scan(&rev, &rawKey)
		if db.IsNoRows(err) {
			return apperr.NotFound("article_not_found", "Article not found.")
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE articles SET status='withdrawn', tombstoned_at=now(), retracted_at=COALESCE(retracted_at, now()), body_text=NULL,
			raw_object_key=NULL, revision=revision+1, version=version+1, updated_at=now() WHERE id=$1`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE article_versions SET body_text=NULL WHERE article_id=$1`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO article_versions (id, article_id, revision, title, text_hash, change_kind)
			SELECT gen_random_uuid(), id, revision, title, text_hash, 'tombstone' FROM articles WHERE id=$1`, id); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `UPDATE event_article_links SET unlinked_at=now() WHERE article_id=$1 AND unlinked_at IS NULL RETURNING event_id`, id)
		if err != nil {
			return err
		}
		events, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO content_removal_requests (id, target_type, target_value, reason, legal_basis, status, requested_by, processed_at)
			VALUES (gen_random_uuid(),'article',$1,$2,$3,'succeeded',$4,now())`, id.String(), reason, legalBasis, actorID(ctx)); err != nil {
			return err
		}
		if _, err := bus.Publish(ctx, tx, bus.Draft{Type: bus.ContentDeleted, AggregateType: "article", AggregateID: id.String(),
			Payload: map[string]any{"doc_type": "article", "id": id, "force": true}}); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "article.withdraw", ResourceType: "article", ResourceID: id.String(),
			New: map[string]any{"reason": reason, "legal_basis": legalBasis, "events": events}})
	})
	if err == nil && rawKey != nil && s.store != nil {
		_ = s.store.Delete(ctx, *rawKey)
	}
	return events, err
}

// SetEditorialCategories overrides categories for an article (AI assignments are preserved
// separately with their model/confidence for comparison).
func (s *Service) SetEditorialCategories(ctx context.Context, id uuid.UUID, cats []uuid.UUID, primary *uuid.UUID) error {
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM articles WHERE id=$1)`, id).Scan(&exists); err != nil || !exists {
			return apperr.NotFound("article_not_found", "Article not found.")
		}
		if _, err := tx.Exec(ctx, `DELETE FROM article_categories WHERE article_id=$1 AND source='editorial'`, id); err != nil {
			return err
		}
		for _, c := range cats {
			prim := primary != nil && *primary == c
			if _, err := tx.Exec(ctx, `INSERT INTO article_categories (article_id, category_id, source, is_primary, confidence, assigned_by)
				VALUES ($1,$2,'editorial',$3,1,$4)`, id, c, prim, actorID(ctx)); err != nil {
				if db.IsForeignKeyViolation(err) {
					return apperr.ValidationMsg("category_not_found", "Unknown category id.")
				}
				return err
			}
		}
		if primary != nil {
			if _, err := tx.Exec(ctx, `UPDATE article_categories SET is_primary=false WHERE article_id=$1 AND source='ai'`, id); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE articles SET version=version+1, updated_at=now() WHERE id=$1`, id); err != nil {
			return err
		}
		if _, err := bus.Publish(ctx, tx, bus.Draft{Type: bus.SearchIndexRequested, AggregateType: "article", AggregateID: id.String(),
			Payload: map[string]any{"doc_type": "article", "id": id, "force": true}}); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "article.categories_override", ResourceType: "article", ResourceID: id.String(),
			New: map[string]any{"category_ids": cats, "primary": primary}})
	})
}
