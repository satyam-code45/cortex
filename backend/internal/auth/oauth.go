// Package auth holds the OAuth 2.0 / OIDC primitives Cortex hand-rolls:
// the authorization-code flow shared by Gmail authorization (cmd/gmail-auth)
// and Google sign-in (REQ-7.1), ID-token verification against Google's JWKS,
// and the session-token helpers behind the API's cookie auth.
//
// Hand-rolled for the same reasons the Gmail flow was (it started there and
// was extracted on Day 7): the flow is small enough to own outright, owning it
// keeps the locked stack intact, and every path stays testable against
// httptest — which a vendored SDK's internal transport is not.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"cortex/internal/tools/httpx"
)

// Client is an OAuth client registration: the id/secret pair plus the
// provider's two endpoints.
type Client struct {
	ID       string
	Secret   string
	AuthURL  string
	TokenURL string
}

// PKCE is one authorization attempt's proof key.
type PKCE struct {
	Verifier  string
	Challenge string
}

// NewPKCE generates a code verifier and its S256 challenge.
func NewPKCE() (*PKCE, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("auth: generate PKCE verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	return &PKCE{
		Verifier:  verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
	}, nil
}

// RandomState generates an anti-forgery state value.
func RandomState() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("auth: generate state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// AuthCodeParams parameterizes one consent URL.
//
// Extra is where a flow's provider-specific knobs go — the Gmail authorization
// flow passes access_type=offline&prompt=consent because it needs a refresh
// token re-issued every run; a plain sign-in passes nothing, because asking
// for offline access it will never use only widens the consent screen.
type AuthCodeParams struct {
	RedirectURI string
	State       string
	PKCE        *PKCE
	Scopes      []string
	Extra       url.Values
}

// AuthCodeURL builds the consent URL the user's browser is sent to.
func (c *Client) AuthCodeURL(p AuthCodeParams) string {
	query := url.Values{}
	query.Set("client_id", c.ID)
	query.Set("redirect_uri", p.RedirectURI)
	query.Set("response_type", "code")
	query.Set("scope", strings.Join(p.Scopes, " "))
	query.Set("state", p.State)
	if p.PKCE != nil {
		query.Set("code_challenge", p.PKCE.Challenge)
		query.Set("code_challenge_method", "S256")
	}
	for key, values := range p.Extra {
		for _, v := range values {
			query.Add(key, v)
		}
	}
	return c.AuthURL + "?" + query.Encode()
}

// Token is the result of a code exchange.
type Token struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	Scope        string
	// IDToken is the OIDC identity assertion (a signed JWT). Present when the
	// flow requested the openid scope; the Gmail flow never sees one.
	IDToken string
	Expiry  time.Time
}

// ExchangeOptions parameterizes a code exchange.
type ExchangeOptions struct {
	// HTTPClient is optional; tests point it at an httptest server.
	HTTPClient *http.Client
	// RequireRefreshToken makes a response without a refresh token an error.
	// The Gmail authorization flow requires one (it is the whole point of the
	// flow); a plain sign-in never receives one and must not demand it.
	RequireRefreshToken bool
}

// ErrNoRefreshToken is returned when RequireRefreshToken is set and the
// provider omitted the refresh token. Callers wrap it with flow-specific
// recovery instructions.
var ErrNoRefreshToken = errors.New("auth: token response carried no refresh token")

// ExchangeCode swaps an authorization code for a token.
func (c *Client) ExchangeCode(ctx context.Context, code, redirectURI string, pkce *PKCE, opts ExchangeOptions) (*Token, error) {
	base, path, err := SplitEndpoint(c.TokenURL)
	if err != nil {
		return nil, err
	}
	transport, err := httpx.New(httpx.Config{
		Name:        "oauth",
		BaseURL:     base,
		HTTPClient:  opts.HTTPClient,
		MinInterval: -1,
		ParseError:  ParseOAuthError,
	})
	if err != nil {
		return nil, err
	}

	form := url.Values{}
	form.Set("client_id", c.ID)
	form.Set("client_secret", c.Secret)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("grant_type", "authorization_code")
	if pkce != nil {
		form.Set("code_verifier", pkce.Verifier)
	}

	response, err := PostTokenForm(ctx, transport, path, form)
	if err != nil {
		return nil, err
	}
	if opts.RequireRefreshToken && response.RefreshToken == "" {
		return nil, ErrNoRefreshToken
	}
	return &Token{
		AccessToken:  response.AccessToken,
		RefreshToken: response.RefreshToken,
		TokenType:    response.TokenType,
		Scope:        response.Scope,
		IDToken:      response.IDToken,
		Expiry:       time.Now().Add(time.Duration(response.ExpiresIn) * time.Second),
	}, nil
}

// TokenResponse is the OAuth token endpoint's payload shape.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	IDToken      string `json:"id_token"`
}

// PostTokenForm posts a form-encoded body to a token endpoint. Exported so the
// gmail package's refresh path shares the exact wire behavior.
func PostTokenForm(ctx context.Context, client *httpx.Client, path string, form url.Values) (*TokenResponse, error) {
	var response TokenResponse
	err := client.Do(ctx, httpx.Request{
		Method:      http.MethodPost,
		Path:        path,
		RawBody:     []byte(form.Encode()),
		ContentType: "application/x-www-form-urlencoded",
	}, &response)
	if err != nil {
		return nil, err
	}
	if response.AccessToken == "" {
		return nil, errors.New("auth: token endpoint returned no access token")
	}
	return &response, nil
}

// OAuthError is a failure from an OAuth token endpoint.
type OAuthError struct {
	StatusCode  int
	Code        string
	Description string
}

func (e *OAuthError) Error() string {
	switch {
	case e.Code != "" && e.Description != "":
		return fmt.Sprintf("oauth %s: %s", e.Code, e.Description)
	case e.Code != "":
		return fmt.Sprintf("oauth %s (HTTP %d)", e.Code, e.StatusCode)
	default:
		return fmt.Sprintf("oauth failed with HTTP %d", e.StatusCode)
	}
}

// Permanent reports whether retrying is pointless. An invalid_grant means the
// refresh token has been revoked or expired, and no number of retries will
// bring it back — only re-running the auth flow will.
func (e *OAuthError) Permanent() bool { return httpx.PermanentStatus(e.StatusCode) }

// ParseOAuthError extracts the standard OAuth error shape.
func ParseOAuthError(status int, raw []byte) error {
	oauthErr := &OAuthError{StatusCode: status}
	var body struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.Unmarshal(raw, &body); err == nil {
		oauthErr.Code = body.Error
		oauthErr.Description = body.Description
	}
	return oauthErr
}

// SplitEndpoint splits a full URL into an origin and a path, which is the
// shape httpx.Client wants.
func SplitEndpoint(endpoint string) (base, path string, err error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return "", "", fmt.Errorf("auth: endpoint is not a valid URL: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", "", fmt.Errorf("auth: endpoint %q is not absolute", endpoint)
	}
	path = parsed.Path
	if path == "" {
		path = "/"
	}
	return parsed.Scheme + "://" + parsed.Host, path, nil
}
