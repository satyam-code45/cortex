package api

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"cortex/internal/auth"
	"cortex/internal/connections"
	"cortex/internal/tools/gmail"
)

// connectStateCookiePath scopes the connect-flow state cookie to its own
// endpoints, so it and the login flow's cookie (Path /api/auth) can never
// collide.
const connectStateCookiePath = "/api/connections"

// intentRead and intentWrite label what a connect flow is asking Google for.
//
// Carried in the state cookie rather than inferred at the callback, for the same
// reason the PKCE verifier is: the callback must not be able to be talked into a
// broader grant than the request that started it. A user who clicked "connect"
// cannot come back holding send permission because a crafted callback URL said
// so.
const (
	intentRead  = "read"
	intentWrite = "write"
)

// handleGmailConnect starts the Gmail connect flow: mint state +
// PKCE, park them in a short-lived cookie, and send the browser to Google's
// consent screen.
//
// It asks for gmail.readonly, and for gmail.send as well only when the request
// carries writes=1 — which the Connections page sends when a user turns writes
// on for Gmail, never as part of connecting. That separation is the point: the
// permission to send mail as somebody is requested at the moment they ask for
// it, in a consent screen that names it, and a user who never asks never has a
// token that could send anything.
//
// gmail.send is a Google restricted scope, the same tier as reading mail: a
// published app needs Google's review, and possibly a security assessment,
// before it may ask the public for it. Test users are unaffected.
//
// This is incremental authorization — sign-in (openid/email/profile) is a
// separate flow and stays untouched. access_type=offline + prompt=consent make
// Google return a refresh token, which is the credential this connection
// stores. No nonce: this flow issues no ID token; identity comes from the
// mailbox's own profile endpoint, validated before storing.
func (s *Server) handleGmailConnect(w http.ResponseWriter, r *http.Request) {
	state, err := auth.RandomState()
	if err != nil {
		s.deps.Logger.Error("connections: generate state", "error", err)
		writeError(w, s.deps.Logger, http.StatusInternalServerError, "internal error")
		return
	}
	pkce, err := auth.NewPKCE()
	if err != nil {
		s.deps.Logger.Error("connections: generate pkce", "error", err)
		writeError(w, s.deps.Logger, http.StatusInternalServerError, "internal error")
		return
	}

	intent := intentRead
	scopes := []string{gmail.ScopeReadonly}
	if r.URL.Query().Get("writes") == "1" {
		intent = intentWrite
		// Both scopes, not just send. Google's grant replaces the previous one
		// for this client, so asking for send alone would produce a token that
		// can send mail and no longer read it — and every existing read tool
		// would start failing.
		scopes = []string{gmail.ScopeReadonly, gmail.ScopeSend}
	}

	http.SetCookie(w, &http.Cookie{
		Name:     auth.ConnectStateCookieName,
		Value:    state + "." + pkce.Verifier + "." + intent,
		Path:     connectStateCookiePath,
		MaxAge:   int(stateTTL.Seconds()),
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})

	authURL := s.deps.OIDC.Client.AuthCodeURL(auth.AuthCodeParams{
		RedirectURI: s.deps.ConnectRedirectURI,
		State:       state,
		PKCE:        pkce,
		Scopes:      scopes,
		Extra: url.Values{
			"access_type": {"offline"},
			"prompt":      {"consent"},
		},
	})
	http.Redirect(w, r, authURL, http.StatusFound)
}

