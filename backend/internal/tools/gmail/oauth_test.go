package gmail_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cortex/internal/tools/gmail"
)

// The Gmail token store.
//
// The cached file holds a *refresh* token, which does not expire: it is a
// long-lived credential to a real mailbox. Three things therefore have to hold,
// and each is checked here against an httptest stand-in for Google — no test in
// this package touches the network.
//
//   - The refresh exchange sends what Google requires and adopts what it
//     returns, keeping the refresh token when Google (as it normally does) does
//     not resend one.
//   - Expiry is honoured: a still-valid access token is reused rather than
//     re-bought, and one that is about to expire is renewed *before* it is
//     handed to a request that would then fail mid-flight.
//   - The file is 0600. Not chmod'ed to 0600 after a world-readable create —
//     that leaves a window in which any local process can read it.

// fakeOAuth is an httptest stand-in for Google's token endpoint.
type fakeOAuth struct {
	t      *testing.T
	server *httptest.Server

	mu        sync.Mutex
	forms     []url.Values
	responses []oauthResponse
	next      int
}

// oauthResponse is one scripted token-endpoint reply.
type oauthResponse struct {
	// status defaults to 200.
	status int
	body   string
}

func newFakeOAuth(t *testing.T, responses ...oauthResponse) *fakeOAuth {
	t.Helper()
	f := &fakeOAuth{t: t, responses: responses}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeOAuth) handle(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	form, err := url.ParseQuery(string(raw))
	if err != nil {
		f.t.Errorf("token endpoint body is not form-encoded: %v", err)
	}
	if got := r.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/x-www-form-urlencoded") {
		f.t.Errorf("token request Content-Type = %q, want application/x-www-form-urlencoded", got)
	}
	if r.Method != http.MethodPost {
		f.t.Errorf("token request method = %s, want POST", r.Method)
	}

	f.mu.Lock()
	f.forms = append(f.forms, form)
	index := f.next
	f.next++
	var scripted oauthResponse
	exhausted := index >= len(f.responses)
	if !exhausted {
		scripted = f.responses[index]
	}
	f.mu.Unlock()

	if exhausted {
		f.t.Errorf("fakeOAuth: unscripted token request #%d", index+1)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	status := scripted.status
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(scripted.body))
}

func (f *fakeOAuth) exchanges() []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]url.Values, len(f.forms))
	copy(out, f.forms)
	return out
}

func (f *fakeOAuth) credentials() *gmail.Credentials {
	return &gmail.Credentials{
		ClientID:     testClientID,
		ClientSecret: testClientSecret,
		AuthURI:      "https://accounts.google.com/o/oauth2/auth",
		TokenURI:     f.server.URL + "/token",
	}
}

// ---------------------------------------------------------------------------
// refresh-token exchange
// ---------------------------------------------------------------------------

func TestTokenSourceRefreshesExpiredToken(t *testing.T) {
	fake := newFakeOAuth(t, oauthResponse{body: `{
	  "access_token":"ya29.freshly-minted",
	  "expires_in":3599,
	  "scope":"https://www.googleapis.com/auth/gmail.readonly",
	  "token_type":"Bearer"
	}`})

	source, err := gmail.NewTokenSource(gmail.TokenSourceConfig{
		Credentials: fake.credentials(),
		Token: &gmail.Token{
			RefreshToken: testRefreshToken,
			AccessToken:  "ya29.long-expired",
			TokenType:    "Bearer",
			Expiry:       time.Now().Add(-2 * time.Hour),
		},
		HTTPClient: fake.server.Client(),
	})
	if err != nil {
		t.Fatalf("build token source: %v", err)
	}

	token, err := source.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if token != "ya29.freshly-minted" {
		t.Errorf("access token = %q, want the newly exchanged one", token)
	}

	exchanges := fake.exchanges()
	if len(exchanges) != 1 {
		t.Fatalf("token exchanges = %d, want 1", len(exchanges))
	}
	form := exchanges[0]
	for field, want := range map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": testRefreshToken,
		"client_id":     testClientID,
		"client_secret": testClientSecret,
	} {
		if got := form.Get(field); got != want {
			t.Errorf("refresh form %s = %q, want %q", field, got, want)
		}
	}

	// The refreshed token is cached in memory: a second call must not buy
	// another one, or every tool call in a run pays a round trip.
	if _, err := source.AccessToken(context.Background()); err != nil {
		t.Fatalf("second AccessToken: %v", err)
	}
	if n := len(fake.exchanges()); n != 1 {
		t.Errorf("token exchanges = %d after a second call, want 1 (the fresh token must be reused)", n)
	}
}

