// Package api holds the HTTP surface: the Chi router, its middleware, and the
// request handlers.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"mime"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"cortex/internal/auth"
	"cortex/internal/connections"
	"cortex/internal/keys"
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

	// NewestFinalizedIndexJob reports when the most recent index job finished,
	// and whether any has. It drives the Sources refresh cooldown.
	NewestFinalizedIndexJob(ctx context.Context) (finishedAt time.Time, ok bool, err error)

	// EnqueueExecuteWrite queues an approved write. It takes the transaction
	// for the same reason the agent-run enqueue does, and the stakes are
	// higher: the job must become visible only when the approval commits, so
	// there is no window in which a worker could send an email whose approval
	// was then rolled back.
	EnqueueExecuteWrite(ctx context.Context, tx pgx.Tx, actionID uuid.UUID) error

	// EnqueueResumeRun continues a paused run. It takes the transaction because
	// the caller decides whether to resume from inside it — reading its own
	// uncommitted decision — and a job that became visible before that commit
	// would read the pre-decision state, find work still outstanding, and do
	// nothing, leaving the run waiting forever.
	EnqueueResumeRun(ctx context.Context, tx pgx.Tx, runID uuid.UUID) error
}

// Deps are the collaborators the handlers need.
//
// There is no LLM provider here any more: since runs went asynchronous the
// handlers only record and report on runs, and every model call happens in the
// River worker.
type Deps struct {
	// DB is the connection pool used for queries and transactions.
	DB DB
	// Enqueuer queues agent runs.
	Enqueuer Enqueuer
	// Model is the model name recorded on runs.
	Model string
	// IndexSources are the source names POST /api/admin/index accepts. Empty
	// disables the endpoint, which is what a build with no indexer wants.
	IndexSources []string
	// FrontendOrigin is the one browser origin CORS admits. Empty sends no
	// CORS headers at all, which shuts browsers out entirely.
	FrontendOrigin string
	// Logger receives request and handler logs.
	Logger *slog.Logger

	// OIDC drives Google sign-in.
	OIDC *auth.OIDC
	// Keys stores and reports users' LLM keys.
	Keys *keys.Service
	// APIToken is the static bearer token for non-browser callers. Empty
	// disables bearer auth entirely (the comparison can never succeed).
	APIToken string
	// BearerEmail is the user the bearer token acts as (DEV_USER_EMAIL).
	BearerEmail string
	// AllowedEmails, when non-empty, restricts sign-in to these addresses
	// (lowercase).
	AllowedEmails []string
	// AdminEmails are the session emails admitted to admin endpoints.
	AdminEmails []string
	// RunsPerUserPerHour caps run creation per user; 0 disables the limit
	// (tests), production always sets it.
	RunsPerUserPerHour int
	// ActionTTL is how long a proposed write stays decidable. The approve
	// endpoint enforces it in the same predicate as the status check, and the
	// audit list uses it to report whether a row can still be acted on. Zero
	// disables expiry.
	ActionTTL time.Duration
	// IndexRefreshCooldown is the minimum interval between user-triggered
	// Sources refreshes.
	IndexRefreshCooldown time.Duration
	// OpenAIBaseURL overrides the endpoint key validation calls; tests point
	// it at an httptest server.
	OpenAIBaseURL string

	// Connections stores and reports users' source connections.
	Connections *connections.Service
	// NotionBaseURL overrides the endpoint Notion token validation calls;
	// tests point it at an httptest server. (Jira needs no equivalent — the
	// user supplies their site URL, so tests just paste a fake server's.)
	NotionBaseURL string
	// ConnectRedirectURI is where Google sends the browser after Gmail
	// consent — must be registered on the Web OAuth client.
	ConnectRedirectURI string
	// GmailBaseURL overrides the Gmail API root for the connect flow's
	// mailbox validation; tests point it at an httptest server.
	GmailBaseURL string
	// WriteReadiness checks whether one source's stored credential can actually
	// perform writes, before the flag is flipped. A function rather than the
	// registry builder itself, so the HTTP layer depends on the question it asks
	// rather than on how connections are built. Nil means this deployment cannot
	// enable writes at all, which the handler answers honestly.
	WriteReadiness func(ctx context.Context, userID uuid.UUID, source string) error
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
		// Subrouter-level (r.Use) rather than an inline group: preflight
		// OPTIONS requests match no registered route, and only subrouter
		// middleware wraps chi's routing itself — group middleware would
		// never see them. /admin/index is exempted from the grant: the
		// frontend has no admin UI, so a passing preflight there would only
		// authorize whatever happens to be served on the allowed origin (any
		// dev server on port 3000) to queue paid indexing crawls. curl and
		// make index are unaffected — CORS binds browsers, not clients.
		r.Use(corsMiddleware(deps.FrontendOrigin, "/api/admin/index"))
		// Auth runs after CORS: preflights carry no cookies, so an auth-first
		// ordering would 401 every OPTIONS and shut browsers out entirely.
		r.Use(s.requireAuth)
		r.Get("/auth/google/login", s.handleGoogleLogin)
		r.Get("/auth/google/callback", s.handleGoogleCallback)
		r.Post("/auth/logout", s.handleLogout)
		r.Get("/auth/me", s.handleMe)
		r.Post("/chat", s.handleChat)
		r.Get("/runs/{id}", s.handleGetRun)
		r.Get("/runs/{id}/events", s.handleRunEvents)
		r.Get("/runs/{id}/trace", s.handleGetRunTrace)
		r.Get("/conversations", s.handleListConversations)
		r.Get("/conversations/{id}/messages", s.handleListConversationMessages)
		r.Get("/settings/llm-key", s.handleGetLLMKey)
		r.Put("/settings/llm-key", s.handlePutLLMKey)
		r.Delete("/settings/llm-key", s.handleDeleteLLMKey)
		r.Get("/connections", s.handleGetConnections)
		r.Put("/connections/jira", s.handlePutJiraConnection)
		r.Put("/connections/notion", s.handlePutNotionConnection)
		r.Put("/connections/mode", s.handlePutConnectionsMode)
		r.Delete("/connections/{source}", s.handleDeleteConnection)
		r.Get("/connections/gmail/connect", s.handleGmailConnect)
		r.Get("/connections/gmail/callback", s.handleGmailCallback)
		r.Get("/actions", s.handleListActions)
		r.Post("/actions/{id}/approve", s.handleApproveAction)
		r.Post("/actions/{id}/reject", s.handleRejectAction)
		r.Put("/connections/{source}/writes", s.handlePutConnectionWrites)
		r.Get("/documents", s.handleListDocuments)
		r.Get("/documents/{id}", s.handleGetDocument)
		r.Post("/documents/refresh", s.handleRefreshDocuments)
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
