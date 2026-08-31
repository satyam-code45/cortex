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

// handleGmailConnect starts the Gmail connect flow: mint state +
// PKCE, park them in a short-lived cookie, and send the browser to Google's
// consent screen asking for gmail.readonly only.
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

	http.SetCookie(w, &http.Cookie{
		Name:     auth.ConnectStateCookieName,
		Value:    state + "." + pkce.Verifier,
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
		Scopes:      []string{gmail.ScopeReadonly},
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
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		writeError(w, logger, http.StatusBadRequest, "connect state malformed — start again")
		return
	}
	stateVal, verifier := parts[0], parts[1]
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

	creds := connections.GmailCredentials{RefreshToken: token.RefreshToken}
	identity := map[string]string{"email": email}
	if err := s.deps.Connections.Save(r.Context(), user.ID, connections.SourceGmail, creds, identity); err != nil {
		logger.Error("connections: save gmail", "error", err)
		writeError(w, logger, http.StatusInternalServerError, "internal error")
		return
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
