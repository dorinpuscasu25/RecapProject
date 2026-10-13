package briefings_test

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dorinpuscasu25/recapproject/internal/ai"
	"github.com/dorinpuscasu25/recapproject/internal/notifications"
	"github.com/dorinpuscasu25/recapproject/internal/testutil"
)

func eventOf(t *testing.T, e *testutil.Env, articleTitle string) string {
	t.Helper()
	var id string
	if err := e.App.DB.Pool.QueryRow(e.Ctx, `SELECT l.event_id::text FROM event_article_links l JOIN articles a ON a.id=l.article_id
		WHERE a.title=$1 AND l.unlinked_at IS NULL`, articleTitle).Scan(&id); err != nil {
		t.Fatalf("event for %q: %v", articleTitle, err)
	}
	return id
}

func newsEnv(t *testing.T) (*testutil.Env, string) {
	e := testutil.NewEnv(t)
	admin := e.Admin("exp-admin@example.com", "super_admin")
	_, ro := e.SetupSource(admin, testutil.FixtureSource{Name: "Știri Exemplu", Domain: "stiri-exemplu.md", Feed: "stiri-exemplu.xml", Languages: []string{"ro"}})
	_, off := e.SetupSource(admin, testutil.FixtureSource{Name: "ANRE exemplu", Domain: "anre-exemplu.md", Type: "government", Feed: "anre-exemplu.xml", FetchPages: true, Languages: []string{"ro"}})
	_, div := e.SetupSource(admin, testutil.FixtureSource{Name: "Știri Diverse Exemplu", Domain: "diverse-exemplu.md", Feed: "diverse-ro.xml", Languages: []string{"ro"}})
	e.Crawl(admin, off)
	e.Crawl(admin, ro)
	e.Crawl(admin, div)
	return e, admin
}

// fakeSpeaker is a deterministic TTS stand-in (dev/test only) that records calls.
type fakeSpeaker struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeSpeaker) Speak(_ context.Context, _ *ai.Model, req ai.SpeechRequest) (*ai.SpeechResult, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return &ai.SpeechResult{Audio: []byte("FAKEMP3|" + req.Language + "|" + req.Voice + "|"), ContentType: "audio/mpeg", DurationMS: 1000,
		Usage: ai.Usage{Characters: len([]rune(req.Text))}}, nil
}

