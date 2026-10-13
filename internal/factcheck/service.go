package factcheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dorinpuscasu25/recapproject/internal/ai"
	"github.com/dorinpuscasu25/recapproject/internal/events"
	"github.com/dorinpuscasu25/recapproject/internal/nlp"
	"github.com/dorinpuscasu25/recapproject/internal/platform/bus"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ids"
	"github.com/dorinpuscasu25/recapproject/internal/policy"
	"github.com/dorinpuscasu25/recapproject/internal/search"
)

type Service struct {
	db     *db.DB
	gw     *ai.Gateway
	search *search.Service
	pol    *policy.Store
	log    *slog.Logger
}

func NewService(d *db.DB, gw *ai.Gateway, s *search.Service, pol *policy.Store, log *slog.Logger) *Service {
	return &Service{db: d, gw: gw, search: s, pol: pol, log: log}
}

func (s *Service) policy(ctx context.Context) (Policy, int) {
	p := DefaultPolicy()
	ver, _ := s.pol.Get(ctx, "factcheck", &p)
	if p.PolicyVersion == 0 {
		p.PolicyVersion = max(ver, 1)
	}
	return p, ver
}

// VerdictUpdated is the claim.verdict.updated.v1 payload.
type VerdictUpdated struct {
	ClaimID      uuid.UUID `json:"claim_id"`
	VerdictID    uuid.UUID `json:"verdict_id"`
	Version      int       `json:"version"`
	Verdict      string    `json:"verdict"`
	Proposed     string    `json:"proposed_verdict"`
	ReviewStatus string    `json:"review_status"`
}

type claimRow struct {
	ID           uuid.UUID
	Text         string
	Language     string
	Checkability string
	Risk         string
	FirstSeen    time.Time
	Status       string
	Occurrences  int
}

func (s *Service) loadClaim(ctx context.Context, q db.Querier, id uuid.UUID) (*claimRow, error) {
	c := &claimRow{ID: id}
	err := q.QueryRow(ctx, `SELECT canonical_text, language, checkability, risk_level, first_seen_at, status, occurrence_count FROM claims WHERE id=$1`, id).
		Scan(&c.Text, &c.Language, &c.Checkability, &c.Risk, &c.FirstSeen, &c.Status, &c.Occurrences)
	return c, err
}

// HandleVerification is the consumer for claim.verification.requested.v1 and content.deleted.v1.
func (s *Service) HandleVerification(ctx context.Context, d *bus.Delivery) error {
	if done, err := d.AlreadyProcessed(ctx); err != nil || done {
		return err
	}
	var claimIDs []uuid.UUID
	reason := ""
	switch d.Message.Type {
	case bus.ClaimVerificationRequested:
		var msg events.VerificationRequested
		if err := d.Message.Decode(&msg); err != nil {
			return bus.PermanentErr(err)
		}
		claimIDs, reason = []uuid.UUID{msg.ClaimID}, msg.Reason
	case bus.ContentDeleted:
		var msg struct {
			DocType string    `json:"doc_type"`
			ID      uuid.UUID `json:"id"`
		}
		if err := d.Message.Decode(&msg); err != nil {
			return bus.PermanentErr(err)
		}
		if msg.DocType != "article" {
			return d.Once(ctx, func(pgx.Tx) error { return nil })
		}
		// Evidence from a withdrawn/quarantined article is superseded; affected claims are reopened.
		rows, err := s.db.Pool.Query(ctx, `UPDATE claim_evidence SET superseded_at=now() WHERE evidence_article_id=$1 AND superseded_at IS NULL RETURNING claim_id`, msg.ID)
		if err != nil {
			return err
		}
		claimIDs, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return err
		}
		reason = "evidence_withdrawn"
	default:
		return nil
	}
	for _, id := range uniqueIDs(claimIDs) {
		if err := s.Verify(ctx, id, reason); err != nil {
			if errors.Is(err, ai.ErrDeferred) {
				return err // retry later; never fabricate a verdict
			}
			if db.IsNoRows(err) {
				continue
			}
			return err
		}
	}
	return d.Once(ctx, func(pgx.Tx) error { return nil })
}

