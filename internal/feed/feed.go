// Package feed implements user preferences, interests (follows/mutes), onboarding, per-user
// event state and the personalized feed / "what did I miss?" catch-up. Ranking works on
// precomputed event representations — no model calls on the request path.
package feed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dorinpuscasu25/recapproject/internal/events"
	"github.com/dorinpuscasu25/recapproject/internal/nlp"
	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ids"
	"github.com/dorinpuscasu25/recapproject/internal/policy"
	"github.com/dorinpuscasu25/recapproject/internal/taxonomy"
)

type Service struct {
	db     *db.DB
	events *events.Service
	tax    *taxonomy.Service
	pol    *policy.Store
	cache  Cache
	now    func() time.Time
}

// Cache stores ranked feed id lists (Redis in multi-replica deployments, memory otherwise).
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, bool)
	Set(ctx context.Context, key string, val []byte, ttl time.Duration)
}

type MemoryCache struct {
	mu sync.Mutex
	m  map[string]memEntry
}

type memEntry struct {
	v   []byte
	exp time.Time
}

func NewMemoryCache() *MemoryCache { return &MemoryCache{m: map[string]memEntry{}} }

func (c *MemoryCache) Get(_ context.Context, k string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok || time.Now().After(e.exp) {
		return nil, false
	}
	return e.v, true
}

func (c *MemoryCache) Set(_ context.Context, k string, v []byte, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) > 10000 {
		c.m = map[string]memEntry{}
	}
	c.m[k] = memEntry{v: v, exp: time.Now().Add(ttl)}
}

func NewService(d *db.DB, ev *events.Service, tax *taxonomy.Service, pol *policy.Store, cache Cache) *Service {
	if cache == nil {
		cache = NewMemoryCache()
	}
	return &Service{db: d, events: ev, tax: tax, pol: pol, cache: cache, now: time.Now}
}

// ---------- Preferences ----------

type Preferences struct {
	Languages              []string   `json:"languages"`
	SummaryLanguage        string     `json:"summary_language"`
	Regions                []string   `json:"regions"`
	AudioEnabled           bool       `json:"audio_enabled"`
	BriefingMinutes        int        `json:"briefing_minutes"`
	PersonalizationConsent bool       `json:"personalization_consent"`
	OnboardingCompletedAt  *time.Time `json:"onboarding_completed_at,omitempty"`
	Version                int        `json:"version"`
}

func (s *Service) GetPreferences(ctx context.Context, user uuid.UUID) (*Preferences, error) {
	p := &Preferences{}
	err := s.db.Pool.QueryRow(ctx, `SELECT languages, summary_language, regions, audio_enabled, briefing_minutes, personalization_consent, onboarding_completed_at, version
		FROM user_preferences WHERE user_id=$1`, user).Scan(&p.Languages, &p.SummaryLanguage, &p.Regions, &p.AudioEnabled, &p.BriefingMinutes,
		&p.PersonalizationConsent, &p.OnboardingCompletedAt, &p.Version)
	if db.IsNoRows(err) {
		if _, err := s.db.Pool.Exec(ctx, `INSERT INTO user_preferences (user_id) VALUES ($1) ON CONFLICT DO NOTHING`, user); err != nil {
			return nil, err
		}
		return s.GetPreferences(ctx, user)
	}
	return p, err
}

type PreferencesInput struct {
	Languages              []string `json:"languages,omitempty"`
	SummaryLanguage        *string  `json:"summary_language,omitempty"`
	Regions                []string `json:"regions,omitempty"`
	AudioEnabled           *bool    `json:"audio_enabled,omitempty"`
	BriefingMinutes        *int     `json:"briefing_minutes,omitempty"`
	PersonalizationConsent *bool    `json:"personalization_consent,omitempty"`
	Version                *int     `json:"version,omitempty"`
}

var langs = map[string]bool{"ro": true, "ru": true, "en": true}

