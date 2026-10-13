package ai

import (
	"fmt"
	"strings"
)

// Typed task contracts. Each structured task has an input (also the heuristic provider's
// payload), an output with a JSON schema, and a Validate method for semantic checks.

// ---------- Category classification ----------

type CategoryDef struct {
	Slug        string            `json:"slug"`
	ParentSlug  string            `json:"parent_slug,omitempty"`
	Names       map[string]string `json:"names"`
	Description string            `json:"description,omitempty"`
	Hints       []string          `json:"hints,omitempty"`
	Aliases     []string          `json:"aliases,omitempty"`
	Positive    []string          `json:"positive_examples,omitempty"`
	Negative    []string          `json:"negative_examples,omitempty"`
	Boost       float32           `json:"boost,omitempty"`
}

type ClassifyInput struct {
	Title           string        `json:"title"`
	Text            string        `json:"text"`
	Language        string        `json:"language"`
	Categories      []CategoryDef `json:"categories"`
	TaxonomyVersion int64         `json:"taxonomy_version"`
	SourceHints     []string      `json:"source_hints,omitempty"` // category slugs suggested by the source endpoint
}

type CategoryAssignment struct {
	Slug       string  `json:"category_slug"`
	Confidence float64 `json:"confidence"`
	IsPrimary  bool    `json:"is_primary"`
	Rationale  string  `json:"rationale"`
}

type ClassifyOutput struct {
	Assignments []CategoryAssignment `json:"assignments"`
}

var ClassifySchema = obj(map[string]any{
	"assignments": arr(obj(map[string]any{
		"category_slug": str(), "confidence": numRange(0, 1), "is_primary": boolean(), "rationale": strMax(300),
	}), 0, 5),
})

func (o *ClassifyOutput) Validate(input any) error {
	in, ok := input.(*ClassifyInput)
	if !ok {
		return nil
	}
	valid := map[string]bool{}
	for _, c := range in.Categories {
		valid[c.Slug] = true
	}
	primaries := 0
	for _, a := range o.Assignments {
		if !valid[a.Slug] {
			return fmt.Errorf("unknown category slug %q (not in current taxonomy)", a.Slug)
		}
		if a.IsPrimary {
			primaries++
		}
	}
	if primaries > 1 {
		return fmt.Errorf("more than one primary category")
	}
	return nil
}

// ---------- Entity extraction ----------

type EntityInput struct {
	Text     string `json:"text"`
	Language string `json:"language"`
}

type ExtractedEntity struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type EntityOutput struct {
	Entities []ExtractedEntity `json:"entities"`
}

var EntitySchema = obj(map[string]any{
	"entities": arr(obj(map[string]any{"name": strMax(200), "kind": enum("person", "organization", "location", "event", "other")}), 0, 50),
})

func (o *EntityOutput) Validate(any) error { return nil }

// ---------- Claim extraction ----------

type ClaimInput struct {
	Title       string `json:"title"`
	Text        string `json:"text"`
	Language    string `json:"language"`
	ContentType string `json:"content_type"`
	MaxClaims   int    `json:"max_claims"`
}

type ExtractedClaim struct {
	Quote         string `json:"quote"` // exact source span
	Start         int    `json:"start"` // byte offset into Text
	End           int    `json:"end"`
	Canonical     string `json:"canonical"` // normalized, self-contained formulation
	Checkability  string `json:"checkability"`
	ClaimType     string `json:"claim_type"`
	AttributedTo  string `json:"attributed_to"`
	IsDirectQuote bool   `json:"is_direct_quote"`
}

type ClaimOutput struct {
	Claims []ExtractedClaim `json:"claims"`
}

var ClaimSchema = obj(map[string]any{
	"claims": arr(obj(map[string]any{
		"quote": strMax(1200), "start": integer(), "end": integer(), "canonical": strMax(600),
		"checkability":  enum("checkable", "opinion", "prediction", "satire", "rhetorical", "quote_only"),
		"claim_type":    enum("statistic", "decision", "event", "attribution", "statement"),
		"attributed_to": strMax(200), "is_direct_quote": boolean(),
	}), 0, 30),
})