// A still-valid access token must be reused rather than re-bought.
func TestTokenSourceReusesValidToken(t *testing.T) {
	fake := newFakeOAuth(t) // no response scripted: a refresh here is a failure

	source, err := gmail.NewTokenSource(gmail.TokenSourceConfig{
		Credentials: fake.credentials(),
		Token: &gmail.Token{
			RefreshToken: testRefreshToken,
			AccessToken:  testAccessToken,
			TokenType:    "Bearer",
			Expiry:       time.Now().Add(45 * time.Minute),
		},
		HTTPClient: fake.server.Client(),
	})
	if err != nil {
		t.Fatalf("build token source: %v", err)
	}

	token, err := source.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if token != testAccessToken {
		t.Errorf("access token = %q, want the cached %q", token, testAccessToken)
	}
	if n := len(fake.exchanges()); n != 0 {
		t.Errorf("token exchanges = %d, want 0 for a valid cached token", n)
	}
}

// Expiry handling, table-driven. A token that expires in a handful of seconds
// is treated as already expired: the request it authorizes has to travel, and
// a token that dies in flight surfaces as an unexplained 401 in the middle of
// an agent run.
func TestTokenSourceExpiryHandling(t *testing.T) {
	tests := []struct {
		name        string
		accessToken string
		expiresIn   time.Duration
		wantRefresh bool
	}{
		{name: "long-lived token is reused", accessToken: testAccessToken, expiresIn: time.Hour},
		{name: "expired token is refreshed", accessToken: testAccessToken, expiresIn: -time.Minute, wantRefresh: true},
		{name: "token expiring within seconds is refreshed", accessToken: testAccessToken, expiresIn: 5 * time.Second, wantRefresh: true},
		{name: "zero expiry is refreshed", accessToken: testAccessToken, expiresIn: 0, wantRefresh: true},
		{name: "empty access token is refreshed", accessToken: "", expiresIn: time.Hour, wantRefresh: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var scripted []oauthResponse
			if tc.wantRefresh {
				scripted = append(scripted, oauthResponse{body: `{
				  "access_token":"ya29.freshly-minted","expires_in":3599,"token_type":"Bearer"
				}`})
			}
			fake := newFakeOAuth(t, scripted...)

			source, err := gmail.NewTokenSource(gmail.TokenSourceConfig{
				Credentials: fake.credentials(),
				Token: &gmail.Token{
					RefreshToken: testRefreshToken,
					AccessToken:  tc.accessToken,
					Expiry:       time.Now().Add(tc.expiresIn),
				},
				HTTPClient: fake.server.Client(),
			})
			if err != nil {
				t.Fatalf("build token source: %v", err)
			}

			token, err := source.AccessToken(context.Background())
			if err != nil {
				t.Fatalf("AccessToken: %v", err)
			}

			want := len(scripted)
			if got := len(fake.exchanges()); got != want {
				t.Errorf("token exchanges = %d, want %d", got, want)
			}
			if tc.wantRefresh && token != "ya29.freshly-minted" {
				t.Errorf("access token = %q, want the refreshed one", token)
			}
			if !tc.wantRefresh && token != tc.accessToken {
				t.Errorf("access token = %q, want the cached %q", token, tc.accessToken)
			}
		})
	}
}

