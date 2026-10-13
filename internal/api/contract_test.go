package api_test

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/dorinpuscasu25/recapproject/internal/apispec"
	"github.com/dorinpuscasu25/recapproject/internal/testutil"
)

// The committed contract must equal the generated one (run `go run ./cmd/openapi-gen`).
func TestOpenAPIUpToDate(t *testing.T) {
	want, err := apispec.YAML()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(testutil.RepoRoot(), "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("api/openapi.yaml is stale: run `go run ./cmd/openapi-gen`")
	}
}

var param = regexp.MustCompile(`\{[A-Za-z]+\}`)

// Every mounted route is documented, every documented operation is mounted, and every documented
// permission is the one the middleware actually enforces.
func TestRoutesMatchContractAndPermissions(t *testing.T) {
	e := testutil.NewEnv(t)
	mounted := map[string]bool{}
	_ = chi.Walk(e.App.Handler().(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		mounted[method+" "+strings.TrimSuffix(route, "/")] = true
		return nil
	})
	documented := map[string]apispec.Op{}
	for _, op := range apispec.Operations() {
		k := op.Method + " " + op.Path
		if _, dup := documented[k]; dup {
			t.Errorf("duplicate operation %s", k)
		}
		documented[k] = op
	}
	var missing, extra []string
	for k := range mounted {
		if _, ok := documented[k]; !ok {
			missing = append(missing, k)
		}
	}
	for k := range documented {
		if !mounted[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("contract mismatch\nundocumented routes: %v\ndocumented but not mounted: %v", missing, extra)
	}

	user, _, _ := e.RegisterVerified("contract-user@example.com")
	checker := e.Admin("contract-checker@example.com", "fact_checker") // has sources:read, factcheck:review
	for _, op := range apispec.Operations() {
		path := param.ReplaceAllString(op.Path, "00000000-0000-7000-8000-000000000001")
		var body any
		if op.Req != nil {
			body = map[string]any{}
		}
		switch op.Access {
		case apispec.Self, apispec.Consumer:
			if r := e.Do(op.Method, path, "", body); r.Status != 401 {
				t.Errorf("%s %s anonymous: got %d, want 401", op.Method, op.Path, r.Status)
			}
		case apispec.Staff:
			if r := e.Do(op.Method, path, "", body); r.Status != 401 {
				t.Errorf("%s %s anonymous: got %d, want 401", op.Method, op.Path, r.Status)
			}
			r := e.Do(op.Method, path, user, body)
			if r.Status != 403 || r.Get("details", "required_permission") != op.Perms[0] {
				t.Errorf("%s %s as user: got %d %v, want 403 requiring %s", op.Method, op.Path, r.Status, r.Get("details", "required_permission"), op.Perms[0])
			}
			// A role holding the outer permission but not the inner one is stopped at the inner check.
			if len(op.Perms) == 2 && op.Perms[0] == "sources:read" {
				r := e.Do(op.Method, path, checker, body)
				if r.Status != 403 || r.Get("details", "required_permission") != op.Perms[1] {
					t.Errorf("%s %s as fact_checker: got %d %v, want 403 requiring %s", op.Method, op.Path, r.Status, r.Get("details", "required_permission"), op.Perms[1])
				}
			}
		}
	}
}
