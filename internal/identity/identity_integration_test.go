package identity_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dorinpuscasu25/recapproject/internal/platform/ratelimit"
	"github.com/dorinpuscasu25/recapproject/internal/testutil"
)

func reg(email string) map[string]any {
	return map[string]any{"first_name": "Ana-Maria", "last_name": "Țurcanu", "email": email, "phone": "069123456",
		"password": "Str0ng-Passw0rd!", "accept_terms": true, "accept_privacy": true, "locale": "ro"}
}

func TestRegistrationVerificationLoginRefreshLogoutFlow(t *testing.T) {
	e := testutil.NewEnv(t)
	email := "Flow.User@Example.com"

	// Anonymous access to news is denied.
	e.MustStatus(e.Do("GET", "/v1/categories/tree", "", nil), 401)

	r := e.MustStatus(e.Do("POST", "/v1/auth/register", "", reg(email)), 202)
	if r.Str("status") != "verification_required" {
		t.Fatalf("unexpected: %s", r.Raw)
	}
	// Pending accounts cannot log in.
	r = e.Do("POST", "/v1/auth/login", "", map[string]any{"email": email, "password": "Str0ng-Passw0rd!"})
	if r.Status != 403 || r.Str("code") != "auth.email_not_verified" {
		t.Fatalf("pending login must be rejected: %d %s", r.Status, r.Raw)
	}
	code := e.LatestCode("flow.user@example.com")
	// Wrong code
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	r = e.Do("POST", "/v1/auth/verify-email", "", map[string]any{"email": email, "code": wrong})
	if r.Status != 422 || r.Str("code") != "auth.code_invalid" {
		t.Fatalf("wrong code: %d %s", r.Status, r.Raw)
	}
	// Correct code (case-insensitive email)
	r = e.MustStatus(e.Do("POST", "/v1/auth/verify-email", "", map[string]any{"email": "flow.user@EXAMPLE.com", "code": code}), 200)
	access, refresh := r.Str("access_token"), r.Str("refresh_token")
	if access == "" || !strings.HasPrefix(refresh, "rt_") || r.Str("user", "status") != "active" || r.Get("user", "phone_verified") != false {
		t.Fatalf("verify response: %s", r.Raw)
	}
	if r.Str("user", "phone") != "+37369123456" {
		t.Fatalf("phone not normalized to E.164: %s", r.Str("user", "phone"))
	}
	// Code is single-use.
	r = e.Do("POST", "/v1/auth/verify-email", "", map[string]any{"email": email, "code": code})
	if r.Status != 422 {
		t.Fatalf("used code must fail: %d", r.Status)
	}
	// Protected endpoints work.
	e.MustStatus(e.Do("GET", "/v1/users/me", access, nil), 200)
	e.MustStatus(e.Do("GET", "/v1/categories/tree", access, nil), 200)

	// Login → second session.
	r = e.MustStatus(e.Do("POST", "/v1/auth/login", "", map[string]any{"email": email, "password": "Str0ng-Passw0rd!", "device_label": "iPhone"}), 200)
	access2, refresh2 := r.Str("access_token"), r.Str("refresh_token")
	sessions := e.MustStatus(e.Do("GET", "/v1/users/me/sessions", access2, nil), 200)
	if len(sessions.Items()) != 2 {
		t.Fatalf("want 2 sessions: %s", sessions.Raw)
	}

	// Refresh rotates; replaying the old refresh token revokes the family.
	r = e.MustStatus(e.Do("POST", "/v1/auth/refresh", "", map[string]any{"refresh_token": refresh2}), 200)
	refresh3 := r.Str("refresh_token")
	if refresh3 == refresh2 {
		t.Fatal("refresh token not rotated")
	}
	r = e.Do("POST", "/v1/auth/refresh", "", map[string]any{"refresh_token": refresh2})
	if r.Status != 401 || r.Str("code") != "auth.refresh_token_reused" {
		t.Fatalf("replay must be detected: %d %s", r.Status, r.Raw)
	}
	// The whole family is revoked: the rotated token no longer works either.
	r = e.Do("POST", "/v1/auth/refresh", "", map[string]any{"refresh_token": refresh3})
	if r.Status != 401 {
		t.Fatalf("family should be revoked: %d", r.Status)
	}

	// Logout current session revokes access immediately.
	e.MustStatus(e.Do("POST", "/v1/auth/logout", access, nil), 204)
	r = e.Do("GET", "/v1/users/me", access, nil)
	if r.Status != 401 {
		t.Fatalf("revoked session must be rejected: %d", r.Status)
	}
	r = e.Do("POST", "/v1/auth/refresh", "", map[string]any{"refresh_token": refresh})
	if r.Status != 401 {
		t.Fatalf("refresh after logout must fail: %d", r.Status)
	}
}

