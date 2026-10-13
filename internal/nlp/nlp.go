// Package nlp contains deterministic, dependency-free text utilities for Romanian, Russian and
// English: normalization, transliteration, tokenization, language identification, sentence
// splitting, fingerprints (SHA-256 / SimHash), hashed character n-gram embeddings and simple
// pattern-based entity and fact extraction.
//
// These are baseline implementations used by the `heuristic` AI provider and as pre-filters for
// model-based processing. They are intentionally transparent and measurable (see internal/eval).
package nlp

import (
	"crypto/sha256"
	"hash/fnv"
	"math"
	"math/bits"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// ---------- Normalization ----------

var spaceRe = regexp.MustCompile(`\s+`)

// CollapseSpace trims and collapses whitespace.
func CollapseSpace(s string) string { return strings.TrimSpace(spaceRe.ReplaceAllString(s, " ")) }

// Romanian comma-below vs cedilla variants are unified (ş→ș, ţ→ț).
var roFix = strings.NewReplacer("ş", "ș", "Ş", "Ș", "ţ", "ț", "Ţ", "Ț")

// NormalizeText applies NFC, fixes Romanian diacritic variants and collapses whitespace.
func NormalizeText(s string) string {
	return CollapseSpace(roFix.Replace(norm.NFC.String(s)))
}

// StripDiacritics removes combining marks (ă→a, ș→s, й→и).
func StripDiacritics(s string) string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	out, _, err := transform.String(t, s)
	if err != nil {
		return s
	}
	return out
}

var cyrToLat = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "e", 'ж': "j", 'з': "z", 'и': "i", 'й': "i",
	'к': "c", 'л': "l", 'м': "m", 'н': "n", 'о': "o", 'п': "p", 'р': "r", 'с': "s", 'т': "t", 'у': "u", 'ф': "f",
	'х': "h", 'ц': "t", 'ч': "ci", 'ш': "s", 'щ': "s", 'ъ': "", 'ы': "i", 'ь': "", 'э': "e", 'ю': "iu", 'я': "ia",
}

