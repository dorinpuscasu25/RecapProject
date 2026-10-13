package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// LexicalSchemaVersion is bumped when index mappings change; a new physical index
// (<prefix>-<doc>-v<N>) is created and the alias is moved after backfill.
const LexicalSchemaVersion = 1

// LexicalDoc is the OpenSearch projection of an event, article or claim.
type LexicalDoc struct {
	ID          string         `json:"id"`
	DocType     string         `json:"doc_type"`
	Title       string         `json:"title"`
	Body        string         `json:"body"`
	Language    string         `json:"language"`
	PublishedAt *time.Time     `json:"published_at,omitempty"`
	CategoryIDs []string       `json:"category_ids"`
	PublisherID string         `json:"publisher_id,omitempty"`
	Regions     []string       `json:"regions"`
	EntityKeys  []string       `json:"entity_keys"`
	Status      string         `json:"status"`
	Verdict     string         `json:"verdict,omitempty"`
	EventID     string         `json:"event_id,omitempty"`
	Version     int            `json:"entity_version"`
	Extra       map[string]any `json:"extra,omitempty"`
}

type LexicalQuery struct {
	Text         string
	DocTypes     []string
	Languages    []string
	CategoryIDs  []string
	PublisherIDs []string
	Regions      []string
	EntityKeys   []string
	Statuses     []string
	Verdicts     []string
	From, To     *time.Time
	Limit        int
}

type LexicalHit struct {
	ID      string
	DocType string
	Score   float64
	Snippet string
}

// LexicalIndex abstracts the lexical engine (OpenSearch or the Postgres FTS fallback).
type LexicalIndex interface {
	Ensure(ctx context.Context) error
	Index(ctx context.Context, d LexicalDoc) error
	Delete(ctx context.Context, docType, id string) error
	Search(ctx context.Context, q LexicalQuery) ([]LexicalHit, error)
	Name() string
}

type OpenSearch struct {
	URL, User, Password, Prefix string
	Client                      *http.Client
}

func NewOpenSearch(url, user, pass, prefix string) *OpenSearch {
	return &OpenSearch{URL: strings.TrimSuffix(url, "/"), User: user, Password: pass, Prefix: prefix, Client: &http.Client{Timeout: 15 * time.Second}}
}

func (o *OpenSearch) Name() string { return "opensearch" }

func (o *OpenSearch) alias(docType string) string { return fmt.Sprintf("%s-%s", o.Prefix, docType) }
func (o *OpenSearch) physical(docType string) string {
	return fmt.Sprintf("%s-%s-v%d", o.Prefix, docType, LexicalSchemaVersion)
}

func (o *OpenSearch) do(ctx context.Context, method, path string, body any, out any) (int, error) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, o.URL+path, rdr)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.User != "" {
		req.SetBasicAuth(o.User, o.Password)
	}
	resp, err := o.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("opensearch %s %s: %d %s", method, path, resp.StatusCode, truncate(string(raw), 300))
	}
	if out != nil {
		return resp.StatusCode, json.Unmarshal(raw, out)
	}
	return resp.StatusCode, nil
}

// Mapping uses language-aware sub-fields: Romanian and Russian analyzers plus an ICU-free
// folding analyzer that strips diacritics (works with or without the analysis-icu plugin).
func lexicalMapping() map[string]any {
	text := map[string]any{"type": "text", "analyzer": "folded", "fields": map[string]any{
		"ro": map[string]any{"type": "text", "analyzer": "romanian"},
		"ru": map[string]any{"type": "text", "analyzer": "russian"},
	}}
	kw := map[string]any{"type": "keyword"}
	return map[string]any{
		"settings": map[string]any{"analysis": map[string]any{"analyzer": map[string]any{
			"folded": map[string]any{"type": "custom", "tokenizer": "standard", "filter": []string{"lowercase", "asciifolding"}}}}},
		"mappings": map[string]any{"dynamic": "strict", "properties": map[string]any{
			"id": kw, "doc_type": kw, "title": text, "body": text, "language": kw, "published_at": map[string]any{"type": "date"},
			"category_ids": kw, "publisher_id": kw, "regions": kw, "entity_keys": kw, "status": kw, "verdict": kw, "event_id": kw,
			"entity_version": map[string]any{"type": "integer"}, "extra": map[string]any{"type": "object", "enabled": false},
		}},
	}
}

