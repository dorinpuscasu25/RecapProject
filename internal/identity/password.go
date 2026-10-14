package identity

import (
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"

	"github.com/dorinpuscasu25/recapproject/internal/platform/crypto"
)

// Argon2Params are encoded into every hash (PHC string) so parameters can be raised over time.
type Argon2Params struct {
	MemoryKiB   uint32
	Iterations  uint32
	Parallelism uint8
	SaltLen     int
	KeyLen      uint32
}

// HashPassword returns an Argon2id PHC string: $argon2id$v=19$m=..,t=..,p=..$salt$hash
func HashPassword(pw string, p Argon2Params) string {
	if p.SaltLen == 0 {
		p.SaltLen = 16
	}
	if p.KeyLen == 0 {
		p.KeyLen = 32
	}
	salt := crypto.RandomBytes(p.SaltLen)
	key := argon2.IDKey([]byte(pw), salt, p.Iterations, p.MemoryKiB, p.Parallelism, p.KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.MemoryKiB, p.Iterations, p.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

var errBadHash = errors.New("invalid password hash encoding")

// VerifyPassword checks pw against an encoded hash in constant time. needsRehash reports whether
// the stored parameters are weaker than the current ones.
func VerifyPassword(pw, encoded string, current Argon2Params) (ok, needsRehash bool, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, false, errBadHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, false, errBadHash
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, false, errBadHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, false, errBadHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, false, errBadHash
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want))) // #nosec G115 -- hash length is 32
	ok = subtle.ConstantTimeCompare(got, want) == 1
	needsRehash = m < current.MemoryKiB || t < current.Iterations || p < current.Parallelism
	return ok, needsRehash, nil
}

// commonPasswords is a small deny-list of the most common passwords; a fuller list can be
// loaded from configuration later.
var commonPasswords = map[string]bool{
	"password": true, "password1": true, "password123": true, "123456789": true, "1234567890": true,
	"qwertyuiop": true, "qwerty123": true, "1q2w3e4r5t": true, "iloveyou12": true, "parola1234": true,
	"parolaparola": true, "moldova123": true, "chisinau123": true, "admin12345": true, "welcome123": true,
	"0000000000": true, "1111111111": true, "abcdefghij": true, "letmein123": true, "passw0rd123": true,
}

const (
	minPasswordLen = 10
	maxPasswordLen = 128
)

// ValidatePassword enforces length and basic strength rules (NIST 800-63B style: length over
// composition rules, deny common passwords, reject passwords containing the email local part).
func ValidatePassword(pw, email string) (code, msg string, ok bool) {
	n := utf8.RuneCountInString(pw)
	if n < minPasswordLen {
		return "too_short", fmt.Sprintf("Password must have at least %d characters.", minPasswordLen), false
	}
	if n > maxPasswordLen {
		return "too_long", fmt.Sprintf("Password must have at most %d characters.", maxPasswordLen), false
	}
	lower := strings.ToLower(pw)
	if commonPasswords[lower] {
		return "too_common", "Password is too common.", false
	}
	distinct := map[rune]bool{}
	hasLetter, hasOther := false, false
	for _, r := range pw {
		distinct[r] = true
		if unicode.IsLetter(r) {
			hasLetter = true
		} else {
			hasOther = true
		}
	}
	if len(distinct) < 5 {
		return "too_simple", "Password must contain more distinct characters.", false
	}
	if !hasLetter || !hasOther {
		return "too_simple", "Password must mix letters with digits or symbols.", false
	}
	if local, _, found := strings.Cut(strings.ToLower(email), "@"); found && len(local) >= 4 && strings.Contains(lower, local) {
		return "contains_email", "Password must not contain your email address.", false
	}
	return "", "", true
}
