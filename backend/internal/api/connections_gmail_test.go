package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"cortex/internal/api"
	"cortex/internal/auth"
	"cortex/internal/keys"
	"cortex/internal/tools/gmail"
)

// TEST-8.3 — Connect Gmail via OAuth (REQ-8.3), against a fake Google.
//
// The connect leg must ask for exactly gmail.readonly with
// access_type=offline and prompt=consent; the callback must validate state,
// prove the token reads its mailbox before storing anything, store the
// refresh token encrypted, and record the mailbox address as identity. A
// declined consent or a missing refresh token stores nothing. (The
// refresh-failure → status=error → gmail-drops-out-of-the-registry half of
// TEST-8.3 lives with the RegistryBuilder tests in internal/connections.)

const (
	testGmailRefreshToken = "1//fake-refresh-token-from-consent-0042"
	testGmailAccessToken  = "ya29.fake-access-token-from-exchange"
	testMailboxAddress    = "satyam.owner@gmail.example"
)

// fakeGoogleGmail fakes the two remote surfaces the connect flow touches: the
// token endpoint (code exchange) and the Gmail profile endpoint (mailbox
// validation).
type fakeGoogleGmail struct {
	server *httptest.Server

	mu sync.Mutex
	// omitRefreshToken scripts Google withholding the refresh token (a
	// lingering prior grant).
	omitRefreshToken bool
	tokenCalls       int
	profileCalls     int
	exchangeForms    []url.Values
}

