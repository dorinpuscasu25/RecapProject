package factcheck

import "testing"

func TestDecideDefaultsToUnverifiedWithoutEvidence(t *testing.T) {
	d := Decide(DefaultPolicy(), "checkable", "low", Signals{})
	if d.Verdict != "unverified" || d.RequiresReview {
		t.Fatalf("got %+v", d)
	}
}

func TestSyndicatedCopiesDoNotCountAsIndependent(t *testing.T) {
	ev := []Evidence{{Relation: "supports", IndependenceKey: "pubA"}, {Relation: "supports", IndependenceKey: "pubA"}, {Relation: "supports", IndependenceKey: "pubA"}}
	s := Collapse(ev)
	if s.IndependentSupporting != 1 {
		t.Fatalf("syndication must collapse: %+v", s)
	}
	if d := Decide(DefaultPolicy(), "checkable", "low", s); d.Verdict != "unverified" {
		t.Fatalf("one independent source is insufficient: %s", d.Verdict)
	}
}

func TestSupportedWithTwoIndependentOrPrimary(t *testing.T) {
	two := Collapse([]Evidence{{Relation: "supports", IndependenceKey: "a"}, {Relation: "supports", IndependenceKey: "b"}})
	if d := Decide(DefaultPolicy(), "checkable", "low", two); d.Verdict != "supported" || d.RequiresReview {
		t.Fatalf("two independent: %+v", d)
	}
	primary := Collapse([]Evidence{{Relation: "supports", IndependenceKey: "gov", IsPrimary: true}})
	if d := Decide(DefaultPolicy(), "checkable", "low", primary); d.Verdict != "supported" {
		t.Fatalf("primary source: %+v", d)
	}
}

func TestRefutedRequiresReview(t *testing.T) {
	s := Collapse([]Evidence{{Relation: "contradicts", IndependenceKey: "gov", IsPrimary: true}})
	d := Decide(DefaultPolicy(), "checkable", "low", s)
	if d.Verdict != "refuted" || !d.RequiresReview {
		t.Fatalf("refuted must be gated: %+v", d)
	}
}

func TestConflictIsDisputed(t *testing.T) {
	s := Collapse([]Evidence{{Relation: "contradicts", IndependenceKey: "a"}, {Relation: "supports", IndependenceKey: "b"}})
	if d := Decide(DefaultPolicy(), "checkable", "low", s); d.Verdict != "disputed" || !d.RequiresReview {
		t.Fatalf("got %+v", d)
	}
}

func TestHighRiskSupportedNeedsReviewAndOpinionNotCheckable(t *testing.T) {
	s := Collapse([]Evidence{{Relation: "supports", IndependenceKey: "a"}, {Relation: "supports", IndependenceKey: "b"}})
	if d := Decide(DefaultPolicy(), "checkable", "high", s); !d.RequiresReview {
		t.Fatal("high-risk definitive labels must be reviewed")
	}
	if d := Decide(DefaultPolicy(), "opinion", "high", s); d.Verdict != "not_checkable" {
		t.Fatalf("opinion: %s", d.Verdict)
	}
}

func TestContextOnlyIsUnverified(t *testing.T) {
	s := Collapse([]Evidence{{Relation: "context", IndependenceKey: "a"}, {Relation: "context", IndependenceKey: "b"}})
	if d := Decide(DefaultPolicy(), "checkable", "low", s); d.Verdict != "unverified" {
		t.Fatalf("context-only evidence must not confirm or refute: %s", d.Verdict)
	}
}

func TestPriorityOrdering(t *testing.T) {
	hi, _ := Priority(10, 5, 0.5, "high", "refuted")
	lo, _ := Priority(1, 1, 0.9, "low", "supported")
	if hi <= lo {
		t.Fatalf("priority: %f <= %f", hi, lo)
	}
}
