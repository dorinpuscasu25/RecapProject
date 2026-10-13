// Package sources manages publishers, source endpoints, extraction profiles, usage permissions,
// crawl runs, health and URL blocklists — all database-managed via authenticated admin APIs.
package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dorinpuscasu25/recapproject/internal/audit"
	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ids"
	"github.com/dorinpuscasu25/recapproject/internal/platform/principal"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ssrf"
)

type Service struct {
	db     *db.DB
	policy ssrf.Policy
}

func NewService(d *db.DB, policy ssrf.Policy) *Service { return &Service{db: d, policy: policy} }

func (s *Service) SSRFPolicy() ssrf.Policy { return s.policy }

func actor(ctx context.Context) *uuid.UUID {
	if p := principal.From(ctx); p != nil && p.Kind == principal.KindUser {
		return &p.ID
	}
	return nil
}

// ============ Publishers ============

type Publisher struct {
	ID               uuid.UUID      `json:"id"`
	Name             string         `json:"name"`
	CanonicalDomain  string         `json:"canonical_domain"`
	Country          string         `json:"country"`
	PrimaryLanguages []string       `json:"primary_languages"`
	PublisherType    string         `json:"publisher_type"`
	Metadata         map[string]any `json:"metadata"`
	Status           string         `json:"status"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
	Version          int            `json:"version"`
}

type PublisherInput struct {
	Name             *string        `json:"name,omitempty"`
	CanonicalDomain  *string        `json:"canonical_domain,omitempty"`
	Country          *string        `json:"country,omitempty"`
	PrimaryLanguages []string       `json:"primary_languages,omitempty"`
	PublisherType    *string        `json:"publisher_type,omitempty"`
	Metadata         map[string]any `json:"metadata,omitempty"`
	Status           *string        `json:"status,omitempty"`
	Version          *int           `json:"version,omitempty"`
}

const pubCols = `id, name, canonical_domain, country, primary_languages, publisher_type, metadata, status, created_at, updated_at, version`

func scanPublisher(row pgx.Row) (*Publisher, error) {
	var p Publisher
	var meta []byte
	if err := row.Scan(&p.ID, &p.Name, &p.CanonicalDomain, &p.Country, &p.PrimaryLanguages, &p.PublisherType, &meta, &p.Status,
		&p.CreatedAt, &p.UpdatedAt, &p.Version); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(meta, &p.Metadata)
	return &p, nil
}

var domainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*\.[a-z]{2,}$`)
var langRe = regexp.MustCompile(`^[a-z]{2}$`)

var publisherTypes = map[string]bool{"newsroom": true, "government": true, "public_institution": true, "fact_checker": true, "other": true}
var publisherStatuses = map[string]bool{"active": true, "paused": true, "disabled": true, "archived": true, "pending_permission": true}

func (p *Publisher) apply(in PublisherInput) {
	if in.Name != nil {
		p.Name = strings.TrimSpace(*in.Name)
	}
	if in.CanonicalDomain != nil {
		d := strings.ToLower(strings.TrimSpace(*in.CanonicalDomain))
		d = strings.TrimPrefix(strings.TrimPrefix(d, "https://"), "http://")
		d = strings.TrimPrefix(strings.TrimSuffix(d, "/"), "www.")
		p.CanonicalDomain = d
	}
	if in.Country != nil {
		p.Country = strings.ToUpper(strings.TrimSpace(*in.Country))
	}
	if in.PrimaryLanguages != nil {
		p.PrimaryLanguages = in.PrimaryLanguages
	}
	if in.PublisherType != nil {
		p.PublisherType = *in.PublisherType
	}
	if in.Metadata != nil {
		p.Metadata = in.Metadata
	}
	if in.Status != nil {
		p.Status = *in.Status
	}
}

func (p *Publisher) validate() error {
	var v apperr.Validator
	v.Check(p.Name != "" && len(p.Name) <= 200, "name", "invalid", "Name is required (max 200 chars).")
	v.Check(domainRe.MatchString(p.CanonicalDomain), "canonical_domain", "invalid", "A valid domain like example.md is required.")
	v.Check(regexp.MustCompile(`^[A-Z]{2}$`).MatchString(p.Country), "country", "invalid", "Country must be an ISO 3166-1 alpha-2 code.")
	v.Check(publisherTypes[p.PublisherType], "publisher_type", "invalid", "Invalid publisher type.")
	v.Check(publisherStatuses[p.Status], "status", "invalid", "Invalid status.")
	for _, l := range p.PrimaryLanguages {
		v.Check(langRe.MatchString(l), "primary_languages", "invalid", "Languages must be ISO 639-1 codes.")
	}
	return v.Err()
}

