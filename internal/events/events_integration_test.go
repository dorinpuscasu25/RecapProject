package events_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/dorinpuscasu25/recapproject/internal/testutil"
)

func eventIDOf(t *testing.T, e *testutil.Env, title string) string {
	t.Helper()
	var id string
	err := e.App.DB.Pool.QueryRow(e.Ctx, `SELECT l.event_id::text FROM event_article_links l JOIN articles a ON a.id=l.article_id
		WHERE a.title=$1 AND l.unlinked_at IS NULL`, title).Scan(&id)
	if err != nil {
		var status, lastErr string
		_ = e.App.DB.Pool.QueryRow(e.Ctx, `SELECT status, coalesce(last_error,'') FROM articles WHERE title=$1`, title).Scan(&status, &lastErr)
		var dl string
		_ = e.App.DB.Pool.QueryRow(e.Ctx, `SELECT coalesce(string_agg(consumer||': '||error, ' | '),'') FROM dead_letters`).Scan(&dl)
		t.Fatalf("article %q not linked to an event (status=%s err=%s dead_letters=%s): %v", title, status, lastErr, dl, err)
	}
	return id
}

const (
	titleRO   = "ANRE a aprobat majorarea tarifului la energia electrică cu 15%"
	titleRU   = "НАРЭ утвердило повышение тарифа на электроэнергию на 15%"
	titleOff  = "Comunicat: ANRE a aprobat noile tarife la energia electrică"
	titleGas  = "ANRE a aprobat reducerea tarifului la gazele naturale cu 8%"
	titleDay2 = "Majorarea tarifului la energia electrică: ce trebuie să știe consumatorii"
)