func (s *Service) PutPreferences(ctx context.Context, user uuid.UUID, in PreferencesInput) (*Preferences, error) {
	cur, err := s.GetPreferences(ctx, user)
	if err != nil {
		return nil, err
	}
	var v apperr.Validator
	if in.Languages != nil {
		for _, l := range in.Languages {
			v.Check(langs[l], "languages", "invalid", "Supported languages: ro, ru, en.")
		}
		v.Check(len(in.Languages) > 0, "languages", "required", "At least one language.")
		cur.Languages = in.Languages
	}
	if in.SummaryLanguage != nil {
		v.Check(langs[*in.SummaryLanguage], "summary_language", "invalid", "Supported languages: ro, ru, en.")
		cur.SummaryLanguage = *in.SummaryLanguage
	}
	if in.Regions != nil {
		v.Check(len(in.Regions) <= 20, "regions", "too_many", "At most 20 regions.")
		var keys []string
		for _, r := range in.Regions {
			if k := nlp.EntityKey(r); k != "" {
				keys = append(keys, k)
			}
		}
		cur.Regions = nonNil(keys)
	}
	if in.AudioEnabled != nil {
		cur.AudioEnabled = *in.AudioEnabled
	}
	if in.BriefingMinutes != nil {
		v.Check(*in.BriefingMinutes == 2 || *in.BriefingMinutes == 5 || *in.BriefingMinutes == 10, "briefing_minutes", "invalid", "2, 5 or 10.")
		cur.BriefingMinutes = *in.BriefingMinutes
	}
	if in.PersonalizationConsent != nil {
		cur.PersonalizationConsent = *in.PersonalizationConsent
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	tag, err := s.db.Pool.Exec(ctx, `UPDATE user_preferences SET languages=$2, summary_language=$3, regions=$4, audio_enabled=$5, briefing_minutes=$6,
		personalization_consent=$7, version=version+1, updated_at=now() WHERE user_id=$1 AND ($8::int IS NULL OR version=$8)`,
		user, cur.Languages, cur.SummaryLanguage, cur.Regions, cur.AudioEnabled, cur.BriefingMinutes, cur.PersonalizationConsent, in.Version)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, apperr.VersionConflict("preferences")
	}
	return s.GetPreferences(ctx, user)
}

// ---------- Interests ----------

