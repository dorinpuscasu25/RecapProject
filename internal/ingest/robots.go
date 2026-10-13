package ingest

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Robots is a minimal robots.txt evaluator (RFC 9309): user-agent groups, Allow/Disallow with
// longest-match precedence, '*' and '$' wildcards, and Crawl-delay.
type Robots struct {
	groups []robotsGroup
}

type robotsGroup struct {
	agents     []string
	rules      []robotsRule
	crawlDelay time.Duration
}

type robotsRule struct {
	allow   bool
	pattern string
}

func ParseRobots(b []byte) *Robots {
	r := &Robots{}
	var cur *robotsGroup
	lastWasAgent := false
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), 512*1024)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		switch k {
		case "user-agent":
			if !lastWasAgent || cur == nil {
				r.groups = append(r.groups, robotsGroup{})
				cur = &r.groups[len(r.groups)-1]
			}
			cur.agents = append(cur.agents, strings.ToLower(v))
			lastWasAgent = true
		case "allow", "disallow":
			lastWasAgent = false
			if cur == nil || (k == "disallow" && v == "") {
				continue
			}
			cur.rules = append(cur.rules, robotsRule{allow: k == "allow", pattern: v})
		case "crawl-delay":
			lastWasAgent = false
			if cur != nil {
				if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 && f < 3600 {
					cur.crawlDelay = time.Duration(f * float64(time.Second))
				}
			}
		default:
			lastWasAgent = false
		}
	}
	return r
}

func (r *Robots) group(ua string) *robotsGroup {
	ua = strings.ToLower(ua)
	var star *robotsGroup
	best, bestLen := (*robotsGroup)(nil), 0
	for i := range r.groups {
		g := &r.groups[i]
		for _, a := range g.agents {
			if a == "*" {
				star = g
				continue
			}
			if strings.Contains(ua, a) && len(a) > bestLen {
				best, bestLen = g, len(a)
			}
		}
	}
	if best != nil {
		return best
	}
	return star
}

// Allowed reports whether path (incl. query) may be fetched by ua.
func (r *Robots) Allowed(ua, path string) bool {
	if r == nil {
		return true
	}
	g := r.group(ua)
	if g == nil {
		return true
	}
	matchLen, allow := -1, true
	for _, rule := range g.rules {
		if robotsMatch(rule.pattern, path) {
			l := len(rule.pattern)
			if l > matchLen || (l == matchLen && rule.allow) {
				matchLen, allow = l, rule.allow
			}
		}
	}
	return allow
}

func (r *Robots) CrawlDelay(ua string) time.Duration {
	if r == nil {
		return 0
	}
	if g := r.group(ua); g != nil {
		return g.crawlDelay
	}
	return 0
}

func robotsMatch(pattern, path string) bool {
	anchored := strings.HasSuffix(pattern, "$")
	pattern = strings.TrimSuffix(pattern, "$")
	parts := strings.Split(pattern, "*")
	pos := 0
	for i, p := range parts {
		if i == 0 {
			if !strings.HasPrefix(path, p) {
				return false
			}
			pos = len(p)
			continue
		}
		j := strings.Index(path[pos:], p)
		if j < 0 {
			return false
		}
		pos += j + len(p)
	}
	if anchored {
		return pos == len(path) || (len(parts) > 1 && parts[len(parts)-1] == "")
	}
	return true
}

// robotsCache caches parsed robots.txt per scheme+host.
type robotsCache struct {
	mu sync.Mutex
	m  map[string]robotsEntry
}

type robotsEntry struct {
	r  *Robots
	at time.Time
}

func (c *robotsCache) get(key string) (*Robots, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok || time.Since(e.at) > time.Hour {
		return nil, false
	}
	return e.r, true
}

func (c *robotsCache) put(key string, r *Robots) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]robotsEntry{}
	}
	c.m[key] = robotsEntry{r: r, at: time.Now()}
}
