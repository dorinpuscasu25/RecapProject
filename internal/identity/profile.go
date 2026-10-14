package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dorinpuscasu25/recapproject/internal/audit"
	"github.com/dorinpuscasu25/recapproject/internal/jobs"
	"github.com/dorinpuscasu25/recapproject/internal/mail"
	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/crypto"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ids"
	"github.com/dorinpuscasu25/recapproject/internal/platform/principal"
	"github.com/dorinpuscasu25/recapproject/internal/platform/storage"
)

const userCols = `u.id, u.first_name, u.last_name, u.email, u.email_normalized, u.email_verified_at, u.phone_e164,
	u.phone_verified_at IS NOT NULL, u.status, u.locale, u.timezone, u.marketing_opt_in, u.created_at, u.version, u.password_hash,
	COALESCE((SELECT array_agg(role_code ORDER BY role_code) FROM user_roles r WHERE r.user_id=u.id), '{}')`

func scanUser(row pgx.Row) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.FirstName, &u.LastName, &u.Email, &u.emailNorm, &u.EmailVerifiedAt, &u.Phone, &u.PhoneVerified,
		&u.Status, &u.Locale, &u.Timezone, &u.MarketingOptIn, &u.CreatedAt, &u.Version, &u.passwordHash, &u.Roles)
	return &u, err
}

func (s *Service) userByEmail(ctx context.Context, q db.Querier, norm string) (*User, error) {
	return scanUser(q.QueryRow(ctx, `SELECT `+userCols+` FROM users u WHERE u.email_normalized=$1`, norm))
}

func (s *Service) getUserFull(ctx context.Context, q db.Querier, id uuid.UUID) (*User, error) {
	u, err := scanUser(q.QueryRow(ctx, `SELECT `+userCols+` FROM users u WHERE u.id=$1`, id))
	if db.IsNoRows(err) {
		return nil, apperr.NotFound("user_not_found", "User not found.")
	}
	return u, err
}

func (s *Service) GetUser(ctx context.Context, id uuid.UUID) (*User, error) {
	return s.getUserFull(ctx, s.db.Pool, id)
}

// FindUserByEmail is used by operator tooling (bootstrap admin).
func (s *Service) FindUserByEmail(ctx context.Context, email string) (*User, error) {
	_, norm, ok := NormalizeEmail(email)
	if !ok {
		return nil, apperr.Validation(apperr.FieldError{Field: "email", Code: "invalid", Message: "Invalid email."})
	}
	u, err := s.userByEmail(ctx, s.db.Pool, norm)
	if db.IsNoRows(err) {
		return nil, apperr.NotFound("user_not_found", "User not found.")
	}
	return u, err
}

type UpdateMeInput struct {
	FirstName      *string `json:"first_name,omitempty"`
	LastName       *string `json:"last_name,omitempty"`
	Phone          *string `json:"phone,omitempty"`
	Locale         *string `json:"locale,omitempty"`
	Timezone       *string `json:"timezone,omitempty"`
	MarketingOptIn *bool   `json:"marketing_opt_in,omitempty"`
	Version        *int    `json:"version,omitempty"` // optimistic concurrency (optional)
}