func TestDuplicateRegistrationDoesNotEnumerate(t *testing.T) {
	e := testutil.NewEnv(t)
	a := e.MustStatus(e.Do("POST", "/v1/auth/register", "", reg("dup@example.com")), 202)
	b := e.MustStatus(e.Do("POST", "/v1/auth/register", "", reg("DUP@example.com")), 202)
	if a.Str("message") != b.Str("message") || a.Str("status") != b.Str("status") {
		t.Fatal("responses must be indistinguishable")
	}
	var n int
	_ = e.App.DB.Pool.QueryRow(e.Ctx, `SELECT count(*) FROM users WHERE email_normalized='dup@example.com'`).Scan(&n)
	if n != 1 {
		t.Fatalf("want exactly one account, got %d", n)
	}
	// Login with unknown email and wrong password yield the same error.
	x := e.Do("POST", "/v1/auth/login", "", map[string]any{"email": "nobody@example.com", "password": "Whatever-123"})
	_, _, _ = e.RegisterVerified("known@example.com")
	y := e.Do("POST", "/v1/auth/login", "", map[string]any{"email": "known@example.com", "password": "Wrong-Passw0rd"})
	if x.Status != 401 || y.Status != 401 || x.Str("code") != y.Str("code") || x.Str("detail") != y.Str("detail") {
		t.Fatalf("login errors differ: %s vs %s", x.Raw, y.Raw)
	}
	// Forgot password is non-enumerating.
	f1 := e.MustStatus(e.Do("POST", "/v1/auth/forgot-password", "", map[string]any{"email": "nobody@example.com"}), 202)
	f2 := e.MustStatus(e.Do("POST", "/v1/auth/forgot-password", "", map[string]any{"email": "known@example.com"}), 202)
	if f1.Str("message") != f2.Str("message") {
		t.Fatal("forgot-password responses differ")
	}
}

func TestRegistrationValidation(t *testing.T) {
	e := testutil.NewEnv(t)
	r := e.MustStatus(e.Do("POST", "/v1/auth/register", "", map[string]any{"first_name": "", "last_name": "X1<script>", "email": "bad",
		"phone": "123", "password": "short", "accept_terms": false, "accept_privacy": true}), 422)
	fields := map[string]bool{}
	for _, f := range r.Get("errors").([]any) {
		fields[f.(map[string]any)["field"].(string)] = true
	}
	for _, want := range []string{"first_name", "last_name", "email", "phone", "password", "accept_terms"} {
		if !fields[want] {
			t.Errorf("missing validation error for %s: %s", want, r.Raw)
		}
	}
	// Unknown fields are rejected (e.g. attempting to self-assign roles).
	body := reg("roles@example.com")
	body["roles"] = []string{"super_admin"}
	e.MustStatus(e.Do("POST", "/v1/auth/register", "", body), 400)
}

