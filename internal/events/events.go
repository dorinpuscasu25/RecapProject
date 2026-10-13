// Package events implements event intelligence: matching articles to evolving real-world events,
// event creation/versioning, novelty and contradiction detection, cited multi-language
// synthesis, timelines and reversible editorial merge/split/correct operations.
package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dorinpuscasu25/recapproject/internal/ai"
	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/policy"
	"github.com/dorinpuscasu25/recapproject/internal/search"
)

type Service struct {
	db     *db.DB
	gw     *ai.Gateway
	search *search.Service
	pol    *policy.Store
	log    *slog.Logger

	embMu    sync.Mutex
	embCache map[string][]float32 // event id + version → vector (bounded)
}

func NewService(d *db.DB, gw *ai.Gateway, s *search.Service, pol *policy.Store, log *slog.Logger) *Service {
	return &Service{db: d, gw: gw, search: s, pol: pol, log: log, embCache: map[string][]float32{}}
}

// MatchPolicy is the DB-managed event_matching policy.
type MatchPolicy struct {
	TimeWindowHours     int     `json:"time_window_hours"`
	MaxCandidates       int     `json:"max_candidates"`
	MinCandidateScore   float64 `json:"min_candidate_score"`
	SameStoryScore      float64 `json:"same_story_score"`
	AmbiguousLow        float64 `json:"ambiguous_low"`
	UseLLMForAmbiguous  bool    `json:"use_llm_for_ambiguous"`
	RequireSharedAnchor bool    `json:"require_shared_anchor"`
	Weights             struct {
		Semantic float64 `json:"semantic"`
		Entities float64 `json:"entities"`
		Facts    float64 `json:"facts"`
		Time     float64 `json:"time"`
	} `json:"weights"`
}

func (s *Service) matchPolicy(ctx context.Context) MatchPolicy {
	p := MatchPolicy{TimeWindowHours: 120, MaxCandidates: 12, MinCandidateScore: 0.22, SameStoryScore: 0.5, AmbiguousLow: 0.3,
		UseLLMForAmbiguous: true, RequireSharedAnchor: true}
	p.Weights.Semantic, p.Weights.Entities, p.Weights.Facts, p.Weights.Time = 0.45, 0.25, 0.2, 0.1
	_, _ = s.pol.Get(ctx, "event_matching", &p)
	return p
}

// ---------- Read models ----------

