// Package identity implements first-party registration, email OTP verification, login, rotating
// refresh sessions, password recovery, profile management and RBAC principals.
package identity

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dorinpuscasu25/recapproject/internal/audit"
	"github.com/dorinpuscasu25/recapproject/internal/mail"
	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/config"
	"github.com/dorinpuscasu25/recapproject/internal/platform/crypto"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ids"
	"github.com/dorinpuscasu25/recapproject/internal/platform/principal"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ratelimit"
)

type Service struct {
	db      *db.DB
	cfg     config.AuthConfig
	tokens  *TokenIssuer
	sealer  *crypto.Sealer
	limiter ratelimit.Limiter
	log     *slog.Logger
	argon   Argon2Params
	now     func() time.Time

	permMu    sync.RWMutex
	permCache map[string][]string // role -> permissions
	permAt    time.Time

	dummyHash string // used to equalize timing for unknown accounts
}

func NewService(d *db.DB, cfg config.AuthConfig, sealer *crypto.Sealer, limiter ratelimit.Limiter, log *slog.Logger) *Service {
	argon := Argon2Params{MemoryKiB: cfg.Argon2MemoryKiB, Iterations: cfg.Argon2Iterations, Parallelism: cfg.Argon2Parallelism}
	s := &Service{
		db: d, cfg: cfg, sealer: sealer, limiter: limiter, log: log, argon: argon, now: time.Now,
		tokens: NewTokenIssuer(cfg.Issuer, cfg.Audience, cfg.AccessTTL, cfg.SigningKeys, cfg.ActiveKeyID),
	}
	s.dummyHash = HashPassword(crypto.RandomToken(16), argon)
	return s
}

func (s *Service) Tokens() *TokenIssuer { return s.tokens }

// ---------- Types ----------

type User struct {
	ID              uuid.UUID  `json:"id"`
	FirstName       string     `json:"first_name"`
	LastName        string     `json:"last_name"`
	Email           string     `json:"email"`
	EmailVerifiedAt *time.Time `json:"email_verified_at,omitempty"`
	Phone           string     `json:"phone"`
	PhoneVerified   bool       `json:"phone_verified"`
	Status          string     `json:"status"`
	Locale          string     `json:"locale"`
	Timezone        string     `json:"timezone"`
	MarketingOptIn  bool       `json:"marketing_opt_in"`
	Roles           []string   `json:"roles"`
	CreatedAt       time.Time  `json:"created_at"`
	Version         int        `json:"version"`

	passwordHash string
	emailNorm    string
}

type RegisterInput struct {
	FirstName       string `json:"first_name"`
	LastName        string `json:"last_name"`
	Email           string `json:"email"`
	Phone           string `json:"phone"`
	Password        string `json:"password"`
	Locale          string `json:"locale,omitempty"`
	Timezone        string `json:"timezone,omitempty"`
	AcceptTerms     bool   `json:"accept_terms"`
	AcceptPrivacy   bool   `json:"accept_privacy"`
	MarketingOptIn  bool   `json:"marketing_opt_in"`
	TermsVersion    string `json:"terms_version,omitempty"`
}

type ClientInfo struct {
	IP          string
	IPPrefix    string
	UserAgent   string
	DeviceLabel string
}

