package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dorinpuscasu25/recapproject/internal/ai"
	"github.com/dorinpuscasu25/recapproject/internal/enrich"
	"github.com/dorinpuscasu25/recapproject/internal/nlp"
	"github.com/dorinpuscasu25/recapproject/internal/platform/bus"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ids"
	"github.com/dorinpuscasu25/recapproject/internal/search"
)

// Changed is the event.created.v1 / event.updated.v1 payload.
type Changed struct {
	EventID     uuid.UUID   `json:"event_id"`
	Version     int         `json:"version"`
	Kind        string      `json:"kind"` // created | material_update | correction | contradiction | coverage | merge | split | editorial
	Material    bool        `json:"material"`
	Severity    string      `json:"severity,omitempty"`
	ArticleIDs  []uuid.UUID `json:"article_ids,omitempty"`
	CategoryIDs []uuid.UUID `json:"category_ids,omitempty"`
	Importance  float32     `json:"importance"`
}

// VerificationRequested is the claim.verification.requested.v1 payload.
type VerificationRequested struct {
	ClaimID uuid.UUID  `json:"claim_id"`
	EventID *uuid.UUID `json:"event_id,omitempty"`
	Reason  string     `json:"reason"`
}

type artInfo struct {
	ID, PublisherID uuid.UUID
	Title, Lead     string
	Text            string
	Language        string
	PublishedAt     time.Time
	IsCorrection    bool
	ContentType     string
	EntityKeys      []string
	EntityKinds     map[string]string
	FactKeys        []string
	FactTexts       []string
	CategoryIDs     []uuid.UUID
	PrimaryCategory *uuid.UUID
	ClaimIDs        []uuid.UUID
	Status          string
}

func (s *Service) loadArticle(ctx context.Context, q db.Querier, id uuid.UUID) (*artInfo, error) {
	a := &artInfo{ID: id, EntityKinds: map[string]string{}}
	var body *string
	var pub *time.Time
	var first time.Time
	err := q.QueryRow(ctx, `SELECT publisher_id, title, description, body_text, language, published_at, first_seen_at, is_correction, content_type, status
		FROM articles WHERE id=$1`, id).Scan(&a.PublisherID, &a.Title, &a.Lead, &body, &a.Language, &pub, &first, &a.IsCorrection, &a.ContentType, &a.Status)
	if err != nil {
		return nil, err
	}
	if body != nil {
		a.Text = *body
	}
	if a.Lead == "" {
		a.Lead = nlp.Truncate(a.Text, 400)
	}
	a.PublishedAt = first
	if pub != nil {
		a.PublishedAt = *pub
	}
	rows, err := q.Query(ctx, `SELECT e.normalized_key, e.kind FROM article_entities ae JOIN entities e ON e.id=ae.entity_id WHERE ae.article_id=$1`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k, kind string
		if err := rows.Scan(&k, &kind); err != nil {
			rows.Close()
			return nil, err
		}
		a.EntityKeys = append(a.EntityKeys, k)
		a.EntityKinds[k] = kind
	}
	rows.Close()
	rows, err = q.Query(ctx, `SELECT fact_key, text FROM article_facts WHERE article_id=$1 ORDER BY start_offset`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k, t string
		if err := rows.Scan(&k, &t); err != nil {
			rows.Close()
			return nil, err
		}
		a.FactKeys = append(a.FactKeys, k)
		a.FactTexts = append(a.FactTexts, t)
	}
	rows.Close()
	rows, err = q.Query(ctx, `SELECT category_id, is_primary FROM article_categories WHERE article_id=$1 ORDER BY is_primary DESC, confidence DESC NULLS LAST`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var cid uuid.UUID
		var prim bool
		if err := rows.Scan(&cid, &prim); err != nil {
			rows.Close()
			return nil, err
		}
		a.CategoryIDs = append(a.CategoryIDs, cid)
		if prim && a.PrimaryCategory == nil {
			c := cid
			a.PrimaryCategory = &c
		}
	}
	rows.Close()
	rows, err = q.Query(ctx, `SELECT DISTINCT claim_id FROM claim_occurrences WHERE article_id=$1`, id)
	if err != nil {
		return nil, err
	}
	a.ClaimIDs, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	return a, err
}

func (a *artInfo) embedText() string {
	return a.Title + ". " + a.Lead + " " + nlp.Truncate(a.Text, 1500)
}