// Google does not resend the refresh token on a refresh, so the stored one must
// survive — losing it would silently turn a durable authorization into a
// one-hour one.
func TestTokenSourceKeepsRefreshTokenWhenGoogleOmitsIt(t *testing.T) {
	fake := newFakeOAuth(t, oauthResponse{body: `{
	  "access_token":"ya29.freshly-minted","expires_in":3599,"token_type":"Bearer"
	}`})
	path := filepath.Join(t.TempDir(), ".gmail-token.json")

	source, err := gmail.NewTokenSource(gmail.TokenSourceConfig{
		Credentials: fake.credentials(),
		Token: &gmail.Token{
			RefreshToken: testRefreshToken,
			Expiry:       time.Now().Add(-time.Minute),
		},
		TokenPath:  path,
		HTTPClient: fake.server.Client(),
	})
	if err != nil {
		t.Fatalf("build token source: %v", err)
	}
	if _, err := source.AccessToken(context.Background()); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}

	stored, err := gmail.LoadToken(path, "")
	if err != nil {
		t.Fatalf("LoadToken after refresh: %v", err)
	}
	if stored.RefreshToken != testRefreshToken {
		t.Errorf("stored refresh token = %q, want the original %q", stored.RefreshToken, testRefreshToken)
	}
	if stored.AccessToken != "ya29.freshly-minted" {
		t.Errorf("stored access token = %q, want the refreshed one", stored.AccessToken)
	}
	if !stored.Expiry.After(time.Now().Add(50 * time.Minute)) {
		t.Errorf("stored expiry = %s, want roughly an hour out (expires_in was 3599)", stored.Expiry)
	}
}

// A rotated refresh token must be adopted, or the next refresh uses a token
// Google has already retired.
func TestTokenSourceAdoptsRotatedRefreshToken(t *testing.T) {
	fake := newFakeOAuth(t, oauthResponse{body: `{
	  "access_token":"ya29.freshly-minted",
	  "refresh_token":"1//rotated-refresh-token",
	  "expires_in":3599,
	  "token_type":"Bearer"
	}`})
	path := filepath.Join(t.TempDir(), ".gmail-token.json")

	source, err := gmail.NewTokenSource(gmail.TokenSourceConfig{
		Credentials: fake.credentials(),
		Token: &gmail.Token{
			RefreshToken: testRefreshToken,
			Expiry:       time.Now().Add(-time.Minute),
		},
		TokenPath:  path,
		HTTPClient: fake.server.Client(),
	})
	if err != nil {
		t.Fatalf("build token source: %v", err)
	}
	if _, err := source.AccessToken(context.Background()); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}

	stored, err := gmail.LoadToken(path, "")
	if err != nil {
		t.Fatalf("LoadToken after refresh: %v", err)
	}
	if stored.RefreshToken != "1//rotated-refresh-token" {
		t.Errorf("stored refresh token = %q, want the rotated one", stored.RefreshToken)
	}
}

// A revoked refresh token is permanent: no retry brings it back, only re-running
// the auth flow. And the failure must not echo the client secret or the token.
func TestTokenSourceRefreshFailureIsPermanentAndSecretFree(t *testing.T) {
	fake := newFakeOAuth(t, oauthResponse{
		status: http.StatusBadRequest,
		body:   `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`,
	})

	source, err := gmail.NewTokenSource(gmail.TokenSourceConfig{
		Credentials: fake.credentials(),
		Token: &gmail.Token{
			RefreshToken: testRefreshToken,
			Expiry:       time.Now().Add(-time.Hour),
		},
		HTTPClient: fake.server.Client(),
	})
	if err != nil {
		t.Fatalf("build token source: %v", err)
	}

	_, err = source.AccessToken(context.Background())
	if err == nil {
		t.Fatal("AccessToken against invalid_grant returned no error")
	}

	var oauthErr *gmail.OAuthError
	if !errors.As(err, &oauthErr) {
		t.Fatalf("error = %T (%v), want *gmail.OAuthError", err, err)
	}
	if oauthErr.Code != "invalid_grant" {
		t.Errorf("error code = %q, want invalid_grant", oauthErr.Code)
	}
	if !oauthErr.Permanent() {
		t.Error("Permanent() = false on invalid_grant; no retry can revive a revoked token")
	}
	for _, secret := range []string{testClientSecret, testRefreshToken} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaks a credential: %s", err.Error())
		}
	}
}