type TokenPair struct {
	AccessToken      string    `json:"access_token"`
	TokenType        string    `json:"token_type"`
	ExpiresIn        int       `json:"expires_in"`
	AccessExpiresAt  time.Time `json:"access_expires_at"`
	RefreshToken     string    `json:"refresh_token"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
	SessionID        uuid.UUID `json:"session_id"`
}

// RegistrationAccepted is returned for every syntactically valid registration, whether or not
// the address is already registered (prevents account enumeration).
type RegistrationAccepted struct {
	Status        string `json:"status"`
	Email         string `json:"email"`
	CodeExpiresIn int    `json:"code_expires_in"`
	Message       string `json:"message"`
}

// ---------- Rate limiting ----------

func (s *Service) limit(ctx context.Context, key string, n int, win time.Duration) error {
	ok, retry, err := s.limiter.Allow(ctx, key, n, win)
	if err != nil {
		s.log.Warn("rate limiter unavailable; failing open", "error", err)
		return nil
	}
	if !ok {
		return apperr.RateLimited(retry)
	}
	return nil
}

// ---------- Registration ----------

func (s *Service) Register(ctx context.Context, in RegisterInput, ci ClientInfo) (*RegistrationAccepted, error) {
	if err := s.limit(ctx, "register:ip:"+ci.IP, 10, time.Hour); err != nil {
		return nil, err
	}
	var v apperr.Validator
	first, okF := validName(in.FirstName)
	v.Check(okF, "first_name", "invalid", "First name is required (1-100 letters).")
	last, okL := validName(in.LastName)
	v.Check(okL, "last_name", "invalid", "Last name is required (1-100 letters).")
	email, emailNorm, okE := NormalizeEmail(in.Email)
	v.Check(okE, "email", "invalid", "A valid email address is required.")
	phone, okP := NormalizePhone(in.Phone, "MD")
	v.Check(okP, "phone", "invalid", "A valid phone number in international format is required (e.g. +37369123456).")
	if code, msg, ok := ValidatePassword(in.Password, email); !ok {
		v.Add("password", code, msg)
	}
	locale := in.Locale
	if locale == "" {
		locale = "ro"
	}
	v.Check(validLocale(locale), "locale", "invalid", "Locale must be one of ro, ru, en.")
	tz := in.Timezone
	if tz == "" {
		tz = "Europe/Chisinau"
	}
	v.Check(validTimezone(tz), "timezone", "invalid", "Timezone must be a valid IANA zone.")
	v.Check(in.AcceptTerms, "accept_terms", "required", "You must accept the terms of service.")
	v.Check(in.AcceptPrivacy, "accept_privacy", "required", "You must accept the privacy policy.")
	if in.TermsVersion != "" && in.TermsVersion != s.cfg.TermsPolicyVersion {
		v.Add("terms_version", "outdated", "The accepted policy version is not current.")
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	resp := &RegistrationAccepted{
		Status: "verification_required", Email: email, CodeExpiresIn: int(s.cfg.CodeTTL.Seconds()),
		Message: "If this address can be used, a verification code has been sent to it.",
	}

	// Hash before touching the DB so timing does not depend on whether the account exists.
	hash := HashPassword(in.Password, s.argon)
	now := s.now().UTC()

	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		var existingID uuid.UUID
		var existingLocale string
		err := tx.QueryRow(ctx, `SELECT id, locale FROM users WHERE email_normalized=$1`, emailNorm).Scan(&existingID, &existingLocale)
		if err == nil {
			// Existing account: notify the owner (throttled by idempotency key per hour), change nothing.
			s.securityEvent(ctx, tx, &existingID, "registration_attempt_existing", ci, nil)
			return mail.Enqueue(ctx, tx, s.sealer, mail.Email{
				UserID: &existingID, To: email, Template: "account_exists", Locale: existingLocale,
				IdempotencyKey: fmt.Sprintf("account_exists:%s:%s", existingID, now.Format("2006010215")),
			})
		}
		if !db.IsNoRows(err) {
			return err
		}
		userID := ids.New()
		_, err = tx.Exec(ctx, `INSERT INTO users (id, first_name, last_name, email, email_normalized, phone_e164, password_hash,
			status, locale, timezone, marketing_opt_in) VALUES ($1,$2,$3,$4,$5,$6,$7,'pending_verification',$8,$9,$10)`,
			userID, first, last, email, emailNorm, phone, hash, locale, tz, in.MarketingOptIn)
		if db.IsUniqueViolation(err) {
			return nil // concurrent registration of the same email: same generic response
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO user_roles (user_id, role_code) VALUES ($1,'user')`, userID); err != nil {
			return err
		}
		for _, doc := range []struct {
			name     string
			accepted bool
		}{{"terms", true}, {"privacy", true}, {"marketing", in.MarketingOptIn}} {
			if _, err := tx.Exec(ctx, `INSERT INTO user_terms_acceptances (id, user_id, document, policy_version, accepted)
				VALUES ($1,$2,$3,$4,$5)`, ids.New(), userID, doc.name, s.cfg.TermsPolicyVersion, doc.accepted); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO user_preferences (user_id, languages, summary_language) VALUES ($1, $2, $3)`,
			userID, []string{locale}, locale); err != nil {
			return err
		}
		if err := s.issueEmailChallenge(ctx, tx, userID, "signup", email, emailNorm, first, locale); err != nil {
			return err
		}
		s.securityEvent(ctx, tx, &userID, "registered", ci, nil)
		return audit.Record(principal.With(ctx, &principal.Principal{Kind: principal.KindUser, ID: userID, IPPrefix: ci.IPPrefix}), tx,
			audit.Entry{Action: "user.register", ResourceType: "user", ResourceID: userID.String()})
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// issueEmailChallenge invalidates any live challenge for (user, purpose), creates a new one and
// enqueues the code email — all in the caller's transaction.
func (s *Service) issueEmailChallenge(ctx context.Context, tx pgx.Tx, userID uuid.UUID, purpose, email, emailNorm, firstName, locale string) error {
	if _, err := tx.Exec(ctx, `UPDATE email_verification_challenges SET invalidated_at=now(), invalidated_reason='superseded'
		WHERE user_id=$1 AND purpose=$2 AND consumed_at IS NULL AND invalidated_at IS NULL`, userID, purpose); err != nil {
		return err
	}
	chID := ids.New()
	code := crypto.SixDigitCode()
	expires := s.now().UTC().Add(s.cfg.CodeTTL)
	mac := crypto.HMAC(s.cfg.CodeHMACSecret, "email:"+purpose, chID.String(), userID.String(), code)
	if _, err := tx.Exec(ctx, `INSERT INTO email_verification_challenges
		(id, user_id, purpose, target_email, target_email_normalized, code_hmac, max_attempts, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, chID, userID, purpose, email, emailNorm, mac, s.cfg.CodeMaxAttempts, expires); err != nil {
		return err
	}
	tmpl := "verification_code"
	if purpose == "email_change" {
		tmpl = "email_change_code"
	}
	return mail.Enqueue(ctx, tx, s.sealer, mail.Email{
		UserID: &userID, To: email, Template: tmpl, Locale: locale, ExpiresAt: &expires,
		IdempotencyKey: "challenge:" + chID.String(),
		Data:           map[string]string{"code": code, "first_name": firstName, "minutes": fmt.Sprint(int(s.cfg.CodeTTL.Minutes()))},
	})
}

