// Package factcheck implements evidence-driven, claim-level verification: evidence retrieval,
// source-independence accounting, deterministic sufficiency rules, editorial review gates,
// versioned verdicts with corrections, reader-facing explanations and the misinformation monitor.
//
// It is deliberately NOT a truth oracle: models only classify the relation between a claim and
// a piece of evidence; verdicts are computed by transparent rules and high-risk definitive labels
// require human approval.
package factcheck

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Policy is the DB-managed `factcheck` editorial policy.
type Policy struct {
	PolicyVersion                   int      `json:"policy_version"`
	MinIndependentSupporting        int      `json:"min_independent_supporting"`
	MinIndependentSupportingPrimary int      `json:"min_independent_supporting_with_primary"`
	MinIndependentContradicting     int      `json:"min_independent_contradicting"`
	OutdatedAfterDays               int      `json:"outdated_after_days"`
	EvidenceWindowDays              int      `json:"evidence_window_days"`
	ReviewRequiredVerdicts          []string `json:"review_required_verdicts"`
	ReviewConfidenceBelow           float64  `json:"review_confidence_below"`
	HighRiskTerms                   []string `json:"high_risk_terms"`
}

func DefaultPolicy() Policy {
	return Policy{PolicyVersion: 1, MinIndependentSupporting: 2, MinIndependentSupportingPrimary: 1, MinIndependentContradicting: 1,
		OutdatedAfterDays: 365, EvidenceWindowDays: 30, ReviewRequiredVerdicts: []string{"refuted", "misleading_context", "disputed"}, ReviewConfidenceBelow: 0.6}
}

// Evidence is a relation between the claim and one evidence document.
type Evidence struct {
	Relation        string // supports | contradicts | context | irrelevant
	IndependenceKey string
	IsPrimary       bool // official/primary document or institution
	PublishedAt     *time.Time
}

// Signals summarizes evidence after collapsing non-independent sources.
type Signals struct {
	IndependentSupporting    int  `json:"independent_supporting"`
	IndependentContradicting int  `json:"independent_contradicting"`
	PrimarySupporting        bool `json:"primary_supporting"`
	PrimaryContradicting     bool `json:"primary_contradicting"`
	ContextOnly              int  `json:"context_only"`
	EvidenceConsidered       int  `json:"evidence_considered"`
}

// Collapse counts each independence cluster at most once per relation (syndicated copies and
// multiple articles from one publisher do not count as independent corroboration).
func Collapse(ev []Evidence) Signals {
	sup, con, ctx := map[string]bool{}, map[string]bool{}, map[string]bool{}
	var s Signals
	for _, e := range ev {
		if e.Relation == "irrelevant" {
			continue
		}
		s.EvidenceConsidered++
		switch e.Relation {
		case "supports":
			sup[e.IndependenceKey] = true
			if e.IsPrimary {
				s.PrimarySupporting = true
			}
		case "contradicts":
			con[e.IndependenceKey] = true
			if e.IsPrimary {
				s.PrimaryContradicting = true
			}
		case "context":
			ctx[e.IndependenceKey] = true
		}
	}
	s.IndependentSupporting, s.IndependentContradicting, s.ContextOnly = len(sup), len(con), len(ctx)
	return s
}

// Decision is the automated outcome of the sufficiency rules.
type Decision struct {
	Verdict        string  // proposed verdict
	Confidence     float64 // internal calibrated signal (never shown as certainty)
	RequiresReview bool
	ReviewReasons  []string
	Limitations    []string
	Signals        Signals
}

// Decide applies deterministic sufficiency rules. Without sufficient evidence the default is
// "unverified" — never "refuted".
func Decide(p Policy, checkability string, risk string, s Signals) Decision {
	d := Decision{Signals: s}
	if checkability != "checkable" {
		d.Verdict = "not_checkable"
		d.Confidence = 0.9
		d.Limitations = []string{"The statement is " + strings.ReplaceAll(checkability, "_", " ") + ", not a verifiable factual claim."}
		return d
	}
	supEnough := s.IndependentSupporting >= p.MinIndependentSupporting ||
		(s.PrimarySupporting && s.IndependentSupporting >= max(1, p.MinIndependentSupportingPrimary))
	conEnough := s.IndependentContradicting >= max(1, p.MinIndependentContradicting)
	switch {
	case s.IndependentSupporting > 0 && s.IndependentContradicting > 0:
		d.Verdict = "disputed"
		d.Confidence = 0.5
		d.Limitations = append(d.Limitations, "Credible sources disagree; the conflict is unresolved.")
	case conEnough && s.IndependentSupporting == 0:
		d.Verdict = "refuted"
		d.Confidence = math.Min(0.95, 0.5+0.2*float64(s.IndependentContradicting)+boolF(s.PrimaryContradicting)*0.15)
	case supEnough && s.IndependentContradicting == 0:
		d.Verdict = "supported"
		d.Confidence = math.Min(0.95, 0.45+0.15*float64(s.IndependentSupporting)+boolF(s.PrimarySupporting)*0.15)
	default:
		d.Verdict = "unverified"
		d.Confidence = 0.3
		if s.IndependentSupporting == 1 {
			d.Limitations = append(d.Limitations, "Only one independent source supports the claim.")
		} else if s.EvidenceConsidered == 0 {
			d.Limitations = append(d.Limitations, "No independent evidence was found.")
		} else {
			d.Limitations = append(d.Limitations, "Available sources provide context but do not directly confirm or contradict the claim.")
		}
	}
	if !s.PrimarySupporting && !s.PrimaryContradicting && d.Verdict != "unverified" {
		d.Limitations = append(d.Limitations, "No primary/official document was found; assessment relies on secondary reporting.")
	}
	if s.PrimarySupporting && d.Verdict == "supported" {
		d.Limitations = append(d.Limitations, "Official statements are claims by an institution; they were weighed, not treated as infallible.")
	}
	for _, v := range p.ReviewRequiredVerdicts {
		if v == d.Verdict {
			d.RequiresReview = true
			d.ReviewReasons = append(d.ReviewReasons, "verdict_requires_review:"+v)
		}
	}
	if risk == "high" && d.Verdict != "unverified" && d.Verdict != "not_checkable" {
		d.RequiresReview = true
		d.ReviewReasons = append(d.ReviewReasons, "high_risk_topic")
	}
	if d.Verdict != "unverified" && d.Verdict != "not_checkable" && d.Confidence < p.ReviewConfidenceBelow {
		d.RequiresReview = true
		d.ReviewReasons = append(d.ReviewReasons, fmt.Sprintf("low_confidence:%.2f", d.Confidence))
	}
	return d
}