// handleGmailCallback finishes the connect flow: state check, code exchange
// (refresh token required), live mailbox validation, encrypted store.
// Reconnecting is the same flow — the upsert replaces the credential and
// clears any error state.
func (s *Server) handleGmailCallback(w http.ResponseWriter, r *http.Request) {
	logger := s.deps.Logger
	user, ok := auth.UserFrom(r.Context())
	if !ok {
		writeError(w, logger, http.StatusInternalServerError, "internal error")
		return
	}

	cookie, err := r.Cookie(auth.ConnectStateCookieName)
	if err != nil {
		writeError(w, logger, http.StatusBadRequest, "connect state missing or expired — start again from the Connections page")
		return
	}
	clearCookie(w, r, auth.ConnectStateCookieName, connectStateCookiePath)

	parts := strings.Split(cookie.Value, ".")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		writeError(w, logger, http.StatusBadRequest, "connect state malformed — start again")
		return
	}
	stateVal, verifier := parts[0], parts[1]
	// A two-part cookie is a read-only connect: either an older flow still in
	// flight, or the plain connect button. Defaulting to the narrower intent is
	// the only safe direction to be wrong in.
	intent := intentRead
	if len(parts) == 3 && parts[2] == intentWrite {
		intent = intentWrite
	}
	if subtle.ConstantTimeCompare([]byte(stateVal), []byte(r.URL.Query().Get("state"))) != 1 {
		writeError(w, logger, http.StatusBadRequest, "connect state mismatch — start again")
		return
	}
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		// The user declined consent, or Google refused. Their words, our page.
		s.redirectConnections(w, r, "error="+url.QueryEscape(errParam))
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		writeError(w, logger, http.StatusBadRequest, "callback carried no code")
		return
	}

	token, err := s.deps.OIDC.Client.ExchangeCode(r.Context(), code, s.deps.ConnectRedirectURI,
		&auth.PKCE{Verifier: verifier}, auth.ExchangeOptions{RequireRefreshToken: true})
	if errors.Is(err, auth.ErrNoRefreshToken) {
		// Google only re-issues a refresh token with prompt=consent, which we
		// send — so this means a previous grant is lingering. The fix is on
		// the user's Google account page.
		logger.Error("connections: gmail exchange returned no refresh token")
		s.redirectConnections(w, r, "error=no_refresh_token")
		return
	}
	if err != nil {
		logger.Error("connections: gmail code exchange failed", "error", err)
		writeError(w, logger, http.StatusBadGateway, "could not complete the Gmail connection with Google")
		return
	}

	// Prove the token reads its mailbox and learn the address, before storing
	// anything — the same live-validation contract as the paste flows. The
	// exchange's access token is still fresh, so no refresh round trip.
	gmailToken := &gmail.Token{
		RefreshToken: token.RefreshToken,
		AccessToken:  token.AccessToken,
		TokenType:    token.TokenType,
		Expiry:       token.Expiry,
		Scope:        token.Scope,
	}
	email, err := connections.ValidateGmail(r.Context(), s.gmailWebCredentials(), gmailToken, s.deps.GmailBaseURL, logger)
	if err != nil {
		logger.Error("connections: gmail mailbox validation failed", "error", err)
		s.redirectConnections(w, r, "error=gmail_validation_failed")
		return
	}

	creds := connections.GmailCredentials{
		RefreshToken: token.RefreshToken,
		// The scopes Google actually granted, which is not necessarily what was
		// asked for: a consent screen lets a user tick some boxes and not
		// others. Stored so the send capability can be checked later without
		// attempting a send.
		Scopes: token.Scope,
	}
	identity := map[string]string{"email": email}
	if err := s.deps.Connections.Save(r.Context(), user.ID, connections.SourceGmail, creds, identity); err != nil {
		logger.Error("connections: save gmail", "error", err)
		writeError(w, logger, http.StatusInternalServerError, "internal error")
		return
	}

	// Save resets writes_enabled to its default only for a brand-new row; an
	// upsert of an existing connection leaves the flag alone. So both outcomes
	// are set explicitly here, and the deciding fact is what Google granted
	// rather than what was requested.
	if intent == intentWrite {
		if !connections.GmailCanSend(creds) {
			logger.Warn("connections: gmail write consent completed without the send scope")
			s.redirectConnections(w, r, "error=gmail_send_not_granted")
			return
		}
		if err := s.deps.Connections.SetWritesEnabled(r.Context(), user.ID, connections.SourceGmail, true); err != nil {
			logger.Error("connections: enable gmail writes", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}
		s.redirectConnections(w, r, "writes_enabled=gmail")
		return
	}

	// A plain reconnect replaces the previous grant, so a connection that could
	// send a moment ago may no longer be able to. The flag has to follow: left
	// true, the Connections page would report that Cortex can propose changes
	// while no send tool is ever registered, and the only trace would be a
	// warning line in the log. This is the "both outcomes" half the comment
	// above promises.
	if !connections.GmailCanSend(creds) {
		if err := s.deps.Connections.SetWritesEnabled(r.Context(), user.ID, connections.SourceGmail, false); err != nil {
			logger.Error("connections: disable gmail writes after a read-only reconnect", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}
	}
	s.redirectConnections(w, r, "connected=gmail")
}

// redirectConnections sends the browser back to the Connections page with a
// query the page renders as a banner.
func (s *Server) redirectConnections(w http.ResponseWriter, r *http.Request, query string) {
	http.Redirect(w, r, s.deps.FrontendOrigin+"/connections?"+query, http.StatusFound)
}

// gmailWebCredentials shapes the sign-in Web OAuth client for the gmail
// package. TokenURI comes from the same client the exchange used, so tests
// that point OIDC at a fake Google cover this too.
func (s *Server) gmailWebCredentials() *gmail.Credentials {
	return &gmail.Credentials{
		ClientID:     s.deps.OIDC.Client.ID,
		ClientSecret: s.deps.OIDC.Client.Secret,
		AuthURI:      s.deps.OIDC.Client.AuthURL,
		TokenURI:     s.deps.OIDC.Client.TokenURL,
	}
}