// ---------- Resend ----------

type ResendInput struct {
	Email string `json:"email"`
}

func (s *Service) ResendVerification(ctx context.Context, in ResendInput, ci ClientInfo) error {
	if err := s.limit(ctx, "resend:ip:"+ci.IP, 20, time.Hour); err != nil {
		return err
	}
	_, norm, ok := NormalizeEmail(in.Email)
	if !ok {
		return apperr.Validation(apperr.FieldError{Field: "email", Code: "invalid", Message: "A valid email address is required."})
	}
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		var userID uuid.UUID
		var status, email, first, locale string
		err := tx.QueryRow(ctx, `SELECT id, status, email, first_name, locale FROM users WHERE email_normalized=$1 FOR UPDATE`, norm).
			Scan(&userID, &status, &email, &first, &locale)
		if db.IsNoRows(err) || (err == nil && status != "pending_verification") {
			return nil // non-enumerating: same response
		}
		if err != nil {
			return err
		}
		var lastAt *time.Time
		var lastHour int
		if err := tx.QueryRow(ctx, `SELECT max(created_at), count(*) FILTER (WHERE created_at > now() - interval '1 hour')
			FROM email_verification_challenges WHERE user_id=$1 AND purpose='signup'`, userID).Scan(&lastAt, &lastHour); err != nil {
			return err
		}
		if lastAt != nil {
			if wait := s.cfg.ResendMinInterval - s.now().Sub(*lastAt); wait > 0 {
				return apperr.RateLimited(wait).WithDetail("reason", "resend_too_soon")
			}
		}
		if lastHour >= s.cfg.ResendHourlyCap {
			return apperr.RateLimited(time.Hour).WithDetail("reason", "hourly_cap")
		}
		if err := s.issueEmailChallenge(ctx, tx, userID, "signup", email, norm, first, locale); err != nil {
			return err
		}
		s.securityEvent(ctx, tx, &userID, "verification_resent", ci, nil)
		return nil
	})
}

// ---------- Verify email ----------

type VerifyInput struct {
	Email       string `json:"email"`
	Code        string `json:"code"`
	DeviceLabel string `json:"device_label,omitempty"`
}

var errCodeInvalid = apperr.ValidationMsg("auth.code_invalid", "The verification code is invalid or has already been used.")

