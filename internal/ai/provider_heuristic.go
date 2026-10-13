package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/dorinpuscasu25/recapproject/internal/nlp"
)

// Heuristic is a deterministic, transparent rule-based provider (no network). It is a real
// baseline — every output is marked with provider "heuristic" in provenance — and the default
// for local development and tests. It cannot translate: tasks that require output in a language
// absent from the inputs fail with ErrUnsupported instead of producing wrong-language text.
type Heuristic struct{}

const HashEmbeddingDims = 384

func (Heuristic) Generate(_ context.Context, r ProviderRequest) (*ProviderResponse, error) {
	var out any
	var err error
	switch in := r.Payload.(type) {
	case *ClassifyInput:
		out = heurClassify(in)
	case *EntityInput:
		out = heurEntities(in)
	case *ClaimInput:
		out = heurClaims(in)
	case *EquivalenceInput:
		out = heurEquivalence(in)
	case *SynthesisInput:
		out, err = heurSynthesis(in)
	case *NoveltyInput:
		out = heurNovelty(in)
	case *EvidenceInput:
		out = heurEvidence(in)
	case *AnswerInput:
		out, err = heurAnswer(in)
	case *ScriptInput:
		out, err = heurScript(in)
	default:
		return nil, &ProviderError{Category: ErrUnsupported, Msg: fmt.Sprintf("heuristic provider does not support task %s", r.Task)}
	}
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(out)
	return &ProviderResponse{Text: string(b)}, nil
}

func (Heuristic) Embed(_ context.Context, m *Model, texts []string) ([][]float32, Usage, error) {
	dims := m.EmbeddingDims
	if dims <= 0 {
		dims = HashEmbeddingDims
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = nlp.HashEmbed(t, dims)
	}
	return out, Usage{}, nil
}

// ---------- classification ----------

func padded(s string) string { return " " + stemKey(s) + " " }

func stemKey(s string) string {
	toks := strings.Fields(nlp.Key(s))
	for i, t := range toks {
		toks[i] = nlp.Stem(t)
	}
	return strings.Join(toks, " ")
}

