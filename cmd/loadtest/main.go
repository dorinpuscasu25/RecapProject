// Command loadtest drives a running API with a weighted mix of read requests and reports
// throughput, latency percentiles and status codes per endpoint. It only reads; it never
// creates data. Rate-limited responses (429) are counted separately from errors.
//
//	go run ./cmd/loadtest -base http://localhost:8080 -c 32 -d 60s
//
// Credentials come from LOADTEST_EMAIL/LOADTEST_PASSWORD, falling back to ADMIN_EMAIL/ADMIN_PASSWORD.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type target struct {
	name   string
	weight int
	path   func() string
}

type stats struct {
	mu       sync.Mutex
	lat      map[string][]time.Duration
	codes    map[string]map[int]int
	failures map[string]int
}

func (s *stats) add(name string, d time.Duration, code int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.failures[name]++
		return
	}
	s.lat[name] = append(s.lat[name], d)
	if s.codes[name] == nil {
		s.codes[name] = map[int]int{}
	}
	s.codes[name][code]++
}

func main() {
	base := flag.String("base", envOr("BASE", "http://localhost:8080"), "API base URL")
	conc := flag.Int("c", 16, "concurrent workers")
	dur := flag.Duration("d", 30*time.Second, "test duration")
	out := flag.String("out", "", "also write the markdown report to this file")
	maxP95 := flag.Duration("max-p95", 0, "fail (exit 1) when the overall p95 exceeds this")
	maxErr := flag.Float64("max-error-rate", 0.01, "fail when the share of 5xx/transport errors exceeds this")
	flag.Parse()

	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: *conc * 2}}
	token, err := login(client, *base)
	if err != nil {
		fmt.Fprintln(os.Stderr, "login:", err)
		os.Exit(2)
	}
	events := discoverEvents(client, *base, token)
	queries := []string{"energie", "tarif", "guvern", "энергия", "тариф", "parlament", "startup", "buget"}
	pick := func(xs []string) string { return xs[rand.IntN(len(xs))] }
	targets := []target{
		{"GET /v1/feed", 30, func() string { return "/v1/feed?limit=20" }},
		{"GET /v1/search", 25, func() string { return "/v1/search?q=" + url.QueryEscape(pick(queries)) }},
		{"GET /v1/categories/tree", 10, func() string { return "/v1/categories/tree?lang=ro" }},
		{"GET /v1/fact-checks", 10, func() string { return "/v1/fact-checks?limit=10" }},
	}
	if len(events) > 0 {
		targets = append(targets,
			target{"GET /v1/events/{id}", 20, func() string { return "/v1/events/" + pick(events) + "?lang=ro" }},
			target{"GET /v1/events/{id}/timeline", 5, func() string { return "/v1/events/" + pick(events) + "/timeline" }})
	}
	total := 0
	for _, t := range targets {
		total += t.weight
	}
	choose := func() target {
		n := rand.IntN(total)
		for _, t := range targets {
			if n < t.weight {
				return t
			}
			n -= t.weight
		}
		return targets[0]
	}

	st := &stats{lat: map[string][]time.Duration{}, codes: map[string]map[int]int{}, failures: map[string]int{}}
	ctx, cancel := context.WithTimeout(context.Background(), *dur)
	defer cancel()
	start := time.Now()
	var wg sync.WaitGroup
	for range *conc {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				t := choose()
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, *base+t.path(), nil)
				req.Header.Set("Authorization", "Bearer "+token)
				t0 := time.Now()
				resp, err := client.Do(req)
				if err != nil {
					if ctx.Err() == nil {
						st.add(t.name, 0, 0, err)
					}
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				st.add(t.name, time.Since(t0), resp.StatusCode, nil)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	report, p95, errRate := render(st, *base, *conc, elapsed, len(events))
	fmt.Print(report)
	if *out != "" {
		if err := os.WriteFile(*out, []byte(report), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}
	if (*maxP95 > 0 && p95 > *maxP95) || errRate > *maxErr {
		fmt.Fprintf(os.Stderr, "thresholds failed: p95=%s (max %s) error_rate=%.4f (max %.4f)\n", p95, *maxP95, errRate, *maxErr)
		os.Exit(1)
	}
}

func render(st *stats, base string, conc int, elapsed time.Duration, events int) (string, time.Duration, float64) {
	var b strings.Builder
	var all []time.Duration
	var n, errs, limited int
	names := make([]string, 0, len(st.lat))
	for k := range st.lat {
		names = append(names, k)
	}
	for k := range st.failures {
		if _, ok := st.lat[k]; !ok {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	fmt.Fprintf(&b, "# Load test\n\n%s, %d workers, %s, %d events sampled, %s\n\n", base, conc, elapsed.Round(time.Second), events, time.Now().UTC().Format(time.RFC3339))
	b.WriteString("| Endpoint | Requests | RPS | p50 | p95 | p99 | Max | 2xx | 429 | 5xx | Transport errors |\n|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, name := range names {
		l := st.lat[name]
		sort.Slice(l, func(i, j int) bool { return l[i] < l[j] })
		all = append(all, l...)
		c := st.codes[name]
		ok, s5, s429 := 0, 0, c[429]
		for code, k := range c {
			switch {
			case code < 300:
				ok += k
			case code >= 500:
				s5 += k
			}
		}
		n += len(l) + st.failures[name]
		errs += s5 + st.failures[name]
		limited += s429
		fmt.Fprintf(&b, "| %s | %d | %.1f | %s | %s | %s | %s | %d | %d | %d | %d |\n", name, len(l), float64(len(l))/elapsed.Seconds(),
			pct(l, .50), pct(l, .95), pct(l, .99), pct(l, 1), ok, s429, s5, st.failures[name])
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	rate := 0.0
	if n > 0 {
		rate = float64(errs) / float64(n)
	}
	fmt.Fprintf(&b, "\n**Total:** %d requests, %.1f req/s, p50 %s, p95 %s, p99 %s, error rate %.4f, rate-limited %d\n",
		n, float64(len(all))/elapsed.Seconds(), pct(all, .50), pct(all, .95), pct(all, .99), rate, limited)
	return b.String(), pct(all, .95), rate
}

func pct(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * p)
	return sorted[i].Round(100 * time.Microsecond)
}

func login(c *http.Client, base string) (string, error) {
	email := envOr("LOADTEST_EMAIL", os.Getenv("ADMIN_EMAIL"))
	pw := envOr("LOADTEST_PASSWORD", os.Getenv("ADMIN_PASSWORD"))
	if email == "" || pw == "" {
		return "", fmt.Errorf("set LOADTEST_EMAIL/LOADTEST_PASSWORD or ADMIN_EMAIL/ADMIN_PASSWORD")
	}
	body, _ := json.Marshal(map[string]string{"email": email, "password": pw})
	resp, err := c.Post(base+"/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.AccessToken == "" {
		return "", fmt.Errorf("login failed with status %d", resp.StatusCode)
	}
	return out.AccessToken, nil
}

func discoverEvents(c *http.Client, base, token string) []string {
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/feed?limit=50", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var out struct {
		Items []struct {
			Event struct {
				ID string `json:"id"`
			} `json:"event"`
		} `json:"items"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	var ids []string
	for _, it := range out.Items {
		if it.Event.ID != "" {
			ids = append(ids, it.Event.ID)
		}
	}
	return ids
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
