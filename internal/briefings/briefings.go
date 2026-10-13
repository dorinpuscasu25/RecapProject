// Package briefings builds durable, cited text briefings from canonical event representations
// (reusing per-event narrations across users) and orchestrates audio generation through the TTS
// route. Without a configured TTS provider, audio jobs end in `provider_unavailable` — never a
// fabricated URL.
package briefings

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dorinpuscasu25/recapproject/internal/ai"
	"github.com/dorinpuscasu25/recapproject/internal/events"
	"github.com/dorinpuscasu25/recapproject/internal/feed"
	"github.com/dorinpuscasu25/recapproject/internal/jobs"
	"github.com/dorinpuscasu25/recapproject/internal/nlp"
	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/bus"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ids"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ratelimit"
	"github.com/dorinpuscasu25/recapproject/internal/platform/storage"
	"github.com/dorinpuscasu25/recapproject/internal/policy"
)

const (
	JobGenerate = "briefing.generate"
	JobAudio    = "audio.generate"
)

type Service struct {
	db      *db.DB
	gw      *ai.Gateway
	events  *events.Service
	feed    *feed.Service
	pol     *policy.Store
	store   storage.Store
	limiter ratelimit.Limiter
	log     *slog.Logger
	urlTTL  time.Duration
	now     func() time.Time
}

func NewService(d *db.DB, gw *ai.Gateway, ev *events.Service, f *feed.Service, pol *policy.Store, st storage.Store, lim ratelimit.Limiter, log *slog.Logger, urlTTL time.Duration) *Service {
	return &Service{db: d, gw: gw, events: ev, feed: f, pol: pol, store: st, limiter: lim, log: log, urlTTL: urlTTL, now: time.Now}
}

type Request struct {
	Language        string      `json:"language,omitempty"`
	DurationMinutes int         `json:"duration_minutes,omitempty"`
	CategoryIDs     []uuid.UUID `json:"category_ids,omitempty"`
	Region          string      `json:"region,omitempty"`
	Since           *time.Time  `json:"since,omitempty"`
	Tone            string      `json:"tone,omitempty"`
}

