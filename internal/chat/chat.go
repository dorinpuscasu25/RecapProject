// Package chat provides grounded conversational Q&A over events, claims and articles. Answers
// are generated only from retrieved context; citations are validated server-side and mapped to
// canonical records. Conversation memory is bounded and retention-limited.
package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dorinpuscasu25/recapproject/internal/ai"
	"github.com/dorinpuscasu25/recapproject/internal/events"
	"github.com/dorinpuscasu25/recapproject/internal/factcheck"
	"github.com/dorinpuscasu25/recapproject/internal/nlp"
	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ids"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ratelimit"
	"github.com/dorinpuscasu25/recapproject/internal/policy"
	"github.com/dorinpuscasu25/recapproject/internal/search"
)

type Service struct {
	db      *db.DB
	gw      *ai.Gateway
	events  *events.Service
	fc      *factcheck.Service
	search  *search.Service
	pol     *policy.Store
	limiter ratelimit.Limiter
	now     func() time.Time
}

func NewService(d *db.DB, gw *ai.Gateway, ev *events.Service, fc *factcheck.Service, s *search.Service, pol *policy.Store, lim ratelimit.Limiter) *Service {
	return &Service{db: d, gw: gw, events: ev, fc: fc, search: s, pol: pol, limiter: lim, now: time.Now}
}

type cfg struct {
	RetentionDays   int `json:"retention_days"`
	MaxHistoryTurns int `json:"max_history_turns"`
	MaxContextDocs  int `json:"max_context_docs"`
}

func (s *Service) cfg(ctx context.Context) cfg {
	c := cfg{RetentionDays: 30, MaxHistoryTurns: 6, MaxContextDocs: 8}
	_, _ = s.pol.Get(ctx, "chat", &c)
	return c
}

type Session struct {
	ID            uuid.UUID  `json:"id"`
	ScopeType     string     `json:"scope_type"`
	ScopeID       *uuid.UUID `json:"scope_id,omitempty"`
	Language      string     `json:"language"`
	Title         string     `json:"title"`
	CreatedAt     time.Time  `json:"created_at"`
	LastMessageAt time.Time  `json:"last_message_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	Messages      []Message  `json:"messages,omitempty"`
}

type Citation struct {
	Ref         string     `json:"ref"`
	Type        string     `json:"type"` // event | article | claim
	ID          uuid.UUID  `json:"id"`
	Title       string     `json:"title"`
	URL         string     `json:"url,omitempty"`
	Publisher   string     `json:"publisher,omitempty"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	Excerpt     string     `json:"excerpt,omitempty"`
	// Sources lists the original articles behind a synthesized event summary.
	Sources []SourceLink `json:"sources,omitempty"`
}