func (s *Service) UpdateMe(ctx context.Context, p *principal.Principal, in UpdateMeInput) (*User, error) {
	cur, err := s.GetUser(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	var v apperr.Validator
	first, last, phone, locale, tz := cur.FirstName, cur.LastName, cur.Phone, cur.Locale, cur.Timezone
	marketing := cur.MarketingOptIn
	phoneChanged := false
	if in.FirstName != nil {
		n, ok := validName(*in.FirstName)
		v.Check(ok, "first_name", "invalid", "First name must be 1-100 letters.")
		first = n
	}
	if in.LastName != nil {
		n, ok := validName(*in.LastName)
		v.Check(ok, "last_name", "invalid", "Last name must be 1-100 letters.")
		last = n
	}
	if in.Phone != nil {
		ph, ok := NormalizePhone(*in.Phone, "MD")
		v.Check(ok, "phone", "invalid", "A valid international phone number is required.")
		phoneChanged = ok && ph != cur.Phone
		phone = ph
	}
	if in.Locale != nil {
		v.Check(validLocale(*in.Locale), "locale", "invalid", "Locale must be one of ro, ru, en.")
		locale = *in.Locale
	}
	if in.Timezone != nil {
		v.Check(validTimezone(*in.Timezone), "timezone", "invalid", "Timezone must be a valid IANA zone.")
		tz = *in.Timezone
	}
	if in.MarketingOptIn != nil {
		marketing = *in.MarketingOptIn
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	err = s.db.Tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE users SET first_name=$2, last_name=$3, phone_e164=$4,
			phone_verified_at = CASE WHEN $5 THEN NULL ELSE phone_verified_at END,
			locale=$6, timezone=$7, marketing_opt_in=$8, updated_at=now(), version=version+1
			WHERE id=$1 AND ($9::int IS NULL OR version=$9)`, p.ID, first, last, phone, phoneChanged, locale, tz, marketing, in.Version)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return apperr.VersionConflict("profile")
		}
		if in.MarketingOptIn != nil && *in.MarketingOptIn != cur.MarketingOptIn {
			if _, err := tx.Exec(ctx, `INSERT INTO user_terms_acceptances (id, user_id, document, policy_version, accepted) VALUES ($1,$2,'marketing',$3,$4)`,
				ids.New(), p.ID, s.cfg.TermsPolicyVersion, marketing); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "user.profile_updated", ResourceType: "user", ResourceID: p.ID.String(),
			Old: map[string]any{"first_name": cur.FirstName, "last_name": cur.LastName, "locale": cur.Locale, "timezone": cur.Timezone, "phone_changed": false},
			New: map[string]any{"first_name": first, "last_name": last, "locale": locale, "timezone": tz, "phone_changed": phoneChanged}})
	})
	if err != nil {
		return nil, err
	}
	return s.GetUser(ctx, p.ID)
}

// ---------- Email change ----------

type EmailChangeInput struct {
	NewEmail string `json:"new_email"`
	Password string `json:"password"`
}

func (s *Service) InitiateEmailChange(ctx context.Context, p *principal.Principal, in EmailChangeInput, ci ClientInfo) error {
	if err := s.limit(ctx, "emailchange:user:"+p.ID.String(), 5, time.Hour); err != nil {
		return err
	}
	u, err := s.GetUser(ctx, p.ID)
	if err != nil {
		return err
	}
	if ok, _, _ := VerifyPassword(in.Password, u.passwordHash, s.argon); !ok {
		return apperr.ValidationMsg("auth.invalid_current_password", "The current password is incorrect.")
	}
	email, norm, ok := NormalizeEmail(in.NewEmail)
	if !ok {
		return apperr.Validation(apperr.FieldError{Field: "new_email", Code: "invalid", Message: "A valid email address is required."})
	}
	if norm == u.emailNorm {
		return apperr.Validation(apperr.FieldError{Field: "new_email", Code: "unchanged", Message: "The new email equals the current one."})
	}
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		var taken bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE email_normalized=$1)`, norm).Scan(&taken); err != nil {
			return err
		}
		if taken {
			// Do not reveal that another account owns the address; no code is sent.
			s.securityEvent(ctx, tx, &p.ID, "email_change_target_taken", ci, nil)
			return nil
		}
		if err := s.issueEmailChallenge(ctx, tx, p.ID, "email_change", email, norm, u.FirstName, u.Locale); err != nil {
			return err
		}
		s.securityEvent(ctx, tx, &p.ID, "email_change_requested", ci, nil)
		return mail.Enqueue(ctx, tx, s.sealer, mail.Email{UserID: &p.ID, To: u.Email, Template: "email_change_notice", Locale: u.Locale,
			IdempotencyKey: "emailchange-notice:" + ids.New().String()})
	})
}

type EmailChangeConfirmInput struct {
	Code string `json:"code"`
}

