package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/dorinpuscasu25/recapproject/internal/app"
	"github.com/dorinpuscasu25/recapproject/internal/ingest"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/sources"
)

type endpointCheck struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	URL      string    `json:"url"`
	Type     string    `json:"type"`
	TestOK   bool      `json:"test_ok"`
	Format   string    `json:"format"`
	Items    int       `json:"items"`
	Errors   []string  `json:"errors"`
	Run      string    `json:"run_status"`
	Accepted int       `json:"accepted"`
	Articles int       `json:"articles"`
	Langs    string    `json:"languages"`
	AvgChars int       `json:"avg_body_chars"`
	Cats     string    `json:"top_categories"`
	Linked   int       `json:"linked_to_events"`
}

// verifySources registers real sources under an operator-stated legal basis, probes and crawls a
// few items from each, waits for the pipeline and writes a report (markdown + JSON).
func verifySources(ctx context.Context, args []string, log *slog.Logger) (int, error) {
	fs := flag.NewFlagSet("verify-sources", flag.ContinueOnError)
	manifest := fs.String("manifest", "config/sources/moldova.json", "source manifest")
	legal := fs.String("legal-basis", "", "REQUIRED: the legal basis under which you may crawl these sources (recorded on each permission)")
	maxItems := fs.Int("max-items", 5, "items per source")
	only := fs.String("only", "", "only endpoints whose name contains this text")
	timeout := fs.Duration("timeout", 10*time.Minute, "max time to wait for processing")
	out := fs.String("out", "", "report path (default var/reports/sources-<time>.md)")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if strings.TrimSpace(*legal) == "" {
		return 2, errors.New("-legal-basis is required: state the permission or legal basis for crawling these publishers")
	}
	a, err := open(ctx, log)
	if err != nil {
		return 1, err
	}
	defer a.Close()
	if !a.Cfg.CrawlLiveEnabled {
		return 2, errors.New("live crawling is disabled: set CRAWL_LIVE_ENABLED=true")
	}
	raw, err := os.ReadFile(filepath.Clean(*manifest))
	if err != nil {
		return 1, err
	}
	var m fixtureManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return 1, err
	}
	var checks []*endpointCheck
	start := time.Now()
	for _, s := range m.Sources {
		if *only != "" && !strings.Contains(strings.ToLower(*s.Endpoint.Name), strings.ToLower(*only)) {
			continue
		}
		s.Permission = sources.PermissionInput{LegalBasis: *legal + " (verification run " + start.UTC().Format(time.RFC3339) + ")",
			PermissionStatus: "granted", AllowCrawl: true, AllowStoreFullText: true, AllowSummarize: true, AllowEmbed: true, AllowSnippets: true,
			RobotsReviewStatus: "unreviewed", TermsReviewStatus: "unreviewed", AttributionText: "Sursa: " + *s.Publisher.Name}
		mx := *maxItems
		s.Endpoint.MaxItemsPerPoll = &mx
		ids, err := registerSources(ctx, a, []manifestSource{s}, false)
		if err != nil {
			checks = append(checks, &endpointCheck{Name: *s.Endpoint.Name, URL: *s.Endpoint.EntryURL, Errors: []string{err.Error()}})
			continue
		}
		c := &endpointCheck{ID: ids[0], Name: *s.Endpoint.Name, URL: *s.Endpoint.EntryURL, Type: *s.Endpoint.EndpointType, Errors: []string{}}
		fap := true
		res, err := a.Ingest.TestSource(ctx, ingest.TestSourceInput{EndpointType: c.Type, EntryURL: c.URL, FetchArticlePages: &fap, PreviewItems: 2})
		if err != nil {
			c.Errors = append(c.Errors, err.Error())
		} else {
			c.TestOK, c.Format, c.Items = res.OK, res.DetectedFormat, res.ItemCount
			c.Errors = append(c.Errors, res.Errors...)
		}
		if c.TestOK {
			if _, err := a.Sources.SetEndpointStatus(ctx, c.ID, "enabled"); err != nil {
				c.Errors = append(c.Errors, "enable: "+err.Error())
			} else if _, err := a.Ingest.RequestCrawl(ctx, c.ID, nil, *maxItems); err != nil {
				c.Errors = append(c.Errors, "crawl: "+err.Error())
			}
		}
		checks = append(checks, c)
		fmt.Fprintf(os.Stderr, "%-24s test_ok=%v items=%d %s\n", c.Name, c.TestOK, c.Items, strings.Join(c.Errors, "; "))
	}

	// Wait for the pipeline: drain the PostgreSQL bus here, or wait for the worker (Kafka).
	deadline := time.Now().Add(*timeout)
	for time.Now().Before(deadline) {
		if a.Cfg.BusDriver != "kafka" {
			if err := a.Drain(ctx); err != nil {
				return 1, err
			}
		}
		var pending int
		_ = a.DB.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM outbox_events WHERE published_at IS NULL)
			+ (SELECT count(*) FROM source_crawl_runs WHERE status IN ('queued','processing') AND started_at > $1)
			+ (SELECT count(*) FROM articles WHERE first_seen_at > $1 AND status NOT IN ('enriched','duplicate','quarantined','failed','withdrawn'))`, start).Scan(&pending)
		if pending == 0 {
			break
		}
		time.Sleep(3 * time.Second)
	}

	for _, c := range checks {
		if c.ID == uuid.Nil {
			continue
		}
		var run *string
		_ = a.DB.Pool.QueryRow(ctx, `SELECT status, accepted_count FROM source_crawl_runs WHERE source_endpoint_id=$1 ORDER BY started_at DESC LIMIT 1`, c.ID).Scan(&run, &c.Accepted)
		if run != nil {
			c.Run = *run
		}
		_ = a.DB.Pool.QueryRow(ctx, `SELECT count(*), coalesce(string_agg(DISTINCT language, ','),''), coalesce(avg(body_chars),0)::int,
			count(*) FILTER (WHERE EXISTS (SELECT 1 FROM event_article_links l WHERE l.article_id=a.id AND l.unlinked_at IS NULL))
			FROM articles a WHERE source_endpoint_id=$1`, c.ID).Scan(&c.Articles, &c.Langs, &c.AvgChars, &c.Linked)
		_ = a.DB.Pool.QueryRow(ctx, `SELECT coalesce(string_agg(slug||':'||n, ', ' ORDER BY n DESC),'') FROM (
			SELECT c.slug, count(*) n FROM article_categories ac JOIN categories c ON c.id=ac.category_id JOIN articles a ON a.id=ac.article_id
			WHERE a.source_endpoint_id=$1 GROUP BY c.slug ORDER BY n DESC LIMIT 4) x`, c.ID).Scan(&c.Cats)
	}
	report, err := renderReport(ctx, a, checks, start, *legal)
	if err != nil {
		return 1, err
	}
	path := *out
	if path == "" {
		path = filepath.Join("var", "reports", "sources-"+start.UTC().Format("20060102-150405")+".md")
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755) // #nosec G301 -- operator report directory
	// #nosec G306 -- report has no secrets
	if err := os.WriteFile(path, []byte(report), 0o644); err != nil {
		return 1, err
	}
	printJSON(map[string]any{"report": path, "endpoints": checks})
	for _, c := range checks {
		if !c.TestOK {
			return 3, fmt.Errorf("one or more sources failed (see %s)", path)
		}
	}
	return 0, nil
}

func renderReport(ctx context.Context, a *app.App, checks []*endpointCheck, start time.Time, legal string) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "# Real-source verification — %s\n\nLegal basis stated by the operator: %s\n\n", start.UTC().Format(time.RFC3339), legal)
	b.WriteString("| Source | Type | Test | Items | Run | Accepted | Articles | Languages | Avg chars | Linked to events | Top categories | Errors |\n|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, c := range checks {
		fmt.Fprintf(&b, "| %s | %s | %v | %d | %s | %d | %d | %s | %d | %d | %s | %s |\n", c.Name, c.Type, c.TestOK, c.Items, c.Run, c.Accepted,
			c.Articles, c.Langs, c.AvgChars, c.Linked, c.Cats, strings.ReplaceAll(strings.Join(c.Errors, "; "), "|", "/"))
	}
	rows, err := a.DB.Pool.Query(ctx, `SELECT coalesce(e.titles->>'ro', e.titles->>'ru', ''), e.source_count, e.article_count, array_to_string(e.languages, ','), e.version,
		(SELECT string_agg(DISTINCT p.name, ', ') FROM event_article_links l JOIN articles a ON a.id=l.article_id JOIN publishers p ON p.id=a.publisher_id
		 WHERE l.event_id=e.id AND l.unlinked_at IS NULL)
		FROM events e WHERE e.created_at > $1 AND e.source_count >= 2 ORDER BY e.source_count DESC, e.article_count DESC LIMIT 25`, start)
	if err != nil {
		return "", err
	}
	b.WriteString("\n## Events with several sources (clustering and attribution)\n\n| Event | Sources | Articles | Languages | Version | Publishers |\n|---|---|---|---|---|---|\n")
	for rows.Next() {
		var title, langs string
		var pubs *string
		var src, arts, ver int
		if err := rows.Scan(&title, &src, &arts, &langs, &ver, &pubs); err != nil {
			rows.Close()
			return "", err
		}
		p := ""
		if pubs != nil {
			p = *pubs
		}
		fmt.Fprintf(&b, "| %s | %d | %d | %s | %d | %s |\n", strings.ReplaceAll(title, "|", "/"), src, arts, langs, ver, p)
	}
	rows.Close()
	var events, multi, cross, claims int
	_ = a.DB.Pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE source_count>=2), count(*) FILTER (WHERE cardinality(languages)>=2) FROM events WHERE created_at > $1`, start).Scan(&events, &multi, &cross)
	_ = a.DB.Pool.QueryRow(ctx, `SELECT count(DISTINCT o.claim_id) FROM claim_occurrences o JOIN articles a ON a.id=o.article_id WHERE a.first_seen_at > $1`, start).Scan(&claims)
	fmt.Fprintf(&b, "\nEvents: %d · with ≥2 sources: %d · cross-language (ro+ru): %d · claims extracted: %d\n", events, multi, cross, claims)
	vrows, err := a.DB.Pool.Query(ctx, `SELECT v.verdict, v.review_status, count(*) FROM claims c JOIN claim_verdicts v ON v.id=c.current_verdict_id
		WHERE c.first_seen_at > $1 GROUP BY 1,2 ORDER BY 3 DESC`, start)
	if err == nil {
		b.WriteString("\n## Fact-check outcomes\n\n| Verdict | Review status | Claims |\n|---|---|---|\n")
		for vrows.Next() {
			var v, r string
			var n int
			_ = vrows.Scan(&v, &r, &n)
			fmt.Fprintf(&b, "| %s | %s | %d |\n", v, r, n)
		}
		vrows.Close()
	}
	var ai string
	_ = a.DB.Pool.QueryRow(ctx, `SELECT coalesce(string_agg(task||' '||provider_slug||'/'||model_slug||': '||ok||' ok, '||bad||' failed', '; '),'') FROM (
		SELECT task, provider_slug, model_slug, count(*) FILTER (WHERE status='succeeded') ok, count(*) FILTER (WHERE status<>'succeeded') bad
		FROM ai_executions WHERE created_at > $1 GROUP BY 1,2,3 ORDER BY 1) x`, start).Scan(&ai)
	fmt.Fprintf(&b, "\n## AI executions\n\n%s\n", ai)
	return b.String(), nil
}

var _ = db.IsNoRows