// candidate is a scored event candidate with rationale.
type candidate struct {
	EventID        uuid.UUID `json:"event_id"`
	Semantic       float64   `json:"semantic"`
	EntityOverlap  float64   `json:"entity_overlap"`
	FactOverlap    float64   `json:"fact_overlap"`
	TimeProximity  float64   `json:"time_proximity"`
	Score          float64   `json:"score"`
	SharedEntities []string  `json:"shared_entities"`
	SharedFacts    []string  `json:"shared_facts"`
	Conflicts      []string  `json:"conflicting_facts"`
	Sources        []string  `json:"retrieved_by"`
	title, summary string
	lastUpdate     time.Time
	version        int
}

// weightedJaccard down-weights generic locations (e.g. the country name) that appear everywhere.
func weightedJaccard(a, b []string, kinds map[string]string) (float64, []string) {
	w := func(k string) float64 {
		switch kinds[k] {
		case "location":
			return 0.35
		case "other":
			return 0.6
		}
		return 1
	}
	sb := map[string]bool{}
	for _, x := range b {
		sb[x] = true
	}
	var inter, union float64
	var shared []string
	seen := map[string]bool{}
	for _, x := range a {
		if seen[x] {
			continue
		}
		seen[x] = true
		union += w(x)
		if sb[x] {
			inter += w(x)
			shared = append(shared, x)
		}
	}
	for x := range sb {
		if !seen[x] {
			union += w(x)
		}
	}
	if union == 0 {
		return 0, nil
	}
	return inter / union, shared
}

// factOverlap compares comparable facts. Decisions/money/percent conflicts (same type, different
// values, none shared) indicate a different event on the same subject.
func factOverlap(article, event []string) (float64, []string, []string) {
	ev := map[string]bool{}
	evTypes := map[string]bool{}
	for _, k := range event {
		ev[k] = true
		evTypes[strings.SplitN(k, ":", 2)[0]] = true
	}
	var shared, conflicts []string
	aTypes := map[string]bool{}
	for _, k := range article {
		t := strings.SplitN(k, ":", 2)[0]
		aTypes[t] = true
		if ev[k] {
			shared = append(shared, k)
		}
	}
	sharedTypes := map[string]bool{}
	for _, k := range shared {
		sharedTypes[strings.SplitN(k, ":", 2)[0]] = true
	}
	for _, t := range []string{"money", "percent", "date"} {
		if aTypes[t] && evTypes[t] && !sharedTypes[t] {
			conflicts = append(conflicts, t)
		}
	}
	n := len(uniq(article))
	if n == 0 {
		return 0, shared, conflicts
	}
	return math.Min(1, float64(len(shared))/math.Min(float64(n), 4)), shared, conflicts
}

func uniq(s []string) []string {
	m := map[string]bool{}
	var out []string
	for _, x := range s {
		if !m[x] {
			m[x] = true
			out = append(out, x)
		}
	}
	return out
}

// eventVector embeds an event's search text with the active event-collection model (cached).
func (s *Service) eventVector(ctx context.Context, id uuid.UUID, version int, text string) ([]float32, error) {
	key := fmt.Sprintf("%s:%d", id, version)
	s.embMu.Lock()
	v, ok := s.embCache[key]
	s.embMu.Unlock()
	if ok {
		return v, nil
	}
	v, _, err := s.search.EmbedActive(ctx, "event", text)
	if err != nil {
		return nil, err
	}
	s.embMu.Lock()
	if len(s.embCache) > 5000 {
		s.embCache = map[string][]float32{}
	}
	s.embCache[key] = v
	s.embMu.Unlock()
	return v, nil
}

