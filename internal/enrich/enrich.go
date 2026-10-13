// Package enrich turns extracted articles into structured knowledge: entities, comparable facts,
// dynamic-taxonomy categories and atomic claims, each with model/version provenance.
package enrich

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dorinpuscasu25/recapproject/internal/ai"
	"github.com/dorinpuscasu25/recapproject/internal/ingest"
	"github.com/dorinpuscasu25/recapproject/internal/nlp"
	"github.com/dorinpuscasu25/recapproject/internal/platform/bus"
	"github.com/dorinpuscasu25/recapproject/internal/platform/crypto"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ids"
	"github.com/dorinpuscasu25/recapproject/internal/policy"
	"github.com/dorinpuscasu25/recapproject/internal/taxonomy"
)

type Service struct {
	db  *db.DB
	gw  *ai.Gateway
	tax *taxonomy.Service
	pol *policy.Store
	log *slog.Logger

	gazMu sync.Mutex
	gaz   *nlp.Gazetteer
	gazAt time.Time
}

func NewService(d *db.DB, gw *ai.Gateway, tax *taxonomy.Service, pol *policy.Store, log *slog.Logger) *Service {
	return &Service{db: d, gw: gw, tax: tax, pol: pol, log: log}
}

// Enriched is the article.enriched.v1 payload.
type Enriched struct {
	ArticleID       uuid.UUID   `json:"article_id"`
	Revision        int         `json:"revision"`
	Language        string      `json:"language"`
	CategorySlugs   []string    `json:"category_slugs"`
	ClaimIDs        []uuid.UUID `json:"claim_ids"`
	TaxonomyVersion int64       `json:"taxonomy_version"`
}

// ResolutionRequested is the event.resolution.requested.v1 payload.
type ResolutionRequested struct {
	ArticleID uuid.UUID `json:"article_id"`
	Revision  int       `json:"revision"`
	Reason    string    `json:"reason"`
}

// Gazetteer returns the DB-backed entity gazetteer (refreshed every 5 minutes).
func (s *Service) Gazetteer(ctx context.Context) (*nlp.Gazetteer, error) {
	s.gazMu.Lock()
	defer s.gazMu.Unlock()
	if s.gaz != nil && time.Since(s.gazAt) < 5*time.Minute {
		return s.gaz, nil
	}
	rows, err := s.db.Pool.Query(ctx, `SELECT kind, canonical_name, aliases FROM entities WHERE cardinality(aliases) > 0 OR kind IN ('person','organization','location') LIMIT 50000`)
	if err != nil {
		return nil, err
	}
	var entries []nlp.GazetteerEntry
	for rows.Next() {
		var e nlp.GazetteerEntry
		if err := rows.Scan(&e.Kind, &e.Canonical, &e.Aliases); err != nil {
			rows.Close()
			return nil, err
		}
		entries = append(entries, e)
	}
	rows.Close()
	s.gaz, s.gazAt = nlp.NewGazetteer(entries), time.Now()
	return s.gaz, nil
}

type article struct {
	ID, PublisherID       uuid.UUID
	EndpointID            *uuid.UUID
	Title, Description    string
	Body                  string
	Language, ContentType string
	Status                string
	Revision              int
	PublishedAt           *time.Time
	AllowedCats, HintCats []uuid.UUID
}

func (s *Service) loadArticle(ctx context.Context, id uuid.UUID) (*article, error) {
	a := &article{ID: id}
	var body *string
	err := s.db.Pool.QueryRow(ctx, `SELECT a.publisher_id, a.source_endpoint_id, a.title, a.description, a.body_text, a.language, a.content_type, a.status,
		a.revision, a.published_at, COALESCE(e.allowed_category_ids,'{}'), COALESCE(e.category_hints,'{}')
		FROM articles a LEFT JOIN source_endpoints e ON e.id=a.source_endpoint_id WHERE a.id=$1`, id).
		Scan(&a.PublisherID, &a.EndpointID, &a.Title, &a.Description, &body, &a.Language, &a.ContentType, &a.Status, &a.Revision, &a.PublishedAt, &a.AllowedCats, &a.HintCats)
	if body != nil {
		a.Body = *body
	}
	return a, err
}