// A TokenSource without a refresh token is unusable, and saying so at
// construction beats discovering it on the first tool call of a run.
func TestNewTokenSourceRequiresRefreshToken(t *testing.T) {
	fake := newFakeOAuth(t)

	for _, tc := range []struct {
		name  string
		token *gmail.Token
	}{
		{name: "nil token", token: nil},
		{name: "token with no refresh token", token: &gmail.Token{AccessToken: testAccessToken}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := gmail.NewTokenSource(gmail.TokenSourceConfig{
				Credentials: fake.credentials(),
				Token:       tc.token,
				HTTPClient:  fake.server.Client(),
			}); err == nil {
				t.Error("NewTokenSource returned no error")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// the cached token file
// ---------------------------------------------------------------------------

// The cached token file must be mode 0600.
func TestSaveTokenIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", ".gmail-token.json")

	if err := gmail.SaveToken(path, &gmail.Token{
		RefreshToken: testRefreshToken,
		AccessToken:  testAccessToken,
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour),
		Scope:        gmail.ScopeReadonly + " " + gmail.ScopeInsert,
	}); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %04o, want 0600 (it holds a long-lived mailbox credential)", perm)
	}

	loaded, err := gmail.LoadToken(path, "")
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if loaded.RefreshToken != testRefreshToken || loaded.AccessToken != testAccessToken {
		t.Errorf("round-tripped token = %+v, want the saved one", loaded)
	}
	if !strings.Contains(loaded.Scope, gmail.ScopeReadonly) {
		t.Errorf("round-tripped scope = %q, want it to include %q", loaded.Scope, gmail.ScopeReadonly)
	}
}

// Rewriting an existing token must narrow it: os.WriteFile leaves the mode of
// an existing file alone, so a file that was once world-readable would stay so.
func TestSaveTokenNarrowsAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".gmail-token.json")
	if err := os.WriteFile(path, []byte(`{"refresh_token":"old"}`), 0o644); err != nil {
		t.Fatalf("seed a world-readable token file: %v", err)
	}

	if err := gmail.SaveToken(path, &gmail.Token{RefreshToken: testRefreshToken}); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %04o after rewrite, want 0600", perm)
	}
}

// A missing token file must name the one-time command that creates it, rather
// than surfacing a bare ENOENT during a run.
func TestLoadTokenMissingFileNamesTheAuthCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), gmail.DefaultTokenPath)

	_, err := gmail.LoadToken(path, "")
	if err == nil {
		t.Fatal("LoadToken on a missing file returned no error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error = %v, want it to wrap os.ErrNotExist", err)
	}
	if !strings.Contains(err.Error(), "gmail-auth") {
		t.Errorf("error = %q, want it to name `make gmail-auth`", err.Error())
	}
}

// A token file holding no refresh token is worse than none: it would authorize
// for an hour and then fail with no way to renew.
func TestLoadTokenRejectsFileWithoutRefreshToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".gmail-token.json")
	if err := os.WriteFile(path, []byte(`{"access_token":"ya29.only"}`), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	if _, err := gmail.LoadToken(path, ""); err == nil {
		t.Error("LoadToken on a file with no refresh token returned no error")
	}
}

// ---------------------------------------------------------------------------
// the one-time authorization flow
// ---------------------------------------------------------------------------