func (s *Service) ConfirmEmailChange(ctx context.Context, p *principal.Principal, in EmailChangeConfirmInput, ci ClientInfo) (*User, error) {
	var outcome error
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		var chID uuid.UUID
		var mac []byte
		var attempts, maxAttempts int
		var exp time.Time
		var email, norm string
		err := tx.QueryRow(ctx, `SELECT id, code_hmac, attempts, max_attempts, expires_at, target_email, target_email_normalized
			FROM email_verification_challenges WHERE user_id=$1 AND purpose='email_change' AND consumed_at IS NULL AND invalidated_at IS NULL
			FOR UPDATE`, p.ID).Scan(&chID, &mac, &attempts, &maxAttempts, &exp, &email, &norm)
		if db.IsNoRows(err) {
			outcome = errCodeInvalid
			return nil
		}
		if err != nil {
			return err
		}
		if s.now().After(exp) {
			outcome = apperr.ValidationMsg("auth.code_expired", "The code has expired. Start the email change again.")
			_, err := tx.Exec(ctx, `UPDATE email_verification_challenges SET invalidated_at=now(), invalidated_reason='expired' WHERE id=$1`, chID)
			return err
		}
		if !crypto.Equal(mac, crypto.HMAC(s.cfg.CodeHMACSecret, "email:email_change", chID.String(), p.ID.String(), in.Code)) {
			attempts++
			if attempts >= maxAttempts {
				outcome = apperr.ValidationMsg("auth.code_locked", "Too many incorrect attempts. Start the email change again.")
				_, err = tx.Exec(ctx, `UPDATE email_verification_challenges SET attempts=$2, invalidated_at=now(), invalidated_reason='max_attempts' WHERE id=$1`, chID, attempts)
			} else {
				outcome = errCodeInvalid
				_, err = tx.Exec(ctx, `UPDATE email_verification_challenges SET attempts=$2 WHERE id=$1`, chID, attempts)
			}
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE email_verification_challenges SET consumed_at=now() WHERE id=$1`, chID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE users SET email=$2, email_normalized=$3, email_verified_at=now(), updated_at=now(), version=version+1 WHERE id=$1`, p.ID, email, norm)
		if db.IsUniqueViolation(err) {
			outcome = apperr.Conflict("auth.email_unavailable", "This email address can no longer be used.")
			return nil
		}
		if err != nil {
			return err
		}
		s.securityEvent(ctx, tx, &p.ID, "email_changed", ci, nil)
		return audit.Record(ctx, tx, audit.Entry{Action: "user.email_changed", ResourceType: "user", ResourceID: p.ID.String()})
	})
	if err != nil {
		return nil, err
	}
	if outcome != nil {
		return nil, outcome
	}
	return s.GetUser(ctx, p.ID)
}

// ---------- Sessions ----------

type Session struct {
	ID          uuid.UUID `json:"id"`
	DeviceLabel string    `json:"device_label"`
	UserAgent   string    `json:"user_agent"`
	IPPrefix    string    `json:"ip_prefix"`
	CreatedAt   time.Time `json:"created_at"`
	LastUsedAt  time.Time `json:"last_used_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	Current     bool      `json:"current"`
}

func (s *Service) ListSessions(ctx context.Context, p *principal.Principal) ([]Session, error) {
	rows, err := s.db.Pool.Query(ctx, `SELECT id, device_label, user_agent, ip_prefix, created_at, last_used_at, expires_at
		FROM auth_sessions WHERE user_id=$1 AND revoked_at IS NULL AND expires_at > now() ORDER BY last_used_at DESC LIMIT 100`, p.ID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Session, error) {
		var x Session
		err := r.Scan(&x.ID, &x.DeviceLabel, &x.UserAgent, &x.IPPrefix, &x.CreatedAt, &x.LastUsedAt, &x.ExpiresAt)
		x.Current = x.ID == p.SessionID
		return x, err
	})
}

func (s *Service) RevokeSession(ctx context.Context, p *principal.Principal, id uuid.UUID, ci ClientInfo) error {
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		var owner uuid.UUID
		err := tx.QueryRow(ctx, `SELECT user_id FROM auth_sessions WHERE id=$1 AND revoked_at IS NULL`, id).Scan(&owner)
		if db.IsNoRows(err) || (err == nil && owner != p.ID) {
			return apperr.NotFound("session_not_found", "Session not found.")
		}
		if err != nil {
			return err
		}
		if err := s.revokeSessionTx(ctx, tx, id, "user_revoked"); err != nil {
			return err
		}
		s.securityEvent(ctx, tx, &p.ID, "session_revoked", ci, map[string]any{"session_id": id})
		return nil
	})
}

// ---------- Privacy: export & deletion ----------

type DataRequest struct {
	ID          uuid.UUID  `json:"id"`
	Kind        string     `json:"kind"`
	Status      string     `json:"status"`
	RequestedAt time.Time  `json:"requested_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	// DownloadURL is set for completed exports: a short-lived presigned URL (S3) or the
	// authenticated download endpoint.
	DownloadURL  string     `json:"download_url,omitempty"`
	URLExpiresAt *time.Time `json:"url_expires_at,omitempty"`
	resultKey    *string
}

// exportURLTTL bounds how long a presigned export link stays valid.
const exportURLTTL = 15 * time.Minute

const JobUserExport = "user.export"
const JobUserDeletion = "user.deletion"

// DeletionGracePeriod is how long a deletion_pending account is kept before purge.
var DeletionGracePeriod = 7 * 24 * time.Hour