// Validate enforces that every quote is an exact span of the source text (offsets are repaired
// when the quote occurs exactly once; otherwise the output is rejected).
func (o *ClaimOutput) Validate(input any) error {
	in, ok := input.(*ClaimInput)
	if !ok {
		return nil
	}
	for i := range o.Claims {
		c := &o.Claims[i]
		if c.Start >= 0 && c.End <= len(in.Text) && c.Start < c.End && in.Text[c.Start:c.End] == c.Quote {
			continue
		}
		if n := strings.Count(in.Text, c.Quote); n >= 1 && c.Quote != "" {
			c.Start = strings.Index(in.Text, c.Quote)
			c.End = c.Start + len(c.Quote)
			continue
		}
		return fmt.Errorf("claim %d quote is not an exact span of the source text", i)
	}
	return nil
}

// ---------- Event equivalence ----------

type EquivalenceInput struct {
	ArticleTitle   string   `json:"article_title"`
	ArticleLead    string   `json:"article_lead"`
	ArticleDate    string   `json:"article_date"`
	EventTitle     string   `json:"event_title"`
	EventSummary   string   `json:"event_summary"`
	EventDate      string   `json:"event_date"`
	SharedEntities []string `json:"shared_entities"`
	SharedFacts    []string `json:"shared_facts"`
	ConflictFacts  []string `json:"conflicting_facts"`
	Similarity     float64  `json:"similarity"`
}

type EquivalenceOutput struct {
	Relation   string  `json:"relation"` // same_story | follow_up | related | same_subject_different_event | unrelated
	Confidence float64 `json:"confidence"`
	Rationale  string  `json:"rationale"`
}

var EquivalenceSchema = obj(map[string]any{
	"relation":   enum("same_story", "follow_up", "related", "same_subject_different_event", "unrelated"),
	"confidence": numRange(0, 1), "rationale": strMax(500),
})

func (o *EquivalenceOutput) Validate(any) error { return nil }

// ---------- Event synthesis ----------

type SourceSnippet struct {
	Ref         string `json:"ref"` // e.g. "a1" → maps to an article id server-side
	Publisher   string `json:"publisher"`
	PublishedAt string `json:"published_at"`
	Language    string `json:"language"`
	Title       string `json:"title"`
	Text        string `json:"text"`
}

type SynthesisInput struct {
	Language        string          `json:"language"`
	Sources         []SourceSnippet `json:"sources"`
	PreviousSummary string          `json:"previous_summary,omitempty"`
}

type SynthesisOutput struct {
	Title     string   `json:"title"`
	Short     string   `json:"short"`
	Normal    string   `json:"normal"`
	Detailed  string   `json:"detailed"`
	Citations []string `json:"citations"` // refs used
	Disputed  []string `json:"disputed_points"`
}

var SynthesisSchema = obj(map[string]any{
	"title": strMax(200), "short": strMax(400), "normal": strMax(1500), "detailed": strMax(5000),
	"citations": arr(str(), 1, 50), "disputed_points": arr(strMax(400), 0, 10),
})

func (o *SynthesisOutput) Validate(input any) error {
	in, ok := input.(*SynthesisInput)
	if !ok {
		return nil
	}
	refs := map[string]bool{}
	for _, s := range in.Sources {
		refs[s.Ref] = true
	}
	for _, c := range o.Citations {
		if !refs[c] {
			return fmt.Errorf("citation %q does not refer to a provided source", c)
		}
	}
	for _, field := range []string{o.Short, o.Normal, o.Detailed} {
		for _, ref := range citedRefs(field) {
			if !refs[ref] {
				return fmt.Errorf("inline citation [%s] does not refer to a provided source", ref)
			}
		}
	}
	if len(o.Citations) == 0 {
		return fmt.Errorf("summary without citations")
	}
	return nil
}

// citedRefs extracts [ref] markers from text.
func citedRefs(s string) []string {
	var out []string
	for {
		i := strings.Index(s, "[")
		if i < 0 {
			return out
		}
		j := strings.Index(s[i:], "]")
		if j < 0 {
			return out
		}
		ref := s[i+1 : i+j]
		if ref != "" && len(ref) <= 12 && !strings.ContainsAny(ref, " ,") {
			out = append(out, ref)
		}
		s = s[i+j+1:]
	}
}