// The consent URL carries PKCE (S256), a state value, and
// access_type=offline — the last is what makes Google issue a refresh token at
// all.
func TestAuthCodeURLCarriesPKCEAndOfflineAccess(t *testing.T) {
	creds := &gmail.Credentials{
		ClientID:     testClientID,
		ClientSecret: testClientSecret,
		AuthURI:      "https://accounts.google.com/o/oauth2/auth",
		TokenURI:     "https://oauth2.googleapis.com/token",
	}
	pkce, err := gmail.NewPKCE()
	if err != nil {
		t.Fatalf("NewPKCE: %v", err)
	}
	state, err := gmail.RandomState()
	if err != nil {
		t.Fatalf("RandomState: %v", err)
	}
	if state == "" {
		t.Error("RandomState returned an empty state; it is the anti-forgery check")
	}
	if pkce.Verifier == "" || pkce.Challenge == "" || pkce.Verifier == pkce.Challenge {
		t.Errorf("PKCE = %+v, want a verifier and a distinct S256 challenge", pkce)
	}

	raw := creds.AuthCodeURL("http://127.0.0.1:8765/callback", state, pkce,
		[]string{gmail.ScopeReadonly, gmail.ScopeInsert})
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse consent URL: %v", err)
	}
	query := parsed.Query()

	for field, want := range map[string]string{
		"client_id":             testClientID,
		"redirect_uri":          "http://127.0.0.1:8765/callback",
		"response_type":         "code",
		"state":                 state,
		"code_challenge":        pkce.Challenge,
		"code_challenge_method": "S256",
		"access_type":           "offline",
	} {
		if got := query.Get(field); got != want {
			t.Errorf("consent URL %s = %q, want %q", field, got, want)
		}
	}
	scope := query.Get("scope")
	for _, want := range []string{gmail.ScopeReadonly, gmail.ScopeInsert} {
		if !strings.Contains(scope, want) {
			t.Errorf("consent URL scope = %q, want it to include %q", scope, want)
		}
	}
	// The verifier is the secret half of PKCE; it is never sent to the
	// authorization endpoint.
	if strings.Contains(raw, pkce.Verifier) {
		t.Error("consent URL leaks the PKCE verifier")
	}
}

func TestExchangeCodeSendsVerifierAndReturnsRefreshToken(t *testing.T) {
	fake := newFakeOAuth(t, oauthResponse{body: `{
	  "access_token":"ya29.first-access-token",
	  "refresh_token":"1//first-refresh-token",
	  "expires_in":3599,
	  "scope":"https://www.googleapis.com/auth/gmail.readonly https://www.googleapis.com/auth/gmail.insert",
	  "token_type":"Bearer"
	}`})
	pkce, err := gmail.NewPKCE()
	if err != nil {
		t.Fatalf("NewPKCE: %v", err)
	}

	token, err := gmail.ExchangeCode(context.Background(), fake.credentials(),
		"4/authorization-code", "http://127.0.0.1:8765/callback", pkce, fake.server.Client())
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if token.RefreshToken != "1//first-refresh-token" {
		t.Errorf("refresh token = %q, want the exchanged one", token.RefreshToken)
	}
	if token.AccessToken != "ya29.first-access-token" {
		t.Errorf("access token = %q, want the exchanged one", token.AccessToken)
	}
	if !token.Expiry.After(time.Now().Add(50 * time.Minute)) {
		t.Errorf("expiry = %s, want roughly an hour out", token.Expiry)
	}

	exchanges := fake.exchanges()
	if len(exchanges) != 1 {
		t.Fatalf("token exchanges = %d, want 1", len(exchanges))
	}
	for field, want := range map[string]string{
		"grant_type":    "authorization_code",
		"code":          "4/authorization-code",
		"redirect_uri":  "http://127.0.0.1:8765/callback",
		"code_verifier": pkce.Verifier,
		"client_id":     testClientID,
	} {
		if got := exchanges[0].Get(field); got != want {
			t.Errorf("exchange form %s = %q, want %q", field, got, want)
		}
	}
}

