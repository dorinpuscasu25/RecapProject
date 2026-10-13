package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dorinpuscasu25/recapproject/internal/ai"
	"github.com/dorinpuscasu25/recapproject/internal/audit"
	"github.com/dorinpuscasu25/recapproject/internal/jobs"
	"github.com/dorinpuscasu25/recapproject/internal/nlp"
	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/bus"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ids"
	"github.com/dorinpuscasu25/recapproject/internal/policy"
)

var DocTypes = []string{"article", "event", "claim"}

type Service struct {
	db       *db.DB
	gw       *ai.Gateway
	vec      VectorStore
	lex      LexicalIndex
	pol      *policy.Store
	log      *slog.Logger
	prefix   string
	degraded []string
}

func NewService(d *db.DB, gw *ai.Gateway, vec VectorStore, lex LexicalIndex, pol *policy.Store, log *slog.Logger, prefix string) *Service {
	s := &Service{db: d, gw: gw, vec: vec, lex: lex, pol: pol, log: log, prefix: prefix}
	if lex.Name() == "postgres_fts" {
		s.degraded = append(s.degraded, "lexical:postgres_fts")
	}
	return s
}

func (s *Service) Lexical() LexicalIndex { return s.lex }
func (s *Service) Vectors() VectorStore  { return s.vec }

// ---------- Collections ----------

type Collection struct {
	ID              uuid.UUID  `json:"id"`
	DocType         string     `json:"doc_type"`
	ModelID         uuid.UUID  `json:"model_id"`
	ModelSlug       string     `json:"model_slug"`
	Name            string     `json:"collection_name"`
	Dims            int        `json:"dims"`
	Status          string     `json:"status"`
	BackfilledCount int        `json:"backfilled_count"`
	CreatedAt       time.Time  `json:"created_at"`
	ActivatedAt     *time.Time `json:"activated_at,omitempty"`
}

var slugClean = regexp.MustCompile(`[^a-z0-9]+`)

func (s *Service) collectionName(docType string, m *ai.Model) string {
	return fmt.Sprintf("%s%s__%s__%d", s.prefix, docType, strings.Trim(slugClean.ReplaceAllString(strings.ToLower(m.Slug), "_"), "_"), m.EmbeddingDims)
}

func (s *Service) Alias(docType string) string { return s.prefix + docType }

func (s *Service) collections(ctx context.Context, docType string) ([]Collection, error) {
	rows, err := s.db.Pool.Query(ctx, `SELECT c.id, c.doc_type, c.model_id, m.slug, c.collection_name, c.dims, c.status, c.backfilled_count, c.created_at, c.activated_at
		FROM vector_collections c JOIN ai_models m ON m.id=c.model_id WHERE ($1='' OR c.doc_type=$1) ORDER BY c.created_at`, docType)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Collection, error) {
		var c Collection
		err := r.Scan(&c.ID, &c.DocType, &c.ModelID, &c.ModelSlug, &c.Name, &c.Dims, &c.Status, &c.BackfilledCount, &c.CreatedAt, &c.ActivatedAt)
		return c, err
	})
}

func (s *Service) ListCollections(ctx context.Context) ([]Collection, error) {
	return s.collections(ctx, "")
}

