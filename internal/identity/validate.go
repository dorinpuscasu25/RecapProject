package identity

import (
	"encoding/base64"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nyaruka/phonenumbers"
	"golang.org/x/text/unicode/norm"
)

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// NormalizeEmail trims and lower-cases the address (conservative: no dot/plus stripping, since
// those rules are provider-specific). Returns ok=false for syntactically invalid addresses.
func NormalizeEmail(raw string) (display, normalized string, ok bool) {
	display = strings.TrimSpace(raw)
	if display == "" || len(display) > 254 || strings.ContainsAny(display, " \t\r\n<>") {
		return "", "", false
	}
	addr, err := mail.ParseAddress(display)
	if err != nil || addr.Address != display || addr.Name != "" {
		return "", "", false
	}
	local, domain, found := strings.Cut(display, "@")
	if !found || local == "" || len(local) > 64 || !strings.Contains(domain, ".") || strings.HasSuffix(domain, ".") {
		return "", "", false
	}
	return display, strings.ToLower(display), true
}

// nameAllowed: letters (any script), combining marks, spaces, apostrophes, hyphens, periods.
func validName(s string) (string, bool) {
	s = norm.NFC.String(strings.TrimSpace(s))
	n := utf8.RuneCountInString(s)
	if n < 1 || n > 100 {
		return "", false
	}
	letters := 0
	for _, r := range s {
		switch {
		case unicode.IsLetter(r):
			letters++
		case unicode.Is(unicode.Mn, r), r == ' ', r == '\'', r == '-', r == '.', r == '’':
		default:
			return "", false
		}
	}
	return s, letters > 0
}

// NormalizePhone parses an international number; defaultRegion is a hint only (e.g. "MD").
func NormalizePhone(raw, defaultRegion string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 32 {
		return "", false
	}
	num, err := phonenumbers.Parse(raw, defaultRegion)
	if err != nil || !phonenumbers.IsValidNumber(num) {
		return "", false
	}
	return phonenumbers.Format(num, phonenumbers.E164), true
}

var localeRe = regexp.MustCompile(`^(ro|ru|en)$`)

func validLocale(s string) bool { return localeRe.MatchString(s) }

func validTimezone(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	_, err := time.LoadLocation(s)
	return err == nil && s != "Local"
}

// deviceLabel sanitizes a client-provided device label.
func deviceLabel(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if utf8.RuneCountInString(s) > 100 {
		s = string([]rune(s)[:100])
	}
	return s
}
