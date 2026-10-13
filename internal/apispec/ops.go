package apispec

import (
	"encoding/json"
	"reflect"
	"time"

	"github.com/google/uuid"

	"github.com/dorinpuscasu25/recapproject/internal/ai"
	"github.com/dorinpuscasu25/recapproject/internal/api"
	"github.com/dorinpuscasu25/recapproject/internal/audit"
	"github.com/dorinpuscasu25/recapproject/internal/briefings"
	"github.com/dorinpuscasu25/recapproject/internal/chat"
	"github.com/dorinpuscasu25/recapproject/internal/eval"
	"github.com/dorinpuscasu25/recapproject/internal/events"
	"github.com/dorinpuscasu25/recapproject/internal/factcheck"
	"github.com/dorinpuscasu25/recapproject/internal/feed"
	"github.com/dorinpuscasu25/recapproject/internal/identity"
	"github.com/dorinpuscasu25/recapproject/internal/ingest"
	"github.com/dorinpuscasu25/recapproject/internal/jobs"
	"github.com/dorinpuscasu25/recapproject/internal/notifications"
	"github.com/dorinpuscasu25/recapproject/internal/policy"
	"github.com/dorinpuscasu25/recapproject/internal/search"
	"github.com/dorinpuscasu25/recapproject/internal/sources"
	"github.com/dorinpuscasu25/recapproject/internal/taxonomy"
)

// Access levels.
const (
	Public   = "public"   // no credentials (identity bootstrap, health)
	Refresh  = "refresh"  // refresh credential only
	Self     = "self"     // verified human account with me:manage
	Consumer = "consumer" // verified human account with news:read
	Staff    = "staff"    // permissions listed in Perms
	Dev      = "dev"      // development-only, never mounted in production
)

// Op documents one operation. Req/Resp are Go values (or reflect.Type) whose JSON shape is the contract.
type Op struct {
	Method, Path, Tag, Summary string
	Access                     string
	Perms                      []string // all required, outermost first
	Req                        any
	Resp                       any
	Status                     int  // success status (default 200)
	List                       bool // response is {"items": [...], "next_cursor"?: string}
	Query                      []string
	Conflict                   bool // 409 on optimistic-concurrency/state conflicts
	Notes                      string
}

// ret returns the first result type of a function or method expression (pointer stripped).
func ret(fn any) reflect.Type {
	t := reflect.TypeOf(fn).Out(0)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// elem returns the element type of a slice-returning function.
func elem(fn any) reflect.Type {
	t := ret(fn)
	if t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// ---- Shapes of handler-local request/response bodies (kept in sync by the route contract test) ----

type AuthResponse struct {
	identity.TokenPair
	User identity.User `json:"user"`
}
type Accepted struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}
type JobAccepted struct {
	JobID     uuid.UUID `json:"job_id"`
	Status    string    `json:"status"`
	StatusURL string    `json:"status_url"`
	DryRun    *bool     `json:"dry_run,omitempty"`
}
type CrawlAccepted struct {
	CrawlRunID uuid.UUID `json:"crawl_run_id"`
	Status     string    `json:"status"`
	StatusURL  string    `json:"status_url"`
}
type RolesRequest struct {
	Roles []string `json:"roles"`
}
type StatusRequest struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}
type ReasonRequest struct {
	Reason string `json:"reason"`
}
type ReparentRequest struct {
	ParentID *uuid.UUID `json:"parent_id"`
	Version  *int       `json:"version,omitempty"`
}
type ReorderRequest struct {
	ParentID *uuid.UUID  `json:"parent_id"`
	Ordered  []uuid.UUID `json:"ordered_ids"`
}
type MergeCategoryRequest struct {
	Into   uuid.UUID `json:"into_category_id"`
	Reason string    `json:"reason"`
}
type CategoryTree struct {
	TaxonomyVersion int64                     `json:"taxonomy_version"`
	Items           []taxonomy.PublicCategory `json:"items"`
}
type AdminCategoryTree struct {
	TaxonomyVersion int64               `json:"taxonomy_version"`
	Items           []taxonomy.Category `json:"items"`
}
type PreviewURLRequest struct {
	URL       string     `json:"url"`
	ProfileID *uuid.UUID `json:"extraction_profile_id,omitempty"`
}
type CrawlRequest struct {
	MaxItems int `json:"max_items"`
}
type BlockedURLRequest struct {
	MatchType string `json:"match_type"`
	Value     string `json:"value"`
	Reason    string `json:"reason"`
}
type ProfileValidation struct {
	Total   int              `json:"total"`
	Passed  int              `json:"passed"`
	Results []map[string]any `json:"results"`
}
type DeadLetter struct {
	ID         uuid.UUID  `json:"id"`
	Consumer   string     `json:"consumer"`
	MessageID  uuid.UUID  `json:"message_id"`
	Topic      string     `json:"topic"`
	EventType  string     `json:"event_type"`
	Error      string     `json:"error"`
	Attempts   int        `json:"attempts"`
	CreatedAt  time.Time  `json:"created_at"`
	ReplayedAt *time.Time `json:"replayed_at,omitempty"`
}
type WithdrawRequest struct {
	Reason     string `json:"reason"`
	LegalBasis string `json:"legal_basis"`
}
type WithdrawResult struct {
	Status         string      `json:"status"`
	AffectedEvents []uuid.UUID `json:"affected_events"`
}
type ArticleCategoriesRequest struct {
	CategoryIDs []uuid.UUID `json:"category_ids"`
	PrimaryID   *uuid.UUID  `json:"primary_category_id"`
}
type AdminEvent struct {
	Event    events.Event           `json:"event"`
	Versions []map[string]any       `json:"versions"`
	Timeline []events.TimelineEntry `json:"timeline"`
	Links    []map[string]any       `json:"links"`
}
type MergeEventsRequest struct {
	SourceEventIDs []uuid.UUID `json:"source_event_ids"`
	Reason         string      `json:"reason"`
}
type MergeEventsResult struct {
	MergeHistoryID uuid.UUID `json:"merge_history_id"`
	EventID        uuid.UUID `json:"event_id"`
}
type SplitEventRequest struct {
	ArticleIDs []uuid.UUID `json:"article_ids"`
	Reason     string      `json:"reason"`
}
type SplitEventResult struct {
	NewEventID uuid.UUID `json:"new_event_id"`
}
type EvalRunResults struct {
	RunID   uuid.UUID         `json:"run_id"`
	Results []eval.ResultView `json:"results"`
}
type ReindexRequest struct {
	DocTypes []string `json:"doc_types"`
}
type PolicyRequest struct {
	Value   json.RawMessage `json:"value"`
	Version int             `json:"version"`
}
type FlagRequest struct {
	Enabled        bool `json:"enabled"`
	RolloutPercent int  `json:"rollout_percent"`
}
type FactCheckDetail struct {
	FactCheck factcheck.FactCheck      `json:"fact_check"`
	History   []factcheck.HistoryEntry `json:"history"`
}
type NarrativeCreated struct {
	ID     uuid.UUID `json:"id"`
	Status string    `json:"status"`
}
type CategoriesRequest struct {
	CategoryIDs []uuid.UUID `json:"category_ids"`
}
type SeenRequest struct {
	Version *int `json:"version"`
}
type AcknowledgeRequest struct {
	At *string `json:"acknowledged_at"`
}
type Acknowledged struct {
	AcknowledgedAt time.Time `json:"acknowledged_at"`
}
type AudioRequest struct {
	Voice string `json:"voice,omitempty"`
}
type AskRequest struct {
	Content string `json:"content"`
	Level   string `json:"level,omitempty"`
}
type Health struct {
	Status string `json:"status"`
}
type Readiness struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