func uniqueIDs(in []uuid.UUID) []uuid.UUID {
	seen := map[uuid.UUID]bool{}
	var out []uuid.UUID
	for _, x := range in {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

type evidenceCand struct {
	ArticleID   uuid.UUID
	PublisherID uuid.UUID
	Publisher   string
	PubType     string
	URL         string
	Title       string
	Body        string
	Published   *time.Time
	DupOf       *uuid.UUID
	DupPub      *uuid.UUID
	Scores      map[string]float64
}

// retrieveEvidence finds candidate evidence documents: independently published articles (not the
// ones where the claim occurs, not from the same publishers) via lexical + semantic retrieval
// and via the claim's events.
func (s *Service) retrieveEvidence(ctx context.Context, c *claimRow, p Policy) ([]*evidenceCand, error) {
	rows, err := s.db.Pool.Query(ctx, `SELECT DISTINCT o.article_id, a.publisher_id FROM claim_occurrences o JOIN articles a ON a.id=o.article_id WHERE o.claim_id=$1`, c.ID)
	if err != nil {
		return nil, err
	}
	excludeArt, excludePub := map[uuid.UUID]bool{}, map[uuid.UUID]bool{}
	for rows.Next() {
		var aid, pid uuid.UUID
		if err := rows.Scan(&aid, &pid); err != nil {
			rows.Close()
			return nil, err
		}
		excludeArt[aid], excludePub[pid] = true, true
	}
	rows.Close()
	from := c.FirstSeen.Add(-time.Duration(p.EvidenceWindowDays) * 24 * time.Hour)
	scores := map[uuid.UUID]map[string]float64{}
	addC := func(id uuid.UUID, k string, v float64) {
		if excludeArt[id] {
			return
		}
		if scores[id] == nil {
			scores[id] = map[string]float64{}
		}
		scores[id][k] = v
	}
	if hits, err := s.search.Lexical().Search(ctx, search.LexicalQuery{Text: c.Text, DocTypes: []string{"article"}, From: &from, Limit: 15}); err == nil {
		for i, h := range hits {
			if id, err := uuid.Parse(h.ID); err == nil {
				addC(id, "lexical_rank", float64(i+1))
			}
		}
	} else {
		s.log.Warn("lexical evidence retrieval unavailable", "error", err)
	}
	if vec, _, err := s.search.EmbedActive(ctx, "article", c.Text); err == nil {
		if hits, err := s.search.SimilarByVector(ctx, "article", vec, search.VectorFilter{RangeGTE: map[string]float64{"published_ts": float64(from.Unix())}}, 15); err == nil {
			for _, h := range hits {
				if h.Score > 0.15 {
					addC(h.ID, "semantic", h.Score)
				}
			}
		}
	}
	// Articles covering the same events.
	rows, err = s.db.Pool.Query(ctx, `SELECT l.article_id FROM event_claims ec JOIN event_article_links l ON l.event_id=ec.event_id AND l.unlinked_at IS NULL
		WHERE ec.claim_id=$1 LIMIT 30`, c.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		addC(id, "same_event", 1)
	}
	rows.Close()
	var out []*evidenceCand
	for id, sc := range scores {
		e := &evidenceCand{ArticleID: id, Scores: sc}
		var body *string
		err := s.db.Pool.QueryRow(ctx, `SELECT a.publisher_id, p.name, p.publisher_type, a.canonical_url, a.title, a.body_text, a.published_at, a.duplicate_of,
			(SELECT publisher_id FROM articles o WHERE o.id=a.duplicate_of)
			FROM articles a JOIN publishers p ON p.id=a.publisher_id
			WHERE a.id=$1 AND a.status IN ('enriched','duplicate') AND a.tombstoned_at IS NULL AND a.content_type NOT IN ('satire','opinion')`, id).
			Scan(&e.PublisherID, &e.Publisher, &e.PubType, &e.URL, &e.Title, &body, &e.Published, &e.DupOf, &e.DupPub)
		if db.IsNoRows(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if excludePub[e.PublisherID] || body == nil {
			continue
		}
		e.Body = *body
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		return evScore(out[i]) > evScore(out[j])
	})
	if len(out) > 10 {
		out = out[:10]
	}
	return out, nil
}

func evScore(e *evidenceCand) float64 {
	s := e.Scores["semantic"] + e.Scores["same_event"]*0.5
	if r, ok := e.Scores["lexical_rank"]; ok {
		s += 1 / (1 + r)
	}
	return s
}

func independenceKey(e *evidenceCand) string {
	if e.DupPub != nil {
		return "publisher:" + e.DupPub.String() // syndicated copy counts with its original
	}
	return "publisher:" + e.PublisherID.String()
}

func isPrimary(pubType string) bool {
	return pubType == "government" || pubType == "public_institution"
}

func sourceKind(pubType string) string {
	switch pubType {
	case "government", "public_institution":
		return "official"
	case "fact_checker":
		return "fact_checker"
	case "newsroom":
		return "newsroom"
	}
	return "other"
}

// Verify re-evaluates one claim and records a new verdict version if anything changed.
func (s *Service) Verify(ctx context.Context, claimID uuid.UUID, reason string) error {
	p, _ := s.policy(ctx)
	c, err := s.loadClaim(ctx, s.db.Pool, claimID)
	if err != nil {
		return err
	}
	if c.Status == "withdrawn" {
		return nil
	}
	var assessments []ai.EvidenceAssessment
	var cands []*evidenceCand
	modelRef := "rules"
	if c.Checkability == "checkable" {
		cands, err = s.retrieveEvidence(ctx, c, p)
		if err != nil {
			return err
		}
		if len(cands) > 0 {
			in := &ai.EvidenceInput{Claim: c.Text, ClaimDate: c.FirstSeen.Format("2006-01-02"), Language: c.Language}
			for i, e := range cands {
				pub := ""
				if e.Published != nil {
					pub = e.Published.Format("2006-01-02")
				}
				in.Evidence = append(in.Evidence, ai.EvidenceItem{Ref: fmt.Sprintf("e%d", i+1), Publisher: e.Publisher, SourceKind: sourceKind(e.PubType),
					PublishedAt: pub, Text: nlp.Truncate(e.Body, 4000)})
			}
			var out ai.EvidenceOutput
			exec, err := s.gw.GenerateStructured(ctx, ai.TaskEvidenceAssessment, in, &out, ai.CallOpts{SubjectType: "claim", SubjectID: c.ID.String()})
			if err != nil {
				return err
			}
			assessments, modelRef = out.Assessments, exec.ModelRef
		}
	}
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM claims WHERE id=$1 FOR UPDATE`, c.ID); err != nil {
			return err
		}
		// Supersede previous model/rule evidence; editor-provided evidence is kept.
		if _, err := tx.Exec(ctx, `UPDATE claim_evidence SET superseded_at=now() WHERE claim_id=$1 AND assessed_by <> 'editor' AND superseded_at IS NULL`, c.ID); err != nil {
			return err
		}
		byRef := map[string]*evidenceCand{}
		for i, e := range cands {
			byRef[fmt.Sprintf("e%d", i+1)] = e
		}
		for _, a := range assessments {
			e := byRef[a.Ref]
			if e == nil || a.Relation == "irrelevant" {
				continue
			}
			var start, end *int
			if a.Quote != "" {
				if i := strings.Index(e.Body, a.Quote); i >= 0 {
					st, en := i, i+len(a.Quote)
					start, end = &st, &en
				}
			}
			scores, _ := json.Marshal(e.Scores)
			if _, err := tx.Exec(ctx, `INSERT INTO claim_evidence (id, claim_id, evidence_article_id, evidence_url, publisher_id, source_kind, relation, excerpt,
				start_offset, end_offset, rationale, independence_key, is_primary_source, published_at, assessed_by, model_ref, retrieval_scores)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'model',$15,$16)`,
				ids.New(), c.ID, e.ArticleID, e.URL, e.PublisherID, sourceKind(e.PubType), a.Relation, nlp.Truncate(a.Quote, 1500), start, end,
				nlp.Truncate(a.Rationale, 500), independenceKey(e), isPrimary(e.PubType), e.Published, modelRef, scores); err != nil {
				return err
			}
		}
		return s.decideAndRecord(ctx, tx, c, p, modelRef, reason)
	})
}

// decideAndRecord computes the verdict from all current evidence and writes a new version when
// the outcome changed. High-risk/negative verdicts are held for review (readers see
// "unverified" + pending_review until an editor decides).
func (s *Service) decideAndRecord(ctx context.Context, tx pgx.Tx, c *claimRow, p Policy, modelRef, reason string) error {
	rows, err := tx.Query(ctx, `SELECT id, relation, independence_key, is_primary_source, published_at FROM claim_evidence WHERE claim_id=$1 AND superseded_at IS NULL`, c.ID)
	if err != nil {
		return err
	}
	var evs []Evidence
	var evIDs []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		var e Evidence
		if err := rows.Scan(&id, &e.Relation, &e.IndependenceKey, &e.IsPrimary, &e.PublishedAt); err != nil {
			rows.Close()
			return err
		}
		evs = append(evs, e)
		evIDs = append(evIDs, id)
	}
	rows.Close()
	sig := Collapse(evs)
	dec := Decide(p, c.Checkability, c.Risk, sig)
	// Current state.
	var curVerdict, curProposed, curReview *string
	var curVersion int
	var curEvidence []uuid.UUID
	_ = tx.QueryRow(ctx, `SELECT v.verdict, v.proposed_verdict, v.review_status, v.version, v.evidence_ids FROM claims c JOIN claim_verdicts v ON v.id=c.current_verdict_id
		WHERE c.id=$1`, c.ID).Scan(&curVerdict, &curProposed, &curReview, &curVersion, &curEvidence)
	// Editor decisions are authoritative until new evidence changes the proposal.
	editorial := curReview != nil && strings.HasPrefix(*curReview, "editor_")
	publicVerdict, review, visibility := dec.Verdict, "automatic_preliminary", "public"
	if dec.RequiresReview {
		publicVerdict, review = "unverified", "pending_review"
	}
	if editorial && curProposed != nil && *curProposed == dec.Verdict {
		return nil // nothing new for the editor's decision
	}
	if curVerdict != nil && !editorial && *curVerdict == publicVerdict && curProposed != nil && *curProposed == dec.Verdict && sameIDs(curEvidence, evIDs) {
		return nil // unchanged
	}
	if editorial {
		// New material evidence reopens an editor-reviewed claim: keep the published verdict,
		// queue a review with the new proposal.
		publicVerdict = *curVerdict
		review = "pending_review"
	}
	headline, explanation := map[string]string{}, map[string]string{}
	for _, l := range []string{"ro", "ru", "en"} {
		headline[l] = Headline(publicVerdict, l)
		explanation[l] = Explanation(publicVerdict, l, sig, review == "pending_review")
	}
	signals, _ := json.Marshal(map[string]any{"signals": sig, "review_reasons": dec.ReviewReasons, "proposed_confidence": dec.Confidence, "reason": reason})
	vid := ids.New()
	version := curVersion + 1
	var supersedes *uuid.UUID
	if curVersion > 0 {
		var prev uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT current_verdict_id FROM claims WHERE id=$1`, c.ID).Scan(&prev); err == nil {
			supersedes = &prev
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO claim_verdicts (id, claim_id, version, verdict, proposed_verdict, review_status, visibility, headline, explanation,
		limitations, confidence, evidence_ids, rationale, signals, policy_version, model_ref, supersedes_id, published_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17, now())`,
		vid, c.ID, version, publicVerdict, dec.Verdict, review, visibility, mustJSON(headline), mustJSON(explanation), mustJSON(nonNilS(dec.Limitations)),
		dec.Confidence, nonNilU(evIDs), strings.Join(dec.ReviewReasons, ","), signals, p.PolicyVersion, modelRef, supersedes); err != nil {
		return err
	}
	if supersedes != nil && !editorial {
		if _, err := tx.Exec(ctx, `UPDATE claim_verdicts SET review_status='superseded' WHERE id=$1 AND review_status IN ('automatic_preliminary','pending_review')`, *supersedes); err != nil {
			return err
		}
	}
	status := "pending"
	switch {
	case review == "pending_review":
		status = "pending"
	case publicVerdict == "disputed":
		status = "contested"
	case publicVerdict != "unverified":
		status = "fact_checked"
	}
	if _, err := tx.Exec(ctx, `UPDATE claims SET current_verdict_id=$2, status=CASE WHEN status IN ('corrected','withdrawn') THEN status ELSE $3 END,
		needs_reverification=false, version=version+1, updated_at=now() WHERE id=$1`, c.ID, vid, status); err != nil {
		return err
	}
	if review == "pending_review" {
		var sources int
		_ = tx.QueryRow(ctx, `SELECT count(DISTINCT a.publisher_id) FROM claim_occurrences o JOIN articles a ON a.id=o.article_id WHERE o.claim_id=$1`, c.ID).Scan(&sources)
		score, factors := Priority(c.Occurrences, sources, dec.Confidence, c.Risk, dec.Verdict)
		if _, err := tx.Exec(ctx, `INSERT INTO fact_check_reviews (id, claim_id, verdict_id, reason, priority_score, priority_factors)
			VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (claim_id) WHERE status IN ('open','in_progress')
			DO UPDATE SET verdict_id=EXCLUDED.verdict_id, reason=EXCLUDED.reason, priority_score=EXCLUDED.priority_score,
			  priority_factors=EXCLUDED.priority_factors, updated_at=now(), version=fact_check_reviews.version+1`,
			ids.New(), c.ID, vid, "proposed:"+dec.Verdict+" "+strings.Join(dec.ReviewReasons, ","), score, mustJSON(factors)); err != nil {
			return err
		}
	}
	_, err = bus.Publish(ctx, tx, bus.Draft{Type: bus.ClaimVerdictUpdated, AggregateType: "claim", AggregateID: c.ID.String(),
		Payload: VerdictUpdated{ClaimID: c.ID, VerdictID: vid, Version: version, Verdict: publicVerdict, Proposed: dec.Verdict, ReviewStatus: review}})
	return err
}

func sameIDs(a, b []uuid.UUID) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[uuid.UUID]bool{}
	for _, x := range a {
		m[x] = true
	}
	for _, x := range b {
		if !m[x] {
			return false
		}
	}
	return true
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func nonNilS(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nonNilU(s []uuid.UUID) []uuid.UUID {
	if s == nil {
		return []uuid.UUID{}
	}
	return s
}
