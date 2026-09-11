package api

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"cortex/internal/auth"
	"cortex/internal/keys"
	"cortex/internal/store"
)

const (
	// sessionTTL is how long a login lasts. Sessions are revocable rows, not
	// signed tokens, so a long TTL costs nothing in exposure that logout and
	// expiry don't already bound.
	sessionTTL = 30 * 24 * time.Hour
	// stateTTL bounds one trip to Google's consent screen and back.
	stateTTL = 10 * time.Minute
)

// handleGoogleLogin starts the sign-in flow: mint state + PKCE, park them in a
// short-lived cookie, and send the browser to Google.
func (s *Server) handleGoogleLogin(w http.ResponseWriter, r *http.Request) {
	state, err := auth.RandomState()
	if err != nil {
		s.deps.Logger.Error("auth: generate state", "error", err)
		writeError(w, s.deps.Logger, http.StatusInternalServerError, "internal error")
		return
	}
	pkce, err := auth.NewPKCE()
	if err != nil {
		s.deps.Logger.Error("auth: generate pkce", "error", err)
		writeError(w, s.deps.Logger, http.StatusInternalServerError, "internal error")
		return
	}
	// The nonce binds the ID token itself to this login attempt — state guards
	// the callback request, PKCE guards the code, the nonce guards the token.
	nonce, err := auth.RandomState()
	if err != nil {
		s.deps.Logger.Error("auth: generate nonce", "error", err)
		writeError(w, s.deps.Logger, http.StatusInternalServerError, "internal error")
		return
	}

	// State, verifier and nonce travel together: all three are needed on the
	// callback and none is a secret once the flow completes. HttpOnly keeps
	// scripts on any origin from reading them; Lax still sends the cookie on
	// Google's top-level redirect back.
	secure, sameSite := s.cookieAttrs(r)
	http.SetCookie(w, &http.Cookie{
		Name:     auth.StateCookieName,
		Value:    state + "." + pkce.Verifier + "." + nonce,
		Path:     "/api/auth",
		MaxAge:   int(stateTTL.Seconds()),
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
	})
	http.Redirect(w, r, s.deps.OIDC.LoginURL(state, pkce, nonce), http.StatusFound)
}

// handleGoogleCallback finishes the flow: state check, code exchange, ID-token
// verification, user upsert, session issue.
func (s *Server) handleGoogleCallback(w http.ResponseWriter, r *http.Request) {
	logger := s.deps.Logger

	// The state cookie can be missing legitimately (a bookmarked callback URL,
	// a browser that ate the cookie) — answer with instructions, not a 500.
	cookie, err := r.Cookie(auth.StateCookieName)
	if err != nil {
		writeError(w, logger, http.StatusBadRequest, "login state missing or expired — start again at /api/auth/google/login")
		return
	}
	s.clearCookie(w, r, auth.StateCookieName, "/api/auth")

	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		writeError(w, logger, http.StatusBadRequest, "login state malformed — start again")
		return
	}
	stateVal, verifier, nonce := parts[0], parts[1], parts[2]
	if subtle.ConstantTimeCompare([]byte(stateVal), []byte(r.URL.Query().Get("state"))) != 1 {
		writeError(w, logger, http.StatusBadRequest, "login state mismatch — start again")
		return
	}
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		// The user declined consent, or Google refused. Their words, our page.
		http.Redirect(w, r, s.deps.FrontendOrigin+"/login?error="+errParam, http.StatusFound)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		writeError(w, logger, http.StatusBadRequest, "callback carried no code")
		return
	}

	token, err := s.deps.OIDC.Exchange(r.Context(), code, &auth.PKCE{Verifier: verifier})
	if err != nil {
		logger.Error("auth: code exchange failed", "error", err)
		writeError(w, logger, http.StatusBadGateway, "could not complete sign-in with Google")
		return
	}
	claims, err := s.deps.OIDC.VerifyIDToken(r.Context(), token.IDToken)
	if err != nil {
		logger.Error("auth: id token rejected", "error", err)
		writeError(w, logger, http.StatusUnauthorized, "sign-in token rejected")
		return
	}
	// The token must echo this attempt's nonce: a valid token replayed from
	// another login (same client, same user) fails here.
	if subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(nonce)) != 1 {
		logger.Error("auth: id token nonce mismatch")
		writeError(w, logger, http.StatusUnauthorized, "sign-in token rejected")
		return
	}

	email := strings.ToLower(claims.Email)
	if len(s.deps.AllowedEmails) > 0 && !slices.Contains(s.deps.AllowedEmails, email) {
		// Named refusal, not a silent bounce: the person just authenticated
		// with Google successfully and needs to know this is an allowlist.
		http.Redirect(w, r, s.deps.FrontendOrigin+"/login?error=not_allowed", http.StatusFound)
		return
	}

	q := store.New(s.deps.DB)
	user, err := q.UpsertGoogleUser(r.Context(), store.UpsertGoogleUserParams{
		Email:     email,
		GoogleSub: &claims.Sub,
		Name:      &claims.Name,
		AvatarUrl: &claims.Picture,
	})
	if err != nil {
		logger.Error("auth: upsert user", "error", err)
		writeError(w, logger, http.StatusInternalServerError, "internal error")
		return
	}

	sessionToken, hash, err := auth.NewSessionToken()
	if err != nil {
		logger.Error("auth: mint session token", "error", err)
		writeError(w, logger, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := q.CreateSession(r.Context(), store.CreateSessionParams{
		UserID:    user.ID,
		TokenHash: hash,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(sessionTTL).UTC(), Valid: true},
	}); err != nil {
		logger.Error("auth: create session", "error", err)
		writeError(w, logger, http.StatusInternalServerError, "internal error")
		return
	}
	// Opportunistic housekeeping: there is no background sweeper, and login is
	// the one moment expired rows are guaranteed to be near a write anyway.
	if _, err := q.DeleteExpiredSessions(r.Context()); err != nil {
		logger.Warn("auth: sweep expired sessions", "error", err)
	}

	secure, sameSite := s.cookieAttrs(r)
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    sessionToken,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
	})
	http.Redirect(w, r, s.deps.FrontendOrigin+"/", http.StatusFound)
}