func (s *Service) RequestExport(ctx context.Context, p *principal.Principal) (*DataRequest, error) {
	if err := s.limit(ctx, "export:user:"+p.ID.String(), 3, 24*time.Hour); err != nil {
		return nil, err
	}
	r := &DataRequest{ID: ids.New(), Kind: "export", Status: "queued", RequestedAt: s.now().UTC()}
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO user_data_requests (id, user_id, kind) VALUES ($1,$2,'export')`, r.ID, p.ID); err != nil {
			return err
		}
		_, err := jobs.Enqueue(ctx, tx, JobUserExport, map[string]string{"request_id": r.ID.String(), "user_id": p.ID.String()},
			jobs.EnqueueOpts{IdempotencyKey: "export:" + r.ID.String(), CreatedBy: &p.ID})
		return err
	})
	return r, err
}

type DeletionInput struct {
	Password string `json:"password"`
}

func (s *Service) RequestDeletion(ctx context.Context, p *principal.Principal, in DeletionInput, ci ClientInfo) (*DataRequest, error) {
	u, err := s.GetUser(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	if ok, _, _ := VerifyPassword(in.Password, u.passwordHash, s.argon); !ok {
		return nil, apperr.ValidationMsg("auth.invalid_current_password", "The current password is incorrect.")
	}
	r := &DataRequest{ID: ids.New(), Kind: "deletion", Status: "queued", RequestedAt: s.now().UTC()}
	err = s.db.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE users SET status='deletion_pending', deletion_requested_at=now(), updated_at=now(), version=version+1 WHERE id=$1`, p.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO user_data_requests (id, user_id, kind) VALUES ($1,$2,'deletion')`, r.ID, p.ID); err != nil {
			return err
		}
		if err := s.revokeAllTx(ctx, tx, p.ID, nil, "account_deletion"); err != nil {
			return err
		}
		if _, err := jobs.Enqueue(ctx, tx, JobUserDeletion, map[string]string{"request_id": r.ID.String(), "user_id": p.ID.String()},
			jobs.EnqueueOpts{IdempotencyKey: "deletion:" + r.ID.String(), RunAt: s.now().Add(DeletionGracePeriod)}); err != nil {
			return err
		}
		s.securityEvent(ctx, tx, &p.ID, "deletion_requested", ci, nil)
		return audit.Record(ctx, tx, audit.Entry{Action: "user.deletion_requested", ResourceType: "user", ResourceID: p.ID.String()})
	})
	return r, err
}

func (s *Service) ListDataRequests(ctx context.Context, p *principal.Principal) ([]DataRequest, error) {
	rows, err := s.db.Pool.Query(ctx, `SELECT id, kind, status, requested_at, completed_at, result_object_key FROM user_data_requests WHERE user_id=$1 ORDER BY requested_at DESC LIMIT 50`, p.ID)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (DataRequest, error) {
		var d DataRequest
		err := r.Scan(&d.ID, &d.Kind, &d.Status, &d.RequestedAt, &d.CompletedAt, &d.resultKey)
		return d, err
	})
	if err != nil || s.store == nil {
		return list, err
	}
	for i := range list {
		d := &list[i]
		if d.Kind != "export" || d.Status != "succeeded" || d.resultKey == nil {
			continue
		}
		if u, err := s.store.PresignGet(ctx, *d.resultKey, exportURLTTL); err == nil && u != "" {
			exp := s.now().Add(exportURLTTL).UTC()
			d.DownloadURL, d.URLExpiresAt = u, &exp
		} else {
			d.DownloadURL = "/v1/users/me/data-requests/" + d.ID.String() + "/download"
		}
	}
	return list, nil
}

// OpenExport streams a completed export to its owner.
func (s *Service) OpenExport(ctx context.Context, p *principal.Principal, id uuid.UUID) (io.ReadCloser, int64, error) {
	var key *string
	var status string
	err := s.db.Pool.QueryRow(ctx, `SELECT status, result_object_key FROM user_data_requests WHERE id=$1 AND user_id=$2 AND kind='export'`, id, p.ID).Scan(&status, &key)
	if db.IsNoRows(err) {
		return nil, 0, apperr.NotFound("data_request_not_found", "Data request not found.")
	}
	if err != nil {
		return nil, 0, err
	}
	if status != "succeeded" || key == nil || s.store == nil {
		return nil, 0, apperr.Conflict("export_not_ready", "The export is not ready ("+status+").")
	}
	rc, n, err := s.store.Get(ctx, *key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, 0, apperr.NotFound("export_expired", "The export file is no longer available; request a new export.")
	}
	return rc, n, err
}

// BuildExport assembles the user's personal data (profile, preferences, interests, sessions
// metadata, chat history, briefings, notification settings). Used by the export job.
func (s *Service) BuildExport(ctx context.Context, userID uuid.UUID) (map[string]any, error) {
	out := map[string]any{"generated_at": time.Now().UTC(), "format_version": 1}
	queries := map[string]string{
		"profile":         `SELECT row_to_json(x) FROM (SELECT id, first_name, last_name, email, phone_e164, status, locale, timezone, marketing_opt_in, created_at, email_verified_at FROM users WHERE id=$1) x`,
		"preferences":     `SELECT row_to_json(x) FROM (SELECT * FROM user_preferences WHERE user_id=$1) x`,
		"interests":       `SELECT coalesce(json_agg(x),'[]') FROM (SELECT interest_type, target_key, mode, weight, created_at FROM user_interests WHERE user_id=$1) x`,
		"sessions":        `SELECT coalesce(json_agg(x),'[]') FROM (SELECT id, device_label, created_at, last_used_at, revoked_at FROM auth_sessions WHERE user_id=$1) x`,
		"consents":        `SELECT coalesce(json_agg(x),'[]') FROM (SELECT document, policy_version, accepted, created_at FROM user_terms_acceptances WHERE user_id=$1) x`,
		"chat_sessions":   `SELECT coalesce(json_agg(x),'[]') FROM (SELECT s.id, s.scope_type, s.created_at, (SELECT json_agg(json_build_object('role', m.role, 'content', m.content, 'created_at', m.created_at) ORDER BY m.created_at) FROM chat_messages m WHERE m.session_id=s.id) AS messages FROM chat_sessions s WHERE s.user_id=$1) x`,
		"briefings":       `SELECT coalesce(json_agg(x),'[]') FROM (SELECT id, status, language, duration_minutes, requested_at FROM briefings WHERE user_id=$1) x`,
		"notifications":   `SELECT coalesce(json_agg(x),'[]') FROM (SELECT name, kind, local_times, weekdays, timezone, enabled FROM notification_schedules WHERE user_id=$1) x`,
		"followed_events": `SELECT coalesce(json_agg(x),'[]') FROM (SELECT event_id, created_at FROM followed_events WHERE user_id=$1) x`,
	}
	for k, q := range queries {
		var raw []byte
		if err := s.db.Pool.QueryRow(ctx, q, userID).Scan(&raw); err != nil && !db.IsNoRows(err) {
			return nil, fmt.Errorf("export %s: %w", k, err)
		}
		out[k] = json.RawMessage(raw)
	}
	return out, nil
}

// PurgeUser hard-deletes a deletion_pending user (cascades personal data). Audit logs keep the
// opaque actor id (documented retention exception); security events are deleted by cascade.
func (s *Service) PurgeUser(ctx context.Context, userID uuid.UUID, requestID uuid.UUID) error {
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&status)
		if db.IsNoRows(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if status != "deletion_pending" {
			_, err := tx.Exec(ctx, `UPDATE user_data_requests SET status='cancelled', completed_at=now() WHERE id=$1`, requestID)
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM users WHERE id=$1`, userID); err != nil {
			return err
		}
		ctx = principal.With(ctx, principal.System())
		return audit.Record(ctx, tx, audit.Entry{Action: "user.purged", ResourceType: "user", ResourceID: userID.String()})
	})
}