func (o *OpenSearch) Ensure(ctx context.Context) error {
	for _, dt := range []string{"event", "article", "claim"} {
		idx := o.physical(dt)
		if st, err := o.do(ctx, http.MethodHead, "/"+idx, nil, nil); err != nil && st != http.StatusNotFound {
			return err
		} else if st == http.StatusNotFound {
			if _, err := o.do(ctx, http.MethodPut, "/"+idx, lexicalMapping(), nil); err != nil && !strings.Contains(err.Error(), "resource_already_exists") {
				return err
			}
		}
		if _, err := o.do(ctx, http.MethodPost, "/_aliases", map[string]any{"actions": []map[string]any{
			{"add": map[string]any{"index": idx, "alias": o.alias(dt), "is_write_index": true}}}}, nil); err != nil {
			return err
		}
	}
	return nil
}

// Index writes with external versioning: OpenSearch rejects (409) a write whose version is not
// greater than the stored one, so out-of-order or replayed messages cannot regress documents.
func (o *OpenSearch) Index(ctx context.Context, d LexicalDoc) error {
	st, err := o.do(ctx, http.MethodPut, fmt.Sprintf("/%s/_doc/%s?version=%d&version_type=external_gte&refresh=false", o.alias(d.DocType), d.ID, d.Version), d, nil)
	if st == http.StatusConflict {
		return nil // stale write ignored
	}
	return err
}

func (o *OpenSearch) Delete(ctx context.Context, docType, id string) error {
	st, err := o.do(ctx, http.MethodDelete, fmt.Sprintf("/%s/_doc/%s", o.alias(docType), id), nil, nil)
	if st == http.StatusNotFound {
		return nil
	}
	return err
}

func terms(field string, vals []string) map[string]any {
	return map[string]any{"terms": map[string]any{field: vals}}
}

// BuildQuery builds the OpenSearch query DSL (exported for contract tests).
func (o *OpenSearch) BuildQuery(q LexicalQuery) map[string]any {
	var filter []map[string]any
	add := func(field string, vals []string) {
		if len(vals) > 0 {
			filter = append(filter, terms(field, vals))
		}
	}
	add("doc_type", q.DocTypes)
	add("language", q.Languages)
	add("category_ids", q.CategoryIDs)
	add("publisher_id", q.PublisherIDs)
	add("regions", q.Regions)
	add("entity_keys", q.EntityKeys)
	add("status", q.Statuses)
	add("verdict", q.Verdicts)
	if q.From != nil || q.To != nil {
		r := map[string]any{}
		if q.From != nil {
			r["gte"] = q.From.Format(time.RFC3339)
		}
		if q.To != nil {
			r["lte"] = q.To.Format(time.RFC3339)
		}
		filter = append(filter, map[string]any{"range": map[string]any{"published_at": r}})
	}
	return map[string]any{
		"size": q.Limit,
		"query": map[string]any{"bool": map[string]any{
			"must": []map[string]any{{"multi_match": map[string]any{"query": q.Text, "type": "best_fields",
				"fields": []string{"title^3", "title.ro^2", "title.ru^2", "body", "body.ro", "body.ru"}}}},
			"filter": filter,
		}},
		"highlight": map[string]any{"fields": map[string]any{"body": map[string]any{"fragment_size": 200, "number_of_fragments": 1}}},
		"_source":   []string{"id", "doc_type"},
	}
}

func (o *OpenSearch) Search(ctx context.Context, q LexicalQuery) ([]LexicalHit, error) {
	idx := []string{}
	types := q.DocTypes
	if len(types) == 0 {
		types = []string{"event", "article", "claim"}
	}
	for _, t := range types {
		idx = append(idx, o.alias(t))
	}
	var res struct {
		Hits struct {
			Hits []struct {
				Score     float64             `json:"_score"`
				Source    map[string]any      `json:"_source"`
				Highlight map[string][]string `json:"highlight"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if _, err := o.do(ctx, http.MethodPost, "/"+strings.Join(idx, ",")+"/_search", o.BuildQuery(q), &res); err != nil {
		return nil, err
	}
	var out []LexicalHit
	for _, h := range res.Hits.Hits {
		hit := LexicalHit{ID: fmt.Sprint(h.Source["id"]), DocType: fmt.Sprint(h.Source["doc_type"]), Score: h.Score}
		if hl := h.Highlight["body"]; len(hl) > 0 {
			hit.Snippet = hl[0]
		}
		out = append(out, hit)
	}
	return out, nil
}