// retrieve gathers candidates from Qdrant (semantic), Postgres (shared entities, recent events
// not yet projected) and the lexical index, then scores them.
func (s *Service) retrieve(ctx context.Context, tx pgx.Tx, a *artInfo, p MatchPolicy) ([]*candidate, []float32, error) {
	vec, _, err := s.search.EmbedActive(ctx, "event", a.embedText())
	if err != nil {
		return nil, nil, err
	}
	window := time.Duration(p.TimeWindowHours) * time.Hour
	from, to := a.PublishedAt.Add(-window), a.PublishedAt.Add(window)
	cands := map[uuid.UUID]*candidate{}
	add := func(id uuid.UUID, src string) {
		c := cands[id]
		if c == nil {
			c = &candidate{EventID: id}
			cands[id] = c
		}
		c.Sources = append(c.Sources, src)
	}
	if hits, err := s.search.SimilarByVector(ctx, "event", vec, search.VectorFilter{RangeGTE: map[string]float64{"published_ts": float64(from.Unix())}}, p.MaxCandidates); err == nil {
		for _, h := range hits {
			add(h.ID, "semantic")
		}
	} else {
		s.log.Warn("semantic candidate retrieval unavailable", "error", err)
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT ee.event_id FROM event_entities ee JOIN entities en ON en.id=ee.entity_id JOIN events e ON e.id=ee.event_id
		WHERE en.normalized_key = ANY($1) AND en.kind <> 'location' AND e.status='active' AND e.last_material_update_at BETWEEN $2 AND $3 LIMIT $4`,
		nonNil(a.EntityKeys), from, to, p.MaxCandidates*2)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, nil, err
		}
		add(id, "entities")
	}
	rows.Close()
	// Recently created events may not be projected into Qdrant yet.
	rows, err = tx.Query(ctx, `SELECT id FROM events WHERE status='active' AND created_at > now() - interval '6 hours' ORDER BY created_at DESC LIMIT 30`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, nil, err
		}
		add(id, "recent")
	}
	rows.Close()
	if hits, err := s.search.Lexical().Search(ctx, search.LexicalQuery{Text: a.Title, DocTypes: []string{"event"}, From: &from, To: &to, Limit: p.MaxCandidates}); err == nil {
		for _, h := range hits {
			if id, err := uuid.Parse(h.ID); err == nil {
				add(id, "lexical")
			}
		}
	}
	var out []*candidate
	for id, c := range cands {
		var searchText string
		var titles, summaries []byte
		var fp []byte
		var last *time.Time
		err := tx.QueryRow(ctx, `SELECT search_text, titles, summaries, fingerprint, last_material_update_at, version FROM events WHERE id=$1 AND status='active'`, id).
			Scan(&searchText, &titles, &summaries, &fp, &last, &c.version)
		if db.IsNoRows(err) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		var f struct {
			EntityKeys  []string          `json:"entity_keys"`
			EntityKinds map[string]string `json:"entity_kinds"`
			FactKeys    []string          `json:"fact_keys"`
		}
		_ = json.Unmarshal(fp, &f)
		ev, err := s.eventVector(ctx, id, c.version, searchText)
		if err != nil {
			return nil, nil, err
		}
		c.Semantic = math.Max(0, nlp.Cosine(vec, ev))
		kinds := map[string]string{}
		for k, v := range f.EntityKinds {
			kinds[k] = v
		}
		for k, v := range a.EntityKinds {
			kinds[k] = v
		}
		c.EntityOverlap, c.SharedEntities = weightedJaccard(a.EntityKeys, f.EntityKeys, kinds)
		c.FactOverlap, c.SharedFacts, c.Conflicts = factOverlap(a.FactKeys, f.FactKeys)
		if last != nil {
			c.lastUpdate = *last
			dt := math.Abs(a.PublishedAt.Sub(*last).Hours())
			c.TimeProximity = math.Exp(-dt / 48)
		}
		c.Score = p.Weights.Semantic*c.Semantic + p.Weights.Entities*c.EntityOverlap + p.Weights.Facts*c.FactOverlap + p.Weights.Time*c.TimeProximity
		if len(c.Conflicts) > 0 && len(c.SharedFacts) == 0 {
			c.Score *= 0.6
		}
		var t map[string]string
		_ = json.Unmarshal(titles, &t)
		c.title, _, _ = pick(t, a.Language)
		var sm map[string]Summary
		_ = json.Unmarshal(summaries, &sm)
		for _, l := range []string{a.Language, "ro", "ru"} {
			if x, ok := sm[l]; ok {
				c.summary = x.Normal
				break
			}
		}
		if c.summary == "" {
			c.summary = nlp.Truncate(searchText, 800)
		}
		if c.Score >= p.MinCandidateScore {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > p.MaxCandidates {
		out = out[:p.MaxCandidates]
	}
	return out, vec, nil
}

type decision struct {
	Relation   string       `json:"relation"` // same_story | follow_up | new_event
	EventID    *uuid.UUID   `json:"event_id,omitempty"`
	RelatedTo  *uuid.UUID   `json:"related_to,omitempty"`
	Method     string       `json:"method"` // rules | model
	ModelRef   string       `json:"model_ref,omitempty"`
	Reason     string       `json:"reason"`
	Policy     MatchPolicy  `json:"policy"`
	Candidates []*candidate `json:"candidates"`
	IndexModel string       `json:"index_model,omitempty"`
}

func (s *Service) decide(ctx context.Context, a *artInfo, cands []*candidate, p MatchPolicy) decision {
	d := decision{Relation: "new_event", Method: "rules", Policy: p, Candidates: cands}
	if len(cands) == 0 {
		d.Reason = "no candidate above minimum score"
		return d
	}
	best := cands[0]
	anchor := len(best.SharedEntities) > 0 || len(best.SharedFacts) > 0
	hardConflict := len(best.Conflicts) > 0 && len(best.SharedFacts) == 0
	switch {
	case best.Score >= p.SameStoryScore && (anchor || !p.RequireSharedAnchor) && !hardConflict:
		d.Relation, d.EventID = "same_story", &best.EventID
		d.Reason = fmt.Sprintf("score %.3f ≥ %.2f with shared anchors", best.Score, p.SameStoryScore)
	case best.Score >= p.AmbiguousLow && p.UseLLMForAmbiguous && s.pol.FlagEnabled(ctx, "llm_event_equivalence", a.ID.String()):
		var out ai.EquivalenceOutput
		exec, err := s.gw.GenerateStructured(ctx, ai.TaskEventEquivalence, &ai.EquivalenceInput{ArticleTitle: a.Title, ArticleLead: nlp.Truncate(a.Lead, 800),
			ArticleDate: a.PublishedAt.Format(time.RFC3339), EventTitle: best.title, EventSummary: nlp.Truncate(best.summary, 1200),
			EventDate: best.lastUpdate.Format(time.RFC3339), SharedEntities: best.SharedEntities, SharedFacts: best.SharedFacts,
			ConflictFacts: best.Conflicts, Similarity: best.Semantic}, &out, ai.CallOpts{SubjectType: "article", SubjectID: a.ID.String()})
		if err != nil {
			d.Reason = "ambiguous; equivalence model unavailable → conservative new event: " + err.Error()
			return d
		}
		d.Method, d.ModelRef = "model", exec.ModelRef
		switch out.Relation {
		case "same_story":
			d.Relation, d.EventID = "same_story", &best.EventID
		case "follow_up":
			d.Relation, d.EventID = "follow_up", &best.EventID
		case "related", "same_subject_different_event":
			d.RelatedTo = &best.EventID
		}
		d.Reason = fmt.Sprintf("ambiguous score %.3f; model relation=%s (%.2f): %s", best.Score, out.Relation, out.Confidence, out.Rationale)
	default:
		d.Reason = fmt.Sprintf("best score %.3f below threshold or conflicting facts %v", best.Score, best.Conflicts)
		if best.Score >= p.AmbiguousLow {
			d.RelatedTo = &best.EventID
		}
	}
	return d
}

// HandleResolution is the consumer for event.resolution.requested.v1.
func (s *Service) HandleResolution(ctx context.Context, del *bus.Delivery) error {
	var msg enrich.ResolutionRequested
	if err := del.Message.Decode(&msg); err != nil {
		return bus.PermanentErr(err)
	}
	if done, err := del.AlreadyProcessed(ctx); err != nil || done {
		return err
	}
	err := s.Resolve(ctx, msg.ArticleID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO consumer_dedupe (consumer, message_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, del.Consumer, del.Message.ID)
		return err
	})
	if errors.Is(err, errSkip) {
		return del.Once(ctx, func(pgx.Tx) error { return nil })
	}
	return err
}

var errSkip = errors.New("skip")

// resolutionLock serializes event resolution so concurrent articles about the same new event
// cannot create duplicate events (news volume makes a single critical section affordable).
const resolutionLock = 7_100_001

// Resolve links an article to an existing event or creates a new one, transactionally.
func (s *Service) Resolve(ctx context.Context, articleID uuid.UUID, inTx func(pgx.Tx) error) error {
	p := s.matchPolicy(ctx)
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, resolutionLock); err != nil {
			return err
		}
		a, err := s.loadArticle(ctx, tx, articleID)
		if db.IsNoRows(err) {
			return errSkip
		}
		if err != nil {
			return err
		}
		if a.Status != "enriched" {
			return errSkip
		}
		var existing uuid.UUID
		err = tx.QueryRow(ctx, `SELECT event_id FROM event_article_links WHERE article_id=$1 AND unlinked_at IS NULL AND relation IN ('origin','same_story','follow_up')`, a.ID).Scan(&existing)
		switch {
		case err == nil:
			// Revision of an already-linked article: re-evaluate novelty for its event.
			if err := s.applyToEvent(ctx, tx, existing, a, "revision", nil); err != nil {
				return err
			}
		case db.IsNoRows(err):
			cands, _, err := s.retrieve(ctx, tx, a, p)
			if err != nil {
				return err
			}
			d := s.decide(ctx, a, cands, p)
			if d.EventID != nil {
				if err := s.applyToEvent(ctx, tx, *d.EventID, a, d.Relation, &d); err != nil {
					return err
				}
			} else {
				if _, err := s.createEvent(ctx, tx, a, &d); err != nil {
					return err
				}
			}
		default:
			return err
		}
		if inTx != nil {
			return inTx(tx)
		}
		return nil
	})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func newID() uuid.UUID { return ids.New() }