// ---------- Principal resolution ----------

type sessionInfo struct {
	userID  uuid.UUID
	status  string
	roles   []string
	revoked bool
	at      time.Time
}

var (
	sessCacheMu sync.Mutex
	sessCache   = map[uuid.UUID]sessionInfo{}
)

// sessionCacheTTL bounds how long a revoked session or suspended account can still use an
// unexpired access token on another replica. Revocation on the same process is immediate.
var sessionCacheTTL = 10 * time.Second

func invalidateSessionCache(id uuid.UUID) {
	sessCacheMu.Lock()
	delete(sessCache, id)
	sessCacheMu.Unlock()
}

// InvalidateAllSessionCache drops cached session state (used after role/status changes).
func InvalidateAllSessionCache() {
	sessCacheMu.Lock()
	sessCache = map[uuid.UUID]sessionInfo{}
	sessCacheMu.Unlock()
}

// Authenticate resolves a bearer token into a principal. It enforces session revocation and
// account state on every request.
func (s *Service) Authenticate(ctx context.Context, bearer string) (*principal.Principal, error) {
	claims, err := s.tokens.Validate(bearer)
	if err != nil {
		return nil, apperr.Unauthenticated("auth.invalid_token", "The access token is invalid or expired.")
	}
	uid, err1 := uuid.Parse(claims.Subject)
	sid, err2 := uuid.Parse(claims.SessionID)
	if err1 != nil || err2 != nil {
		return nil, apperr.Unauthenticated("auth.invalid_token", "The access token is invalid or expired.")
	}
	info, err := s.sessionInfo(ctx, sid)
	if err != nil {
		return nil, err
	}
	if info.revoked || info.userID != uid {
		return nil, apperr.Unauthenticated("auth.session_revoked", "The session has ended. Sign in again.")
	}
	if info.status != "active" {
		return nil, apperr.Forbidden("auth.account_unavailable", "This account is not active.")
	}
	perms, err := s.permissionsFor(ctx, info.roles)
	if err != nil {
		return nil, err
	}
	return &principal.Principal{Kind: principal.KindUser, ID: uid, SessionID: sid, Roles: info.roles, Permissions: perms, TokenID: claims.ID}, nil
}

