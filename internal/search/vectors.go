// Package search owns the derived retrieval projections: OpenSearch (lexical/BM25, facets) and
// Qdrant (dense vectors). PostgreSQL remains the system of record; both projections are
// rebuildable. Index writes carry the entity version so stale writes never overwrite newer data.
package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// VectorPoint is a stored embedding with filterable payload.
type VectorPoint struct {
	ID      uuid.UUID
	Vector  []float32
	Payload map[string]any
}

type VectorHit struct {
	ID      uuid.UUID
	Score   float64
	Payload map[string]any
}

// VectorFilter supports the payload filters we need (exact match, any-of, time range).
type VectorFilter struct {
	Must     map[string]any      // field == value
	MustAny  map[string][]string // field in values
	MustNot  map[string]any
	RangeGTE map[string]float64 // numeric (e.g. unix seconds)
	RangeLTE map[string]float64
}

// VectorStore abstracts the dense index. Implementations: Qdrant (production) and Memory
// (explicitly selected for tests / single-process dev; not persistent).
type VectorStore interface {
	EnsureCollection(ctx context.Context, name string, dims int) error
	Upsert(ctx context.Context, collection string, pts []VectorPoint) error
	Delete(ctx context.Context, collection string, ids []uuid.UUID) error
	Search(ctx context.Context, collection string, vec []float32, f VectorFilter, limit int) ([]VectorHit, error)
	SetAlias(ctx context.Context, alias, collection string) error
	Count(ctx context.Context, collection string) (int, error)
	Driver() string
}

// ---------- Qdrant (REST) ----------

type Qdrant struct {
	URL    string
	APIKey string
	Client *http.Client
}

func NewQdrant(url, key string) *Qdrant {
	return &Qdrant{URL: strings.TrimSuffix(url, "/"), APIKey: key, Client: &http.Client{Timeout: 20 * time.Second}}
}

func (q *Qdrant) Driver() string { return "qdrant" }

func (q *Qdrant) call(ctx context.Context, method, path string, body any, out any) (int, error) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, q.URL+path, rdr)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if q.APIKey != "" {
		req.Header.Set("api-key", q.APIKey)
	}
	resp, err := q.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("qdrant %s %s: %d %s", method, path, resp.StatusCode, truncate(string(raw), 300))
	}
	if out != nil {
		return resp.StatusCode, json.Unmarshal(raw, out)
	}
	return resp.StatusCode, nil
}

func (q *Qdrant) EnsureCollection(ctx context.Context, name string, dims int) error {
	var info struct {
		Result struct {
			Config struct {
				Params struct {
					Vectors struct {
						Size int `json:"size"`
					} `json:"vectors"`
				} `json:"params"`
			} `json:"config"`
		} `json:"result"`
	}
	status, err := q.call(ctx, http.MethodGet, "/collections/"+name, nil, &info)
	if err == nil {
		if info.Result.Config.Params.Vectors.Size != dims {
			return fmt.Errorf("collection %s exists with %d dims, expected %d", name, info.Result.Config.Params.Vectors.Size, dims)
		}
		return nil
	}
	if status != http.StatusNotFound {
		return err
	}
	if _, err := q.call(ctx, http.MethodPut, "/collections/"+name, map[string]any{
		"vectors": map[string]any{"size": dims, "distance": "Cosine"},
	}, nil); err != nil && !strings.Contains(err.Error(), "already exists") {
		return err
	}
	for _, f := range []struct{ name, schema string }{{"doc_type", "keyword"}, {"language", "keyword"}, {"category_ids", "keyword"},
		{"publisher_id", "keyword"}, {"published_ts", "integer"}, {"status", "keyword"}, {"entity_keys", "keyword"}} {
		_, _ = q.call(ctx, http.MethodPut, "/collections/"+name+"/index?wait=true", map[string]any{"field_name": f.name, "field_schema": f.schema}, nil)
	}
	return nil
}

func (q *Qdrant) Upsert(ctx context.Context, col string, pts []VectorPoint) error {
	if len(pts) == 0 {
		return nil
	}
	var points []map[string]any
	for _, p := range pts {
		points = append(points, map[string]any{"id": p.ID.String(), "vector": p.Vector, "payload": p.Payload})
	}
	_, err := q.call(ctx, http.MethodPut, "/collections/"+col+"/points?wait=true", map[string]any{"points": points}, nil)
	return err
}

func (q *Qdrant) Delete(ctx context.Context, col string, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	var s []string
	for _, id := range ids {
		s = append(s, id.String())
	}
	_, err := q.call(ctx, http.MethodPost, "/collections/"+col+"/points/delete?wait=true", map[string]any{"points": s}, nil)
	return err
}

func qdrantFilter(f VectorFilter) map[string]any {
	var must, mustNot []map[string]any
	for k, v := range f.Must {
		must = append(must, map[string]any{"key": k, "match": map[string]any{"value": v}})
	}
	for k, vs := range f.MustAny {
		if len(vs) > 0 {
			must = append(must, map[string]any{"key": k, "match": map[string]any{"any": vs}})
		}
	}
	for k, v := range f.MustNot {
		mustNot = append(mustNot, map[string]any{"key": k, "match": map[string]any{"value": v}})
	}
	for k, v := range f.RangeGTE {
		must = append(must, map[string]any{"key": k, "range": map[string]any{"gte": v}})
	}
	for k, v := range f.RangeLTE {
		must = append(must, map[string]any{"key": k, "range": map[string]any{"lte": v}})
	}
	out := map[string]any{}
	if len(must) > 0 {
		out["must"] = must
	}
	if len(mustNot) > 0 {
		out["must_not"] = mustNot
	}
	return out
}

