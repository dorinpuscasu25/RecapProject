package nlp

import (
	"strings"
	"testing"
)

func TestDetectLanguage(t *testing.T) {
	cases := map[string]string{
		"Parlamentul a aprobat astăzi legea privind prețul la energie electrică.": "ro",
		"Парламент сегодня утвердил закон о цене на электроэнергию.":              "ru",
		"The parliament approved the law on electricity prices today.":            "en",
	}
	for text, want := range cases {
		if got, _ := DetectLanguage(text); got != want {
			t.Errorf("DetectLanguage(%q) = %s, want %s", text, got, want)
		}
	}
}

func TestTransliterationKeysConverge(t *testing.T) {
	if Key("Майя Санду") != "maia sandu" {
		t.Fatalf("got %q", Key("Майя Санду"))
	}
	if Key("Chișinău") != "chisinau" {
		t.Fatalf("got %q", Key("Chișinău"))
	}
}

func TestSimHashNearDuplicate(t *testing.T) {
	a := "Guvernul a aprobat majorarea salariului minim la 6300 de lei începând cu 1 ianuarie, a anunțat premierul la ședința de luni."
	b := "Guvernul a aprobat majorarea salariului minim la 6300 de lei începând cu 1 ianuarie, a anunțat premierul la ședința de luni dimineață."
	c := "Echipa națională de fotbal a câștigat meciul amical disputat la Chișinău în fața a zece mii de spectatori."
	if d := Hamming(SimHash(a), SimHash(b)); d > 12 {
		t.Errorf("near duplicates too far: %d", d)
	}
	if d := Hamming(SimHash(a), SimHash(c)); d < 15 {
		t.Errorf("unrelated texts too close: %d", d)
	}
}

func TestHashEmbedCrossLanguageSignal(t *testing.T) {
	ro := "Parlamentul Republicii Moldova a aprobat tariful la energie electrică. Maia Sandu a semnat legea."
	ru := "Парламент Республики Молдова утвердил тариф на электроэнергию. Майя Санду подписала закон."
	other := "Echipa de fotbal Sheriff Tiraspol a pierdut meciul din Liga Campionilor."
	e1, e2, e3 := HashEmbed(ro, 384), HashEmbed(ru, 384), HashEmbed(other, 384)
	if Cosine(e1, e2) <= Cosine(e1, e3) {
		t.Errorf("expected RO/RU same story to be closer (%.3f) than unrelated (%.3f)", Cosine(e1, e2), Cosine(e1, e3))
	}
}

func TestSentencesOffsets(t *testing.T) {
	text := "Dl. Popescu a declarat că inflația este 5,2%. Guvernul a aprobat bugetul! Ce urmează?"
	ss := Sentences(text)
	if len(ss) != 3 {
		t.Fatalf("want 3 sentences, got %d: %+v", len(ss), ss)
	}
	for _, s := range ss {
		if text[s.Start:s.End] != s.Text {
			t.Errorf("offset mismatch: %q vs %q", text[s.Start:s.End], s.Text)
		}
	}
}

func TestFacts(t *testing.T) {
	text := "Tariful crește cu 15% de la 1 noiembrie, până la 3,5 lei pe kWh. Bugetul este de 2 mln lei. Парламент утвердил закон 13 октября 2026."
	fs := Facts(text)
	keys := map[string]bool{}
	for _, f := range fs {
		keys[f.Key] = true
		if text[f.Start:f.End] != f.Text {
			t.Errorf("fact offset mismatch for %s", f.Key)
		}
	}
	for _, want := range []string{"percent:15", "money:mdl:3.5", "money:mdl:2000000", "date:11-01", "date:2026-10-13"} {
		if !keys[want] {
			t.Errorf("missing fact %s in %v", want, keys)
		}
	}
	hasDecision := false
	for k := range keys {
		if strings.HasPrefix(k, "decision:") {
			hasDecision = true
		}
	}
	if !hasDecision {
		t.Error("missing decision fact")
	}
}

func TestGazetteerCrossScript(t *testing.T) {
	g := NewGazetteer([]GazetteerEntry{{Kind: "person", Canonical: "Maia Sandu", Aliases: []string{"Майя Санду"}},
		{Kind: "organization", Canonical: "Parlamentul Republicii Moldova", Aliases: []string{"Parlamentul", "Парламент"}}})
	ro := g.Entities("Maia Sandu a promulgat legea votată de Parlamentul.")
	ru := g.Entities("Майя Санду подписала закон, принятый Парламентом.")
	if len(ro) < 2 || len(ru) < 2 {
		t.Fatalf("expected two known entities each, got ro=%+v ru=%+v", ro, ru)
	}
	if ro[0].Key != ru[0].Key && ro[0].Key != ru[1].Key {
		t.Errorf("cross-script keys differ: %v vs %v", ro, ru)
	}
}

func TestClaimCandidatesCheckability(t *testing.T) {
	text := "Guvernul a aprobat majorarea pensiilor cu 10%. Cred că această decizie este greșită pentru economie. " +
		"Prețurile vor crește probabil anul viitor, se așteaptă experții. Cine va plăti pentru toate acestea?"
	cs := ClaimCandidates(text, 10)
	got := map[string]string{}
	for _, c := range cs {
		got[c.Checkability] = c.Text
	}
	if _, ok := got["checkable"]; !ok {
		t.Error("expected a checkable claim")
	}
	if _, ok := got["opinion"]; !ok {
		t.Error("expected an opinion")
	}
}