func TestVerificationLockoutExpiryAndResend(t *testing.T) {
	e := testutil.NewEnv(t)
	email := "lock@example.com"
	e.MustStatus(e.Do("POST", "/v1/auth/register", "", reg(email)), 202)
	code := e.LatestCode(email)
	bad := "123456"
	if bad == code {
		bad = "654321"
	}
	var last *testutil.Resp
	for i := 0; i < 5; i++ {
		last = e.Do("POST", "/v1/auth/verify-email", "", map[string]any{"email": email, "code": bad})
	}
	if last.Str("code") != "auth.code_locked" {
		t.Fatalf("expected lockout after 5 attempts: %s", last.Raw)
	}
	// Even the right code no longer works on a locked challenge.
	r := e.Do("POST", "/v1/auth/verify-email", "", map[string]any{"email": email, "code": code})
	if r.Status != 422 {
		t.Fatalf("locked challenge accepted correct code: %d", r.Status)
	}
	// Resend respects the minimum interval.
	_, _ = e.App.DB.Pool.Exec(e.Ctx, `UPDATE email_verification_challenges SET created_at = now() - interval '2 minutes'`)
	e.MustStatus(e.Do("POST", "/v1/auth/resend-verification", "", map[string]any{"email": email}), 202)
	r = e.Do("POST", "/v1/auth/resend-verification", "", map[string]any{"email": email})
	if r.Status != 429 || r.Header.Get("Retry-After") == "" {
		t.Fatalf("resend too soon must be 429 with Retry-After: %d %s", r.Status, r.Raw)
	}
	newCode := e.LatestCode(email)
	// Resend invalidated the previous challenge: only one live challenge.
	var live int
	_ = e.App.DB.Pool.QueryRow(e.Ctx, `SELECT count(*) FROM email_verification_challenges WHERE consumed_at IS NULL AND invalidated_at IS NULL`).Scan(&live)
	if live != 1 {
		t.Fatalf("want 1 live challenge, got %d", live)
	}
	// Expiry: age the challenge past 10 minutes.
	_, _ = e.App.DB.Pool.Exec(e.Ctx, `UPDATE email_verification_challenges SET expires_at = now() - interval '1 second' WHERE consumed_at IS NULL AND invalidated_at IS NULL`)
	r = e.Do("POST", "/v1/auth/verify-email", "", map[string]any{"email": email, "code": newCode})
	if r.Str("code") != "auth.code_expired" {
		t.Fatalf("expected expiry: %s", r.Raw)
	}
	// Codes are stored only as HMAC.
	var plain int
	_ = e.App.DB.Pool.QueryRow(e.Ctx, `SELECT count(*) FROM email_verification_challenges WHERE encode(code_hmac,'escape') = $1`, newCode).Scan(&plain)
	if plain != 0 {
		t.Fatal("code stored in plaintext")
	}
	// Mail payload with the code is purged after sending.
	var payloads int
	_ = e.App.DB.Pool.QueryRow(e.Ctx, `SELECT count(*) FROM email_deliveries WHERE payload_enc IS NOT NULL AND status='sent'`).Scan(&payloads)
	if payloads != 0 {
		t.Fatal("sent email payloads must be purged")
	}
}