// handleLogout revokes the session row and expires the cookie.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(auth.SessionCookieName); err == nil {
		if _, err := store.New(s.deps.DB).DeleteSessionByTokenHash(r.Context(), auth.HashSessionToken(cookie.Value)); err != nil {
			s.deps.Logger.Error("auth: delete session", "error", err)
			writeError(w, s.deps.Logger, http.StatusInternalServerError, "internal error")
			return
		}
	}
	s.clearCookie(w, r, auth.SessionCookieName, "/")
	w.WriteHeader(http.StatusNoContent)
}

// meResponse is GET /api/auth/me.
type meResponse struct {
	Email     string `json:"email"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
	HasLLMKey bool   `json:"has_llm_key"`
	Provider  string `json:"provider,omitempty"`
}

// handleMe reports the signed-in identity and whether a key is on file — the
// one call the frontend needs to render the header and gate the chat input.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFrom(r.Context())
	if !ok {
		writeError(w, s.deps.Logger, http.StatusInternalServerError, "internal error")
		return
	}
	resp := meResponse{Email: user.Email, Name: user.Name, AvatarURL: user.AvatarURL}
	provider, _, err := s.deps.Keys.Info(r.Context(), user.ID)
	switch {
	case err == nil:
		resp.HasLLMKey = true
		resp.Provider = provider
	case errors.Is(err, keys.ErrNoKey):
		// No key yet — the zero values are the answer.
	default:
		s.deps.Logger.Error("auth: key info", "error", err)
		writeError(w, s.deps.Logger, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, s.deps.Logger, http.StatusOK, resp)
}

// cookieAttrs returns the Secure and SameSite attributes every auth cookie in
// this deployment must carry.
//
// One place, because the four cookies this server sets have to agree: a cookie
// cleared with different attributes than it was set with is not cleared at all,
// and browsers reject SameSite=None unless Secure is also set — so deciding
// them together is what keeps that pair valid.
func (s *Server) cookieAttrs(r *http.Request) (bool, http.SameSite) {
	secure := r.TLS != nil || s.deps.CookieSecure
	if s.deps.CookieCrossSite {
		return secure, http.SameSiteNoneMode
	}
	return secure, http.SameSiteLaxMode
}

// clearCookie expires a cookie with the attributes it was set with.
func (s *Server) clearCookie(w http.ResponseWriter, r *http.Request, name, path string) {
	secure, sameSite := s.cookieAttrs(r)
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     path,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
	})
}
