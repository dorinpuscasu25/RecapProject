package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dorinpuscasu25/recapproject/internal/ai"
	"github.com/dorinpuscasu25/recapproject/internal/nlp"
	"github.com/dorinpuscasu25/recapproject/internal/platform/bus"
	"github.com/dorinpuscasu25/recapproject/internal/platform/principal"
	"github.com/dorinpuscasu25/recapproject/internal/search"
)

// SummaryLanguages are always attempted for every event (plus the event's source languages).
var SummaryLanguages = []string{"ro", "ru"}

func (s *Service) createEvent(ctx context.Context, tx pgx.Tx, a *artInfo, d *decision) (uuid.UUID, error) {
	id := newID()
	titles := map[string]string{a.Language: a.Title}
	var occurred *time.Time
	if t := occurredFromFacts(a); t != nil {
		occurred = t
	}
	if _, err := tx.Exec(ctx, `INSERT INTO events (id, titles, state, occurred_start, earliest_published_at, last_material_update_at, primary_category_id,
		languages, version) VALUES ($1,$2,'developing',$3,$4,$4,$5,$6,1)`, id, mustJSON(titles), occurred, a.PublishedAt, a.PrimaryCategory, []string{a.Language}); err != nil {
		return uuid.Nil, err
	}
	conf := 1.0
	if d != nil && len(d.Candidates) > 0 {
		conf = 1 - d.Candidates[0].Score
	}
	if _, err := tx.Exec(ctx, `INSERT INTO event_article_links (event_id, article_id, relation, confidence, rationale) VALUES ($1,$2,'origin',$3,$4)`,
		id, a.ID, conf, mustJSON(d)); err != nil {
		return uuid.Nil, err
	}
	if d != nil && d.RelatedTo != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO event_relationships (from_event_id, to_event_id, relation, confidence) VALUES ($1,$2,'related',$3) ON CONFLICT DO NOTHING`,
			id, *d.RelatedTo, d.Candidates[0].Score); err != nil {
			return uuid.Nil, err
		}
	}
	if err := s.linkArticleClaims(ctx, tx, id, a); err != nil {
		return uuid.Nil, err
	}
	agg, err := s.recompute(ctx, tx, id, true)
	if err != nil {
		return uuid.Nil, err
	}
	if err := s.snapshot(ctx, tx, id, 1, "created", d); err != nil {
		return uuid.Nil, err
	}
	what := map[string]any{"summary": "Event first reported", "language": a.Language}
	if _, err := tx.Exec(ctx, `INSERT INTO event_updates (id, event_id, event_version, update_kind, what_changed, supporting_article_ids, severity, occurred_at)
		VALUES ($1,$2,1,'created',$3,$4,'minor',$5)`, newID(), id, mustJSON(what), []uuid.UUID{a.ID}, a.PublishedAt); err != nil {
		return uuid.Nil, err
	}
	if _, err := bus.Publish(ctx, tx, bus.Draft{Type: bus.EventCreated, AggregateType: "event", AggregateID: id.String(),
		Payload: Changed{EventID: id, Version: 1, Kind: "created", Material: true, ArticleIDs: []uuid.UUID{a.ID}, CategoryIDs: agg.categoryIDs, Importance: agg.importance}}); err != nil {
		return uuid.Nil, err
	}
	return id, s.requestVerification(ctx, tx, id, a.ClaimIDs, "new_event")
}

func occurredFromFacts(a *artInfo) *time.Time {
	for _, k := range a.FactKeys {
		if !strings.HasPrefix(k, "date:") || len(k) != len("date:2006-01-02") {
			continue
		}
		t, err := time.Parse("2006-01-02", strings.TrimPrefix(k, "date:"))
		if err == nil && math.Abs(a.PublishedAt.Sub(t).Hours()) < 24*7 {
			return &t
		}
	}
	return nil
}

func (s *Service) linkArticleClaims(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, a *artInfo) error {
	if len(a.ClaimIDs) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO event_claims (event_id, claim_id) SELECT $1, unnest($2::uuid[]) ON CONFLICT DO NOTHING`, eventID, a.ClaimIDs); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE claim_occurrences SET event_id=$1 WHERE article_id=$2`, eventID, a.ID)
	return err
}

func (s *Service) requestVerification(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, claims []uuid.UUID, reason string) error {
	for _, c := range claims {
		ev := eventID
		if _, err := bus.Publish(ctx, tx, bus.Draft{Type: bus.ClaimVerificationRequested, AggregateType: "claim", AggregateID: c.String(),
			Payload: VerificationRequested{ClaimID: c, EventID: &ev, Reason: reason}}); err != nil {
			return err
		}
	}
	return nil
}

// applyToEvent attaches an article (or a revision of an attached article) and decides whether
// it materially changes the event.
func (s *Service) applyToEvent(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, a *artInfo, relation string, d *decision) error {
	var fp []byte
	var version int
	var state, editorial string
	var lastUpdate *time.Time
	var summaries []byte
	if err := tx.QueryRow(ctx, `SELECT fingerprint, version, state, editorial_status, last_material_update_at, summaries FROM events WHERE id=$1 FOR UPDATE`, eventID).
		Scan(&fp, &version, &state, &editorial, &lastUpdate, &summaries); err != nil {
		return err
	}
	var f struct {
		FactKeys []string `json:"fact_keys"`
	}
	_ = json.Unmarshal(fp, &f)
	if relation != "revision" {
		conf := 0.0
		if d != nil && len(d.Candidates) > 0 {
			conf = d.Candidates[0].Score
		}
		if _, err := tx.Exec(ctx, `INSERT INTO event_article_links (event_id, article_id, relation, confidence, rationale) VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (event_id, article_id) DO UPDATE SET relation=EXCLUDED.relation, unlinked_at=NULL, rationale=EXCLUDED.rationale, linked_at=now()`,
			eventID, a.ID, relation, conf, mustJSON(d)); err != nil {
			return err
		}
	}
	if err := s.linkArticleClaims(ctx, tx, eventID, a); err != nil {
		return err
	}
	// Novelty: only facts not already known can make an update material.
	known := map[string]bool{}
	for _, k := range f.FactKeys {
		known[k] = true
	}
	var newKeys, newTexts []string
	for i, k := range a.FactKeys {
		if !known[k] {
			newKeys = append(newKeys, k)
			newTexts = append(newTexts, a.FactTexts[i])
		}
	}
	var sm map[string]Summary
	_ = json.Unmarshal(summaries, &sm)
	evSummary := ""
	for _, l := range []string{a.Language, "ro", "ru"} {
		if x, ok := sm[l]; ok {
			evSummary = x.Normal
			break
		}
	}
	material, kind, severity := false, "coverage", "minor"
	var nov ai.NoveltyOutput
	var novModel string
	if len(newKeys) > 0 || a.IsCorrection {
		exec, err := s.gw.GenerateStructured(ctx, ai.TaskNoveltyDetection, &ai.NoveltyInput{EventSummary: nlp.Truncate(evSummary, 1500), KnownFactKeys: f.FactKeys,
			NewFactKeys: newKeys, NewFactTexts: newTexts, NewArticleLead: nlp.Truncate(a.Title+". "+a.Lead, 800), Language: a.Language},
			&nov, ai.CallOpts{SubjectType: "event", SubjectID: eventID.String()})
		if err != nil {
			return fmt.Errorf("novelty: %w", err)
		}
		novModel = exec.ModelRef
		material = nov.MaterialChange
		severity = nov.Severity
		if material {
			kind = "material_update"
		}
		if len(nov.Contradictions) > 0 {
			kind = "contradiction"
		}
	}
	if a.IsCorrection {
		material, kind, severity = true, "correction", "major"
	}
	newVersion := version
	if material {
		newVersion = version + 1
		newState := state
		switch kind {
		case "correction":
			newState = "corrected"
		case "contradiction":
			newState = "disputed"
		default:
			if state == "developing" {
				newState = "ongoing"
			}
		}
		when := a.PublishedAt
		if lastUpdate != nil && when.Before(*lastUpdate) {
			when = time.Now().UTC() // late-arriving report of a change: record the time we learned it
		}
		if _, err := tx.Exec(ctx, `UPDATE events SET version=$2, state=$3, last_material_update_at=$4, updated_at=now() WHERE id=$1`,
			eventID, newVersion, newState, when); err != nil {
			return err
		}
		what := map[string]any{"summary": nov.WhatChanged, "new_facts": nov.NewFacts, "contradictions": nov.Contradictions, "model_ref": novModel, "language": a.Language}
		if kind == "correction" && nov.WhatChanged == "" {
			what["summary"] = "A source published a correction"
		}
		requiresApproval := kind == "contradiction" || severity == "critical"
		if _, err := tx.Exec(ctx, `INSERT INTO event_updates (id, event_id, event_version, update_kind, what_changed, supporting_article_ids, affected_claim_ids,
			severity, requires_approval, occurred_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, newID(), eventID, newVersion, kind, mustJSON(what),
			[]uuid.UUID{a.ID}, nonNilUUID(a.ClaimIDs), severity, requiresApproval, a.PublishedAt); err != nil {
			return err
		}
	}
	agg, err := s.recompute(ctx, tx, eventID, material || editorial == "automatic" && newLanguage(sm, a.Language))
	if err != nil {
		return err
	}
	if material {
		if err := s.snapshot(ctx, tx, eventID, newVersion, kind, d); err != nil {
			return err
		}
	}
	payload := Changed{EventID: eventID, Version: newVersion, Kind: kind, Material: material, Severity: severity, ArticleIDs: []uuid.UUID{a.ID},
		CategoryIDs: agg.categoryIDs, Importance: agg.importance}
	if _, err := bus.Publish(ctx, tx, bus.Draft{Type: bus.EventUpdated, AggregateType: "event", AggregateID: eventID.String(), Payload: payload}); err != nil {
		return err
	}
	if !material {
		// Non-material coverage changes keep the version; force a projection refresh.
		if err := search.RequestIndexTx(ctx, tx, "event", eventID, true); err != nil {
			return err
		}
	}
	return s.requestVerification(ctx, tx, eventID, a.ClaimIDs, "event_coverage")
}