// A code exchange that yields no refresh token leaves an authorization that
// dies in an hour; failing loudly is what sends the operator back through
// consent instead of into a mystery 401 mid-run.
func TestExchangeCodeRejectsMissingRefreshToken(t *testing.T) {
	fake := newFakeOAuth(t, oauthResponse{body: `{
	  "access_token":"ya29.only","expires_in":3599,"token_type":"Bearer"
	}`})
	pkce, err := gmail.NewPKCE()
	if err != nil {
		t.Fatalf("NewPKCE: %v", err)
	}

	if _, err := gmail.ExchangeCode(context.Background(), fake.credentials(),
		"4/authorization-code", "http://127.0.0.1:8765/callback", pkce,
		fake.server.Client()); err == nil {
		t.Fatal("ExchangeCode returned no error for a response without a refresh token")
	}
}

// ---------------------------------------------------------------------------
// credential file
// ---------------------------------------------------------------------------

func TestLoadCredentials(t *testing.T) {
	dir := t.TempDir()

	desktop := filepath.Join(dir, "installed.json")
	if err := os.WriteFile(desktop, []byte(`{"installed":{
	  "client_id":"`+testClientID+`",
	  "client_secret":"`+testClientSecret+`",
	  "auth_uri":"https://accounts.google.com/o/oauth2/auth",
	  "token_uri":"https://oauth2.googleapis.com/token"
	}}`), 0o600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}

	creds, err := gmail.LoadCredentials(desktop)
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	if creds.ClientID != testClientID || creds.ClientSecret != testClientSecret {
		t.Errorf("credentials = %+v, want the installed client", creds)
	}
	if creds.TokenURI != "https://oauth2.googleapis.com/token" {
		t.Errorf("token URI = %q, want Google's token endpoint", creds.TokenURI)
	}

	bad := filepath.Join(dir, "service-account.json")
	if err := os.WriteFile(bad, []byte(`{"type":"service_account"}`), 0o600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
	if _, err := gmail.LoadCredentials(bad); err == nil {
		t.Error("LoadCredentials accepted a file with neither an installed nor a web client")
	}

	if _, err := gmail.LoadCredentials(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("LoadCredentials accepted a missing file")
	}
}

// The default cached-token path is the gitignored one .env.example documents.
func TestDefaultTokenPath(t *testing.T) {
	if gmail.DefaultTokenPath != ".gmail-token.json" {
		t.Errorf("DefaultTokenPath = %q, want the gitignored .gmail-token.json", gmail.DefaultTokenPath)
	}
}

// A token file must never be readable as JSON that anyone but the owner can
// open; this belt-and-braces check reads the bytes back to prove the refresh
// token really is what was persisted.
func TestSaveTokenPersistsExactly(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".gmail-token.json")
	token := &gmail.Token{
		RefreshToken: testRefreshToken,
		AccessToken:  testAccessToken,
		TokenType:    "Bearer",
		Expiry:       time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),
		Scope:        gmail.ScopeReadonly,
	}
	if err := gmail.SaveToken(path, token); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("token file is not JSON: %v", err)
	}
	if decoded["refresh_token"] != testRefreshToken {
		t.Errorf("refresh_token = %v, want %q", decoded["refresh_token"], testRefreshToken)
	}
}

// ---------------------------------------------------------------------------
// the token as an environment variable

// The demo mailbox's refresh token is written by an interactive flow — `make
// gmail-auth` opens a browser — which cannot run inside a container, while the
// hosts this deploys to may discard the filesystem on every deploy. The
// operator pastes the same JSON into GMAIL_TOKEN_JSON instead.
//
// Precedence is file-then-environment, deliberately: a developer's local
// re-authorization must take effect without anyone editing the environment.

const (
	envRefreshToken  = "1//env-pasted-refresh-token"
	fileRefreshToken = "1//file-cached-refresh-token"
)