type Citation struct {
	Ref         string     `json:"ref"`
	ArticleID   uuid.UUID  `json:"article_id"`
	Publisher   string     `json:"publisher"`
	URL         string     `json:"url"`
	Title       string     `json:"title"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	Language    string     `json:"language"`
}

type Summary struct {
	Title        string     `json:"title"`
	Short        string     `json:"short"`
	Normal       string     `json:"normal"`
	Detailed     string     `json:"detailed"`
	Disputed     []string   `json:"disputed_points"`
	Citations    []Citation `json:"citations"`
	ModelRef     string     `json:"model_ref"`
	GeneratedAt  time.Time  `json:"generated_at"`
	EventVersion int        `json:"event_version"`
}

type CategoryRef struct {
	ID        uuid.UUID         `json:"id"`
	Slug      string            `json:"slug"`
	Names     map[string]string `json:"names"`
	IsPrimary bool              `json:"is_primary"`
	Source    string            `json:"source"`
}

type EntityRef struct {
	ID   uuid.UUID `json:"id"`
	Kind string    `json:"kind"`
	Name string    `json:"name"`
}

type Event struct {
	ID                   uuid.UUID         `json:"id"`
	Title                string            `json:"title"`
	Titles               map[string]string `json:"titles"`
	Language             string            `json:"language"`
	LanguageFallback     bool              `json:"language_fallback"`
	Summary              *Summary          `json:"summary,omitempty"`
	EventType            string            `json:"event_type"`
	State                string            `json:"state"`
	Status               string            `json:"status"`
	MergedInto           *uuid.UUID        `json:"merged_into,omitempty"`
	OccurredStart        *time.Time        `json:"occurred_start,omitempty"`
	OccurredEnd          *time.Time        `json:"occurred_end,omitempty"`
	EarliestPublishedAt  *time.Time        `json:"earliest_published_at,omitempty"`
	LastMaterialUpdateAt *time.Time        `json:"last_material_update_at,omitempty"`
	Locations            []string          `json:"locations"`
	Categories           []CategoryRef     `json:"categories"`
	Entities             []EntityRef       `json:"entities"`
	Importance           float32           `json:"importance"`
	EditorialStatus      string            `json:"editorial_status"`
	Confidence           float32           `json:"confidence"`
	ArticleCount         int               `json:"article_count"`
	SourceCount          int               `json:"source_count"`
	CitationCoverage     float32           `json:"citation_coverage"`
	Languages            []string          `json:"languages"`
	Version              int               `json:"version"`
	CreatedAt            time.Time         `json:"created_at"`
	UpdatedAt            time.Time         `json:"updated_at"`
	PublishedAtNote      string            `json:"time_semantics"`
	summaries            map[string]Summary
}

const timeNote = "earliest_published_at = first publication; occurred_start/end = when the event happened (if known); last_material_update_at = last verified material change; summaries may lag newer sources."

func pick(m map[string]string, lang string) (string, string, bool) {
	if v := m[lang]; v != "" {
		return v, lang, false
	}
	for _, l := range []string{"ro", "ru", "en"} {
		if v := m[l]; v != "" {
			return v, l, true
		}
	}
	for l, v := range m {
		return v, l, true
	}
	return "", lang, true
}

// Get returns an event localized to lang (falls back to an available language explicitly).
func (s *Service) Get(ctx context.Context, id uuid.UUID, lang string) (*Event, error) {
	e := &Event{ID: id, PublishedAtNote: timeNote}
	var titles, summaries []byte
	err := s.db.Pool.QueryRow(ctx, `SELECT titles, summaries, event_type, state, status, merged_into, occurred_start, occurred_end, earliest_published_at,
		last_material_update_at, locations, importance, editorial_status, confidence, article_count, source_count, citation_coverage, languages, version, created_at, updated_at
		FROM events WHERE id=$1`, id).Scan(&titles, &summaries, &e.EventType, &e.State, &e.Status, &e.MergedInto, &e.OccurredStart, &e.OccurredEnd,
		&e.EarliestPublishedAt, &e.LastMaterialUpdateAt, &e.Locations, &e.Importance, &e.EditorialStatus, &e.Confidence, &e.ArticleCount, &e.SourceCount,
		&e.CitationCoverage, &e.Languages, &e.Version, &e.CreatedAt, &e.UpdatedAt)
	if db.IsNoRows(err) {
		return nil, apperr.NotFound("event_not_found", "Event not found.")
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(titles, &e.Titles)
	_ = json.Unmarshal(summaries, &e.summaries)
	e.Title, e.Language, e.LanguageFallback = pick(e.Titles, lang)
	if sm, ok := e.summaries[lang]; ok {
		e.Summary = &sm
		e.Language = lang
		e.LanguageFallback = false
	} else {
		for _, l := range []string{"ro", "ru", "en"} {
			if sm, ok := e.summaries[l]; ok {
				e.Summary = &sm
				e.Language, e.LanguageFallback = l, true
				break
			}
		}
	}
	rows, err := s.db.Pool.Query(ctx, `SELECT c.id, c.slug::text, c.names, ec.is_primary, ec.source FROM event_categories ec JOIN categories c ON c.id=ec.category_id
		WHERE ec.event_id=$1 AND c.status <> 'merged' ORDER BY ec.is_primary DESC, c.display_order`, id)
	if err != nil {
		return nil, err
	}
	seen := map[uuid.UUID]bool{}
	for rows.Next() {
		var c CategoryRef
		var names []byte
		if err := rows.Scan(&c.ID, &c.Slug, &names, &c.IsPrimary, &c.Source); err != nil {
			rows.Close()
			return nil, err
		}
		_ = json.Unmarshal(names, &c.Names)
		if !seen[c.ID] {
			seen[c.ID] = true
			e.Categories = append(e.Categories, c)
		}
	}
	rows.Close()
	rows, err = s.db.Pool.Query(ctx, `SELECT en.id, en.kind, en.canonical_name FROM event_entities ee JOIN entities en ON en.id=ee.entity_id
		WHERE ee.event_id=$1 ORDER BY ee.weight DESC LIMIT 20`, id)
	if err != nil {
		return nil, err
	}
	e.Entities, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (EntityRef, error) {
		var x EntityRef
		err := r.Scan(&x.ID, &x.Kind, &x.Name)
		return x, err
	})
	if e.Categories == nil {
		e.Categories = []CategoryRef{}
	}
	if e.Entities == nil {
		e.Entities = []EntityRef{}
	}
	return e, err
}

type Source struct {
	ArticleID   uuid.UUID  `json:"article_id"`
	PublisherID uuid.UUID  `json:"publisher_id"`
	Publisher   string     `json:"publisher"`
	Title       string     `json:"title"`
	URL         string     `json:"url"`
	Language    string     `json:"language"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	FirstSeenAt time.Time  `json:"first_seen_at"`
	Relation    string     `json:"relation"`
	Confidence  float32    `json:"match_confidence"`
	Snippet     string     `json:"snippet,omitempty"` // only if the source permits snippets
	Attribution string     `json:"attribution,omitempty"`
	ContentType string     `json:"content_type"`
	Duplicates  int        `json:"syndicated_copies"`
}