func (s *Service) sessionInfo(ctx context.Context, sid uuid.UUID) (sessionInfo, error) {
	sessCacheMu.Lock()
	info, ok := sessCache[sid]
	sessCacheMu.Unlock()
	if ok && time.Since(info.at) < sessionCacheTTL {
		return info, nil
	}
	var revokedAt *time.Time
	var expires time.Time
	err := s.db.Pool.QueryRow(ctx, `SELECT s.user_id, s.revoked_at, s.expires_at, u.status,
		COALESCE((SELECT array_agg(role_code) FROM user_roles r WHERE r.user_id=u.id), '{}')
		FROM auth_sessions s JOIN users u ON u.id = s.user_id WHERE s.id=$1`, sid).
		Scan(&info.userID, &revokedAt, &expires, &info.status, &info.roles)
	if db.IsNoRows(err) {
		return sessionInfo{}, apperr.Unauthenticated("auth.session_revoked", "The session has ended. Sign in again.")
	}
	if err != nil {
		return sessionInfo{}, err
	}
	info.revoked = revokedAt != nil || time.Now().After(expires)
	info.at = time.Now()
	sessCacheMu.Lock()
	sessCache[sid] = info
	sessCacheMu.Unlock()
	return info, nil
}

func (s *Service) permissionsFor(ctx context.Context, roles []string) (map[string]bool, error) {
	s.permMu.RLock()
	cache, at := s.permCache, s.permAt
	s.permMu.RUnlock()
	if cache == nil || time.Since(at) > time.Minute {
		rows, err := s.db.Pool.Query(ctx, `SELECT role_code, permission_code FROM role_permissions`)
		if err != nil {
			return nil, err
		}
		m := map[string][]string{}
		for rows.Next() {
			var r, p string
			if err := rows.Scan(&r, &p); err != nil {
				rows.Close()
				return nil, err
			}
			m[r] = append(m[r], p)
		}
		rows.Close()
		s.permMu.Lock()
		s.permCache, s.permAt = m, time.Now()
		s.permMu.Unlock()
		cache = m
	}
	out := map[string]bool{}
	for _, r := range roles {
		for _, p := range cache[r] {
			out[p] = true
		}
	}
	return out, nil
}

// AuthenticateServiceKey resolves an X-API-Key (format "sk_<prefix>_<secret>").
func (s *Service) AuthenticateServiceKey(ctx context.Context, key string) (*principal.Principal, error) {
	parts := strings.SplitN(key, "_", 3)
	if len(parts) != 3 || parts[0] != "sk" {
		return nil, apperr.Unauthenticated("auth.invalid_api_key", "Invalid API key.")
	}
	var id uuid.UUID
	var hash []byte
	var roles []string
	var revoked *time.Time
	err := s.db.Pool.QueryRow(ctx, `SELECT id, key_hash, roles, revoked_at FROM service_accounts WHERE key_prefix=$1`, parts[1]).
		Scan(&id, &hash, &roles, &revoked)
	if err != nil || revoked != nil || !crypto.Equal(hash, crypto.SHA256([]byte(key))) {
		return nil, apperr.Unauthenticated("auth.invalid_api_key", "Invalid API key.")
	}
	_, _ = s.db.Pool.Exec(ctx, `UPDATE service_accounts SET last_used_at=now() WHERE id=$1`, id)
	perms, err := s.permissionsFor(ctx, roles)
	if err != nil {
		return nil, err
	}
	return &principal.Principal{Kind: principal.KindService, ID: id, Roles: roles, Permissions: perms}, nil
}