func (s *Service) VerifyEmail(ctx context.Context, in VerifyInput, ci ClientInfo) (*TokenPair, *User, error) {
	if err := s.limit(ctx, "verify:ip:"+ci.IP, 60, 15*time.Minute); err != nil {
		return nil, nil, err
	}
	_, norm, ok := NormalizeEmail(in.Email)
	if !ok || len(in.Code) != 6 || strings.Trim(in.Code, "0123456789") != "" {
		return nil, nil, errCodeInvalid
	}
	var pair *TokenPair
	var userID uuid.UUID
	var outcome error
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		var chID uuid.UUID
		var mac []byte
		var attempts, maxAttempts int
		var expires time.Time
		var status string
		err := tx.QueryRow(ctx, `SELECT c.id, c.user_id, c.code_hmac, c.attempts, c.max_attempts, c.expires_at, u.status
			FROM users u JOIN email_verification_challenges c ON c.user_id = u.id AND c.purpose='signup'
			WHERE u.email_normalized=$1 AND c.consumed_at IS NULL AND c.invalidated_at IS NULL
			FOR UPDATE OF c, u`, norm).Scan(&chID, &userID, &mac, &attempts, &maxAttempts, &expires, &status)
		if db.IsNoRows(err) {
			outcome = errCodeInvalid
			return nil
		}
		if err != nil {
			return err
		}
		if status != "pending_verification" {
			outcome = errCodeInvalid
			return nil
		}
		if s.now().After(expires) {
			_, _ = tx.Exec(ctx, `UPDATE email_verification_challenges SET invalidated_at=now(), invalidated_reason='expired' WHERE id=$1`, chID)
			outcome = apperr.ValidationMsg("auth.code_expired", "The verification code has expired. Request a new one.")
			return nil
		}
		if attempts >= maxAttempts {
			outcome = apperr.ValidationMsg("auth.code_locked", "Too many incorrect attempts. Request a new code.")
			return nil
		}
		want := crypto.HMAC(s.cfg.CodeHMACSecret, "email:signup", chID.String(), userID.String(), in.Code)
		if !crypto.Equal(want, mac) {
			attempts++
			if attempts >= maxAttempts {
				_, err = tx.Exec(ctx, `UPDATE email_verification_challenges SET attempts=$2, invalidated_at=now(), invalidated_reason='max_attempts' WHERE id=$1`, chID, attempts)
				s.securityEvent(ctx, tx, &userID, "verification_locked", ci, nil)
				outcome = apperr.ValidationMsg("auth.code_locked", "Too many incorrect attempts. Request a new code.")
			} else {
				_, err = tx.Exec(ctx, `UPDATE email_verification_challenges SET attempts=$2 WHERE id=$1`, chID, attempts)
				outcome = errCodeInvalid.WithDetail("attempts_remaining", maxAttempts-attempts)
			}
			return err // commit attempt counter
		}
		if _, err := tx.Exec(ctx, `UPDATE email_verification_challenges SET consumed_at=now(), attempts=attempts+1 WHERE id=$1`, chID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET status='active', email_verified_at=now(), updated_at=now(), version=version+1 WHERE id=$1`, userID); err != nil {
			return err
		}
		s.securityEvent(ctx, tx, &userID, "email_verified", ci, nil)
		pair, err = s.createSession(ctx, tx, userID, ci)
		if err != nil {
			return err
		}
		return audit.Record(principal.With(ctx, &principal.Principal{Kind: principal.KindUser, ID: userID, IPPrefix: ci.IPPrefix}), tx,
			audit.Entry{Action: "user.email_verified", ResourceType: "user", ResourceID: userID.String()})
	})
	if err != nil {
		return nil, nil, err
	}
	if outcome != nil {
		return nil, nil, outcome
	}
	u, err := s.GetUser(ctx, userID)
	return pair, u, err
}

// ---------- Login ----------

type LoginInput struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DeviceLabel string `json:"device_label,omitempty"`
}

var errInvalidCredentials = apperr.Unauthenticated("auth.invalid_credentials", "Invalid email or password.")

func (s *Service) Login(ctx context.Context, in LoginInput, ci ClientInfo) (*TokenPair, *User, error) {
	if err := s.limit(ctx, "login:ip:"+ci.IP, 30, 15*time.Minute); err != nil {
		return nil, nil, err
	}
	_, norm, ok := NormalizeEmail(in.Email)
	if !ok || in.Password == "" || len(in.Password) > 1024 {
		_, _, _ = VerifyPassword(in.Password, s.dummyHash, s.argon)
		return nil, nil, errInvalidCredentials
	}
	if err := s.limit(ctx, "login:acct:"+crypto.SHA256Hex(norm), 10, 15*time.Minute); err != nil {
		return nil, nil, err
	}
	u, err := s.userByEmail(ctx, s.db.Pool, norm)
	if db.IsNoRows(err) {
		_, _, _ = VerifyPassword(in.Password, s.dummyHash, s.argon)
		return nil, nil, errInvalidCredentials
	}
	if err != nil {
		return nil, nil, err
	}
	okPw, rehash, err := VerifyPassword(in.Password, u.passwordHash, s.argon)
	if err != nil || !okPw {
		_ = s.db.Tx(ctx, func(tx pgx.Tx) error { s.securityEvent(ctx, tx, &u.ID, "login_failed", ci, nil); return nil })
		return nil, nil, errInvalidCredentials
	}
	switch u.Status {
	case "active":
	case "pending_verification":
		return nil, nil, apperr.Forbidden("auth.email_not_verified", "Verify your email address before signing in.")
	default:
		return nil, nil, apperr.Forbidden("auth.account_unavailable", "This account cannot sign in. Contact support.")
	}
	ci.DeviceLabel = deviceLabel(in.DeviceLabel)
	var pair *TokenPair
	err = s.db.Tx(ctx, func(tx pgx.Tx) error {
		if rehash {
			if _, err := tx.Exec(ctx, `UPDATE users SET password_hash=$2 WHERE id=$1`, u.ID, HashPassword(in.Password, s.argon)); err != nil {
				return err
			}
		}
		var err error
		pair, err = s.createSession(ctx, tx, u.ID, ci)
		if err != nil {
			return err
		}
		s.securityEvent(ctx, tx, &u.ID, "login_succeeded", ci, map[string]any{"session_id": pair.SessionID})
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return pair, u, nil
}

func (s *Service) createSession(ctx context.Context, tx pgx.Tx, userID uuid.UUID, ci ClientInfo) (*TokenPair, error) {
	now := s.now().UTC()
	sessID, famID := ids.New(), ids.New()
	absExp := now.Add(s.cfg.RefreshTTL)
	ua := ci.UserAgent
	if len(ua) > 200 {
		ua = ua[:200]
	}
	if _, err := tx.Exec(ctx, `INSERT INTO auth_sessions (id, user_id, device_label, user_agent, ip_prefix, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6)`, sessID, userID, ci.DeviceLabel, ua, ci.IPPrefix, absExp); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO auth_refresh_token_families (id, session_id, user_id, absolute_expires_at)
		VALUES ($1,$2,$3,$4)`, famID, sessID, userID, absExp); err != nil {
		return nil, err
	}
	refresh, err := s.insertRefreshToken(ctx, tx, famID, nil, absExp)
	if err != nil {
		return nil, err
	}
	access, accessExp, err := s.tokens.Issue(userID, sessID)
	if err != nil {
		return nil, err
	}
	return &TokenPair{AccessToken: access, TokenType: "Bearer", ExpiresIn: int(s.cfg.AccessTTL.Seconds()), AccessExpiresAt: accessExp,
		RefreshToken: refresh, RefreshExpiresAt: absExp, SessionID: sessID}, nil
}