// Sources lists coverage with attribution and original links (snippets only where permitted).
func (s *Service) Sources(ctx context.Context, eventID uuid.UUID) ([]Source, error) {
	rows, err := s.db.Pool.Query(ctx, `SELECT a.id, a.publisher_id, p.name, a.title, a.canonical_url, a.language, a.published_at, a.first_seen_at, l.relation, l.confidence,
		a.description, a.content_type,
		COALESCE(perm.allow_snippets,false), COALESCE(perm.max_snippet_chars,0), COALESCE(perm.attribution_text,''),
		(SELECT count(*) FROM articles d WHERE d.duplicate_of=a.id)
		FROM event_article_links l JOIN articles a ON a.id=l.article_id JOIN publishers p ON p.id=a.publisher_id
		LEFT JOIN LATERAL (SELECT allow_snippets, max_snippet_chars, attribution_text FROM source_permissions sp
			WHERE sp.publisher_id=a.publisher_id AND (sp.source_endpoint_id=a.source_endpoint_id OR sp.source_endpoint_id IS NULL)
			ORDER BY sp.source_endpoint_id NULLS LAST LIMIT 1) perm ON true
		WHERE l.event_id=$1 AND l.unlinked_at IS NULL AND a.tombstoned_at IS NULL
		ORDER BY a.published_at NULLS LAST, a.first_seen_at`, eventID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Source, error) {
		var x Source
		var desc string
		var allow bool
		var max int
		err := r.Scan(&x.ArticleID, &x.PublisherID, &x.Publisher, &x.Title, &x.URL, &x.Language, &x.PublishedAt, &x.FirstSeenAt, &x.Relation, &x.Confidence,
			&desc, &x.ContentType, &allow, &max, &x.Attribution, &x.Duplicates)
		if allow && max > 0 {
			x.Snippet = truncRunes(desc, max)
		}
		return x, err
	})
}

type TimelineEntry struct {
	ID                   uuid.UUID       `json:"id"`
	EventVersion         int             `json:"event_version"`
	Kind                 string          `json:"kind"`
	WhatChanged          json.RawMessage `json:"what_changed"`
	Severity             string          `json:"severity"`
	SupportingArticleIDs []uuid.UUID     `json:"supporting_article_ids"`
	AffectedClaimIDs     []uuid.UUID     `json:"affected_claim_ids"`
	OccurredAt           *time.Time      `json:"occurred_at,omitempty"`
	RecordedAt           time.Time       `json:"recorded_at"`
	RequiresApproval     bool            `json:"requires_approval"`
	ApprovedAt           *time.Time      `json:"approved_at,omitempty"`
}

// Timeline returns append-only material updates (unapproved high-impact entries hidden unless includeUnapproved).
func (s *Service) Timeline(ctx context.Context, eventID uuid.UUID, includeUnapproved bool) ([]TimelineEntry, error) {
	rows, err := s.db.Pool.Query(ctx, `SELECT id, event_version, update_kind, what_changed, severity, supporting_article_ids, affected_claim_ids, occurred_at,
		created_at, requires_approval, approved_at FROM event_updates WHERE event_id=$1 AND ($2 OR NOT requires_approval OR approved_at IS NOT NULL)
		ORDER BY created_at, event_version`, eventID, includeUnapproved)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (TimelineEntry, error) {
		var t TimelineEntry
		err := r.Scan(&t.ID, &t.EventVersion, &t.Kind, &t.WhatChanged, &t.Severity, &t.SupportingArticleIDs, &t.AffectedClaimIDs, &t.OccurredAt,
			&t.RecordedAt, &t.RequiresApproval, &t.ApprovedAt)
		return t, err
	})
}

type ListFilter struct {
	CategoryIDs []uuid.UUID
	Since       *time.Time
	Until       *time.Time
	States      []string
	Region      string
	Lang        string
	Before      *time.Time // cursor
	BeforeID    *uuid.UUID
	Limit       int
}

// List returns active events ordered by last material update (keyset pagination).
func (s *Service) List(ctx context.Context, f ListFilter) ([]Event, error) {
	if f.Limit <= 0 || f.Limit > 100 {
		f.Limit = 20
	}
	cats := f.CategoryIDs
	if cats == nil {
		cats = []uuid.UUID{}
	}
	states := f.States
	if states == nil {
		states = []string{}
	}
	rows, err := s.db.Pool.Query(ctx, `SELECT e.id FROM events e WHERE e.status='active'
		AND (cardinality($1::uuid[])=0 OR EXISTS (SELECT 1 FROM event_categories ec WHERE ec.event_id=e.id AND ec.category_id=ANY($1)))
		AND ($2::timestamptz IS NULL OR e.last_material_update_at >= $2) AND ($3::timestamptz IS NULL OR e.last_material_update_at <= $3)
		AND (cardinality($4::text[])=0 OR e.state=ANY($4)) AND ($5='' OR $5=ANY(e.locations))
		AND ($6::timestamptz IS NULL OR (e.last_material_update_at, e.id) < ($6, $7::uuid))
		ORDER BY e.last_material_update_at DESC, e.id DESC LIMIT $8`,
		cats, f.Since, f.Until, states, f.Region, f.Before, f.BeforeID, f.Limit)
	if err != nil {
		return nil, err
	}
	idList, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(idList))
	for _, id := range idList {
		e, err := s.Get(ctx, id, f.Lang)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, nil
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
