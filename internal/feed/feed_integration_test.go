package feed_test

import (
	"testing"

	"github.com/dorinpuscasu25/recapproject/internal/testutil"
)

func categoryID(t *testing.T, e *testutil.Env, slug string) string {
	t.Helper()
	var id string
	if err := e.App.DB.Pool.QueryRow(e.Ctx, `SELECT id::text FROM categories WHERE slug=$1`, slug).Scan(&id); err != nil {
		t.Fatalf("category %s: %v", slug, err)
	}
	return id
}

func titles(r *testutil.Resp) map[string]bool {
	out := map[string]bool{}
	for _, it := range r.Items() {
		m := it.(map[string]any)["event"].(map[string]any)
		out[m["title"].(string)] = true
	}
	return out
}

const (
	techTitle = "Un nou centru de tehnologie și startup-uri a fost inaugurat la Chișinău"
	agriTitle = "Fermierii din sudul țării au început recoltarea strugurilor"
)

// Scenario 9: different interests → different feeds; catch-up uses the acknowledged cursor.
func TestPersonalizedFeedAndCatchUp(t *testing.T) {
	e := testutil.NewEnv(t)
	admin := e.Admin("feed-admin@example.com", "super_admin")
	_, ep := e.SetupSource(admin, testutil.FixtureSource{Name: "Știri Diverse Exemplu", Domain: "diverse-exemplu.md", Feed: "diverse-ro.xml", Languages: []string{"ro"}})
	e.Crawl(admin, ep)

	alice, _, _ := e.RegisterVerified("alice@example.com")
	bob, _, _ := e.RegisterVerified("bob@example.com")
	// Cold start: editorial defaults (importance/recency), not empty.
	cold := e.MustStatus(e.Do("GET", "/v1/feed", alice, nil), 200)
	if cold.Get("cold_start") != true || len(cold.Items()) == 0 {
		t.Fatalf("cold-start feed: %s", cold.Raw)
	}
	// Onboarding: pick categories from the active taxonomy.
	tree := e.MustStatus(e.Do("GET", "/v1/categories/tree", alice, nil), 200)
	if len(tree.Items()) < 10 {
		t.Fatalf("taxonomy tree: %s", tree.Raw)
	}
	e.MustStatus(e.Do("PUT", "/v1/users/me/categories", alice, map[string]any{"category_ids": []string{categoryID(t, e, "technology")}}), 200)
	e.MustStatus(e.Do("POST", "/v1/users/me/interests", alice, map[string]any{"interest_type": "region", "target": "Chișinău", "mode": "follow"}), 201)
	e.MustStatus(e.Do("PUT", "/v1/users/me/categories", bob, map[string]any{"category_ids": []string{categoryID(t, e, "agriculture")}}), 200)
	// Users cannot create categories or follow unknown ones.
	e.MustStatus(e.Do("PUT", "/v1/users/me/categories", bob, map[string]any{"category_ids": []string{"00000000-0000-0000-0000-000000000001"}}), 422)
	prefs := e.MustStatus(e.Do("GET", "/v1/users/me/preferences", alice, nil), 200)
	if prefs.Get("onboarding_completed_at") == nil {
		t.Fatal("onboarding should be marked complete")
	}

	fa := titles(e.MustStatus(e.Do("GET", "/v1/feed", alice, nil), 200))
	fb := titles(e.MustStatus(e.Do("GET", "/v1/feed", bob, nil), 200))
	if !fa[techTitle] || fa[agriTitle] {
		t.Fatalf("alice feed should contain tech, not agriculture: %v", fa)
	}
	if !fb[agriTitle] || fb[techTitle] {
		t.Fatalf("bob feed should contain agriculture, not tech: %v", fb)
	}

	// Catch-up relative to acknowledgement; seen events at the latest version are not repeated.
	cu := e.MustStatus(e.Do("GET", "/v1/catch-up", alice, nil), 200)
	if cu.Str("since_source") != "default_24h" || len(cu.Items()) == 0 {
		t.Fatalf("initial catch-up: %s", cu.Raw)
	}
	var techEvent string
	for _, it := range cu.Items() {
		ev := it.(map[string]any)["event"].(map[string]any)
		if ev["title"] == techTitle {
			techEvent = ev["id"].(string)
		}
	}
	if techEvent == "" {
		t.Fatalf("tech event missing from catch-up: %s", cu.Raw)
	}
	e.MustStatus(e.Do("POST", "/v1/events/"+techEvent+"/seen", alice, nil), 204)
	e.MustStatus(e.Do("POST", "/v1/catch-up/acknowledge", alice, nil), 200)
	cu = e.MustStatus(e.Do("GET", "/v1/catch-up", alice, nil), 200)
	if cu.Str("since_source") != "acknowledged" || len(cu.Items()) != 0 {
		t.Fatalf("nothing new after acknowledgement: %s", cu.Raw)
	}
	// A material update brings the seen event back, with what changed.
	e.SwitchFeed(admin, ep, "diverse-ro-day2.xml")
	e.Crawl(admin, ep)
	cu = e.MustStatus(e.Do("GET", "/v1/catch-up", alice, nil), 200)
	if len(cu.Items()) != 1 {
		t.Fatalf("expected the updated tech event: %s", cu.Raw)
	}
	item := cu.Items()[0].(map[string]any)
	if item["previously_seen"] != true || len(item["changes_since"].([]any)) == 0 {
		t.Fatalf("update should be marked and explained: %v", item)
	}
	if cb := e.MustStatus(e.Do("GET", "/v1/catch-up", bob, nil), 200); len(cb.Items()) == 0 {
		t.Log("bob has no acknowledged cursor; default window applies")
	}
	cbItems := titles(e.MustStatus(e.Do("GET", "/v1/catch-up", bob, nil), 200))
	if cbItems[techTitle] {
		t.Fatal("bob must not get tech updates")
	}
	// Follow + bookmark + mute.
	e.MustStatus(e.Do("PUT", "/v1/events/"+techEvent+"/bookmark", alice, nil), 204)
	if bm := e.MustStatus(e.Do("GET", "/v1/users/me/bookmarks", alice, nil), 200); len(bm.Items()) != 1 {
		t.Fatalf("bookmarks: %s", bm.Raw)
	}
	e.MustStatus(e.Do("POST", "/v1/users/me/interests", alice, map[string]any{"interest_type": "keyword", "target": "startup", "mode": "mute"}), 201)
	if fa := titles(e.MustStatus(e.Do("GET", "/v1/feed", alice, nil), 200)); fa[techTitle] {
		t.Fatal("muted keyword must remove the event from the feed")
	}
	// Pagination is stable.
	p1 := e.MustStatus(e.Do("GET", "/v1/feed?limit=1", bob, nil), 200)
	if c := p1.Str("next_cursor"); c != "" {
		p2 := e.MustStatus(e.Do("GET", "/v1/feed?limit=1&cursor="+c, bob, nil), 200)
		if len(p2.Items()) > 0 && p2.Str("items", "0", "event", "id") == p1.Str("items", "0", "event", "id") {
			t.Fatal("pagination repeated an item")
		}
	}
	e.MustStatus(e.Do("GET", "/v1/feed?cursor=garbage", bob, nil), 400)

	// Usage analytics: pseudonymous subject, route pattern only (no ids or query text).
	e.App.Analytics.Close()
	var n int
	var leaked bool
	_ = e.App.DB.Pool.QueryRow(e.Ctx, `SELECT count(*) FILTER (WHERE properties->>'route'='/v1/feed'),
		bool_or(properties::text LIKE '%'||$1||'%' OR subject_hash IN (SELECT id::text FROM users)) FROM analytics_events WHERE event_name='api_request'`, techEvent).Scan(&n, &leaked)
	if n == 0 || leaked {
		t.Fatalf("analytics: feed requests=%d leaked identifiers=%v", n, leaked)
	}
}