// writeTargets returns the active collection and, during a migration, the building one
// (dual-write). Bootstraps an active collection on first use.
func (s *Service) writeTargets(ctx context.Context, docType string) ([]Collection, error) {
	cols, err := s.collections(ctx, docType)
	if err != nil {
		return nil, err
	}
	var out []Collection
	for _, c := range cols {
		if c.Status == "active" || c.Status == "building" {
			out = append(out, c)
		}
	}
	if len(out) > 0 {
		for _, c := range out {
			if err := s.vec.EnsureCollection(ctx, c.Name, c.Dims); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	m, err := s.gw.EmbeddingModel(ctx)
	if err != nil {
		return nil, err
	}
	c := Collection{ID: ids.New(), DocType: docType, ModelID: m.ID, ModelSlug: m.Slug, Name: s.collectionName(docType, m), Dims: m.EmbeddingDims, Status: "active"}
	if err := s.vec.EnsureCollection(ctx, c.Name, c.Dims); err != nil {
		return nil, err
	}
	_, err = s.db.Pool.Exec(ctx, `INSERT INTO vector_collections (id, doc_type, model_id, collection_name, dims, status, activated_at)
		VALUES ($1,$2,$3,$4,$5,'active',now()) ON CONFLICT DO NOTHING`, c.ID, docType, m.ID, c.Name, c.Dims)
	if err != nil {
		return nil, err
	}
	_ = s.vec.SetAlias(ctx, s.Alias(docType), c.Name)
	return s.writeTargets(ctx, docType)
}

// ActiveCollection returns the collection reads should use.
func (s *Service) ActiveCollection(ctx context.Context, docType string) (*Collection, error) {
	ts, err := s.writeTargets(ctx, docType)
	if err != nil {
		return nil, err
	}
	for _, c := range ts {
		if c.Status == "active" {
			return &c, nil
		}
	}
	return nil, fmt.Errorf("no active collection for %s", docType)
}

func (s *Service) modelByID(ctx context.Context, id uuid.UUID) (*ai.Model, error) {
	var slug string
	if err := s.db.Pool.QueryRow(ctx, `SELECT slug FROM ai_models WHERE id=$1`, id).Scan(&slug); err != nil {
		return nil, err
	}
	return s.gw.ModelBySlug(ctx, slug)
}

const JobVectorBackfill = "search.vector_backfill"

// PlanEmbeddingMigration creates 'building' collections for the current embeddings-route model
// (if it differs from the active one) and enqueues backfill jobs. Reads keep using the active
// collection until Cutover; new writes go to both (dual-write).
func (s *Service) PlanEmbeddingMigration(ctx context.Context) (map[string]any, error) {
	s.gw.Invalidate()
	m, err := s.gw.EmbeddingModel(ctx)
	if err != nil {
		return nil, err
	}
	plan := map[string]any{"target_model": m.Ref(), "dims": m.EmbeddingDims, "collections": []map[string]any{}}
	err = s.db.Tx(ctx, func(tx pgx.Tx) error {
		for _, dt := range DocTypes {
			var activeModel uuid.UUID
			err := tx.QueryRow(ctx, `SELECT model_id FROM vector_collections WHERE doc_type=$1 AND status='active'`, dt).Scan(&activeModel)
			if err != nil && !db.IsNoRows(err) {
				return err
			}
			if activeModel == m.ID {
				continue
			}
			name := s.collectionName(dt, m)
			if err := s.vec.EnsureCollection(ctx, name, m.EmbeddingDims); err != nil {
				return err
			}
			id := ids.New()
			if _, err := tx.Exec(ctx, `INSERT INTO vector_collections (id, doc_type, model_id, collection_name, dims, status)
				VALUES ($1,$2,$3,$4,$5,'building') ON CONFLICT (collection_name) DO UPDATE SET status='building', retired_at=NULL`, id, dt, m.ID, name, m.EmbeddingDims); err != nil {
				return err
			}
			jobID, err := jobs.Enqueue(ctx, tx, JobVectorBackfill, map[string]string{"doc_type": dt, "collection": name}, jobs.EnqueueOpts{Priority: 300,
				IdempotencyKey: "backfill:" + name + ":" + time.Now().UTC().Format("20060102150405")})
			if err != nil {
				return err
			}
			plan["collections"] = append(plan["collections"].([]map[string]any), map[string]any{"doc_type": dt, "collection": name, "backfill_job_id": jobID})
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "embeddings.migration_planned", ResourceType: "vector_collection", New: plan})
	})
	return plan, err
}

// RunBackfill embeds all canonical records of a doc type into a building collection.
func (s *Service) RunBackfill(ctx context.Context, j *jobs.Job) (any, error) {
	var p struct {
		DocType    string `json:"doc_type"`
		Collection string `json:"collection"`
	}
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		return nil, jobs.Permanent(err)
	}
	var modelID uuid.UUID
	if err := s.db.Pool.QueryRow(ctx, `SELECT model_id FROM vector_collections WHERE collection_name=$1`, p.Collection).Scan(&modelID); err != nil {
		return nil, jobs.Permanent(err)
	}
	m, err := s.modelByID(ctx, modelID)
	if err != nil {
		return nil, err
	}
	idList, err := s.allIDs(ctx, p.DocType)
	if err != nil {
		return nil, err
	}
	n := 0
	for _, id := range idList {
		pt, ok, err := s.vectorPoint(ctx, p.DocType, id, m)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if err := s.vec.Upsert(ctx, p.Collection, []VectorPoint{*pt}); err != nil {
			return nil, err
		}
		n++
		if n%200 == 0 {
			_, _ = s.db.Pool.Exec(ctx, `UPDATE vector_collections SET backfilled_count=$2 WHERE collection_name=$1`, p.Collection, n)
		}
	}
	_, err = s.db.Pool.Exec(ctx, `UPDATE vector_collections SET backfilled_count=$2 WHERE collection_name=$1`, p.Collection, n)
	return map[string]int{"backfilled": n}, err
}