func (s *Service) CreatePublisher(ctx context.Context, in PublisherInput) (*Publisher, error) {
	p := &Publisher{ID: ids.New(), Country: "MD", PrimaryLanguages: []string{"ro"}, Status: "pending_permission", Metadata: map[string]any{}}
	p.apply(in)
	if err := p.validate(); err != nil {
		return nil, err
	}
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		meta, _ := json.Marshal(p.Metadata)
		_, err := tx.Exec(ctx, `INSERT INTO publishers (id, name, canonical_domain, country, primary_languages, publisher_type, metadata, status, created_by, updated_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$9)`, p.ID, p.Name, p.CanonicalDomain, p.Country, p.PrimaryLanguages, p.PublisherType, meta, p.Status, actor(ctx))
		if db.IsUniqueViolation(err) {
			return apperr.Conflict("publisher_domain_taken", "A publisher with this domain already exists.")
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "publisher.create", ResourceType: "publisher", ResourceID: p.ID.String(), New: p})
	})
	if err != nil {
		return nil, err
	}
	return s.GetPublisher(ctx, p.ID)
}

func (s *Service) GetPublisher(ctx context.Context, id uuid.UUID) (*Publisher, error) {
	p, err := scanPublisher(s.db.Pool.QueryRow(ctx, `SELECT `+pubCols+` FROM publishers WHERE id=$1`, id))
	if db.IsNoRows(err) {
		return nil, apperr.NotFound("publisher_not_found", "Publisher not found.")
	}
	return p, err
}

