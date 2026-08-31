package api_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"cortex/internal/api"
	"cortex/internal/auth"
	"cortex/internal/keys"
)

// Google OAuth login, tested against an httptest fake Google.
//
// The fake serves the two endpoints the callback consults: a JWKS set holding
// the test's own RSA public key, and a token endpoint returning an ID token the
// test signed itself. The whole contract is asserted from the outside:
// state mismatch and bad tokens rejected, the user upserted, the session cookie
// HttpOnly with only its SHA-256 in the database, the allowlist enforced,
// logout revoking, /healthz open, and the bearer token accepted.

// testFrontendOrigin is declared in cors_test.go.
const (
	testGoogleClientID = "test-client.apps.googleusercontent.com"

	// testKeyEncryptionSecret is a valid LLM_KEY_ENCRYPTION_SECRET (64 hex
	// chars); the settings tests share it.
	testKeyEncryptionSecret = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
)

// newTestKeysService builds the keys.Service the router Deps carry.
func newTestKeysService(t *testing.T, db api.DB) *keys.Service {
	t.Helper()
	cipher, err := keys.NewCipher(testKeyEncryptionSecret)
	if err != nil {
		t.Fatalf("build cipher: %v", err)
	}
	return keys.NewService(db, cipher)
}

// sessionRequest is a request carrying a session cookie and no bearer token —
// the browser's shape.
func sessionRequest(method, target string, body io.Reader, cookie *http.Cookie) *http.Request {
	req := anonymousRequest(method, target, body)
	req.AddCookie(cookie)
	return req
}

// ---------------------------------------------------------------------------
// fake Google
// ---------------------------------------------------------------------------

// fakeGoogle is an httptest stand-in for Google's OIDC surface: /jwks serving
// the test key, /token returning a scripted id_token, /auth never called (the
// test skips the consent screen by calling the callback directly).
type fakeGoogle struct {
	t      *testing.T
	server *httptest.Server
	key    *rsa.PrivateKey
	kid    string

	// idToken, when set, is handed out by the token endpoint verbatim — the
	// bad-token tests script it. When empty, the endpoint signs claims at
	// exchange time with this login attempt's nonce injected, like Google.
	idToken string
	claims  map[string]any
	// nonce is captured from the login redirect by startLogin.
	nonce string
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	g := &fakeGoogle{t: t, key: key, kid: "test-kid-1"}

	mux := http.NewServeMux()
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{
				"kty": "RSA",
				"kid": g.kid,
				"use": "sig",
				"alg": "RS256",
				"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			}},
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		idToken := g.idToken
		if idToken == "" && g.claims != nil {
			claims := make(map[string]any, len(g.claims)+1)
			for k, v := range g.claims {
				claims[k] = v
			}
			claims["nonce"] = g.nonce
			idToken = signJWT(g.t, g.key, g.kid, claims)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "test-access-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
			"id_token":     idToken,
		})
	})
	g.server = httptest.NewServer(mux)
	t.Cleanup(g.server.Close)
	return g
}

// oidc builds the auth.OIDC pointed at the fake.
func (g *fakeGoogle) oidc() *auth.OIDC {
	return &auth.OIDC{
		Client: auth.Client{
			ID:       testGoogleClientID,
			Secret:   "test-client-secret",
			AuthURL:  g.server.URL + "/auth",
			TokenURL: g.server.URL + "/token",
		},
		JWKS:        auth.NewJWKSCache(g.server.URL+"/jwks", g.server.Client()),
		Issuer:      "https://accounts.google.com",
		RedirectURI: "http://localhost:8080/api/auth/google/callback",
	}
}