func newFakeGoogleGmail(t *testing.T) *fakeGoogleGmail {
	t.Helper()
	f := &fakeGoogleGmail{}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse token form: %v", err)
		}
		f.mu.Lock()
		f.tokenCalls++
		f.exchangeForms = append(f.exchangeForms, r.PostForm)
		omit := f.omitRefreshToken
		f.mu.Unlock()

		body := map[string]any{
			"access_token": testGmailAccessToken,
			"token_type":   "Bearer",
			"expires_in":   3600,
			"scope":        gmail.ScopeReadonly,
		}
		if !omit {
			body["refresh_token"] = testGmailRefreshToken
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("/gmail/v1/users/me/profile", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.profileCalls++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+testGmailAccessToken {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":401,"status":"UNAUTHENTICATED","message":"invalid access token"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"emailAddress":"` + testMailboxAddress + `"}`))
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// newGmailConnectRouter wires a router whose Google and Gmail endpoints are
// the fake's.
func newGmailConnectRouter(t *testing.T, db api.DB, g *fakeGoogleGmail) http.Handler {
	t.Helper()
	return api.NewRouter(withTestAuth(api.Deps{
		DB:       db,
		Enqueuer: &stubEnqueuer{},
		Model:    testModel,
		Logger:   discardLogger(),
		OIDC: &auth.OIDC{
			Client: auth.Client{
				ID:       testGoogleClientID,
				Secret:   "test-client-secret",
				AuthURL:  g.server.URL + "/auth",
				TokenURL: g.server.URL + "/token",
			},
			Issuer:      "https://accounts.google.com",
			RedirectURI: "http://localhost:8080/api/auth/google/callback",
		},
		Keys:               newTestKeysService(t, db),
		Connections:        newConnectionsService(t, db),
		FrontendOrigin:     testFrontendOrigin,
		ConnectRedirectURI: "http://localhost:8080/api/connections/gmail/callback",
		GmailBaseURL:       g.server.URL,
	}))
}

// startGmailConnect drives GET /api/connections/gmail/connect and returns the
// state Google would echo back plus the connect-state cookie.
func startGmailConnect(t *testing.T, h http.Handler) (string, *http.Cookie, *url.URL) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localRequest(http.MethodGet, "/api/connections/gmail/connect", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("connect status = %d, want 302 (body %q)", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse consent redirect %q: %v", rec.Header().Get("Location"), err)
	}
	state := loc.Query().Get("state")
	if state == "" {
		t.Fatal("consent redirect carries no state")
	}
	var stateCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.ConnectStateCookieName {
			stateCookie = c
		}
	}
	if stateCookie == nil {
		t.Fatal("connect set no state cookie")
	}
	return state, stateCookie, loc
}

// finishGmailConnect drives the callback with the given query and cookie.
func finishGmailConnect(t *testing.T, h http.Handler, query string, stateCookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := localRequest(http.MethodGet, "/api/connections/gmail/callback?"+query, nil)
	if stateCookie != nil {
		req.AddCookie(stateCookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestGmailConnectAsksForReadonlyScopeOfflineConsent(t *testing.T) {
	g := newFakeGoogleGmail(t)
	h := newGmailConnectRouter(t, &stubDB{}, g)

	state, stateCookie, loc := startGmailConnect(t, h)

	q := loc.Query()
	if got := q.Get("scope"); got != gmail.ScopeReadonly {
		t.Errorf("scope = %q, want exactly %q (incremental — login scopes stay out)", got, gmail.ScopeReadonly)
	}
	if got := q.Get("access_type"); got != "offline" {
		t.Errorf("access_type = %q, want offline (no refresh token without it)", got)
	}
	if got := q.Get("prompt"); got != "consent" {
		t.Errorf("prompt = %q, want consent (forces the refresh token to be re-issued)", got)
	}
	if got := q.Get("redirect_uri"); got != "http://localhost:8080/api/connections/gmail/callback" {
		t.Errorf("redirect_uri = %q, want the connect callback", got)
	}
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		t.Errorf("consent URL lacks S256 PKCE: challenge %q method %q",
			q.Get("code_challenge"), q.Get("code_challenge_method"))
	}

	if !stateCookie.HttpOnly {
		t.Error("connect state cookie is not HttpOnly")
	}
	if stateCookie.Path != "/api/connections" {
		t.Errorf("connect state cookie path = %q, want /api/connections (must not collide with the login flow)", stateCookie.Path)
	}
	if !strings.HasPrefix(stateCookie.Value, state+".") {
		t.Error("connect state cookie does not carry the redirect's state value")
	}
}

func TestGmailCallbackRejectsAStateMismatch(t *testing.T) {
	pool := testPool(t)
	g := newFakeGoogleGmail(t)
	h := newGmailConnectRouter(t, pool, g)

	_, stateCookie, _ := startGmailConnect(t, h)

	rec := finishGmailConnect(t, h, "state=attacker-forged-state&code=some-code", stateCookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	if g.tokenCalls != 0 {
		t.Errorf("a forged state still reached the token endpoint %d time(s)", g.tokenCalls)
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM user_connections`); n != 0 {
		t.Errorf("stored connections after a forged state = %d, want 0", n)
	}

	// No cookie at all is refused too.
	rec = finishGmailConnect(t, h, "state=whatever&code=some-code", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status without the state cookie = %d, want 400", rec.Code)
	}
}

func TestGmailCallbackStoresTheRefreshTokenEncryptedWithMailboxIdentity(t *testing.T) {
	pool := testPool(t)
	g := newFakeGoogleGmail(t)
	h := newGmailConnectRouter(t, pool, g)

	state, stateCookie, _ := startGmailConnect(t, h)
	rec := finishGmailConnect(t, h, "state="+url.QueryEscape(state)+"&code=consent-code", stateCookie)

	if rec.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want 302 (body %q)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != testFrontendOrigin+"/connections?connected=gmail" {
		t.Errorf("callback redirects to %q, want the connections page success banner", got)
	}
	if g.tokenCalls != 1 {
		t.Errorf("token endpoint calls = %d, want exactly 1 exchange", g.tokenCalls)
	}
	if g.profileCalls != 1 {
		t.Errorf("profile calls = %d, want 1 — the mailbox is validated before storing", g.profileCalls)
	}

	// The refresh token appears in no response.
	if strings.Contains(rec.Body.String(), testGmailRefreshToken) ||
		strings.Contains(rec.Header().Get("Location"), testGmailRefreshToken) {
		t.Error("the callback response carries the refresh token")
	}

	// Stored encrypted, round-trippable, with the mailbox address as identity.
	var ciphertext []byte
	var identityJSON, status string
	if err := pool.QueryRow(context.Background(),
		`SELECT credentials_ciphertext, identity::text, status FROM user_connections WHERE source = 'gmail'`).
		Scan(&ciphertext, &identityJSON, &status); err != nil {
		t.Fatalf("read stored gmail connection: %v", err)
	}
	if status != "active" {
		t.Errorf("stored status = %q, want active", status)
	}
	if strings.Contains(string(ciphertext), testGmailRefreshToken) {
		t.Fatal("the database holds the plaintext refresh token")
	}
	cipher, err := keys.NewCipher(testKeyEncryptionSecret)
	if err != nil {
		t.Fatalf("build cipher: %v", err)
	}
	plain, err := cipher.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("decrypt stored ciphertext: %v", err)
	}
	var creds struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal([]byte(plain), &creds); err != nil {
		t.Fatalf("decode decrypted credentials: %v", err)
	}
	if creds.RefreshToken != testGmailRefreshToken {
		t.Errorf("decrypted refresh token = %q, want the one Google issued", creds.RefreshToken)
	}
	if strings.Contains(identityJSON, testGmailRefreshToken) {
		t.Errorf("identity %q carries the refresh token", identityJSON)
	}
	if !strings.Contains(identityJSON, testMailboxAddress) {
		t.Errorf("identity %q does not record the mailbox address", identityJSON)
	}

	// The overview now shows gmail connected, mode user — token still nowhere.
	getRec, overview := getConnections(t, h)
	if overview.Mode != "user" || overview.Sources["gmail"].Status != "connected" {
		t.Errorf("overview = mode %q gmail %q, want user/connected", overview.Mode, overview.Sources["gmail"].Status)
	}
	if overview.Sources["gmail"].Identity["email"] != testMailboxAddress {
		t.Errorf("gmail identity = %v, want the mailbox address", overview.Sources["gmail"].Identity)
	}
	if strings.Contains(getRec.Body.String(), testGmailRefreshToken) {
		t.Errorf("GET /api/connections echoes the refresh token: %q", getRec.Body.String())
	}
}

func TestGmailCallbackWithoutARefreshTokenStoresNothing(t *testing.T) {
	pool := testPool(t)
	g := newFakeGoogleGmail(t)
	g.omitRefreshToken = true
	h := newGmailConnectRouter(t, pool, g)

	state, stateCookie, _ := startGmailConnect(t, h)
	rec := finishGmailConnect(t, h, "state="+url.QueryEscape(state)+"&code=consent-code", stateCookie)

	if rec.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want 302 (body %q)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != testFrontendOrigin+"/connections?error=no_refresh_token" {
		t.Errorf("callback redirects to %q, want the no_refresh_token banner", got)
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM user_connections`); n != 0 {
		t.Errorf("stored connections without a refresh token = %d, want 0", n)
	}
}

func TestGmailCallbackWhenTheUserDeclinesConsentStoresNothing(t *testing.T) {
	pool := testPool(t)
	g := newFakeGoogleGmail(t)
	h := newGmailConnectRouter(t, pool, g)

	state, stateCookie, _ := startGmailConnect(t, h)
	rec := finishGmailConnect(t, h, "state="+url.QueryEscape(state)+"&error=access_denied", stateCookie)

	if rec.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want 302 (body %q)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != testFrontendOrigin+"/connections?error=access_denied" {
		t.Errorf("callback redirects to %q, want the access_denied banner", got)
	}
	if g.tokenCalls != 0 {
		t.Errorf("a declined consent still reached the token endpoint %d time(s)", g.tokenCalls)
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM user_connections`); n != 0 {
		t.Errorf("stored connections after a declined consent = %d, want 0", n)
	}
}