// CreateServiceAccount returns the plaintext key once.
func (s *Service) CreateServiceAccount(ctx context.Context, name string, roles []string) (uuid.UUID, string, error) {
	for _, r := range roles {
		if r != "service" && r != "analyst" {
			return uuid.Nil, "", apperr.ValidationMsg("invalid_role", "Service accounts may only hold service or analyst roles.")
		}
	}
	prefix := strings.ToLower(crypto.RandomToken(6))
	prefix = strings.NewReplacer("-", "x", "_", "y").Replace(prefix)
	key := "sk_" + prefix + "_" + crypto.RandomToken(32)
	id := ids.New()
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO service_accounts (id, name, key_prefix, key_hash, roles) VALUES ($1,$2,$3,$4,$5)`,
			id, name, prefix, crypto.SHA256([]byte(key)), roles); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "service_account.created", ResourceType: "service_account", ResourceID: id.String(), New: map[string]any{"name": name, "roles": roles}})
	})
	return id, key, err
}

// ---------- Role management ----------

var staffRoles = map[string]bool{"super_admin": true, "admin": true, "editor": true, "fact_checker": true, "analyst": true}

// SetRoles replaces a user's staff roles. Only super_admin may grant admin/super_admin; admins
// may grant editor/fact_checker/analyst. Nobody can modify their own roles.
func (s *Service) SetRoles(ctx context.Context, actor *principal.Principal, userID uuid.UUID, roles []string) (*User, error) {
	if actor.ID == userID {
		return nil, apperr.Forbidden("roles.self_modification", "You cannot change your own roles.")
	}
	for _, r := range roles {
		if !staffRoles[r] && r != "user" {
			return nil, apperr.ValidationMsg("roles.invalid", "Unknown or non-assignable role: "+r)
		}
		if (r == "super_admin" || r == "admin") && !actor.HasRole("super_admin") {
			return nil, apperr.Forbidden("roles.insufficient_privilege", "Only a super admin can grant admin roles.")
		}
	}
	cur, err := s.GetUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	for _, r := range cur.Roles {
		if (r == "super_admin" || r == "admin") && !actor.HasRole("super_admin") {
			return nil, apperr.Forbidden("roles.insufficient_privilege", "Only a super admin can modify an admin.")
		}
	}
	if cur.Status != "active" {
		return nil, apperr.ValidationMsg("roles.user_not_active", "Roles can only be granted to verified, active accounts.")
	}
	err = s.db.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM user_roles WHERE user_id=$1 AND role_code <> 'user'`, userID); err != nil {
			return err
		}
		for _, r := range roles {
			if _, err := tx.Exec(ctx, `INSERT INTO user_roles (user_id, role_code, granted_by) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, userID, r, actor.ID); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "user.roles_set", ResourceType: "user", ResourceID: userID.String(),
			Old: map[string]any{"roles": cur.Roles}, New: map[string]any{"roles": roles}})
	})
	if err != nil {
		return nil, err
	}
	InvalidateAllSessionCache()
	_ = db.NotifyConfig(ctx, s.db.Pool, "identity")
	return s.GetUser(ctx, userID)
}

// SetStatus suspends/disables/reactivates an account (admin action) and revokes sessions when locking.
func (s *Service) SetStatus(ctx context.Context, actor *principal.Principal, userID uuid.UUID, status, reason string) error {
	if status != "active" && status != "suspended" && status != "disabled" {
		return apperr.ValidationMsg("users.invalid_status", "Status must be active, suspended or disabled.")
	}
	if actor.ID == userID {
		return apperr.Forbidden("users.self_modification", "You cannot change your own status.")
	}
	cur, err := s.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	if status == "active" && cur.EmailVerifiedAt == nil {
		return apperr.ValidationMsg("users.not_verified", "Unverified accounts cannot be activated by an administrator.")
	}
	err = s.db.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE users SET status=$2, updated_at=now(), version=version+1 WHERE id=$1`, userID, status); err != nil {
			return err
		}
		if status != "active" {
			if err := s.revokeAllTx(ctx, tx, userID, nil, "account_"+status); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "user.status_set", ResourceType: "user", ResourceID: userID.String(),
			Old: map[string]any{"status": cur.Status}, New: map[string]any{"status": status, "reason": reason}})
	})
	InvalidateAllSessionCache()
	_ = db.NotifyConfig(ctx, s.db.Pool, "identity")
	return err
}

