package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"cortex/internal/auth"
	"cortex/internal/keys"
	"cortex/internal/tools/gmail"
)

// Gmail scope escalation.
//
// Sending mail needs a scope reading mail does not, and the only thing that can
// grant it is a fresh consent screen. So enabling Gmail writes re-runs the
// consent flow asking for gmail.send *in addition to* gmail.readonly —
// incremental authorization — and the deciding fact afterwards is what Google
// says it granted, not what was requested. A consent screen lets a user tick
// some boxes and not others.
//
// The intent travels in the state cookie rather than in the callback URL, so a
// crafted callback cannot come back holding a broader grant than the request
// that started it.

// startGmailWriteConnect drives the connect leg with writes=1.
func startGmailWriteConnect(t *testing.T, h http.Handler, query string) (string, *http.Cookie, *url.URL) {
	t.Helper()
	target := "/api/connections/gmail/connect"
	if query != "" {
		target += "?" + query
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("connect status = %d, want 302 (body %q)", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse consent redirect: %v", err)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.ConnectStateCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("connect set no state cookie")
	}
	return loc.Query().Get("state"), cookie, loc
}

// The write consent asks for both scopes. Asking for send alone would
// replace the grant with one that can send but no longer read, and every
// existing read tool would start failing.
func TestGmailWriteConnectAsksForSendAlongsideReadonly(t *testing.T) {
	g := newFakeGoogleGmail(t)
	h := newGmailConnectRouter(t, &stubDB{}, g)

	state, cookie, loc := startGmailWriteConnect(t, h, "writes=1")

	scope := loc.Query().Get("scope")
	for _, want := range []string{gmail.ScopeReadonly, gmail.ScopeSend} {
		if !strings.Contains(scope, want) {
			t.Errorf("consent scope = %q, want it to include %q", scope, want)
		}
	}
	if got := loc.Query().Get("prompt"); got != "consent" {
		t.Errorf("prompt = %q, want consent so the send permission is actually shown", got)
	}
	if got := loc.Query().Get("access_type"); got != "offline" {
		t.Errorf("access_type = %q, want offline", got)
	}

	// The intent is in the cookie, not in a query parameter the browser could
	// change on the way back.
	if !strings.HasPrefix(cookie.Value, state+".") {
		t.Error("the state cookie does not carry the redirect's state")
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 3 || parts[2] != "write" {
		t.Errorf("state cookie = %d parts (%v), want state.verifier.write", len(parts), parts[len(parts)-1:])
	}

	// And a plain connect still asks for readonly only: connecting a source
	// never bundles the ability to write.
	_, readCookie, readLoc := startGmailWriteConnect(t, h, "")
	if got := readLoc.Query().Get("scope"); got != gmail.ScopeReadonly {
		t.Errorf("plain connect scope = %q, want exactly %q", got, gmail.ScopeReadonly)
	}
	if strings.HasSuffix(readCookie.Value, ".write") {
		t.Errorf("a plain connect recorded write intent (%q)", readCookie.Value)
	}
}

// Completing the write consent stores the new refresh token with the
// granted scopes and turns writes on for Gmail.
func TestGmailWriteConsentEnablesWritesAndStoresTheSendScope(t *testing.T) {
	pool := testPool(t)
	g := newFakeGoogleGmail(t)
	g.mu.Lock()
	g.grantedScope = gmail.ScopeReadonly + " " + gmail.ScopeSend
	g.mu.Unlock()
	h := newGmailConnectRouter(t, pool, g)

	state, cookie, _ := startGmailWriteConnect(t, h, "writes=1")
	rec := finishGmailConnect(t, h, "state="+url.QueryEscape(state)+"&code=consent-code", cookie)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want 302 (body %q)", rec.Code, rec.Body.String())
	}

	var enabled bool
	var ciphertext []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT writes_enabled, credentials_ciphertext FROM user_connections WHERE source = 'gmail'`).
		Scan(&enabled, &ciphertext); err != nil {
		t.Fatalf("read stored gmail connection: %v", err)
	}
	if !enabled {
		t.Error("writes_enabled is false after a completed send consent")
	}

	// The granted scopes are stored, so the send capability can be checked later
	// without attempting a send.
	cipher, err := keys.NewCipher(testKeyEncryptionSecret)
	if err != nil {
		t.Fatalf("build cipher: %v", err)
	}
	plain, err := cipher.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("decrypt credentials: %v", err)
	}
	var creds struct {
		RefreshToken string `json:"refresh_token"`
		Scopes       string `json:"scopes"`
	}
	if err := json.Unmarshal([]byte(plain), &creds); err != nil {
		t.Fatalf("decode credentials: %v", err)
	}
	if !strings.Contains(creds.Scopes, gmail.ScopeSend) {
		t.Errorf("stored scopes = %q, want the send scope recorded", creds.Scopes)
	}
	if creds.RefreshToken != testGmailRefreshToken {
		t.Errorf("stored refresh token = %q, want the one the re-consent issued", creds.RefreshToken)
	}
	if strings.Contains(rec.Header().Get("Location"), testGmailRefreshToken) {
		t.Error("the redirect carries the refresh token")
	}
}

// What Google granted decides, not what was asked. A user who
// completes the flow with the send box unticked must not end up with writes on —
// a proposal would be approved and then refused by Google.
func TestGmailWriteConsentWithoutTheSendScopeLeavesWritesOff(t *testing.T) {
	pool := testPool(t)
	g := newFakeGoogleGmail(t) // grants readonly only
	h := newGmailConnectRouter(t, pool, g)

	state, cookie, _ := startGmailWriteConnect(t, h, "writes=1")
	rec := finishGmailConnect(t, h, "state="+url.QueryEscape(state)+"&code=consent-code", cookie)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want 302 (body %q)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); !strings.Contains(got, "gmail_send_not_granted") {
		t.Errorf("redirect = %q, want it to report that sending was not granted", got)
	}

	var enabled bool
	if err := pool.QueryRow(context.Background(),
		`SELECT writes_enabled FROM user_connections WHERE source = 'gmail'`).Scan(&enabled); err != nil {
		t.Fatalf("read stored gmail connection: %v", err)
	}
	if enabled {
		t.Error("writes_enabled is true although Google never granted the send scope")
	}
}

// A read-intent connect can never turn writes on, even if the grant happens to
// carry the send scope: the intent is fixed by the request that started the
// flow, not by what comes back.
func TestAReadOnlyConnectNeverEnablesWrites(t *testing.T) {
	pool := testPool(t)
	g := newFakeGoogleGmail(t)
	g.mu.Lock()
	g.grantedScope = gmail.ScopeReadonly + " " + gmail.ScopeSend
	g.mu.Unlock()
	h := newGmailConnectRouter(t, pool, g)

	state, cookie, _ := startGmailWriteConnect(t, h, "")
	rec := finishGmailConnect(t, h, "state="+url.QueryEscape(state)+"&code=consent-code", cookie)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want 302 (body %q)", rec.Code, rec.Body.String())
	}

	var enabled bool
	if err := pool.QueryRow(context.Background(),
		`SELECT writes_enabled FROM user_connections WHERE source = 'gmail'`).Scan(&enabled); err != nil {
		t.Fatalf("read stored gmail connection: %v", err)
	}
	if enabled {
		t.Error("a plain connect enabled writes; only an explicit request may")
	}
}