var (
	listQ    = []string{"limit"}
	langQ    = []string{"lang"}
	pageQ    = []string{"lang", "limit", "cursor"}
	sources_ = "Sources & publishers"
)

// Operations is the documented API surface.
func Operations() []Op {
	return []Op{
		// ---- health ----
		{Method: "GET", Path: "/healthz", Tag: "Health", Summary: "Liveness probe", Access: Public, Resp: Health{}},
		{Method: "GET", Path: "/readyz", Tag: "Health", Summary: "Readiness probe (dependencies)", Access: Public, Resp: Readiness{}, Notes: "503 when PostgreSQL is unreachable."},
		{Method: "GET", Path: "/openapi.yaml", Tag: "Health", Summary: "This contract", Access: Public, Resp: nil},

		// ---- identity (public) ----
		{Method: "POST", Path: "/v1/auth/register", Tag: "Auth", Summary: "Register with email and password; a 6-digit code is emailed", Access: Public,
			Req: identity.RegisterInput{}, Resp: ret((*identity.Service).Register), Status: 202, Notes: "Responses do not reveal whether an email is already registered."},
		{Method: "POST", Path: "/v1/auth/verify-email", Tag: "Auth", Summary: "Verify the emailed code; returns tokens", Access: Public, Req: identity.VerifyInput{}, Resp: AuthResponse{},
			Notes: "Codes expire after 10 minutes; 5 wrong attempts invalidate the challenge."},
		{Method: "POST", Path: "/v1/auth/resend-verification", Tag: "Auth", Summary: "Resend the verification code (rate limited)", Access: Public, Req: identity.ResendInput{}, Resp: Accepted{}, Status: 202},
		{Method: "POST", Path: "/v1/auth/login", Tag: "Auth", Summary: "Log in (verified accounts only)", Access: Public, Req: identity.LoginInput{}, Resp: AuthResponse{}},
		{Method: "POST", Path: "/v1/auth/refresh", Tag: "Auth", Summary: "Rotate the refresh token", Access: Refresh, Req: identity.RefreshInput{}, Resp: identity.TokenPair{},
			Notes: "Reuse of a rotated refresh token revokes the whole token family."},
		{Method: "POST", Path: "/v1/auth/forgot-password", Tag: "Auth", Summary: "Request a password reset code", Access: Public, Req: identity.ForgotInput{}, Resp: Accepted{}, Status: 202},
		{Method: "POST", Path: "/v1/auth/reset-password", Tag: "Auth", Summary: "Reset the password with the emailed code", Access: Public, Req: identity.ResetInput{}, Status: 204},
		{Method: "GET", Path: "/v1/auth/jwks", Tag: "Auth", Summary: "Public signing keys (EdDSA)", Access: Public, Resp: map[string]any{}},
		{Method: "GET", Path: "/v1/dev/mailbox", Tag: "Development", Summary: "Development mail sink (never mounted in production)", Access: Dev, Resp: map[string]any{}, List: true, Query: []string{"to"}},

		// ---- self-service ----
		{Method: "POST", Path: "/v1/auth/logout", Tag: "Account", Summary: "Revoke the current session", Access: Self, Status: 204},
		{Method: "POST", Path: "/v1/auth/logout-all", Tag: "Account", Summary: "Revoke all sessions", Access: Self, Status: 204},
		{Method: "POST", Path: "/v1/auth/change-password", Tag: "Account", Summary: "Change password (revokes other sessions)", Access: Self, Req: identity.ChangePasswordInput{}, Status: 204},
		{Method: "GET", Path: "/v1/users/me", Tag: "Account", Summary: "Current user", Access: Self, Resp: identity.User{}},
		{Method: "PATCH", Path: "/v1/users/me", Tag: "Account", Summary: "Update profile", Access: Self, Req: identity.UpdateMeInput{}, Resp: identity.User{}, Conflict: true},
		{Method: "GET", Path: "/v1/users/me/sessions", Tag: "Account", Summary: "Active sessions", Access: Self, Resp: elem((*identity.Service).ListSessions), List: true},
		{Method: "DELETE", Path: "/v1/users/me/sessions/{id}", Tag: "Account", Summary: "Revoke one session", Access: Self, Status: 204},
		{Method: "POST", Path: "/v1/users/me/email-change", Tag: "Account", Summary: "Start an email change (code sent to the new address)", Access: Self, Req: identity.EmailChangeInput{}, Resp: Accepted{}, Status: 202},
		{Method: "POST", Path: "/v1/users/me/email-change/confirm", Tag: "Account", Summary: "Confirm an email change", Access: Self, Req: identity.EmailChangeConfirmInput{}, Resp: identity.User{}},
		{Method: "POST", Path: "/v1/users/me/data-export", Tag: "Account", Summary: "Request a personal data export", Access: Self, Resp: ret((*identity.Service).RequestExport), Status: 202},
		{Method: "GET", Path: "/v1/users/me/data-requests", Tag: "Account", Summary: "Export/deletion requests", Access: Self, Resp: elem((*identity.Service).ListDataRequests), List: true},
		{Method: "POST", Path: "/v1/users/me/deletion", Tag: "Account", Summary: "Request account deletion", Access: Self, Req: identity.DeletionInput{}, Resp: ret((*identity.Service).RequestDeletion), Status: 202},

		// ---- consumer: taxonomy, events, search ----
		{Method: "GET", Path: "/v1/categories", Tag: "Categories", Summary: "Active categories (flat)", Access: Consumer, Resp: taxonomy.PublicCategory{}, List: true, Query: langQ},
		{Method: "GET", Path: "/v1/categories/tree", Tag: "Categories", Summary: "Active category tree", Access: Consumer, Resp: CategoryTree{}, Query: langQ},
		{Method: "GET", Path: "/v1/events", Tag: "Events", Summary: "Events (newest first)", Access: Consumer, Resp: events.Event{}, List: true,
			Query: []string{"lang", "limit", "cursor", "category_id", "state", "since"}},
		{Method: "GET", Path: "/v1/events/{id}", Tag: "Events", Summary: "Event with synthesized summary and citations", Access: Consumer, Resp: events.Event{}, Query: langQ,
			Notes: "A merged event returns {id, status: merged, merged_into}."},
		{Method: "GET", Path: "/v1/events/{id}/timeline", Tag: "Events", Summary: "Approved material updates", Access: Consumer, Resp: events.TimelineEntry{}, List: true},
		{Method: "GET", Path: "/v1/events/{id}/sources", Tag: "Events", Summary: "Source articles (with relation and attribution)", Access: Consumer, Resp: events.Source{}, List: true},
		{Method: "GET", Path: "/v1/events/{id}/claims", Tag: "Fact-checking", Summary: "Claims and current verdicts for an event", Access: Consumer, Resp: factcheck.FactCheck{}, List: true, Query: langQ},
		{Method: "GET", Path: "/v1/events/{id}/explain", Tag: "Conversations", Summary: "One-shot grounded explanation", Access: Consumer, Resp: chat.Message{}, Query: []string{"lang", "level", "question"}},
		{Method: "GET", Path: "/v1/search", Tag: "Search", Summary: "Hybrid lexical + semantic search", Access: Consumer, Resp: ret((*search.Service).Search),
			Query: []string{"q", "mode", "types", "languages", "category_ids", "publisher_ids", "from", "to", "verdicts", "limit", "rerank"},
			Notes: "`degraded` lists unavailable retrieval components (e.g. lexical:postgres_fts)."},

		// ---- consumer: fact-checking ----
		{Method: "GET", Path: "/v1/fact-checks", Tag: "Fact-checking", Summary: "Published fact-checks / misinformation watch", Access: Consumer, Resp: factcheck.FactCheck{}, List: true,
			Query: []string{"lang", "verdict", "before", "limit"}},
		{Method: "GET", Path: "/v1/fact-checks/{id}", Tag: "Fact-checking", Summary: "Claim, evidence, verdict, explanation and correction history", Access: Consumer, Resp: FactCheckDetail{}, Query: langQ},
		{Method: "GET", Path: "/v1/misinformation/trends", Tag: "Fact-checking", Summary: "Trending reviewed narratives and claims", Access: Consumer, Resp: ret((*factcheck.Service).Trends), Query: []string{"lang", "days"}},

		// ---- consumer: personalization ----
		{Method: "GET", Path: "/v1/users/me/preferences", Tag: "Personalization", Summary: "Preferences", Access: Consumer, Resp: feed.Preferences{}},
		{Method: "PUT", Path: "/v1/users/me/preferences", Tag: "Personalization", Summary: "Replace preferences", Access: Consumer, Req: feed.PreferencesInput{}, Resp: feed.Preferences{}},
		{Method: "GET", Path: "/v1/users/me/interests", Tag: "Personalization", Summary: "Follows and mutes", Access: Consumer, Resp: feed.Interest{}, List: true},
		{Method: "POST", Path: "/v1/users/me/interests", Tag: "Personalization", Summary: "Follow or mute a category, entity, keyword, region or publisher", Access: Consumer, Req: feed.InterestInput{}, Resp: feed.Interest{}, Status: 201},
		{Method: "DELETE", Path: "/v1/users/me/interests/{iid}", Tag: "Personalization", Summary: "Remove an interest", Access: Consumer, Status: 204},
		{Method: "PUT", Path: "/v1/users/me/categories", Tag: "Personalization", Summary: "Onboarding: set followed categories", Access: Consumer, Req: CategoriesRequest{}, Resp: feed.Interest{}, List: true},
		{Method: "GET", Path: "/v1/users/me/bookmarks", Tag: "Personalization", Summary: "Bookmarked events", Access: Consumer, Resp: events.Event{}, List: true, Query: langQ},
		{Method: "POST", Path: "/v1/events/{id}/follow", Tag: "Personalization", Summary: "Follow an event", Access: Consumer, Status: 204},
		{Method: "POST", Path: "/v1/events/{id}/unfollow", Tag: "Personalization", Summary: "Unfollow an event", Access: Consumer, Status: 204},
		{Method: "POST", Path: "/v1/events/{id}/seen", Tag: "Personalization", Summary: "Mark an event version as seen", Access: Consumer, Req: SeenRequest{}, Status: 204},
		{Method: "PUT", Path: "/v1/events/{id}/bookmark", Tag: "Personalization", Summary: "Bookmark", Access: Consumer, Status: 204},
		{Method: "DELETE", Path: "/v1/events/{id}/bookmark", Tag: "Personalization", Summary: "Remove bookmark", Access: Consumer, Status: 204},
		{Method: "POST", Path: "/v1/events/{id}/dismiss", Tag: "Personalization", Summary: "Hide an event from the feed", Access: Consumer, Status: 204},
		{Method: "GET", Path: "/v1/feed", Tag: "Personalization", Summary: "Ranked personalized feed (stable cursor pagination)", Access: Consumer, Resp: feed.FeedPage{}, Query: pageQ},
		{Method: "GET", Path: "/v1/catch-up", Tag: "Personalization", Summary: "What changed since the last acknowledged visit", Access: Consumer, Resp: feed.CatchUp{}, Query: []string{"lang", "since", "limit"}},
		{Method: "POST", Path: "/v1/catch-up/acknowledge", Tag: "Personalization", Summary: "Acknowledge catch-up up to a time", Access: Consumer, Req: AcknowledgeRequest{}, Resp: Acknowledged{}},

		// ---- consumer: briefings & audio ----
		{Method: "POST", Path: "/v1/briefings", Tag: "Briefings", Summary: "Request a text briefing (async; identical recent request reused with 200)", Access: Consumer,
			Req: briefings.Request{}, Resp: briefings.Briefing{}, Status: 202},
		{Method: "GET", Path: "/v1/briefings", Tag: "Briefings", Summary: "Recent briefings", Access: Consumer, Resp: briefings.Briefing{}, List: true, Query: listQ},
		{Method: "GET", Path: "/v1/briefings/latest", Tag: "Briefings", Summary: "Latest briefing", Access: Consumer, Resp: briefings.Briefing{}},
		{Method: "GET", Path: "/v1/briefings/{id}", Tag: "Briefings", Summary: "Briefing with ordered events and citations", Access: Consumer, Resp: briefings.Briefing{}},
		{Method: "POST", Path: "/v1/briefings/{id}/audio", Tag: "Briefings", Summary: "Request narration audio", Access: Consumer, Req: AudioRequest{}, Resp: briefings.Audio{}, Status: 202, Conflict: true,
			Notes: "Without a configured TTS route the asset ends in status provider_unavailable; audio is never fabricated."},
		{Method: "GET", Path: "/v1/briefings/{id}/audio", Tag: "Briefings", Summary: "Audio status and short-lived URL", Access: Consumer, Resp: briefings.Audio{}},
		{Method: "DELETE", Path: "/v1/briefings/{id}/audio", Tag: "Briefings", Summary: "Cancel pending audio", Access: Consumer, Status: 204},
		{Method: "GET", Path: "/v1/briefings/{id}/audio/stream", Tag: "Briefings", Summary: "Stream the audio (owner only)", Access: Consumer, Resp: "audio", Conflict: true},

		// ---- consumer: chat ----
		{Method: "POST", Path: "/v1/chat/sessions", Tag: "Conversations", Summary: "Start a chat (global, event or category scope)", Access: Consumer, Req: chat.CreateInput{}, Resp: chat.Session{}, Status: 201},
		{Method: "GET", Path: "/v1/chat/sessions", Tag: "Conversations", Summary: "Chat sessions", Access: Consumer, Resp: chat.Session{}, List: true},
		{Method: "GET", Path: "/v1/chat/sessions/{id}", Tag: "Conversations", Summary: "Session with messages", Access: Consumer, Resp: chat.Session{}},
		{Method: "DELETE", Path: "/v1/chat/sessions/{id}", Tag: "Conversations", Summary: "Delete a session and its messages", Access: Consumer, Status: 204},
		{Method: "POST", Path: "/v1/chat/sessions/{id}/messages", Tag: "Conversations", Summary: "Ask; answer is grounded in retrieved sources or abstains", Access: Consumer,
			Req: AskRequest{}, Resp: chat.Message{}, Status: 201},

		// ---- consumer: notifications ----
		{Method: "GET", Path: "/v1/notifications/schedules", Tag: "Notifications", Summary: "Digest schedules", Access: Consumer, Resp: notifications.ScheduleView{}, List: true},
		{Method: "PUT", Path: "/v1/notifications/schedules", Tag: "Notifications", Summary: "Create or replace the default schedule", Access: Consumer, Req: notifications.ScheduleInput{}, Resp: notifications.ScheduleView{}},
		{Method: "PUT", Path: "/v1/notifications/schedules/{name}", Tag: "Notifications", Summary: "Create or replace a named schedule", Access: Consumer, Req: notifications.ScheduleInput{}, Resp: notifications.ScheduleView{}},
		{Method: "DELETE", Path: "/v1/notifications/schedules/{name}", Tag: "Notifications", Summary: "Delete a schedule", Access: Consumer, Status: 204},
		{Method: "GET", Path: "/v1/notifications/deliveries", Tag: "Notifications", Summary: "Delivery history (sent, skipped with reason, provider_unavailable)", Access: Consumer, Resp: notifications.DeliveryView{}, List: true, Query: listQ},
		{Method: "GET", Path: "/v1/devices", Tag: "Notifications", Summary: "Registered devices (tokens are never returned)", Access: Consumer, Resp: notifications.Device{}, List: true},
		{Method: "POST", Path: "/v1/devices", Tag: "Notifications", Summary: "Register a push token", Access: Consumer, Req: notifications.DeviceInput{}, Resp: notifications.Device{}, Status: 201},
		{Method: "DELETE", Path: "/v1/devices/{id}", Tag: "Notifications", Summary: "Revoke a device", Access: Consumer, Status: 204},

		// ---- admin: users ----
		{Method: "GET", Path: "/v1/admin/users/{id}", Tag: "Admin: users", Summary: "User", Access: Staff, Perms: []string{"users:manage"}, Resp: identity.User{}},
		{Method: "PUT", Path: "/v1/admin/users/{id}/roles", Tag: "Admin: users", Summary: "Replace roles", Access: Staff, Perms: []string{"users:manage"}, Req: RolesRequest{}, Resp: identity.User{}},
		{Method: "PUT", Path: "/v1/admin/users/{id}/status", Tag: "Admin: users", Summary: "Suspend/reactivate", Access: Staff, Perms: []string{"users:manage"}, Req: StatusRequest{}, Status: 204},

		// ---- admin: taxonomy ----
		{Method: "GET", Path: "/v1/admin/categories", Tag: "Admin: categories", Summary: "All categories (any status) as a tree", Access: Staff, Perms: []string{"categories:write"}, Resp: AdminCategoryTree{}},
		{Method: "POST", Path: "/v1/admin/categories", Tag: "Admin: categories", Summary: "Create a category", Access: Staff, Perms: []string{"categories:write"}, Req: taxonomy.Input{}, Resp: taxonomy.Category{}, Status: 201, Conflict: true},
		{Method: "GET", Path: "/v1/admin/categories/{id}", Tag: "Admin: categories", Summary: "Category", Access: Staff, Perms: []string{"categories:write"}, Resp: taxonomy.Category{}},
		{Method: "PATCH", Path: "/v1/admin/categories/{id}", Tag: "Admin: categories", Summary: "Update (optimistic version)", Access: Staff, Perms: []string{"categories:write"}, Req: taxonomy.Input{}, Resp: taxonomy.Category{}, Conflict: true},
		{Method: "POST", Path: "/v1/admin/categories/{id}/activate", Tag: "Admin: categories", Summary: "Activate", Access: Staff, Perms: []string{"categories:write"}, Resp: taxonomy.Category{}},
		{Method: "POST", Path: "/v1/admin/categories/{id}/hide", Tag: "Admin: categories", Summary: "Hide from users", Access: Staff, Perms: []string{"categories:write"}, Resp: taxonomy.Category{}},
		{Method: "POST", Path: "/v1/admin/categories/{id}/archive", Tag: "Admin: categories", Summary: "Archive", Access: Staff, Perms: []string{"categories:write"}, Resp: taxonomy.Category{}},
		{Method: "POST", Path: "/v1/admin/categories/{id}/reparent", Tag: "Admin: categories", Summary: "Move under another parent (cycle-checked)", Access: Staff, Perms: []string{"categories:write"}, Req: ReparentRequest{}, Resp: taxonomy.Category{}, Conflict: true},
		{Method: "POST", Path: "/v1/admin/categories/reorder", Tag: "Admin: categories", Summary: "Reorder siblings", Access: Staff, Perms: []string{"categories:write"}, Req: ReorderRequest{}, Status: 204},
		{Method: "POST", Path: "/v1/admin/categories/{id}/merge", Tag: "Admin: categories", Summary: "Merge into another category (redirects kept)", Access: Staff, Perms: []string{"categories:write"}, Req: MergeCategoryRequest{}, Status: 204, Conflict: true},
		{Method: "POST", Path: "/v1/admin/categories/reclassify", Tag: "Admin: categories", Summary: "Reclassify articles (dry run supported)", Access: Staff, Perms: []string{"categories:write"}, Req: taxonomy.ReclassifyRequest{}, Resp: JobAccepted{}, Status: 202},

		// ---- admin: sources ----
		{Method: "GET", Path: "/v1/admin/publishers", Tag: sources_, Summary: "Publishers", Access: Staff, Perms: []string{"sources:read"}, Resp: sources.Publisher{}, List: true, Query: []string{"status", "q", "limit"}},
		{Method: "POST", Path: "/v1/admin/publishers", Tag: sources_, Summary: "Create publisher", Access: Staff, Perms: []string{"sources:read", "sources:write"}, Req: sources.PublisherInput{}, Resp: sources.Publisher{}, Status: 201, Conflict: true},
		{Method: "GET", Path: "/v1/admin/publishers/{id}", Tag: sources_, Summary: "Publisher", Access: Staff, Perms: []string{"sources:read"}, Resp: sources.Publisher{}},
		{Method: "PATCH", Path: "/v1/admin/publishers/{id}", Tag: sources_, Summary: "Update publisher", Access: Staff, Perms: []string{"sources:read", "sources:write"}, Req: sources.PublisherInput{}, Resp: sources.Publisher{}, Conflict: true},
		{Method: "GET", Path: "/v1/admin/publishers/{id}/permissions", Tag: sources_, Summary: "Content-rights permissions", Access: Staff, Perms: []string{"sources:read"}, Resp: sources.Permission{}, List: true},
		{Method: "PUT", Path: "/v1/admin/publishers/{id}/permissions", Tag: sources_, Summary: "Set content-rights permission", Access: Staff, Perms: []string{"sources:read", "sources:write"}, Req: sources.PermissionInput{}, Resp: sources.Permission{}},
		{Method: "GET", Path: "/v1/admin/sources", Tag: sources_, Summary: "Source endpoints", Access: Staff, Perms: []string{"sources:read"}, Resp: sources.Endpoint{}, List: true, Query: []string{"publisher_id", "status", "limit"}},
		{Method: "POST", Path: "/v1/admin/sources", Tag: sources_, Summary: "Create endpoint (URL validated against SSRF policy)", Access: Staff, Perms: []string{"sources:read", "sources:write"}, Req: sources.EndpointInput{}, Resp: sources.Endpoint{}, Status: 201},
		{Method: "POST", Path: "/v1/admin/sources/test", Tag: sources_, Summary: "Test an unsaved source configuration (nothing persisted)", Access: Staff, Perms: []string{"sources:read", "sources:write"}, Req: ingest.TestSourceInput{}, Resp: ingest.TestSourceResult{}},
		{Method: "POST", Path: "/v1/admin/sources/preview-url", Tag: sources_, Summary: "Preview extraction of one URL", Access: Staff, Perms: []string{"sources:read", "sources:write"}, Req: PreviewURLRequest{}, Resp: ingest.ItemPreview{}},
		{Method: "GET", Path: "/v1/admin/sources/{id}", Tag: sources_, Summary: "Endpoint", Access: Staff, Perms: []string{"sources:read"}, Resp: sources.Endpoint{}},
		{Method: "PATCH", Path: "/v1/admin/sources/{id}", Tag: sources_, Summary: "Update endpoint (polling, extraction, limits)", Access: Staff, Perms: []string{"sources:read", "sources:write"}, Req: sources.EndpointInput{}, Resp: sources.Endpoint{}, Conflict: true},
		{Method: "POST", Path: "/v1/admin/sources/{id}/enable", Tag: sources_, Summary: "Enable (requires active publisher and granted permission)", Access: Staff, Perms: []string{"sources:read", "sources:write"}, Resp: sources.Endpoint{}},
		{Method: "POST", Path: "/v1/admin/sources/{id}/pause", Tag: sources_, Summary: "Pause", Access: Staff, Perms: []string{"sources:read", "sources:write"}, Resp: sources.Endpoint{}},
		{Method: "POST", Path: "/v1/admin/sources/{id}/disable", Tag: sources_, Summary: "Disable", Access: Staff, Perms: []string{"sources:read", "sources:write"}, Resp: sources.Endpoint{}},
		{Method: "POST", Path: "/v1/admin/sources/{id}/archive", Tag: sources_, Summary: "Archive", Access: Staff, Perms: []string{"sources:read", "sources:write"}, Resp: sources.Endpoint{}},
		{Method: "POST", Path: "/v1/admin/sources/{id}/pending-permission", Tag: sources_, Summary: "Mark as awaiting permission", Access: Staff, Perms: []string{"sources:read", "sources:write"}, Resp: sources.Endpoint{}},
		{Method: "POST", Path: "/v1/admin/sources/{id}/test", Tag: sources_, Summary: "Test the saved configuration", Access: Staff, Perms: []string{"sources:read", "sources:write"}, Resp: ingest.TestSourceResult{}},
		{Method: "POST", Path: "/v1/admin/sources/{id}/crawl", Tag: sources_, Summary: "Crawl now", Access: Staff, Perms: []string{"sources:read", "sources:write"}, Req: CrawlRequest{}, Resp: CrawlAccepted{}, Status: 202},
		{Method: "GET", Path: "/v1/admin/sources/{id}/runs", Tag: sources_, Summary: "Crawl runs", Access: Staff, Perms: []string{"sources:read"}, Resp: elem((*sources.Service).ListCrawlRuns), List: true, Query: listQ},
		{Method: "GET", Path: "/v1/admin/sources/{id}/health", Tag: sources_, Summary: "Health, coverage and anomalies", Access: Staff, Perms: []string{"sources:read"}, Resp: sources.Health{}},
		{Method: "POST", Path: "/v1/admin/sources/{id}/reprocess", Tag: sources_, Summary: "Reprocess this source's articles", Access: Staff, Perms: []string{"sources:read", "sources:write"}, Req: api.ReprocessRequest{}, Resp: JobAccepted{}, Status: 202},
		{Method: "GET", Path: "/v1/admin/health/source-metrics", Tag: sources_, Summary: "Health across sources", Access: Staff, Perms: []string{"sources:read"}, Resp: elem((*sources.Service).SourceMetrics), List: true},
		{Method: "GET", Path: "/v1/admin/blocked-urls", Tag: sources_, Summary: "URL blocklist", Access: Staff, Perms: []string{"sources:read"}, Resp: elem((*sources.Service).ListBlocked), List: true},
		{Method: "POST", Path: "/v1/admin/blocked-urls", Tag: sources_, Summary: "Block a URL/host/prefix", Access: Staff, Perms: []string{"sources:read", "sources:write"}, Req: BlockedURLRequest{}, Resp: ret((*sources.Service).AddBlocked), Status: 201},
		{Method: "DELETE", Path: "/v1/admin/blocked-urls/{id}", Tag: sources_, Summary: "Unblock", Access: Staff, Perms: []string{"sources:read", "sources:write"}, Status: 204},
		{Method: "GET", Path: "/v1/admin/extraction-profiles", Tag: sources_, Summary: "Extraction profiles", Access: Staff, Perms: []string{"sources:read"}, Resp: elem((*sources.Service).ListProfiles), List: true},
		{Method: "POST", Path: "/v1/admin/extraction-profiles", Tag: sources_, Summary: "Create profile (selectors validated)", Access: Staff, Perms: []string{"sources:read", "sources:write"}, Req: sources.ProfileInput{}, Resp: ret((*sources.Service).GetProfile), Status: 201},
		{Method: "GET", Path: "/v1/admin/extraction-profiles/{id}", Tag: sources_, Summary: "Profile", Access: Staff, Perms: []string{"sources:read"}, Resp: ret((*sources.Service).GetProfile)},
		{Method: "PATCH", Path: "/v1/admin/extraction-profiles/{id}", Tag: sources_, Summary: "Update profile", Access: Staff, Perms: []string{"sources:read", "sources:write"}, Req: sources.ProfileInput{}, Resp: ret((*sources.Service).GetProfile), Conflict: true},
		{Method: "POST", Path: "/v1/admin/extraction-profiles/{id}/validate", Tag: sources_, Summary: "Run the profile against its fixtures", Access: Staff, Perms: []string{"sources:read"}, Resp: ProfileValidation{}},

		// ---- admin: operations & audit ----
		{Method: "GET", Path: "/v1/admin/jobs", Tag: "Admin: operations", Summary: "Durable jobs", Access: Staff, Perms: []string{"jobs:manage"}, Resp: elem(jobs.List), List: true, Query: []string{"kind", "status", "limit"}},
		{Method: "GET", Path: "/v1/admin/jobs/{id}", Tag: "Admin: operations", Summary: "Job (status, attempts, result, last error)", Access: Staff, Perms: []string{"jobs:manage"}, Resp: ret(jobs.Get)},
		{Method: "POST", Path: "/v1/admin/jobs/{id}/retry", Tag: "Admin: operations", Summary: "Retry a failed job", Access: Staff, Perms: []string{"jobs:manage"}, Resp: Accepted{}, Status: 202, Conflict: true},
		{Method: "POST", Path: "/v1/admin/jobs/{id}/cancel", Tag: "Admin: operations", Summary: "Cancel a queued job", Access: Staff, Perms: []string{"jobs:manage"}, Status: 204, Conflict: true},
		{Method: "GET", Path: "/v1/admin/dead-letters", Tag: "Admin: operations", Summary: "Dead-lettered messages", Access: Staff, Perms: []string{"jobs:manage"}, Resp: DeadLetter{}, List: true, Query: []string{"consumer", "limit"}},
		{Method: "POST", Path: "/v1/admin/dead-letters/{id}/replay", Tag: "Admin: operations", Summary: "Replay (idempotency key preserved)", Access: Staff, Perms: []string{"jobs:manage"}, Resp: Accepted{}, Status: 202},
		{Method: "GET", Path: "/v1/admin/audit-logs", Tag: "Admin: operations", Summary: "Audit trail", Access: Staff, Perms: []string{"audit:read"}, Resp: elem(audit.List), List: true,
			Query: []string{"resource_type", "resource_id", "actor_id", "before", "limit"}},

		// ---- admin: articles & events ----
		{Method: "GET", Path: "/v1/admin/articles", Tag: "Admin: content", Summary: "Articles by processing status", Access: Staff, Perms: []string{"articles:moderate"}, Resp: elem((*ingest.Service).ListArticles), List: true,
			Query: []string{"status", "publisher_id", "q", "limit"}},
		{Method: "GET", Path: "/v1/admin/articles/{id}", Tag: "Admin: content", Summary: "Article with versions, categories, entities, facts, claims and event links", Access: Staff, Perms: []string{"articles:moderate"}, Resp: ret((*ingest.Service).ArticleDetail)},
		{Method: "POST", Path: "/v1/admin/articles/{id}/quarantine", Tag: "Admin: content", Summary: "Quarantine", Access: Staff, Perms: []string{"articles:moderate"}, Req: ReasonRequest{}, Resp: Accepted{}, Status: 202},
		{Method: "POST", Path: "/v1/admin/articles/{id}/release", Tag: "Admin: content", Summary: "Release from quarantine", Access: Staff, Perms: []string{"articles:moderate"}, Req: ReasonRequest{}, Resp: Accepted{}, Status: 202},
		{Method: "POST", Path: "/v1/admin/articles/{id}/retry", Tag: "Admin: content", Summary: "Retry failed processing", Access: Staff, Perms: []string{"articles:moderate"}, Req: ReasonRequest{}, Resp: Accepted{}, Status: 202},
		{Method: "POST", Path: "/v1/admin/articles/{id}/withdraw", Tag: "Admin: content", Summary: "Withdraw (legal/takedown); affected events refreshed", Access: Staff, Perms: []string{"articles:moderate"}, Req: WithdrawRequest{}, Resp: WithdrawResult{}},
		{Method: "PUT", Path: "/v1/admin/articles/{id}/categories", Tag: "Admin: content", Summary: "Editorial category override", Access: Staff, Perms: []string{"articles:moderate"}, Req: ArticleCategoriesRequest{}, Status: 204},
		{Method: "POST", Path: "/v1/admin/articles/reprocess", Tag: "Admin: content", Summary: "Reprocess selected articles", Access: Staff, Perms: []string{"articles:moderate"}, Req: api.ReprocessRequest{}, Resp: JobAccepted{}, Status: 202},
		{Method: "GET", Path: "/v1/admin/events/{id}", Tag: "Admin: content", Summary: "Event with versions, full timeline and link rationale", Access: Staff, Perms: []string{"events:edit"}, Resp: AdminEvent{}},
		{Method: "POST", Path: "/v1/admin/events/{id}/merge", Tag: "Admin: content", Summary: "Merge events into this one (reversible)", Access: Staff, Perms: []string{"events:edit"}, Req: MergeEventsRequest{}, Resp: MergeEventsResult{}, Conflict: true},
		{Method: "POST", Path: "/v1/admin/events/unmerge/{historyId}", Tag: "Admin: content", Summary: "Undo a merge", Access: Staff, Perms: []string{"events:edit"}, Req: ReasonRequest{}, Status: 204, Conflict: true},
		{Method: "POST", Path: "/v1/admin/events/{id}/split", Tag: "Admin: content", Summary: "Split articles into a new event", Access: Staff, Perms: []string{"events:edit"}, Req: SplitEventRequest{}, Resp: SplitEventResult{}, Status: 201},
		{Method: "POST", Path: "/v1/admin/events/{id}/correct", Tag: "Admin: content", Summary: "Editorial correction (new version)", Access: Staff, Perms: []string{"events:edit"}, Req: events.CorrectInput{}, Resp: events.Event{}, Conflict: true},
		{Method: "POST", Path: "/v1/admin/events/{id}/updates/{updateId}/approve", Tag: "Admin: content", Summary: "Approve a held update (contradiction/critical)", Access: Staff, Perms: []string{"events:edit"}, Status: 204, Conflict: true},

		// ---- admin: fact-checking ----
		{Method: "GET", Path: "/v1/admin/fact-checks/queue", Tag: "Admin: fact-checking", Summary: "Review queue (priority ordered)", Access: Staff, Perms: []string{"factcheck:review"}, Resp: factcheck.ReviewItem{}, List: true, Query: []string{"status", "limit"}},
		{Method: "GET", Path: "/v1/admin/claims/{id}", Tag: "Admin: fact-checking", Summary: "Claim dossier: occurrences, evidence, verdict history", Access: Staff, Perms: []string{"factcheck:review"}, Resp: map[string]any{}},
		{Method: "POST", Path: "/v1/admin/claims/{id}/evidence", Tag: "Admin: fact-checking", Summary: "Attach evidence (URL SSRF-checked)", Access: Staff, Perms: []string{"factcheck:review"}, Req: factcheck.EvidenceInput{}, Resp: map[string]any{}, Status: 201},
		{Method: "POST", Path: "/v1/admin/claims/{id}/reverify", Tag: "Admin: fact-checking", Summary: "Re-run evidence retrieval and assessment", Access: Staff, Perms: []string{"factcheck:review"}, Resp: map[string]any{}},
		{Method: "POST", Path: "/v1/admin/claims/{id}/status", Tag: "Admin: fact-checking", Summary: "Set claim status", Access: Staff, Perms: []string{"factcheck:review"}, Req: StatusRequest{}, Status: 204},
		{Method: "POST", Path: "/v1/admin/fact-checks/{id}/decision", Tag: "Admin: fact-checking", Summary: "Record a reviewer decision", Access: Staff, Perms: []string{"factcheck:review"}, Req: factcheck.DecisionInput{}, Resp: factcheck.ReviewItem{}, Conflict: true},
		{Method: "POST", Path: "/v1/admin/fact-checks/{id}/publish", Tag: "Admin: fact-checking", Summary: "Publish a reviewed verdict", Access: Staff, Perms: []string{"factcheck:publish"}, Status: 204, Conflict: true,
			Notes: "Separate permission from review; definitive high-risk labels cannot be published without an approved review."},
		{Method: "POST", Path: "/v1/admin/claims/{id}/correct", Tag: "Admin: fact-checking", Summary: "Correct a published verdict (history kept)", Access: Staff, Perms: []string{"factcheck:publish"}, Req: factcheck.CorrectionInput{}, Resp: factcheck.FactCheck{}},
		{Method: "POST", Path: "/v1/admin/narratives", Tag: "Admin: fact-checking", Summary: "Create a misinformation narrative (draft)", Access: Staff, Perms: []string{"factcheck:review"}, Req: factcheck.NarrativeInput{}, Resp: NarrativeCreated{}, Status: 201},
		{Method: "POST", Path: "/v1/admin/narratives/{id}/publish", Tag: "Admin: fact-checking", Summary: "Publish a narrative", Access: Staff, Perms: []string{"factcheck:publish"}, Status: 204, Conflict: true},

		// ---- admin: AI platform ----
		{Method: "GET", Path: "/v1/admin/ai/providers", Tag: "Admin: AI", Summary: "Providers (secrets never returned)", Access: Staff, Perms: []string{"ai:configure"}, Resp: ai.ProviderView{}, List: true},
		{Method: "POST", Path: "/v1/admin/ai/providers", Tag: "Admin: AI", Summary: "Create provider", Access: Staff, Perms: []string{"ai:configure"}, Req: ai.ProviderInput{}, Resp: ai.ProviderView{}, Status: 201, Conflict: true},
		{Method: "PATCH", Path: "/v1/admin/ai/providers/{id}", Tag: "Admin: AI", Summary: "Update provider", Access: Staff, Perms: []string{"ai:configure"}, Req: ai.ProviderInput{}, Resp: ai.ProviderView{}, Conflict: true},
		{Method: "GET", Path: "/v1/admin/ai/models", Tag: "Admin: AI", Summary: "Models", Access: Staff, Perms: []string{"ai:configure"}, Resp: ai.ModelView{}, List: true},
		{Method: "POST", Path: "/v1/admin/ai/models", Tag: "Admin: AI", Summary: "Register model", Access: Staff, Perms: []string{"ai:configure"}, Req: ai.ModelInput{}, Resp: ai.ModelView{}, Status: 201, Conflict: true},
		{Method: "PATCH", Path: "/v1/admin/ai/models/{id}", Tag: "Admin: AI", Summary: "Update model", Access: Staff, Perms: []string{"ai:configure"}, Req: ai.ModelInput{}, Resp: ai.ModelView{}, Conflict: true},
		{Method: "GET", Path: "/v1/admin/ai/routes", Tag: "Admin: AI", Summary: "Task routes", Access: Staff, Perms: []string{"ai:configure"}, Resp: ai.RouteView{}, List: true},
		{Method: "POST", Path: "/v1/admin/ai/routes", Tag: "Admin: AI", Summary: "Create route (production or shadow)", Access: Staff, Perms: []string{"ai:configure"}, Req: ai.RouteInput{}, Resp: ai.RouteView{}, Status: 201, Conflict: true},
		{Method: "PATCH", Path: "/v1/admin/ai/routes/{id}", Tag: "Admin: AI", Summary: "Switch model, fallbacks, prompt, budget", Access: Staff, Perms: []string{"ai:configure"}, Req: ai.RouteInput{}, Resp: ai.RouteView{}, Conflict: true},
		{Method: "GET", Path: "/v1/admin/ai/prompts", Tag: "Admin: AI", Summary: "Prompt versions", Access: Staff, Perms: []string{"ai:configure"}, Resp: elem((*ai.Admin).ListPrompts), List: true, Query: []string{"task"}},
		{Method: "POST", Path: "/v1/admin/ai/prompts", Tag: "Admin: AI", Summary: "Create a prompt version", Access: Staff, Perms: []string{"ai:configure"}, Req: ai.PromptInput{}, Resp: ret((*ai.Admin).CreatePromptVersion), Status: 201},
		{Method: "GET", Path: "/v1/admin/ai/executions", Tag: "Admin: AI", Summary: "Executions (tokens, cost, latency, errors)", Access: Staff, Perms: []string{"ai:configure"}, Resp: ai.ExecutionView{}, List: true, Query: []string{"task", "status", "limit"}},
		{Method: "GET", Path: "/v1/admin/ai/usage", Tag: "Admin: AI", Summary: "Usage and cost summary", Access: Staff, Perms: []string{"ai:configure"}, Resp: elem((*ai.Admin).UsageSummary), List: true, Query: []string{"hours"}},
		{Method: "GET", Path: "/v1/admin/ai/evaluations", Tag: "Admin: AI", Summary: "Evaluation runs and metrics", Access: Staff, Perms: []string{"ai:evaluate"}, Resp: eval.RunView{}, List: true, Query: []string{"suite", "limit"}},
		{Method: "GET", Path: "/v1/admin/ai/evaluations/{id}", Tag: "Admin: AI", Summary: "Per-case evaluation results", Access: Staff, Perms: []string{"ai:evaluate"}, Resp: EvalRunResults{}},
		{Method: "GET", Path: "/v1/admin/search/collections", Tag: "Admin: AI", Summary: "Vector collections per doc type/model", Access: Staff, Perms: []string{"ai:configure"}, Resp: elem((*search.Service).ListCollections), List: true},
		{Method: "POST", Path: "/v1/admin/search/embedding-migration", Tag: "Admin: AI", Summary: "Plan an embedding migration (building collections + backfill jobs)", Access: Staff, Perms: []string{"ai:configure"}, Resp: map[string]any{}, Status: 202},
		{Method: "POST", Path: "/v1/admin/search/collections/{docType}/cutover", Tag: "Admin: AI", Summary: "Activate a fully backfilled collection", Access: Staff, Perms: []string{"ai:configure"}, Status: 204, Conflict: true},
		{Method: "POST", Path: "/v1/admin/search/collections/{docType}/rollback", Tag: "Admin: AI", Summary: "Reactivate the previous collection", Access: Staff, Perms: []string{"ai:configure"}, Status: 204, Conflict: true},
		{Method: "POST", Path: "/v1/admin/search/reindex", Tag: "Admin: AI", Summary: "Rebuild search indices from PostgreSQL", Access: Staff, Perms: []string{"ai:configure"}, Req: ReindexRequest{}, Resp: JobAccepted{}, Status: 202},

		// ---- admin: policies ----
		{Method: "GET", Path: "/v1/admin/policies", Tag: "Admin: policies", Summary: "Editorial/ranking/notification policies", Access: Staff, Perms: []string{"policies:write"}, Resp: policy.Policy{}, List: true},
		{Method: "PUT", Path: "/v1/admin/policies/{key}", Tag: "Admin: policies", Summary: "Replace a policy (optimistic version)", Access: Staff, Perms: []string{"policies:write"}, Req: PolicyRequest{}, Resp: policy.Policy{}, Conflict: true},
		{Method: "GET", Path: "/v1/admin/feature-flags", Tag: "Admin: policies", Summary: "Feature flags", Access: Staff, Perms: []string{"policies:write"}, Resp: elem((*policy.Store).Flags), List: true},
		{Method: "PUT", Path: "/v1/admin/feature-flags/{key}", Tag: "Admin: policies", Summary: "Set a flag and rollout percentage", Access: Staff, Perms: []string{"policies:write"}, Req: FlagRequest{}, Status: 204},
	}
}
