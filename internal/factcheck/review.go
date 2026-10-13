package factcheck

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dorinpuscasu25/recapproject/internal/audit"
	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/bus"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ids"
	"github.com/dorinpuscasu25/recapproject/internal/platform/principal"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ssrf"
)

var validVerdicts = map[string]bool{"supported": true, "refuted": true, "misleading_context": true, "partially_supported": true,
	"disputed": true, "unverified": true, "not_checkable": true, "outdated": true}

type ReviewItem struct {
	ID              uuid.UUID          `json:"id"`
	ClaimID         uuid.UUID          `json:"claim_id"`
	Claim           string             `json:"claim"`
	Language        string             `json:"language"`
	RiskLevel       string             `json:"risk_level"`
	Status          string             `json:"status"`
	Reason          string             `json:"reason"`
	PriorityScore   float32            `json:"priority_score"`
	PriorityFactors map[string]float64 `json:"priority_factors"`
	ProposedVerdict *string            `json:"proposed_verdict,omitempty"`
	PublicVerdict   *string            `json:"public_verdict,omitempty"`
	Decision        *string            `json:"decision,omitempty"`
	DecisionVerdict *string            `json:"decision_verdict,omitempty"`
	CreatedAt       time.Time          `json:"created_at"`
	Version         int                `json:"version"`
}

// Queue lists reviews ordered by priority (reach × uncertainty × impact × harm).
func (s *Service) Queue(ctx context.Context, status string, limit int) ([]ReviewItem, error) {
	statuses := []string{"open", "in_progress"}
	if status != "" {
		statuses = []string{status}
	}
	rows, err := s.db.Pool.Query(ctx, `SELECT r.id, r.claim_id, c.canonical_text, c.language, c.risk_level, r.status, r.reason, r.priority_score, r.priority_factors,
		v.proposed_verdict, v.verdict, r.decision, r.decision_verdict, r.created_at, r.version
		FROM fact_check_reviews r JOIN claims c ON c.id=r.claim_id LEFT JOIN claim_verdicts v ON v.id=r.verdict_id
		WHERE r.status = ANY($1) ORDER BY r.priority_score DESC, r.created_at LIMIT $2`, statuses, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ReviewItem, error) {
		var x ReviewItem
		var factors []byte
		err := r.Scan(&x.ID, &x.ClaimID, &x.Claim, &x.Language, &x.RiskLevel, &x.Status, &x.Reason, &x.PriorityScore, &factors, &x.ProposedVerdict,
			&x.PublicVerdict, &x.Decision, &x.DecisionVerdict, &x.CreatedAt, &x.Version)
		_ = json.Unmarshal(factors, &x.PriorityFactors)
		return x, err
	})
}

