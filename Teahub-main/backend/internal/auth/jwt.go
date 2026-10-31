// Package auth implements TeaHub's GitHub OAuth login flow and the HS256
// JWT session tokens issued after a successful login.
//
// The JWT implementation here is deliberately hand-rolled against the
// stdlib (crypto/hmac + encoding/json + encoding/base64) rather than a
// third-party library: TeaHub only needs HS256 sign/verify with a small,
// fixed claim set, and stdlib-only keeps the dependency surface (and the
// "unnecessary dependencies" audit item in ROADMAP/PHASE 3) minimal. If
// TeaHub ever needs RS256/JWKS (e.g. to accept tokens from another
// service), switch to golang-jwt/jwt at that point rather than growing
// this file into a second JWT library.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	ErrExpiredToken = errors.New("auth: token expired")
	ErrInvalidToken = errors.New("auth: invalid token")
)

// Claims is TeaHub's JWT payload. Kept intentionally small: the subject
// (user ID) is the only thing request handlers need, everything else
// (profile fields) is looked up fresh from Postgres so a stale JWT can't
// serve stale profile data.
type Claims struct {
	Subject   string `json:"sub"`
	JWTID     string `json:"jti"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

func b64encode(b []byte) string          { return base64.RawURLEncoding.EncodeToString(b) }
func b64decode(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

// SignJWT produces a compact HS256 JWT for the given claims.
func SignJWT(key []byte, claims Claims) (string, error) {
	header, err := json.Marshal(jwtHeader{Alg: "HS256", Typ: "JWT"})
	if err != nil {
		return "", fmt.Errorf("marshaling jwt header: %w", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshaling jwt claims: %w", err)
	}
	signingInput := b64encode(header) + "." + b64encode(payload)

	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signingInput))
	sig := mac.Sum(nil)

	return signingInput + "." + b64encode(sig), nil
}

// VerifyJWT checks the signature and expiry of a compact JWT and returns
// its claims. Uses constant-time comparison for the signature check to
// avoid timing side-channels.
func VerifyJWT(key []byte, token string) (Claims, error) {
	var claims Claims

	parts := splitJWT(token)
	if len(parts) != 3 {
		return claims, ErrInvalidToken
	}
	signingInput := parts[0] + "." + parts[1]

	sig, err := b64decode(parts[2])
	if err != nil {
		return claims, ErrInvalidToken
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signingInput))
	expected := mac.Sum(nil)
	if subtle.ConstantTimeCompare(sig, expected) != 1 {
		return claims, ErrInvalidToken
	}

	payload, err := b64decode(parts[1])
	if err != nil {
		return claims, ErrInvalidToken
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return claims, ErrInvalidToken
	}
	if claims.Subject == "" || claims.JWTID == "" {
		return claims, ErrInvalidToken
	}
	if time.Now().Unix() > claims.ExpiresAt {
		return claims, ErrExpiredToken
	}
	return claims, nil
}

func splitJWT(token string) []string {
	parts := make([]string, 0, 3)
	start := 0
	for i := 0; i < len(token); i++ {
		if token[i] == '.' {
			parts = append(parts, token[start:i])
			start = i + 1
		}
	}
	parts = append(parts, token[start:])
	return parts
}
