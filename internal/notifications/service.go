package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dorinpuscasu25/recapproject/internal/events"
	"github.com/dorinpuscasu25/recapproject/internal/feed"
	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/bus"
	"github.com/dorinpuscasu25/recapproject/internal/platform/crypto"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ids"
	"github.com/dorinpuscasu25/recapproject/internal/policy"
)

// Service owns schedules, devices and delivery records. Sending happens in the
// notification-sender consumer so provider latency never blocks the scheduler.
type Service struct {
	db        *db.DB
	feed      *feed.Service
	events    *events.Service
	pol       *policy.Store
	sealer    *crypto.Sealer
	hashKey   []byte
	providers map[string]PushProvider
	log       *slog.Logger
	now       func() time.Time
}

func NewService(d *db.DB, f *feed.Service, ev *events.Service, pol *policy.Store, sealer *crypto.Sealer, hashKey []byte,
	providers map[string]PushProvider, log *slog.Logger) *Service {
	if providers == nil {
		providers = map[string]PushProvider{}
	}
	return &Service{db: d, feed: f, events: ev, pol: pol, sealer: sealer, hashKey: hashKey, providers: providers, log: log, now: time.Now}
}

// SetProvider swaps a push provider (tests and runtime reconfiguration).
func (s *Service) SetProvider(name string, p PushProvider) { s.providers[name] = p }

// SetClock overrides the clock (tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// Config is the "notifications" editorial policy.
type Config struct {
	MaxPerDay            int     `json:"max_per_day"`
	UpdateMinSeverity    string  `json:"update_min_severity"` // minor | major | critical
	DigestLookbackHours  int     `json:"digest_lookback_hours"`
	UpdateCooldownMinute int     `json:"update_cooldown_minutes"`
	MinImportanceDefault float64 `json:"min_importance_default"`
}

func (s *Service) cfg(ctx context.Context) Config {
	c := Config{MaxPerDay: 6, UpdateMinSeverity: "major", DigestLookbackHours: 24, UpdateCooldownMinute: 60, MinImportanceDefault: 0.3}
	_, _ = s.pol.Get(ctx, "notifications", &c)
	if c.MaxPerDay <= 0 {
		c.MaxPerDay = 6
	}
	return c
}

// ---------- Schedules ----------

type ScheduleView struct {
	ID                  uuid.UUID   `json:"id"`
	Name                string      `json:"name"`
	Kind                string      `json:"kind"`
	LocalTimes          []string    `json:"local_times"`
	Weekdays            []int32     `json:"weekdays"`
	Timezone            string      `json:"timezone"`
	QuietStart          *string     `json:"quiet_start,omitempty"`
	QuietEnd            *string     `json:"quiet_end,omitempty"`
	ImportanceThreshold float32     `json:"importance_threshold"`
	CategoryIDs         []uuid.UUID `json:"category_ids"`
	Language            string      `json:"language"`
	DigestSize          int         `json:"digest_size"`
	Enabled             bool        `json:"enabled"`
	NextRunAt           *time.Time  `json:"next_run_at,omitempty"`
	UpdatedAt           time.Time   `json:"updated_at"`
	userID              uuid.UUID
}