// ClaimDossier returns everything an editor needs: verbatim spans, evidence graph, verdict
// history, reviews and model provenance.
func (s *Service) ClaimDossier(ctx context.Context, id uuid.UUID) (map[string]any, error) {
	var raw []byte
	err := s.db.Pool.QueryRow(ctx, `SELECT row_to_json(x) FROM (SELECT c.id, c.canonical_text, c.language, c.checkability, c.claim_type, c.risk_level,
		c.status, c.first_seen_at, c.last_seen_at, c.occurrence_count, c.current_verdict_id, c.version,
		(SELECT coalesce(json_agg(json_build_object('article_id', o.article_id, 'event_id', o.event_id, 'quote', o.quote_text, 'start', o.start_offset,
		   'end', o.end_offset, 'attributed_to', o.attributed_to, 'is_direct_quote', o.is_direct_quote, 'asserted_at', o.asserted_at,
		   'publisher', p.name, 'url', a.canonical_url, 'title', a.title, 'model_ref', o.extraction_model_ref) ORDER BY o.asserted_at),'[]')
		   FROM claim_occurrences o JOIN articles a ON a.id=o.article_id JOIN publishers p ON p.id=a.publisher_id WHERE o.claim_id=c.id) AS occurrences,
		(SELECT coalesce(json_agg(json_build_object('id', e.id, 'relation', e.relation, 'url', e.evidence_url, 'article_id', e.evidence_article_id,
		   'publisher_id', e.publisher_id, 'source_kind', e.source_kind, 'excerpt', e.excerpt, 'start', e.start_offset, 'end', e.end_offset,
		   'rationale', e.rationale, 'independence_key', e.independence_key, 'is_primary_source', e.is_primary_source, 'published_at', e.published_at,
		   'assessed_by', e.assessed_by, 'model_ref', e.model_ref, 'retrieval_scores', e.retrieval_scores, 'superseded_at', e.superseded_at)
		   ORDER BY e.created_at),'[]') FROM claim_evidence e WHERE e.claim_id=c.id) AS evidence,
		(SELECT coalesce(json_agg(json_build_object('id', v.id, 'version', v.version, 'verdict', v.verdict, 'proposed_verdict', v.proposed_verdict,
		   'review_status', v.review_status, 'confidence', v.confidence, 'rationale', v.rationale, 'signals', v.signals, 'policy_version', v.policy_version,
		   'model_ref', v.model_ref, 'assessed_at', v.assessed_at, 'reviewed_by', v.reviewed_by, 'reviewed_at', v.reviewed_at,
		   'correction_note', v.correction_note, 'explanation', v.explanation) ORDER BY v.version),'[]') FROM claim_verdicts v WHERE v.claim_id=c.id) AS verdicts,
		(SELECT coalesce(json_agg(json_build_object('id', r.id, 'status', r.status, 'reason', r.reason, 'priority_score', r.priority_score,
		   'decision', r.decision, 'decision_verdict', r.decision_verdict, 'decision_rationale', r.decision_rationale, 'reviewer_id', r.reviewer_id,
		   'decided_at', r.decided_at) ORDER BY r.created_at),'[]') FROM fact_check_reviews r WHERE r.claim_id=c.id) AS reviews
		FROM claims c WHERE c.id=$1) x`, id).Scan(&raw)
	if db.IsNoRows(err) {
		return nil, apperr.NotFound("claim_not_found", "Claim not found.")
	}
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = json.Unmarshal(raw, &out)
	return out, err
}

type EvidenceInput struct {
	URL         string     `json:"url"`
	ArticleID   *uuid.UUID `json:"article_id,omitempty"`
	Relation    string     `json:"relation"`
	SourceKind  string     `json:"source_kind"`
	Excerpt     string     `json:"excerpt"`
	Rationale   string     `json:"rationale"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	IsPrimary   bool       `json:"is_primary_source"`
}

// AddEvidence records editor-provided evidence (a reference; the URL is validated, not fetched)
// and re-evaluates the claim.
func (s *Service) AddEvidence(ctx context.Context, claimID uuid.UUID, in EvidenceInput, policy ssrf.Policy) error {
	var v apperr.Validator
	v.Check(in.Relation == "supports" || in.Relation == "contradicts" || in.Relation == "context", "relation", "invalid", "supports, contradicts or context")
	v.Check(in.SourceKind == "official" || in.SourceKind == "newsroom" || in.SourceKind == "fact_checker" || in.SourceKind == "document" || in.SourceKind == "editor_provided" || in.SourceKind == "other",
		"source_kind", "invalid", "Invalid source kind.")
	v.Check(strings.TrimSpace(in.Rationale) != "", "rationale", "required", "A rationale is required.")
	if in.URL != "" {
		if _, err := policy.ValidateURL(in.URL); err != nil {
			v.Add("url", "blocked", err.Error())
		}
	} else if in.ArticleID == nil {
		v.Add("url", "required", "url or article_id is required.")
	}
	if err := v.Err(); err != nil {
		return err
	}
	p, _ := s.policy(ctx)
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		c, err := s.loadClaim(ctx, tx, claimID)
		if db.IsNoRows(err) {
			return apperr.NotFound("claim_not_found", "Claim not found.")
		}
		if err != nil {
			return err
		}
		url, key := in.URL, "url:"+strings.ToLower(in.URL)
		var pub *uuid.UUID
		if in.ArticleID != nil {
			var pid uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT canonical_url, publisher_id FROM articles WHERE id=$1`, *in.ArticleID).Scan(&url, &pid); err != nil {
				return apperr.ValidationMsg("article_not_found", "Unknown article_id.")
			}
			pub, key = &pid, "publisher:"+pid.String()
		}
		var by *uuid.UUID
		if pr := principal.From(ctx); pr != nil {
			by = &pr.ID
		}
		if _, err := tx.Exec(ctx, `INSERT INTO claim_evidence (id, claim_id, evidence_article_id, evidence_url, publisher_id, source_kind, relation, excerpt, rationale,
			independence_key, is_primary_source, published_at, assessed_by, added_by) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'editor',$13)`,
			ids.New(), claimID, in.ArticleID, url, pub, in.SourceKind, in.Relation, in.Excerpt, in.Rationale, key, in.IsPrimary, in.PublishedAt, by); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, audit.Entry{Action: "claim.evidence_added", ResourceType: "claim", ResourceID: claimID.String(), New: in}); err != nil {
			return err
		}
		return s.decideAndRecord(ctx, tx, c, p, "editor", "editor_evidence")
	})
}