type Interest struct {
	ID        uuid.UUID `json:"id"`
	Type      string    `json:"interest_type"`
	TargetKey string    `json:"target_key"`
	Label     string    `json:"label,omitempty"`
	Mode      string    `json:"mode"`
	Weight    float32   `json:"weight"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Service) Interests(ctx context.Context, user uuid.UUID) ([]Interest, error) {
	rows, err := s.db.Pool.Query(ctx, `SELECT i.id, i.interest_type, i.target_key, i.mode, i.weight, i.created_at,
		COALESCE(CASE i.interest_type WHEN 'category' THEN (SELECT c.names->>'ro' FROM categories c WHERE c.id::text=i.target_key)
		  WHEN 'entity' THEN (SELECT e.canonical_name FROM entities e WHERE e.id::text=i.target_key)
		  WHEN 'publisher' THEN (SELECT p.name FROM publishers p WHERE p.id::text=i.target_key) ELSE i.target_key END, '')
		FROM user_interests i WHERE i.user_id=$1 ORDER BY i.interest_type, i.created_at`, user)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Interest, error) {
		var x Interest
		err := r.Scan(&x.ID, &x.Type, &x.TargetKey, &x.Mode, &x.Weight, &x.CreatedAt, &x.Label)
		return x, err
	})
}

type InterestInput struct {
	Type   string  `json:"interest_type"`
	Target string  `json:"target"`
	Mode   string  `json:"mode"`
	Weight float32 `json:"weight,omitempty"`
}

// AddInterest follows or mutes a category, entity, keyword, region or publisher. Users can only
// reference existing active categories (they never create global categories).
func (s *Service) AddInterest(ctx context.Context, user uuid.UUID, in InterestInput) (*Interest, error) {
	if in.Mode != "follow" && in.Mode != "mute" {
		return nil, apperr.Validation(apperr.FieldError{Field: "mode", Code: "invalid", Message: "follow or mute"})
	}
	if in.Weight == 0 {
		in.Weight = 1
	}
	if in.Weight < 0 || in.Weight > 5 {
		return nil, apperr.Validation(apperr.FieldError{Field: "weight", Code: "out_of_range", Message: "0..5"})
	}
	key := strings.TrimSpace(in.Target)
	switch in.Type {
	case "category":
		id, err := uuid.Parse(key)
		if err != nil {
			return nil, apperr.Validation(apperr.FieldError{Field: "target", Code: "invalid", Message: "Category id required."})
		}
		id, err = s.tax.Resolve(ctx, id)
		if err != nil {
			return nil, err
		}
		var status string
		if err := s.db.Pool.QueryRow(ctx, `SELECT status FROM categories WHERE id=$1`, id).Scan(&status); err != nil || status != "active" {
			return nil, apperr.ValidationMsg("category_not_available", "Category is not available.")
		}
		key = id.String()
	case "entity", "publisher":
		id, err := uuid.Parse(key)
		if err != nil {
			return nil, apperr.Validation(apperr.FieldError{Field: "target", Code: "invalid", Message: "Id required."})
		}
		table := map[string]string{"entity": "entities", "publisher": "publishers"}[in.Type]
		var ok bool
		_ = s.db.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+table+` WHERE id=$1)`, id).Scan(&ok)
		if !ok {
			return nil, apperr.ValidationMsg("target_not_found", "Target not found.")
		}
	case "keyword", "region":
		key = nlp.EntityKey(key)
		if len(key) < 2 || len(key) > 80 {
			return nil, apperr.Validation(apperr.FieldError{Field: "target", Code: "invalid", Message: "2..80 characters."})
		}
	default:
		return nil, apperr.Validation(apperr.FieldError{Field: "interest_type", Code: "invalid", Message: "category, entity, keyword, region or publisher"})
	}
	var count int
	_ = s.db.Pool.QueryRow(ctx, `SELECT count(*) FROM user_interests WHERE user_id=$1`, user).Scan(&count)
	if count >= 200 {
		return nil, apperr.ValidationMsg("too_many_interests", "Interest limit reached.")
	}
	var x Interest
	err := s.db.Pool.QueryRow(ctx, `INSERT INTO user_interests (id, user_id, interest_type, target_key, mode, weight) VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (user_id, interest_type, target_key) DO UPDATE SET mode=EXCLUDED.mode, weight=EXCLUDED.weight
		RETURNING id, interest_type, target_key, mode, weight, created_at`, ids.New(), user, in.Type, key, in.Mode, in.Weight).
		Scan(&x.ID, &x.Type, &x.TargetKey, &x.Mode, &x.Weight, &x.CreatedAt)
	return &x, err
}

func (s *Service) RemoveInterest(ctx context.Context, user, id uuid.UUID) error {
	tag, err := s.db.Pool.Exec(ctx, `DELETE FROM user_interests WHERE id=$1 AND user_id=$2`, id, user)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("interest_not_found", "Interest not found.")
	}
	return nil
}

// SetCategories replaces followed categories (onboarding) and marks onboarding complete.
func (s *Service) SetCategories(ctx context.Context, user uuid.UUID, cats []uuid.UUID) ([]Interest, error) {
	if len(cats) > 50 {
		return nil, apperr.ValidationMsg("too_many_categories", "At most 50 categories.")
	}
	resolved := map[uuid.UUID]bool{}
	for _, c := range cats {
		id, err := s.tax.Resolve(ctx, c)
		if err != nil {
			return nil, err
		}
		var status string
		if err := s.db.Pool.QueryRow(ctx, `SELECT status FROM categories WHERE id=$1`, id).Scan(&status); err != nil || status != "active" {
			return nil, apperr.Validation(apperr.FieldError{Field: "category_ids", Code: "not_available", Message: "Unknown or inactive category " + c.String()})
		}
		resolved[id] = true
	}
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM user_interests WHERE user_id=$1 AND interest_type='category' AND mode='follow'`, user); err != nil {
			return err
		}
		for id := range resolved {
			if _, err := tx.Exec(ctx, `INSERT INTO user_interests (id, user_id, interest_type, target_key, mode) VALUES ($1,$2,'category',$3,'follow')
				ON CONFLICT (user_id, interest_type, target_key) DO UPDATE SET mode='follow'`, ids.New(), user, id.String()); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO user_preferences (user_id, onboarding_completed_at) VALUES ($1, now())
			ON CONFLICT (user_id) DO UPDATE SET onboarding_completed_at=COALESCE(user_preferences.onboarding_completed_at, now()), version=user_preferences.version+1`, user)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.Interests(ctx, user)
}

// ---------- Event state ----------

func (s *Service) eventExists(ctx context.Context, id uuid.UUID) (int, error) {
	var v int
	err := s.db.Pool.QueryRow(ctx, `SELECT version FROM events WHERE id=$1 AND status='active'`, id).Scan(&v)
	if db.IsNoRows(err) {
		return 0, apperr.NotFound("event_not_found", "Event not found.")
	}
	return v, err
}

func (s *Service) Follow(ctx context.Context, user, event uuid.UUID, follow bool) error {
	if _, err := s.eventExists(ctx, event); err != nil {
		return err
	}
	var err error
	if follow {
		_, err = s.db.Pool.Exec(ctx, `INSERT INTO followed_events (user_id, event_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, user, event)
	} else {
		_, err = s.db.Pool.Exec(ctx, `DELETE FROM followed_events WHERE user_id=$1 AND event_id=$2`, user, event)
	}
	return err
}

// MarkSeen records that the user has seen the event at a version (defaults to current).
func (s *Service) MarkSeen(ctx context.Context, user, event uuid.UUID, version *int) error {
	cur, err := s.eventExists(ctx, event)
	if err != nil {
		return err
	}
	v := cur
	if version != nil && *version > 0 && *version <= cur {
		v = *version
	}
	_, err = s.db.Pool.Exec(ctx, `INSERT INTO user_event_state (user_id, event_id, seen_version, seen_at) VALUES ($1,$2,$3,now())
		ON CONFLICT (user_id, event_id) DO UPDATE SET seen_version=GREATEST(user_event_state.seen_version, EXCLUDED.seen_version), seen_at=now()`, user, event, v)
	return err
}

