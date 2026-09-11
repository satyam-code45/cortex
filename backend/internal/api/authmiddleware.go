package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"cortex/internal/auth"
	"cortex/internal/store"
)

// touchInterval throttles last_seen_at writes: a busy session would otherwise
// pay one UPDATE per request for a column nobody reads at that resolution.
const touchInterval = 5 * time.Minute

// requireAuth authenticates every /api request by session cookie or bearer
// token and injects the resulting auth.User into the context. The two auth
// endpoints themselves are skipped — they exist to create what this middleware
// checks for.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// CORS answers preflights before this middleware; the guard is
		// belt-and-braces for a future reordering, because a 401 to OPTIONS
		// breaks every browser call at once.
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		switch r.URL.Path {
		case "/api/auth/google/login", "/api/auth/google/callback":
			next.ServeHTTP(w, r)
			return
		}

		if header := r.Header.Get("Authorization"); header != "" {
			s.authenticateBearer(w, r, next, header)
			return
		}
		s.authenticateSession(w, r, next)
	})
}

// authenticateBearer admits the one static operator token. It acts as the
// DEV_USER_EMAIL user with admin rights — it is the credential `make index`
// and scripts carry, i.e. the operator's own.
func (s *Server) authenticateBearer(w http.ResponseWriter, r *http.Request, next http.Handler, header string) {
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		writeError(w, s.deps.Logger, http.StatusUnauthorized, "unauthorized")
		return
	}
	// Hash both sides first: ConstantTimeCompare short-circuits on unequal
	// lengths, and hashing removes the length signal entirely.
	tokenSum := sha256.Sum256([]byte(token))
	wantSum := sha256.Sum256([]byte(s.deps.APIToken))
	if s.deps.APIToken == "" || subtle.ConstantTimeCompare(tokenSum[:], wantSum[:]) != 1 {
		writeError(w, s.deps.Logger, http.StatusUnauthorized, "unauthorized")
		return
	}

	user, err := store.New(s.deps.DB).UpsertUser(r.Context(), s.deps.BearerEmail)
	if err != nil {
		s.deps.Logger.Error("auth: upsert bearer user", "error", err)
		writeError(w, s.deps.Logger, http.StatusInternalServerError, "internal error")
		return
	}
	next.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), auth.User{
		ID:      user.ID,
		Email:   user.Email,
		IsAdmin: true,
		Machine: true,
	})))
}

// authenticateSession admits a valid session cookie.
func (s *Server) authenticateSession(w http.ResponseWriter, r *http.Request, next http.Handler) {
	cookie, err := r.Cookie(auth.SessionCookieName)
	if err != nil {
		writeError(w, s.deps.Logger, http.StatusUnauthorized, "unauthorized")
		return
	}

	q := store.New(s.deps.DB)
	row, err := q.GetSessionUserByTokenHash(r.Context(), auth.HashSessionToken(cookie.Value))
	if errors.Is(err, pgx.ErrNoRows) {
		// Unknown, revoked, or expired — indistinguishable by design.
		writeError(w, s.deps.Logger, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err != nil {
		s.deps.Logger.Error("auth: session lookup", "error", err)
		writeError(w, s.deps.Logger, http.StatusInternalServerError, "internal error")
		return
	}

	if time.Since(row.LastSeenAt.Time) > touchInterval {
		if err := q.TouchSession(r.Context(), row.SessionID); err != nil {
			// last_seen_at is bookkeeping; the session itself is valid.
			s.deps.Logger.Warn("auth: touch session", "error", err)
		}
	}

	user := auth.User{
		ID:      row.UserID,
		Email:   row.Email,
		IsAdmin: slices.Contains(s.deps.AdminEmails, row.Email),
	}
	if row.Name != nil {
		user.Name = *row.Name
	}
	if row.AvatarUrl != nil {
		user.AvatarURL = *row.AvatarUrl
	}
	next.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), user)))
}