type DecisionInput struct {
	Decision    string            `json:"decision"` // accept | modify | reject | request_more_evidence
	Verdict     string            `json:"verdict,omitempty"`
	Headline    map[string]string `json:"headline,omitempty"`
	Explanation map[string]string `json:"explanation,omitempty"`
	Rationale   string            `json:"rationale"`
	Version     *int              `json:"version,omitempty"`
}

// Decide records an editor decision. Definitive negative labels additionally require the
// factcheck:publish permission; otherwise the review stays in_progress awaiting a publisher.
func (s *Service) Decide(ctx context.Context, reviewID uuid.UUID, in DecisionInput) (*ReviewItem, error) {
	p := principal.From(ctx)
	var v apperr.Validator
	v.Check(in.Decision == "accept" || in.Decision == "modify" || in.Decision == "reject" || in.Decision == "request_more_evidence", "decision", "invalid",
		"accept, modify, reject or request_more_evidence")
	v.Check(strings.TrimSpace(in.Rationale) != "", "rationale", "required", "A rationale is required.")
	if in.Decision == "modify" {
		v.Check(validVerdicts[in.Verdict], "verdict", "invalid", "A valid verdict is required for modify.")
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	fp, _ := s.policy(ctx)
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		var claimID uuid.UUID
		var verdictID *uuid.UUID
		var status string
		var version int
		err := tx.QueryRow(ctx, `SELECT claim_id, verdict_id, status, version FROM fact_check_reviews WHERE id=$1 FOR UPDATE`, reviewID).Scan(&claimID, &verdictID, &status, &version)
		if db.IsNoRows(err) {
			return apperr.NotFound("review_not_found", "Review not found.")
		}
		if err != nil {
			return err
		}
		if status == "resolved" || status == "dismissed" {
			return apperr.Conflict("review_closed", "This review is already closed.")
		}
		if in.Version != nil && *in.Version != version {
			return apperr.VersionConflict("review")
		}
		var proposed *string
		if verdictID != nil {
			_ = tx.QueryRow(ctx, `SELECT proposed_verdict FROM claim_verdicts WHERE id=$1`, *verdictID).Scan(&proposed)
		}
		final := ""
		review := ""
		switch in.Decision {
		case "accept":
			if proposed == nil {
				return apperr.ValidationMsg("nothing_to_accept", "There is no proposed verdict.")
			}
			final, review = *proposed, "editor_approved"
		case "modify":
			final, review = in.Verdict, "editor_modified"
		case "reject":
			final, review = "unverified", "editor_rejected"
		case "request_more_evidence":
			_, err := tx.Exec(ctx, `UPDATE fact_check_reviews SET status='in_progress', decision='request_more_evidence', decision_rationale=$2, reviewer_id=$3,
				updated_at=now(), version=version+1 WHERE id=$1`, reviewID, in.Rationale, p.ID)
			if err != nil {
				return err
			}
			return audit.Record(ctx, tx, audit.Entry{Action: "factcheck.more_evidence_requested", ResourceType: "fact_check_review", ResourceID: reviewID.String(), New: in})
		}
		gated := false
		for _, rv := range fp.ReviewRequiredVerdicts {
			if rv == final {
				gated = true
			}
		}
		if gated && !p.Can("factcheck:publish") {
			_, err := tx.Exec(ctx, `UPDATE fact_check_reviews SET status='in_progress', decision=$2, decision_verdict=$3, decision_rationale=$4, reviewer_id=$5,
				updated_at=now(), version=version+1 WHERE id=$1`, reviewID, in.Decision, final, in.Rationale, p.ID)
			if err != nil {
				return err
			}
			return audit.Record(ctx, tx, audit.Entry{Action: "factcheck.decision_pending_publication", ResourceType: "fact_check_review", ResourceID: reviewID.String(), New: in})
		}
		if err := s.publishVerdict(ctx, tx, claimID, final, review, in.Headline, in.Explanation, in.Rationale, nil); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE fact_check_reviews SET status='resolved', decision=$2, decision_verdict=$3, decision_rationale=$4, reviewer_id=$5,
			decided_at=now(), updated_at=now(), version=version+1 WHERE id=$1`, reviewID, in.Decision, final, in.Rationale, p.ID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "factcheck.decision", ResourceType: "fact_check_review", ResourceID: reviewID.String(),
			New: map[string]any{"decision": in.Decision, "verdict": final, "rationale": in.Rationale, "claim_id": claimID}})
	})
	if err != nil {
		return nil, err
	}
	items, err := s.Queue(ctx, "", 1000)
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		if it.ID == reviewID {
			return &it, nil
		}
	}
	all, err := s.Queue(ctx, "resolved", 1000)
	if err != nil {
		return nil, err
	}
	for _, it := range all {
		if it.ID == reviewID {
			return &it, nil
		}
	}
	return nil, nil
}

// Publish finalizes a decision recorded by a fact-checker without publish permission.
func (s *Service) Publish(ctx context.Context, reviewID uuid.UUID) error {
	p := principal.From(ctx)
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		var claimID uuid.UUID
		var status string
		var dv, decision, rationale *string
		err := tx.QueryRow(ctx, `SELECT claim_id, status, decision_verdict, decision, decision_rationale FROM fact_check_reviews WHERE id=$1 FOR UPDATE`, reviewID).
			Scan(&claimID, &status, &dv, &decision, &rationale)
		if db.IsNoRows(err) {
			return apperr.NotFound("review_not_found", "Review not found.")
		}
		if err != nil {
			return err
		}
		if status != "in_progress" || dv == nil || decision == nil || *decision == "request_more_evidence" {
			return apperr.Conflict("review_not_publishable", "No pending editor decision to publish.")
		}
		review := map[string]string{"accept": "editor_approved", "modify": "editor_modified", "reject": "editor_rejected"}[*decision]
		if err := s.publishVerdict(ctx, tx, claimID, *dv, review, nil, nil, deref(rationale), nil); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE fact_check_reviews SET status='resolved', decided_at=now(), updated_at=now(), version=version+1 WHERE id=$1`, reviewID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "factcheck.published", ResourceType: "fact_check_review", ResourceID: reviewID.String(),
			New: map[string]any{"verdict": *dv, "publisher_id": p.ID}})
	})
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// publishVerdict writes a new reader-visible verdict version authored by an editor.
func (s *Service) publishVerdict(ctx context.Context, tx pgx.Tx, claimID uuid.UUID, verdict, review string, headline, explanation map[string]string,
	rationale string, correction map[string]string) error {
	p := principal.From(ctx)
	var curID uuid.UUID
	var curVersion int
	var curProposed *string
	var sig []byte
	var ev []uuid.UUID
	var limitations []byte
	var conf *float32
	err := tx.QueryRow(ctx, `SELECT v.id, v.version, v.proposed_verdict, v.signals, v.evidence_ids, v.limitations, v.confidence FROM claims c
		JOIN claim_verdicts v ON v.id=c.current_verdict_id WHERE c.id=$1`, claimID).Scan(&curID, &curVersion, &curProposed, &sig, &ev, &limitations, &conf)
	if err != nil && !db.IsNoRows(err) {
		return err
	}
	var signals struct {
		Signals Signals `json:"signals"`
	}
	_ = json.Unmarshal(sig, &signals)
	h, e := map[string]string{}, map[string]string{}
	for _, l := range []string{"ro", "ru", "en"} {
		h[l] = Headline(verdict, l)
		e[l] = Explanation(verdict, l, signals.Signals, false)
	}
	for k, v := range headline {
		if v != "" && (k == "ro" || k == "ru" || k == "en") {
			h[k] = v
		}
	}
	for k, v := range explanation {
		if v != "" && (k == "ro" || k == "ru" || k == "en") {
			e[k] = v
		}
	}
	fp, _ := s.policy(ctx)
	vid := ids.New()
	var corr any
	if correction != nil {
		corr = mustJSON(correction)
	}
	if limitations == nil {
		limitations = []byte("[]")
	}
	var supersedes *uuid.UUID
	if curID != uuid.Nil {
		supersedes = &curID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO claim_verdicts (id, claim_id, version, verdict, proposed_verdict, review_status, visibility, headline, explanation,
		limitations, confidence, evidence_ids, rationale, signals, policy_version, model_ref, reviewed_by, reviewed_at, published_at, correction_note, supersedes_id)
		VALUES ($1,$2,$3,$4,$5,$6,'public',$7,$8,$9,$10,$11,$12,$13,$14,'editor',$15,now(),now(),$16,$17)`,
		vid, claimID, curVersion+1, verdict, curProposed, review, mustJSON(h), mustJSON(e), limitations, conf, nonNilU(ev), rationale, sigOrEmpty(sig),
		fp.PolicyVersion, p.ID, corr, supersedes); err != nil {
		return err
	}
	status := "fact_checked"
	switch {
	case correction != nil:
		status = "corrected"
	case verdict == "disputed":
		status = "contested"
	case verdict == "unverified":
		status = "pending"
	}
	if _, err := tx.Exec(ctx, `UPDATE claims SET current_verdict_id=$2, status=$3, version=version+1, updated_at=now() WHERE id=$1`, claimID, vid, status); err != nil {
		return err
	}
	_, err = bus.Publish(ctx, tx, bus.Draft{Type: bus.ClaimVerdictUpdated, AggregateType: "claim", AggregateID: claimID.String(),
		Payload: VerdictUpdated{ClaimID: claimID, VerdictID: vid, Version: curVersion + 1, Verdict: verdict, Proposed: deref(curProposed), ReviewStatus: review}})
	return err
}

func sigOrEmpty(b []byte) []byte {
	if b == nil {
		return []byte("{}")
	}
	return b
}

type CorrectionInput struct {
	Verdict     string            `json:"verdict"`
	Explanation map[string]string `json:"explanation"`
	Note        map[string]string `json:"correction_note"`
	Rationale   string            `json:"rationale"`
}

// Correct publishes a corrected verdict; previous versions remain in the public history.
func (s *Service) Correct(ctx context.Context, claimID uuid.UUID, in CorrectionInput) error {
	if !validVerdicts[in.Verdict] {
		return apperr.Validation(apperr.FieldError{Field: "verdict", Code: "invalid", Message: "Invalid verdict."})
	}
	if len(in.Note) == 0 || strings.TrimSpace(in.Rationale) == "" {
		return apperr.Validation(apperr.FieldError{Field: "correction_note", Code: "required", Message: "A correction note and rationale are required."})
	}
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM claims WHERE id=$1 AND current_verdict_id IS NOT NULL)`, claimID).Scan(&exists); err != nil || !exists {
			return apperr.NotFound("claim_not_found", "Claim without a published verdict.")
		}
		if err := s.publishVerdict(ctx, tx, claimID, in.Verdict, "editor_modified", nil, in.Explanation, in.Rationale, in.Note); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "factcheck.correction", ResourceType: "claim", ResourceID: claimID.String(), New: in})
	})
}