// signJWT signs an RS256 JWT by hand — stdlib crypto only, mirroring what
// Google does rather than what internal/auth verifies.
func signJWT(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	headerJSON, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": kid})
	if err != nil {
		t.Fatalf("marshal JWT header: %v", err)
	}
	payloadJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal JWT claims: %v", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) +
		"." + base64.RawURLEncoding.EncodeToString(payloadJSON)
	sum := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

// googleClaims is a valid ID-token claim set for this client; tests override
// individual claims to make it invalid.
func googleClaims(email string) map[string]any {
	return map[string]any{
		"iss":            "https://accounts.google.com",
		"aud":            testGoogleClientID,
		"exp":            time.Now().Add(time.Hour).Unix(),
		"sub":            "google-sub-1234567890",
		"email":          email,
		"email_verified": true,
		"name":           "Ada Lovelace",
		"picture":        "https://example.com/ada.png",
	}
}

// newOIDCRouter builds a router wired to the fake Google.
func newOIDCRouter(t *testing.T, db api.DB, oidc *auth.OIDC, allowed []string) http.Handler {
	t.Helper()
	return api.NewRouter(withTestAuth(api.Deps{
		DB:             db,
		Enqueuer:       &stubEnqueuer{},
		Model:          testModel,
		Logger:         discardLogger(),
		OIDC:           oidc,
		Keys:           newTestKeysService(t, db),
		FrontendOrigin: testFrontendOrigin,
		AllowedEmails:  allowed,
	}))
}

// startLogin drives GET /api/auth/google/login and returns the state Google
// would echo back plus the state cookie the browser would hold. The redirect's
// nonce is captured onto the fake, which injects it into lazily-signed tokens
// the way Google injects it into real ones.
func startLogin(t *testing.T, h http.Handler, g *fakeGoogle) (string, *http.Cookie) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, anonymousRequest(http.MethodGet, "/api/auth/google/login", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("login status = %d, want 302 (body %q)", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse login redirect %q: %v", rec.Header().Get("Location"), err)
	}
	state := loc.Query().Get("state")
	if state == "" {
		t.Fatal("login redirect carries no state")
	}
	g.nonce = loc.Query().Get("nonce")
	if g.nonce == "" {
		t.Fatal("login redirect carries no nonce")
	}
	var stateCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.StateCookieName {
			stateCookie = c
		}
	}
	if stateCookie == nil {
		t.Fatal("login set no state cookie")
	}
	if !stateCookie.HttpOnly {
		t.Error("state cookie is not HttpOnly")
	}
	return state, stateCookie
}