// BootstrapSuperAdmin grants super_admin to an existing verified user. Operator CLI only.
func (s *Service) BootstrapSuperAdmin(ctx context.Context, email string) (*User, error) {
	u, err := s.FindUserByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	if u.Status != "active" {
		return nil, apperr.ValidationMsg("bootstrap.user_not_active", "The account must have a verified email and be active.")
	}
	err = s.db.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO user_roles (user_id, role_code) VALUES ($1,'super_admin') ON CONFLICT DO NOTHING`, u.ID); err != nil {
			return err
		}
		return audit.Record(principal.With(ctx, principal.System()), tx, audit.Entry{Action: "user.bootstrap_super_admin", ResourceType: "user", ResourceID: u.ID.String()})
	})
	if err != nil {
		return nil, err
	}
	InvalidateAllSessionCache()
	_ = db.NotifyConfig(ctx, s.db.Pool, "identity")
	return s.GetUser(ctx, u.ID)
}

// CleanupExpired removes expired challenges, tokens and dev mail (TTL cleanup job).
func (s *Service) CleanupExpired(ctx context.Context) (map[string]int64, error) {
	stmts := map[string]string{
		"email_challenges": `DELETE FROM email_verification_challenges WHERE expires_at < now() - interval '7 days'`,
		"reset_challenges": `DELETE FROM password_reset_challenges WHERE expires_at < now() - interval '7 days'`,
		"refresh_tokens":   `DELETE FROM auth_refresh_tokens WHERE expires_at < now() - interval '7 days'`,
		"sessions":         `DELETE FROM auth_sessions WHERE expires_at < now() - interval '30 days'`,
		"security_events":  `DELETE FROM user_security_events WHERE created_at < now() - interval '365 days'`,
		"dev_mail":         `DELETE FROM dev_mail_messages WHERE created_at < now() - interval '2 days'`,
		"email_deliveries": `DELETE FROM email_deliveries WHERE created_at < now() - interval '90 days' AND status IN ('sent','dead','expired')`,
		"chat_sessions":    `DELETE FROM chat_sessions WHERE expires_at < now()`,
		"consumer_dedupe":  `DELETE FROM consumer_dedupe WHERE processed_at < now() - interval '30 days'`,
	}
	out := map[string]int64{}
	for k, q := range stmts {
		tag, err := s.db.Pool.Exec(ctx, q)
		if err != nil {
			return out, fmt.Errorf("cleanup %s: %w", k, err)
		}
		out[k] = tag.RowsAffected()
	}
	return out, nil
}

// CreateOperatorAdmin creates (or promotes) a verified super_admin account. Operator CLI only
// (`newsctl create-admin`), used to bootstrap a fresh installation without email round-trips.
// The password must satisfy the normal password policy.
func (s *Service) CreateOperatorAdmin(ctx context.Context, email, password, phone, first, last string) (*User, error) {
	addr, norm, ok := NormalizeEmail(email)
	if !ok {
		return nil, apperr.ValidationMsg("bootstrap.invalid_email", "A valid email address is required.")
	}
	ph, ok := NormalizePhone(phone, "MD")
	if !ok {
		return nil, apperr.ValidationMsg("bootstrap.invalid_phone", "A valid phone number is required (e.g. +37369123456).")
	}
	if code, msg, ok := ValidatePassword(password, addr); !ok {
		return nil, apperr.Validation(apperr.FieldError{Field: "password", Code: code, Message: msg})
	}
	if first == "" {
		first = "Admin"
	}
	if last == "" {
		last = "Operator"
	}
	hash := HashPassword(password, s.argon)
	var id uuid.UUID
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO users (id, first_name, last_name, email, email_normalized, phone_e164, password_hash, status, email_verified_at, locale, timezone)
			VALUES ($1,$2,$3,$4,$5,$6,$7,'active',now(),'ro','Europe/Chisinau')
			ON CONFLICT (email_normalized) DO UPDATE SET status='active', email_verified_at=COALESCE(users.email_verified_at, now())
			RETURNING id`, ids.New(), first, last, addr, norm, ph, hash).Scan(&id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO user_roles (user_id, role_code) VALUES ($1,'user'),($1,'super_admin') ON CONFLICT DO NOTHING`, id); err != nil {
			return err
		}
		return audit.Record(principal.With(ctx, principal.System()), tx, audit.Entry{Action: "user.operator_admin_created", ResourceType: "user", ResourceID: id.String()})
	})
	if err != nil {
		return nil, err
	}
	InvalidateAllSessionCache()
	_ = db.NotifyConfig(ctx, s.db.Pool, "identity")
	return s.GetUser(ctx, id)
}
