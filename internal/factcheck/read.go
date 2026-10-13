package factcheck

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dorinpuscasu25/recapproject/internal/nlp"
	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
)

// EvidenceView is reader-facing evidence (excerpts only where the source permits snippets).
type EvidenceView struct {
	SourceID      *uuid.UUID `json:"source_id,omitempty"`
	Publisher     string     `json:"publisher,omitempty"`
	URL           string     `json:"url"`
	Relation      string     `json:"relation"`
	SourceKind    string     `json:"source_kind"`
	PrimarySource bool       `json:"primary_source"`
	PublishedAt   *time.Time `json:"published_at,omitempty"`
	RetrievedAt   time.Time  `json:"retrieved_at"`
	Excerpt       string     `json:"excerpt,omitempty"`
}

// FactCheck is the reader-facing representation (see docs/fact-checking-policy.md).
type FactCheck struct {
	ClaimID      uuid.UUID         `json:"claim_id"`
	Claim        string            `json:"claim"`
	Language     string            `json:"language"`
	AttributedTo []string          `json:"attributed_to"`
	Verdict      string            `json:"verdict"`
	Headline     string            `json:"headline"`
	Explanation  string            `json:"explanation"`
	Confidence   *float32          `json:"confidence"`
	ReviewStatus string            `json:"review_status"`
	AssessedAt   time.Time         `json:"assessed_at"`
	ReviewedAt   *time.Time        `json:"reviewed_at,omitempty"`
	FirstSeenAt  time.Time         `json:"first_seen_at"`
	Evidence     []EvidenceView    `json:"evidence"`
	Limitations  []string          `json:"limitations"`
	Correction   map[string]string `json:"correction,omitempty"`
	Version      int               `json:"version"`
	Status       string            `json:"status"`
	Disclaimer   string            `json:"disclaimer"`
}

var disclaimer = map[string]string{
	"ro": "Evaluarea privește această afirmație, nu publicația sau persoana care a făcut-o. Verdictele sunt revizuite când apar dovezi noi.",
	"ru": "Оценка относится к утверждению, а не к изданию или человеку. Вердикты пересматриваются при появлении новых данных.",
	"en": "This assessment concerns the claim, not the publication or person who made it. Verdicts are revised when new evidence appears.",
}

func pickLang(m map[string]string, lang string) string {
	if v := m[lang]; v != "" {
		return v
	}
	for _, l := range []string{"ro", "en", "ru"} {
		if v := m[l]; v != "" {
			return v
		}
	}
	return ""
}