// Scenario 10: a briefing lists events in order with citations; audio reports provider_unavailable
// without TTS, then works after an admin configures a TTS route, reusing per-event segments.
func TestBriefingAndAudio(t *testing.T) {
	e, admin := newsEnv(t)
	user, _, _ := e.RegisterVerified("listener@example.com")

	b := e.MustStatus(e.Do("POST", "/v1/briefings", user, map[string]any{"duration_minutes": 5, "language": "ro"}), 202)
	id := b.Str("id")
	e.Drain()
	b = e.MustStatus(e.Do("GET", "/v1/briefings/"+id, user, nil), 200)
	if b.Str("status") != "ready" || b.Str("text") == "" {
		t.Fatalf("briefing not ready: %s", b.Raw)
	}
	items := b.Get("items").([]any)
	if len(items) < 2 {
		t.Fatalf("expected several events: %s", b.Raw)
	}
	for i, it := range items {
		m := it.(map[string]any)
		if int(m["position"].(float64)) != i+1 {
			t.Fatalf("items out of order: %s", b.Raw)
		}
		cites := m["citations"].([]any)
		if len(cites) == 0 || cites[0].(map[string]any)["url"] == "" {
			t.Fatalf("item %d lacks citations: %s", i, b.Raw)
		}
	}
	// Another user cannot read it.
	other, _, _ := e.RegisterVerified("other@example.com")
	e.MustStatus(e.Do("GET", "/v1/briefings/"+id, other, nil), 404)
	if l := e.MustStatus(e.Do("GET", "/v1/briefings/latest", user, nil), 200); l.Str("id") != id {
		t.Fatalf("latest: %s", l.Raw)
	}

	// No TTS route configured: explicit provider_unavailable, never fake audio.
	e.MustStatus(e.Do("POST", "/v1/briefings/"+id+"/audio", user, nil), 202)
	e.Drain()
	a := e.MustStatus(e.Do("GET", "/v1/briefings/"+id+"/audio", user, nil), 200)
	if a.Str("status") != "provider_unavailable" || a.Str("url") != "" {
		t.Fatalf("expected provider_unavailable: %s", a.Raw)
	}
	e.MustStatus(e.Do("GET", "/v1/briefings/"+id+"/audio/stream", user, nil), 409)

	// Admin configures a TTS provider/model/route through the API (no code change).
	p := e.MustStatus(e.Do("POST", "/v1/admin/ai/providers", admin, map[string]any{"slug": "tts-test", "kind": "openai_compatible",
		"display_name": "TTS test", "base_url": "http://127.0.0.1:9", "enabled": true}), 201)
	m := e.MustStatus(e.Do("POST", "/v1/admin/ai/models", admin, map[string]any{"provider_id": p.Str("id"), "slug": "tts-test-voice",
		"model_name": "tts-1", "kind": "tts", "languages": []string{"ro", "ru", "en"}, "price_per_mchar": 15.0, "enabled": true}), 201)
	e.MustStatus(e.Do("POST", "/v1/admin/ai/routes", admin, map[string]any{"task": "tts", "primary_model_id": m.Str("id"), "enabled": true}), 201)
	speaker := &fakeSpeaker{}
	e.App.AIGateway().OverrideProvider("tts-test", speaker)
	e.App.AIGateway().Invalidate()

	e.MustStatus(e.Do("POST", "/v1/briefings/"+id+"/audio", user, map[string]any{"voice": "default"}), 202)
	e.Drain()
	a = e.MustStatus(e.Do("GET", "/v1/briefings/"+id+"/audio", user, nil), 200)
	if a.Str("status") != "ready" || a.Str("url") == "" || a.Str("provider") != "tts-test" {
		t.Fatalf("audio not ready: %s", a.Raw)
	}
	resp := e.Do("GET", "/v1/briefings/"+id+"/audio/stream", user, nil)
	if resp.Status != 200 || !bytes.Contains(resp.Raw, []byte("FAKEMP3|ro|default|")) {
		t.Fatalf("stream: %d %.200s", resp.Status, resp.Raw)
	}
	e.MustStatus(e.Do("GET", "/v1/briefings/"+id+"/audio/stream", other, nil), 404)
	firstCalls := speaker.calls

	// A second listener with the same events reuses canonical per-event narration segments.
	b2 := e.MustStatus(e.Do("POST", "/v1/briefings", other, map[string]any{"duration_minutes": 5, "language": "ro"}), 202)
	e.Drain()
	e.MustStatus(e.Do("POST", "/v1/briefings/"+b2.Str("id")+"/audio", other, nil), 202)
	e.Drain()
	a2 := e.MustStatus(e.Do("GET", "/v1/briefings/"+b2.Str("id")+"/audio", other, nil), 200)
	if a2.Str("status") != "ready" || !bytes.Contains(a2.Raw, []byte(`"reused":true`)) {
		t.Fatalf("expected reused segments: %s", a2.Raw)
	}
	if speaker.calls-firstCalls >= firstCalls {
		t.Fatalf("segments were re-synthesized: %d then %d", firstCalls, speaker.calls-firstCalls)
	}
}

// Scenario 11: grounded chat over an event answers with links and stated uncertainty; unrelated
// questions abstain instead of inventing.
func TestGroundedChat(t *testing.T) {
	e, _ := newsEnv(t)
	user, _, _ := e.RegisterVerified("asker@example.com")
	ev := eventOf(t, e, "Comunicat: ANRE a aprobat noile tarife la energia electrică")

	s := e.MustStatus(e.Do("POST", "/v1/chat/sessions", user, map[string]any{"scope_type": "event", "scope_id": ev, "language": "ro"}), 201)
	msg := e.MustStatus(e.Do("POST", "/v1/chat/sessions/"+s.Str("id")+"/messages", user,
		map[string]any{"content": "Why is this important for a Moldova business?"}), 201)
	if msg.Get("abstained") == true || msg.Str("content") == "" {
		t.Fatalf("expected a grounded answer: %s", msg.Raw)
	}
	cites := msg.Get("citations").([]any)
	if len(cites) == 0 {
		t.Fatalf("answer without citations: %s", msg.Raw)
	}
	hasURL := false
	for _, c := range cites {
		cm := c.(map[string]any)
		if u, _ := cm["url"].(string); strings.HasPrefix(u, "https://") {
			hasURL = true
		}
		if srcs, ok := cm["sources"].([]any); ok && len(srcs) > 0 && strings.HasPrefix(srcs[0].(map[string]any)["url"].(string), "https://") {
			hasURL = true
		}
	}
	if !hasURL || msg.Str("uncertainty") == "" {
		t.Fatalf("answer needs source links and an uncertainty note: %s", msg.Raw)
	}
	// History is persisted and owned.
	full := e.MustStatus(e.Do("GET", "/v1/chat/sessions/"+s.Str("id"), user, nil), 200)
	if len(full.Get("messages").([]any)) != 2 {
		t.Fatalf("history: %s", full.Raw)
	}
	other, _, _ := e.RegisterVerified("snoop@example.com")
	e.MustStatus(e.Do("GET", "/v1/chat/sessions/"+s.Str("id"), other, nil), 404)

	// Out-of-scope question in a global session: abstain, no fabricated citations.
	g := e.MustStatus(e.Do("POST", "/v1/chat/sessions", user, map[string]any{"scope_type": "global", "language": "ro"}), 201)
	ab := e.MustStatus(e.Do("POST", "/v1/chat/sessions/"+g.Str("id")+"/messages", user,
		map[string]any{"content": "Care este scorul meciului de cricket de pe Marte?"}), 201)
	if ab.Get("abstained") != true || len(ab.Get("citations").([]any)) != 0 {
		t.Fatalf("expected abstention: %s", ab.Raw)
	}

	// One-shot explanation of an event.
	ex := e.MustStatus(e.Do("GET", "/v1/events/"+ev+"/explain?level=short&lang=ro", user, nil), 200)
	if ex.Str("content") == "" || len(ex.Get("citations").([]any)) == 0 {
		t.Fatalf("explain: %s", ex.Raw)
	}
	e.MustStatus(e.Do("DELETE", "/v1/chat/sessions/"+s.Str("id"), user, nil), 204)
	e.MustStatus(e.Do("GET", "/v1/chat/sessions/"+s.Str("id"), user, nil), 404)
}