func boolF(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Priority scores review urgency from reach, uncertainty, impact and potential harm (0..1).
func Priority(occurrences, sources int, confidence float64, risk string, verdict string) (float64, map[string]float64) {
	reach := 1 - math.Exp(-float64(occurrences+sources)/4)
	uncertainty := 1 - confidence
	impact := 0.4
	if risk == "high" {
		impact = 1
	} else if risk == "medium" {
		impact = 0.7
	}
	harm := 0.3
	switch verdict {
	case "refuted", "misleading_context":
		harm = 1
	case "disputed":
		harm = 0.7
	}
	score := 0.3*reach + 0.2*uncertainty + 0.25*impact + 0.25*harm
	return math.Round(score*1000) / 1000, map[string]float64{"reach": reach, "uncertainty": uncertainty, "impact": impact, "harm": harm}
}

// ---------- Reader-facing text (deterministic templates; no generated allegations) ----------

var headlines = map[string]map[string]string{
	"supported": {"ro": "Afirmația este susținută de surse credibile", "ru": "Утверждение подтверждается достоверными источниками",
		"en": "The claim is supported by credible sources"},
	"refuted": {"ro": "Dovezile disponibile contrazic această afirmație", "ru": "Имеющиеся данные опровергают это утверждение",
		"en": "Available evidence contradicts this claim"},
	"misleading_context": {"ro": "Afirmația omite context important", "ru": "В утверждении упущен важный контекст",
		"en": "The claim omits important context"},
	"partially_supported": {"ro": "Afirmația este doar parțial susținută", "ru": "Утверждение подтверждается лишь частично",
		"en": "The claim is only partially supported"},
	"disputed": {"ro": "Sursele credibile se contrazic în privința acestei afirmații", "ru": "Достоверные источники расходятся в оценке этого утверждения",
		"en": "Credible sources disagree about this claim"},
	"unverified": {"ro": "Nu există suficiente dovezi credibile pentru a confirma această afirmație", "ru": "Недостаточно достоверных данных, чтобы подтвердить это утверждение",
		"en": "There is not enough reliable evidence to confirm this claim"},
	"not_checkable": {"ro": "Este o opinie sau o predicție, nu o afirmație verificabilă", "ru": "Это мнение или прогноз, а не проверяемое утверждение",
		"en": "This is an opinion or prediction, not a checkable claim"},
	"outdated": {"ro": "Afirmația a fost corectă la un moment dat, dar nu mai este actuală", "ru": "Утверждение было верным ранее, но устарело",
		"en": "The claim was accurate at one point but is now outdated"},
}

var pendingNote = map[string]string{
	"ro": " O evaluare editorială este în curs.", "ru": " Идёт редакционная проверка.", "en": " An editorial review is in progress.",
}

func Headline(verdict, lang string) string {
	h, ok := headlines[verdict]
	if !ok {
		return verdict
	}
	if v := h[lang]; v != "" {
		return v
	}
	return h["en"]
}

// Explanation builds a factual explanation from the evidence signals only.
func Explanation(verdict, lang string, s Signals, pending bool) string {
	var b strings.Builder
	switch lang {
	case "ru":
		fmt.Fprintf(&b, "Найдено независимых источников, подтверждающих утверждение: %d; опровергающих: %d; дающих только контекст: %d.",
			s.IndependentSupporting, s.IndependentContradicting, s.ContextOnly)
		if s.PrimarySupporting || s.PrimaryContradicting {
			b.WriteString(" Среди них есть первичный (официальный) документ.")
		}
		b.WriteString(" Оценка может измениться при появлении новых первичных документов или независимых подтверждений.")
	case "en":
		fmt.Fprintf(&b, "Independent sources supporting the claim: %d; contradicting it: %d; providing only context: %d.",
			s.IndependentSupporting, s.IndependentContradicting, s.ContextOnly)
		if s.PrimarySupporting || s.PrimaryContradicting {
			b.WriteString(" They include a primary (official) document.")
		}
		b.WriteString(" The assessment may change if new primary documents or independent confirmations appear.")
	default:
		fmt.Fprintf(&b, "Surse independente care susțin afirmația: %d; care o contrazic: %d; care oferă doar context: %d.",
			s.IndependentSupporting, s.IndependentContradicting, s.ContextOnly)
		if s.PrimarySupporting || s.PrimaryContradicting {
			b.WriteString(" Printre ele se află un document primar (oficial).")
		}
		b.WriteString(" Evaluarea se poate schimba dacă apar documente primare noi sau confirmări independente.")
	}
	if pending {
		b.WriteString(pendingNote[lang])
	}
	return b.String()
}
