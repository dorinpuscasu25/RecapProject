package app_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/dorinpuscasu25/recapproject/internal/ai"
	"github.com/dorinpuscasu25/recapproject/internal/app"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ratelimit"
	"github.com/dorinpuscasu25/recapproject/internal/testutil"
)

// Admin changes made through one process reach a second process (e.g. the worker) immediately,
// well inside the 30 s cache TTL, through PostgreSQL LISTEN/NOTIFY: no restart or redeploy.
func TestConfigChangesPropagateAcrossProcesses(t *testing.T) {
	e := testutil.NewEnv(t)
	other, err := app.New(e.Ctx, e.App.Cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Options{Limiter: ratelimit.Noop{}})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	admin := e.Admin("prop-admin@example.com", "super_admin")

	// Warm the second process's caches.
	r, err := other.AIGateway().RouteFor(e.Ctx, ai.TaskClassification)
	if err != nil {
		t.Fatal(err)
	}
	before := r.Chain[0].Slug
	var policy struct {
		DailyBudget int `json:"daily_budget"`
	}
	if _, err := other.PolicyStore().Get(e.Ctx, "notifications", &policy); err != nil {
		t.Fatal(err)
	}

	// Change AI routing and a policy through the first process's admin API.
	p := e.MustStatus(e.Do("POST", "/v1/admin/ai/providers", admin, map[string]any{"slug": "alt-rules", "kind": "heuristic", "display_name": "Alt rules", "enabled": true}), 201)
	m := e.MustStatus(e.Do("POST", "/v1/admin/ai/models", admin, map[string]any{"provider_id": p.Str("id"), "slug": "alt-local-rules", "model_name": "local-rules",
		"kind": "chat", "supports_structured": true, "enabled": true}), 201)
	var routeID string
	var routeVersion float64
	for _, it := range e.MustStatus(e.Do("GET", "/v1/admin/ai/routes", admin, nil), 200).Items() {
		if x := it.(map[string]any); x["task"] == "category_classification" && x["mode"] == "production" {
			routeID, routeVersion = x["id"].(string), x["version"].(float64)
		}
	}
	e.MustStatus(e.Do("PATCH", "/v1/admin/ai/routes/"+routeID, admin, map[string]any{"primary_model_id": m.Str("id"), "version": routeVersion}), 200)
	var pv float64
	var current map[string]any
	for _, it := range e.MustStatus(e.Do("GET", "/v1/admin/policies", admin, nil), 200).Items() {
		if x := it.(map[string]any); x["key"] == "notifications" {
			pv = x["version"].(float64)
			raw, _ := json.Marshal(x["value"])
			_ = json.Unmarshal(raw, &current)
		}
	}
	current["daily_budget"] = 2
	e.MustStatus(e.Do("PUT", "/v1/admin/policies/notifications", admin, map[string]any{"version": pv, "value": current}), 200)

	deadline := time.Now().Add(2 * time.Second)
	for {
		r, err := other.AIGateway().RouteFor(e.Ctx, ai.TaskClassification)
		if err != nil {
			t.Fatal(err)
		}
		var pol struct {
			DailyBudget int `json:"daily_budget"`
		}
		_, _ = other.PolicyStore().Get(e.Ctx, "notifications", &pol)
		if r.Chain[0].Slug == "alt-local-rules" && pol.DailyBudget == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("second process still sees route %s (was %s) and daily_budget %d", r.Chain[0].Slug, before, pol.DailyBudget)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