func TestLoadTokenFallsBackToTheEnvironmentValue(t *testing.T) {
	tests := []struct {
		name string
		// fileContents is written to the token path; empty means no file at
		// all, which is the deployed case.
		fileContents string
		fallbackJSON string

		wantRefreshToken string
		wantErrContains  []string
	}{
		{
			// The deployed case: nothing on disk, the token in the
			// environment.
			name:             "no file, a valid GMAIL_TOKEN_JSON",
			fallbackJSON:     `{"refresh_token":"` + envRefreshToken + `","token_type":"Bearer"}`,
			wantRefreshToken: envRefreshToken,
		},
		{
			// The workstation case: `make gmail-auth` just wrote a fresh
			// token, and it wins over whatever is in the environment.
			name:             "the file wins when both are present",
			fileContents:     `{"refresh_token":"` + fileRefreshToken + `"}`,
			fallbackJSON:     `{"refresh_token":"` + envRefreshToken + `"}`,
			wantRefreshToken: fileRefreshToken,
		},
		{
			// A truncated paste must say which variable to fix, not report an
			// anonymous parse failure that looks like a corrupt file.
			name:            "a malformed GMAIL_TOKEN_JSON names the variable",
			fallbackJSON:    `{"refresh_token": "oops`,
			wantErrContains: []string{"GMAIL_TOKEN_JSON"},
		},
		{
			// Valid JSON carrying no refresh token is worse than none: it
			// would authorize for an hour and then fail with no way to renew.
			name:            "GMAIL_TOKEN_JSON without a refresh token names the variable",
			fallbackJSON:    `{"access_token":"ya29.only"}`,
			wantErrContains: []string{"GMAIL_TOKEN_JSON", "refresh token"},
		},
		{
			// Neither source: the error has to name both ways in, because
			// which one applies depends on where this is running.
			name:            "neither a file nor GMAIL_TOKEN_JSON names both ways in",
			wantErrContains: []string{"gmail-auth", "GMAIL_TOKEN_JSON"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".gmail-token.json")
			if tt.fileContents != "" {
				if err := os.WriteFile(path, []byte(tt.fileContents), 0o600); err != nil {
					t.Fatalf("write token file: %v", err)
				}
			}

			token, err := gmail.LoadToken(path, tt.fallbackJSON)

			if len(tt.wantErrContains) > 0 {
				if err == nil {
					t.Fatalf("LoadToken = %+v, want an error", token)
				}
				for _, want := range tt.wantErrContains {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not mention %q", err, want)
					}
				}
				// An error message about a token must never carry the token.
				if strings.Contains(err.Error(), envRefreshToken) {
					t.Errorf("error %q carries the refresh token", err)
				}
				return
			}

			if err != nil {
				t.Fatalf("LoadToken error = %v, want nil", err)
			}
			if token.RefreshToken != tt.wantRefreshToken {
				t.Errorf("refresh token = %q, want %q", token.RefreshToken, tt.wantRefreshToken)
			}
		})
	}
}

// The environment value is specified to be read when the token path is absent
// OR UNREADABLE. A read-only secret mount that the process cannot open, or a
// path that names something other than a readable file, is the case this
// covers: the durable value in the environment must still get the demo mailbox
// authenticated rather than the boot failing with an unusable file.
func TestLoadTokenFallsBackWhenTheFileIsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unreadable file is still readable, so the case cannot be staged")
	}
	path := filepath.Join(t.TempDir(), ".gmail-token.json")
	if err := os.WriteFile(path, []byte(`{"refresh_token":"`+fileRefreshToken+`"}`), 0o000); err != nil {
		t.Fatalf("write an unreadable token file: %v", err)
	}

	token, err := gmail.LoadToken(path, `{"refresh_token":"`+envRefreshToken+`"}`)
	if err != nil {
		t.Fatalf("LoadToken over an unreadable file = %v, want the GMAIL_TOKEN_JSON value", err)
	}
	if token.RefreshToken != envRefreshToken {
		t.Errorf("refresh token = %q, want the environment's %q", token.RefreshToken, envRefreshToken)
	}
}