func (s *Service) Bookmark(ctx context.Context, user, event uuid.UUID, on bool) error {
	if _, err := s.eventExists(ctx, event); err != nil {
		return err
	}
	_, err := s.db.Pool.Exec(ctx, `INSERT INTO user_event_state (user_id, event_id, bookmarked_at) VALUES ($1,$2, CASE WHEN $3 THEN now() END)
		ON CONFLICT (user_id, event_id) DO UPDATE SET bookmarked_at = CASE WHEN $3 THEN now() ELSE NULL END`, user, event, on)
	return err
}

func (s *Service) Dismiss(ctx context.Context, user, event uuid.UUID) error {
	if _, err := s.eventExists(ctx, event); err != nil {
		return err
	}
	_, err := s.db.Pool.Exec(ctx, `INSERT INTO user_event_state (user_id, event_id, dismissed_at) VALUES ($1,$2,now())
		ON CONFLICT (user_id, event_id) DO UPDATE SET dismissed_at=now()`, user, event)
	return err
}

func (s *Service) Bookmarks(ctx context.Context, user uuid.UUID, lang string) ([]events.Event, error) {
	rows, err := s.db.Pool.Query(ctx, `SELECT event_id FROM user_event_state WHERE user_id=$1 AND bookmarked_at IS NOT NULL ORDER BY bookmarked_at DESC LIMIT 200`, user)
	if err != nil {
		return nil, err
	}
	idl, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, err
	}
	out := []events.Event{}
	for _, id := range idl {
		if e, err := s.events.Get(ctx, id, lang); err == nil {
			out = append(out, *e)
		}
	}
	return out, nil
}

// ---------- Ranking ----------

// RankPolicy is the DB-managed `ranking` policy.
type RankPolicy struct {
	Weights struct {
		Relevance       float64 `json:"relevance"`
		Recency         float64 `json:"recency"`
		Importance      float64 `json:"importance"`
		Novelty         float64 `json:"novelty"`
		SourceDiversity float64 `json:"source_diversity"`
	} `json:"weights"`
	RecencyHalfLifeHours float64 `json:"recency_half_life_hours"`
	DisputedPenalty      float64 `json:"disputed_penalty"`
	MaxPerCategoryRatio  float64 `json:"max_per_category_ratio"`
}

func (s *Service) rankPolicy(ctx context.Context) RankPolicy {
	var p RankPolicy
	p.Weights.Relevance, p.Weights.Recency, p.Weights.Importance, p.Weights.Novelty, p.Weights.SourceDiversity = 0.35, 0.25, 0.2, 0.15, 0.05
	p.RecencyHalfLifeHours, p.DisputedPenalty, p.MaxPerCategoryRatio = 18, 0.1, 0.5
	_, _ = s.pol.Get(ctx, "ranking", &p)
	return p
}

type profile struct {
	cats, mutedCats         map[string]float64
	entities, mutedEntities map[string]float64
	keywords, mutedKeywords map[string]float64
	regions                 map[string]float64
	mutedPublishers         map[string]bool
	followedEvents          map[uuid.UUID]bool
	coldStart               bool
	fingerprint             string
}

func (s *Service) profile(ctx context.Context, user uuid.UUID) (*profile, error) {
	p := &profile{cats: map[string]float64{}, mutedCats: map[string]float64{}, entities: map[string]float64{}, mutedEntities: map[string]float64{},
		keywords: map[string]float64{}, mutedKeywords: map[string]float64{}, regions: map[string]float64{}, mutedPublishers: map[string]bool{},
		followedEvents: map[uuid.UUID]bool{}}
	rows, err := s.db.Pool.Query(ctx, `SELECT interest_type, target_key, mode, weight,
		CASE WHEN interest_type='entity' THEN (SELECT normalized_key FROM entities WHERE id::text=target_key) END
		FROM user_interests WHERE user_id=$1 ORDER BY id`, user)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	for rows.Next() {
		var typ, key, mode string
		var w float64
		var entKey *string
		if err := rows.Scan(&typ, &key, &mode, &w, &entKey); err != nil {
			rows.Close()
			return nil, err
		}
		fmt.Fprintf(h, "%s|%s|%s|%v;", typ, key, mode, w)
		follow := mode == "follow"
		switch typ {
		case "category":
			if follow {
				p.cats[key] = w
			} else {
				p.mutedCats[key] = w
			}
		case "entity":
			if entKey != nil {
				if follow {
					p.entities[*entKey] = w
				} else {
					p.mutedEntities[*entKey] = w
				}
			}
		case "keyword":
			if follow {
				p.keywords[key] = w
			} else {
				p.mutedKeywords[key] = w
			}
		case "region":
			if follow {
				p.regions[key] = w
			}
		case "publisher":
			if !follow {
				p.mutedPublishers[key] = true
			}
		}
	}
	rows.Close()
	var prefRegions []string
	_ = s.db.Pool.QueryRow(ctx, `SELECT regions FROM user_preferences WHERE user_id=$1`, user).Scan(&prefRegions)
	for _, r := range prefRegions {
		if _, ok := p.regions[r]; !ok {
			p.regions[r] = 1
		}
		fmt.Fprintf(h, "pr|%s;", r)
	}
	rows, err = s.db.Pool.Query(ctx, `SELECT event_id FROM followed_events WHERE user_id=$1`, user)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id uuid.UUID
		if rows.Scan(&id) == nil {
			p.followedEvents[id] = true
			fmt.Fprintf(h, "fe|%s;", id)
		}
	}
	rows.Close()
	p.coldStart = len(p.cats) == 0 && len(p.entities) == 0 && len(p.keywords) == 0 && len(p.regions) == 0 && len(p.followedEvents) == 0
	p.fingerprint = hex.EncodeToString(h.Sum(nil))[:16]
	return p, nil
}