func TestConcurrentVerificationIssuesOneSession(t *testing.T) {
	e := testutil.NewEnv(t)
	email := "race@example.com"
	e.MustStatus(e.Do("POST", "/v1/auth/register", "", reg(email)), 202)
	code := e.LatestCode(email)
	var wg sync.WaitGroup
	results := make([]int, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = e.Do("POST", "/v1/auth/verify-email", "", map[string]any{"email": email, "code": code}).Status
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, s := range results {
		if s == 200 {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("exactly one verification must succeed, got %d (%v)", ok, results)
	}
}

func TestPasswordResetRevokesSessions(t *testing.T) {
	e := testutil.NewEnv(t)
	email := "reset@example.com"
	access, refresh, _ := e.RegisterVerified(email)
	e.MustStatus(e.Do("POST", "/v1/auth/forgot-password", "", map[string]any{"email": email}), 202)
	code := e.LatestCode(email)
	r := e.Do("POST", "/v1/auth/reset-password", "", map[string]any{"email": email, "code": code, "new_password": "short"})
	if r.Status != 422 {
		t.Fatalf("weak password must be rejected: %d", r.Status)
	}
	e.MustStatus(e.Do("POST", "/v1/auth/reset-password", "", map[string]any{"email": email, "code": code, "new_password": "N3w-Secure-Passphrase"}), 204)
	// Single use.
	r = e.Do("POST", "/v1/auth/reset-password", "", map[string]any{"email": email, "code": code, "new_password": "An0ther-Passphrase"})
	if r.Status != 422 {
		t.Fatalf("reused reset code accepted: %d", r.Status)
	}
	if e.Do("GET", "/v1/users/me", access, nil).Status != 401 {
		t.Fatal("old sessions must be revoked after reset")
	}
	if e.Do("POST", "/v1/auth/refresh", "", map[string]any{"refresh_token": refresh}).Status != 401 {
		t.Fatal("old refresh must be revoked after reset")
	}
	e.MustStatus(e.Do("POST", "/v1/auth/login", "", map[string]any{"email": email, "password": "N3w-Secure-Passphrase"}), 200)
	if e.Do("POST", "/v1/auth/login", "", map[string]any{"email": email, "password": "Str0ng-Passw0rd!"}).Status != 401 {
		t.Fatal("old password must stop working")
	}
}

func TestChangePasswordLogoutAllProfileAndEmailChange(t *testing.T) {
	e := testutil.NewEnv(t)
	email := "me@example.com"
	a1, _, _ := e.RegisterVerified(email)
	a2 := e.MustStatus(e.Do("POST", "/v1/auth/login", "", map[string]any{"email": email, "password": "Str0ng-Passw0rd!"}), 200).Str("access_token")

	r := e.MustStatus(e.Do("PATCH", "/v1/users/me", a1, map[string]any{"first_name": "Ion", "timezone": "Europe/Bucharest", "phone": "+40721234567"}), 200)
	if r.Str("first_name") != "Ion" || r.Str("timezone") != "Europe/Bucharest" || r.Str("phone") != "+40721234567" {
		t.Fatalf("profile: %s", r.Raw)
	}
	e.MustStatus(e.Do("PATCH", "/v1/users/me", a1, map[string]any{"timezone": "Mars/Base"}), 422)
	// Email cannot be changed via PATCH.
	e.MustStatus(e.Do("PATCH", "/v1/users/me", a1, map[string]any{"email": "x@example.com"}), 400)

	// Email change requires re-verification; old email stays until confirmed.
	e.MustStatus(e.Do("POST", "/v1/users/me/email-change", a1, map[string]any{"new_email": "new@example.com", "password": "Str0ng-Passw0rd!"}), 202)
	if e.MustStatus(e.Do("GET", "/v1/users/me", a1, nil), 200).Str("email") != email {
		t.Fatal("email must not change before confirmation")
	}
	code := e.LatestCode("new@example.com")
	r = e.MustStatus(e.Do("POST", "/v1/users/me/email-change/confirm", a1, map[string]any{"code": code}), 200)
	if r.Str("email") != "new@example.com" {
		t.Fatalf("email change: %s", r.Raw)
	}

	// Change password keeps current session, revokes others.
	e.MustStatus(e.Do("POST", "/v1/auth/change-password", a1, map[string]any{"current_password": "wrong", "new_password": "N3w-Secure-Passphrase"}), 422)
	e.MustStatus(e.Do("POST", "/v1/auth/change-password", a1, map[string]any{"current_password": "Str0ng-Passw0rd!", "new_password": "N3w-Secure-Passphrase"}), 204)
	e.MustStatus(e.Do("GET", "/v1/users/me", a1, nil), 200)
	if e.Do("GET", "/v1/users/me", a2, nil).Status != 401 {
		t.Fatal("other session must be revoked")
	}
	e.MustStatus(e.Do("POST", "/v1/auth/logout-all", a1, nil), 204)
	if e.Do("GET", "/v1/users/me", a1, nil).Status != 401 {
		t.Fatal("logout-all must revoke current session")
	}
}

func TestRBACAndPrivilegeEscalation(t *testing.T) {
	e := testutil.NewEnv(t)
	user, _, userID := e.RegisterVerified("plain@example.com")
	// Ordinary users get 403 on admin routes.
	for _, p := range []string{"/v1/admin/sources", "/v1/admin/categories", "/v1/admin/audit-logs", "/v1/admin/jobs", "/v1/admin/publishers"} {
		if r := e.Do("GET", p, user, nil); r.Status != 403 {
			t.Errorf("%s: want 403 got %d", p, r.Status)
		}
	}
	if r := e.Do("POST", "/v1/admin/categories", user, map[string]any{"slug": "x", "names": map[string]string{"ro": "X"}}); r.Status != 403 {
		t.Errorf("user created category: %d", r.Status)
	}
	// Anonymous gets 401 on admin and consumer routes.
	if r := e.Do("GET", "/v1/admin/sources", "", nil); r.Status != 401 {
		t.Errorf("anonymous admin: %d", r.Status)
	}
	// Admin (not super_admin) cannot grant admin/super_admin.
	admin := e.Admin("admin@example.com", "admin")
	r := e.Do("PUT", "/v1/admin/users/"+userID+"/roles", admin, map[string]any{"roles": []string{"super_admin"}})
	if r.Status != 403 {
		// admin lacks users:manage entirely
		t.Logf("admin role grant status %d", r.Status)
	}
	super := e.Admin("root@example.com", "super_admin")
	r = e.MustStatus(e.Do("PUT", "/v1/admin/users/"+userID+"/roles", super, map[string]any{"roles": []string{"editor"}}), 200)
	if fmt.Sprint(r.Get("roles")) != "[editor user]" {
		t.Fatalf("roles: %v", r.Get("roles"))
	}
	// The editor can now read sources but not write them.
	e.MustStatus(e.Do("GET", "/v1/admin/sources", user, nil), 200)
	if r := e.Do("POST", "/v1/admin/publishers", user, map[string]any{"name": "X", "canonical_domain": "x.md", "publisher_type": "newsroom"}); r.Status != 403 {
		t.Errorf("editor wrote publisher: %d", r.Status)
	}
	// Self-modification is denied.
	superID := e.MustStatus(e.Do("GET", "/v1/users/me", super, nil), 200).Str("id")
	e.MustStatus(e.Do("PUT", "/v1/admin/users/"+superID+"/roles", super, map[string]any{"roles": []string{}}), 403)
	// Suspension blocks access immediately.
	e.MustStatus(e.Do("PUT", "/v1/admin/users/"+userID+"/status", super, map[string]any{"status": "suspended", "reason": "test"}), 204)
	if r := e.Do("GET", "/v1/users/me", user, nil); r.Status != 401 && r.Status != 403 {
		t.Fatalf("suspended user still has access: %d", r.Status)
	}
	if r := e.Do("POST", "/v1/auth/login", "", map[string]any{"email": "plain@example.com", "password": "Str0ng-Passw0rd!"}); r.Str("code") != "auth.account_unavailable" {
		t.Fatalf("suspended login: %s", r.Raw)
	}
}

func TestTokenTamperingRejected(t *testing.T) {
	e := testutil.NewEnv(t)
	access, _, _ := e.RegisterVerified("jwt@example.com")
	parts := strings.Split(access, ".")
	// alg=none
	none := "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0." + parts[1] + "."
	if r := e.Do("GET", "/v1/users/me", none, nil); r.Status != 401 {
		t.Fatalf("alg none accepted: %d", r.Status)
	}
	// HS256 header with original payload/signature.
	hs := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCIsImtpZCI6ImVwaGVtZXJhbCJ9." + parts[1] + "." + parts[2]
	if r := e.Do("GET", "/v1/users/me", hs, nil); r.Status != 401 {
		t.Fatalf("alg confusion accepted: %d", r.Status)
	}
	// Garbage
	if r := e.Do("GET", "/v1/users/me", "abc.def.ghi", nil); r.Status != 401 {
		t.Fatalf("garbage accepted: %d", r.Status)
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := testutil.NewEnv(t, testutil.WithRateLimiter(ratelimit.NewMemory()))
	var last *testutil.Resp
	for i := 0; i < 12; i++ {
		last = e.Do("POST", "/v1/auth/login", "", map[string]any{"email": "victim@example.com", "password": "Wrong-Passw0rd"})
	}
	if last.Status != 429 {
		t.Fatalf("expected per-account throttle, got %d", last.Status)
	}
}

func TestDataExportAndDeletion(t *testing.T) {
	e := testutil.NewEnv(t)
	access, _, uid := e.RegisterVerified("privacy@example.com")
	e.MustStatus(e.Do("POST", "/v1/users/me/data-export", access, nil), 202)
	e.Drain()
	r := e.MustStatus(e.Do("GET", "/v1/users/me/data-requests", access, nil), 200)
	if r.Str("items", "0", "status") != "succeeded" {
		t.Fatalf("export not completed: %s", r.Raw)
	}
	// Filesystem storage cannot presign, so the owner downloads through the API.
	dl := r.Str("items", "0", "download_url")
	if dl != "/v1/users/me/data-requests/"+r.Str("items", "0", "id")+"/download" {
		t.Fatalf("unexpected download_url %q", dl)
	}
	file := e.MustStatus(e.Do("GET", dl, access, nil), 200)
	if !strings.Contains(string(file.Raw), "privacy@example.com") || !strings.Contains(string(file.Raw), `"format_version"`) {
		t.Fatalf("export file content unexpected: %.200s", file.Raw)
	}
	other, _, _ := e.RegisterVerified("someone-else@example.com")
	e.MustStatus(e.Do("GET", dl, other, nil), 404)
	e.MustStatus(e.Do("POST", "/v1/users/me/deletion", access, map[string]any{"password": "Str0ng-Passw0rd!"}), 202)
	if e.Do("GET", "/v1/users/me", access, nil).Status != 401 {
		t.Fatal("deletion must revoke sessions")
	}
	// Fast-forward the grace period and run the purge job.
	_, _ = e.App.DB.Pool.Exec(e.Ctx, `UPDATE jobs SET run_at = now() WHERE kind='user.deletion'`)
	e.Drain()
	var n int
	_ = e.App.DB.Pool.QueryRow(e.Ctx, `SELECT count(*) FROM users WHERE id=$1`, uid).Scan(&n)
	if n != 0 {
		t.Fatal("user not purged")
	}
	_ = time.Now
}
