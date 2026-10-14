// Command newsctl is the operator CLI: seeding, admin bootstrap, fixture ingestion, index
// rebuilds, dead-letter replay and evaluation runs. It uses the same services as the API.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/dorinpuscasu25/recapproject/internal/app"
	"github.com/dorinpuscasu25/recapproject/internal/eval"
	"github.com/dorinpuscasu25/recapproject/internal/jobs"
	"github.com/dorinpuscasu25/recapproject/internal/platform/bus"
	"github.com/dorinpuscasu25/recapproject/internal/platform/config"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/logx"
	"github.com/dorinpuscasu25/recapproject/internal/platform/principal"
	"github.com/dorinpuscasu25/recapproject/internal/search"
	"github.com/dorinpuscasu25/recapproject/internal/seed"
	"github.com/dorinpuscasu25/recapproject/internal/sources"
)

const usage = `newsctl — operator CLI

  newsctl seed [-categories=true] [-entities=true]     idempotent reference data (policies, AI registry, taxonomy, gazetteer)
  newsctl bootstrap-admin -email ADDRESS                 grant super_admin to an existing verified account
  newsctl ingest-fixtures [-manifest fixtures/sources.json] [-drain=true]
                                                         register the synthetic fixture sources and crawl them
  newsctl drain                                          process all pending messages, jobs and mail once (postgres bus)
  newsctl reindex [-types event,article,claim]           rebuild OpenSearch/Qdrant documents from PostgreSQL
  newsctl replay-dead-letter (-id UUID | -all)           requeue dead-lettered messages
  newsctl eval [-suite all|classification|claims|answers|retrieval] [-dir fixtures/eval]
                                                         run evaluation suites; exit status 3 when a quality gate fails

Configuration comes from the environment (see .env.example).
`

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		fmt.Print(usage)
		return
	}
	cmd, args := os.Args[1], os.Args[2:]
	log := logx.New("newsctl", os.Stderr)
	if os.Getenv("NEWSCTL_QUIET") != "" {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx = principal.With(ctx, principal.System())
	code, err := run(ctx, cmd, args, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func open(ctx context.Context, log *slog.Logger) (*app.App, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if err := db.Migrate(ctx, cfg.DatabaseURL); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return app.New(ctx, cfg, log, app.Options{})
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func run(ctx context.Context, cmd string, args []string, log *slog.Logger) (int, error) {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	switch cmd {
	case "seed":
		cats := fs.Bool("categories", true, "seed the starter taxonomy (skipped for categories that already exist)")
		ents := fs.Bool("entities", true, "seed the entity gazetteer")
		if err := fs.Parse(args); err != nil {
			return 2, err
		}
		a, err := open(ctx, log)
		if err != nil {
			return 1, err
		}
		defer a.Close()
		if err := seed.Run(ctx, a.DB, seed.Options{Categories: *cats, Entities: *ents, MLServiceURL: os.Getenv("ML_SERVICE_URL")}); err != nil {
			return 1, err
		}
		fmt.Println("seed complete")
		return 0, nil

	case "bootstrap-admin":
		email := fs.String("email", "", "email of an existing, verified account")
		if err := fs.Parse(args); err != nil {
			return 2, err
		}
		if *email == "" {
			return 2, errors.New("-email is required")
		}
		a, err := open(ctx, log)
		if err != nil {
			return 1, err
		}
		defer a.Close()
		u, err := a.Identity.BootstrapSuperAdmin(ctx, *email)
		if err != nil {
			return 1, err
		}
		fmt.Printf("granted super_admin to %s (%s)\n", u.Email, u.ID)
		return 0, nil

	case "ingest-fixtures":
		manifest := fs.String("manifest", "fixtures/sources.json", "fixture source manifest")
		drain := fs.Bool("drain", true, "process the pipeline synchronously after requesting crawls (postgres bus)")
		if err := fs.Parse(args); err != nil {
			return 2, err
		}
		a, err := open(ctx, log)
		if err != nil {
			return 1, err
		}
		defer a.Close()
		if a.Cfg.IsProduction() {
			return 2, errors.New("ingest-fixtures is a development command and is refused in production")
		}
		ids, err := registerFixtures(ctx, a, *manifest)
		if err != nil {
			return 1, err
		}
		for _, id := range ids {
			if _, err := a.Ingest.RequestCrawl(ctx, id, nil, 50); err != nil {
				return 1, err
			}
		}
		fmt.Printf("requested crawls for %d fixture sources\n", len(ids))
		if *drain && a.Cfg.BusDriver != "kafka" {
			if err := a.Drain(ctx); err != nil {
				return 1, err
			}
			var arts, evs int
			_ = a.DB.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM articles), (SELECT count(*) FROM events)`).Scan(&arts, &evs)
			fmt.Printf("pipeline drained: %d articles, %d events\n", arts, evs)
		}
		return 0, nil

	case "drain":
		a, err := open(ctx, log)
		if err != nil {
			return 1, err
		}
		defer a.Close()
		if a.Cfg.BusDriver == "kafka" {
			return 2, errors.New("drain processes the postgres bus; with BUS_DRIVER=kafka run the worker instead")
		}
		return 0, a.Drain(ctx)

	case "reindex":
		types := fs.String("types", "", "comma-separated doc types (default: all)")
		if err := fs.Parse(args); err != nil {
			return 2, err
		}
		a, err := open(ctx, log)
		if err != nil {
			return 1, err
		}
		defer a.Close()
		payload := map[string]any{}
		if *types != "" {
			payload["doc_types"] = splitComma(*types)
		}
		raw, _ := json.Marshal(payload)
		h := a.JobHandlers()[search.JobReindex]
		start := time.Now()
		res, err := h(ctx, &jobs.Job{ID: uuid.New(), Kind: search.JobReindex, Payload: raw})
		if err != nil {
			return 1, err
		}
		printJSON(map[string]any{"reindexed": res, "duration": time.Since(start).String()})
		return 0, nil

	case "replay-dead-letter":
		id := fs.String("id", "", "dead letter id")
		all := fs.Bool("all", false, "replay every unreplayed dead letter")
		if err := fs.Parse(args); err != nil {
			return 2, err
		}
		a, err := open(ctx, log)
		if err != nil {
			return 1, err
		}
		defer a.Close()
		var targets []uuid.UUID
		switch {
		case *id != "":
			u, err := uuid.Parse(*id)
			if err != nil {
				return 2, err
			}
			targets = append(targets, u)
		case *all:
			rows, err := a.DB.Pool.Query(ctx, `SELECT id FROM dead_letters WHERE replayed_at IS NULL ORDER BY created_at`)
			if err != nil {
				return 1, err
			}
			for rows.Next() {
				var u uuid.UUID
				_ = rows.Scan(&u)
				targets = append(targets, u)
			}
			rows.Close()
		default:
			return 2, errors.New("-id or -all is required")
		}
		for _, t := range targets {
			if err := bus.ReplayDeadLetter(ctx, a.DB, t); err != nil {
				return 1, fmt.Errorf("replay %s: %w", t, err)
			}
		}
		fmt.Printf("requeued %d dead letters\n", len(targets))
		return 0, nil

	case "eval":
		suite := fs.String("suite", "all", "suite name or all")
		dir := fs.String("dir", "fixtures/eval", "dataset directory")
		if err := fs.Parse(args); err != nil {
			return 2, err
		}
		a, err := open(ctx, log)
		if err != nil {
			return 1, err
		}
		defer a.Close()
		r := &eval.Runner{DB: a.DB, GW: a.AIGateway(), Tax: a.Taxonomy, Dir: *dir}
		names := []string{*suite}
		if *suite == "all" {
			names = []string{"classification", "claims", "answers", "retrieval"}
		}
		failed := false
		var reports []*eval.Report
		for _, n := range names {
			rep, err := r.Run(ctx, n, nil)
			if err != nil {
				return 1, fmt.Errorf("suite %s: %w", n, err)
			}
			reports = append(reports, rep)
			if !rep.OK() {
				failed = true
			}
		}
		printJSON(reports)
		if failed {
			return 3, errors.New("one or more quality gates failed")
		}
		return 0, nil
	}
	fmt.Print(usage)
	return 2, fmt.Errorf("unknown command %q", cmd)
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}

type fixtureManifest struct {
	Sources []struct {
		Publisher  sources.PublisherInput  `json:"publisher"`
		Permission sources.PermissionInput `json:"permission"`
		Endpoint   sources.EndpointInput   `json:"endpoint"`
	} `json:"sources"`
}

// registerFixtures creates (or reuses) publishers and endpoints from the manifest. Idempotent
// by canonical domain and entry URL.
func registerFixtures(ctx context.Context, a *app.App, path string) ([]uuid.UUID, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	var m fixtureManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	var out []uuid.UUID
	for _, s := range m.Sources {
		var pubID uuid.UUID
		err := a.DB.Pool.QueryRow(ctx, `SELECT id FROM publishers WHERE canonical_domain=$1`, *s.Publisher.CanonicalDomain).Scan(&pubID)
		if db.IsNoRows(err) {
			p, err := a.Sources.CreatePublisher(ctx, s.Publisher)
			if err != nil {
				return nil, fmt.Errorf("publisher %s: %w", *s.Publisher.Name, err)
			}
			pubID = p.ID
		} else if err != nil {
			return nil, err
		}
		if _, err := a.Sources.UpsertPermission(ctx, pubID, s.Permission); err != nil {
			return nil, fmt.Errorf("permission %s: %w", *s.Publisher.Name, err)
		}
		active := "active"
		if _, err := a.Sources.UpdatePublisher(ctx, pubID, sources.PublisherInput{Status: &active}); err != nil {
			return nil, err
		}
		var epID uuid.UUID
		err = a.DB.Pool.QueryRow(ctx, `SELECT id FROM source_endpoints WHERE entry_url=$1`, *s.Endpoint.EntryURL).Scan(&epID)
		if db.IsNoRows(err) {
			s.Endpoint.PublisherID = &pubID
			e, err := a.Sources.CreateEndpoint(ctx, s.Endpoint)
			if err != nil {
				return nil, fmt.Errorf("endpoint %s: %w", *s.Endpoint.Name, err)
			}
			epID = e.ID
		} else if err != nil {
			return nil, err
		}
		if _, err := a.Sources.SetEndpointStatus(ctx, epID, "enabled"); err != nil {
			return nil, fmt.Errorf("enable %s: %w", *s.Endpoint.Name, err)
		}
		out = append(out, epID)
	}
	return out, nil
}
