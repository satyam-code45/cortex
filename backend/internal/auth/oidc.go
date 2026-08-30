package auth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Google's OIDC endpoints. Fields on OIDC override them in tests.
const (
	GoogleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	GoogleTokenURL = "https://oauth2.googleapis.com/token"
	GoogleIssuer   = "https://accounts.google.com"
)

// clockSkew is the tolerance on the ID token's exp check. Google's clocks and
// this machine's disagree by seconds at most; a minute absorbs that without
// meaningfully extending a token's life.
const clockSkew = time.Minute

// OIDC drives the Google sign-in flow: consent URL, code exchange, ID-token
// verification.
type OIDC struct {
	Client      Client
	JWKS        *JWKSCache
	Issuer      string
	RedirectURI string
}

// IDClaims is the verified identity from an ID token.
type IDClaims struct {
	Sub   string
	Email string
	Name  string
	// Nonce echoes the value LoginURL sent; the callback compares it to the
	// one parked in the state cookie, binding this ID token to this specific
	// login attempt (a replayed token for the same client fails the compare).
	Nonce   string
	Picture string
}

// LoginURL builds the consent URL for a sign-in attempt. Scopes are fixed:
// identity only, never data access — data-source scopes belong to their own
// flows and their own tokens.
func (o *OIDC) LoginURL(state string, pkce *PKCE, nonce string) string {
	return o.Client.AuthCodeURL(AuthCodeParams{
		RedirectURI: o.RedirectURI,
		State:       state,
		PKCE:        pkce,
		Scopes:      []string{"openid", "email", "profile"},
		Extra:       url.Values{"nonce": {nonce}},
	})
}

// Exchange swaps the callback's code for tokens. Sign-in needs no refresh
// token — the session outlives the tokens, and Google is only consulted again
// at the next login.
func (o *OIDC) Exchange(ctx context.Context, code string, pkce *PKCE) (*Token, error) {
	return o.Client.ExchangeCode(ctx, code, o.RedirectURI, pkce, ExchangeOptions{})
}

// VerifyIDToken checks the token's RS256 signature against the JWKS and its
// iss/aud/exp claims, returning the identity claims.
//
// Hand-rolled on stdlib crypto (split, decode, VerifyPKCS1v15) — a JWT library
// would mostly add the alg-confusion attack surface this function avoids by
// accepting exactly one algorithm.
func (o *OIDC) VerifyIDToken(ctx context.Context, raw string) (*IDClaims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("auth: ID token is not a JWT")
	}

	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("auth: decode ID token header: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, fmt.Errorf("auth: parse ID token header: %w", err)
	}
	// Exactly RS256. Accepting whatever the header claims is the classic JWT
	// vulnerability ("alg":"none", or HS256 keyed with the public key).
	if header.Alg != "RS256" {
		return nil, fmt.Errorf("auth: ID token alg %q is not RS256", header.Alg)
	}

	key, err := o.JWKS.Key(ctx, header.Kid)
	if err != nil {
		return nil, err
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("auth: decode ID token signature: %w", err)
	}
	signed := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, signed[:], signature); err != nil {
		return nil, fmt.Errorf("auth: ID token signature invalid: %w", err)
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("auth: decode ID token payload: %w", err)
	}
	var payload struct {
		Iss           string `json:"iss"`
		Aud           string `json:"aud"`
		Exp           int64  `json:"exp"`
		Sub           string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
		Nonce         string `json:"nonce"`
		Picture       string `json:"picture"`
	}
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return nil, fmt.Errorf("auth: parse ID token payload: %w", err)
	}

	issuer := o.Issuer
	if issuer == "" {
		issuer = GoogleIssuer
	}
	// Google historically issued both forms; both attest the same issuer.
	if payload.Iss != issuer && payload.Iss != strings.TrimPrefix(issuer, "https://") {
		return nil, fmt.Errorf("auth: ID token issuer %q is not %q", payload.Iss, issuer)
	}
	if payload.Aud != o.Client.ID {
		return nil, fmt.Errorf("auth: ID token audience is not this client")
	}
	if time.Now().Add(-clockSkew).After(time.Unix(payload.Exp, 0)) {
		return nil, fmt.Errorf("auth: ID token expired")
	}
	if payload.Sub == "" || payload.Email == "" {
		return nil, fmt.Errorf("auth: ID token carries no identity")
	}
	// Accounts are keyed and linked by email, so an unverified email claim is
	// an account-takeover primitive: Google will assert addresses it has not
	// verified (non-Gmail signups, misconfigured Workspace domains), and
	// accepting one would let that assertion capture an existing user's row —
	// their conversations, and the stored key that funds their runs.
	if !payload.EmailVerified {
		return nil, fmt.Errorf("auth: ID token email is not verified")
	}

	return &IDClaims{
		Sub:     payload.Sub,
		Email:   payload.Email,
		Name:    payload.Name,
		Nonce:   payload.Nonce,
		Picture: payload.Picture,
	}, nil
}