func (s *Service) ListPublishers(ctx context.Context, status, q string, limit int) ([]Publisher, error) {
	rows, err := s.db.Pool.Query(ctx, `SELECT `+pubCols+` FROM publishers
		WHERE ($1='' OR status=$1) AND ($2='' OR name ILIKE '%'||$2||'%' OR canonical_domain ILIKE '%'||$2||'%')
		ORDER BY name LIMIT $3`, status, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Publisher
	for rows.Next() {
		p, err := scanPublisher(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (s *Service) UpdatePublisher(ctx context.Context, id uuid.UUID, in PublisherInput) (*Publisher, error) {
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		cur, err := scanPublisher(tx.QueryRow(ctx, `SELECT `+pubCols+` FROM publishers WHERE id=$1 FOR UPDATE`, id))
		if db.IsNoRows(err) {
			return apperr.NotFound("publisher_not_found", "Publisher not found.")
		}
		if err != nil {
			return err
		}
		if in.Version != nil && *in.Version != cur.Version {
			return apperr.VersionConflict("publisher")
		}
		old := *cur
		cur.apply(in)
		if err := cur.validate(); err != nil {
			return err
		}
		meta, _ := json.Marshal(cur.Metadata)
		_, err = tx.Exec(ctx, `UPDATE publishers SET name=$2, canonical_domain=$3, country=$4, primary_languages=$5, publisher_type=$6,
			metadata=$7, status=$8, updated_by=$9, updated_at=now(), version=version+1 WHERE id=$1`,
			id, cur.Name, cur.CanonicalDomain, cur.Country, cur.PrimaryLanguages, cur.PublisherType, meta, cur.Status, actor(ctx))
		if db.IsUniqueViolation(err) {
			return apperr.Conflict("publisher_domain_taken", "A publisher with this domain already exists.")
		}
		if err != nil {
			return err
		}
		// Disabling/archiving a publisher pauses its endpoints.
		if cur.Status == "disabled" || cur.Status == "archived" || cur.Status == "pending_permission" {
			if _, err := tx.Exec(ctx, `UPDATE source_endpoints SET status = CASE WHEN status='archived' THEN status ELSE 'paused' END,
				updated_at=now(), version=version+1 WHERE publisher_id=$1 AND status='enabled'`, id); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "publisher.update", ResourceType: "publisher", ResourceID: id.String(), Old: old, New: cur})
	})
	if err != nil {
		return nil, err
	}
	return s.GetPublisher(ctx, id)
}

// ============ Permissions ============

type Permission struct {
	ID                 uuid.UUID  `json:"id"`
	PublisherID        uuid.UUID  `json:"publisher_id"`
	SourceEndpointID   *uuid.UUID `json:"source_endpoint_id,omitempty"`
	LegalBasis         string     `json:"legal_basis"`
	PermissionStatus   string     `json:"permission_status"`
	AllowCrawl         bool       `json:"allow_crawl"`
	AllowStoreFullText bool       `json:"allow_store_full_text"`
	AllowStoreRaw      bool       `json:"allow_store_raw"`
	AllowSummarize     bool       `json:"allow_summarize"`
	AllowEmbed         bool       `json:"allow_embed"`
	AllowAudio         bool       `json:"allow_audio"`
	AllowSnippets      bool       `json:"allow_snippets"`
	MaxSnippetChars    int        `json:"max_snippet_chars"`
	RobotsReviewStatus string     `json:"robots_review_status"`
	TermsReviewStatus  string     `json:"terms_review_status"`
	RetentionDays      *int       `json:"retention_days,omitempty"`
	AttributionText    string     `json:"attribution_text"`
	ReviewedAt         *time.Time `json:"reviewed_at,omitempty"`
	ReviewedBy         *uuid.UUID `json:"reviewed_by,omitempty"`
	Notes              string     `json:"notes"`
	Version            int        `json:"version"`
}

// Effective returns true when content may be crawled at all.
func (p *Permission) CrawlAllowed() bool {
	return p != nil && p.PermissionStatus == "granted" && p.AllowCrawl &&
		p.RobotsReviewStatus != "blocked" && p.TermsReviewStatus != "blocked"
}

const permCols = `id, publisher_id, source_endpoint_id, legal_basis, permission_status, allow_crawl, allow_store_full_text, allow_store_raw,
	allow_summarize, allow_embed, allow_audio, allow_snippets, max_snippet_chars, robots_review_status, terms_review_status, retention_days,
	attribution_text, reviewed_at, reviewed_by, notes, version`

func scanPerm(row pgx.Row) (*Permission, error) {
	var p Permission
	err := row.Scan(&p.ID, &p.PublisherID, &p.SourceEndpointID, &p.LegalBasis, &p.PermissionStatus, &p.AllowCrawl, &p.AllowStoreFullText,
		&p.AllowStoreRaw, &p.AllowSummarize, &p.AllowEmbed, &p.AllowAudio, &p.AllowSnippets, &p.MaxSnippetChars, &p.RobotsReviewStatus,
		&p.TermsReviewStatus, &p.RetentionDays, &p.AttributionText, &p.ReviewedAt, &p.ReviewedBy, &p.Notes, &p.Version)
	return &p, err
}

// EffectivePermission returns the endpoint-specific permission if present, otherwise the
// publisher-level one. Nil means no permission recorded (treated as denied).
func (s *Service) EffectivePermission(ctx context.Context, q db.Querier, publisherID uuid.UUID, endpointID *uuid.UUID) (*Permission, error) {
	p, err := scanPerm(q.QueryRow(ctx, `SELECT `+permCols+` FROM source_permissions
		WHERE publisher_id=$1 AND (source_endpoint_id=$2 OR source_endpoint_id IS NULL)
		ORDER BY source_endpoint_id NULLS LAST LIMIT 1`, publisherID, endpointID))
	if db.IsNoRows(err) {
		return nil, nil
	}
	return p, err
}

func (s *Service) ListPermissions(ctx context.Context, publisherID uuid.UUID) ([]Permission, error) {
	rows, err := s.db.Pool.Query(ctx, `SELECT `+permCols+` FROM source_permissions WHERE publisher_id=$1 ORDER BY source_endpoint_id NULLS FIRST`, publisherID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Permission
	for rows.Next() {
		p, err := scanPerm(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

type PermissionInput struct {
	SourceEndpointID   *uuid.UUID `json:"source_endpoint_id,omitempty"`
	LegalBasis         string     `json:"legal_basis"`
	PermissionStatus   string     `json:"permission_status"`
	AllowCrawl         bool       `json:"allow_crawl"`
	AllowStoreFullText bool       `json:"allow_store_full_text"`
	AllowStoreRaw      bool       `json:"allow_store_raw"`
	AllowSummarize     bool       `json:"allow_summarize"`
	AllowEmbed         bool       `json:"allow_embed"`
	AllowAudio         bool       `json:"allow_audio"`
	AllowSnippets      bool       `json:"allow_snippets"`
	MaxSnippetChars    *int       `json:"max_snippet_chars,omitempty"`
	RobotsReviewStatus string     `json:"robots_review_status"`
	TermsReviewStatus  string     `json:"terms_review_status"`
	RetentionDays      *int       `json:"retention_days,omitempty"`
	AttributionText    string     `json:"attribution_text"`
	Notes              string     `json:"notes"`
}

var permStatuses = map[string]bool{"pending": true, "granted": true, "restricted": true, "denied": true, "revoked": true}
var reviewStatuses = map[string]bool{"unreviewed": true, "compliant": true, "restricted": true, "blocked": true}

// UpsertPermission records a permission review for a publisher (or one endpoint).
func (s *Service) UpsertPermission(ctx context.Context, publisherID uuid.UUID, in PermissionInput) (*Permission, error) {
	if in.RobotsReviewStatus == "" {
		in.RobotsReviewStatus = "unreviewed"
	}
	if in.TermsReviewStatus == "" {
		in.TermsReviewStatus = "unreviewed"
	}
	var v apperr.Validator
	v.Check(permStatuses[in.PermissionStatus], "permission_status", "invalid", "Invalid permission status.")
	v.Check(reviewStatuses[in.RobotsReviewStatus], "robots_review_status", "invalid", "Invalid review status.")
	v.Check(reviewStatuses[in.TermsReviewStatus], "terms_review_status", "invalid", "Invalid review status.")
	v.Check(in.RetentionDays == nil || *in.RetentionDays > 0, "retention_days", "invalid", "Retention must be positive.")
	v.Check(!(in.AllowStoreRaw && !in.AllowCrawl), "allow_store_raw", "inconsistent", "Storing raw content requires crawl permission.")
	if in.MaxSnippetChars != nil {
		v.Check(*in.MaxSnippetChars >= 0 && *in.MaxSnippetChars <= 2000, "max_snippet_chars", "out_of_range", "0..2000")
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	snip := 300
	if in.MaxSnippetChars != nil {
		snip = *in.MaxSnippetChars
	}
	var out *Permission
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := s.getPublisherTx(ctx, tx, publisherID); err != nil {
			return err
		}
		if in.SourceEndpointID != nil {
			var pub uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT publisher_id FROM source_endpoints WHERE id=$1`, *in.SourceEndpointID).Scan(&pub); err != nil || pub != publisherID {
				return apperr.ValidationMsg("permission_endpoint_mismatch", "The endpoint does not belong to this publisher.")
			}
		}
		old, _ := scanPerm(tx.QueryRow(ctx, `SELECT `+permCols+` FROM source_permissions WHERE publisher_id=$1 AND source_endpoint_id IS NOT DISTINCT FROM $2`, publisherID, in.SourceEndpointID))
		var id uuid.UUID
		err := tx.QueryRow(ctx, `INSERT INTO source_permissions (id, publisher_id, source_endpoint_id, legal_basis, permission_status, allow_crawl,
			allow_store_full_text, allow_store_raw, allow_summarize, allow_embed, allow_audio, allow_snippets, max_snippet_chars,
			robots_review_status, terms_review_status, retention_days, attribution_text, reviewed_at, reviewed_by, notes)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,now(),$18,$19)
			ON CONFLICT (publisher_id, coalesce(source_endpoint_id, '00000000-0000-0000-0000-000000000000'::uuid)) DO UPDATE SET
			legal_basis=EXCLUDED.legal_basis, permission_status=EXCLUDED.permission_status, allow_crawl=EXCLUDED.allow_crawl,
			allow_store_full_text=EXCLUDED.allow_store_full_text, allow_store_raw=EXCLUDED.allow_store_raw, allow_summarize=EXCLUDED.allow_summarize,
			allow_embed=EXCLUDED.allow_embed, allow_audio=EXCLUDED.allow_audio, allow_snippets=EXCLUDED.allow_snippets,
			max_snippet_chars=EXCLUDED.max_snippet_chars, robots_review_status=EXCLUDED.robots_review_status,
			terms_review_status=EXCLUDED.terms_review_status, retention_days=EXCLUDED.retention_days, attribution_text=EXCLUDED.attribution_text,
			reviewed_at=now(), reviewed_by=EXCLUDED.reviewed_by, notes=EXCLUDED.notes, updated_at=now(), version=source_permissions.version+1
			RETURNING id`,
			ids.New(), publisherID, in.SourceEndpointID, in.LegalBasis, in.PermissionStatus, in.AllowCrawl, in.AllowStoreFullText, in.AllowStoreRaw,
			in.AllowSummarize, in.AllowEmbed, in.AllowAudio, in.AllowSnippets, snip, in.RobotsReviewStatus, in.TermsReviewStatus, in.RetentionDays,
			in.AttributionText, actor(ctx), in.Notes).Scan(&id)
		if err != nil {
			return err
		}
		out, err = scanPerm(tx.QueryRow(ctx, `SELECT `+permCols+` FROM source_permissions WHERE id=$1`, id))
		if err != nil {
			return err
		}
		// Revoking/denying crawl permission immediately pauses affected endpoints.
		if !out.CrawlAllowed() {
			if _, err := tx.Exec(ctx, `UPDATE source_endpoints SET status='pending_permission', updated_at=now(), version=version+1
				WHERE publisher_id=$1 AND ($2::uuid IS NULL OR id=$2) AND status='enabled'`, publisherID, in.SourceEndpointID); err != nil {
				return err
			}
		}
		var oldV any
		if old != nil && old.ID != uuid.Nil {
			oldV = old
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "source_permission.upsert", ResourceType: "publisher", ResourceID: publisherID.String(), Old: oldV, New: out})
	})
	return out, err
}

func (s *Service) getPublisherTx(ctx context.Context, q db.Querier, id uuid.UUID) (*Publisher, error) {
	p, err := scanPublisher(q.QueryRow(ctx, `SELECT `+pubCols+` FROM publishers WHERE id=$1`, id))
	if db.IsNoRows(err) {
		return nil, apperr.NotFound("publisher_not_found", "Publisher not found.")
	}
	return p, err
}

// ============ Blocklist ============

type BlockedURL struct {
	ID        uuid.UUID `json:"id"`
	MatchType string    `json:"match_type"`
	Value     string    `json:"value"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Service) AddBlocked(ctx context.Context, matchType, value, reason string) (*BlockedURL, error) {
	value = strings.TrimSpace(value)
	switch matchType {
	case "domain":
		value = strings.ToLower(strings.TrimPrefix(value, "www."))
		if !domainRe.MatchString(value) {
			return nil, apperr.ValidationMsg("blocked_invalid", "Invalid domain.")
		}
	case "url", "prefix":
		if _, err := url.ParseRequestURI(value); err != nil {
			return nil, apperr.ValidationMsg("blocked_invalid", "Invalid URL.")
		}
	default:
		return nil, apperr.ValidationMsg("blocked_invalid", "match_type must be url, prefix or domain.")
	}
	b := &BlockedURL{ID: ids.New(), MatchType: matchType, Value: value, Reason: reason, CreatedAt: time.Now().UTC()}
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO blocked_urls (id, match_type, value, reason, created_by) VALUES ($1,$2,$3,$4,$5)`,
			b.ID, matchType, value, reason, actor(ctx))
		if db.IsUniqueViolation(err) {
			return apperr.Conflict("blocked_exists", "This entry already exists.")
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "blocklist.add", ResourceType: "blocked_url", ResourceID: b.ID.String(), New: b})
	})
	return b, err
}

func (s *Service) ListBlocked(ctx context.Context) ([]BlockedURL, error) {
	rows, err := s.db.Pool.Query(ctx, `SELECT id, match_type, value, reason, created_at FROM blocked_urls ORDER BY created_at DESC LIMIT 1000`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (BlockedURL, error) {
		var b BlockedURL
		err := r.Scan(&b.ID, &b.MatchType, &b.Value, &b.Reason, &b.CreatedAt)
		return b, err
	})
}

func (s *Service) DeleteBlocked(ctx context.Context, id uuid.UUID) error {
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM blocked_urls WHERE id=$1`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return apperr.NotFound("blocked_not_found", "Entry not found.")
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "blocklist.remove", ResourceType: "blocked_url", ResourceID: id.String()})
	})
}

// IsBlocked checks a URL against the global blocklist and an endpoint-level blocklist.
func (s *Service) IsBlocked(ctx context.Context, raw string, endpointBlocklist []string) (bool, string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return true, "unparsable", nil
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	for _, pat := range endpointBlocklist {
		if pat != "" && (strings.HasPrefix(raw, pat) || host == strings.ToLower(pat)) {
			return true, "endpoint_blocklist", nil
		}
	}
	var hit bool
	err = s.db.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM blocked_urls WHERE
		(match_type='url' AND value=$1) OR (match_type='prefix' AND $1 LIKE value || '%') OR
		(match_type='domain' AND ($2 = value OR $2 LIKE '%.' || value)))`, raw, host).Scan(&hit)
	if hit {
		return true, "global_blocklist", err
	}
	return false, "", err
}

func jsonOrEmpty(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return []byte("{}")
	}
	return b
}

func fmtInterval(d time.Duration) string { return fmt.Sprintf("%d seconds", int(d.Seconds())) }
