package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
)

// Manifest is a source list file (config/sources/*.json, fixtures/sources.json): publishers with
// their content-rights permission and one endpoint each.
type Manifest struct {
	Description string           `json:"description"`
	Sources     []ManifestSource `json:"sources"`
}

type ManifestSource struct {
	Publisher  PublisherInput  `json:"publisher"`
	Permission PermissionInput `json:"permission"`
	Endpoint   EndpointInput   `json:"endpoint"`
}

// LoadManifest reads one manifest file.
func LoadManifest(path string) (*Manifest, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &m, nil
}

// Preset is a manifest offered to administrators for import, with the import state of each source.
type Preset struct {
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Sources     []PresetItem `json:"sources"`
}

type PresetItem struct {
	Publisher    string     `json:"publisher"`
	Domain       string     `json:"domain"`
	Name         string     `json:"name"`
	EndpointType string     `json:"endpoint_type"`
	EntryURL     string     `json:"entry_url"`
	Languages    []string   `json:"languages"`
	EndpointID   *uuid.UUID `json:"endpoint_id,omitempty"` // set when already imported
	Status       *string    `json:"status,omitempty"`
}

// Presets lists the manifests in dir (one preset per *.json file, named after the file).
func (s *Service) Presets(ctx context.Context, dir string) ([]Preset, error) {
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	sort.Strings(files)
	out := []Preset{}
	for _, f := range files {
		m, err := LoadManifest(f)
		if err != nil {
			return nil, err
		}
		p := Preset{Name: strings.TrimSuffix(filepath.Base(f), ".json"), Description: m.Description, Sources: []PresetItem{}}
		for _, src := range m.Sources {
			it := PresetItem{Publisher: deref(src.Publisher.Name), Domain: deref(src.Publisher.CanonicalDomain), Name: deref(src.Endpoint.Name),
				EndpointType: deref(src.Endpoint.EndpointType), EntryURL: deref(src.Endpoint.EntryURL), Languages: src.Endpoint.Languages}
			var id uuid.UUID
			var status string
			if err := s.db.Pool.QueryRow(ctx, `SELECT id, status FROM source_endpoints WHERE entry_url=$1`, it.EntryURL).Scan(&id, &status); err == nil {
				it.EndpointID, it.Status = &id, &status
			} else if !db.IsNoRows(err) {
				return nil, err
			}
			p.Sources = append(p.Sources, it)
		}
		out = append(out, p)
	}
	return out, nil
}

// LoadPreset returns the manifest for a preset name (a file in dir; names cannot escape it).
func LoadPreset(dir, name string) (*Manifest, error) {
	if name == "" || strings.ContainsAny(name, `/\.`) {
		return nil, apperr.NotFound("preset_not_found", "Source preset not found.")
	}
	m, err := LoadManifest(filepath.Join(dir, name+".json"))
	if os.IsNotExist(err) {
		return nil, apperr.NotFound("preset_not_found", "Source preset not found.")
	}
	return m, err
}

// ImportOptions control how manifest sources are registered.
type ImportOptions struct {
	// LegalBasis, when set, replaces the manifest's permission with a granted one that records it
	// (crawl, store text, summarize, embed, snippets; no raw HTML retention).
	LegalBasis string
	// Enable switches the imported endpoints to enabled (they are then polled when live crawling is on).
	Enable bool
	// Only limits the import to endpoints whose name contains one of these (case-insensitive).
	Only []string
	// MaxItemsPerPoll overrides the manifest value when > 0.
	MaxItemsPerPoll int
}

// ImportResult describes one imported endpoint.
type ImportResult struct {
	Name       string    `json:"name"`
	EntryURL   string    `json:"entry_url"`
	EndpointID uuid.UUID `json:"endpoint_id"`
	Created    bool      `json:"created"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
}

// Import creates or reuses publishers (by domain) and endpoints (by entry URL), applies the
// permission and optionally enables the endpoints. It is idempotent; errors are reported per
// source and do not stop the others.
func (s *Service) Import(ctx context.Context, list []ManifestSource, o ImportOptions) []ImportResult {
	out := []ImportResult{}
	for _, src := range list {
		if !matchesAny(deref(src.Endpoint.Name), o.Only) {
			continue
		}
		res := ImportResult{Name: deref(src.Endpoint.Name), EntryURL: deref(src.Endpoint.EntryURL)}
		if err := s.importOne(ctx, src, o, &res); err != nil {
			res.Error = err.Error()
		}
		out = append(out, res)
	}
	return out
}

func (s *Service) importOne(ctx context.Context, src ManifestSource, o ImportOptions, res *ImportResult) error {
	if src.Publisher.CanonicalDomain == nil || src.Endpoint.EntryURL == nil {
		return fmt.Errorf("publisher.canonical_domain and endpoint.entry_url are required")
	}
	var pubID uuid.UUID
	err := s.db.Pool.QueryRow(ctx, `SELECT id FROM publishers WHERE canonical_domain=$1`, *src.Publisher.CanonicalDomain).Scan(&pubID)
	if db.IsNoRows(err) {
		p, err := s.CreatePublisher(ctx, src.Publisher)
		if err != nil {
			return fmt.Errorf("publisher: %w", err)
		}
		pubID = p.ID
	} else if err != nil {
		return err
	}
	perm := src.Permission
	if strings.TrimSpace(o.LegalBasis) != "" {
		perm = PermissionInput{LegalBasis: o.LegalBasis, PermissionStatus: "granted", AllowCrawl: true, AllowStoreFullText: true,
			AllowSummarize: true, AllowEmbed: true, AllowSnippets: true, RobotsReviewStatus: "unreviewed", TermsReviewStatus: "unreviewed",
			AttributionText: "Sursa: " + deref(src.Publisher.Name)}
	}
	if _, err := s.UpsertPermission(ctx, pubID, perm); err != nil {
		return fmt.Errorf("permission: %w", err)
	}
	active := "active"
	if _, err := s.UpdatePublisher(ctx, pubID, PublisherInput{Status: &active}); err != nil {
		return err
	}
	if o.MaxItemsPerPoll > 0 {
		n := o.MaxItemsPerPoll
		src.Endpoint.MaxItemsPerPoll = &n
	}
	err = s.db.Pool.QueryRow(ctx, `SELECT id, status FROM source_endpoints WHERE entry_url=$1`, *src.Endpoint.EntryURL).Scan(&res.EndpointID, &res.Status)
	if db.IsNoRows(err) {
		src.Endpoint.PublisherID = &pubID
		e, err := s.CreateEndpoint(ctx, src.Endpoint)
		if err != nil {
			return fmt.Errorf("endpoint: %w", err)
		}
		res.EndpointID, res.Status, res.Created = e.ID, e.Status, true
	} else if err != nil {
		return err
	}
	if o.Enable && res.Status != "enabled" {
		e, err := s.SetEndpointStatus(ctx, res.EndpointID, "enabled")
		if err != nil {
			return fmt.Errorf("enable: %w", err)
		}
		res.Status = e.Status
	}
	return nil
}

func matchesAny(name string, only []string) bool {
	if len(only) == 0 {
		return true
	}
	for _, o := range only {
		if o != "" && strings.Contains(strings.ToLower(name), strings.ToLower(o)) {
			return true
		}
	}
	return false
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
