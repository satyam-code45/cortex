package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// Cookie names. Session persistence itself lives in the api package via sqlc;
// this package only owns the token math so the hash-only-in-DB invariant has
// one implementation.
const (
	// SessionCookieName carries the session token.
	SessionCookieName = "cortex_session"
	// StateCookieName carries the OAuth state and PKCE verifier between the
	// login redirect and the callback, as "state.verifier".
	StateCookieName = "cortex_oauth_state"
	// ConnectStateCookieName is the same for the Gmail connect flow,
	// as "state.verifier" — no nonce, the flow issues no ID token. A distinct
	// name and path keep a concurrent login flow's cookie from colliding.
	ConnectStateCookieName = "cortex_connect_state"
)

// NewSessionToken generates a session token and its storage hash. The token
// (256 random bits, base64url) goes in the cookie; only the SHA-256 goes in
// the database, so a database leak does not mint valid cookies.
func NewSessionToken() (token string, hash []byte, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("auth: generate session token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	return token, HashSessionToken(token), nil
}

// HashSessionToken maps a cookie token to its database representation.
func HashSessionToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