// Get returns the current public fact-check for a claim. Internal confidence is not exposed
// for automatic verdicts.
func (s *Service) Get(ctx context.Context, claimID uuid.UUID, lang string) (*FactCheck, error) {
	f := &FactCheck{ClaimID: claimID, Disclaimer: pickLang(disclaimer, lang)}
	var vid uuid.UUID
	var head, expl, lim, corr []byte
	var evIDs []uuid.UUID
	var conf *float32
	err := s.db.Pool.QueryRow(ctx, `SELECT c.canonical_text, c.language, c.first_seen_at, c.status, v.id, v.verdict, v.review_status, v.headline, v.explanation,
		v.limitations, v.confidence, v.assessed_at, v.reviewed_at, v.correction_note, v.version, v.evidence_ids
		FROM claims c JOIN claim_verdicts v ON v.id=c.current_verdict_id WHERE c.id=$1 AND v.visibility='public' AND c.status <> 'withdrawn'`, claimID).
		Scan(&f.Claim, &f.Language, &f.FirstSeenAt, &f.Status, &vid, &f.Verdict, &f.ReviewStatus, &head, &expl, &lim, &conf, &f.AssessedAt, &f.ReviewedAt,
			&corr, &f.Version, &evIDs)
	if db.IsNoRows(err) {
		return nil, apperr.NotFound("fact_check_not_found", "No published fact-check for this claim.")
	}
	if err != nil {
		return nil, err
	}
	var hm, em map[string]string
	_ = json.Unmarshal(head, &hm)
	_ = json.Unmarshal(expl, &em)
	_ = json.Unmarshal(lim, &f.Limitations)
	if f.Limitations == nil {
		f.Limitations = []string{}
	}
	if corr != nil {
		_ = json.Unmarshal(corr, &f.Correction)
	}
	f.Headline, f.Explanation = pickLang(hm, lang), pickLang(em, lang)
	if f.ReviewStatus == "editor_approved" || f.ReviewStatus == "editor_modified" {
		f.Confidence = conf
	}
	rows, err := s.db.Pool.Query(ctx, `SELECT DISTINCT o.attributed_to FROM claim_occurrences o WHERE o.claim_id=$1 AND o.attributed_to <> ''`, claimID)
	if err != nil {
		return nil, err
	}
	f.AttributedTo, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	if f.AttributedTo == nil {
		f.AttributedTo = []string{}
	}
	// Pending reviews never expose the proposed (possibly negative) evidence relations.
	rows, err = s.db.Pool.Query(ctx, `SELECT e.evidence_article_id, COALESCE(p.name,''), e.evidence_url, e.relation, e.source_kind, e.is_primary_source,
		e.published_at, e.retrieved_at, e.excerpt,
		COALESCE((SELECT sp.allow_snippets FROM source_permissions sp WHERE sp.publisher_id=e.publisher_id ORDER BY sp.source_endpoint_id NULLS FIRST LIMIT 1), e.assessed_by='editor'),
		COALESCE((SELECT sp.max_snippet_chars FROM source_permissions sp WHERE sp.publisher_id=e.publisher_id ORDER BY sp.source_endpoint_id NULLS FIRST LIMIT 1), 300)
		FROM claim_evidence e LEFT JOIN publishers p ON p.id=e.publisher_id
		WHERE e.id = ANY($1) ORDER BY e.is_primary_source DESC, e.published_at`, evIDs)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ev EvidenceView
		var excerpt string
		var allow bool
		var max int
		if err := rows.Scan(&ev.SourceID, &ev.Publisher, &ev.URL, &ev.Relation, &ev.SourceKind, &ev.PrimarySource, &ev.PublishedAt, &ev.RetrievedAt, &excerpt, &allow, &max); err != nil {
			rows.Close()
			return nil, err
		}
		if f.ReviewStatus == "pending_review" && (ev.Relation == "contradicts") {
			ev.Relation = "context" // do not pre-announce an unreviewed negative finding
		}
		if allow && max > 0 {
			ev.Excerpt = nlp.Truncate(excerpt, max)
		}
		f.Evidence = append(f.Evidence, ev)
	}
	rows.Close()
	if f.Evidence == nil {
		f.Evidence = []EvidenceView{}
	}
	return f, nil
}

type HistoryEntry struct {
	Version      int               `json:"version"`
	Verdict      string            `json:"verdict"`
	ReviewStatus string            `json:"review_status"`
	AssessedAt   time.Time         `json:"assessed_at"`
	ReviewedAt   *time.Time        `json:"reviewed_at,omitempty"`
	Correction   map[string]string `json:"correction,omitempty"`
}

// History returns the public verdict history (never erased).
func (s *Service) History(ctx context.Context, claimID uuid.UUID) ([]HistoryEntry, error) {
	rows, err := s.db.Pool.Query(ctx, `SELECT version, verdict, review_status, assessed_at, reviewed_at, correction_note FROM claim_verdicts
		WHERE claim_id=$1 AND visibility='public' ORDER BY version`, claimID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (HistoryEntry, error) {
		var h HistoryEntry
		var corr []byte
		err := r.Scan(&h.Version, &h.Verdict, &h.ReviewStatus, &h.AssessedAt, &h.ReviewedAt, &corr)
		if corr != nil {
			_ = json.Unmarshal(corr, &h.Correction)
		}
		return h, err
	})
}

type ListFilter struct {
	Verdicts []string
	EventID  *uuid.UUID
	Lang     string
	Before   *time.Time
	Limit    int
}

