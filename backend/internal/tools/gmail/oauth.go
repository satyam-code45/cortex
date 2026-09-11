package gmail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"cortex/internal/auth"
	"cortex/internal/tools/httpx"
)

// OAuth against Google.
//
// The generic pieces — PKCE, state, the consent URL, the code exchange, the
// OAuth error shape — live in internal/auth, shared with Google
// sign-in. What stays here is the Gmail-flow specifics: the credential file
// format, the on-disk token cache, the refreshing TokenSource, and the two
// knobs a data-access flow needs that a sign-in must not have
// (access_type=offline + prompt=consent, and the demand for a refresh token).
//
// The security-relevant choices, none of which are defaults:
//
//   - PKCE (S256) on a flow that does not strictly require it. The redirect
//     lands on a loopback port any local process can race for, and the verifier
//     is what makes an intercepted authorization code useless.
//   - A random state parameter, checked on return, so a stray request to the
//     loopback listener cannot inject someone else's code.
//   - The cached token is written 0600 and holds a *refresh* token, which does
//     not expire. It is a long-lived credential to the mailbox and is treated
//     like one: gitignored, never logged, never printed.

const (
	// ScopeReadonly lets the agent tools read mail. This is all the server ever
	// needs.
	ScopeReadonly = "https://www.googleapis.com/auth/gmail.readonly"
	// ScopeInsert lets the seeder place fixture messages in the mailbox. It is
	// requested by cmd/gmail-auth because the token is shared, but nothing on
	// the agent path uses it.
	ScopeInsert = "https://www.googleapis.com/auth/gmail.insert"
	// ScopeSend lets Cortex send mail as the account. It is requested ONLY when
	// a user explicitly enables writes for their Gmail connection — never
	// bundled into the read connection — so a user who never enabled writes has
	// no token that could send anything, whatever the rest of the system does.
	//
	// Google classes this as a restricted scope, the same tier as reading mail:
	// a published app needs Google's review and possibly a security assessment
	// before it may ask the public for it. Test users are unaffected.
	ScopeSend = "https://www.googleapis.com/auth/gmail.send"
	// ScopeLabels lets the seeder create the fixture label. EnsureLabel needs
	// it; without this scope label creation fails with a 403.
	ScopeLabels = "https://www.googleapis.com/auth/gmail.labels"

	// DefaultTokenPath is where the refresh token is cached, relative to the
	// repository root.
	DefaultTokenPath = ".gmail-token.json"

	// tokenFileMode keeps the cached refresh token owner-only.
	tokenFileMode = 0o600

	// refreshSkew is how long before expiry an access token is treated as
	// already expired. It covers clock skew and the round trip itself, so a
	// token cannot expire between the check and the request that uses it.
	refreshSkew = 60 * time.Second
)

// Credentials is the OAuth client from the downloaded Google credential file.
type Credentials struct {
	ClientID     string
	ClientSecret string
	AuthURI      string
	TokenURI     string
}

// credentialsFile is the shape Google Cloud hands out. A Desktop client puts
// everything under "installed"; a Web client uses "web".
type credentialsFile struct {
	Installed *credentialsBody `json:"installed"`
	Web       *credentialsBody `json:"web"`
}

type credentialsBody struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	AuthURI      string `json:"auth_uri"`
	TokenURI     string `json:"token_uri"`
}

// LoadCredentials reads an OAuth client credential file from disk.
func LoadCredentials(path string) (*Credentials, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("gmail: read credentials %s: %w", path, err)
	}
	var file credentialsFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("gmail: parse credentials %s: %w", path, err)
	}
	body := file.Installed
	if body == nil {
		body = file.Web
	}
	if body == nil {
		return nil, fmt.Errorf("gmail: %s has neither an \"installed\" nor a \"web\" client; "+
			"download the OAuth *Desktop* client JSON from Google Cloud", path)
	}
	if body.ClientID == "" || body.ClientSecret == "" {
		return nil, fmt.Errorf("gmail: %s is missing client_id or client_secret", path)
	}

	creds := &Credentials{
		ClientID:     body.ClientID,
		ClientSecret: body.ClientSecret,
		AuthURI:      body.AuthURI,
		TokenURI:     body.TokenURI,
	}
	if creds.AuthURI == "" {
		creds.AuthURI = "https://accounts.google.com/o/oauth2/auth"
	}
	if creds.TokenURI == "" {
		creds.TokenURI = "https://oauth2.googleapis.com/token"
	}
	return creds, nil
}

// Token is the cached OAuth token.
//
// RefreshToken is the durable part; AccessToken and Expiry are a cache of the
// most recent exchange, persisted only so that a restart does not have to spend
// a round trip re-deriving what is still valid.
type Token struct {
	RefreshToken string    `json:"refresh_token"`
	AccessToken  string    `json:"access_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`
	Scope        string    `json:"scope,omitempty"`
}

// valid reports whether the access token can still be used.
func (t *Token) valid() bool {
	return t.AccessToken != "" && time.Now().Add(refreshSkew).Before(t.Expiry)
}