type fakePush struct {
	mu      sync.Mutex
	sent    []notifications.PushMessage
	invalid map[string]bool
}

func (f *fakePush) Name() string { return "apns" }
func (f *fakePush) Send(_ context.Context, m notifications.PushMessage) (*notifications.PushResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.invalid[m.Token] {
		return &notifications.PushResult{InvalidToken: true}, nil
	}
	f.sent = append(f.sent, m)
	return &notifications.PushResult{ProviderMessageID: "fake-1"}, nil
}

func deliveries(t *testing.T, e *testutil.Env, token string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, it := range e.MustStatus(e.Do("GET", "/v1/notifications/deliveries", token, nil), 200).Items() {
		out = append(out, it.(map[string]any))
	}
	return out
}

// Notifications: timezone schedules, explicit provider_unavailable, dedupe, quiet hours, invalid tokens.
func TestScheduledNotifications(t *testing.T) {
	e, _ := newsEnv(t)
	user, _, _ := e.RegisterVerified("pushy@example.com")
	svc := e.App.NotificationService()
	loc, _ := time.LoadLocation("Europe/Chisinau")
	now := time.Now().UTC().Truncate(time.Minute)
	clock := now
	svc.SetClock(func() time.Time { return clock })

	e.MustStatus(e.Do("POST", "/v1/devices", user, map[string]any{"platform": "ios", "token": "short"}), 422)
	dev := e.MustStatus(e.Do("POST", "/v1/devices", user, map[string]any{"platform": "ios", "token": "apns-device-token-0123456789abcdef", "app_version": "1.0.0"}), 201)
	if bytes.Contains(dev.Raw, []byte("0123456789abcdef")) {
		t.Fatal("device token must never be echoed")
	}
	at := now.Add(10 * time.Minute).In(loc).Format("15:04")
	e.MustStatus(e.Do("PUT", "/v1/notifications/schedules/morning", user, map[string]any{"kind": "daily", "local_times": []string{"25:00"}, "timezone": "Europe/Chisinau"}), 422)
	sc := e.MustStatus(e.Do("PUT", "/v1/notifications/schedules/morning", user, map[string]any{"kind": "daily", "local_times": []string{at},
		"timezone": "Europe/Chisinau", "importance_threshold": 0.0}), 200)
	next, err := time.Parse(time.RFC3339, sc.Str("next_run_at"))
	if err != nil || !next.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("next_run_at %v (%s), want %v", next, sc.Raw, now.Add(10*time.Minute))
	}

	// Not due yet.
	if n, _ := svc.Tick(e.Ctx); n != 0 {
		t.Fatalf("schedule fired early: %d", n)
	}
	// Due; no push credentials configured → provider_unavailable (not "sent").
	clock = next.Add(time.Minute)
	if n, err := svc.Tick(e.Ctx); err != nil || n != 1 {
		t.Fatalf("tick: %d %v", n, err)
	}
	if n, _ := svc.Tick(e.Ctx); n != 0 {
		t.Fatalf("occurrence processed twice: %d", n)
	}
	e.Drain()
	ds := deliveries(t, e, user)
	if len(ds) != 1 || ds[0]["status"] != "provider_unavailable" || ds[0]["kind"] != "digest" {
		t.Fatalf("deliveries: %+v", ds)
	}
	if p := ds[0]["payload"].(map[string]any); p["title"] == "" || len(p["event_ids"].([]any)) == 0 {
		t.Fatalf("digest payload: %+v", p)
	}

	// A schedule whose window has no new events is skipped, not sent empty.
	clock = next.Add(24*time.Hour + time.Minute)
	if n, _ := svc.Tick(e.Ctx); n != 1 {
		t.Fatal("next day's occurrence should be processed")
	}
	e.Drain()
	if ds = deliveries(t, e, user); ds[0]["status"] != "skipped" || ds[0]["skip_reason"] != "no_new_content" {
		t.Fatalf("expected no_new_content: %+v", ds[0])
	}

	put := func(name string, offset time.Duration, extra map[string]any) time.Time {
		body := map[string]any{"kind": "daily", "local_times": []string{now.Add(offset).In(loc).Format("15:04")}, "timezone": "Europe/Chisinau", "importance_threshold": 0.0}
		for k, v := range extra {
			body[k] = v
		}
		clock = now
		r := e.MustStatus(e.Do("PUT", "/v1/notifications/schedules/"+name, user, body), 200)
		ts, _ := time.Parse(time.RFC3339, r.Str("next_run_at"))
		return ts
	}

	// With a configured provider the push is sent (token decrypted only at send time).
	fp := &fakePush{invalid: map[string]bool{}}
	svc.SetProvider("apns", fp)
	clock = put("evening", 20*time.Minute, nil).Add(time.Minute)
	if n, _ := svc.Tick(e.Ctx); n != 1 {
		t.Fatalf("evening occurrence: %d", n)
	}
	e.Drain()
	if len(fp.sent) != 1 || fp.sent[0].Token != "apns-device-token-0123456789abcdef" || fp.sent[0].Title == "" {
		t.Fatalf("push not sent: %+v", fp.sent)
	}
	if ds = deliveries(t, e, user); ds[0]["status"] != "sent" {
		t.Fatalf("latest delivery: %+v", ds[0])
	}

	// Quiet hours covering the scheduled time → skipped with a reason.
	q := now.Add(30 * time.Minute).In(loc)
	clock = put("night", 30*time.Minute, map[string]any{"quiet_start": q.Add(-10 * time.Minute).Format("15:04"), "quiet_end": q.Add(10 * time.Minute).Format("15:04")}).Add(time.Minute)
	svc.Tick(e.Ctx)
	e.Drain()
	if ds = deliveries(t, e, user); ds[0]["status"] != "skipped" || ds[0]["skip_reason"] != "quiet_hours" {
		t.Fatalf("quiet hours: %+v", ds[0])
	}
	e.MustStatus(e.Do("DELETE", "/v1/notifications/schedules/night", user, nil), 204)

	// Provider rejects the token → delivery invalid_token, device deactivated.
	fp.invalid["apns-device-token-0123456789abcdef"] = true
	clock = put("late", 40*time.Minute, nil).Add(time.Minute)
	svc.Tick(e.Ctx)
	e.Drain()
	if ds = deliveries(t, e, user); ds[0]["status"] != "invalid_token" {
		t.Fatalf("invalid token: %+v", ds[0])
	}
	devs := e.MustStatus(e.Do("GET", "/v1/devices", user, nil), 200).Items()
	if devs[0].(map[string]any)["status"] != "invalid" {
		t.Fatalf("device should be invalid: %+v", devs)
	}
	if len(fp.sent) != 1 {
		t.Fatalf("no extra sends expected: %d", len(fp.sent))
	}
	e.MustStatus(e.Do("DELETE", "/v1/devices/"+dev.Str("id"), user, nil), 204)
	e.MustStatus(e.Do("DELETE", "/v1/notifications/schedules/morning", user, nil), 204)
}