func (s *Service) insertRefreshToken(ctx context.Context, tx pgx.Tx, famID uuid.UUID, parent *uuid.UUID, exp time.Time) (string, error) {
	raw := "rt_" + crypto.RandomToken(32)
	_, err := tx.Exec(ctx, `INSERT INTO auth_refresh_tokens (id, family_id, token_hash, parent_id, expires_at) VALUES ($1,$2,$3,$4,$5)`,
		ids.New(), famID, crypto.SHA256([]byte(raw)), parent, exp)
	return raw, err
}

// ---------- Refresh ----------

type RefreshInput struct {
	RefreshToken string `json:"refresh_token"`
}

var errInvalidRefresh = apperr.Unauthenticated("auth.invalid_refresh_token", "The refresh token is invalid or expired.")

func (s *Service) Refresh(ctx context.Context, in RefreshInput, ci ClientInfo) (*TokenPair, error) {
	if err := s.limit(ctx, "refresh:ip:"+ci.IP, 120, 15*time.Minute); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(in.RefreshToken, "rt_") || len(in.RefreshToken) > 100 {
		return nil, errInvalidRefresh
	}
	hash := crypto.SHA256([]byte(in.RefreshToken))
	var pair *TokenPair
	var outcome error
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		var tokID, famID, sessID, userID uuid.UUID
		var rotatedAt, revokedAt, famRevoked, sessRevoked *time.Time
		var expires, famExp time.Time
		var status string
		err := tx.QueryRow(ctx, `SELECT t.id, t.family_id, f.session_id, f.user_id, t.rotated_at, t.revoked_at, t.expires_at,
				f.revoked_at, f.absolute_expires_at, s.revoked_at, u.status
			FROM auth_refresh_tokens t
			JOIN auth_refresh_token_families f ON f.id = t.family_id
			JOIN auth_sessions s ON s.id = f.session_id
			JOIN users u ON u.id = f.user_id
			WHERE t.token_hash = $1 FOR UPDATE OF t, f`, hash).
			Scan(&tokID, &famID, &sessID, &userID, &rotatedAt, &revokedAt, &expires, &famRevoked, &famExp, &sessRevoked, &status)
		if db.IsNoRows(err) {
			outcome = errInvalidRefresh
			return nil
		}
		if err != nil {
			return err
		}
		if rotatedAt != nil && famRevoked == nil {
			// Replay of an already-rotated token: assume theft, revoke the whole family + session.
			if err := s.revokeSessionTx(ctx, tx, sessID, "refresh_token_reuse"); err != nil {
				return err
			}
			s.securityEvent(ctx, tx, &userID, "refresh_token_reuse_detected", ci, map[string]any{"session_id": sessID})
			outcome = apperr.Unauthenticated("auth.refresh_token_reused", "Refresh token reuse detected; the session was revoked. Sign in again.")
			return nil
		}
		now := s.now()
		if rotatedAt != nil || revokedAt != nil || famRevoked != nil || sessRevoked != nil || now.After(expires) || now.After(famExp) {
			outcome = errInvalidRefresh
			return nil
		}
		if status != "active" {
			outcome = apperr.Forbidden("auth.account_unavailable", "This account cannot sign in. Contact support.")
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE auth_refresh_tokens SET rotated_at=now() WHERE id=$1`, tokID); err != nil {
			return err
		}
		refresh, err := s.insertRefreshToken(ctx, tx, famID, &tokID, famExp)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE auth_sessions SET last_used_at=now(), ip_prefix=$2 WHERE id=$1`, sessID, ci.IPPrefix); err != nil {
			return err
		}
		access, accessExp, err := s.tokens.Issue(userID, sessID)
		if err != nil {
			return err
		}
		pair = &TokenPair{AccessToken: access, TokenType: "Bearer", ExpiresIn: int(s.cfg.AccessTTL.Seconds()), AccessExpiresAt: accessExp,
			RefreshToken: refresh, RefreshExpiresAt: famExp, SessionID: sessID}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if outcome != nil {
		return nil, outcome
	}
	return pair, nil
}