// CitedRefs is exported for server-side citation validation in other modules.
func CitedRefs(s string) []string { return citedRefs(s) }

// ---------- Novelty detection ----------

type NoveltyInput struct {
	EventSummary   string   `json:"event_summary"`
	KnownFactKeys  []string `json:"known_fact_keys"`
	NewFactKeys    []string `json:"new_fact_keys"`
	NewFactTexts   []string `json:"new_fact_texts"`
	NewArticleLead string   `json:"new_article_lead"`
	Language       string   `json:"language"`
}

type NoveltyOutput struct {
	MaterialChange bool     `json:"material_change"`
	Severity       string   `json:"severity"` // minor | major | critical
	WhatChanged    string   `json:"what_changed"`
	NewFacts       []string `json:"new_facts"`
	Contradictions []string `json:"contradictions"`
}

var NoveltySchema = obj(map[string]any{
	"material_change": boolean(), "severity": enum("minor", "major", "critical"), "what_changed": strMax(600),
	"new_facts": arr(strMax(300), 0, 20), "contradictions": arr(strMax(300), 0, 20),
})

func (o *NoveltyOutput) Validate(any) error { return nil }

// ---------- Evidence assessment ----------

type EvidenceItem struct {
	Ref         string `json:"ref"`
	Publisher   string `json:"publisher"`
	SourceKind  string `json:"source_kind"`
	PublishedAt string `json:"published_at"`
	Text        string `json:"text"`
}

type EvidenceInput struct {
	Claim     string         `json:"claim"`
	ClaimDate string         `json:"claim_date"`
	Language  string         `json:"language"`
	Evidence  []EvidenceItem `json:"evidence"`
}

type EvidenceAssessment struct {
	Ref       string `json:"ref"`
	Relation  string `json:"relation"` // supports | contradicts | context | irrelevant
	Quote     string `json:"quote"`    // exact supporting excerpt from the evidence text
	Rationale string `json:"rationale"`
}

type EvidenceOutput struct {
	Assessments []EvidenceAssessment `json:"assessments"`
}

var EvidenceSchema = obj(map[string]any{
	"assessments": arr(obj(map[string]any{
		"ref": str(), "relation": enum("supports", "contradicts", "context", "irrelevant"), "quote": strMax(800), "rationale": strMax(400),
	}), 0, 30),
})

func (o *EvidenceOutput) Validate(input any) error {
	in, ok := input.(*EvidenceInput)
	if !ok {
		return nil
	}
	texts := map[string]string{}
	for _, e := range in.Evidence {
		texts[e.Ref] = e.Text
	}
	for _, a := range o.Assessments {
		t, ok := texts[a.Ref]
		if !ok {
			return fmt.Errorf("assessment references unknown evidence %q", a.Ref)
		}
		if a.Relation != "irrelevant" && a.Quote != "" && !strings.Contains(t, a.Quote) {
			return fmt.Errorf("quote for %q is not an exact excerpt of the evidence", a.Ref)
		}
	}
	return nil
}

// ---------- Grounded answers (explanation, chat) ----------

type ContextDoc struct {
	Ref         string `json:"ref"`
	Kind        string `json:"kind"` // event | article | claim | update
	Title       string `json:"title"`
	Publisher   string `json:"publisher,omitempty"`
	PublishedAt string `json:"published_at,omitempty"`
	Text        string `json:"text"`
}

type AnswerInput struct {
	Question string       `json:"question"`
	Level    string       `json:"level"` // short | detailed | background
	Language string       `json:"language"`
	Now      string       `json:"now"`
	History  []Message    `json:"history,omitempty"`
	Context  []ContextDoc `json:"context"`
	// ScopeDefault: the user scoped the conversation to a specific item; when the sources do not
	// address the question directly, summarise what they do say and state that limitation.
	ScopeDefault bool `json:"scope_default,omitempty"`
}