// HandleEnrichment is the consumer for article.enrichment.requested.v1.
func (s *Service) HandleEnrichment(ctx context.Context, d *bus.Delivery) error {
	var msg ingest.EnrichmentRequested
	if err := d.Message.Decode(&msg); err != nil {
		return bus.PermanentErr(err)
	}
	if done, err := d.AlreadyProcessed(ctx); err != nil || done {
		return err
	}
	a, err := s.loadArticle(ctx, msg.ArticleID)
	if db.IsNoRows(err) {
		return d.Once(ctx, func(pgx.Tx) error { return nil })
	}
	if err != nil {
		return err
	}
	switch a.Status {
	case "withdrawn", "duplicate", "quarantined", "under_review":
		return d.Once(ctx, func(pgx.Tx) error { return nil })
	}
	if a.Revision > msg.Revision && msg.Reason != "reprocess" && msg.Reason != "reclassify" {
		// A newer revision has its own request; skip stale work.
		return d.Once(ctx, func(pgx.Tx) error { return nil })
	}
	_, _ = s.db.Pool.Exec(ctx, `UPDATE articles SET status='enriching', processing_attempts=processing_attempts+1 WHERE id=$1`, a.ID)
	res, err := s.Enrich(ctx, a, msg.Reason == "reclassify")
	if err != nil {
		if d.Attempt >= 5 {
			_, _ = s.db.Pool.Exec(ctx, `UPDATE articles SET status='failed', last_error=$2 WHERE id=$1`, a.ID, truncate(err.Error(), 500))
			return bus.PermanentErr(err)
		}
		if errors.Is(err, ai.ErrDeferred) {
			s.log.Warn("enrichment deferred: AI unavailable", "article_id", a.ID, "error", err)
		}
		return err
	}
	return d.Once(ctx, func(tx pgx.Tx) error { return s.persist(ctx, tx, a, res, msg.Reason) })
}

type result struct {
	entities       []nlp.Mention
	facts          []nlp.Fact
	cats           []ai.CategoryAssignment
	catModel       string
	taxVersion     int64
	claims         []ai.ExtractedClaim
	claimModel     string
	categoryIDs    map[string]uuid.UUID
	parentOf       map[string]string
	onlyCategories bool
}

// Enrich computes enrichment without persisting (also used by the evaluation harness).
func (s *Service) Enrich(ctx context.Context, a *article, onlyCategories bool) (*result, error) {
	r := &result{onlyCategories: onlyCategories, categoryIDs: map[string]uuid.UUID{}, parentOf: map[string]string{}}
	text := a.Body
	if text == "" {
		text = a.Description
	}
	full := a.Title + ". " + text
	if !onlyCategories {
		g, err := s.Gazetteer(ctx)
		if err != nil {
			return nil, err
		}
		r.entities = g.Entities(full)
		r.facts = nlp.Facts(text)
	}
	// Dynamic taxonomy: the classifier sees the current DB categories every time.
	cats, ver, err := s.tax.Active(ctx)
	if err != nil {
		return nil, err
	}
	r.taxVersion = ver
	allowed := map[uuid.UUID]bool{}
	for _, id := range a.AllowedCats {
		allowed[id] = true
	}
	slugByID := map[uuid.UUID]string{}
	for _, c := range cats {
		slugByID[c.ID] = c.Slug
	}
	var defs []ai.CategoryDef
	for _, c := range cats {
		if !c.AutoClassify || (len(allowed) > 0 && !allowed[c.ID]) {
			continue
		}
		def := ai.CategoryDef{Slug: c.Slug, Names: c.Names, Description: c.EditorialDescription, Hints: c.ClassificationHints, Aliases: c.Aliases,
			Positive: c.PositiveExamples, Negative: c.NegativeExamples, Boost: c.Boost}
		if c.ParentID != nil {
			def.ParentSlug = slugByID[*c.ParentID]
			r.parentOf[c.Slug] = def.ParentSlug
		}
		defs = append(defs, def)
		r.categoryIDs[c.Slug] = c.ID
	}
	var hints []string
	for _, id := range a.HintCats {
		if sl, ok := slugByID[id]; ok {
			hints = append(hints, sl)
		}
	}
	if len(defs) > 0 {
		var out ai.ClassifyOutput
		exec, err := s.gw.GenerateStructured(ctx, ai.TaskClassification, &ai.ClassifyInput{Title: a.Title, Text: nlp.Truncate(text, 6000), Language: a.Language,
			Categories: defs, TaxonomyVersion: ver, SourceHints: hints}, &out, ai.CallOpts{SubjectType: "article", SubjectID: a.ID.String()})
		if err != nil {
			return nil, fmt.Errorf("classify: %w", err)
		}
		r.cats, r.catModel = out.Assignments, exec.ModelRef
	}
	if onlyCategories {
		return r, nil
	}
	if a.ContentType != "satire" && len([]rune(text)) >= 80 {
		var out ai.ClaimOutput
		in := &ai.ClaimInput{Title: a.Title, Text: text, Language: a.Language, ContentType: a.ContentType, MaxClaims: 12}
		exec, err := s.gw.GenerateStructured(ctx, ai.TaskClaimExtraction, in, &out, ai.CallOpts{SubjectType: "article", SubjectID: a.ID.String()})
		if err != nil {
			return nil, fmt.Errorf("claims: %w", err)
		}
		r.claims, r.claimModel = out.Claims, exec.ModelRef
	}
	return r, nil
}