func (s *Service) revokeSessionTx(ctx context.Context, tx pgx.Tx, sessID uuid.UUID, reason string) error {
	if _, err := tx.Exec(ctx, `UPDATE auth_sessions SET revoked_at=now(), revoke_reason=$2 WHERE id=$1 AND revoked_at IS NULL`, sessID, reason); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE auth_refresh_token_families SET revoked_at=now(), revoke_reason=$2 WHERE session_id=$1 AND revoked_at IS NULL`, sessID, reason); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE auth_refresh_tokens SET revoked_at=now() WHERE family_id IN
		(SELECT id FROM auth_refresh_token_families WHERE session_id=$1) AND revoked_at IS NULL`, sessID)
	invalidateSessionCache(sessID)
	return err
}

// revokeAllTx revokes every session of a user, optionally keeping one.
func (s *Service) revokeAllTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, except *uuid.UUID, reason string) error {
	rows, err := tx.Query(ctx, `SELECT id FROM auth_sessions WHERE user_id=$1 AND revoked_at IS NULL AND ($2::uuid IS NULL OR id <> $2)`, userID, except)
	if err != nil {
		return err
	}
	sess, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	for _, id := range sess {
		if err := s.revokeSessionTx(ctx, tx, id, reason); err != nil {
			return err
		}
	}
	return nil
}

// ---------- Logout ----------

