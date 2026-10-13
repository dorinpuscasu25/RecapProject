package nlp

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// ---------- Entities ----------

type GazetteerEntry struct {
	Kind      string   // person | organization | location
	Canonical string   // canonical display name
	Aliases   []string // surface forms in any language/script
}

// Gazetteer matches known entities by cross-script keys. Entries come from the database
// (entities table), never from code.
type Gazetteer struct {
	byKey map[string]*GazetteerEntry
	maxN  int
}

func NewGazetteer(entries []GazetteerEntry) *Gazetteer {
	g := &Gazetteer{byKey: map[string]*GazetteerEntry{}}
	for i := range entries {
		e := &entries[i]
		for _, a := range append([]string{e.Canonical}, e.Aliases...) {
			k := stemKey(a)
			if k == "" {
				continue
			}
			g.byKey[k] = e
			if n := len(strings.Fields(k)); n > g.maxN {
				g.maxN = n
			}
		}
	}
	return g
}

func stemKey(s string) string {
	var parts []string
	for _, t := range strings.Fields(Key(s)) {
		parts = append(parts, Stem(t))
	}
	return strings.Join(parts, " ")
}

type Mention struct {
	Kind      string
	Name      string // canonical name if known, else surface form
	Key       string // normalized entity key used for matching
	Count     int
	Known     bool
	FirstByte int
}

var capSeqRe = regexp.MustCompile(`(?:\p{Lu}[\p{L}'’\-]+)(?:\s+(?:\p{Lu}[\p{L}'’\-]+|de|din|lui|al|и))*`)