type SourceLink struct {
	ArticleID   uuid.UUID  `json:"article_id"`
	Publisher   string     `json:"publisher"`
	URL         string     `json:"url"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
}

type Message struct {
	ID          uuid.UUID  `json:"id"`
	Role        string     `json:"role"`
	Content     string     `json:"content"`
	Citations   []Citation `json:"citations"`
	Abstained   bool       `json:"abstained"`
	Uncertainty string     `json:"uncertainty,omitempty"`
	ModelRef    *string    `json:"model_ref,omitempty"`
	RetrievedAt *time.Time `json:"retrieved_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

type CreateInput struct {
	ScopeType string     `json:"scope_type"`
	ScopeID   *uuid.UUID `json:"scope_id,omitempty"`
	Language  string     `json:"language,omitempty"`
}

func (s *Service) Create(ctx context.Context, user uuid.UUID, in CreateInput) (*Session, error) {
	if in.ScopeType == "" {
		in.ScopeType = "global"
	}
	if in.Language == "" {
		_ = s.db.Pool.QueryRow(ctx, `SELECT summary_language FROM user_preferences WHERE user_id=$1`, user).Scan(&in.Language)
		if in.Language == "" {
			in.Language = "ro"
		}
	}
	var v apperr.Validator
	v.Check(in.Language == "ro" || in.Language == "ru" || in.Language == "en", "language", "invalid", "ro, ru or en")
	switch in.ScopeType {
	case "global":
		v.Check(in.ScopeID == nil, "scope_id", "invalid", "Global sessions have no scope id.")
	case "event", "briefing", "claim", "article":
		v.Check(in.ScopeID != nil, "scope_id", "required", "scope_id is required.")
	default:
		v.Add("scope_type", "invalid", "global, event, briefing, claim or article")
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	title := ""
	if in.ScopeID != nil {
		var q string
		switch in.ScopeType {
		case "event":
			q = `SELECT COALESCE(titles->>'ro', '') FROM events WHERE id=$1 AND status='active'`
		case "briefing":
			q = `SELECT title FROM briefings WHERE id=$1 AND user_id='` + user.String() + `'`
		case "claim":
			q = `SELECT canonical_text FROM claims WHERE id=$1`
		case "article":
			q = `SELECT title FROM articles WHERE id=$1 AND status='enriched' AND tombstoned_at IS NULL`
		}
		if err := s.db.Pool.QueryRow(ctx, q, *in.ScopeID).Scan(&title); err != nil {
			return nil, apperr.NotFound("scope_not_found", "The chat scope was not found.")
		}
	}
	c := s.cfg(ctx)
	sess := &Session{ID: ids.New(), ScopeType: in.ScopeType, ScopeID: in.ScopeID, Language: in.Language, Title: nlp.Truncate(title, 120),
		CreatedAt: s.now().UTC(), LastMessageAt: s.now().UTC(), ExpiresAt: s.now().UTC().Add(time.Duration(c.RetentionDays) * 24 * time.Hour)}
	_, err := s.db.Pool.Exec(ctx, `INSERT INTO chat_sessions (id, user_id, scope_type, scope_id, language, title, expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		sess.ID, user, sess.ScopeType, sess.ScopeID, sess.Language, sess.Title, sess.ExpiresAt)
	return sess, err
}

func (s *Service) Get(ctx context.Context, user, id uuid.UUID) (*Session, error) {
	sess := &Session{ID: id}
	err := s.db.Pool.QueryRow(ctx, `SELECT scope_type, scope_id, language, title, created_at, last_message_at, expires_at FROM chat_sessions
		WHERE id=$1 AND user_id=$2 AND expires_at > now()`, id, user).Scan(&sess.ScopeType, &sess.ScopeID, &sess.Language, &sess.Title, &sess.CreatedAt,
		&sess.LastMessageAt, &sess.ExpiresAt)
	if db.IsNoRows(err) {
		return nil, apperr.NotFound("chat_session_not_found", "Chat session not found.")
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Pool.Query(ctx, `SELECT id, role, content, citations, abstained, retrieval, model_ref, created_at FROM chat_messages WHERE session_id=$1 ORDER BY created_at`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sess.Messages = []Message{}
	for rows.Next() {
		var m Message
		var cites, retr []byte
		if err := rows.Scan(&m.ID, &m.Role, &m.Content, &cites, &m.Abstained, &retr, &m.ModelRef, &m.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(cites, &m.Citations)
		var r struct {
			Uncertainty string     `json:"uncertainty"`
			RetrievedAt *time.Time `json:"retrieved_at"`
		}
		_ = json.Unmarshal(retr, &r)
		m.Uncertainty, m.RetrievedAt = r.Uncertainty, r.RetrievedAt
		if m.Citations == nil {
			m.Citations = []Citation{}
		}
		sess.Messages = append(sess.Messages, m)
	}
	return sess, rows.Err()
}

func (s *Service) List(ctx context.Context, user uuid.UUID) ([]Session, error) {
	rows, err := s.db.Pool.Query(ctx, `SELECT id, scope_type, scope_id, language, title, created_at, last_message_at, expires_at FROM chat_sessions
		WHERE user_id=$1 AND expires_at > now() ORDER BY last_message_at DESC LIMIT 50`, user)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Session, error) {
		var x Session
		err := r.Scan(&x.ID, &x.ScopeType, &x.ScopeID, &x.Language, &x.Title, &x.CreatedAt, &x.LastMessageAt, &x.ExpiresAt)
		return x, err
	})
}

func (s *Service) Delete(ctx context.Context, user, id uuid.UUID) error {
	tag, err := s.db.Pool.Exec(ctx, `DELETE FROM chat_sessions WHERE id=$1 AND user_id=$2`, id, user)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("chat_session_not_found", "Chat session not found.")
	}
	return nil
}

// ---------- Retrieval ----------

type ctxDoc struct {
	doc  ai.ContextDoc
	cite Citation
}

func (s *Service) eventDocs(ctx context.Context, eventID uuid.UUID, lang string, startRef int) ([]ctxDoc, error) {
	e, err := s.events.Get(ctx, eventID, lang)
	if err != nil {
		return nil, err
	}
	var out []ctxDoc
	ref := startRef
	next := func(prefix string) string { ref++; return fmt.Sprintf("%s%d", prefix, ref) }
	srcs, err := s.events.Sources(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if e.Summary != nil {
		upd := e.LastMaterialUpdateAt
		r := next("e")
		var links []SourceLink
		for i, src := range srcs {
			if i >= 5 {
				break
			}
			links = append(links, SourceLink{ArticleID: src.ArticleID, Publisher: src.Publisher, URL: src.URL, PublishedAt: src.PublishedAt})
		}
		out = append(out, ctxDoc{doc: ai.ContextDoc{Ref: r, Kind: "event", Title: e.Title, PublishedAt: fmtTime(upd),
			Text: stripRefs(e.Summary.Detailed)}, cite: Citation{Ref: r, Type: "event", ID: e.ID, Title: e.Title, PublishedAt: upd, Sources: links}})
	}
	for i, src := range srcs {
		if i >= 5 {
			break
		}
		var body *string
		var allowSumm bool
		_ = s.db.Pool.QueryRow(ctx, `SELECT a.body_text, COALESCE((SELECT sp.allow_summarize FROM source_permissions sp WHERE sp.publisher_id=a.publisher_id
			ORDER BY sp.source_endpoint_id NULLS FIRST LIMIT 1), false) FROM articles a WHERE a.id=$1`, src.ArticleID).Scan(&body, &allowSumm)
		if body == nil || !allowSumm {
			continue
		}
		r := next("a")
		out = append(out, ctxDoc{doc: ai.ContextDoc{Ref: r, Kind: "article", Title: src.Title, Publisher: src.Publisher, PublishedAt: fmtTime(src.PublishedAt),
			Text: nlp.Truncate(*body, 2500)}, cite: Citation{Ref: r, Type: "article", ID: src.ArticleID, Title: src.Title, URL: src.URL, Publisher: src.Publisher,
			PublishedAt: src.PublishedAt, Excerpt: src.Snippet}})
	}
	fcs, _ := s.fc.List(ctx, factcheck.ListFilter{EventID: &eventID, Lang: lang, Limit: 5})
	for _, fc := range fcs {
		if fc.Verdict == "not_checkable" {
			continue
		}
		r := next("c")
		t := fc.AssessedAt
		out = append(out, ctxDoc{doc: ai.ContextDoc{Ref: r, Kind: "claim", Title: fc.Claim, PublishedAt: fmtTime(&t),
			Text: fc.Claim + " — " + fc.Headline + " (" + fc.ReviewStatus + "). " + fc.Explanation}, cite: Citation{Ref: r, Type: "claim", ID: fc.ClaimID, Title: fc.Claim, PublishedAt: &t}})
	}
	return out, nil
}

func (s *Service) retrieve(ctx context.Context, user uuid.UUID, sess *Session, question string, maxDocs int) ([]ctxDoc, error) {
	switch sess.ScopeType {
	case "event":
		return s.eventDocs(ctx, *sess.ScopeID, sess.Language, 0)
	case "claim":
		fc, err := s.fc.Get(ctx, *sess.ScopeID, sess.Language)
		if err != nil {
			return nil, err
		}
		t := fc.AssessedAt
		docs := []ctxDoc{{doc: ai.ContextDoc{Ref: "c1", Kind: "claim", Title: fc.Claim, PublishedAt: fmtTime(&t), Text: fc.Claim + " — " + fc.Headline + ". " + fc.Explanation},
			cite: Citation{Ref: "c1", Type: "claim", ID: fc.ClaimID, Title: fc.Claim, PublishedAt: &t}}}
		for i, ev := range fc.Evidence {
			if ev.Excerpt == "" || ev.SourceID == nil {
				continue
			}
			r := fmt.Sprintf("a%d", i+1)
			docs = append(docs, ctxDoc{doc: ai.ContextDoc{Ref: r, Kind: "article", Title: ev.Publisher, Publisher: ev.Publisher, PublishedAt: fmtTime(ev.PublishedAt), Text: ev.Excerpt},
				cite: Citation{Ref: r, Type: "article", ID: *ev.SourceID, Title: ev.Publisher, URL: ev.URL, Publisher: ev.Publisher, PublishedAt: ev.PublishedAt, Excerpt: ev.Excerpt}})
		}
		return docs, nil
	case "briefing":
		rows, err := s.db.Pool.Query(ctx, `SELECT i.event_id FROM briefing_items i JOIN briefings b ON b.id=i.briefing_id WHERE b.id=$1 AND b.user_id=$2 ORDER BY i.position`, *sess.ScopeID, user)
		if err != nil {
			return nil, err
		}
		evs, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return nil, err
		}
		var out []ctxDoc
		for _, e := range evs {
			d, err := s.eventDocs(ctx, e, sess.Language, len(out))
			if err == nil && len(d) > 0 {
				out = append(out, d[0]) // event summary per briefing item
			}
		}
		return out, nil
	case "article":
		var title, url, pub string
		var body *string
		var published *time.Time
		if err := s.db.Pool.QueryRow(ctx, `SELECT a.title, a.canonical_url, p.name, a.body_text, a.published_at FROM articles a JOIN publishers p ON p.id=a.publisher_id WHERE a.id=$1`,
			*sess.ScopeID).Scan(&title, &url, &pub, &body, &published); err != nil {
			return nil, err
		}
		if body == nil {
			return nil, nil
		}
		return []ctxDoc{{doc: ai.ContextDoc{Ref: "a1", Kind: "article", Title: title, Publisher: pub, PublishedAt: fmtTime(published), Text: nlp.Truncate(*body, 4000)},
			cite: Citation{Ref: "a1", Type: "article", ID: *sess.ScopeID, Title: title, URL: url, Publisher: pub, PublishedAt: published}}}, nil
	}
	// Global: hybrid search, then expand the top events.
	res, err := s.search.Search(ctx, search.Query{Text: question, Mode: "hybrid", DocTypes: []string{"event", "claim"}, Limit: 6})
	if err != nil {
		var ae *apperr.Error
		if errors.As(err, &ae) {
			return nil, nil
		}
		return nil, err
	}
	var out []ctxDoc
	for _, h := range res.Items {
		if len(out) >= maxDocs {
			break
		}
		switch h.DocType {
		case "event":
			d, err := s.eventDocs(ctx, h.ID, sess.Language, len(out))
			if err == nil && len(d) > 0 {
				out = append(out, d[0])
				if len(d) > 1 && len(out) < maxDocs {
					out = append(out, d[1])
				}
			}
		case "claim":
			if fc, err := s.fc.Get(ctx, h.ID, sess.Language); err == nil {
				r := fmt.Sprintf("c%d", len(out)+1)
				t := fc.AssessedAt
				out = append(out, ctxDoc{doc: ai.ContextDoc{Ref: r, Kind: "claim", Title: fc.Claim, PublishedAt: fmtTime(&t), Text: fc.Claim + " — " + fc.Headline + ". " + fc.Explanation},
					cite: Citation{Ref: r, Type: "claim", ID: fc.ClaimID, Title: fc.Claim, PublishedAt: &t}})
			}
		}
	}
	return out, nil
}

func fmtTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func stripRefs(s string) string {
	for _, r := range ai.CitedRefs(s) {
		s = strings.ReplaceAll(s, " ["+r+"]", "")
		s = strings.ReplaceAll(s, "["+r+"]", "")
	}
	return s
}

// Ask appends a user message and a grounded assistant answer.
func (s *Service) Ask(ctx context.Context, user, sessionID uuid.UUID, question, level string) (*Message, error) {
	question = strings.TrimSpace(question)
	if question == "" || len([]rune(question)) > 1000 {
		return nil, apperr.Validation(apperr.FieldError{Field: "content", Code: "invalid", Message: "Question must be 1-1000 characters."})
	}
	if level == "" {
		level = "short"
	}
	if level != "short" && level != "detailed" && level != "background" {
		return nil, apperr.Validation(apperr.FieldError{Field: "level", Code: "invalid", Message: "short, detailed or background"})
	}
	if ok, retry, _ := s.limiter.Allow(ctx, "chat:"+user.String(), 60, time.Hour); !ok {
		return nil, apperr.RateLimited(retry)
	}
	sess, err := s.Get(ctx, user, sessionID)
	if err != nil {
		return nil, err
	}
	c := s.cfg(ctx)
	var history []ai.Message
	start := max(0, len(sess.Messages)-2*c.MaxHistoryTurns)
	for _, m := range sess.Messages[start:] {
		history = append(history, ai.Message{Role: m.Role, Content: nlp.Truncate(stripRefs(m.Content), 600)})
	}
	retrievalQuery := question
	if sess.ScopeType == "global" {
		// Follow-ups keep scope: fold the previous user turn into the retrieval query.
		for i := len(sess.Messages) - 1; i >= 0; i-- {
			if sess.Messages[i].Role == "user" {
				retrievalQuery = sess.Messages[i].Content + " " + question
				break
			}
		}
	}
	docs, err := s.retrieve(ctx, user, sess, retrievalQuery, c.MaxContextDocs)
	if err != nil {
		return nil, err
	}
	retrievedAt := s.now().UTC()
	in := &ai.AnswerInput{Question: question, Level: level, Language: sess.Language, Now: retrievedAt.Format(time.RFC3339), History: history,
		ScopeDefault: sess.ScopeType != "global"}
	byRef := map[string]Citation{}
	for i, d := range docs {
		if i >= c.MaxContextDocs {
			break
		}
		in.Context = append(in.Context, d.doc)
		byRef[d.doc.Ref] = d.cite
	}
	var out ai.AnswerOutput
	var modelRef *string
	if len(in.Context) == 0 {
		out = ai.AnswerOutput{Abstained: true, Answer: abstain(sess.Language), Uncertainty: abstain(sess.Language)}
	} else {
		exec, err := s.gw.GenerateStructured(ctx, ai.TaskChatAnswering, in, &out, ai.CallOpts{SubjectType: "chat_session", SubjectID: sessionID.String()})
		if err != nil {
			if errors.Is(err, ai.ErrDeferred) {
				return nil, apperr.Unavailable("ai_unavailable", "The answering model is temporarily unavailable. Please retry.")
			}
			return nil, err
		}
		modelRef = &exec.ModelRef
	}
	// Server-side citation mapping: only refs present in retrieved context survive.
	var cites []Citation
	for _, r := range out.Citations {
		if ct, ok := byRef[r]; ok {
			cites = append(cites, ct)
		}
	}
	if !out.Abstained && len(cites) == 0 {
		out = ai.AnswerOutput{Abstained: true, Answer: abstain(sess.Language), Uncertainty: abstain(sess.Language)}
	}
	if cites == nil {
		cites = []Citation{}
	}
	userMsg := Message{ID: ids.New(), Role: "user", Content: question, Citations: []Citation{}, CreatedAt: retrievedAt}
	ans := Message{ID: ids.New(), Role: "assistant", Content: out.Answer, Citations: cites, Abstained: out.Abstained, Uncertainty: out.Uncertainty,
		ModelRef: modelRef, RetrievedAt: &retrievedAt, CreatedAt: s.now().UTC()}
	retr, _ := json.Marshal(map[string]any{"uncertainty": out.Uncertainty, "retrieved_at": retrievedAt, "context_refs": len(in.Context), "level": level})
	cb, _ := json.Marshal(cites)
	err = s.db.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO chat_messages (id, session_id, role, content, created_at) VALUES ($1,$2,'user',$3,$4)`, userMsg.ID, sessionID, question, userMsg.CreatedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO chat_messages (id, session_id, role, content, citations, retrieval, abstained, model_ref, created_at)
			VALUES ($1,$2,'assistant',$3,$4,$5,$6,$7,$8)`, ans.ID, sessionID, ans.Content, cb, retr, ans.Abstained, modelRef, ans.CreatedAt); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE chat_sessions SET last_message_at=now() WHERE id=$1`, sessionID)
		return err
	})
	return &ans, err
}