// Scenario 3 + 4: cross-language grouping, separate decisions stay separate, repeats are not
// material, verified changes create a version and a timeline entry.
func TestEventGroupingNoveltyAndTimeline(t *testing.T) {
	e := testutil.NewEnv(t)
	admin := e.Admin("editor-admin@example.com", "super_admin")
	_, roEP := e.SetupSource(admin, testutil.FixtureSource{Name: "Știri Exemplu", Domain: "stiri-exemplu.md", Feed: "stiri-exemplu.xml", Languages: []string{"ro"}})
	_, ruEP := e.SetupSource(admin, testutil.FixtureSource{Name: "Новости Пример", Domain: "novosti-primer.md", Feed: "novosti-primer.xml", Languages: []string{"ru"}})
	_, offEP := e.SetupSource(admin, testutil.FixtureSource{Name: "ANRE exemplu", Domain: "anre-exemplu.md", Type: "government", Feed: "anre-exemplu.xml", FetchPages: true, Languages: []string{"ro"}})
	e.Crawl(admin, offEP)
	e.Crawl(admin, roEP)
	e.Crawl(admin, ruEP)

	evRO := eventIDOf(t, e, titleRO)
	evRU := eventIDOf(t, e, titleRU)
	evOff := eventIDOf(t, e, titleOff)
	evGas := eventIDOf(t, e, titleGas)
	if evRO != evRU || evRO != evOff {
		t.Fatalf("expected one event for the decision in RO/RU/official: %s %s %s", evRO, evRU, evOff)
	}
	if evGas == evRO {
		t.Fatal("separate gas tariff decision must not merge with the electricity decision")
	}

	user, _, _ := e.RegisterVerified("reader@example.com")
	ev := e.MustStatus(e.Do("GET", "/v1/events/"+evRO+"?lang=ro", user, nil), 200)
	if ev.Get("article_count").(float64) != 3 || ev.Get("source_count").(float64) != 3 {
		t.Fatalf("counts: %s", ev.Raw)
	}
	cites := ev.Get("summary", "citations")
	if cites == nil || len(cites.([]any)) < 2 {
		t.Fatalf("summary must carry citations to several sources: %s", ev.Raw)
	}
	if !strings.Contains(ev.Str("summary", "normal"), "[a") {
		t.Fatalf("summary sentences must carry inline citations: %s", ev.Str("summary", "normal"))
	}
	ru := e.MustStatus(e.Do("GET", "/v1/events/"+evRO+"?lang=ru", user, nil), 200)
	if ru.Str("language") != "ru" || ru.Get("language_fallback") != false {
		t.Fatalf("russian summary expected from the russian source: %s", ru.Raw)
	}
	src := e.MustStatus(e.Do("GET", "/v1/events/"+evRO+"/sources", user, nil), 200)
	if len(src.Items()) != 3 {
		t.Fatalf("sources: %s", src.Raw)
	}
	for _, it := range src.Items() {
		m := it.(map[string]any)
		if m["url"] == "" || m["publisher"] == "" {
			t.Fatalf("every source needs attribution and original link: %v", m)
		}
	}
	version := int(ev.Get("version").(float64))

	// Day 2: repeated facts → no material update (version unchanged, no new timeline entry).
	_, day2EP := e.SetupSource(admin, testutil.FixtureSource{Name: "Ziar Exemplu", Domain: "ziar-exemplu.md", Feed: "ziar-exemplu-day2.xml", Languages: []string{"ro"}})
	e.Crawl(admin, day2EP)
	if got := eventIDOf(t, e, titleDay2); got != evRO {
		t.Fatalf("day-2 explainer should join the event, got %s", got)
	}
	ev2 := e.MustStatus(e.Do("GET", "/v1/events/"+evRO, user, nil), 200)
	if int(ev2.Get("version").(float64)) != version {
		t.Fatalf("repeated facts must not create a new version: %d → %v", version, ev2.Get("version"))
	}
	if ev2.Get("article_count").(float64) != 4 {
		t.Fatalf("coverage count should still grow: %v", ev2.Get("article_count"))
	}
	tl := e.MustStatus(e.Do("GET", "/v1/events/"+evRO+"/timeline", user, nil), 200)
	if len(tl.Items()) != 1 {
		t.Fatalf("timeline should only have the creation entry: %s", tl.Raw)
	}

	// Day 3: revised official figures → material change (new version + timeline entry held for review).
	e.SwitchFeed(admin, offEP, "anre-exemplu-day3.xml")
	e.Crawl(admin, offEP)
	if got := eventIDOf(t, e, "ANRE a revizuit tariful la energia electrică: majorarea va fi de 12%"); got != evRO {
		t.Fatalf("revision should join the same event, got %s", got)
	}
	ev3 := e.MustStatus(e.Do("GET", "/v1/events/"+evRO, user, nil), 200)
	if int(ev3.Get("version").(float64)) != version+1 {
		t.Fatalf("material change must bump version: %v", ev3.Get("version"))
	}
	if ev3.Str("state") != "disputed" {
		t.Fatalf("conflicting figures must mark the event disputed, got %s", ev3.Str("state"))
	}
	adminView := e.MustStatus(e.Do("GET", "/v1/admin/events/"+evRO, admin, nil), 200)
	entries := adminView.Get("timeline").([]any)
	if len(entries) != 2 {
		t.Fatalf("admin timeline: %s", adminView.Raw)
	}
	upd := entries[1].(map[string]any)
	if upd["kind"] != "contradiction" || upd["requires_approval"] != true {
		t.Fatalf("expected a held contradiction update: %v", upd)
	}
	// Reader timeline hides it until approved.
	if n := len(e.MustStatus(e.Do("GET", "/v1/events/"+evRO+"/timeline", user, nil), 200).Items()); n != 1 {
		t.Fatalf("unapproved update visible to readers (%d)", n)
	}
	e.MustStatus(e.Do("POST", fmt.Sprintf("/v1/admin/events/%s/updates/%s/approve", evRO, upd["id"]), admin, nil), 204)
	if n := len(e.MustStatus(e.Do("GET", "/v1/events/"+evRO+"/timeline", user, nil), 200).Items()); n != 2 {
		t.Fatalf("approved update not visible (%d)", n)
	}

	// Search returns cited events (hybrid; lexical falls back to Postgres FTS in tests).
	sr := e.MustStatus(e.Do("GET", "/v1/search?q=tarif+energie+electrica&types=event", user, nil), 200)
	found := false
	for _, it := range sr.Items() {
		if it.(map[string]any)["id"] == evRO {
			found = true
		}
	}
	if !found {
		t.Fatalf("search should find the event: %s", sr.Raw)
	}
	if !strings.Contains(string(sr.Raw), "postgres_fts") {
		t.Fatalf("degraded lexical mode should be reported: %s", sr.Raw)
	}

	// Editorial split + merge (+ unmerge) are reversible.
	var offArticle string
	_ = e.App.DB.Pool.QueryRow(e.Ctx, `SELECT id::text FROM articles WHERE title=$1`, titleOff).Scan(&offArticle)
	sp := e.MustStatus(e.Do("POST", "/v1/admin/events/"+evRO+"/split", admin, map[string]any{"article_ids": []string{offArticle}, "reason": "test split"}), 201)
	newEv := sp.Str("new_event_id")
	if eventIDOf(t, e, titleOff) != newEv {
		t.Fatal("split article must move to the new event")
	}
	m := e.MustStatus(e.Do("POST", "/v1/admin/events/"+evRO+"/merge", admin, map[string]any{"source_event_ids": []string{newEv}, "reason": "undo split"}), 200)
	if eventIDOf(t, e, titleOff) != evRO {
		t.Fatal("merge must move coverage back")
	}
	merged := e.MustStatus(e.Do("GET", "/v1/events/"+newEv, user, nil), 200)
	if merged.Str("status") != "merged" || merged.Str("merged_into") != evRO {
		t.Fatalf("merged event should redirect: %s", merged.Raw)
	}
	e.MustStatus(e.Do("POST", "/v1/admin/events/unmerge/"+m.Str("merge_history_id"), admin, map[string]any{"reason": "test unmerge"}), 204)
	if eventIDOf(t, e, titleOff) != newEv {
		t.Fatal("unmerge must restore the previous grouping")
	}
	// Audit trail exists for editorial operations.
	logs := e.MustStatus(e.Do("GET", "/v1/admin/audit-logs?resource_type=event", admin, nil), 200)
	if len(logs.Items()) < 3 {
		t.Fatalf("expected audit entries for split/merge/unmerge: %s", logs.Raw)
	}
}