type Citation struct {
	Ref         string     `json:"ref"`
	EventID     uuid.UUID  `json:"event_id"`
	ArticleID   *uuid.UUID `json:"article_id,omitempty"`
	Publisher   string     `json:"publisher,omitempty"`
	URL         string     `json:"url,omitempty"`
	Title       string     `json:"title,omitempty"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
}

type Item struct {
	Position     int        `json:"position"`
	EventID      uuid.UUID  `json:"event_id"`
	EventVersion int        `json:"event_version"`
	Title        string     `json:"title"`
	Text         string     `json:"text"`
	Citations    []Citation `json:"citations"`
}

type Briefing struct {
	ID                uuid.UUID   `json:"id"`
	Status            string      `json:"status"`
	Trigger           string      `json:"trigger"`
	Language          string      `json:"language"`
	DurationMinutes   int         `json:"duration_minutes"`
	CategoryIDs       []uuid.UUID `json:"category_ids"`
	Region            *string     `json:"region,omitempty"`
	Tone              string      `json:"tone"`
	Since             time.Time   `json:"since"`
	Until             time.Time   `json:"until"`
	Title             string      `json:"title"`
	Text              *string     `json:"text,omitempty"`
	EstimatedSeconds  *int        `json:"estimated_seconds,omitempty"`
	ModelRef          *string     `json:"model_ref,omitempty"`
	PromptVersion     *string     `json:"prompt_version,omitempty"`
	GenerationVersion int         `json:"generation_version"`
	ErrorCode         *string     `json:"error_code,omitempty"`
	ErrorMessage      *string     `json:"error_message,omitempty"`
	RequestedAt       time.Time   `json:"requested_at"`
	CompletedAt       *time.Time  `json:"completed_at,omitempty"`
	Items             []Item      `json:"items"`
	userID            uuid.UUID
	fingerprint       string
}

const cols = `id, user_id, status, trigger, language, duration_minutes, category_ids, region, tone, since, until, title, text, estimated_seconds,
	model_ref, prompt_version, generation_version, error_code, error_message, requested_at, completed_at, cache_fingerprint`

func scan(row pgx.Row) (*Briefing, error) {
	b := &Briefing{}
	err := row.Scan(&b.ID, &b.userID, &b.Status, &b.Trigger, &b.Language, &b.DurationMinutes, &b.CategoryIDs, &b.Region, &b.Tone, &b.Since, &b.Until,
		&b.Title, &b.Text, &b.EstimatedSeconds, &b.ModelRef, &b.PromptVersion, &b.GenerationVersion, &b.ErrorCode, &b.ErrorMessage, &b.RequestedAt,
		&b.CompletedAt, &b.fingerprint)
	return b, err
}

// Create validates and enqueues a briefing. An identical recent ready briefing is reused.
func (s *Service) Create(ctx context.Context, user uuid.UUID, in Request, trigger string) (*Briefing, bool, error) {
	if ok, retry, _ := s.limiter.Allow(ctx, "briefing:"+user.String(), 20, time.Hour); !ok {
		return nil, false, apperr.RateLimited(retry)
	}
	var prefLang string
	var prefMin int
	_ = s.db.Pool.QueryRow(ctx, `SELECT summary_language, briefing_minutes FROM user_preferences WHERE user_id=$1`, user).Scan(&prefLang, &prefMin)
	if in.Language == "" {
		in.Language = prefLang
	}
	if in.Language == "" {
		in.Language = "ro"
	}
	if in.DurationMinutes == 0 {
		in.DurationMinutes = max(prefMin, 5)
	}
	if in.Tone == "" {
		in.Tone = "neutral"
	}
	var v apperr.Validator
	v.Check(in.Language == "ro" || in.Language == "ru" || in.Language == "en", "language", "invalid", "ro, ru or en")
	v.Check(in.DurationMinutes == 2 || in.DurationMinutes == 5 || in.DurationMinutes == 10, "duration_minutes", "invalid", "2, 5 or 10")
	v.Check(in.Tone == "neutral", "tone", "unsupported", "Only the neutral editorial tone is available.")
	v.Check(len(in.CategoryIDs) <= 20, "category_ids", "too_many", "At most 20 categories.")
	if err := v.Err(); err != nil {
		return nil, false, err
	}
	until := s.now().UTC()
	since := until.Add(-24 * time.Hour)
	if in.Since != nil {
		if in.Since.After(until) || until.Sub(*in.Since) > 14*24*time.Hour {
			return nil, false, apperr.Validation(apperr.FieldError{Field: "since", Code: "out_of_range", Message: "since must be within the last 14 days."})
		}
		since = in.Since.UTC()
	} else {
		var ack *time.Time
		_ = s.db.Pool.QueryRow(ctx, `SELECT acknowledged_at FROM user_feed_cursors WHERE user_id=$1`, user).Scan(&ack)
		if ack != nil && until.Sub(*ack) < 7*24*time.Hour {
			since = *ack
		}
	}
	// Fingerprint: same user scope + content high-water mark → reuse within 30 minutes.
	var hw time.Time
	_ = s.db.Pool.QueryRow(ctx, `SELECT COALESCE(max(created_at),'epoch') FROM event_updates`).Scan(&hw)
	cats := make([]string, 0, len(in.CategoryIDs))
	for _, c := range in.CategoryIDs {
		cats = append(cats, c.String())
	}
	sort.Strings(cats)
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d|%s|%s|%s|%d", user, in.Language, in.DurationMinutes, strings.Join(cats, ","), in.Region,
		since.Truncate(time.Hour).Format(time.RFC3339), hw.Unix())))
	fp := hex.EncodeToString(h[:])
	if existing, err := scan(s.db.Pool.QueryRow(ctx, `SELECT `+cols+` FROM briefings WHERE user_id=$1 AND cache_fingerprint=$2
		AND status IN ('queued','processing','ready') AND requested_at > now() - interval '30 minutes' ORDER BY requested_at DESC LIMIT 1`, user, fp)); err == nil {
		b, err := s.Get(ctx, user, existing.ID)
		return b, true, err
	}
	id := ids.New()
	var region *string
	if in.Region != "" {
		region = &in.Region
	}
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO briefings (id, user_id, status, trigger, language, duration_minutes, category_ids, region, tone, since, until, cache_fingerprint)
			VALUES ($1,$2,'queued',$3,$4,$5,$6,$7,$8,$9,$10,$11)`, id, user, trigger, in.Language, in.DurationMinutes, nonNilU(in.CategoryIDs), region, in.Tone, since, until, fp); err != nil {
			return err
		}
		if _, err := jobs.Enqueue(ctx, tx, JobGenerate, map[string]string{"briefing_id": id.String()}, jobs.EnqueueOpts{IdempotencyKey: "briefing:" + id.String(), Priority: 50}); err != nil {
			return err
		}
		_, err := bus.Publish(ctx, tx, bus.Draft{Type: bus.BriefingRequested, AggregateType: "briefing", AggregateID: id.String(),
			Payload: map[string]any{"briefing_id": id, "trigger": trigger, "language": in.Language, "duration_minutes": in.DurationMinutes}})
		return err
	})
	if err != nil {
		return nil, false, err
	}
	b, err := s.Get(ctx, user, id)
	return b, false, err
}