// List returns claims with public verdicts (checkable claims only unless an event is given).
func (s *Service) List(ctx context.Context, f ListFilter) ([]FactCheck, error) {
	if f.Limit <= 0 || f.Limit > 100 {
		f.Limit = 20
	}
	verdicts := f.Verdicts
	if verdicts == nil {
		verdicts = []string{}
	}
	rows, err := s.db.Pool.Query(ctx, `SELECT c.id FROM claims c JOIN claim_verdicts v ON v.id=c.current_verdict_id
		WHERE v.visibility='public' AND c.status <> 'withdrawn' AND (cardinality($1::text[])=0 OR v.verdict = ANY($1))
		AND ($2::uuid IS NULL OR EXISTS (SELECT 1 FROM event_claims ec WHERE ec.claim_id=c.id AND ec.event_id=$2))
		AND ($2::uuid IS NOT NULL OR c.checkability='checkable')
		AND ($3::timestamptz IS NULL OR v.assessed_at < $3)
		ORDER BY v.assessed_at DESC LIMIT $4`, verdicts, f.EventID, f.Before, f.Limit)
	if err != nil {
		return nil, err
	}
	idList, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, err
	}
	out := make([]FactCheck, 0, len(idList))
	for _, id := range idList {
		fc, err := s.Get(ctx, id, f.Lang)
		if err != nil {
			continue
		}
		out = append(out, *fc)
	}
	return out, nil
}

// TrendItem is a claim deserving caution in the monitor.
type TrendItem struct {
	FactCheck
	Publications    int              `json:"publications"`
	Languages       []string         `json:"languages"`
	DailyCounts     map[string]int   `json:"daily_counts"`
	OriginalStories []map[string]any `json:"original_stories"`
	WhyCaution      string           `json:"why_caution"`
}

type Trends struct {
	Title       string           `json:"title"`
	WindowDays  int              `json:"window_days"`
	GeneratedAt time.Time        `json:"generated_at"`
	Narratives  []map[string]any `json:"narratives"`
	Claims      []TrendItem      `json:"claims"`
	Method      string           `json:"method"`
}

var trendTitle = map[string]string{
	"ro": "Afirmații care merită prudență săptămâna aceasta în Moldova",
	"ru": "Утверждения, к которым стоит относиться с осторожностью на этой неделе в Молдове",
	"en": "Claims that deserve caution this week in Moldova",
}

var cautionText = map[string]map[string]string{
	"refuted":            {"ro": "Dovezile disponibile contrazic afirmația.", "ru": "Имеющиеся данные противоречат утверждению.", "en": "Available evidence contradicts the claim."},
	"misleading_context": {"ro": "Afirmația omite context important.", "ru": "В утверждении упущен важный контекст.", "en": "The claim omits important context."},
	"disputed":           {"ro": "Surse credibile se contrazic; nu există încă o concluzie.", "ru": "Достоверные источники расходятся; вывода пока нет.", "en": "Credible sources disagree; there is no conclusion yet."},
}