func heurClassify(in *ClassifyInput) *ClassifyOutput {
	title := padded(in.Title)
	body := padded(nlp.Truncate(in.Text, 4000))
	hinted := map[string]bool{}
	for _, h := range in.SourceHints {
		hinted[h] = true
	}
	type scored struct {
		slug  string
		score float64
		why   []string
	}
	var res []scored
	for _, c := range in.Categories {
		var terms []string
		terms = append(terms, c.Hints...)
		terms = append(terms, c.Aliases...)
		for _, n := range c.Names {
			terms = append(terms, n)
		}
		s := scored{slug: c.Slug}
		seen := map[string]bool{}
		for _, t := range terms {
			k := stemKey(t)
			if k == "" || seen[k] || len(k) < 3 {
				continue
			}
			seen[k] = true
			needle := " " + k + " "
			if strings.Contains(title, needle) {
				s.score += 2
				s.why = append(s.why, "title:"+t)
			}
			if n := strings.Count(body, needle); n > 0 {
				s.score += math.Min(float64(n), 3) * 0.8
				s.why = append(s.why, "text:"+t)
			}
		}
		for _, ex := range c.Negative {
			if k := stemKey(ex); k != "" && strings.Contains(body, " "+k+" ") {
				s.score -= 1.5
			}
		}
		if hinted[c.Slug] {
			s.score += 0.8
			s.why = append(s.why, "source_hint")
		}
		s.score *= 1 + float64(c.Boost)*0.5
		if s.score >= 1.6 {
			res = append(res, s)
		}
	}
	sort.Slice(res, func(i, j int) bool {
		if res[i].score != res[j].score {
			return res[i].score > res[j].score
		}
		return res[i].slug < res[j].slug
	})
	out := &ClassifyOutput{Assignments: []CategoryAssignment{}}
	for i, s := range res {
		if i >= 3 {
			break
		}
		why := strings.Join(s.why, ", ")
		if len(why) > 280 {
			why = why[:280]
		}
		out.Assignments = append(out.Assignments, CategoryAssignment{Slug: s.slug, Confidence: round2(1 - math.Exp(-s.score/3)),
			IsPrimary: i == 0, Rationale: "matched " + why})
	}
	return out
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// ---------- entities / claims ----------

func heurEntities(in *EntityInput) *EntityOutput {
	out := &EntityOutput{Entities: []ExtractedEntity{}}
	for _, m := range (*nlp.Gazetteer)(nil).Entities(in.Text) {
		out.Entities = append(out.Entities, ExtractedEntity{Name: m.Name, Kind: m.Kind})
		if len(out.Entities) >= 50 {
			break
		}
	}
	return out
}

func heurClaims(in *ClaimInput) *ClaimOutput {
	max := in.MaxClaims
	if max <= 0 {
		max = 10
	}
	out := &ClaimOutput{Claims: []ExtractedClaim{}}
	for _, c := range nlp.ClaimCandidates(in.Text, max) {
		check := c.Checkability
		if in.ContentType == "satire" {
			check = "satire"
		} else if in.ContentType == "opinion" && check == "checkable" && c.AttributedTo == "" {
			check = "opinion"
		}
		out.Claims = append(out.Claims, ExtractedClaim{Quote: c.Text, Start: c.Start, End: c.End, Canonical: nlp.NormalizeText(c.Text),
			Checkability: check, ClaimType: c.ClaimType, AttributedTo: c.AttributedTo, IsDirectQuote: c.IsDirectQuote})
	}
	return out
}

// ---------- equivalence / novelty ----------

func heurEquivalence(in *EquivalenceInput) *EquivalenceOutput {
	ent, facts, conflicts := len(in.SharedEntities), len(in.SharedFacts), len(in.ConflictFacts)
	switch {
	case conflicts > 0 && facts == 0:
		return &EquivalenceOutput{Relation: "same_subject_different_event", Confidence: 0.7,
			Rationale: "conflicting key facts (" + strings.Join(in.ConflictFacts, ", ") + ") with no shared facts"}
	case in.Similarity >= 0.55 && (ent > 0 || facts > 0):
		return &EquivalenceOutput{Relation: "same_story", Confidence: round2(math.Min(0.95, 0.5+in.Similarity/2)),
			Rationale: fmt.Sprintf("high similarity %.2f with %d shared entities and %d shared facts", in.Similarity, ent, facts)}
	case facts >= 2 && ent >= 1:
		return &EquivalenceOutput{Relation: "same_story", Confidence: 0.7, Rationale: "multiple shared facts and entities"}
	case in.Similarity >= 0.35 && ent >= 2:
		return &EquivalenceOutput{Relation: "related", Confidence: 0.55, Rationale: "shared actors, moderate similarity"}
	default:
		return &EquivalenceOutput{Relation: "unrelated", Confidence: 0.6, Rationale: "insufficient overlap"}
	}
}

func factType(k string) string {
	t, _, _ := strings.Cut(k, ":")
	return t
}

func heurNovelty(in *NoveltyInput) *NoveltyOutput {
	known := map[string]bool{}
	knownTypes := map[string]bool{}
	for _, k := range in.KnownFactKeys {
		known[k] = true
		knownTypes[factType(k)] = true
	}
	out := &NoveltyOutput{Severity: "minor", NewFacts: []string{}, Contradictions: []string{}}
	for i, k := range in.NewFactKeys {
		if known[k] {
			continue
		}
		t := factType(k)
		text := k
		if i < len(in.NewFactTexts) {
			text = in.NewFactTexts[i]
		}
		switch t {
		case "money", "percent", "decision":
			out.NewFacts = append(out.NewFacts, text)
			if knownTypes[t] && t != "decision" {
				out.Contradictions = append(out.Contradictions, text)
			}
		case "number":
			out.NewFacts = append(out.NewFacts, text)
		}
	}
	if len(out.NewFacts) > 0 {
		out.MaterialChange = true
		out.Severity = "major"
		out.WhatChanged = "New or changed facts: " + strings.Join(out.NewFacts, "; ")
	}
	if len(out.Contradictions) > 0 {
		out.WhatChanged += ". Values differ from earlier reports: " + strings.Join(out.Contradictions, "; ")
	}
	return out
}

// ---------- synthesis ----------

func heurSynthesis(in *SynthesisInput) (*SynthesisOutput, error) {
	var srcs []SourceSnippet
	for _, s := range in.Sources {
		if s.Language == in.Language {
			srcs = append(srcs, s)
		}
	}
	if len(srcs) == 0 {
		return nil, &ProviderError{Category: ErrUnsupported, Msg: "heuristic synthesis has no sources in language " + in.Language + " (no translation)"}
	}
	sort.SliceStable(srcs, func(i, j int) bool { return srcs[i].PublishedAt < srcs[j].PublishedAt })
	type sent struct {
		text, ref string
		emb       []float32
	}
	var picked []sent
	addSentence := func(text, ref string) bool {
		text = nlp.NormalizeText(text)
		if len([]rune(text)) < 30 || len([]rune(text)) > 450 {
			return false
		}
		e := nlp.HashEmbed(text, 256)
		for _, p := range picked {
			if nlp.Cosine(e, p.emb) > 0.7 {
				return false
			}
		}
		picked = append(picked, sent{text, ref, e})
		return true
	}
	// Round-robin over sources so that several publishers are represented.
	idx := make([]int, len(srcs))
	sentences := make([][]nlp.Span, len(srcs))
	for i, s := range srcs {
		sentences[i] = nlp.Sentences(s.Text)
	}
	for round := 0; round < 6 && len(picked) < 8; round++ {
		for i := range srcs {
			for idx[i] < len(sentences[i]) {
				sp := sentences[i][idx[i]]
				idx[i]++
				if addSentence(sp.Text, srcs[i].Ref) {
					break
				}
			}
		}
	}
	if len(picked) == 0 {
		for _, s := range srcs {
			addSentence(s.Title+".", s.Ref)
		}
	}
	if len(picked) == 0 {
		return nil, &ProviderError{Category: ErrInvalid, Msg: "no usable sentences"}
	}
	render := func(n int) string {
		var parts []string
		for i := 0; i < n && i < len(picked); i++ {
			parts = append(parts, picked[i].text+" ["+picked[i].ref+"]")
		}
		return strings.Join(parts, " ")
	}
	refs := map[string]bool{}
	var cites []string
	for _, p := range picked {
		if !refs[p.ref] {
			refs[p.ref] = true
			cites = append(cites, p.ref)
		}
	}
	return &SynthesisOutput{Title: nlp.Truncate(srcs[0].Title, 190), Short: nlp.Truncate(render(1), 395), Normal: nlp.Truncate(render(3), 1490),
		Detailed: nlp.Truncate(render(8), 4990), Citations: cites, Disputed: []string{}}, nil
}

// ---------- evidence ----------

var negationMarkers = []string{" nu ", "fals", "dezminț", "neadevăr", "infirm", "nu este adevărat", " не ", "опроверг", "неправд", "фейк", "ложн", "not true", "false", "denied"}

func heurEvidence(in *EvidenceInput) *EvidenceOutput {
	claimTerms := map[string]bool{}
	for _, t := range nlp.KeyTokens(in.Claim) {
		claimTerms[t] = true
	}
	claimFacts := map[string]string{} // key -> type
	for _, f := range nlp.Facts(in.Claim) {
		claimFacts[f.Key] = f.Type
	}
	out := &EvidenceOutput{Assessments: []EvidenceAssessment{}}
	for _, ev := range in.Evidence {
		best, bestScore := "", 0.0
		for _, s := range nlp.Sentences(ev.Text) {
			score := 0.0
			for _, t := range nlp.KeyTokens(s.Text) {
				if claimTerms[t] {
					score++
				}
			}
			if score > bestScore {
				best, bestScore = s.Text, score
			}
		}
		overlap := bestScore / math.Max(1, float64(len(claimTerms)))
		a := EvidenceAssessment{Ref: ev.Ref, Relation: "irrelevant", Rationale: fmt.Sprintf("term overlap %.2f", overlap)}
		if best == "" || overlap < 0.25 {
			out.Assessments = append(out.Assessments, a)
			continue
		}
		a.Quote = best
		lb := " " + strings.ToLower(best) + " "
		negated := false
		for _, m := range negationMarkers {
			if strings.Contains(lb, m) {
				negated = true
				break
			}
		}
		same, differ := 0, 0
		evFacts := nlp.Facts(best)
		for _, f := range evFacts {
			if _, ok := claimFacts[f.Key]; ok {
				same++
				continue
			}
			for ck, ct := range claimFacts {
				if ct == f.Type && (ct == "money" || ct == "percent" || ct == "number") && ck != f.Key {
					differ++
				}
			}
		}
		switch {
		case negated && overlap >= 0.4:
			a.Relation, a.Rationale = "contradicts", fmt.Sprintf("evidence explicitly negates the statement (overlap %.2f)", overlap)
		case differ > 0 && same == 0 && overlap >= 0.4:
			a.Relation, a.Rationale = "contradicts", "evidence reports different figures for the same subject"
		case same > 0 && overlap >= 0.4:
			a.Relation, a.Rationale = "supports", fmt.Sprintf("evidence states the same figures/decision (%d matching facts)", same)
		case overlap >= 0.6 && len(claimFacts) == 0:
			a.Relation, a.Rationale = "supports", fmt.Sprintf("evidence restates the claim (overlap %.2f)", overlap)
		default:
			a.Relation, a.Rationale = "context", fmt.Sprintf("related but not decisive (overlap %.2f)", overlap)
		}
		out.Assessments = append(out.Assessments, a)
	}
	return out
}

// ---------- grounded answers ----------

var uncertaintyText = map[string]string{
	"ro": "Răspunsul se bazează doar pe sursele citate; informațiile pot fi schimbat după data celei mai recente surse.",
	"ru": "Ответ основан только на процитированных источниках; информация могла измениться после даты последнего источника.",
	"en": "This answer relies only on the cited sources; information may have changed since the most recent source.",
}

var abstainText = map[string]string{
	"ro": "Nu am găsit suficiente informații verificabile în sursele disponibile pentru a răspunde la această întrebare.",
	"ru": "В доступных источниках недостаточно проверяемой информации, чтобы ответить на этот вопрос.",
	"en": "The available sources do not contain enough verifiable information to answer this question.",
}

func heurAnswer(in *AnswerInput) (*AnswerOutput, error) {
	q := map[string]bool{}
	for _, t := range nlp.KeyTokens(in.Question) {
		if len(t) >= 3 {
			q[t] = true
		}
	}
	// Follow-ups inherit terms from the previous user turn.
	for i := len(in.History) - 1; i >= 0 && len(q) < 3; i-- {
		if in.History[i].Role == "user" {
			for _, t := range nlp.KeyTokens(in.History[i].Content) {
				q[t] = true
			}
		}
	}
	type cand struct {
		text, ref string
		score     float64
	}
	var cands []cand
	for _, d := range in.Context {
		for _, s := range nlp.Sentences(d.Text) {
			if len([]rune(s.Text)) < 25 {
				continue
			}
			sc := 0.0
			for _, t := range nlp.KeyTokens(s.Text) {
				if q[t] {
					sc++
				}
			}
			if sc > 0 {
				cands = append(cands, cand{s.Text, d.Ref, sc})
			}
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	lang := in.Language
	if _, ok := uncertaintyText[lang]; !ok {
		lang = "en"
	}
	minScore := 2.0
	if len(q) <= 2 {
		minScore = 1
	}
	n := 2
	switch in.Level {
	case "detailed":
		n = 5
	case "background":
		n = 6
	}
	var parts []string
	var cites []string
	seen := map[string]bool{}
	for _, c := range cands {
		if c.score < minScore || len(parts) >= n {
			break
		}
		parts = append(parts, nlp.NormalizeText(c.text)+" ["+c.ref+"]")
		if !seen[c.ref] {
			seen[c.ref] = true
			cites = append(cites, c.ref)
		}
	}
	if len(parts) == 0 {
		return &AnswerOutput{Answer: abstainText[lang], Citations: []string{}, Abstained: true, Uncertainty: abstainText[lang]}, nil
	}
	return &AnswerOutput{Answer: strings.Join(parts, " "), Citations: cites, Uncertainty: uncertaintyText[lang]}, nil
}

// ---------- script ----------

var scriptFrames = map[string][2]string{
	"ro": {"Bună ziua. Iată principalele subiecte.", "Acestea au fost subiectele principale. Sursele sunt disponibile în aplicație."},
	"ru": {"Здравствуйте. Главные темы.", "Это были главные темы. Источники доступны в приложении."},
	"en": {"Hello. Here are the main stories.", "Those were the main stories. Sources are available in the app."},
}

func heurScript(in *ScriptInput) (*ScriptOutput, error) {
	f, ok := scriptFrames[in.Language]
	if !ok {
		return nil, &ProviderError{Category: ErrUnsupported, Msg: "no script frame for language " + in.Language}
	}
	// ~2.5 words per second of speech.
	budgetWords := int(float64(in.TargetSeconds) * 2.5)
	perSection := budgetWords / max(1, len(in.Sections))
	var b strings.Builder
	b.WriteString(f[0])
	var cites []string
	for _, s := range in.Sections {
		words := strings.Fields(s.Text)
		if len(words) > perSection && perSection > 15 {
			words = words[:perSection]
		}
		text := strings.Join(words, " ")
		if !strings.Contains(text, "["+s.Ref+"]") {
			text += " [" + s.Ref + "]"
		}
		b.WriteString("\n\n" + s.Title + ". " + text)
		cites = append(cites, s.Ref)
	}
	b.WriteString("\n\n" + f[1])
	return &ScriptOutput{Title: "Briefing", Script: b.String(), Citations: cites}, nil
}
