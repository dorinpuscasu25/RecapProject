// Package crypto contains small, audited-by-construction helpers built on the Go standard library.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"math/big"
)

// RandomBytes returns n cryptographically secure random bytes.
func RandomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic("crypto/rand failure: " + err.Error())
	}
	return b
}

// RandomToken returns a URL-safe random token with n bytes of entropy.
func RandomToken(n int) string { return base64.RawURLEncoding.EncodeToString(RandomBytes(n)) }

// SixDigitCode returns a uniformly distributed 6-digit numeric code (000000-999999).
func SixDigitCode() string {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		panic("crypto/rand failure: " + err.Error())
	}
	s := n.String()
	for len(s) < 6 {
		s = "0" + s
	}
	return s
}

// HMAC returns HMAC-SHA256(key, parts joined by 0x1f).
func HMAC(key []byte, parts ...string) []byte {
	m := hmac.New(sha256.New, key)
	for i, p := range parts {
		if i > 0 {
			m.Write([]byte{0x1f})
		}
		m.Write([]byte(p))
	}
	return m.Sum(nil)
}

// Equal compares in constant time.
func Equal(a, b []byte) bool { return hmac.Equal(a, b) }

func SHA256(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

func SHA256Hex(s string) string { return hex.EncodeToString(SHA256([]byte(s))) }

// Sealer encrypts small secrets at rest with AES-256-GCM.
type Sealer struct{ aead cipher.AEAD }

func NewSealer(key []byte) (*Sealer, error) {
	if len(key) < 32 {
		return nil, errors.New("sealer key must be at least 32 bytes")
	}
	k := SHA256(key) // derive exactly 32 bytes
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

func (s *Sealer) Seal(plain []byte) []byte {
	nonce := RandomBytes(s.aead.NonceSize())
	return append(nonce, s.aead.Seal(nil, nonce, plain, nil)...)
}

func (s *Sealer) Open(sealed []byte) ([]byte, error) {
	ns := s.aead.NonceSize()
	if len(sealed) < ns {
		return nil, errors.New("ciphertext too short")
	}
	return s.aead.Open(nil, sealed[:ns], sealed[ns:], nil)
}