// Get returns a briefing owned by user.
func (s *Service) Get(ctx context.Context, user, id uuid.UUID) (*Briefing, error) {
	b, err := scan(s.db.Pool.QueryRow(ctx, `SELECT `+cols+` FROM briefings WHERE id=$1 AND user_id=$2`, id, user))
	if db.IsNoRows(err) {
		return nil, apperr.NotFound("briefing_not_found", "Briefing not found.")
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Pool.Query(ctx, `SELECT position, event_id, event_version, section_text, citations FROM briefing_items WHERE briefing_id=$1 ORDER BY position`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	b.Items = []Item{}
	for rows.Next() {
		var it Item
		var cites []byte
		if err := rows.Scan(&it.Position, &it.EventID, &it.EventVersion, &it.Text, &cites); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(cites, &it.Citations)
		var titles []byte
		_ = s.db.Pool.QueryRow(ctx, `SELECT titles FROM events WHERE id=$1`, it.EventID).Scan(&titles)
		var t map[string]string
		_ = json.Unmarshal(titles, &t)
		it.Title = t[b.Language]
		if it.Title == "" {
			for _, v := range t {
				it.Title = v
				break
			}
		}
		b.Items = append(b.Items, it)
	}
	return b, rows.Err()
}

func (s *Service) Latest(ctx context.Context, user uuid.UUID) (*Briefing, error) {
	var id uuid.UUID
	err := s.db.Pool.QueryRow(ctx, `SELECT id FROM briefings WHERE user_id=$1 AND status='ready' ORDER BY completed_at DESC LIMIT 1`, user).Scan(&id)
	if db.IsNoRows(err) {
		return nil, apperr.NotFound("briefing_not_found", "No ready briefing yet.")
	}
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, user, id)
}

func (s *Service) List(ctx context.Context, user uuid.UUID, limit int) ([]Briefing, error) {
	rows, err := s.db.Pool.Query(ctx, `SELECT `+cols+` FROM briefings WHERE user_id=$1 ORDER BY requested_at DESC LIMIT $2`, user, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Briefing{}
	for rows.Next() {
		b, err := scan(rows)
		if err != nil {
			return nil, err
		}
		b.Text, b.Items = nil, []Item{}
		out = append(out, *b)
	}
	return out, rows.Err()
}

var refMarker = regexp.MustCompile(`\s?\[[a-z]\d{1,3}\]`)

func stripRefs(s string) string { return strings.TrimSpace(refMarker.ReplaceAllString(s, "")) }

// narration returns the canonical narration for an event version + language, generating and
// caching it once (reused across all users' briefings).
func (s *Service) narration(ctx context.Context, e *events.Event, lang string) (string, []Citation, error) {
	var text string
	var cites []byte
	err := s.db.Pool.QueryRow(ctx, `SELECT text, citations FROM event_narrations WHERE event_id=$1 AND event_version=$2 AND language=$3 AND style='neutral'`,
		e.ID, e.Version, lang).Scan(&text, &cites)
	if err == nil {
		var c []Citation
		_ = json.Unmarshal(cites, &c)
		return text, c, nil
	}
	if !db.IsNoRows(err) {
		return "", nil, err
	}
	if e.Summary == nil {
		return "", nil, errors.New("event has no summary")
	}
	text = stripRefs(e.Summary.Normal)
	if e.State == "disputed" {
		switch lang {
		case "ru":
			text += " Источники расходятся в деталях."
		case "en":
			text += " Sources disagree on some details."
		default:
			text += " Sursele se contrazic în privința unor detalii."
		}
	}
	var c []Citation
	for _, sc := range e.Summary.Citations {
		aid := sc.ArticleID
		c = append(c, Citation{Ref: sc.Ref, EventID: e.ID, ArticleID: &aid, Publisher: sc.Publisher, URL: sc.URL, Title: sc.Title, PublishedAt: sc.PublishedAt})
	}
	b, _ := json.Marshal(c)
	_, err = s.db.Pool.Exec(ctx, `INSERT INTO event_narrations (event_id, event_version, language, style, text, citations, model_ref) VALUES ($1,$2,$3,'neutral',$4,$5,$6)
		ON CONFLICT DO NOTHING`, e.ID, e.Version, lang, text, b, e.Summary.ModelRef)
	return text, c, err
}

// RunGenerate is the briefing job handler.
func (s *Service) RunGenerate(ctx context.Context, j *jobs.Job) (any, error) {
	var p struct {
		BriefingID uuid.UUID `json:"briefing_id"`
	}
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		return nil, jobs.Permanent(err)
	}
	b, err := scan(s.db.Pool.QueryRow(ctx, `SELECT `+cols+` FROM briefings WHERE id=$1`, p.BriefingID))
	if err != nil {
		return nil, jobs.Permanent(err)
	}
	if b.Status == "ready" {
		return map[string]string{"status": "already_ready"}, nil
	}
	_, _ = s.db.Pool.Exec(ctx, `UPDATE briefings SET status='processing', started_at=now() WHERE id=$1`, b.ID)
	var cfg struct {
		EventsPerMinute float64 `json:"events_per_minute"`
		MaxEvents       int     `json:"max_events"`
		WordsPerMinute  float64 `json:"words_per_minute"`
	}
	cfg.EventsPerMinute, cfg.MaxEvents, cfg.WordsPerMinute = 1.2, 12, 150
	_, _ = s.pol.Get(ctx, "briefings", &cfg)
	n := int(math.Min(float64(cfg.MaxEvents), math.Ceil(float64(b.DurationMinutes)*cfg.EventsPerMinute)))
	region := ""
	if b.Region != nil {
		region = *b.Region
	}
	eventIDs, err := s.feed.TopEvents(ctx, b.userID, b.Since, b.Until, b.CategoryIDs, region, 0, n)
	if err != nil {
		return nil, err
	}
	if len(eventIDs) == 0 {
		// Widen once to the last 72h so a briefing is never silently empty without saying so.
		eventIDs, err = s.feed.TopEvents(ctx, b.userID, b.Until.Add(-72*time.Hour), b.Until, b.CategoryIDs, region, 0, n)
		if err != nil {
			return nil, err
		}
	}
	if len(eventIDs) == 0 {
		code, msg := "no_events", "No relevant events were found for this briefing window."
		_, err := s.db.Pool.Exec(ctx, `UPDATE briefings SET status='failed', error_code=$2, error_message=$3, completed_at=now() WHERE id=$1`, b.ID, code, msg)
		return map[string]string{"status": "no_events"}, err
	}
	type section struct {
		ev    *events.Event
		text  string
		cites []Citation
	}
	var secs []section
	budgetWords := int(float64(b.DurationMinutes) * cfg.WordsPerMinute)
	perEvent := max(40, budgetWords/len(eventIDs))
	for _, id := range eventIDs {
		e, err := s.events.Get(ctx, id, b.Language)
		if err != nil || e.Summary == nil || e.Language != b.Language {
			continue // only events with a summary in the requested language (no wrong-language audio)
		}
		text, cites, err := s.narration(ctx, e, b.Language)
		if err != nil {
			continue
		}
		words := strings.Fields(text)
		if len(words) > perEvent {
			text = strings.Join(words[:perEvent], " ") + "…"
		}
		secs = append(secs, section{ev: e, text: text, cites: cites})
	}
	if len(secs) == 0 {
		_, err := s.db.Pool.Exec(ctx, `UPDATE briefings SET status='failed', error_code='no_summaries_in_language', error_message=$2, completed_at=now() WHERE id=$1`,
			b.ID, "No event summaries are available in "+b.Language+".")
		return map[string]string{"status": "no_summaries"}, err
	}
	in := &ai.ScriptInput{Language: b.Language, TargetSeconds: b.DurationMinutes * 60, Tone: b.Tone}
	for i, sec := range secs {
		in.Sections = append(in.Sections, ai.ScriptSection{Ref: fmt.Sprintf("s%d", i+1), Title: sec.ev.Title, Text: sec.text})
	}
	var out ai.ScriptOutput
	exec, err := s.gw.GenerateStructured(ctx, ai.TaskScriptGeneration, in, &out, ai.CallOpts{SubjectType: "briefing", SubjectID: b.ID.String()})
	if err != nil {
		if errors.Is(err, ai.ErrDeferred) {
			return nil, err
		}
		msg := err.Error()
		_, _ = s.db.Pool.Exec(ctx, `UPDATE briefings SET status='failed', error_code='generation_failed', error_message=$2, completed_at=now() WHERE id=$1`, b.ID, nlp.Truncate(msg, 500))
		return nil, jobs.Permanent(err)
	}
	// Server-side check: the script may only cite provided sections (validated by the schema
	// contract) and must not introduce numbers absent from the sections (unsupported claims).
	if bad := unsupportedNumbers(out.Script, in.Sections); len(bad) > 0 {
		_, _ = s.db.Pool.Exec(ctx, `UPDATE briefings SET status='failed', error_code='unsupported_content', error_message=$2, completed_at=now() WHERE id=$1`,
			b.ID, "Script contained figures not present in sources: "+strings.Join(bad, ", "))
		return nil, jobs.Permanent(errors.New("unsupported figures in script"))
	}
	script := stripRefs(out.Script)
	words := len(strings.Fields(script))
	secsEst := int(float64(words) / cfg.WordsPerMinute * 60)
	title := out.Title
	if title == "" || title == "Briefing" {
		title = map[string]string{"ro": "Rezumatul știrilor", "ru": "Обзор новостей", "en": "News briefing"}[b.Language]
	}
	err = s.db.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM briefing_items WHERE briefing_id=$1`, b.ID); err != nil {
			return err
		}
		for i, sec := range secs {
			cb, _ := json.Marshal(nonNilC(sec.cites))
			if _, err := tx.Exec(ctx, `INSERT INTO briefing_items (briefing_id, position, event_id, event_version, section_text, citations) VALUES ($1,$2,$3,$4,$5,$6)`,
				b.ID, i+1, sec.ev.ID, sec.ev.Version, sec.text, cb); err != nil {
				return err
			}
		}
		pv := ""
		if exec.PromptVersion != nil {
			pv = exec.PromptVersion.String()
		}
		if _, err := tx.Exec(ctx, `UPDATE briefings SET status='ready', title=$2, text=$3, estimated_seconds=$4, model_ref=$5, prompt_version=$6, completed_at=now(),
			error_code=NULL, error_message=NULL WHERE id=$1`, b.ID, title, script, secsEst, exec.ModelRef, pv); err != nil {
			return err
		}
		_, err := bus.Publish(ctx, tx, bus.Draft{Type: bus.BriefingReady, AggregateType: "briefing", AggregateID: b.ID.String(),
			Payload: map[string]any{"briefing_id": b.ID, "user_id": b.userID, "events": len(secs)}})
		return err
	})
	return map[string]any{"events": len(secs), "estimated_seconds": secsEst}, err
}

var numRe = regexp.MustCompile(`\d+(?:[.,]\d+)?`)

func unsupportedNumbers(script string, secs []ai.ScriptSection) []string {
	allowed := map[string]bool{}
	for _, s := range secs {
		for _, n := range numRe.FindAllString(s.Title+" "+s.Text, -1) {
			allowed[n] = true
		}
		for _, n := range numRe.FindAllString(s.Ref, -1) {
			allowed[n] = true
		}
	}
	var bad []string
	for _, n := range numRe.FindAllString(stripRefs(script), -1) {
		if !allowed[n] {
			bad = append(bad, n)
		}
	}
	return bad
}

func nonNilU(s []uuid.UUID) []uuid.UUID {
	if s == nil {
		return []uuid.UUID{}
	}
	return s
}

func nonNilC(s []Citation) []Citation {
	if s == nil {
		return []Citation{}
	}
	return s
}

// ---------- Audio ----------

type Audio struct {
	ID              uuid.UUID       `json:"id"`
	Status          string          `json:"status"`
	Language        string          `json:"language"`
	Voice           string          `json:"voice"`
	Provider        *string         `json:"provider,omitempty"`
	Model           *string         `json:"model,omitempty"`
	Format          string          `json:"format"`
	DurationMS      *int            `json:"duration_ms,omitempty"`
	SizeBytes       *int64          `json:"size_bytes,omitempty"`
	Checksum        *string         `json:"checksum_sha256,omitempty"`
	Segments        json.RawMessage `json:"segments"`
	CostEstimateUSD *float64        `json:"cost_estimate_usd,omitempty"`
	Error           *string         `json:"error,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	CompletedAt     *time.Time      `json:"completed_at,omitempty"`
	URL             string          `json:"url,omitempty"`
	URLExpiresAt    *time.Time      `json:"url_expires_at,omitempty"`
	objectKey       *string
}

// RequestAudio creates (or returns the existing) audio job for a ready briefing.
func (s *Service) RequestAudio(ctx context.Context, user, briefingID uuid.UUID, voice string) (*Audio, error) {
	b, err := s.Get(ctx, user, briefingID)
	if err != nil {
		return nil, err
	}
	if b.Status != "ready" {
		return nil, apperr.Conflict("briefing_not_ready", "The briefing text is not ready yet.")
	}
	if ok, retry, _ := s.limiter.Allow(ctx, "audio:"+user.String(), 10, 24*time.Hour); !ok {
		return nil, apperr.RateLimited(retry).WithDetail("reason", "daily_audio_quota")
	}
	if voice == "" {
		voice = "default"
	}
	if len(voice) > 40 || strings.ContainsAny(voice, "<>&\"'") {
		return nil, apperr.Validation(apperr.FieldError{Field: "voice", Code: "invalid", Message: "Invalid voice."})
	}
	if a, err := s.audioFor(ctx, user, briefingID); err == nil && a.Status != "failed" && a.Status != "provider_unavailable" {
		return a, nil
	}
	id := ids.New()
	err = s.db.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO audio_assets (id, owner_type, owner_id, user_id, language, voice, status, characters) VALUES ($1,'briefing',$2,$3,$4,$5,'queued',$6)`,
			id, briefingID, user, b.Language, voice, len([]rune(deref(b.Text)))); err != nil {
			return err
		}
		if _, err := jobs.Enqueue(ctx, tx, JobAudio, map[string]string{"audio_id": id.String()}, jobs.EnqueueOpts{IdempotencyKey: "audio:" + id.String(), Priority: 80, MaxAttempts: 3}); err != nil {
			return err
		}
		_, err := bus.Publish(ctx, tx, bus.Draft{Type: bus.AudioRequested, AggregateType: "audio", AggregateID: id.String(), Payload: map[string]any{"audio_id": id, "briefing_id": briefingID}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.audioFor(ctx, user, briefingID)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (s *Service) audioFor(ctx context.Context, user, briefingID uuid.UUID) (*Audio, error) {
	a := &Audio{}
	err := s.db.Pool.QueryRow(ctx, `SELECT id, status, language, voice, provider, model, format, duration_ms, size_bytes, checksum_sha256, segment_manifest,
		cost_estimate_usd::float8, error, created_at, completed_at, object_key FROM audio_assets
		WHERE owner_type='briefing' AND owner_id=$1 AND user_id=$2 ORDER BY created_at DESC LIMIT 1`, briefingID, user).
		Scan(&a.ID, &a.Status, &a.Language, &a.Voice, &a.Provider, &a.Model, &a.Format, &a.DurationMS, &a.SizeBytes, &a.Checksum, &a.Segments,
			&a.CostEstimateUSD, &a.Error, &a.CreatedAt, &a.CompletedAt, &a.objectKey)
	if db.IsNoRows(err) {
		return nil, apperr.NotFound("audio_not_found", "No audio requested for this briefing.")
	}
	return a, err
}

// GetAudio returns status and, when ready, a short-lived download URL (presigned for S3) or the
// authenticated streaming endpoint.
func (s *Service) GetAudio(ctx context.Context, user, briefingID uuid.UUID) (*Audio, error) {
	a, err := s.audioFor(ctx, user, briefingID)
	if err != nil {
		return nil, err
	}
	if a.Status == "ready" && a.objectKey != nil {
		if u, err := s.store.PresignGet(ctx, *a.objectKey, s.urlTTL); err == nil && u != "" {
			exp := s.now().Add(s.urlTTL).UTC()
			a.URL, a.URLExpiresAt = u, &exp
		} else {
			a.URL = fmt.Sprintf("/v1/briefings/%s/audio/stream", briefingID)
		}
	}
	return a, nil
}

// OpenAudio streams the audio object to its owner.
func (s *Service) OpenAudio(ctx context.Context, user, briefingID uuid.UUID) (io.ReadCloser, int64, string, error) {
	a, err := s.audioFor(ctx, user, briefingID)
	if err != nil {
		return nil, 0, "", err
	}
	if a.Status != "ready" || a.objectKey == nil {
		return nil, 0, "", apperr.Conflict("audio_not_ready", "Audio is not ready ("+a.Status+").")
	}
	rc, n, err := s.store.Get(ctx, *a.objectKey)
	return rc, n, "audio/" + map[string]string{"mp3": "mpeg", "opus": "ogg", "wav": "wav"}[a.Format], err
}

var ssmlish = regexp.MustCompile(`<[^>]*>`)

// RunAudio synthesizes per-event segments (reusing canonical narration audio across users) and
// records a manifest. Absent TTS configuration yields provider_unavailable.
func (s *Service) RunAudio(ctx context.Context, j *jobs.Job) (any, error) {
	var p struct {
		AudioID uuid.UUID `json:"audio_id"`
	}
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		return nil, jobs.Permanent(err)
	}
	var briefingID, user uuid.UUID
	var lang, voice, status string
	if err := s.db.Pool.QueryRow(ctx, `SELECT owner_id, user_id, language, voice, status FROM audio_assets WHERE id=$1`, p.AudioID).Scan(&briefingID, &user, &lang, &voice, &status); err != nil {
		return nil, jobs.Permanent(err)
	}
	if status == "ready" || status == "cancelled" {
		return map[string]string{"status": status}, nil
	}
	_, _ = s.db.Pool.Exec(ctx, `UPDATE audio_assets SET status='processing' WHERE id=$1`, p.AudioID)
	b, err := s.Get(ctx, user, briefingID)
	if err != nil {
		return nil, jobs.Permanent(err)
	}
	type seg struct {
		Kind      string     `json:"kind"`
		EventID   *uuid.UUID `json:"event_id,omitempty"`
		ObjectKey string     `json:"object_key"`
		Reused    bool       `json:"reused"`
		Bytes     int        `json:"bytes"`
	}
	var manifest []seg
	var all bytes.Buffer
	var provider, model string
	cost := 0.0
	synth := func(text string) ([]byte, error) {
		clean := strings.TrimSpace(ssmlish.ReplaceAllString(text, " ")) // never pass markup/SSML from content
		if len([]rune(clean)) > 4000 {
			clean = string([]rune(clean)[:4000])
		}
		res, m, err := s.gw.TextToSpeech(ctx, ai.SpeechRequest{Text: clean, Voice: voice, Language: lang, Format: "mp3"}, ai.CallOpts{SubjectType: "audio", SubjectID: p.AudioID.String()})
		if err != nil {
			return nil, err
		}
		provider, model = m.Provider.Slug, m.Slug
		cost += float64(len([]rune(clean))) / 1e6 * m.PricePerMChar
		return res.Audio, nil
	}
	for _, it := range b.Items {
		evID := it.EventID
		key := fmt.Sprintf("audio/narrations/%s/v%d/%s/%s.mp3", it.EventID, it.EventVersion, lang, voice)
		if rc, _, err := s.store.Get(ctx, key); err == nil {
			data, _ := io.ReadAll(rc)
			rc.Close()
			all.Write(data)
			manifest = append(manifest, seg{Kind: "event", EventID: &evID, ObjectKey: key, Reused: true, Bytes: len(data)})
			continue
		}
		data, err := synth(it.Title + ". " + it.Text)
		if err != nil {
			return s.audioFailed(ctx, p.AudioID, err)
		}
		if err := s.store.Put(ctx, key, data, "audio/mpeg"); err != nil {
			return nil, err
		}
		all.Write(data)
		manifest = append(manifest, seg{Kind: "event", EventID: &evID, ObjectKey: key, Bytes: len(data)})
	}
	key := fmt.Sprintf("audio/briefings/%s/%s.mp3", user, briefingID)
	if err := s.store.Put(ctx, key, all.Bytes(), "audio/mpeg"); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(all.Bytes())
	words := len(strings.Fields(deref(b.Text)))
	dur := int(float64(words) / 150 * 60 * 1000)
	mb, _ := json.Marshal(manifest)
	_, err = s.db.Pool.Exec(ctx, `UPDATE audio_assets SET status='ready', provider=$2, model=$3, object_key=$4, size_bytes=$5, checksum_sha256=$6, duration_ms=$7,
		segment_manifest=$8, cost_estimate_usd=$9, completed_at=now(), error=NULL WHERE id=$1`, p.AudioID, provider, model, key, all.Len(), hex.EncodeToString(sum[:]), dur, mb, cost)
	return map[string]any{"segments": len(manifest), "bytes": all.Len()}, err
}

func (s *Service) audioFailed(ctx context.Context, id uuid.UUID, err error) (any, error) {
	if errors.Is(err, ai.ErrTTSUnavailable) {
		_, uerr := s.db.Pool.Exec(ctx, `UPDATE audio_assets SET status='provider_unavailable', error='No text-to-speech provider is configured; text briefing remains available.',
			completed_at=now() WHERE id=$1`, id)
		return map[string]string{"status": "provider_unavailable"}, uerr
	}
	_, _ = s.db.Pool.Exec(ctx, `UPDATE audio_assets SET status='failed', error=$2 WHERE id=$1`, id, nlp.Truncate(err.Error(), 400))
	return nil, err
}

// CancelAudio cancels a queued audio job.
func (s *Service) CancelAudio(ctx context.Context, user, briefingID uuid.UUID) error {
	tag, err := s.db.Pool.Exec(ctx, `UPDATE audio_assets SET status='cancelled', completed_at=now() WHERE owner_id=$1 AND user_id=$2 AND status IN ('queued','processing')`, briefingID, user)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return apperr.Conflict("audio_not_cancellable", "No queued audio to cancel.")
	}
	return nil
}
