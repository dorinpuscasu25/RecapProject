// Package ssrf provides an HTTP client that refuses to connect to internal destinations.
//
// Protections:
//   - only http/https schemes; only ports 80/443 (+ explicitly allowed ports)
//   - DNS is resolved by the dialer and *every* resolved IP is checked at connect time, which
//     defeats DNS rebinding between validation and connection
//   - loopback, RFC1918, CGNAT, link-local (incl. 169.254.169.254 metadata), multicast,
//     unspecified, IPv6 ULA/link-local, IPv4-mapped variants and documentation ranges are denied
//   - redirects are re-validated hop by hop and capped
//   - response bodies are size-limited by callers via LimitedBody
package ssrf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var ErrBlocked = errors.New("destination blocked by SSRF policy")

type Policy struct {
	AllowPrivate bool            // tests only
	AllowedPorts map[int]bool    // in addition to 80/443
	DenyHosts    map[string]bool // explicit deny list (lower-case hostnames)
	MaxRedirects int
}

var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
		"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "255.255.255.255/32",
		"::/128", "::1/128", "::ffff:0:0/96", "64:ff9b::/96", "100::/64", "2001:db8::/32", "fc00::/7", "fe80::/10", "ff00::/8",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// IsBlockedIP reports whether ip is a non-public destination.
func IsBlockedIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// ValidateURL performs static checks (scheme, port, host form, credentials, deny list).
func (p Policy) ValidateURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: unparsable url", ErrBlocked)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%w: scheme %q not allowed", ErrBlocked, u.Scheme)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%w: credentials in url not allowed", ErrBlocked)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return nil, fmt.Errorf("%w: missing host", ErrBlocked)
	}
	if p.DenyHosts[host] || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".internal") ||
		strings.HasSuffix(host, ".local") || host == "metadata.google.internal" {
		if !p.AllowPrivate {
			return nil, fmt.Errorf("%w: host %q not allowed", ErrBlocked, host)
		}
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n <= 0 || n > 65535 {
			return nil, fmt.Errorf("%w: invalid port", ErrBlocked)
		}
		if n != 80 && n != 443 && !p.AllowedPorts[n] && !p.AllowPrivate {
			return nil, fmt.Errorf("%w: port %d not allowed", ErrBlocked, n)
		}
	}
	// Literal IPs (incl. decimal/octal forms normalized by net/url? no — check explicitly).
	if addr, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		if IsBlockedIP(addr) && !p.AllowPrivate {
			return nil, fmt.Errorf("%w: ip %s not allowed", ErrBlocked, addr)
		}
	} else if looksNumericHost(host) && !p.AllowPrivate {
		return nil, fmt.Errorf("%w: ambiguous numeric host", ErrBlocked)
	}
	return u, nil
}

// looksNumericHost catches integer/hex/octal IPv4 encodings such as 2130706433 or 0x7f.1.
func looksNumericHost(h string) bool {
	h = strings.TrimSuffix(h, ".")
	for _, part := range strings.Split(h, ".") {
		if part == "" {
			return false
		}
		if strings.HasPrefix(part, "0x") {
			continue
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
}

// Client returns an http.Client enforcing the policy at dial time and on redirects.
func (p Policy) Client(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return fmt.Errorf("%w: unresolved address", ErrBlocked)
			}
			if IsBlockedIP(ip) && !p.AllowPrivate {
				return fmt.Errorf("%w: resolved ip %s not allowed", ErrBlocked, ip)
			}
			return nil
		},
	}
	tr := &http.Transport{
		Proxy:                 nil, // never route crawler traffic through ambient proxies
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          50,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
	}
	maxRedir := p.MaxRedirects
	if maxRedir == 0 {
		maxRedir = 5
	}
	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedir {
				return fmt.Errorf("%w: too many redirects", ErrBlocked)
			}
			if _, err := p.ValidateURL(req.URL.String()); err != nil {
				return err
			}
			return nil
		},
	}
}

// Get performs a validated GET.
func (p Policy) Get(ctx context.Context, c *http.Client, raw string, hdr http.Header) (*http.Response, error) {
	u, err := p.ValidateURL(raw)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	return c.Do(req)
}

// ReadLimited reads at most max bytes and errors if the body is larger.
func ReadLimited(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("response body exceeds %d bytes", max)
	}
	return b, nil
}
