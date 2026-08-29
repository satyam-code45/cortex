// Package api holds the HTTP surface: the Chi router, its middleware, and the
// request handlers.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"mime"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"cortex/internal/store"
)

// DB is the subset of *pgxpool.Pool the API needs. Declaring it here — rather
// than importing the concrete pool type into every handler — keeps the
// handlers honest about what they use and swappable in tests.
//
// store.DBTX is embedded so a read-only handler can query through the pool
// directly instead of opening a transaction it has no reason to hold.
type DB interface {
	store.DBTX

	Begin(ctx context.Context) (pgx.Tx, error)
	Ping(ctx context.Context) error
}

// Enqueuer submits work to the job queue.
//
// It takes the transaction so the enqueue commits with the caller's inserts —
// that is what makes POST /api/chat atomic. Declared here as an interface rather
// than depending on internal/jobs so River stays out of the HTTP layer, and so
// the rollback path can be exercised with a fake that just returns an error.
type Enqueuer interface {
	EnqueueAgentRun(ctx context.Context, tx pgx.Tx, runID uuid.UUID) error

	// EnqueueIndexSource queues a reindex of one source. It takes no
	// transaction: the admin endpoint writes no rows of its own, so there is
	// nothing for the enqueue to commit alongside.
	EnqueueIndexSource(ctx context.Context, source string) error
}

// Deps are the collaborators the handlers need.
//
// There is no LLM provider here any more: since Day 2 the handlers only record
// and report on runs, and every model call happens in the River worker.
type Deps struct {
	// DB is the connection pool used for queries and transactions.
	DB DB
	// Enqueuer queues agent runs.
	Enqueuer Enqueuer
	// Model is the model name recorded on runs.
	Model string
	// DevUserEmail identifies the single hardcoded user; real auth lands later.
	DevUserEmail string
	// IndexSources are the source names POST /api/admin/index accepts. Empty
	// disables the endpoint, which is what a build with no indexer wants.
	IndexSources []string
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
	r.Use(hostCheck(deps.Logger))
	r.Use(middleware.Recoverer)

	r.Get("/healthz", s.handleHealthz)
	r.Route("/api", func(r chi.Router) {
		r.Post("/chat", s.handleChat)
		r.Get("/runs/{id}", s.handleGetRun)
		r.Get("/runs/{id}/trace", s.handleGetRunTrace)
		r.Post("/admin/index", s.handleAdminIndex)
	})
	return r
}

// hasJSONContentType reports whether the request declares a JSON body. The
// header may carry parameters (`application/json; charset=utf-8`), so only the
// media type is compared.
func hasJSONContentType(r *http.Request) bool {
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mediaType == "application/json"
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