func (s *Service) Logout(ctx context.Context, p *principal.Principal, ci ClientInfo) error {
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		if err := s.revokeSessionTx(ctx, tx, p.SessionID, "logout"); err != nil {
			return err
		}
		s.securityEvent(ctx, tx, &p.ID, "logout", ci, map[string]any{"session_id": p.SessionID})
		return nil
	})
}

func (s *Service) LogoutAll(ctx context.Context, p *principal.Principal, ci ClientInfo) error {
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		if err := s.revokeAllTx(ctx, tx, p.ID, nil, "logout_all"); err != nil {
			return err
		}
		s.securityEvent(ctx, tx, &p.ID, "logout_all", ci, nil)
		return audit.Record(ctx, tx, audit.Entry{Action: "user.logout_all", ResourceType: "user", ResourceID: p.ID.String()})
	})
}

// ---------- Password recovery ----------

type ForgotInput struct {
	Email string `json:"email"`
}

func (s *Service) ForgotPassword(ctx context.Context, in ForgotInput, ci ClientInfo) error {
	if err := s.limit(ctx, "forgot:ip:"+ci.IP, 10, time.Hour); err != nil {
		return err
	}
	_, norm, ok := NormalizeEmail(in.Email)
	if !ok {
		return apperr.Validation(apperr.FieldError{Field: "email", Code: "invalid", Message: "A valid email address is required."})
	}
	// Per-account throttle is silent (never reveals existence).
	if ok, _, _ := s.limiter.Allow(ctx, "forgot:acct:"+crypto.SHA256Hex(norm), 3, time.Hour); !ok {
		return nil
	}
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		var userID uuid.UUID
		var status, email, locale string
		err := tx.QueryRow(ctx, `SELECT id, status, email, locale FROM users WHERE email_normalized=$1 FOR UPDATE`, norm).Scan(&userID, &status, &email, &locale)
		if db.IsNoRows(err) || (err == nil && status != "active" && status != "pending_verification") {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE password_reset_challenges SET invalidated_at=now()
			WHERE user_id=$1 AND consumed_at IS NULL AND invalidated_at IS NULL`, userID); err != nil {
			return err
		}
		chID := ids.New()
		code := crypto.SixDigitCode()
		exp := s.now().UTC().Add(s.cfg.ResetTTL)
		mac := crypto.HMAC(s.cfg.CodeHMACSecret, "reset", chID.String(), userID.String(), code)
		if _, err := tx.Exec(ctx, `INSERT INTO password_reset_challenges (id, user_id, code_hmac, max_attempts, expires_at)
			VALUES ($1,$2,$3,$4,$5)`, chID, userID, mac, s.cfg.CodeMaxAttempts, exp); err != nil {
			return err
		}
		s.securityEvent(ctx, tx, &userID, "password_reset_requested", ci, nil)
		return mail.Enqueue(ctx, tx, s.sealer, mail.Email{UserID: &userID, To: email, Template: "password_reset", Locale: locale,
			ExpiresAt: &exp, IdempotencyKey: "reset:" + chID.String(),
			Data: map[string]string{"code": code, "minutes": fmt.Sprint(int(s.cfg.ResetTTL.Minutes()))}})
	})
}

type ResetInput struct {
	Email       string `json:"email"`
	Code        string `json:"code"`
	NewPassword string `json:"new_password"`
}

func (s *Service) ResetPassword(ctx context.Context, in ResetInput, ci ClientInfo) error {
	if err := s.limit(ctx, "reset:ip:"+ci.IP, 30, 15*time.Minute); err != nil {
		return err
	}
	email, norm, ok := NormalizeEmail(in.Email)
	invalid := apperr.ValidationMsg("auth.code_invalid", "The reset code is invalid or has already been used.")
	if !ok || len(in.Code) != 6 {
		return invalid
	}
	if code, msg, ok := ValidatePassword(in.NewPassword, email); !ok {
		return apperr.Validation(apperr.FieldError{Field: "new_password", Code: code, Message: msg})
	}
	newHash := HashPassword(in.NewPassword, s.argon)
	var outcome error
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		var chID, userID uuid.UUID
		var mac []byte
		var attempts, maxAttempts int
		var exp time.Time
		var locale string
		err := tx.QueryRow(ctx, `SELECT c.id, c.user_id, c.code_hmac, c.attempts, c.max_attempts, c.expires_at, u.locale
			FROM users u JOIN password_reset_challenges c ON c.user_id=u.id
			WHERE u.email_normalized=$1 AND c.consumed_at IS NULL AND c.invalidated_at IS NULL FOR UPDATE OF c, u`, norm).
			Scan(&chID, &userID, &mac, &attempts, &maxAttempts, &exp, &locale)
		if db.IsNoRows(err) {
			outcome = invalid
			return nil
		}
		if err != nil {
			return err
		}
		if s.now().After(exp) {
			_, _ = tx.Exec(ctx, `UPDATE password_reset_challenges SET invalidated_at=now() WHERE id=$1`, chID)
			outcome = apperr.ValidationMsg("auth.code_expired", "The reset code has expired. Request a new one.")
			return nil
		}
		if !crypto.Equal(mac, crypto.HMAC(s.cfg.CodeHMACSecret, "reset", chID.String(), userID.String(), in.Code)) {
			attempts++
			if attempts >= maxAttempts {
				_, err = tx.Exec(ctx, `UPDATE password_reset_challenges SET attempts=$2, invalidated_at=now() WHERE id=$1`, chID, attempts)
				outcome = apperr.ValidationMsg("auth.code_locked", "Too many incorrect attempts. Request a new code.")
			} else {
				_, err = tx.Exec(ctx, `UPDATE password_reset_challenges SET attempts=$2 WHERE id=$1`, chID, attempts)
				outcome = invalid
			}
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE password_reset_challenges SET consumed_at=now() WHERE id=$1`, chID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE password_reset_challenges SET invalidated_at=now() WHERE user_id=$1 AND consumed_at IS NULL AND invalidated_at IS NULL`, userID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET password_hash=$2, password_changed_at=now(), updated_at=now(), version=version+1 WHERE id=$1`, userID, newHash); err != nil {
			return err
		}
		if err := s.revokeAllTx(ctx, tx, userID, nil, "password_reset"); err != nil {
			return err
		}
		s.securityEvent(ctx, tx, &userID, "password_reset_completed", ci, nil)
		return mail.Enqueue(ctx, tx, s.sealer, mail.Email{UserID: &userID, To: email, Template: "password_changed", Locale: locale,
			IdempotencyKey: "pwchanged:" + chID.String()})
	})
	if err != nil {
		return err
	}
	return outcome
}