// Kafka/bus redelivery must not duplicate articles or events (scenario 12, Postgres driver).
func TestRedeliveryIsIdempotent(t *testing.T) {
	e := testutil.NewEnv(t)
	admin := e.Admin("idem@example.com", "super_admin")
	_, ep := e.SetupSource(admin, testutil.FixtureSource{Name: "Știri Exemplu", Domain: "stiri-exemplu.md", Feed: "stiri-exemplu.xml", Languages: []string{"ro"}})
	e.Crawl(admin, ep)
	count := func(q string) int {
		var n int
		if err := e.App.DB.Pool.QueryRow(e.Ctx, q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	arts, evs := count(`SELECT count(*) FROM articles`), count(`SELECT count(*) FROM events`)
	// Simulate at-least-once redelivery of every message to every consumer.
	if _, err := e.App.DB.Pool.Exec(e.Ctx, `UPDATE message_deliveries SET status='queued', next_attempt_at=now()`); err != nil {
		t.Fatal(err)
	}
	e.Drain()
	// And a second crawl of the same feed.
	e.Crawl(admin, ep)
	if a2, e2 := count(`SELECT count(*) FROM articles`), count(`SELECT count(*) FROM events`); a2 != arts || e2 != evs {
		t.Fatalf("redelivery duplicated state: articles %d→%d events %d→%d", arts, a2, evs, e2)
	}
	if n := count(`SELECT count(*) FROM dead_letters`); n != 0 {
		t.Fatalf("unexpected dead letters: %d", n)
	}
}