type AnswerOutput struct {
	Answer      string   `json:"answer"`
	Citations   []string `json:"citations"`
	Abstained   bool     `json:"abstained"`
	Uncertainty string   `json:"uncertainty"`
}

var AnswerSchema = obj(map[string]any{
	"answer": strMax(6000), "citations": arr(str(), 0, 30), "abstained": boolean(), "uncertainty": strMax(600),
})

func (o *AnswerOutput) Validate(input any) error {
	in, ok := input.(*AnswerInput)
	if !ok {
		return nil
	}
	refs := map[string]bool{}
	for _, c := range in.Context {
		refs[c.Ref] = true
	}
	for _, c := range o.Citations {
		if !refs[c] {
			return fmt.Errorf("citation %q not in retrieved context", c)
		}
	}
	for _, c := range citedRefs(o.Answer) {
		if !refs[c] {
			return fmt.Errorf("inline citation [%s] not in retrieved context", c)
		}
	}
	if !o.Abstained && len(o.Citations) == 0 {
		return fmt.Errorf("non-abstaining answer without citations")
	}
	return nil
}

// ---------- Script generation (briefings) ----------

type ScriptSection struct {
	Ref   string `json:"ref"`
	Title string `json:"title"`
	Text  string `json:"text"`
}

type ScriptInput struct {
	Language       string            `json:"language"`
	TargetSeconds  int               `json:"target_seconds"`
	Tone           string            `json:"tone"`
	Sections       []ScriptSection   `json:"sections"`
	Pronunciations map[string]string `json:"pronunciations,omitempty"`
}

type ScriptOutput struct {
	Title     string   `json:"title"`
	Script    string   `json:"script"`
	Citations []string `json:"citations"`
}

var ScriptSchema = obj(map[string]any{"title": strMax(200), "script": strMax(20000), "citations": arr(str(), 0, 50)})

func (o *ScriptOutput) Validate(input any) error {
	in, ok := input.(*ScriptInput)
	if !ok {
		return nil
	}
	refs := map[string]bool{}
	for _, s := range in.Sections {
		refs[s.Ref] = true
	}
	for _, c := range append(append([]string{}, o.Citations...), citedRefs(o.Script)...) {
		if !refs[c] {
			return fmt.Errorf("script cites unknown section %q", c)
		}
	}
	return nil
}

// ---------- schema builders ----------

func obj(props map[string]any) map[string]any {
	req := make([]any, 0, len(props))
	for k := range props {
		req = append(req, k)
	}
	return map[string]any{"type": "object", "properties": props, "required": req, "additionalProperties": false}
}
func arr(items map[string]any, min, max int) map[string]any {
	return map[string]any{"type": "array", "items": items, "minItems": float64(min), "maxItems": float64(max)}
}
func str() map[string]any         { return map[string]any{"type": "string"} }
func strMax(n int) map[string]any { return map[string]any{"type": "string", "maxLength": float64(n)} }
func integer() map[string]any     { return map[string]any{"type": "integer"} }
func boolean() map[string]any     { return map[string]any{"type": "boolean"} }
func numRange(a, b float64) map[string]any {
	return map[string]any{"type": "number", "minimum": a, "maximum": b}
}
func enum(vals ...string) map[string]any {
	e := make([]any, len(vals))
	for i, v := range vals {
		e[i] = v
	}
	return map[string]any{"type": "string", "enum": e}
}

// SchemaFor returns the canonical schema of a structured task.
func SchemaFor(t Task) map[string]any {
	switch t {
	case TaskClassification:
		return ClassifySchema
	case TaskEntityExtraction:
		return EntitySchema
	case TaskClaimExtraction:
		return ClaimSchema
	case TaskEventEquivalence:
		return EquivalenceSchema
	case TaskEventSynthesis:
		return SynthesisSchema
	case TaskNoveltyDetection:
		return NoveltySchema
	case TaskEvidenceAssessment:
		return EvidenceSchema
	case TaskExplanation, TaskChatAnswering:
		return AnswerSchema
	case TaskScriptGeneration:
		return ScriptSchema
	}
	return nil
}