// Cutover validates a building collection (count parity within tolerance) and makes it active.
func (s *Service) Cutover(ctx context.Context, docType string) error {
	cols, err := s.collections(ctx, docType)
	if err != nil {
		return err
	}
	var active, building *Collection
	for i := range cols {
		switch cols[i].Status {
		case "active":
			active = &cols[i]
		case "building":
			building = &cols[i]
		}
	}
	if building == nil {
		return apperr.Conflict("no_building_collection", "There is no collection being built for this document type.")
	}
	// Readiness is judged against the canonical records, not the old collection (which may hold
	// vectors for records that are no longer eligible): the latest backfill for this collection
	// must have succeeded and every backfilled vector must be present.
	var jobStatus string
	err = s.db.Pool.QueryRow(ctx, `SELECT status FROM jobs WHERE kind=$1 AND payload->>'collection'=$2 ORDER BY created_at DESC LIMIT 1`,
		JobVectorBackfill, building.Name).Scan(&jobStatus)
	if err != nil && !db.IsNoRows(err) {
		return err
	}
	if jobStatus != "succeeded" {
		return apperr.Conflict("backfill_incomplete", fmt.Sprintf("The backfill for %s has not completed (status %q).", building.Name, jobStatus))
	}
	b, err := s.vec.Count(ctx, building.Name)
	if err != nil {
		return apperr.Unavailable("vector_store_unavailable", "Cannot verify the building collection: "+err.Error())
	}
	if b < building.BackfilledCount {
		return apperr.Conflict("backfill_incomplete", fmt.Sprintf("Building collection has %d of %d backfilled vectors.", b, building.BackfilledCount))
	}
	err = s.db.Tx(ctx, func(tx pgx.Tx) error {
		if active != nil {
			if _, err := tx.Exec(ctx, `UPDATE vector_collections SET status='retired', retired_at=now() WHERE id=$1`, active.ID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE vector_collections SET status='active', activated_at=now() WHERE id=$1`, building.ID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "embeddings.cutover", ResourceType: "vector_collection", ResourceID: building.Name,
			Old: map[string]any{"active": nameOf(active)}, New: map[string]any{"active": building.Name}})
	})
	if err != nil {
		return err
	}
	return s.vec.SetAlias(ctx, s.Alias(docType), building.Name)
}

// Rollback re-activates the most recently retired collection.
func (s *Service) Rollback(ctx context.Context, docType string) error {
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		var retired uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM vector_collections WHERE doc_type=$1 AND status='retired' ORDER BY retired_at DESC LIMIT 1`, docType).Scan(&retired); err != nil {
			if db.IsNoRows(err) {
				return apperr.Conflict("no_retired_collection", "Nothing to roll back to.")
			}
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE vector_collections SET status='building' WHERE doc_type=$1 AND status='active'`, docType); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE vector_collections SET status='active', retired_at=NULL, activated_at=now() WHERE id=$1`, retired); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "embeddings.rollback", ResourceType: "vector_collection", ResourceID: retired.String()})
	})
	if err != nil {
		return err
	}
	c, err := s.ActiveCollection(ctx, docType)
	if err != nil {
		return err
	}
	return s.vec.SetAlias(ctx, s.Alias(docType), c.Name)
}

func nameOf(c *Collection) string {
	if c == nil {
		return ""
	}
	return c.Name
}

// ---------- Projection building ----------