// finishLogin drives the callback with the given state parameter and cookie.
func finishLogin(t *testing.T, h http.Handler, state string, stateCookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/auth/google/callback?state=" + url.QueryEscape(state) + "&code=test-auth-code"
	req := anonymousRequest(http.MethodGet, target, nil)
	if stateCookie != nil {
		req.AddCookie(stateCookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// sessionCookieFrom returns the cortex_session cookie a response set, or nil.
func sessionCookieFrom(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookieName && c.MaxAge >= 0 && c.Value != "" {
			return c
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// The OAuth callback flow
// ---------------------------------------------------------------------------

func TestGoogleCallbackSignsInAndStoresOnlyTheSessionHash(t *testing.T) {
	pool := testPool(t)
	g := newFakeGoogle(t)
	h := newOIDCRouter(t, pool, g.oidc(), nil)
	g.claims = googleClaims("ada@example.com")

	state, stateCookie := startLogin(t, h, g)
	rec := finishLogin(t, h, state, stateCookie)

	if rec.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want 302 (body %q)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != testFrontendOrigin+"/" {
		t.Errorf("callback redirected to %q, want %q", loc, testFrontendOrigin+"/")
	}

	// The user is upserted with the identity from the verified token.
	if n := queryInt(t, pool,
		`SELECT count(*) FROM users WHERE email = 'ada@example.com'
		   AND google_sub = 'google-sub-1234567890' AND name = 'Ada Lovelace'`); n != 1 {
		t.Errorf("upserted user rows = %d, want 1", n)
	}

	cookie := sessionCookieFrom(rec)
	if cookie == nil {
		t.Fatal("callback set no session cookie")
	}
	if !cookie.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
	if cookie.Path != "/" {
		t.Errorf("session cookie path = %q, want /", cookie.Path)
	}

	// Hash-only in the database: the row is found by SHA-256 of the cookie
	// value, and the raw token itself is stored nowhere.
	if n := queryInt(t, pool, `SELECT count(*) FROM sessions WHERE token_hash = $1`,
		auth.HashSessionToken(cookie.Value)); n != 1 {
		t.Errorf("sessions matching SHA-256(cookie) = %d, want 1", n)
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM sessions WHERE token_hash = $1`,
		[]byte(cookie.Value)); n != 0 {
		t.Errorf("sessions storing the raw cookie token = %d, want 0", n)
	}

	// The cookie signs in: /api/auth/me identifies the user without a bearer.
	me := httptest.NewRecorder()
	h.ServeHTTP(me, sessionRequest(http.MethodGet, "/api/auth/me", nil, cookie))
	if me.Code != http.StatusOK {
		t.Fatalf("GET /api/auth/me with session = %d, want 200 (body %q)", me.Code, me.Body.String())
	}
	var got struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if err := json.Unmarshal(me.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode /api/auth/me: %v", err)
	}
	if got.Email != "ada@example.com" || got.Name != "Ada Lovelace" {
		t.Errorf("me = %+v, want the signed-in identity", got)
	}
}

// A state parameter that does not match the cookie — or a missing/mangled
// cookie — is CSRF and must die at the door.
func TestGoogleCallbackRejectsStateProblems(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mangle func(state string, cookie *http.Cookie) (string, *http.Cookie)
	}{
		{
			name: "state parameter mismatch",
			mangle: func(_ string, cookie *http.Cookie) (string, *http.Cookie) {
				return "attacker-forged-state", cookie
			},
		},
		{
			name: "state cookie missing",
			mangle: func(state string, _ *http.Cookie) (string, *http.Cookie) {
				return state, nil
			},
		},
		{
			name: "state cookie malformed",
			mangle: func(state string, cookie *http.Cookie) (string, *http.Cookie) {
				return state, &http.Cookie{Name: cookie.Name, Value: "no-verifier-part"}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			g := newFakeGoogle(t)
			// stubDB: a state failure must be decided before any database work.
			h := newOIDCRouter(t, &stubDB{}, g.oidc(), nil)
			g.claims = googleClaims("ada@example.com")

			state, stateCookie := startLogin(t, h, g)
			badState, badCookie := tt.mangle(state, stateCookie)
			rec := finishLogin(t, h, badState, badCookie)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
			}
			if c := sessionCookieFrom(rec); c != nil {
				t.Errorf("a rejected callback set a session cookie %q", c.Value)
			}
		})
	}
}

// An ID token that fails verification — wrong signature, wrong audience,
// expired, wrong issuer, or not RS256 — must never create a session.
func TestGoogleCallbackRejectsBadIDTokens(t *testing.T) {
	tests := []struct {
		name string
		mint func(t *testing.T, g *fakeGoogle) string
	}{
		{
			name: "signature from a key not in the JWKS",
			mint: func(t *testing.T, g *fakeGoogle) string {
				rogue, err := rsa.GenerateKey(rand.Reader, 2048)
				if err != nil {
					t.Fatalf("generate rogue key: %v", err)
				}
				return signJWT(t, rogue, g.kid, googleClaims("ada@example.com"))
			},
		},
		{
			name: "audience is another client",
			mint: func(t *testing.T, g *fakeGoogle) string {
				claims := googleClaims("ada@example.com")
				claims["aud"] = "someone-else.apps.googleusercontent.com"
				return signJWT(t, g.key, g.kid, claims)
			},
		},
		{
			name: "expired beyond clock skew",
			mint: func(t *testing.T, g *fakeGoogle) string {
				claims := googleClaims("ada@example.com")
				claims["exp"] = time.Now().Add(-2 * time.Hour).Unix()
				return signJWT(t, g.key, g.kid, claims)
			},
		},
		{
			name: "issuer is not Google",
			mint: func(t *testing.T, g *fakeGoogle) string {
				claims := googleClaims("ada@example.com")
				claims["iss"] = "https://evil.example.com"
				return signJWT(t, g.key, g.kid, claims)
			},
		},
		{
			// Accounts are keyed by email, so an unverified email claim is an
			// account-takeover primitive: Google asserts addresses it has not
			// verified for non-Gmail signups, and accepting one would let that
			// assertion capture an existing user's row — and the stored key
			// funding their runs.
			name: "email_verified is false",
			mint: func(t *testing.T, g *fakeGoogle) string {
				claims := googleClaims("ada@example.com")
				claims["email_verified"] = false
				return signJWT(t, g.key, g.kid, claims)
			},
		},
		{
			// A valid token from a different login attempt: signature, aud, exp
			// all pass, but the nonce does not match this attempt's cookie —
			// the replay the nonce exists to catch.
			name: "nonce from another login attempt",
			mint: func(t *testing.T, g *fakeGoogle) string {
				claims := googleClaims("ada@example.com")
				claims["nonce"] = "attacker-replayed-nonce"
				return signJWT(t, g.key, g.kid, claims)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := testPool(t)
			g := newFakeGoogle(t)
			h := newOIDCRouter(t, pool, g.oidc(), nil)
			g.idToken = tt.mint(t, g)

			state, stateCookie := startLogin(t, h, g)
			rec := finishLogin(t, h, state, stateCookie)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401 (body %q)", rec.Code, rec.Body.String())
			}
			if c := sessionCookieFrom(rec); c != nil {
				t.Errorf("a rejected token produced a session cookie %q", c.Value)
			}
			if n := queryInt(t, pool, `SELECT count(*) FROM sessions`); n != 0 {
				t.Errorf("sessions = %d after a rejected token, want 0", n)
			}
			if n := queryInt(t, pool, `SELECT count(*) FROM users`); n != 0 {
				t.Errorf("users = %d after a rejected token, want 0", n)
			}
		})
	}
}

// AUTH_ALLOWED_EMAILS restricts sign-in: an authenticated stranger is bounced
// to the login page with a named reason and leaves no trace; an allowed
// address (any case) signs in.
func TestGoogleCallbackEnforcesTheAllowlist(t *testing.T) {
	allowed := []string{"ada@example.com"}

	t.Run("a stranger is refused", func(t *testing.T) {
		pool := testPool(t)
		g := newFakeGoogle(t)
		h := newOIDCRouter(t, pool, g.oidc(), allowed)
		g.claims = googleClaims("stranger@example.com")

		state, stateCookie := startLogin(t, h, g)
		rec := finishLogin(t, h, state, stateCookie)

		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302 (body %q)", rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != testFrontendOrigin+"/login?error=not_allowed" {
			t.Errorf("redirected to %q, want the not_allowed login page", loc)
		}
		if c := sessionCookieFrom(rec); c != nil {
			t.Errorf("a refused sign-in set a session cookie %q", c.Value)
		}
		if n := queryInt(t, pool, `SELECT count(*) FROM sessions`); n != 0 {
			t.Errorf("sessions = %d for a refused sign-in, want 0", n)
		}
		if n := queryInt(t, pool, `SELECT count(*) FROM users WHERE email = 'stranger@example.com'`); n != 0 {
			t.Errorf("a refused sign-in upserted the user anyway")
		}
	})

	t.Run("an allowed address signs in regardless of case", func(t *testing.T) {
		pool := testPool(t)
		g := newFakeGoogle(t)
		h := newOIDCRouter(t, pool, g.oidc(), allowed)
		g.claims = googleClaims("Ada@Example.com")

		state, stateCookie := startLogin(t, h, g)
		rec := finishLogin(t, h, state, stateCookie)

		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302 (body %q)", rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != testFrontendOrigin+"/" {
			t.Errorf("redirected to %q, want the app", loc)
		}
		if sessionCookieFrom(rec) == nil {
			t.Error("an allowed sign-in set no session cookie")
		}
	})
}

// Logout must revoke server-side: the same cookie is dead afterwards, because
// the session row is gone — not merely because the browser dropped the cookie.
func TestLogoutRevokesTheSession(t *testing.T) {
	pool := testPool(t)
	h := newOIDCRouter(t, pool, newFakeGoogle(t).oidc(), nil)
	cookie, _ := createSessionForEmail(t, pool, "ada@example.com")

	before := httptest.NewRecorder()
	h.ServeHTTP(before, sessionRequest(http.MethodGet, "/api/auth/me", nil, cookie))
	if before.Code != http.StatusOK {
		t.Fatalf("me before logout = %d, want 200 (body %q)", before.Code, before.Body.String())
	}

	logout := httptest.NewRecorder()
	h.ServeHTTP(logout, sessionRequest(http.MethodPost, "/api/auth/logout", nil, cookie))
	if logout.Code != http.StatusNoContent {
		t.Fatalf("logout = %d, want 204 (body %q)", logout.Code, logout.Body.String())
	}

	if n := queryInt(t, pool, `SELECT count(*) FROM sessions`); n != 0 {
		t.Errorf("sessions = %d after logout, want 0 (revocation must be server-side)", n)
	}

	after := httptest.NewRecorder()
	h.ServeHTTP(after, sessionRequest(http.MethodGet, "/api/auth/me", nil, cookie))
	if after.Code != http.StatusUnauthorized {
		t.Errorf("me after logout = %d, want 401", after.Code)
	}
}

// /healthz stays open — it is what the deploy's liveness probe hits, with no
// credentials to give.
func TestHealthzIsOpenWithoutCredentials(t *testing.T) {
	t.Parallel()

	h := api.NewRouter(withTestAuth(api.Deps{
		DB:       &stubDB{},
		Enqueuer: &stubEnqueuer{},
		Model:    testModel,
		Logger:   discardLogger(),
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, anonymousRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("anonymous /healthz = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
}

// Everything under /api (bar the two auth endpoints) is closed to anonymous
// callers: curl without auth gets 401 everywhere.
func TestAPIRejectsAnonymousRequests(t *testing.T) {
	t.Parallel()

	paths := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/conversations"},
		{http.MethodGet, "/api/auth/me"},
		{http.MethodPost, "/api/chat"},
		{http.MethodGet, "/api/documents"},
		{http.MethodPost, "/api/documents/refresh"},
		{http.MethodGet, "/api/settings/llm-key"},
		{http.MethodPost, "/api/admin/index"},
	}
	for _, tt := range paths {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			t.Parallel()
			db := &stubDB{}
			h := api.NewRouter(withTestAuth(api.Deps{
				DB:       db,
				Enqueuer: &stubEnqueuer{},
				Model:    testModel,
				Logger:   discardLogger(),
			}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, anonymousRequest(tt.method, tt.path, nil))
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401 (body %q)", rec.Code, rec.Body.String())
			}
			if db.begins != 0 {
				t.Errorf("an anonymous request opened %d transactions, want 0", db.begins)
			}
		})
	}
}

// The static bearer token (make index, scripts) is accepted and acts as the
// configured operator user; a wrong token is not.
func TestBearerTokenIsAcceptedAndVerified(t *testing.T) {
	pool := testPool(t)
	h := newOIDCRouter(t, pool, newFakeGoogle(t).oidc(), nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localRequest(http.MethodGet, "/api/auth/me", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("bearer /api/auth/me = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), devUserEmail) {
		t.Errorf("bearer identity = %q, want the operator user %q", rec.Body.String(), devUserEmail)
	}

	wrong := anonymousRequest(http.MethodGet, "/api/auth/me", nil)
	wrong.Header.Set("Authorization", "Bearer "+testAPIToken+"-wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, wrong)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong bearer = %d, want 401", rec.Code)
	}
}