func abstain(lang string) string {
	switch lang {
	case "ru":
		return "В доступных источниках недостаточно проверяемой информации, чтобы ответить на этот вопрос."
	case "en":
		return "The available sources do not contain enough verifiable information to answer this question."
	}
	return "Nu am găsit suficiente informații verificabile în sursele disponibile pentru a răspunde la această întrebare."
}

// Explain produces a one-shot grounded explanation of an event at a given level.
func (s *Service) Explain(ctx context.Context, eventID uuid.UUID, lang, level, question string) (*Message, error) {
	if level == "" {
		level = "short"
	}
	if level != "short" && level != "detailed" && level != "background" {
		return nil, apperr.Validation(apperr.FieldError{Field: "level", Code: "invalid", Message: "short, detailed or background"})
	}
	docs, err := s.eventDocs(ctx, eventID, lang, 0)
	if err != nil {
		return nil, err
	}
	if question == "" {
		question = map[string]string{"ro": "Ce s-a întâmplat și de ce contează?", "ru": "Что произошло и почему это важно?", "en": "What happened and why does it matter?"}[lang]
	}
	now := s.now().UTC()
	in := &ai.AnswerInput{Question: question, Level: level, Language: lang, Now: now.Format(time.RFC3339), ScopeDefault: true}
	byRef := map[string]Citation{}
	for _, d := range docs {
		in.Context = append(in.Context, d.doc)
		byRef[d.doc.Ref] = d.cite
	}
	var out ai.AnswerOutput
	if len(in.Context) == 0 {
		return &Message{Role: "assistant", Content: abstain(lang), Abstained: true, Citations: []Citation{}, CreatedAt: now}, nil
	}
	exec, err := s.gw.GenerateStructured(ctx, ai.TaskExplanation, in, &out, ai.CallOpts{SubjectType: "event", SubjectID: eventID.String()})
	if err != nil {
		return nil, err
	}
	cites := []Citation{}
	for _, r := range out.Citations {
		if ct, ok := byRef[r]; ok {
			cites = append(cites, ct)
		}
	}
	return &Message{Role: "assistant", Content: out.Answer, Citations: cites, Abstained: out.Abstained, Uncertainty: out.Uncertainty, ModelRef: &exec.ModelRef,
		RetrievedAt: &now, CreatedAt: now}, nil
}