func (q *Qdrant) Search(ctx context.Context, col string, vec []float32, f VectorFilter, limit int) ([]VectorHit, error) {
	body := map[string]any{"vector": vec, "limit": limit, "with_payload": true}
	if fl := qdrantFilter(f); len(fl) > 0 {
		body["filter"] = fl
	}
	var res struct {
		Result []struct {
			ID      string         `json:"id"`
			Score   float64        `json:"score"`
			Payload map[string]any `json:"payload"`
		} `json:"result"`
	}
	if _, err := q.call(ctx, http.MethodPost, "/collections/"+col+"/points/search", body, &res); err != nil {
		return nil, err
	}
	out := make([]VectorHit, 0, len(res.Result))
	for _, r := range res.Result {
		id, err := uuid.Parse(r.ID)
		if err != nil {
			continue
		}
		out = append(out, VectorHit{ID: id, Score: r.Score, Payload: r.Payload})
	}
	return out, nil
}

func (q *Qdrant) SetAlias(ctx context.Context, alias, col string) error {
	_, err := q.call(ctx, http.MethodPost, "/collections/aliases", map[string]any{"actions": []map[string]any{
		{"delete_alias": map[string]any{"alias_name": alias}},
		{"create_alias": map[string]any{"collection_name": col, "alias_name": alias}},
	}}, nil)
	if err != nil && strings.Contains(err.Error(), "not found") {
		_, err = q.call(ctx, http.MethodPost, "/collections/aliases", map[string]any{"actions": []map[string]any{
			{"create_alias": map[string]any{"collection_name": col, "alias_name": alias}}}}, nil)
	}
	return err
}

func (q *Qdrant) Count(ctx context.Context, col string) (int, error) {
	var res struct {
		Result struct {
			Count int `json:"count"`
		} `json:"result"`
	}
	_, err := q.call(ctx, http.MethodPost, "/collections/"+col+"/points/count", map[string]any{"exact": true}, &res)
	return res.Result.Count, err
}

// ---------- Memory (tests / single-process dev) ----------

type Memory struct {
	mu    sync.RWMutex
	cols  map[string]map[uuid.UUID]VectorPoint
	dims  map[string]int
	alias map[string]string
}

func NewMemory() *Memory {
	return &Memory{cols: map[string]map[uuid.UUID]VectorPoint{}, dims: map[string]int{}, alias: map[string]string{}}
}

func (m *Memory) Driver() string { return "memory" }

func (m *Memory) resolve(c string) string {
	if a, ok := m.alias[c]; ok {
		return a
	}
	return c
}

func (m *Memory) EnsureCollection(_ context.Context, name string, dims int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d, ok := m.dims[name]; ok && d != dims {
		return fmt.Errorf("collection %s exists with %d dims", name, d)
	}
	if _, ok := m.cols[name]; !ok {
		m.cols[name] = map[uuid.UUID]VectorPoint{}
		m.dims[name] = dims
	}
	return nil
}

func (m *Memory) Upsert(_ context.Context, col string, pts []VectorPoint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.cols[m.resolve(col)]
	if !ok {
		return fmt.Errorf("collection %s not found", col)
	}
	for _, p := range pts {
		if len(p.Vector) != m.dims[m.resolve(col)] {
			return fmt.Errorf("dimension mismatch")
		}
		c[p.ID] = p
	}
	return nil
}

func (m *Memory) Delete(_ context.Context, col string, ids []uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		delete(m.cols[m.resolve(col)], id)
	}
	return nil
}

func matchPayload(p map[string]any, f VectorFilter) bool {
	for k, v := range f.Must {
		if !payloadHas(p[k], fmt.Sprint(v)) {
			return false
		}
	}
	for k, vs := range f.MustAny {
		if len(vs) == 0 {
			continue
		}
		ok := false
		for _, v := range vs {
			if payloadHas(p[k], v) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	for k, v := range f.MustNot {
		if payloadHas(p[k], fmt.Sprint(v)) {
			return false
		}
	}
	for k, v := range f.RangeGTE {
		if n, ok := toF(p[k]); !ok || n < v {
			return false
		}
	}
	for k, v := range f.RangeLTE {
		if n, ok := toF(p[k]); !ok || n > v {
			return false
		}
	}
	return true
}

func payloadHas(v any, want string) bool {
	switch x := v.(type) {
	case []string:
		for _, s := range x {
			if s == want {
				return true
			}
		}
	case []any:
		for _, s := range x {
			if fmt.Sprint(s) == want {
				return true
			}
		}
	default:
		return fmt.Sprint(x) == want
	}
	return false
}

func toF(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	case int:
		return float64(x), true
	}
	return 0, false
}

func (m *Memory) Search(_ context.Context, col string, vec []float32, f VectorFilter, limit int) ([]VectorHit, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.cols[m.resolve(col)]
	if !ok {
		return nil, fmt.Errorf("collection %s not found", col)
	}
	var out []VectorHit
	for _, p := range c {
		if !matchPayload(p.Payload, f) {
			continue
		}
		out = append(out, VectorHit{ID: p.ID, Score: cos(vec, p.Vector), Payload: p.Payload})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *Memory) SetAlias(_ context.Context, alias, col string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.alias[alias] = col
	return nil
}

func (m *Memory) Count(_ context.Context, col string) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.cols[m.resolve(col)]), nil
}

func cos(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		if i >= len(b) {
			break
		}
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