type candidate struct {
	ID          uuid.UUID
	Version     int
	Updated     time.Time
	Importance  float64
	State       string
	Categories  []string
	Primary     string
	Entities    []string
	Locations   []string
	Publishers  []string
	SearchText  string
	SeenVersion int
	Dismissed   bool
	Score       float64
	Reasons     []string
}

// FeedItem is one ranked entry.
type FeedItem struct {
	Event        events.Event `json:"event"`
	Score        float64      `json:"score"`
	Reasons      []string     `json:"reasons"`
	Unseen       bool         `json:"unseen"`
	UpdatedSince bool         `json:"updated_since_seen"`
}

type FeedPage struct {
	Items       []FeedItem `json:"items"`
	NextCursor  string     `json:"next_cursor,omitempty"`
	GeneratedAt time.Time  `json:"generated_at"`
	ColdStart   bool       `json:"cold_start"`
}

// candidates loads events updated in (since, until]. With byRecorded, "updated" means a
// reader-visible material update was recorded by the system in the window (catch-up semantics:
// a late-processed report of an earlier change is still news to the user).
func (s *Service) candidates(ctx context.Context, user uuid.UUID, since, until time.Time, byRecorded ...bool) ([]*candidate, error) {
	window := `e.last_material_update_at > $2 AND e.last_material_update_at <= $3`
	if len(byRecorded) > 0 && byRecorded[0] {
		window = `EXISTS (SELECT 1 FROM event_updates u WHERE u.event_id=e.id AND u.created_at > $2 AND u.created_at <= $3
			AND (NOT u.requires_approval OR u.approved_at IS NOT NULL))`
	}
	rows, err := s.db.Pool.Query(ctx, `SELECT e.id, e.version, e.last_material_update_at, e.importance, e.state, e.locations, e.search_text,
		ARRAY(SELECT category_id::text FROM event_categories WHERE event_id=e.id),
		COALESCE(e.primary_category_id::text, ''),
		ARRAY(SELECT en.normalized_key FROM event_entities ee JOIN entities en ON en.id=ee.entity_id WHERE ee.event_id=e.id),
		ARRAY(SELECT DISTINCT a.publisher_id::text FROM event_article_links l JOIN articles a ON a.id=l.article_id WHERE l.event_id=e.id AND l.unlinked_at IS NULL),
		COALESCE(s.seen_version, 0), s.dismissed_at IS NOT NULL
		FROM events e LEFT JOIN user_event_state s ON s.event_id=e.id AND s.user_id=$1
		WHERE e.status='active' AND `+window+`
		ORDER BY e.last_material_update_at DESC LIMIT 1000`, user, since, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*candidate
	for rows.Next() {
		c := &candidate{}
		if err := rows.Scan(&c.ID, &c.Version, &c.Updated, &c.Importance, &c.State, &c.Locations, &c.SearchText, &c.Categories, &c.Primary, &c.Entities,
			&c.Publishers, &c.SeenVersion, &c.Dismissed); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// categoryClosure expands followed categories with their descendants (following "Energy" includes
// "Electricity prices").
func (s *Service) categoryClosure(ctx context.Context, cats map[string]float64) map[string]float64 {
	if len(cats) == 0 {
		return cats
	}
	all, _, err := s.tax.All(ctx)
	if err != nil {
		return cats
	}
	children := map[string][]string{}
	for _, c := range all {
		if c.ParentID != nil {
			children[c.ParentID.String()] = append(children[c.ParentID.String()], c.ID.String())
		}
	}
	out := map[string]float64{}
	var walk func(id string, w float64, depth int)
	walk = func(id string, w float64, depth int) {
		if depth > 5 {
			return
		}
		if w > out[id] {
			out[id] = w
		}
		for _, ch := range children[id] {
			walk(ch, w, depth+1)
		}
	}
	for id, w := range cats {
		walk(id, w, 0)
	}
	return out
}

// score computes relevance; returns ok=false when the event must be excluded (muted/dismissed/irrelevant).
func (s *Service) score(c *candidate, p *profile, cats map[string]float64, rp RankPolicy, now time.Time, requireRelevance bool) bool {
	if c.Dismissed && c.SeenVersion >= c.Version {
		return false
	}
	for _, cat := range c.Categories {
		if _, ok := p.mutedCats[cat]; ok && cat == c.Primary {
			return false
		}
	}
	if len(c.Publishers) > 0 {
		all := true
		for _, pub := range c.Publishers {
			if !p.mutedPublishers[pub] {
				all = false
			}
		}
		if all {
			return false
		}
	}
	text := " " + strings.ToLower(nlp.Key(c.SearchText)) + " "
	for k := range p.mutedKeywords {
		if strings.Contains(text, " "+k+" ") {
			return false
		}
	}
	for _, e := range c.Entities {
		if _, ok := p.mutedEntities[e]; ok {
			return false
		}
	}
	rel := 0.0
	if p.followedEvents[c.ID] {
		rel += 1.0
		c.Reasons = append(c.Reasons, "followed_event")
	}
	for _, cat := range c.Categories {
		if w, ok := cats[cat]; ok {
			add := 0.6 * w
			if cat == c.Primary {
				add = 1.0 * w
			}
			if add > 0 {
				rel += add
				c.Reasons = append(c.Reasons, "category")
				break
			}
		}
	}
	for _, e := range c.Entities {
		if w, ok := p.entities[e]; ok {
			rel += 0.7 * w
			c.Reasons = append(c.Reasons, "entity")
			break
		}
	}
	for k, w := range p.keywords {
		if strings.Contains(text, " "+k) {
			rel += 0.6 * w
			c.Reasons = append(c.Reasons, "keyword")
			break
		}
	}
	for _, loc := range c.Locations {
		if w, ok := p.regions[loc]; ok {
			rel += 0.5 * w
			c.Reasons = append(c.Reasons, "region")
			break
		}
	}
	if p.coldStart {
		rel = 0.5 // editorial defaults: importance and recency decide
		c.Reasons = append(c.Reasons, "editorial_default")
	} else if requireRelevance && rel == 0 {
		return false
	}
	rel = math.Min(rel, 1.5) / 1.5
	age := now.Sub(c.Updated).Hours()
	recency := math.Exp(-age * math.Ln2 / math.Max(1, rp.RecencyHalfLifeHours))
	novelty := 1.0
	if c.SeenVersion >= c.Version {
		novelty = 0.1
	} else if c.SeenVersion > 0 {
		novelty = 0.8
		c.Reasons = append(c.Reasons, "updated_since_seen")
	}
	diversity := math.Min(1, float64(len(c.Publishers))/3)
	c.Score = rp.Weights.Relevance*rel + rp.Weights.Recency*recency + rp.Weights.Importance*c.Importance + rp.Weights.Novelty*novelty +
		rp.Weights.SourceDiversity*diversity
	if c.State == "disputed" {
		c.Score -= rp.DisputedPenalty // not sensationalised; still shown with its dispute status
	}
	return true
}

// diversify greedily limits any single primary category to MaxPerCategoryRatio of the page.
func diversify(cs []*candidate, ratio float64, page int) []*candidate {
	if ratio <= 0 || ratio >= 1 {
		return cs
	}
	maxPer := int(math.Max(2, math.Ceil(ratio*float64(page))))
	var out, deferred []*candidate
	counts := map[string]int{}
	for _, c := range cs {
		if c.Primary != "" && counts[c.Primary] >= maxPer && len(out)%page != 0 {
			deferred = append(deferred, c)
			continue
		}
		counts[c.Primary]++
		out = append(out, c)
		if len(out)%page == 0 {
			counts = map[string]int{}
		}
	}
	return append(out, deferred...)
}

// Feed returns the personalized home feed with stable cursor pagination: the ranked list is
// computed once per (user, interests fingerprint, content high-water mark) and cached; the cursor
// carries the generation time and offset.
func (s *Service) Feed(ctx context.Context, user uuid.UUID, lang, cursor string, limit int) (*FeedPage, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	gen := s.now().UTC()
	offset := 0
	if cursor != "" {
		var c struct {
			T time.Time `json:"t"`
			O int       `json:"o"`
		}
		if err := decodeCursor(cursor, &c); err != nil {
			return nil, apperr.BadRequest("invalid_cursor", "Invalid pagination cursor.")
		}
		gen, offset = c.T, c.O
	}
	p, err := s.profile(ctx, user)
	if err != nil {
		return nil, err
	}
	var hw time.Time
	_ = s.db.Pool.QueryRow(ctx, `SELECT COALESCE(max(last_material_update_at), 'epoch') FROM events WHERE status='active' AND last_material_update_at <= $1`, gen).Scan(&hw)
	key := fmt.Sprintf("feed:v1:%s:%s:%d:%d", user, p.fingerprint, hw.Unix(), gen.Truncate(time.Minute).Unix())
	var ranked []uuid.UUID
	reasons := map[uuid.UUID][]string{}
	scores := map[uuid.UUID]float64{}
	seen := map[uuid.UUID]int{}
	if raw, ok := s.cache.Get(ctx, key); ok {
		var cached struct {
			IDs     []uuid.UUID            `json:"ids"`
			Reasons map[uuid.UUID][]string `json:"reasons"`
			Scores  map[uuid.UUID]float64  `json:"scores"`
		}
		if json.Unmarshal(raw, &cached) == nil {
			ranked, reasons, scores = cached.IDs, cached.Reasons, cached.Scores
		}
	}
	if ranked == nil {
		rp := s.rankPolicy(ctx)
		cands, err := s.candidates(ctx, user, gen.Add(-7*24*time.Hour), gen)
		if err != nil {
			return nil, err
		}
		cats := s.categoryClosure(ctx, p.cats)
		var kept []*candidate
		for _, c := range cands {
			if s.score(c, p, cats, rp, gen, true) {
				kept = append(kept, c)
			}
		}
		sort.SliceStable(kept, func(i, j int) bool {
			if kept[i].Score != kept[j].Score {
				return kept[i].Score > kept[j].Score
			}
			return kept[i].ID.String() < kept[j].ID.String()
		})
		kept = diversify(kept, rp.MaxPerCategoryRatio, limit)
		if len(kept) > 300 {
			kept = kept[:300]
		}
		for _, c := range kept {
			ranked = append(ranked, c.ID)
			reasons[c.ID] = uniq(c.Reasons)
			scores[c.ID] = math.Round(c.Score*1000) / 1000
		}
		b, _ := json.Marshal(map[string]any{"ids": ranked, "reasons": reasons, "scores": scores})
		s.cache.Set(ctx, key, b, 2*time.Minute)
	}
	rows, err := s.db.Pool.Query(ctx, `SELECT event_id, seen_version FROM user_event_state WHERE user_id=$1`, user)
	if err == nil {
		for rows.Next() {
			var id uuid.UUID
			var v int
			if rows.Scan(&id, &v) == nil {
				seen[id] = v
			}
		}
		rows.Close()
	}
	page := &FeedPage{GeneratedAt: gen, ColdStart: p.coldStart, Items: []FeedItem{}}
	end := min(offset+limit, len(ranked))
	for _, id := range ranked[min(offset, len(ranked)):end] {
		e, err := s.events.Get(ctx, id, lang)
		if err != nil || e.Status != "active" {
			continue
		}
		sv := seen[id]
		page.Items = append(page.Items, FeedItem{Event: *e, Score: scores[id], Reasons: reasons[id], Unseen: sv == 0, UpdatedSince: sv > 0 && sv < e.Version})
	}
	if end < len(ranked) {
		page.NextCursor = encodeCursor(map[string]any{"t": gen, "o": end})
	}
	return page, nil
}

func uniq(s []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, x := range s {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// ---------- Catch-up ----------

type CatchUpItem struct {
	Event          events.Event           `json:"event"`
	Changes        []events.TimelineEntry `json:"changes_since"`
	PreviouslySeen bool                   `json:"previously_seen"`
	Reasons        []string               `json:"reasons"`
}

type CatchUp struct {
	Since          time.Time     `json:"since"`
	SinceSource    string        `json:"since_source"` // acknowledged | default_24h | explicit
	AcknowledgedAt *time.Time    `json:"acknowledged_at,omitempty"`
	LastLoginAt    *time.Time    `json:"last_login_at,omitempty"`
	Items          []CatchUpItem `json:"items"`
	Note           string        `json:"note"`
}

// CatchUp lists relevant events with material changes since the user's explicit acknowledgement
// (distinct from last login). Previously seen events reappear only with new material updates.
func (s *Service) CatchUp(ctx context.Context, user uuid.UUID, lang string, since *time.Time, limit int) (*CatchUp, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	out := &CatchUp{Items: []CatchUpItem{}, Note: "Uses your last acknowledged catch-up time, not your last login."}
	var ack *time.Time
	_ = s.db.Pool.QueryRow(ctx, `SELECT acknowledged_at FROM user_feed_cursors WHERE user_id=$1`, user).Scan(&ack)
	out.AcknowledgedAt = ack
	_ = s.db.Pool.QueryRow(ctx, `SELECT max(created_at) FROM user_security_events WHERE user_id=$1 AND event_type='login_succeeded'`, user).Scan(&out.LastLoginAt)
	switch {
	case since != nil:
		out.Since, out.SinceSource = *since, "explicit"
	case ack != nil:
		out.Since, out.SinceSource = *ack, "acknowledged"
	default:
		out.Since, out.SinceSource = s.now().Add(-24*time.Hour), "default_24h"
	}
	p, err := s.profile(ctx, user)
	if err != nil {
		return nil, err
	}
	rp := s.rankPolicy(ctx)
	now := s.now()
	cands, err := s.candidates(ctx, user, out.Since, now, true)
	if err != nil {
		return nil, err
	}
	cats := s.categoryClosure(ctx, p.cats)
	var kept []*candidate
	for _, c := range cands {
		if c.SeenVersion >= c.Version {
			continue // already seen at the latest material version
		}
		if s.score(c, p, cats, rp, now, true) {
			kept = append(kept, c)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].Score > kept[j].Score })
	if len(kept) > limit {
		kept = kept[:limit]
	}
	for _, c := range kept {
		e, err := s.events.Get(ctx, c.ID, lang)
		if err != nil {
			continue
		}
		tl, err := s.events.Timeline(ctx, c.ID, false)
		if err != nil {
			return nil, err
		}
		var changes []events.TimelineEntry
		for _, t := range tl {
			if t.RecordedAt.After(out.Since) && t.EventVersion > c.SeenVersion {
				changes = append(changes, t)
			}
		}
		if changes == nil {
			changes = []events.TimelineEntry{}
		}
		out.Items = append(out.Items, CatchUpItem{Event: *e, Changes: changes, PreviouslySeen: c.SeenVersion > 0, Reasons: uniq(c.Reasons)})
	}
	return out, nil
}

// Acknowledge moves the catch-up cursor (default: now). Never moves backwards unless explicit.
func (s *Service) Acknowledge(ctx context.Context, user uuid.UUID, at *time.Time) (time.Time, error) {
	t := s.now().UTC()
	if at != nil {
		if at.After(t.Add(time.Minute)) {
			return time.Time{}, apperr.ValidationMsg("ack_in_future", "Acknowledgement time cannot be in the future.")
		}
		t = at.UTC()
	}
	_, err := s.db.Pool.Exec(ctx, `INSERT INTO user_feed_cursors (user_id, acknowledged_at) VALUES ($1,$2)
		ON CONFLICT (user_id) DO UPDATE SET acknowledged_at=EXCLUDED.acknowledged_at, updated_at=now()`, user, t)
	return t, err
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// TopEvents ranks events for a user in a window with optional category/region restriction
// (used by briefings and digests). Category filters include descendants.
func (s *Service) TopEvents(ctx context.Context, user uuid.UUID, since, until time.Time, categories []uuid.UUID, region string, minImportance float64, limit int) ([]uuid.UUID, error) {
	p, err := s.profile(ctx, user)
	if err != nil {
		return nil, err
	}
	rp := s.rankPolicy(ctx)
	cands, err := s.candidates(ctx, user, since, until, true)
	if err != nil {
		return nil, err
	}
	filter := map[string]float64{}
	for _, c := range categories {
		filter[c.String()] = 1
	}
	filter = s.categoryClosure(ctx, filter)
	cats := s.categoryClosure(ctx, p.cats)
	regionKey := ""
	if region != "" {
		regionKey = nlp.EntityKey(region)
	}
	var kept []*candidate
	for _, c := range cands {
		if c.Importance < minImportance {
			continue
		}
		if len(filter) > 0 {
			ok := false
			for _, cat := range c.Categories {
				if _, hit := filter[cat]; hit {
					ok = true
				}
			}
			if !ok {
				continue
			}
		}
		if regionKey != "" {
			ok := false
			for _, l := range c.Locations {
				if l == regionKey {
					ok = true
				}
			}
			if !ok {
				continue
			}
		}
		if s.score(c, p, cats, rp, until, len(filter) == 0 && regionKey == "") {
			kept = append(kept, c)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].Score > kept[j].Score })
	kept = diversify(kept, rp.MaxPerCategoryRatio, max(limit, 1))
	var out []uuid.UUID
	for i, c := range kept {
		if i >= limit {
			break
		}
		out = append(out, c.ID)
	}
	return out, nil
}