func (s *Service) allIDs(ctx context.Context, docType string) ([]uuid.UUID, error) {
	var q string
	switch docType {
	case "article":
		q = `SELECT id FROM articles WHERE status='enriched' AND tombstoned_at IS NULL ORDER BY id`
	case "event":
		q = `SELECT id FROM events WHERE status='active' ORDER BY id`
	case "claim":
		q = `SELECT id FROM claims ORDER BY id`
	default:
		return nil, fmt.Errorf("unknown doc type %s", docType)
	}
	rows, err := s.db.Pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

type docData struct {
	lex       LexicalDoc
	embedText string
	embedOK   bool
	payload   map[string]any
	deleted   bool
}

func (s *Service) load(ctx context.Context, docType string, id uuid.UUID) (*docData, error) {
	d := &docData{}
	switch docType {
	case "article":
		var title, desc, lang, status string
		var body *string
		var pub *time.Time
		var publisher uuid.UUID
		var version int
		var tomb *time.Time
		var allowEmbed bool
		var cats, ents, geos []string
		var eventID *uuid.UUID
		err := s.db.Pool.QueryRow(ctx, `SELECT a.title, a.description, a.body_text, a.language, a.status, a.published_at, a.publisher_id, a.version, a.tombstoned_at,
			COALESCE((SELECT p.allow_embed FROM source_permissions p WHERE p.publisher_id=a.publisher_id AND (p.source_endpoint_id=a.source_endpoint_id OR p.source_endpoint_id IS NULL)
			          ORDER BY p.source_endpoint_id NULLS LAST LIMIT 1), false),
			ARRAY(SELECT category_id::text FROM article_categories WHERE article_id=a.id),
			ARRAY(SELECT e.normalized_key FROM article_entities ae JOIN entities e ON e.id=ae.entity_id WHERE ae.article_id=a.id),
			a.geographies,
			(SELECT event_id FROM event_article_links l WHERE l.article_id=a.id AND l.unlinked_at IS NULL AND relation IN ('origin','same_story','follow_up') LIMIT 1)
			FROM articles a WHERE a.id=$1`, id).Scan(&title, &desc, &body, &lang, &status, &pub, &publisher, &version, &tomb, &allowEmbed, &cats, &ents, &geos, &eventID)
		if db.IsNoRows(err) {
			d.deleted = true
			return d, nil
		}
		if err != nil {
			return nil, err
		}
		if tomb != nil || status == "withdrawn" || status == "duplicate" || status == "quarantined" {
			d.deleted = true
			return d, nil
		}
		b := ""
		if body != nil {
			b = *body
		}
		ev := ""
		if eventID != nil {
			ev = eventID.String()
		}
		d.lex = LexicalDoc{ID: id.String(), DocType: "article", Title: title, Body: desc + "\n" + nlp.Truncate(b, 20000), Language: lang, PublishedAt: pub,
			CategoryIDs: nonNil(cats), PublisherID: publisher.String(), Regions: nonNil(geos), EntityKeys: nonNil(ents), Status: status, EventID: ev, Version: version}
		d.embedOK = allowEmbed
		d.embedText = title + ". " + desc + " " + nlp.Truncate(b, 1500)
		d.payload = map[string]any{"doc_type": "article", "language": lang, "category_ids": nonNil(cats), "publisher_id": publisher.String(),
			"published_ts": unix(pub), "status": status, "entity_keys": nonNil(ents), "event_id": ev, "version": version}
	case "event":
		var titles, summaries []byte
		var state, status string
		var upd *time.Time
		var version int
		var langs, locs, cats, ents []string
		var searchText string
		err := s.db.Pool.QueryRow(ctx, `SELECT e.titles, e.summaries, e.state, e.status, e.last_material_update_at, e.version, e.languages, e.locations, e.search_text,
			ARRAY(SELECT DISTINCT category_id::text FROM event_categories WHERE event_id=e.id),
			ARRAY(SELECT en.normalized_key FROM event_entities ee JOIN entities en ON en.id=ee.entity_id WHERE ee.event_id=e.id)
			FROM events e WHERE e.id=$1`, id).Scan(&titles, &summaries, &state, &status, &upd, &version, &langs, &locs, &searchText, &cats, &ents)
		if db.IsNoRows(err) {
			d.deleted = true
			return d, nil
		}
		if err != nil {
			return nil, err
		}
		if status != "active" {
			d.deleted = true
			return d, nil
		}
		var t map[string]string
		_ = json.Unmarshal(titles, &t)
		title := t["ro"]
		if title == "" {
			for _, v := range t {
				title = v
				break
			}
		}
		lang := "ro"
		if len(langs) > 0 {
			lang = langs[0]
		}
		d.lex = LexicalDoc{ID: id.String(), DocType: "event", Title: title, Body: searchText, Language: lang, PublishedAt: upd, CategoryIDs: nonNil(cats),
			Regions: nonNil(locs), EntityKeys: nonNil(ents), Status: state, Version: version}
		d.embedOK = true
		d.embedText = nlp.Truncate(searchText, 3000)
		d.payload = map[string]any{"doc_type": "event", "languages": nonNil(langs), "category_ids": nonNil(cats), "published_ts": unix(upd),
			"status": state, "entity_keys": nonNil(ents), "version": version}
	case "claim":
		var text, lang, status string
		var seen time.Time
		var version int
		var verdict *string
		err := s.db.Pool.QueryRow(ctx, `SELECT c.canonical_text, c.language, c.status, c.last_seen_at, c.version,
			(SELECT verdict FROM claim_verdicts v WHERE v.id=c.current_verdict_id) FROM claims c WHERE c.id=$1`, id).Scan(&text, &lang, &status, &seen, &version, &verdict)
		if db.IsNoRows(err) {
			d.deleted = true
			return d, nil
		}
		if err != nil {
			return nil, err
		}
		v := ""
		if verdict != nil {
			v = *verdict
		}
		d.lex = LexicalDoc{ID: id.String(), DocType: "claim", Title: text, Body: text, Language: lang, PublishedAt: &seen, Status: status, Verdict: v,
			Version: version, CategoryIDs: []string{}, Regions: []string{}, EntityKeys: []string{}}
		d.embedOK = true
		d.embedText = text
		d.payload = map[string]any{"doc_type": "claim", "language": lang, "status": status, "verdict": v, "published_ts": float64(seen.Unix()), "version": version}
	default:
		return nil, fmt.Errorf("unknown doc type %s", docType)
	}
	return d, nil
}

func unix(t *time.Time) float64 {
	if t == nil {
		return 0
	}
	return float64(t.Unix())
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (s *Service) vectorPoint(ctx context.Context, docType string, id uuid.UUID, m *ai.Model) (*VectorPoint, bool, error) {
	d, err := s.load(ctx, docType, id)
	if err != nil {
		return nil, false, err
	}
	if d.deleted || !d.embedOK {
		return nil, false, nil
	}
	vecs, err := s.gw.EmbedTexts(ctx, m, []string{d.embedText}, ai.CallOpts{SubjectType: docType, SubjectID: id.String()})
	if err != nil {
		return nil, false, err
	}
	d.payload["embedding_model"] = m.Ref()
	return &VectorPoint{ID: id, Vector: vecs[0], Payload: d.payload}, true, nil
}

// IndexDoc projects one canonical record into OpenSearch and Qdrant (all write targets). It is
// idempotent and version-guarded via search_index_state.
func (s *Service) IndexDoc(ctx context.Context, docType string, id uuid.UUID, force bool) error {
	d, err := s.load(ctx, docType, id)
	if err != nil {
		return err
	}
	targets, terr := s.writeTargets(ctx, docType)
	if d.deleted {
		_ = s.lex.Delete(ctx, docType, id.String())
		for _, t := range targets {
			_ = s.vec.Delete(ctx, t.Name, []uuid.UUID{id})
		}
		_, err := s.db.Pool.Exec(ctx, `DELETE FROM search_index_state WHERE doc_type=$1 AND doc_id=$2`, docType, id)
		return err
	}
	if !force && s.upToDate(ctx, docType, id, "lexical", d.lex.Version) && s.allVectorsUpToDate(ctx, docType, id, targets, d.lex.Version) {
		return nil
	}
	if err := s.lex.Index(ctx, d.lex); err != nil {
		return fmt.Errorf("lexical index: %w", err)
	}
	s.markIndexed(ctx, docType, id, "lexical", d.lex.Version)
	if terr != nil {
		return fmt.Errorf("vector targets: %w", terr)
	}
	if !d.embedOK {
		return nil
	}
	for _, t := range targets {
		if !force && s.upToDate(ctx, docType, id, "qdrant:"+t.Name, d.lex.Version) {
			continue
		}
		m, err := s.modelByID(ctx, t.ModelID)
		if err != nil {
			return err
		}
		vecs, err := s.gw.EmbedTexts(ctx, m, []string{d.embedText}, ai.CallOpts{SubjectType: docType, SubjectID: id.String()})
		if err != nil {
			return err
		}
		payload := map[string]any{}
		for k, v := range d.payload {
			payload[k] = v
		}
		payload["embedding_model"] = m.Ref()
		if err := s.vec.Upsert(ctx, t.Name, []VectorPoint{{ID: id, Vector: vecs[0], Payload: payload}}); err != nil {
			return fmt.Errorf("vector upsert: %w", err)
		}
		s.markIndexed(ctx, docType, id, "qdrant:"+t.Name, d.lex.Version)
	}
	return nil
}

func (s *Service) upToDate(ctx context.Context, docType string, id uuid.UUID, backend string, version int) bool {
	var v int
	err := s.db.Pool.QueryRow(ctx, `SELECT indexed_version FROM search_index_state WHERE doc_type=$1 AND doc_id=$2 AND backend=$3`, docType, id, backend).Scan(&v)
	return err == nil && v >= version
}

func (s *Service) allVectorsUpToDate(ctx context.Context, docType string, id uuid.UUID, ts []Collection, version int) bool {
	for _, t := range ts {
		if !s.upToDate(ctx, docType, id, "qdrant:"+t.Name, version) {
			return false
		}
	}
	return true
}

func (s *Service) markIndexed(ctx context.Context, docType string, id uuid.UUID, backend string, version int) {
	// GREATEST guards against a slower, older write lowering the recorded version.
	_, _ = s.db.Pool.Exec(ctx, `INSERT INTO search_index_state (doc_type, doc_id, backend, indexed_version, schema_version) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (doc_type, doc_id, backend) DO UPDATE SET indexed_version=GREATEST(search_index_state.indexed_version, EXCLUDED.indexed_version),
		schema_version=EXCLUDED.schema_version, indexed_at=now()`, docType, id, backend, version, LexicalSchemaVersion)
}

// IndexRequest is the search.index.requested.v1 payload.
type IndexRequest struct {
	DocType string    `json:"doc_type"`
	ID      uuid.UUID `json:"id"`
	Force   bool      `json:"force"`
}

// HandleMessage is the indexer consumer.
func (s *Service) HandleMessage(ctx context.Context, d *bus.Delivery) error {
	if done, err := d.AlreadyProcessed(ctx); err != nil || done {
		return err
	}
	var docType string
	var id uuid.UUID
	force := false
	switch d.Message.Type {
	case bus.ArticleEnriched:
		docType = "article"
	case bus.EventCreated, bus.EventUpdated:
		docType = "event"
	case bus.ClaimVerdictUpdated:
		docType = "claim"
	case bus.SearchIndexRequested, bus.ContentDeleted:
		var r IndexRequest
		if err := d.Message.Decode(&r); err != nil {
			return bus.PermanentErr(err)
		}
		docType, id, force = r.DocType, r.ID, r.Force
	default:
		return nil
	}
	if id == uuid.Nil {
		var err error
		id, err = uuid.Parse(d.Message.AggregateID)
		if err != nil {
			return bus.PermanentErr(err)
		}
	}
	if err := s.IndexDoc(ctx, docType, id, force); err != nil {
		if errors.Is(err, ai.ErrDeferred) {
			return err
		}
		return err
	}
	return d.Once(ctx, func(pgx.Tx) error { return nil })
}

const JobReindex = "search.reindex"

// RunReindex rebuilds projections for a doc type from canonical records (checkpointed by id).
func (s *Service) RunReindex(ctx context.Context, j *jobs.Job) (any, error) {
	var p struct {
		DocTypes []string `json:"doc_types"`
	}
	_ = json.Unmarshal(j.Payload, &p)
	if len(p.DocTypes) == 0 {
		p.DocTypes = DocTypes
	}
	if err := s.lex.Ensure(ctx); err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, dt := range p.DocTypes {
		idList, err := s.allIDs(ctx, dt)
		if err != nil {
			return nil, err
		}
		for _, id := range idList {
			if err := s.IndexDoc(ctx, dt, id, true); err != nil {
				return nil, fmt.Errorf("reindex %s %s: %w", dt, id, err)
			}
			counts[dt]++
		}
	}
	return counts, nil
}

// ---------- Query ----------

type Query struct {
	Text         string     `json:"q"`
	Mode         string     `json:"mode"` // lexical | semantic | hybrid
	DocTypes     []string   `json:"types"`
	Languages    []string   `json:"languages"`
	CategoryIDs  []string   `json:"category_ids"`
	PublisherIDs []string   `json:"publisher_ids"`
	Regions      []string   `json:"regions"`
	EntityKeys   []string   `json:"entities"`
	Statuses     []string   `json:"statuses"`
	Verdicts     []string   `json:"verdicts"`
	From, To     *time.Time `json:"-"`
	Limit        int        `json:"limit"`
	Rerank       bool       `json:"rerank"`
}

type Hit struct {
	DocType      string     `json:"type"`
	ID           uuid.UUID  `json:"id"`
	Score        float64    `json:"score"`
	LexicalRank  *int       `json:"lexical_rank,omitempty"`
	SemanticRank *int       `json:"semantic_rank,omitempty"`
	Title        string     `json:"title"`
	Snippet      string     `json:"snippet"`
	PublishedAt  *time.Time `json:"published_at,omitempty"`
	URL          string     `json:"url,omitempty"`
	Publisher    string     `json:"publisher,omitempty"`
	EventID      *uuid.UUID `json:"event_id,omitempty"`
	Verdict      string     `json:"verdict,omitempty"`
}

type Result struct {
	Query       string    `json:"query"`
	Mode        string    `json:"mode"`
	Items       []Hit     `json:"items"`
	Degraded    []string  `json:"degraded"`
	RetrievedAt time.Time `json:"retrieved_at"`
}

// RRF fuses ranked lists: score(d) = Σ 1/(k + rank_i(d)).
func RRF(k int, lists ...[]string) map[string]float64 {
	out := map[string]float64{}
	for _, l := range lists {
		for i, id := range l {
			out[id] += 1.0 / float64(k+i+1)
		}
	}
	return out
}

var tagRe = regexp.MustCompile(`<[^>]+>`)

func (s *Service) Search(ctx context.Context, q Query) (*Result, error) {
	q.Text = strings.TrimSpace(q.Text)
	if q.Text == "" || len(q.Text) > 500 {
		return nil, apperr.Validation(apperr.FieldError{Field: "q", Code: "invalid", Message: "Query must be 1-500 characters."})
	}
	if q.Mode == "" {
		q.Mode = "hybrid"
	}
	if q.Mode != "hybrid" && q.Mode != "lexical" && q.Mode != "semantic" {
		return nil, apperr.Validation(apperr.FieldError{Field: "mode", Code: "invalid", Message: "mode must be lexical, semantic or hybrid."})
	}
	if q.Limit <= 0 || q.Limit > 50 {
		q.Limit = 20
	}
	if len(q.DocTypes) == 0 {
		q.DocTypes = []string{"event", "claim"}
	}
	for _, t := range q.DocTypes {
		if t != "event" && t != "article" && t != "claim" {
			return nil, apperr.Validation(apperr.FieldError{Field: "types", Code: "invalid", Message: "types must be event, article or claim."})
		}
	}
	var cfg struct {
		RRFK      int `json:"rrf_k"`
		LexicalK  int `json:"lexical_k"`
		SemanticK int `json:"semantic_k"`
	}
	cfg.RRFK, cfg.LexicalK, cfg.SemanticK = 60, 50, 50
	_, _ = s.pol.Get(ctx, "search", &cfg)
	res := &Result{Query: q.Text, Mode: q.Mode, Degraded: append([]string{}, s.degraded...), RetrievedAt: time.Now().UTC()}
	snippets := map[string]string{}
	lexRank, semRank := map[string]int{}, map[string]int{}
	var lexList, semList []string
	if q.Mode != "semantic" {
		hits, err := s.lex.Search(ctx, LexicalQuery{Text: q.Text, DocTypes: q.DocTypes, Languages: q.Languages, CategoryIDs: q.CategoryIDs,
			PublisherIDs: q.PublisherIDs, Regions: q.Regions, EntityKeys: q.EntityKeys, Statuses: q.Statuses, Verdicts: q.Verdicts, From: q.From, To: q.To, Limit: cfg.LexicalK})
		if err != nil {
			s.log.Warn("lexical search unavailable", "error", err)
			res.Degraded = append(res.Degraded, "lexical:unavailable")
		}
		sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
		for i, h := range hits {
			key := h.DocType + ":" + h.ID
			if _, ok := lexRank[key]; ok {
				continue
			}
			lexRank[key] = i + 1
			lexList = append(lexList, key)
			snippets[key] = strings.TrimSpace(tagRe.ReplaceAllString(h.Snippet, ""))
		}
	}
	if q.Mode != "lexical" {
		var all []VectorHit
		m, err := s.gw.EmbeddingModel(ctx)
		var vec []float32
		if err == nil {
			for _, dt := range q.DocTypes {
				col, cerr := s.ActiveCollection(ctx, dt)
				if cerr != nil {
					err = cerr
					break
				}
				if col.ModelID != m.ID {
					// Query must be embedded with the collection's model.
					cm, merr := s.modelByID(ctx, col.ModelID)
					if merr != nil {
						err = merr
						break
					}
					vs, eerr := s.gw.EmbedTexts(ctx, cm, []string{q.Text}, ai.CallOpts{SubjectType: "search_query"})
					if eerr != nil {
						err = eerr
						break
					}
					vec = vs[0]
				} else if vec == nil {
					vs, eerr := s.gw.EmbedTexts(ctx, m, []string{q.Text}, ai.CallOpts{SubjectType: "search_query"})
					if eerr != nil {
						err = eerr
						break
					}
					vec = vs[0]
				}
				f := VectorFilter{MustAny: map[string][]string{}, RangeGTE: map[string]float64{}, RangeLTE: map[string]float64{}}
				if len(q.CategoryIDs) > 0 {
					f.MustAny["category_ids"] = q.CategoryIDs
				}
				if len(q.Languages) > 0 && dt != "event" {
					f.MustAny["language"] = q.Languages
				}
				if len(q.PublisherIDs) > 0 && dt == "article" {
					f.MustAny["publisher_id"] = q.PublisherIDs
				}
				if len(q.EntityKeys) > 0 {
					f.MustAny["entity_keys"] = q.EntityKeys
				}
				if q.From != nil {
					f.RangeGTE["published_ts"] = float64(q.From.Unix())
				}
				if q.To != nil {
					f.RangeLTE["published_ts"] = float64(q.To.Unix())
				}
				hits, serr := s.vec.Search(ctx, col.Name, vec, f, cfg.SemanticK)
				if serr != nil {
					err = serr
					break
				}
				for i := range hits {
					hits[i].Payload["_doc_type"] = dt
				}
				all = append(all, hits...)
			}
		}
		if err != nil {
			s.log.Warn("semantic search unavailable", "error", err)
			res.Degraded = append(res.Degraded, "semantic:unavailable")
		}
		sort.SliceStable(all, func(i, j int) bool { return all[i].Score > all[j].Score })
		for i, h := range all {
			if h.Score < 0.05 {
				continue
			}
			key := fmt.Sprint(h.Payload["_doc_type"]) + ":" + h.ID.String()
			semRank[key] = i + 1
			semList = append(semList, key)
		}
	}
	fused := RRF(cfg.RRFK, lexList, semList)
	keys := make([]string, 0, len(fused))
	for k := range fused {
		keys = append(keys, k)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		if fused[keys[i]] != fused[keys[j]] {
			return fused[keys[i]] > fused[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > q.Limit*2 {
		keys = keys[:q.Limit*2]
	}
	for _, k := range keys {
		dt, idStr, _ := strings.Cut(k, ":")
		id, err := uuid.Parse(idStr)
		if err != nil {
			continue
		}
		h, ok, err := s.resolve(ctx, dt, id)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue // stale projection: canonical record gone or hidden
		}
		h.Score = fused[k]
		if r, ok := lexRank[k]; ok {
			h.LexicalRank = &r
		}
		if r, ok := semRank[k]; ok {
			h.SemanticRank = &r
		}
		if sn := snippets[k]; sn != "" {
			h.Snippet = sn
		}
		res.Items = append(res.Items, *h)
	}
	if q.Rerank && len(res.Items) > 1 {
		docs := make([]string, len(res.Items))
		for i, it := range res.Items {
			docs[i] = it.Title + ". " + it.Snippet
		}
		if scores, err := s.gw.Rerank(ctx, q.Text, docs); err == nil {
			for i := range res.Items {
				res.Items[i].Score = 0.5*res.Items[i].Score*float64(cfg.RRFK) + 0.5*scores[i]
			}
			sort.SliceStable(res.Items, func(i, j int) bool { return res.Items[i].Score > res.Items[j].Score })
		}
	}
	if len(res.Items) > q.Limit {
		res.Items = res.Items[:q.Limit]
	}
	if res.Items == nil {
		res.Items = []Hit{}
	}
	return res, nil
}

// resolve maps an index hit back to its canonical relational record (citations always point to
// DB records, never to index contents).
func (s *Service) resolve(ctx context.Context, dt string, id uuid.UUID) (*Hit, bool, error) {
	h := &Hit{DocType: dt, ID: id}
	var err error
	switch dt {
	case "event":
		var titles, summaries []byte
		err = s.db.Pool.QueryRow(ctx, `SELECT titles, summaries, last_material_update_at FROM events WHERE id=$1 AND status='active'`, id).Scan(&titles, &summaries, &h.PublishedAt)
		if err == nil {
			var t map[string]string
			_ = json.Unmarshal(titles, &t)
			h.Title = firstOf(t, "ro", "ru", "en")
			var sm map[string]map[string]any
			_ = json.Unmarshal(summaries, &sm)
			for _, l := range []string{"ro", "ru", "en"} {
				if v, ok := sm[l]["short"].(string); ok && v != "" {
					h.Snippet = v
					break
				}
			}
		}
	case "article":
		var eventID *uuid.UUID
		err = s.db.Pool.QueryRow(ctx, `SELECT a.title, a.description, a.published_at, a.canonical_url, p.name,
			(SELECT event_id FROM event_article_links l WHERE l.article_id=a.id AND l.unlinked_at IS NULL AND relation IN ('origin','same_story','follow_up') LIMIT 1)
			FROM articles a JOIN publishers p ON p.id=a.publisher_id WHERE a.id=$1 AND a.status='enriched' AND a.tombstoned_at IS NULL`, id).
			Scan(&h.Title, &h.Snippet, &h.PublishedAt, &h.URL, &h.Publisher, &eventID)
		h.EventID = eventID
	case "claim":
		var v *string
		err = s.db.Pool.QueryRow(ctx, `SELECT c.canonical_text, c.last_seen_at,
			(SELECT CASE WHEN visibility='public' THEN verdict ELSE 'unverified' END FROM claim_verdicts v WHERE v.id=c.current_verdict_id)
			FROM claims c WHERE c.id=$1`, id).Scan(&h.Title, &h.PublishedAt, &v)
		if v != nil {
			h.Verdict = *v
		}
	}
	if db.IsNoRows(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	h.Snippet = nlp.Truncate(h.Snippet, 300)
	return h, true, nil
}

func firstOf(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := m[k]; v != "" {
			return v
		}
	}
	for _, v := range m {
		return v
	}
	return ""
}

// SimilarByVector returns nearest neighbours in the active collection of a doc type.
func (s *Service) SimilarByVector(ctx context.Context, docType string, vec []float32, f VectorFilter, k int) ([]VectorHit, error) {
	col, err := s.ActiveCollection(ctx, docType)
	if err != nil {
		return nil, err
	}
	return s.vec.Search(ctx, col.Name, vec, f, k)
}

// EmbedActive embeds text with the model of the active collection for docType.
func (s *Service) EmbedActive(ctx context.Context, docType, text string) ([]float32, string, error) {
	col, err := s.ActiveCollection(ctx, docType)
	if err != nil {
		return nil, "", err
	}
	m, err := s.modelByID(ctx, col.ModelID)
	if err != nil {
		return nil, "", err
	}
	v, err := s.gw.EmbedTexts(ctx, m, []string{text}, ai.CallOpts{SubjectType: docType})
	if err != nil {
		return nil, "", err
	}
	return v[0], m.Ref(), nil
}

// RequestIndexTx enqueues a projection refresh inside the caller's transaction.
func RequestIndexTx(ctx context.Context, tx pgx.Tx, docType string, id uuid.UUID, force bool) error {
	_, err := bus.Publish(ctx, tx, bus.Draft{Type: bus.SearchIndexRequested, AggregateType: docType, AggregateID: id.String(),
		Payload: IndexRequest{DocType: docType, ID: id, Force: force}})
	return err
}