// SetClaimStatus withdraws or reopens a claim (e.g. extraction error, retraction).
func (s *Service) SetClaimStatus(ctx context.Context, claimID uuid.UUID, status, reason string) error {
	if status != "withdrawn" && status != "pending" && status != "contested" {
		return apperr.ValidationMsg("claim_status_invalid", "status must be withdrawn, pending or contested")
	}
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE claims SET status=$2, needs_reverification=($2='pending'), version=version+1, updated_at=now() WHERE id=$1`, claimID, status)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return apperr.NotFound("claim_not_found", "Claim not found.")
		}
		if status == "pending" {
			if _, err := bus.Publish(ctx, tx, bus.Draft{Type: bus.ClaimVerificationRequested, AggregateType: "claim", AggregateID: claimID.String(),
				Payload: map[string]any{"claim_id": claimID, "reason": "editor_reopened"}}); err != nil {
				return err
			}
		}
		if _, err := bus.Publish(ctx, tx, bus.Draft{Type: bus.SearchIndexRequested, AggregateType: "claim", AggregateID: claimID.String(),
			Payload: map[string]any{"doc_type": "claim", "id": claimID, "force": true}}); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "claim.status", ResourceType: "claim", ResourceID: claimID.String(), New: map[string]any{"status": status, "reason": reason}})
	})
}

// ---------- Narratives ----------

type NarrativeInput struct {
	Title      map[string]string `json:"title"`
	Summary    map[string]string `json:"summary"`
	WhyCaution map[string]string `json:"why_caution"`
	ClaimIDs   []uuid.UUID       `json:"claim_ids"`
}

func (s *Service) CreateNarrative(ctx context.Context, in NarrativeInput) (uuid.UUID, error) {
	if in.Title["ro"] == "" || len(in.ClaimIDs) == 0 {
		return uuid.Nil, apperr.ValidationMsg("narrative_invalid", "A Romanian title and at least one claim are required.")
	}
	id := ids.New()
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		p := principal.From(ctx)
		if _, err := tx.Exec(ctx, `INSERT INTO misinformation_narratives (id, title, summary, why_caution, status, created_by,
			first_seen_at, last_seen_at, languages)
			SELECT $1,$2,$3,$4,'draft',$5, min(first_seen_at), max(last_seen_at), array_agg(DISTINCT language) FROM claims WHERE id = ANY($6)`,
			id, mustJSON(in.Title), mustJSON(in.Summary), mustJSON(in.WhyCaution), p.ID, in.ClaimIDs); err != nil {
			return err
		}
		for _, c := range in.ClaimIDs {
			if _, err := tx.Exec(ctx, `INSERT INTO narrative_claim_links (narrative_id, claim_id, linked_by) VALUES ($1,$2,$3)`, id, c, p.ID); err != nil {
				if db.IsForeignKeyViolation(err) {
					return apperr.ValidationMsg("claim_not_found", "Unknown claim id.")
				}
				return err
			}
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "narrative.create", ResourceType: "narrative", ResourceID: id.String(), New: in})
	})
	return id, err
}

// PublishNarrative requires every linked claim to have an editor-reviewed public verdict.
// Narratives describe claims; they never label people or outlets as disinformation actors.
func (s *Service) PublishNarrative(ctx context.Context, id uuid.UUID) error {
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		var unreviewed int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM narrative_claim_links l JOIN claims c ON c.id=l.claim_id
			LEFT JOIN claim_verdicts v ON v.id=c.current_verdict_id
			WHERE l.narrative_id=$1 AND (v.id IS NULL OR v.review_status NOT IN ('editor_approved','editor_modified'))`, id).Scan(&unreviewed); err != nil {
			return err
		}
		if unreviewed > 0 {
			return apperr.ValidationMsg("narrative_unreviewed_claims", "All linked claims need an editor-reviewed verdict before publication.")
		}
		p := principal.From(ctx)
		tag, err := tx.Exec(ctx, `UPDATE misinformation_narratives SET status='published', approved_by=$2, approved_at=now(), updated_at=now(), version=version+1
			WHERE id=$1 AND status IN ('draft','pending_review')`, id, p.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return apperr.NotFound("narrative_not_found", "Draft narrative not found.")
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "narrative.publish", ResourceType: "narrative", ResourceID: id.String()})
	})
}