func (v *ScheduleView) model() Schedule {
	wd := make([]int, len(v.Weekdays))
	for i, d := range v.Weekdays {
		wd[i] = int(d)
	}
	return Schedule{Kind: v.Kind, LocalTimes: v.LocalTimes, Weekdays: wd, Timezone: v.Timezone, QuietStart: deref(v.QuietStart), QuietEnd: deref(v.QuietEnd)}
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

type ScheduleInput struct {
	Kind                string      `json:"kind"`
	LocalTimes          []string    `json:"local_times"`
	Weekdays            []int       `json:"weekdays,omitempty"`
	Timezone            string      `json:"timezone"`
	QuietStart          string      `json:"quiet_start,omitempty"`
	QuietEnd            string      `json:"quiet_end,omitempty"`
	ImportanceThreshold *float64    `json:"importance_threshold,omitempty"`
	CategoryIDs         []uuid.UUID `json:"category_ids,omitempty"`
	Language            string      `json:"language,omitempty"`
	DigestSize          int         `json:"digest_size,omitempty"`
	Enabled             *bool       `json:"enabled,omitempty"`
}

const schedCols = `id, user_id, name, kind, local_times, weekdays, timezone, quiet_start, quiet_end, importance_threshold, category_ids, language,
	digest_size, enabled, next_run_at, updated_at`

func scanSched(r pgx.Row) (*ScheduleView, error) {
	v := &ScheduleView{}
	err := r.Scan(&v.ID, &v.userID, &v.Name, &v.Kind, &v.LocalTimes, &v.Weekdays, &v.Timezone, &v.QuietStart, &v.QuietEnd, &v.ImportanceThreshold,
		&v.CategoryIDs, &v.Language, &v.DigestSize, &v.Enabled, &v.NextRunAt, &v.UpdatedAt)
	return v, err
}

func validName(n string) bool {
	if n == "" || len(n) > 40 {
		return false
	}
	for _, r := range n {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// PutSchedule creates or replaces a named schedule and computes its next run.
func (s *Service) PutSchedule(ctx context.Context, user uuid.UUID, name string, in ScheduleInput) (*ScheduleView, error) {
	if !validName(name) {
		return nil, apperr.Validation(apperr.FieldError{Field: "name", Code: "invalid", Message: "1-40 chars: a-z, 0-9, - or _."})
	}
	if in.Kind == "daily" && len(in.Weekdays) == 0 {
		in.Weekdays = []int{1, 2, 3, 4, 5, 6, 7}
	}
	in.LocalTimes = normalizeTimes(in.LocalTimes)
	m := Schedule{Kind: in.Kind, LocalTimes: in.LocalTimes, Weekdays: in.Weekdays, Timezone: in.Timezone, QuietStart: in.QuietStart, QuietEnd: in.QuietEnd}
	if err := m.Validate(); err != nil {
		return nil, apperr.ValidationMsg("invalid_schedule", err.Error())
	}
	if in.Language == "" {
		in.Language = "ro"
	}
	if in.Language != "ro" && in.Language != "ru" && in.Language != "en" {
		return nil, apperr.Validation(apperr.FieldError{Field: "language", Code: "invalid", Message: "ro, ru or en"})
	}
	if in.DigestSize == 0 {
		in.DigestSize = 5
	}
	if in.DigestSize < 1 || in.DigestSize > 20 {
		return nil, apperr.Validation(apperr.FieldError{Field: "digest_size", Code: "out_of_range", Message: "1..20"})
	}
	thr := s.cfg(ctx).MinImportanceDefault
	if in.ImportanceThreshold != nil {
		thr = *in.ImportanceThreshold
	}
	if thr < 0 || thr > 1 {
		return nil, apperr.Validation(apperr.FieldError{Field: "importance_threshold", Code: "out_of_range", Message: "0..1"})
	}
	if len(in.CategoryIDs) > 50 {
		return nil, apperr.Validation(apperr.FieldError{Field: "category_ids", Code: "too_many", Message: "At most 50."})
	}
	var n int
	if err := s.db.Pool.QueryRow(ctx, `SELECT count(*) FROM notification_schedules WHERE user_id=$1 AND name<>$2`, user, name).Scan(&n); err != nil {
		return nil, err
	}
	if n >= 10 {
		return nil, apperr.Conflict("schedule_limit", "At most 10 schedules per user.")
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	var next *time.Time
	if enabled {
		t, _, err := m.Next(s.now())
		if err != nil {
			return nil, apperr.ValidationMsg("invalid_schedule", err.Error())
		}
		next = &t
	}
	var qs, qe *string
	if in.QuietStart != "" {
		qs, qe = &in.QuietStart, &in.QuietEnd
	}
	wd := make([]int32, len(in.Weekdays))
	for i, d := range in.Weekdays {
		wd[i] = int32(d)
	}
	cats := in.CategoryIDs
	if cats == nil {
		cats = []uuid.UUID{}
	}
	return scanSched(s.db.Pool.QueryRow(ctx, `INSERT INTO notification_schedules (id, user_id, name, kind, local_times, weekdays, timezone, quiet_start, quiet_end,
		importance_threshold, category_ids, language, digest_size, enabled, next_run_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		ON CONFLICT (user_id, name) DO UPDATE SET kind=EXCLUDED.kind, local_times=EXCLUDED.local_times, weekdays=EXCLUDED.weekdays,
		  timezone=EXCLUDED.timezone, quiet_start=EXCLUDED.quiet_start, quiet_end=EXCLUDED.quiet_end, importance_threshold=EXCLUDED.importance_threshold,
		  category_ids=EXCLUDED.category_ids, language=EXCLUDED.language, digest_size=EXCLUDED.digest_size, enabled=EXCLUDED.enabled,
		  next_run_at=EXCLUDED.next_run_at, updated_at=now()
		RETURNING `+schedCols, ids.New(), user, name, in.Kind, in.LocalTimes, wd, in.Timezone, qs, qe, thr, cats, in.Language, in.DigestSize, enabled, next))
}

func (s *Service) Schedules(ctx context.Context, user uuid.UUID) ([]ScheduleView, error) {
	rows, err := s.db.Pool.Query(ctx, `SELECT `+schedCols+` FROM notification_schedules WHERE user_id=$1 ORDER BY name`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ScheduleView{}
	for rows.Next() {
		v, err := scanSched(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

func (s *Service) DeleteSchedule(ctx context.Context, user uuid.UUID, name string) error {
	tag, err := s.db.Pool.Exec(ctx, `DELETE FROM notification_schedules WHERE user_id=$1 AND name=$2`, user, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("schedule_not_found", "Schedule not found.")
	}
	return nil
}

// ---------- Devices ----------

type Device struct {
	ID         uuid.UUID `json:"id"`
	Platform   string    `json:"platform"`
	Provider   string    `json:"provider"`
	AppVersion string    `json:"app_version"`
	Locale     string    `json:"locale"`
	Status     string    `json:"status"`
	LastSeenAt time.Time `json:"last_seen_at"`
	CreatedAt  time.Time `json:"created_at"`
}

type DeviceInput struct {
	Platform   string `json:"platform"`
	Provider   string `json:"provider,omitempty"`
	Token      string `json:"token"`
	AppVersion string `json:"app_version,omitempty"`
	Locale     string `json:"locale,omitempty"`
}

func (s *Service) tokenHash(tok string) []byte { return crypto.HMAC(s.hashKey, "device-token", tok) }

// RegisterDevice stores a push token hashed (lookup) and encrypted (delivery). A token moving to
// another account is re-bound: one physical device belongs to one signed-in user.
func (s *Service) RegisterDevice(ctx context.Context, user uuid.UUID, in DeviceInput) (*Device, error) {
	in.Token = strings.TrimSpace(in.Token)
	switch in.Platform {
	case "ios":
		if in.Provider == "" {
			in.Provider = "apns"
		}
	case "android":
		if in.Provider == "" {
			in.Provider = "fcm"
		}
	default:
		return nil, apperr.Validation(apperr.FieldError{Field: "platform", Code: "invalid", Message: "ios or android"})
	}
	if in.Provider != "apns" && in.Provider != "fcm" {
		return nil, apperr.Validation(apperr.FieldError{Field: "provider", Code: "invalid", Message: "apns or fcm"})
	}
	if len(in.Token) < 16 || len(in.Token) > 4096 || strings.ContainsAny(in.Token, " \n\t") {
		return nil, apperr.Validation(apperr.FieldError{Field: "token", Code: "invalid", Message: "Invalid device token."})
	}
	if len(in.AppVersion) > 40 || len(in.Locale) > 16 {
		return nil, apperr.Validation(apperr.FieldError{Field: "app_version", Code: "too_long", Message: "Too long."})
	}
	if in.Locale == "" {
		in.Locale = "ro"
	}
	var n int
	if err := s.db.Pool.QueryRow(ctx, `SELECT count(*) FROM device_tokens WHERE user_id=$1 AND status='active'`, user).Scan(&n); err != nil {
		return nil, err
	}
	if n >= 20 {
		return nil, apperr.Conflict("device_limit", "Too many active devices; remove one first.")
	}
	d := &Device{}
	err := s.db.Pool.QueryRow(ctx, `INSERT INTO device_tokens (id, user_id, platform, provider, token_hash, token_enc, app_version, locale)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (token_hash) DO UPDATE SET user_id=EXCLUDED.user_id, platform=EXCLUDED.platform, provider=EXCLUDED.provider,
		  token_enc=EXCLUDED.token_enc, app_version=EXCLUDED.app_version, locale=EXCLUDED.locale, status='active', revoked_at=NULL, last_seen_at=now()
		RETURNING id, platform, provider, app_version, locale, status, last_seen_at, created_at`,
		ids.New(), user, in.Platform, in.Provider, s.tokenHash(in.Token), s.sealer.Seal([]byte(in.Token)), in.AppVersion, in.Locale).
		Scan(&d.ID, &d.Platform, &d.Provider, &d.AppVersion, &d.Locale, &d.Status, &d.LastSeenAt, &d.CreatedAt)
	return d, err
}

func (s *Service) Devices(ctx context.Context, user uuid.UUID) ([]Device, error) {
	rows, err := s.db.Pool.Query(ctx, `SELECT id, platform, provider, app_version, locale, status, last_seen_at, created_at FROM device_tokens
		WHERE user_id=$1 AND status<>'revoked' ORDER BY created_at DESC`, user)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Device, error) {
		var d Device
		err := r.Scan(&d.ID, &d.Platform, &d.Provider, &d.AppVersion, &d.Locale, &d.Status, &d.LastSeenAt, &d.CreatedAt)
		return d, err
	})
}

func (s *Service) RevokeDevice(ctx context.Context, user, id uuid.UUID) error {
	tag, err := s.db.Pool.Exec(ctx, `UPDATE device_tokens SET status='revoked', revoked_at=now() WHERE id=$1 AND user_id=$2 AND status<>'revoked'`, id, user)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("device_not_found", "Device not found.")
	}
	return nil
}

// ---------- Deliveries ----------

type Payload struct {
	Title    string            `json:"title"`
	Body     string            `json:"body"`
	Language string            `json:"language"`
	EventIDs []uuid.UUID       `json:"event_ids"`
	Data     map[string]string `json:"data"`
}

type DeliveryView struct {
	ID            uuid.UUID  `json:"id"`
	Kind          string     `json:"kind"`
	ScheduleID    *uuid.UUID `json:"schedule_id,omitempty"`
	OccurrenceKey string     `json:"occurrence_key"`
	DeviceID      *uuid.UUID `json:"device_id,omitempty"`
	Status        string     `json:"status"`
	SkipReason    *string    `json:"skip_reason,omitempty"`
	Attempts      int        `json:"attempts"`
	Payload       Payload    `json:"payload"`
	CreatedAt     time.Time  `json:"created_at"`
	SentAt        *time.Time `json:"sent_at,omitempty"`
}

func (s *Service) Deliveries(ctx context.Context, user uuid.UUID, limit int) ([]DeliveryView, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.db.Pool.Query(ctx, `SELECT id, kind, schedule_id, occurrence_key, device_token_id, status, skip_reason, attempts, payload, created_at, sent_at
		FROM notification_deliveries WHERE user_id=$1 ORDER BY created_at DESC, id DESC LIMIT $2`, user, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (DeliveryView, error) {
		var d DeliveryView
		var raw []byte
		err := r.Scan(&d.ID, &d.Kind, &d.ScheduleID, &d.OccurrenceKey, &d.DeviceID, &d.Status, &d.SkipReason, &d.Attempts, &raw, &d.CreatedAt, &d.SentAt)
		if err == nil {
			_ = json.Unmarshal(raw, &d.Payload)
		}
		return d, err
	})
}

// sentToday counts notifications actually sent or queued in the user's last 24h (anti-spam budget).
func sentToday(ctx context.Context, q db.Querier, user uuid.UUID, now time.Time) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(DISTINCT (coalesce(schedule_id,'00000000-0000-0000-0000-000000000000'::uuid), occurrence_key))
		FROM notification_deliveries WHERE user_id=$1 AND status IN ('queued','sent') AND created_at > $2`, user, now.Add(-24*time.Hour)).Scan(&n)
	return n, err
}

// enqueue writes one delivery row per active device (or one skipped row when nothing can be sent)
// and publishes notification.requested for each queued row. Dedupe makes it idempotent per occurrence.
func (s *Service) enqueue(ctx context.Context, tx pgx.Tx, user uuid.UUID, scheduleID *uuid.UUID, occurrence, kind string, p Payload, skip string) (int, error) {
	raw, _ := json.Marshal(p)
	if skip != "" {
		_, err := tx.Exec(ctx, `INSERT INTO notification_deliveries (id, user_id, schedule_id, occurrence_key, kind, payload, status, skip_reason)
			VALUES ($1,$2,$3,$4,$5,$6,'skipped',$7) ON CONFLICT DO NOTHING`, ids.New(), user, scheduleID, occurrence, kind, raw, skip)
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT id FROM device_tokens WHERE user_id=$1 AND status='active'`, user)
	if err != nil {
		return 0, err
	}
	devs, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return 0, err
	}
	if len(devs) == 0 {
		_, err := tx.Exec(ctx, `INSERT INTO notification_deliveries (id, user_id, schedule_id, occurrence_key, kind, payload, status, skip_reason)
			VALUES ($1,$2,$3,$4,$5,$6,'skipped','no_devices') ON CONFLICT DO NOTHING`, ids.New(), user, scheduleID, occurrence, kind, raw)
		return 0, err
	}
	queued := 0
	for _, dev := range devs {
		id := ids.New()
		tag, err := tx.Exec(ctx, `INSERT INTO notification_deliveries (id, user_id, schedule_id, occurrence_key, device_token_id, kind, payload)
			VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`, id, user, scheduleID, occurrence, dev, kind, raw)
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		queued++
		if _, err := bus.Publish(ctx, tx, bus.Draft{Type: bus.NotificationRequested, AggregateType: "notification", AggregateID: id.String(),
			Key: user.String(), Payload: map[string]any{"delivery_id": id}}); err != nil {
			return 0, err
		}
	}
	return queued, nil
}

var digestTitle = map[string]string{"ro": "Rezumatul tău de știri", "ru": "Ваша сводка новостей", "en": "Your news digest"}
var updateTitle = map[string]string{"ro": "Actualizare: ", "ru": "Обновление: ", "en": "Update: "}
var moreFmt = map[string]string{"ro": " și încă %d", "ru": " и ещё %d", "en": " and %d more"}

// ---------- Scheduler ----------

// Tick processes due schedules. Rows are claimed with SKIP LOCKED so several workers can run it.
func (s *Service) Tick(ctx context.Context) (int, error) {
	processed := 0
	for i := 0; i < 20; i++ {
		done, err := s.tickOne(ctx)
		if err != nil {
			return processed, err
		}
		if !done {
			return processed, nil
		}
		processed++
	}
	return processed, nil
}

func (s *Service) tickOne(ctx context.Context) (bool, error) {
	now := s.now()
	c := s.cfg(ctx)
	found := false
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		v, err := scanSched(tx.QueryRow(ctx, `SELECT `+schedCols+` FROM notification_schedules WHERE enabled AND next_run_at <= $1
			ORDER BY next_run_at LIMIT 1 FOR UPDATE SKIP LOCKED`, now))
		if db.IsNoRows(err) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		m := v.model()
		// The occurrence being fired is the one at next_run_at; its key comes from the schedule model.
		at, occurrence, err := m.Next(v.NextRunAt.Add(-time.Second))
		if err != nil || at.After(now) {
			occurrence = v.NextRunAt.UTC().Format("2006-01-02T15:04") + "@" + v.Timezone
		}
		next, _, err := m.Next(now)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE notification_schedules SET next_run_at=$2 WHERE id=$1`, v.ID, next); err != nil {
			return err
		}
		// A schedule fired late (worker outage) more than an hour past its time is skipped, not sent stale.
		if now.Sub(*v.NextRunAt) > time.Hour {
			_, err := s.enqueue(ctx, tx, v.userID, &v.ID, occurrence, "digest", Payload{Language: v.Language, EventIDs: []uuid.UUID{}}, "missed_window")
			return err
		}
		if m.InQuietHours(now) {
			_, err := s.enqueue(ctx, tx, v.userID, &v.ID, occurrence, "digest", Payload{Language: v.Language, EventIDs: []uuid.UUID{}}, "quiet_hours")
			return err
		}
		if n, err := sentToday(ctx, tx, v.userID, now); err != nil {
			return err
		} else if n >= c.MaxPerDay {
			_, err := s.enqueue(ctx, tx, v.userID, &v.ID, occurrence, "digest", Payload{Language: v.Language, EventIDs: []uuid.UUID{}}, "daily_budget")
			return err
		}
		since := now.Add(-time.Duration(c.DigestLookbackHours) * time.Hour)
		var last *time.Time
		if err := tx.QueryRow(ctx, `SELECT max(created_at) FROM notification_deliveries WHERE schedule_id=$1 AND status IN ('sent','queued')`, v.ID).Scan(&last); err != nil {
			return err
		}
		if last != nil && last.After(since) {
			since = *last
		}
		evIDs, err := s.feed.TopEvents(ctx, v.userID, since, now, v.CategoryIDs, "", float64(v.ImportanceThreshold), v.DigestSize)
		if err != nil {
			return err
		}
		if len(evIDs) == 0 {
			_, err := s.enqueue(ctx, tx, v.userID, &v.ID, occurrence, "digest", Payload{Language: v.Language, EventIDs: []uuid.UUID{}}, "no_new_content")
			return err
		}
		p := s.digestPayload(ctx, v.Language, evIDs)
		_, err = s.enqueue(ctx, tx, v.userID, &v.ID, occurrence, "digest", p, "")
		return err
	})
	return found, err
}

func (s *Service) digestPayload(ctx context.Context, lang string, evIDs []uuid.UUID) Payload {
	var titles []string
	for _, id := range evIDs {
		if e, err := s.events.Get(ctx, id, lang); err == nil {
			titles = append(titles, e.Title)
		}
	}
	body := ""
	if len(titles) > 0 {
		body = truncate(titles[0], 120)
		if len(titles) > 1 {
			body += fmt.Sprintf(moreFmt[lang], len(titles)-1)
		}
	}
	strs := make([]string, len(evIDs))
	for i, id := range evIDs {
		strs[i] = id.String()
	}
	return Payload{Title: digestTitle[lang], Body: body, Language: lang, EventIDs: evIDs,
		Data: map[string]string{"type": "digest", "event_ids": strings.Join(strs, ",")}}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// ---------- Followed-event updates ----------

var severityRank = map[string]int{"minor": 1, "major": 2, "critical": 3}

// HandleEventUpdated notifies followers of a material, approved update at or above the policy severity.
// Updates still awaiting editorial approval are never pushed (they are not public yet).
func (s *Service) HandleEventUpdated(ctx context.Context, del *bus.Delivery) error {
	var ch events.Changed
	if err := del.Message.Decode(&ch); err != nil {
		return bus.PermanentErr(err)
	}
	c := s.cfg(ctx)
	if !ch.Material || severityRank[ch.Severity] < severityRank[c.UpdateMinSeverity] {
		return del.Once(ctx, func(pgx.Tx) error { return nil })
	}
	var pending bool
	err := s.db.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM event_updates WHERE event_id=$1 AND event_version=$2 AND requires_approval AND approved_at IS NULL)`,
		ch.EventID, ch.Version).Scan(&pending)
	if err != nil {
		return err
	}
	if pending {
		return del.Once(ctx, func(pgx.Tx) error { return nil })
	}
	now := s.now()
	return del.Once(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT f.user_id, coalesce(p.summary_language,'ro') FROM followed_events f
			JOIN users u ON u.id=f.user_id AND u.status='active'
			LEFT JOIN user_preferences p ON p.user_id=f.user_id WHERE f.event_id=$1`, ch.EventID)
		if err != nil {
			return err
		}
		type follower struct {
			id   uuid.UUID
			lang string
		}
		fs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (follower, error) {
			var f follower
			return f, r.Scan(&f.id, &f.lang)
		})
		if err != nil {
			return err
		}
		occurrence := fmt.Sprintf("event:%s:v%d", ch.EventID, ch.Version)
		for _, f := range fs {
			p := s.updatePayload(ctx, ch, f.lang)
			skip, err := s.updateSkipReason(ctx, tx, f.id, ch.EventID, now, c)
			if err != nil {
				return err
			}
			if _, err := s.enqueue(ctx, tx, f.id, nil, occurrence, "event_update", p, skip); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Service) updateSkipReason(ctx context.Context, tx pgx.Tx, user, event uuid.UUID, now time.Time, c Config) (string, error) {
	// Quiet hours: honour the user's schedules' quiet windows.
	rows, err := tx.Query(ctx, `SELECT `+schedCols+` FROM notification_schedules WHERE user_id=$1 AND enabled AND quiet_start IS NOT NULL`, user)
	if err != nil {
		return "", err
	}
	var scheds []*ScheduleView
	for rows.Next() {
		v, err := scanSched(rows)
		if err != nil {
			rows.Close()
			return "", err
		}
		scheds = append(scheds, v)
	}
	rows.Close()
	for _, v := range scheds {
		if v.model().InQuietHours(now) {
			return "quiet_hours", nil
		}
	}
	if n, err := sentToday(ctx, tx, user, now); err != nil {
		return "", err
	} else if n >= c.MaxPerDay {
		return "daily_budget", nil
	}
	var recent bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM notification_deliveries WHERE user_id=$1 AND kind='event_update' AND status IN ('queued','sent')
		AND occurrence_key LIKE $2 AND created_at > $3)`, user, "event:"+event.String()+":%", now.Add(-time.Duration(c.UpdateCooldownMinute)*time.Minute)).Scan(&recent); err != nil {
		return "", err
	}
	if recent {
		return "cooldown", nil
	}
	return "", nil
}

func (s *Service) updatePayload(ctx context.Context, ch events.Changed, lang string) Payload {
	if _, ok := updateTitle[lang]; !ok {
		lang = "ro"
	}
	p := Payload{Title: updateTitle[lang], Language: lang, EventIDs: []uuid.UUID{ch.EventID},
		Data: map[string]string{"type": "event_update", "event_id": ch.EventID.String(), "version": fmt.Sprint(ch.Version)}}
	if e, err := s.events.Get(ctx, ch.EventID, lang); err == nil {
		p.Title += truncate(e.Title, 90)
	}
	var what []byte
	_ = s.db.Pool.QueryRow(ctx, `SELECT what_changed FROM event_updates WHERE event_id=$1 AND event_version=$2 ORDER BY created_at DESC LIMIT 1`,
		ch.EventID, ch.Version).Scan(&what)
	var w struct {
		Summary string `json:"summary"`
	}
	_ = json.Unmarshal(what, &w)
	p.Body = truncate(w.Summary, 160)
	return p
}

// ---------- Sender ----------

// HandleRequested sends one delivery. Transient provider errors are retried by the bus; missing
// credentials are recorded as provider_unavailable; rejected tokens deactivate the device.
func (s *Service) HandleRequested(ctx context.Context, del *bus.Delivery) error {
	var msg struct {
		DeliveryID uuid.UUID `json:"delivery_id"`
	}
	if err := del.Message.Decode(&msg); err != nil {
		return bus.PermanentErr(err)
	}
	var (
		status, provider string
		devID            *uuid.UUID
		tokenEnc, raw    []byte
		devStatus        *string
	)
	err := s.db.Pool.QueryRow(ctx, `SELECT d.status, d.device_token_id, coalesce(t.provider,''), t.token_enc, t.status, d.payload
		FROM notification_deliveries d LEFT JOIN device_tokens t ON t.id=d.device_token_id WHERE d.id=$1`, msg.DeliveryID).
		Scan(&status, &devID, &provider, &tokenEnc, &devStatus, &raw)
	if db.IsNoRows(err) {
		return del.Once(ctx, func(pgx.Tx) error { return nil })
	}
	if err != nil {
		return err
	}
	if status != "queued" {
		return del.Once(ctx, func(pgx.Tx) error { return nil })
	}
	finish := func(st, skip, errText, providerID string) error {
		return del.Once(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE notification_deliveries SET status=$2, skip_reason=nullif($3,''), error=nullif($4,''),
				provider_message_id=nullif($5,''), attempts=attempts+1, sent_at=CASE WHEN $2='sent' THEN now() END WHERE id=$1 AND status='queued'`,
				msg.DeliveryID, st, skip, errText, providerID)
			return err
		})
	}
	if devID == nil || devStatus == nil || *devStatus != "active" {
		return finish("skipped", "device_inactive", "", "")
	}
	var p Payload
	_ = json.Unmarshal(raw, &p)
	prov, ok := s.providers[provider]
	if !ok {
		prov = Unconfigured{Provider: provider}
	}
	tok, err := s.sealer.Open(tokenEnc)
	if err != nil {
		return finish("failed", "", "token_decrypt_failed", "")
	}
	res, err := prov.Send(ctx, PushMessage{Token: string(tok), Title: p.Title, Body: p.Body, Data: p.Data,
		CollapseKey: collapseKey(p), HighPriority: false})
	var te *TransientError
	switch {
	case errors.Is(err, ErrNotConfigured):
		return finish("provider_unavailable", "", provider+" credentials not configured", "")
	case errors.As(err, &te):
		if del.Attempt >= 5 {
			return finish("failed", "", te.Error(), "")
		}
		_, _ = s.db.Pool.Exec(ctx, `UPDATE notification_deliveries SET attempts=attempts+1, error=$2 WHERE id=$1`, msg.DeliveryID, te.Error())
		return err
	case err != nil:
		s.log.Warn("push send failed", "delivery_id", msg.DeliveryID, "provider", provider, "error", err)
		return finish("failed", "", truncate(err.Error(), 300), "")
	case res.InvalidToken:
		if _, err := s.db.Pool.Exec(ctx, `UPDATE device_tokens SET status='invalid' WHERE id=$1`, *devID); err != nil {
			return err
		}
		return finish("invalid_token", "", "", "")
	default:
		return finish("sent", "", "", res.ProviderMessageID)
	}
}

func collapseKey(p Payload) string {
	if p.Data["type"] == "event_update" {
		return "ev-" + p.Data["event_id"]
	}
	return ""
}