// Entities returns gazetteer matches plus capitalized multi-word sequences (unknown entities),
// with mention counts and a salience-ordered result.
func (g *Gazetteer) Entities(text string) []Mention {
	found := map[string]*Mention{}
	// Gazetteer n-gram scan over stemmed key tokens.
	if g != nil && len(g.byKey) > 0 {
		toks := strings.Fields(Key(text))
		stemmed := make([]string, len(toks))
		for i, t := range toks {
			stemmed[i] = Stem(t)
		}
		for i := 0; i < len(stemmed); i++ {
			for n := g.maxN; n >= 1; n-- {
				if i+n > len(stemmed) {
					continue
				}
				k := strings.Join(stemmed[i:i+n], " ")
				if e, ok := g.byKey[k]; ok {
					key := e.Kind + ":" + stemKey(e.Canonical)
					m := found[key]
					if m == nil {
						m = &Mention{Kind: e.Kind, Name: e.Canonical, Key: stemKey(e.Canonical), Known: true, FirstByte: i}
						found[key] = m
					}
					m.Count++
					i += n - 1
					break
				}
			}
		}
	}
	// Unknown capitalized sequences (two+ words, or one word not at sentence start).
	for _, sent := range Sentences(text) {
		locs := capSeqRe.FindAllStringIndex(sent.Text, -1)
		for _, loc := range locs {
			surface := strings.TrimSpace(sent.Text[loc[0]:loc[1]])
			words := strings.Fields(surface)
			if len(words) < 2 && loc[0] == 0 {
				continue
			}
			if len([]rune(surface)) < 3 || len(words) > 6 {
				continue
			}
			k := stemKey(surface)
			if k == "" || stopKey[k] {
				continue
			}
			if g != nil {
				if e, ok := g.byKey[k]; ok {
					_ = e
					continue // already counted via gazetteer
				}
			}
			key := "other:" + k
			m := found[key]
			if m == nil {
				m = &Mention{Kind: guessKind(words), Name: surface, Key: k, FirstByte: sent.Start + loc[0]}
				found[key] = m
			}
			m.Count++
		}
	}
	out := make([]Mention, 0, len(found))
	for _, m := range found {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Known != out[j].Known {
			return out[i].Known
		}
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func guessKind(words []string) string {
	last := strings.ToLower(words[len(words)-1])
	for _, s := range []string{"sa", "srl", "ministerul", "agenția", "banca", "consiliul", "comisia", "партия", "министерство", "агентство"} {
		if strings.Contains(strings.ToLower(strings.Join(words, " ")), s) || last == s {
			return "organization"
		}
	}
	if len(words) == 2 || len(words) == 3 {
		return "person"
	}
	return "other"
}

// ---------- Facts ----------

type Fact struct {
	Type  string // number | money | percent | date | decision
	Key   string // normalized comparable key
	Text  string
	Start int
	End   int
	Value map[string]any
}

var (
	percentRe  = regexp.MustCompile(`(?i)(\d+(?:[.,]\d+)?)\s?(%|la sut[ăa]|процент[а-я]*)`)
	moneyRe    = regexp.MustCompile(`(?i)(\d[\d\s.,]*\d|\d)\s?(mii|mln|milioane|miliarde|млн|млрд|тыс\.?)?\s?(lei|mdl|euro|eur|€|dolari|usd|\$|леев|лея|лей|евро|долларов)`)
	numberRe   = regexp.MustCompile(`(?i)(\d[\d\s.,]*\d|\d+)\s?(kwh|mwh|mw|km|tone|tonne|тонн|кв?т[·.]?ч|locuri|persoane|человек|de)?`)
	dateROre   = regexp.MustCompile(`(?i)\b(\d{1,2})\s(ianuarie|februarie|martie|aprilie|mai|iunie|iulie|august|septembrie|octombrie|noiembrie|decembrie)(?:\s(\d{4}))?`)
	dateRUre   = regexp.MustCompile(`(?i)\b(\d{1,2})\s(января|февраля|марта|апреля|мая|июня|июля|августа|сентября|октября|ноября|декабря)(?:\s(\d{4}))?`)
	isoDateRe  = regexp.MustCompile(`\b(20\d{2})-(\d{2})-(\d{2})\b`)
	decisionRe = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(a aprobat|au aprobat|a adoptat|au adoptat|a decis|au decis|a majorat|a redus|a votat|au votat|a semnat|a promulgat|a anulat|a demis|a numit|aprobă|утвердил[аио]?|утвердило|принял[аио]?|приняло|решил[аио]?|повысил[аио]?|снизил[аио]?|проголосовал[аио]?|подписал[аио]?|отменил[аио]?|назначил[аио]?)(?:[^\p{L}]|$)`)
)

var monthIdx = map[string]int{"ianuarie": 1, "februarie": 2, "martie": 3, "aprilie": 4, "mai": 5, "iunie": 6, "iulie": 7, "august": 8,
	"septembrie": 9, "octombrie": 10, "noiembrie": 11, "decembrie": 12, "января": 1, "февраля": 2, "марта": 3, "апреля": 4, "мая": 5,
	"июня": 6, "июля": 7, "августа": 8, "сентября": 9, "октября": 10, "ноября": 11, "декабря": 12}

func parseNum(s string) (float64, bool) {
	s = strings.ReplaceAll(strings.TrimSpace(s), " ", "")
	if strings.Count(s, ",") == 1 && !strings.Contains(s, ".") {
		s = strings.Replace(s, ",", ".", 1)
	} else {
		s = strings.ReplaceAll(s, ",", "")
		if strings.Count(s, ".") > 1 {
			s = strings.ReplaceAll(s, ".", "")
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil
}

func scale(word string) float64 {
	switch strings.ToLower(strings.TrimSuffix(word, ".")) {
	case "mii", "тыс":
		return 1e3
	case "mln", "milioane", "млн":
		return 1e6
	case "miliarde", "млрд":
		return 1e9
	}
	return 1
}

func currency(c string) string {
	switch strings.ToLower(c) {
	case "lei", "mdl", "леев", "лея", "лей":
		return "mdl"
	case "euro", "eur", "€", "евро":
		return "eur"
	case "dolari", "usd", "$", "долларов":
		return "usd"
	}
	return c
}

func fmtNum(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// Facts extracts comparable facts with byte offsets.
func Facts(text string) []Fact {
	var out []Fact
	used := make([]bool, len(text)+1)
	mark := func(s, e int) bool {
		for i := s; i < e; i++ {
			if used[i] {
				return false
			}
		}
		for i := s; i < e; i++ {
			used[i] = true
		}
		return true
	}
	for _, m := range percentRe.FindAllStringSubmatchIndex(text, -1) {
		if v, ok := parseNum(text[m[2]:m[3]]); ok && mark(m[0], m[1]) {
			out = append(out, Fact{Type: "percent", Key: "percent:" + fmtNum(v), Text: text[m[0]:m[1]], Start: m[0], End: m[1], Value: map[string]any{"value": v}})
		}
	}
	for _, m := range moneyRe.FindAllStringSubmatchIndex(text, -1) {
		v, ok := parseNum(text[m[2]:m[3]])
		if !ok || !mark(m[0], m[1]) {
			continue
		}
		if m[4] >= 0 {
			v *= scale(text[m[4]:m[5]])
		}
		cur := currency(text[m[6]:m[7]])
		out = append(out, Fact{Type: "money", Key: "money:" + cur + ":" + fmtNum(v), Text: text[m[0]:m[1]], Start: m[0], End: m[1],
			Value: map[string]any{"amount": v, "currency": cur}})
	}
	for _, re := range []*regexp.Regexp{dateROre, dateRUre} {
		for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
			if !mark(m[0], m[1]) {
				continue
			}
			day, _ := strconv.Atoi(text[m[2]:m[3]])
			mon := monthIdx[strings.ToLower(text[m[4]:m[5]])]
			key := fmt.Sprintf("date:%02d-%02d", mon, day)
			val := map[string]any{"month": mon, "day": day}
			if m[6] >= 0 {
				y, _ := strconv.Atoi(text[m[6]:m[7]])
				key = fmt.Sprintf("date:%04d-%02d-%02d", y, mon, day)
				val["year"] = y
			}
			out = append(out, Fact{Type: "date", Key: key, Text: text[m[0]:m[1]], Start: m[0], End: m[1], Value: val})
		}
	}
	for _, m := range isoDateRe.FindAllStringSubmatchIndex(text, -1) {
		if mark(m[0], m[1]) {
			out = append(out, Fact{Type: "date", Key: "date:" + text[m[0]:m[1]], Text: text[m[0]:m[1]], Start: m[0], End: m[1], Value: map[string]any{}})
		}
	}
	for _, m := range numberRe.FindAllStringSubmatchIndex(text, -1) {
		v, ok := parseNum(text[m[2]:m[3]])
		if !ok || v < 10 || (v >= 1900 && v <= 2100 && m[4] < 0) { // skip tiny numbers and bare years
			continue
		}
		if !mark(m[2], m[3]) {
			continue
		}
		unit := ""
		if m[4] >= 0 {
			unit = strings.ToLower(text[m[4]:m[5]])
		}
		out = append(out, Fact{Type: "number", Key: "number:" + fmtNum(v) + ":" + Key(unit), Text: text[m[2]:m[3]], Start: m[2], End: m[3],
			Value: map[string]any{"value": v, "unit": unit}})
	}
	for _, m := range decisionRe.FindAllStringSubmatchIndex(text, -1) {
		if mark(m[2], m[3]) {
			verb := DecisionClass(text[m[2]:m[3]])
			out = append(out, Fact{Type: "decision", Key: "decision:" + verb, Text: text[m[2]:m[3]], Start: m[2], End: m[3], Value: map[string]any{}})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

// ---------- Claim candidates ----------

type ClaimCandidate struct {
	Text          string
	Start, End    int
	Checkability  string // checkable | opinion | prediction | rhetorical | quote_only
	ClaimType     string // statistic | decision | attribution | statement
	AttributedTo  string
	IsDirectQuote bool
	FactKeys      []string
}

var (
	attribRE  = regexp.MustCompile(`(?i)(?:potrivit|conform|a declarat|a anunțat|a afirmat|a precizat|a spus|susține|по словам|по данным|заявил[аи]?|сообщил[аи]?|отметил[аи]?|согласно)\s+([\p{Lu}][\p{L}\-]+(?:\s[\p{Lu}][\p{L}\-]+){0,3})`)
	attribRE2 = regexp.MustCompile(`([\p{Lu}][\p{L}\-]+(?:\s[\p{Lu}][\p{L}\-]+){0,3}),?\s+(?:a declarat|a anunțat|a afirmat|a precizat|susține|заявил[аи]?|сообщил[аи]?|отметил[аи]?)`)
	opinionRE = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(cred că|consider că|în opinia mea|în opinia|mi se pare|părerea mea|по моему мнению|считаю|думаю|на мой взгляд|ar trebui|trebuie să|должны|следует)(?:[^\p{L}]|$)`)
	predictRE = regexp.MustCompile(`(?i)(ar putea|se așteaptă|prognoz|estimează că|probabil|ar urma să|может|планирует|ожидается|прогноз|вероятно)`)
	futureRE  = regexp.MustCompile(`(?i)(va crește|va scădea|vor crește|vor scădea|urmează să|va fi|vor fi|будет|будут|вырастет|снизится)`)
	rhetorRE  = regexp.MustCompile(`\?\s*$`)
	quoteRE   = regexp.MustCompile(`[„"«“].{10,}[”"»]`)
)

// ClaimCandidates selects sentences that carry checkable factual content.
func ClaimCandidates(text string, max int) []ClaimCandidate {
	var out []ClaimCandidate
	for _, s := range Sentences(text) {
		if len([]rune(s.Text)) < 25 || len([]rune(s.Text)) > 600 {
			continue
		}
		facts := Facts(s.Text)
		var keys []string
		hasNum, hasDecision := false, false
		for _, f := range facts {
			keys = append(keys, f.Key)
			if f.Type == "percent" || f.Type == "money" || f.Type == "number" {
				hasNum = true
			}
			if f.Type == "decision" {
				hasDecision = true
			}
		}
		attrib := ""
		if m := attribRE.FindStringSubmatch(s.Text); m != nil {
			attrib = m[1]
		} else if m := attribRE2.FindStringSubmatch(s.Text); m != nil {
			attrib = m[1]
		}
		c := ClaimCandidate{Text: s.Text, Start: s.Start, End: s.End, AttributedTo: attrib, FactKeys: keys,
			IsDirectQuote: quoteRE.MatchString(s.Text)}
		switch {
		case rhetorRE.MatchString(s.Text):
			c.Checkability = "rhetorical"
		case opinionRE.MatchString(s.Text):
			c.Checkability = "opinion"
		case predictRE.MatchString(s.Text) && !hasDecision:
			c.Checkability = "prediction"
		case futureRE.MatchString(s.Text) && !hasDecision && !(hasNum && (hasDate(facts) || attrib != "")):
			// Plain future statements are predictions; scheduled changes with concrete figures and
			// dates (or an attributed announcement) are checkable announcements.
			c.Checkability = "prediction"
		case hasNum || hasDecision:
			c.Checkability = "checkable"
		case attrib != "":
			c.Checkability = "checkable"
		default:
			continue
		}
		switch {
		case hasDecision:
			c.ClaimType = "decision"
		case hasNum:
			c.ClaimType = "statistic"
		case attrib != "":
			c.ClaimType = "attribution"
		default:
			c.ClaimType = "statement"
		}
		out = append(out, c)
		if len(out) >= max {
			break
		}
	}
	return out
}

func hasDate(fs []Fact) bool {
	for _, f := range fs {
		if f.Type == "date" {
			return true
		}
	}
	return false
}

// ClaimKey normalizes a claim text into a deduplication key (order-insensitive stemmed terms
// plus fact keys), so the same claim in RO and RU transliterations tends to collide only when
// wording is near-identical. Cross-language claim linking happens via event + embeddings.
func ClaimKey(text string) string {
	terms := KeyTokens(text)
	sort.Strings(terms)
	uniq := terms[:0]
	for i, t := range terms {
		if i == 0 || t != terms[i-1] {
			uniq = append(uniq, t)
		}
	}
	return strings.Join(uniq, " ")
}

// IsCapitalized reports whether the first rune is upper-case.
func IsCapitalized(s string) bool {
	for _, r := range s {
		return unicode.IsUpper(r)
	}
	return false
}

// EntityKey is the canonical normalized key for entity identity (cross-script, stemmed).
func EntityKey(s string) string { return stemKey(s) }

// decisionClasses maps decision verbs in RO/RU to language-independent action classes so that
// the same decision reported in different languages yields the same fact key.
var decisionClasses = []struct {
	class string
	stems []string
}{
	{"approve", []string{"aprob", "adopt", "утверд", "принял", "принят"}},
	{"decide", []string{"decis", "решил"}},
	{"increase", []string{"major", "повыс"}},
	{"decrease", []string{"redus", "сниз"}},
	{"vote", []string{"votat", "голосов"}},
	{"sign", []string{"semnat", "promulg", "подпис"}},
	{"cancel", []string{"anulat", "отмен"}},
	{"appoint", []string{"numit", "назнач"}},
	{"dismiss", []string{"demis"}},
}

// DecisionClass returns the canonical action class of a decision verb phrase.
func DecisionClass(phrase string) string {
	l := strings.ToLower(phrase)
	for _, c := range decisionClasses {
		for _, st := range c.stems {
			if strings.Contains(l, st) {
				return c.class
			}
		}
	}
	return Stem(Key(phrase))
}