func (s *Service) persist(ctx context.Context, tx pgx.Tx, a *article, r *result, reason string) error {
	var slugs []string
	// Categories: replace AI assignments; editorial assignments are never touched.
	if _, err := tx.Exec(ctx, `DELETE FROM article_categories WHERE article_id=$1 AND source='ai'`, a.ID); err != nil {
		return err
	}
	for _, c := range r.cats {
		id, ok := r.categoryIDs[c.Slug]
		if !ok {
			continue
		}
		slugs = append(slugs, c.Slug)
		conf := c.Confidence
		if _, err := tx.Exec(ctx, `INSERT INTO article_categories (article_id, category_id, source, is_primary, confidence, model_ref, taxonomy_version)
			VALUES ($1,$2,'ai',$3,$4,$5,$6) ON CONFLICT DO NOTHING`, a.ID, id, c.IsPrimary, conf, r.catModel, r.taxVersion); err != nil {
			return err
		}
		// A subcategory implies its parent (derived, lower confidence, never primary).
		if p := r.parentOf[c.Slug]; p != "" {
			if pid, ok := r.categoryIDs[p]; ok {
				if _, err := tx.Exec(ctx, `INSERT INTO article_categories (article_id, category_id, source, is_primary, confidence, model_ref, taxonomy_version)
					VALUES ($1,$2,'ai',false,$3,$4,$5) ON CONFLICT DO NOTHING`, a.ID, pid, conf*0.9, r.catModel+"+parent", r.taxVersion); err != nil {
					return err
				}
			}
		}
	}
	if r.onlyCategories {
		_, err := tx.Exec(ctx, `UPDATE articles SET enrichment = jsonb_set(enrichment, '{classification}', $2), version=version+1, updated_at=now() WHERE id=$1`,
			a.ID, mustJSON(map[string]any{"model": r.catModel, "taxonomy_version": r.taxVersion, "at": time.Now().UTC()}))
		if err != nil {
			return err
		}
		_, err = bus.Publish(ctx, tx, bus.Draft{Type: bus.ArticleEnriched, AggregateType: "article", AggregateID: a.ID.String(),
			Payload: Enriched{ArticleID: a.ID, Revision: a.Revision, Language: a.Language, CategorySlugs: slugs, TaxonomyVersion: r.taxVersion}})
		return err
	}
	// Entities.
	if _, err := tx.Exec(ctx, `DELETE FROM article_entities WHERE article_id=$1`, a.ID); err != nil {
		return err
	}
	var geos []string
	for i, m := range r.entities {
		if !m.Known && (m.Count < 2 && len(strings.Fields(m.Name)) < 2) {
			continue // avoid polluting the entity table with one-off capitalized words
		}
		if i >= 40 {
			break
		}
		kind := m.Kind
		if kind == "" {
			kind = "other"
		}
		var eid uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO entities (id, kind, canonical_name, normalized_key) VALUES ($1,$2,$3,$4)
			ON CONFLICT (kind, normalized_key) DO UPDATE SET kind=EXCLUDED.kind RETURNING id`, ids.New(), kind, m.Name, m.Key).Scan(&eid); err != nil {
			return err
		}
		sal := float32(m.Count) / float32(m.Count+2)
		if _, err := tx.Exec(ctx, `INSERT INTO article_entities (article_id, entity_id, mention_count, salience) VALUES ($1,$2,$3,$4)
			ON CONFLICT (article_id, entity_id) DO UPDATE SET mention_count=EXCLUDED.mention_count`, a.ID, eid, m.Count, sal); err != nil {
			return err
		}
		if kind == "location" {
			geos = append(geos, m.Key)
		}
	}
	// Facts.
	if _, err := tx.Exec(ctx, `DELETE FROM article_facts WHERE article_id=$1`, a.ID); err != nil {
		return err
	}
	for i, f := range r.facts {
		if i >= 100 {
			break
		}
		if _, err := tx.Exec(ctx, `INSERT INTO article_facts (id, article_id, fact_type, fact_key, text, start_offset, end_offset, value)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, ids.New(), a.ID, f.Type, f.Key, f.Text, f.Start, f.End, mustJSON(f.Value)); err != nil {
			return err
		}
	}
	// Claims (deduplicated by normalized key; occurrences keep exact spans).
	var risk struct {
		Terms []string `json:"high_risk_terms"`
	}
	_, _ = s.pol.Get(ctx, "factcheck", &risk)
	var claimIDs []uuid.UUID
	if _, err := tx.Exec(ctx, `DELETE FROM claim_occurrences WHERE article_id=$1`, a.ID); err != nil {
		return err
	}
	for _, c := range r.claims {
		key := nlp.ClaimKey(c.Canonical)
		if key == "" {
			continue
		}
		level := riskLevel(c.Canonical, risk.Terms)
		var cid uuid.UUID
		err := tx.QueryRow(ctx, `INSERT INTO claims (id, canonical_text, language, normalized_key, checkability, claim_type, risk_level, first_seen_at, last_seen_at, occurrence_count)
			VALUES ($1,$2,$3,$4,$5,$6,$7, COALESCE($8, now()), COALESCE($8, now()), 1)
			ON CONFLICT (normalized_key) DO UPDATE SET last_seen_at=GREATEST(claims.last_seen_at, EXCLUDED.last_seen_at),
			  occurrence_count=claims.occurrence_count+1, updated_at=now(), version=claims.version+1,
			  risk_level=CASE WHEN EXCLUDED.risk_level='high' THEN 'high' ELSE claims.risk_level END
			RETURNING id`, ids.New(), truncate(c.Canonical, 1990), a.Language, crypto.SHA256Hex(key), c.Checkability, c.ClaimType, level, a.PublishedAt).Scan(&cid)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO claim_occurrences (id, claim_id, article_id, quote_text, start_offset, end_offset, attributed_to, is_direct_quote,
			asserted_at, language, extraction_model_ref) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT DO NOTHING`,
			ids.New(), cid, a.ID, c.Quote, c.Start, c.End, c.AttributedTo, c.IsDirectQuote, a.PublishedAt, a.Language, r.claimModel); err != nil {
			return err
		}
		claimIDs = append(claimIDs, cid)
	}
	enr := map[string]any{"classification": map[string]any{"model": r.catModel, "taxonomy_version": r.taxVersion},
		"claims": map[string]any{"model": r.claimModel, "count": len(claimIDs)}, "entities": len(r.entities), "facts": len(r.facts),
		"enriched_at": time.Now().UTC(), "reason": reason}
	if _, err := tx.Exec(ctx, `UPDATE articles SET status='enriched', geographies=$2, enrichment = enrichment || $3::jsonb, last_error=NULL,
		version=version+1, updated_at=now() WHERE id=$1`, a.ID, nonNil(geos), mustJSON(enr)); err != nil {
		return err
	}
	if _, err := bus.Publish(ctx, tx, bus.Draft{Type: bus.ArticleEnriched, AggregateType: "article", AggregateID: a.ID.String(),
		Payload: Enriched{ArticleID: a.ID, Revision: a.Revision, Language: a.Language, CategorySlugs: slugs, ClaimIDs: claimIDs, TaxonomyVersion: r.taxVersion}}); err != nil {
		return err
	}
	_, err := bus.Publish(ctx, tx, bus.Draft{Type: bus.EventResolutionRequested, AggregateType: "article", AggregateID: a.ID.String(),
		Payload: ResolutionRequested{ArticleID: a.ID, Revision: a.Revision, Reason: reason}})
	return err
}

func riskLevel(text string, terms []string) string {
	l := strings.ToLower(text)
	for _, t := range terms {
		if t != "" && strings.Contains(l, strings.ToLower(t)) {
			return "high"
		}
	}
	return "low"
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