// Breaking alerts are policy-gated: sent once for a corroborated, undisputed event; an uncorroborated
// single-portal rumour is withheld even with relaxed source/confidence thresholds.
func TestBreakingAlertsArePolicyGated(t *testing.T) {
	e := testutil.NewEnv(t)
	admin := e.Admin("breaking-admin@example.com", "super_admin")
	user, _, _ := e.RegisterVerified("breaking-user@example.com")
	fp := &fakePush{invalid: map[string]bool{}}
	e.App.NotificationService().SetProvider("apns", fp)
	e.MustStatus(e.Do("POST", "/v1/devices", user, map[string]any{"platform": "ios", "token": "apns-breaking-token-0123456789"}), 201)
	var energy, economy string
	_ = e.App.DB.Pool.QueryRow(e.Ctx, `SELECT (SELECT id::text FROM categories WHERE slug='energy'), (SELECT id::text FROM categories WHERE slug='economy')`).Scan(&energy, &economy)
	e.MustStatus(e.Do("PUT", "/v1/users/me/categories", user, map[string]any{"category_ids": []string{energy, economy}}), 200)

	// Without the feature flag nothing is pushed.
	var version float64
	for _, it := range e.MustStatus(e.Do("GET", "/v1/admin/policies", admin, nil), 200).Items() {
		if m := it.(map[string]any); m["key"] == "notifications" {
			version = m["version"].(float64)
		}
	}
	e.MustStatus(e.Do("PUT", "/v1/admin/policies/notifications", admin, map[string]any{"version": version, "value": map[string]any{
		"daily_budget": 10, "breaking_min_importance": 0, "breaking_min_sources": 1, "breaking_require_no_dispute": true, "breaking_max_age_minutes": 100000, "breaking_min_confidence": 0}}), 200)
	e.MustStatus(e.Do("PUT", "/v1/admin/feature-flags/breaking_notifications", admin, map[string]any{"enabled": true, "rollout_percent": 100}), 204)

	_, off := e.SetupSource(admin, testutil.FixtureSource{Name: "ANRE exemplu", Domain: "anre-exemplu.md", Type: "government", Feed: "anre-exemplu.xml", FetchPages: true, Languages: []string{"ro"}})
	_, ro := e.SetupSource(admin, testutil.FixtureSource{Name: "Știri Exemplu", Domain: "stiri-exemplu.md", Feed: "stiri-exemplu.xml", Languages: []string{"ro"}})
	_, rapid := e.SetupSource(admin, testutil.FixtureSource{Name: "Portal Rapid", Domain: "rapid-portal.md", Feed: "rapid-portal.xml", Languages: []string{"ro"}})
	e.Crawl(admin, off)
	e.Crawl(admin, ro)
	e.Crawl(admin, rapid)

	tariff := eventOf(t, e, "Comunicat: ANRE a aprobat noile tarife la energia electrică")
	pension := ""
	_ = e.App.DB.Pool.QueryRow(e.Ctx, `SELECT o.event_id::text FROM claim_occurrences o JOIN claims c ON c.id=o.claim_id
		WHERE c.canonical_text ILIKE '%reducerea pensiilor cu 30%' AND o.event_id IS NOT NULL LIMIT 1`).Scan(&pension)
	if pension == "" {
		t.Fatal("pension rumour event not found")
	}
	breaking := map[string]int{}
	for _, d := range deliveries(t, e, user) {
		if d["kind"] == "breaking" {
			for _, id := range d["payload"].(map[string]any)["event_ids"].([]any) {
				breaking[id.(string)]++
			}
		}
	}
	if breaking[tariff] != 1 {
		t.Fatalf("expected exactly one breaking alert for the sourced tariff decision, got %v", breaking)
	}
	if breaking[pension] != 0 {
		t.Fatalf("a disputed/under-review rumour must not be pushed as breaking: %v", breaking)
	}
	ok, reason, err := e.App.NotificationService().BreakingEligibility(e.Ctx, e.App.DB.Pool, uuidMust(pension))
	if err != nil || ok || reason != "no_corroborated_claims" {
		t.Fatalf("pension eligibility: ok=%v reason=%q err=%v", ok, reason, err)
	}
	// The ministry's denial arrives: the claim is now contested and held for review.
	_, mf := e.SetupSource(admin, testutil.FixtureSource{Name: "Ministerul Finanțelor exemplu", Domain: "mf-exemplu.md", Type: "government", Feed: "mf-exemplu.xml", FetchPages: true, Languages: []string{"ro"}})
	e.Crawl(admin, mf)
	if ok, reason, _ := e.App.NotificationService().BreakingEligibility(e.Ctx, e.App.DB.Pool, uuidMust(pension)); ok || reason != "disputed_or_under_review" {
		t.Fatalf("after the denial: ok=%v reason=%q", ok, reason)
	}
	sentHigh := false
	for _, m := range fp.sent {
		if m.Data["type"] == "breaking" && m.HighPriority {
			sentHigh = true
		}
	}
	if !sentHigh {
		t.Fatalf("breaking push not delivered with high priority: %+v", fp.sent)
	}
}

func uuidMust(s string) uuid.UUID { return uuid.MustParse(s) }