type ChangePasswordInput struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func (s *Service) ChangePassword(ctx context.Context, p *principal.Principal, in ChangePasswordInput, ci ClientInfo) error {
	if err := s.limit(ctx, "chpw:user:"+p.ID.String(), 10, time.Hour); err != nil {
		return err
	}
	u, err := s.getUserFull(ctx, s.db.Pool, p.ID)
	if err != nil {
		return err
	}
	if ok, _, _ := VerifyPassword(in.CurrentPassword, u.passwordHash, s.argon); !ok {
		return apperr.ValidationMsg("auth.invalid_current_password", "The current password is incorrect.")
	}
	if code, msg, ok := ValidatePassword(in.NewPassword, u.Email); !ok {
		return apperr.Validation(apperr.FieldError{Field: "new_password", Code: code, Message: msg})
	}
	hash := HashPassword(in.NewPassword, s.argon)
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE users SET password_hash=$2, password_changed_at=now(), updated_at=now(), version=version+1 WHERE id=$1`, p.ID, hash); err != nil {
			return err
		}
		if err := s.revokeAllTx(ctx, tx, p.ID, &p.SessionID, "password_changed"); err != nil {
			return err
		}
		s.securityEvent(ctx, tx, &p.ID, "password_changed", ci, nil)
		return mail.Enqueue(ctx, tx, s.sealer, mail.Email{UserID: &p.ID, To: u.Email, Template: "password_changed", Locale: u.Locale,
			IdempotencyKey: "pwchanged:" + ids.New().String()})
	})
}

// ---------- Security events ----------

func (s *Service) securityEvent(ctx context.Context, tx pgx.Tx, userID *uuid.UUID, typ string, ci ClientInfo, meta map[string]any) {
	if meta == nil {
		meta = map[string]any{}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO user_security_events (id, user_id, event_type, ip_prefix, metadata) VALUES ($1,$2,$3,$4,$5)`,
		ids.New(), userID, typ, ci.IPPrefix, meta); err != nil {
		s.log.Error("record security event", "type", typ, "error", err)
	}
}