// Transliterate maps Cyrillic to a Romanian-oriented Latin form so that RO and RU spellings of
// names converge (Санду→sandu, Кишинёв→cisinev, парламент→parlament).
func Transliterate(s string) string {
	var b strings.Builder
	rs := []rune(strings.ToLower(s))
	for i, r := range rs {
		if r == 'й' && i+1 < len(rs) && strings.ContainsRune("яюеё", rs[i+1]) {
			continue // Майя → maia, not maiia
		}
		if l, ok := cyrToLat[r]; ok {
			b.WriteString(l)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Key returns a cross-script comparison key: lower-case, transliterated, unaccented, alnum only.
func Key(s string) string {
	s = StripDiacritics(Transliterate(NormalizeText(s)))
	var b strings.Builder
	prevSpace := false
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
			prevSpace = false
		default:
			if !prevSpace && b.Len() > 0 {
				b.WriteByte(' ')
				prevSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// ---------- Tokenization ----------

// Tokens splits into lower-case word tokens (letters/digits), keeping the original script.
func Tokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// KeyTokens returns cross-script tokens with stop words removed and light stemming.
func KeyTokens(s string) []string {
	var out []string
	for _, t := range strings.Fields(Key(s)) {
		if len([]rune(t)) < 2 || stopKey[t] {
			continue
		}
		out = append(out, Stem(t))
	}
	return out
}

// Stem applies a crude suffix-stripping stemmer shared across RO/RU-transliterated text so that
// inflected forms (guvernul/guvernului, parlamentul/parlamenta) map together.
func Stem(t string) string {
	if len(t) <= 4 {
		return t
	}
	for _, suf := range []string{"urilor", "ului", "elor", "ilor", "ului", "ova", "evoi", "ami", "ami", "ach", "iah", "ogo", "omu",
		"ele", "ile", "ul", "le", "lui", "ii", "ei", "ia", "ie", "a", "e", "i", "u", "o", "y"} {
		if strings.HasSuffix(t, suf) && len(t)-len(suf) >= 4 {
			return t[:len(t)-len(suf)]
		}
	}
	return t
}

var stopwordsRO = splitSet(`și sau dar că de la în pe cu din pentru prin care ce cel cea cei cele un o unui unei unor al ale ai a fost este sunt va vor
au a fi mai nu se să s-a să-și le lui ei ea el ele ei acest această aceste acești acel acea după până între fără spre astfel
despre către iar ori deci totuși foarte mult multe toate tot toți avea are aveau am ați ar le-a i-a ne vă mă te ne`)
var stopwordsRU = splitSet(`и в во не что он на я с со как а то все она так его но да ты к у же вы за бы по только ее мне было вот от меня еще нет о из
ему теперь когда даже ну вдруг ли если уже или ни быть был него до вас нибудь опять уж вам ведь там потом себя ничего ей может они тут где
есть надо ней для мы тебя их чем была сам чтоб без будто чего раз тоже себе под будет ж тогда кто этот того потому этого какой совсем ним
здесь этом один почти мой тем чтобы нее сейчас были куда зачем всех никогда можно при наконец два об другой хоть после над больше тот через
эти нас про всего них какая много разве три эту моя впрочем хорошо свою этой перед иногда лучше чуть том нельзя такой им более всегда
конечно всю между это также заявил заявила`)
var stopwordsEN = splitSet(`the a an and or but of to in on at for with by from is are was were be been has have had it its this that these those
as not no will would can could should may might said says he she they we you i his her their our your than then there here also`)

var stopKey = func() map[string]bool {
	m := map[string]bool{}
	for _, set := range []map[string]bool{stopwordsRO, stopwordsRU, stopwordsEN} {
		for w := range set {
			m[Key(w)] = true
		}
	}
	return m
}()

func splitSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(s) {
		m[w] = true
	}
	return m
}

// ---------- Language identification ----------

// DetectLanguage returns ("ro"|"ru"|"en"|"und", confidence 0..1) using script ratios, language-
// specific letters and stop-word hit rates.
func DetectLanguage(s string) (string, float64) {
	var latin, cyr, total int
	roLetters := 0
	for _, r := range s {
		if !unicode.IsLetter(r) {
			continue
		}
		total++
		switch {
		case unicode.Is(unicode.Cyrillic, r):
			cyr++
		case unicode.Is(unicode.Latin, r):
			latin++
			switch r {
			case 'ă', 'â', 'î', 'ș', 'ț', 'ş', 'ţ', 'Ă', 'Â', 'Î', 'Ș', 'Ț':
				roLetters++
			}
		}
	}
	if total < 3 {
		return "und", 0
	}
	if float64(cyr)/float64(total) > 0.5 {
		// Romanian in Cyrillic (Moldovan Cyrillic) is rare in modern media; treat as ru.
		return "ru", math.Min(1, 0.6+float64(cyr)/float64(total)*0.4)
	}
	toks := Tokens(s)
	var ro, en int
	for _, t := range toks {
		if stopwordsRO[t] {
			ro++
		}
		if stopwordsEN[t] {
			en++
		}
	}
	ro += roLetters
	if ro == 0 && en == 0 {
		return "und", 0.2
	}
	if ro >= en {
		return "ro", math.Min(1, 0.5+float64(ro)/float64(ro+en+1)*0.5)
	}
	return "en", math.Min(1, 0.5+float64(en)/float64(ro+en+1)*0.5)
}

// ---------- Sentences ----------

type Span struct {
	Text  string
	Start int // byte offset in the original text
	End   int
}

var abbrev = splitSet(`dl. dna. d. str. nr. art. alin. lit. prof. dr. ing. ex. etc. ș.a. mun. or. s. r. г. ул. т.е. т.д. им. mr. mrs. st. vs.`)

// Sentences splits text into sentences with byte offsets into the original string.
func Sentences(text string) []Span {
	var out []Span
	start := 0
	runes := []rune(text)
	byteIdx := make([]int, len(runes)+1)
	b := 0
	for i, r := range runes {
		byteIdx[i] = b
		b += len(string(r))
	}
	byteIdx[len(runes)] = b
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r != '.' && r != '!' && r != '?' && r != '…' && r != '\n' {
			continue
		}
		if r == '.' {
			// skip decimals (3.5) and abbreviations
			if i+1 < len(runes) && unicode.IsDigit(runes[i+1]) && i > 0 && unicode.IsDigit(runes[i-1]) {
				continue
			}
			j := i - 1
			for j >= 0 && !unicode.IsSpace(runes[j]) {
				j--
			}
			word := strings.ToLower(string(runes[j+1 : i+1]))
			if abbrev[word] || (len([]rune(word)) == 2 && unicode.IsUpper(runes[j+1])) {
				continue
			}
		}
		end := i + 1
		for end < len(runes) && (runes[end] == '"' || runes[end] == '»' || runes[end] == '”' || runes[end] == ')') {
			end++
		}
		if end < len(runes) && !unicode.IsSpace(runes[end]) && r != '\n' {
			continue
		}
		appendSpan(&out, text, byteIdx[start], byteIdx[end])
		start = end
	}
	appendSpan(&out, text, byteIdx[start], byteIdx[len(runes)])
	return out
}

func appendSpan(out *[]Span, text string, s, e int) {
	seg := text[s:e]
	trimmedLeft := len(seg) - len(strings.TrimLeftFunc(seg, unicode.IsSpace))
	seg2 := strings.TrimSpace(seg)
	if len([]rune(seg2)) < 3 {
		return
	}
	st := s + trimmedLeft
	*out = append(*out, Span{Text: seg2, Start: st, End: st + len(seg2)})
}

// ---------- Fingerprints ----------

// ContentHash is SHA-256 over normalized, unaccented, lower-cased text.
func ContentHash(parts ...string) []byte {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(Key(p)))
		h.Write([]byte{0})
	}
	return h.Sum(nil)
}

// SimHash computes a 64-bit SimHash over 3-word shingles of the key-normalized text.
func SimHash(text string) uint64 {
	toks := strings.Fields(Key(text))
	if len(toks) == 0 {
		return 0
	}
	var v [64]int
	add := func(s string) {
		h := fnv.New64a()
		h.Write([]byte(s))
		x := h.Sum64()
		for i := 0; i < 64; i++ {
			if x&(1<<uint(i)) != 0 {
				v[i]++
			} else {
				v[i]--
			}
		}
	}
	if len(toks) < 3 {
		add(strings.Join(toks, " "))
	}
	for i := 0; i+3 <= len(toks); i++ {
		add(toks[i] + " " + toks[i+1] + " " + toks[i+2])
	}
	var out uint64
	for i := 0; i < 64; i++ {
		if v[i] > 0 {
			out |= 1 << uint(i)
		}
	}
	return out
}

// Hamming returns the number of differing bits.
func Hamming(a, b uint64) int { return bits.OnesCount64(a ^ b) }

// ---------- Hashed embeddings ----------

// HashEmbed produces an L2-normalized feature-hashed vector from cross-script stemmed tokens
// (weight 1.0) and character 4-grams of the key text (weight 0.5). Deterministic, model-free:
// model ref "local-hash-v1". Useful as a baseline and for tests; quality is measured in eval.
func HashEmbed(text string, dims int) []float32 {
	vec := make([]float64, dims)
	add := func(feat string, w float64) {
		h := fnv.New64a()
		h.Write([]byte(feat))
		x := h.Sum64()
		idx := int(x % uint64(dims))
		sign := 1.0
		if (x>>63)&1 == 1 {
			sign = -1.0
		}
		vec[idx] += sign * w
	}
	toks := KeyTokens(text)
	tf := map[string]int{}
	for _, t := range toks {
		tf[t]++
	}
	for t, n := range tf {
		add("w:"+t, 1+math.Log(float64(n)))
	}
	for i := 0; i+1 < len(toks); i++ {
		add("b:"+toks[i]+"_"+toks[i+1], 0.7)
	}
	k := []rune(" " + strings.Join(toks, " ") + " ")
	for i := 0; i+4 <= len(k); i++ {
		add("c:"+string(k[i:i+4]), 0.25)
	}
	var norm2 float64
	for _, x := range vec {
		norm2 += x * x
	}
	out := make([]float32, dims)
	if norm2 == 0 {
		return out
	}
	n := math.Sqrt(norm2)
	for i, x := range vec {
		out[i] = float32(x / n)
	}
	return out
}

// Cosine returns cosine similarity of two equal-length vectors.
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// Jaccard similarity of two string sets.
func Jaccard(a, b []string) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	sa := map[string]bool{}
	for _, x := range a {
		sa[x] = true
	}
	sb := map[string]bool{}
	for _, x := range b {
		sb[x] = true
	}
	inter := 0
	for x := range sa {
		if sb[x] {
			inter++
		}
	}
	union := len(sa) + len(sb) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// TopTerms returns the n most frequent key tokens.
func TopTerms(text string, n int) []string {
	tf := map[string]int{}
	for _, t := range KeyTokens(text) {
		if len(t) >= 4 {
			tf[t]++
		}
	}
	type kv struct {
		k string
		v int
	}
	var list []kv
	for k, v := range tf {
		list = append(list, kv{k, v})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].v != list[j].v {
			return list[i].v > list[j].v
		}
		return list[i].k < list[j].k
	})
	var out []string
	for i := 0; i < len(list) && i < n; i++ {
		out = append(out, list[i].k)
	}
	return out
}

// Truncate returns at most n runes, cutting at a word boundary and appending … when shortened.
func Truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	cut := string(r[:n])
	if i := strings.LastIndexAny(cut, " \n"); i > n/2 {
		cut = cut[:i]
	}
	return strings.TrimSpace(cut) + "…"
}

// NormalizeParagraphs normalizes each paragraph and joins them with blank lines.
func NormalizeParagraphs(s string) string {
	var out []string
	for _, p := range strings.Split(strings.ReplaceAll(s, "\r", ""), "\n") {
		if p = NormalizeText(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n\n")
}