func newLanguage(sm map[string]Summary, lang string) bool {
	_, ok := sm[lang]
	return !ok
}

func nonNilUUID(s []uuid.UUID) []uuid.UUID {
	if s == nil {
		return []uuid.UUID{}
	}
	return s
}

type aggregate struct {
	categoryIDs []uuid.UUID
	importance  float32
}

type srcRow struct {
	id, publisher uuid.UUID
	pubName, url  string
	title, lead   string
	body          string
	lang          string
	published     time.Time
	allowSumm     bool
}

// recompute refreshes counts, entities, categories, fingerprint and search text from the linked
// articles; when resynth is true it regenerates cited summaries.
func (s *Service) recompute(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, resynth bool) (*aggregate, error) {
	rows, err := tx.Query(ctx, `SELECT a.id, a.publisher_id, p.name, a.canonical_url, a.title, a.description, COALESCE(a.body_text,''), a.language,
		COALESCE(a.published_at, a.first_seen_at),
		COALESCE((SELECT sp.allow_summarize FROM source_permissions sp WHERE sp.publisher_id=a.publisher_id
			AND (sp.source_endpoint_id=a.source_endpoint_id OR sp.source_endpoint_id IS NULL) ORDER BY sp.source_endpoint_id NULLS LAST LIMIT 1), false)
		FROM event_article_links l JOIN articles a ON a.id=l.article_id JOIN publishers p ON p.id=a.publisher_id
		WHERE l.event_id=$1 AND l.unlinked_at IS NULL AND a.tombstoned_at IS NULL AND a.status <> 'withdrawn'
		ORDER BY COALESCE(a.published_at, a.first_seen_at)`, eventID)
	if err != nil {
		return nil, err
	}
	var srcs []srcRow
	for rows.Next() {
		var r srcRow
		if err := rows.Scan(&r.id, &r.publisher, &r.pubName, &r.url, &r.title, &r.lead, &r.body, &r.lang, &r.published, &r.allowSumm); err != nil {
			rows.Close()
			return nil, err
		}
		srcs = append(srcs, r)
	}
	rows.Close()
	if len(srcs) == 0 {
		return &aggregate{}, nil
	}
	publishers := map[uuid.UUID]bool{}
	langSet := map[string]bool{}
	var articleIDs []uuid.UUID
	var texts []string
	for _, r := range srcs {
		publishers[r.publisher] = true
		if r.lang != "und" {
			langSet[r.lang] = true
		}
		articleIDs = append(articleIDs, r.id)
		texts = append(texts, r.title)
	}
	var langs []string
	for l := range langSet {
		langs = append(langs, l)
	}
	sort.Strings(langs)
	// Entities: union weighted by article frequency.
	if _, err := tx.Exec(ctx, `DELETE FROM event_entities WHERE event_id=$1`, eventID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO event_entities (event_id, entity_id, weight)
		SELECT $1, entity_id, count(*)::real FROM article_entities WHERE article_id = ANY($2) GROUP BY entity_id`, eventID, articleIDs); err != nil {
		return nil, err
	}
	// Categories derived by majority from article assignments; editorial assignments preserved.
	if _, err := tx.Exec(ctx, `DELETE FROM event_categories WHERE event_id=$1 AND source='derived'`, eventID); err != nil {
		return nil, err
	}
	catRows, err := tx.Query(ctx, `SELECT category_id, count(*) AS n, bool_or(is_primary) FROM (
			SELECT DISTINCT ON (article_id, category_id) article_id, category_id, is_primary FROM article_categories
			WHERE article_id = ANY($1) ORDER BY article_id, category_id, (source='editorial') DESC) x
		GROUP BY category_id ORDER BY count(*) FILTER (WHERE is_primary) DESC, n DESC`, articleIDs)
	if err != nil {
		return nil, err
	}
	type cat struct {
		id   uuid.UUID
		n    int
		prim bool
	}
	var cats []cat
	for catRows.Next() {
		var c cat
		if err := catRows.Scan(&c.id, &c.n, &c.prim); err != nil {
			catRows.Close()
			return nil, err
		}
		cats = append(cats, c)
	}
	catRows.Close()
	agg := &aggregate{}
	var primary *uuid.UUID
	var editorialPrimary uuid.UUID
	_ = tx.QueryRow(ctx, `SELECT category_id FROM event_categories WHERE event_id=$1 AND source='editorial' AND is_primary LIMIT 1`, eventID).Scan(&editorialPrimary)
	for i, c := range cats {
		isPrim := i == 0 && editorialPrimary == uuid.Nil
		if isPrim {
			id := c.id
			primary = &id
		}
		agg.categoryIDs = append(agg.categoryIDs, c.id)
		if _, err := tx.Exec(ctx, `INSERT INTO event_categories (event_id, category_id, source, is_primary, confidence) VALUES ($1,$2,'derived',$3,$4)
			ON CONFLICT DO NOTHING`, eventID, c.id, isPrim, float32(c.n)/float32(len(srcs))); err != nil {
			return nil, err
		}
	}
	if editorialPrimary != uuid.Nil {
		primary = &editorialPrimary
	}
	// Fingerprint for candidate scoring.
	fpRows, err := tx.Query(ctx, `SELECT DISTINCT e.normalized_key, e.kind FROM event_entities ee JOIN entities e ON e.id=ee.entity_id WHERE ee.event_id=$1`, eventID)
	if err != nil {
		return nil, err
	}
	var entKeys []string
	kinds := map[string]string{}
	for fpRows.Next() {
		var k, kind string
		if err := fpRows.Scan(&k, &kind); err != nil {
			fpRows.Close()
			return nil, err
		}
		entKeys = append(entKeys, k)
		kinds[k] = kind
	}
	fpRows.Close()
	factRows, err := tx.Query(ctx, `SELECT DISTINCT fact_key FROM article_facts WHERE article_id = ANY($1)`, articleIDs)
	if err != nil {
		return nil, err
	}
	factKeys, err := pgx.CollectRows(factRows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	var locations []string
	for k, kind := range kinds {
		if kind == "location" {
			locations = append(locations, k)
		}
	}
	sort.Strings(locations)
	fingerprint := map[string]any{"entity_keys": entKeys, "entity_kinds": kinds, "fact_keys": factKeys}
	importance := float32(1 - math.Exp(-(0.45*float64(len(publishers)) + 0.08*float64(len(srcs)))))
	agg.importance = importance

	var titlesRaw, summariesRaw []byte
	var version int
	var editorial string
	if err := tx.QueryRow(ctx, `SELECT titles, summaries, version, editorial_status FROM events WHERE id=$1`, eventID).Scan(&titlesRaw, &summariesRaw, &version, &editorial); err != nil {
		return nil, err
	}
	titles := map[string]string{}
	_ = json.Unmarshal(titlesRaw, &titles)
	summaries := map[string]Summary{}
	_ = json.Unmarshal(summariesRaw, &summaries)
	for _, r := range srcs {
		if titles[r.lang] == "" && r.lang != "und" {
			titles[r.lang] = r.title
		}
	}
	coverage := float32(0)
	if resynth && editorial != "locked" {
		targets := map[string]bool{}
		for _, l := range SummaryLanguages {
			targets[l] = true
		}
		for _, l := range langs {
			targets[l] = true
		}
		for lang := range targets {
			sum, err := s.synthesize(ctx, eventID, version, lang, srcs)
			if err != nil {
				var pe *ai.ProviderError
				if errors.As(err, &pe) || errors.Is(err, ai.ErrDeferred) {
					s.log.Info("summary not generated", "event_id", eventID, "language", lang, "reason", err.Error())
					continue // no summary in this language (explicit language_fallback at read time)
				}
				return nil, err
			}
			summaries[lang] = *sum
			if sum.Title != "" && (titles[lang] == "" || editorial == "automatic") {
				titles[lang] = sum.Title
			}
		}
	}
	for _, sm := range summaries {
		coverage = float32(math.Max(float64(coverage), citationCoverage(sm.Normal)))
	}
	var sb strings.Builder
	for _, l := range []string{"ro", "ru", "en"} {
		if t := titles[l]; t != "" {
			sb.WriteString(t + ". ")
		}
		if sm, ok := summaries[l]; ok {
			sb.WriteString(stripRefs(sm.Normal) + " ")
		}
	}
	for _, r := range srcs {
		sb.WriteString(r.title + ". ")
		if sb.Len() > 6000 {
			break
		}
	}
	_, err = tx.Exec(ctx, `UPDATE events SET titles=$2, summaries=$3, article_count=$4, source_count=$5, languages=$6, locations=$7, primary_category_id=$8,
		importance=$9, fingerprint=$10, search_text=$11, citation_coverage=$12, earliest_published_at=LEAST(earliest_published_at, $13),
		confidence=$14, updated_at=now() WHERE id=$1`,
		eventID, mustJSON(titles), mustJSON(summaries), len(srcs), len(publishers), nonNil(langs), nonNil(locations), primary, importance,
		mustJSON(fingerprint), sb.String(), coverage, srcs[0].published, float32(math.Min(1, 0.4+0.15*float64(len(publishers)))))
	return agg, err
}

func (s *Service) synthesize(ctx context.Context, eventID uuid.UUID, version int, lang string, srcs []srcRow) (*Summary, error) {
	var snippets []ai.SourceSnippet
	cites := map[string]Citation{}
	seenPub := map[uuid.UUID]int{}
	for _, r := range srcs {
		if !r.allowSumm || seenPub[r.publisher] >= 2 || len(snippets) >= 8 {
			continue
		}
		seenPub[r.publisher]++
		ref := fmt.Sprintf("a%d", len(snippets)+1)
		text := r.body
		if text == "" {
			text = r.lead
		}
		pub := r.published
		snippets = append(snippets, ai.SourceSnippet{Ref: ref, Publisher: r.pubName, PublishedAt: pub.Format(time.RFC3339), Language: r.lang, Title: r.title,
			Text: nlp.Truncate(text, 2500)})
		cites[ref] = Citation{Ref: ref, ArticleID: r.id, Publisher: r.pubName, URL: r.url, Title: r.title, PublishedAt: &pub, Language: r.lang}
	}
	if len(snippets) == 0 {
		return nil, &ai.ProviderError{Category: ai.ErrUnsupported, Msg: "no sources permit summarization"}
	}
	var out ai.SynthesisOutput
	exec, err := s.gw.GenerateStructured(ctx, ai.TaskEventSynthesis, &ai.SynthesisInput{Language: lang, Sources: snippets}, &out,
		ai.CallOpts{SubjectType: "event", SubjectID: eventID.String()})
	if err != nil {
		return nil, err
	}
	sum := &Summary{Title: out.Title, Short: out.Short, Normal: out.Normal, Detailed: out.Detailed, Disputed: out.Disputed, ModelRef: exec.ModelRef,
		GeneratedAt: time.Now().UTC(), EventVersion: version}
	for _, ref := range out.Citations {
		sum.Citations = append(sum.Citations, cites[ref])
	}
	return sum, nil
}

func stripRefs(s string) string {
	for _, r := range ai.CitedRefs(s) {
		s = strings.ReplaceAll(s, " ["+r+"]", "")
		s = strings.ReplaceAll(s, "["+r+"]", "")
	}
	return s
}

// citationCoverage is the share of sentences carrying at least one [ref].
func citationCoverage(text string) float64 {
	ss := nlp.Sentences(text)
	if len(ss) == 0 {
		return 0
	}
	n := 0
	for _, sp := range ss {
		if len(ai.CitedRefs(sp.Text)) > 0 {
			n++
		}
	}
	// A trailing citation marker is often split into the next "sentence"; normalize.
	return math.Min(1, float64(n)/float64(len(ss)))
}

func (s *Service) snapshot(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, version int, reason string, d *decision) error {
	var titles, summaries, fp []byte
	var state string
	if err := tx.QueryRow(ctx, `SELECT titles, summaries, fingerprint, state FROM events WHERE id=$1`, eventID).Scan(&titles, &summaries, &fp, &state); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT article_id FROM event_article_links WHERE event_id=$1 AND unlinked_at IS NULL`, eventID)
	if err != nil {
		return err
	}
	arts, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	snap := map[string]any{"titles": json.RawMessage(titles), "summaries": json.RawMessage(summaries), "fingerprint": json.RawMessage(fp),
		"state": state, "article_ids": arts}
	var by *uuid.UUID
	if p := principal.From(ctx); p != nil && p.Kind == principal.KindUser {
		by = &p.ID
	}
	modelRef := ""
	if d != nil {
		modelRef = d.ModelRef
	}
	_, err = tx.Exec(ctx, `INSERT INTO event_versions (id, event_id, version, snapshot, change_reason, created_by, model_ref) VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (event_id, version) DO NOTHING`, newID(), eventID, version, mustJSON(snap), reason, by, modelRef)
	return err
}
