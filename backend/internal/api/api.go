// Package api holds the HTTP surface: the Chi router, its middleware, and the
// request handlers.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5"

	"cortex/internal/llm"
)

// DB is the subset of *pgxpool.Pool the API needs. Declaring it here — rather
// than importing the concrete pool type into every handler — keeps the
// handlers honest about what they use and swappable in tests.
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Ping(ctx context.Context) error
}

// Deps are the collaborators the handlers need.
type Deps struct {
	// DB is the connection pool used for queries and transactions.
	DB DB
	// Provider is the LLM backend.
	Provider llm.Provider
	// Model is the model name recorded on runs and LLM calls.
	Model string
	// DevUserEmail identifies the single hardcoded user; real auth lands later.
	DevUserEmail string
	// Logger receives request and handler logs.
	Logger *slog.Logger
}

// Server holds the handler dependencies.
type Server struct {
	deps Deps
}

// NewRouter builds the application router.
func NewRouter(deps Deps) http.Handler {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	s := &Server{deps: deps}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(requestLogger(deps.Logger))
	r.Use(middleware.Recoverer)

	r.Get("/healthz", s.handleHealthz)
	r.Route("/api", func(r chi.Router) {
		r.Post("/chat", s.handleChat)
	})
	return r
}

// errorResponse is the body returned for every non-2xx response.
type errorResponse struct {
	Error string `json:"error"`
}

// writeJSON writes v as a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, logger *slog.Logger, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already sent, so this can only be logged.
		logger.Error("failed to write JSON response", "error", err)
	}
}

// writeError writes a JSON error body with the given status code.
func writeError(w http.ResponseWriter, logger *slog.Logger, status int, message string) {
	writeJSON(w, logger, status, errorResponse{Error: message})
}