// Trends builds the misinformation monitor: only editor-reviewed refuted/misleading/disputed
// claims, aggregated by spread. It never names outlets or people as disinformation actors.
func (s *Service) Trends(ctx context.Context, days int, lang string) (*Trends, error) {
	if days <= 0 || days > 90 {
		days = 7
	}
	t := &Trends{Title: pickLang(trendTitle, lang), WindowDays: days, GeneratedAt: time.Now().UTC(),
		Method: "Editor-reviewed claim verdicts (refuted, misleading context, disputed) ranked by number of publications in the window. Similarity alone is never used to allege coordination or intent."}
	rows, err := s.db.Pool.Query(ctx, `SELECT c.id, count(DISTINCT o.article_id) AS pubs FROM claims c JOIN claim_verdicts v ON v.id=c.current_verdict_id
		JOIN claim_occurrences o ON o.claim_id=c.id
		WHERE v.visibility='public' AND v.verdict IN ('refuted','misleading_context','disputed') AND v.review_status IN ('editor_approved','editor_modified')
		  AND c.last_seen_at > now() - make_interval(days => $1) AND c.status <> 'withdrawn'
		GROUP BY c.id ORDER BY pubs DESC, max(c.last_seen_at) DESC LIMIT 20`, days)
	if err != nil {
		return nil, err
	}
	type cp struct {
		id   uuid.UUID
		pubs int
	}
	var list []cp
	for rows.Next() {
		var x cp
		if err := rows.Scan(&x.id, &x.pubs); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, x)
	}
	rows.Close()
	for _, x := range list {
		fc, err := s.Get(ctx, x.id, lang)
		if err != nil {
			continue
		}
		it := TrendItem{FactCheck: *fc, Publications: x.pubs, DailyCounts: map[string]int{}, WhyCaution: pickLang(cautionText[fc.Verdict], lang)}
		r2, err := s.db.Pool.Query(ctx, `SELECT DISTINCT a.language FROM claim_occurrences o JOIN articles a ON a.id=o.article_id WHERE o.claim_id=$1`, x.id)
		if err == nil {
			it.Languages, _ = pgx.CollectRows(r2, pgx.RowTo[string])
		}
		r3, err := s.db.Pool.Query(ctx, `SELECT to_char(date_trunc('day', COALESCE(a.published_at, a.first_seen_at)), 'YYYY-MM-DD'), count(*)
			FROM claim_occurrences o JOIN articles a ON a.id=o.article_id WHERE o.claim_id=$1 GROUP BY 1 ORDER BY 1`, x.id)
		if err == nil {
			for r3.Next() {
				var d string
				var n int
				if r3.Scan(&d, &n) == nil {
					it.DailyCounts[d] = n
				}
			}
			r3.Close()
		}
		r4, err := s.db.Pool.Query(ctx, `SELECT a.canonical_url, a.title, a.published_at FROM claim_occurrences o JOIN articles a ON a.id=o.article_id
			WHERE o.claim_id=$1 AND a.tombstoned_at IS NULL ORDER BY a.published_at LIMIT 5`, x.id)
		if err == nil {
			for r4.Next() {
				var u, title string
				var pub *time.Time
				if r4.Scan(&u, &title, &pub) == nil {
					it.OriginalStories = append(it.OriginalStories, map[string]any{"url": u, "title": title, "published_at": pub})
				}
			}
			r4.Close()
		}
		t.Claims = append(t.Claims, it)
	}
	if t.Claims == nil {
		t.Claims = []TrendItem{}
	}
	nrows, err := s.db.Pool.Query(ctx, `SELECT n.id, n.title, n.summary, n.why_caution, n.first_seen_at, n.last_seen_at, n.languages,
		(SELECT array_agg(claim_id) FROM narrative_claim_links WHERE narrative_id=n.id)
		FROM misinformation_narratives n WHERE n.status='published' AND (n.last_seen_at IS NULL OR n.last_seen_at > now() - make_interval(days => $1))
		ORDER BY n.approved_at DESC LIMIT 20`, days)
	if err != nil {
		return nil, err
	}
	for nrows.Next() {
		var id uuid.UUID
		var title, summary, why []byte
		var first, last *time.Time
		var langs []string
		var claims []uuid.UUID
		if err := nrows.Scan(&id, &title, &summary, &why, &first, &last, &langs, &claims); err != nil {
			nrows.Close()
			return nil, err
		}
		var tm, sm, wm map[string]string
		_ = json.Unmarshal(title, &tm)
		_ = json.Unmarshal(summary, &sm)
		_ = json.Unmarshal(why, &wm)
		t.Narratives = append(t.Narratives, map[string]any{"id": id, "title": pickLang(tm, lang), "summary": pickLang(sm, lang), "why_caution": pickLang(wm, lang),
			"first_seen_at": first, "last_seen_at": last, "languages": langs, "claim_ids": claims})
	}
	nrows.Close()
	if t.Narratives == nil {
		t.Narratives = []map[string]any{}
	}
	return t, nil
}
