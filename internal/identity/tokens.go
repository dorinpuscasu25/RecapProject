package identity

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/dorinpuscasu25/recapproject/internal/platform/ids"
)

// TokenIssuer signs and validates short-lived EdDSA access tokens. Keys are selected by `kid`
// to allow rotation: new tokens use the active key; old keys remain valid for verification.
type TokenIssuer struct {
	issuer   string
	audience string
	ttl      time.Duration
	keys     map[string]ed25519.PrivateKey
	active   string
	now      func() time.Time
}

func NewTokenIssuer(issuer, audience string, ttl time.Duration, keys map[string]ed25519.PrivateKey, active string) *TokenIssuer {
	return &TokenIssuer{issuer: issuer, audience: audience, ttl: ttl, keys: keys, active: active, now: time.Now}
}

type AccessClaims struct {
	SessionID string `json:"sid"`
	jwt.RegisteredClaims
}

// Issue returns a signed access token and its expiry.
func (t *TokenIssuer) Issue(userID, sessionID uuid.UUID) (string, time.Time, error) {
	now := t.now().UTC()
	exp := now.Add(t.ttl)
	claims := AccessClaims{
		SessionID: sessionID.String(),
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    t.issuer,
			Subject:   userID.String(),
			Audience:  jwt.ClaimStrings{t.audience},
			ExpiresAt: jwt.NewNumericDate(exp),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ID:        ids.New().String(),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["kid"] = t.active
	s, err := tok.SignedString(t.keys[t.active])
	return s, exp, err
}

var ErrInvalidToken = errors.New("invalid access token")

// Validate parses and validates a token. Only EdDSA is accepted (prevents alg confusion such as
// "none" or HS256-with-public-key); iss, aud, exp, iat, sub, jti and sid are required.
func (t *TokenIssuer) Validate(raw string) (*AccessClaims, error) {
	claims := &AccessClaims{}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}),
		jwt.WithIssuer(t.issuer),
		jwt.WithAudience(t.audience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(30*time.Second),
		jwt.WithTimeFunc(t.now),
	)
	_, err := parser.ParseWithClaims(raw, claims, func(tok *jwt.Token) (any, error) {
		kid, _ := tok.Header["kid"].(string)
		k, ok := t.keys[kid]
		if !ok {
			return nil, fmt.Errorf("unknown kid %q", kid)
		}
		return k.Public(), nil
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if claims.Subject == "" || claims.ID == "" || claims.SessionID == "" || claims.IssuedAt == nil {
		return nil, fmt.Errorf("%w: missing required claims", ErrInvalidToken)
	}
	return claims, nil
}

// PublicJWKS returns the public keys as a JWKS document (for future OIDC-style consumers).
func (t *TokenIssuer) PublicJWKS() map[string]any {
	var keys []map[string]string
	for kid, k := range t.keys {
		pub := k.Public().(ed25519.PublicKey)
		keys = append(keys, map[string]string{"kty": "OKP", "crv": "Ed25519", "kid": kid, "alg": "EdDSA", "use": "sig",
			"x": b64url(pub)})
	}
	return map[string]any{"keys": keys}
}