// LoadToken reads the cached token from disk.
func LoadToken(path string) (*Token, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("gmail: no cached token at %s — run `make gmail-auth` once to authorize: %w",
				path, err)
		}
		return nil, fmt.Errorf("gmail: read token %s: %w", path, err)
	}
	var token Token
	if err := json.Unmarshal(raw, &token); err != nil {
		return nil, fmt.Errorf("gmail: parse token %s: %w", path, err)
	}
	if token.RefreshToken == "" {
		return nil, fmt.Errorf("gmail: %s holds no refresh token — delete it and run `make gmail-auth` again", path)
	}
	return &token, nil
}

// SaveToken writes the token to disk with owner-only permissions.
//
// Written to a fresh temporary file and renamed into place, for two reasons
// that both matter for a non-expiring credential to a real mailbox:
//
//   - os.WriteFile applies its permission argument only when it *creates* the
//     file. Writing straight to an existing .gmail-token.json that had somehow
//     become 0644 — restored from a tar, copied without -p — would put the
//     refresh token on disk world-readable, and a chmod afterwards closes the
//     window only after the fact. Creating with O_EXCL means the mode is never
//     wrong, not even briefly.
//   - Rename is atomic, so a crash or a full disk mid-write cannot leave a
//     truncated file. The old token survives, where an in-place truncate would
//     destroy the only copy and force the whole browser flow again.
func SaveToken(path string, token *Token) error {
	encoded, err := json.MarshalIndent(token, "", "  ")
	if err != nil {
		return fmt.Errorf("gmail: encode token: %w", err)
	}
	encoded = append(encoded, '\n')

	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("gmail: create token directory: %w", err)
		}
	}

	temp, err := os.CreateTemp(dir, ".gmail-token-*.tmp")
	if err != nil {
		return fmt.Errorf("gmail: create temporary token file: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName) //nolint:errcheck // no-op once the rename succeeds

	// CreateTemp already makes the file 0600, but the umask does not apply to
	// Chmod, so this is what guarantees it rather than merely expects it.
	if err := temp.Chmod(tokenFileMode); err != nil {
		temp.Close() //nolint:errcheck // already failing
		return fmt.Errorf("gmail: secure temporary token file: %w", err)
	}
	if _, err := temp.Write(encoded); err != nil {
		temp.Close() //nolint:errcheck // already failing
		return fmt.Errorf("gmail: write token: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("gmail: close temporary token file: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("gmail: install token %s: %w", path, err)
	}
	return nil
}

// PKCE is one authorization attempt's proof key. Alias, not a wrapper: the
// same value flows between this package and internal/auth.
type PKCE = auth.PKCE

// NewPKCE generates a code verifier and its S256 challenge.
func NewPKCE() (*PKCE, error) { return auth.NewPKCE() }

// RandomState generates an anti-forgery state value.
func RandomState() (string, error) { return auth.RandomState() }

// AuthCodeURL builds the consent URL the operator opens in a browser.
func (c *Credentials) AuthCodeURL(redirectURI, state string, pkce *PKCE, scopes []string) string {
	client := auth.Client{ID: c.ClientID, Secret: c.ClientSecret, AuthURL: c.AuthURI, TokenURL: c.TokenURI}
	return client.AuthCodeURL(auth.AuthCodeParams{
		RedirectURI: redirectURI,
		State:       state,
		PKCE:        pkce,
		Scopes:      scopes,
		// offline is what makes Google return a refresh token at all, and
		// consent forces it to be re-issued even if this account has authorized
		// before — without which a second run yields an access token and no way
		// to renew it. Sign-in deliberately passes neither.
		Extra: url.Values{"access_type": {"offline"}, "prompt": {"consent"}},
	})
}

// TokenSource hands out access tokens, refreshing and re-persisting as needed.
//
// It is safe for concurrent use: the agent may run several Gmail tool calls at
// once, and without the lock each would independently notice the expiry and
// fire its own refresh.
type TokenSource struct {
	creds     *Credentials
	tokenPath string
	http      *httpx.Client
	logger    *slog.Logger
	// tokenEndpointPath is the path component of the token endpoint, e.g.
	// "/token". A field rather than a constant because tests point the whole
	// endpoint at an httptest server.
	tokenEndpointPath string

	mu    sync.Mutex
	token *Token
}

// TokenSourceConfig configures a TokenSource.
type TokenSourceConfig struct {
	Credentials *Credentials
	// Token is the currently cached token.
	Token *Token
	// TokenPath is where refreshed tokens are written back. Empty disables
	// persistence, which is what tests use.
	TokenPath string
	// HTTPClient is optional.
	HTTPClient *http.Client
	// TokenURL overrides the credentials' token endpoint, for tests.
	TokenURL string
	// Logger receives token-persistence warnings.
	Logger *slog.Logger
}

// NewTokenSource builds a TokenSource.
func NewTokenSource(cfg TokenSourceConfig) (*TokenSource, error) {
	if cfg.Credentials == nil {
		return nil, errors.New("gmail: Credentials are required")
	}
	if cfg.Token == nil || cfg.Token.RefreshToken == "" {
		return nil, errors.New("gmail: a token with a refresh token is required")
	}

	endpoint := cfg.TokenURL
	if endpoint == "" {
		endpoint = cfg.Credentials.TokenURI
	}
	base, path, err := splitEndpoint(endpoint)
	if err != nil {
		return nil, err
	}

	transport, err := httpx.New(httpx.Config{
		Name:       "gmail-oauth",
		BaseURL:    base,
		HTTPClient: cfg.HTTPClient,
		// The token endpoint is not the rate-limited resource; throttling it
		// would only add latency to a refresh that happens once an hour.
		MinInterval: -1,
		// Deliberately tighter than the defaults. This client runs inside
		// Config.Authorize, which runs inside the outer Gmail client's attempt,
		// which runs inside the orchestrator's per-tool deadline. At the default
		// four retries a flapping token endpoint would spend 7.5s of backoff
		// before the Gmail request was even issued, and a refresh that needs a
		// fourth attempt is not going to land inside a tool call anyway.
		MaxRetries:    2,
		MaxRetryDelay: 2 * time.Second,
		ParseError:    parseOAuthError,
	})
	if err != nil {
		return nil, err
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &TokenSource{
		creds:             cfg.Credentials,
		tokenPath:         cfg.TokenPath,
		http:              transport,
		logger:            logger,
		tokenEndpointPath: path,
		token:             cfg.Token,
	}, nil
}

// AccessToken returns a currently-valid access token, refreshing if needed.
func (s *TokenSource) AccessToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.token.valid() {
		return s.token.AccessToken, nil
	}
	if err := s.refreshLocked(ctx); err != nil {
		return "", err
	}
	return s.token.AccessToken, nil
}

// refreshLocked exchanges the refresh token for a new access token. The caller
// holds s.mu.
func (s *TokenSource) refreshLocked(ctx context.Context) error {
	form := url.Values{}
	form.Set("client_id", s.creds.ClientID)
	form.Set("client_secret", s.creds.ClientSecret)
	form.Set("refresh_token", s.token.RefreshToken)
	form.Set("grant_type", "refresh_token")

	response, err := auth.PostTokenForm(ctx, s.http, s.tokenEndpointPath, form)
	if err != nil {
		return err
	}

	s.token.AccessToken = response.AccessToken
	s.token.TokenType = response.TokenType
	s.token.Expiry = time.Now().Add(time.Duration(response.ExpiresIn) * time.Second)
	if response.Scope != "" {
		s.token.Scope = response.Scope
	}
	// Google does not resend the refresh token on a refresh, so the stored one
	// stays. It only rotates if the user revokes access, which invalidates it
	// anyway.
	if response.RefreshToken != "" {
		s.token.RefreshToken = response.RefreshToken
	}

	if s.tokenPath != "" {
		if err := SaveToken(s.tokenPath, s.token); err != nil {
			// Persisting is a cache optimization, not correctness: the refresh
			// already succeeded and the token in memory is usable. Failing the
			// request here would turn a read-only-disk annoyance into an
			// outage. It is logged rather than swallowed, though — otherwise a
			// permanently unwritable token file produces no signal at all, and
			// every restart silently pays a refresh nobody can explain.
			s.logger.Warn("gmail: refreshed token could not be persisted; "+
				"it is valid in memory but will be re-fetched after a restart",
				"path", s.tokenPath, "error", err)
		}
	}
	return nil
}

// ExchangeCode swaps an authorization code for a token. Used once, by
// cmd/gmail-auth.
func ExchangeCode(
	ctx context.Context,
	creds *Credentials,
	code, redirectURI string,
	pkce *PKCE,
	httpClient *http.Client,
) (*Token, error) {
	client := auth.Client{ID: creds.ClientID, Secret: creds.ClientSecret, AuthURL: creds.AuthURI, TokenURL: creds.TokenURI}
	token, err := client.ExchangeCode(ctx, code, redirectURI, pkce, auth.ExchangeOptions{
		HTTPClient: httpClient,
		// This flow exists to obtain a refresh token — the durable credential
		// the server's TokenSource lives on. Without one the exchange failed at
		// its purpose, whatever the HTTP status said.
		RequireRefreshToken: true,
	})
	if errors.Is(err, auth.ErrNoRefreshToken) {
		return nil, errors.New("gmail: Google returned no refresh token; " +
			"revoke Cortex's access at https://myaccount.google.com/permissions and authorize again")
	}
	if err != nil {
		return nil, err
	}
	return &Token{
		RefreshToken: token.RefreshToken,
		AccessToken:  token.AccessToken,
		TokenType:    token.TokenType,
		Expiry:       token.Expiry,
		Scope:        token.Scope,
	}, nil
}

// OAuthError is a failure from Google's token endpoint. Alias so errors.As
// works identically across this package and internal/auth.
type OAuthError = auth.OAuthError

// parseOAuthError extracts Google's OAuth error shape.
func parseOAuthError(status int, raw []byte) error { return auth.ParseOAuthError(status, raw) }

// splitEndpoint splits a full URL into an origin and a path, which is the shape
// httpx.Client wants.
func splitEndpoint(endpoint string) (base, path string, err error) {
	return auth.SplitEndpoint(endpoint)
}
