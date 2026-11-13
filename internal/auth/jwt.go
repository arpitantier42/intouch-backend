// Package auth validates Supabase access tokens and API keys.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Claims are the parts of a Supabase access token the API uses.
type Claims struct {
	Subject   string `json:"sub"`
	Role      string `json:"role"`
	Audience  any    `json:"aud"`
	SessionID string `json:"session_id"`
	Email     string `json:"email"`
	Phone     string `json:"phone"`
	ExpiresAt int64  `json:"exp"`
	IssuedAt  int64  `json:"iat"`

	// Raw is the decoded payload, forwarded to Postgres as request.jwt.claims.
	Raw json.RawMessage `json:"-"`
}

// Errors returned by Verify.
var (
	ErrMalformed = errors.New("malformed token")
	ErrSignature = errors.New("invalid token signature")
	ErrExpired   = errors.New("token expired")
	ErrClaims    = errors.New("token is not a signed-in user session")
)

// Verifier checks HS256 tokens signed with the project's JWT secret.
type Verifier struct {
	secret []byte
	now    func() time.Time
}

// NewVerifier returns a verifier for the given secret.
func NewVerifier(secret string) *Verifier {
	return &Verifier{secret: []byte(secret), now: time.Now}
}

// Verify parses token and returns its claims when the signature is valid,
// it has not expired, and it belongs to an authenticated user.
func (v *Verifier) Verify(token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(v.secret) == 0 {
		return nil, ErrMalformed
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, ErrMalformed
	}
	var h struct {
		Alg string `json:"alg"`
	}
	if json.Unmarshal(header, &h) != nil || h.Alg != "HS256" {
		return nil, ErrMalformed
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, ErrMalformed
	}
	mac := hmac.New(sha256.New, v.secret)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, ErrSignature
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, ErrMalformed
	}
	var c Claims
	if json.Unmarshal(payload, &c) != nil {
		return nil, ErrMalformed
	}
	c.Raw = payload
	if c.ExpiresAt == 0 || v.now().Unix() >= c.ExpiresAt {
		return nil, ErrExpired
	}
	if c.Role != "authenticated" || c.Subject == "" || !hasAudience(c.Audience, "authenticated") {
		return nil, ErrClaims
	}
	return &c, nil
}

func hasAudience(aud any, want string) bool {
	switch a := aud.(type) {
	case string:
		return a == want
	case []any:
		for _, x := range a {
			if s, ok := x.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

// Sign creates an HS256 token for claims. Used by tests and local tooling;
// production tokens are issued by Supabase Auth.
func Sign(secret string, claims map[string]any) string {
	enc := base64.RawURLEncoding
	h := enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	p, _ := json.Marshal(claims)
	unsigned := h + "." + enc.EncodeToString(p)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(unsigned))
	return unsigned + "." + enc.EncodeToString(mac.Sum(nil))
}

// KeyEqual compares API keys in constant time; an empty expected key never matches.
func KeyEqual(expected, got string) bool {
	if expected == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(got)) == 1
}
