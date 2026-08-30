package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"cortex/internal/auth"
)

// maxAdminBodyBytes bounds the optional request body.
const maxAdminBodyBytes = 1 << 12

// indexRequest is the optional POST /api/admin/index body.
type indexRequest struct {
	// Source names one source to reindex. Empty means all of them.
	Source string `json:"source"`
}

// indexResponse reports what was queued.
type indexResponse struct {
	Queued []string `json:"queued"`
}

// handleAdminIndex queues a reindex of the vector store.
//
// POST /api/admin/index            → 202 {"queued":["gmail","jira","notion"]}
// POST /api/admin/index {"source"} → 202 {"queued":["notion"]}
//
// Queued rather than performed: a full crawl of three sources makes thousands of
// API calls and takes minutes, which is not something to do inside an HTTP
// request. The work happens in the River workers, and `make index` is a curl at
// this endpoint.
//
// Admin-gated (REQ-7.3): the bearer token qualifies, and so does a session
// whose email is in ADMIN_EMAILS. Everyone else gets 403 — the user-facing
// path to a reindex is POST /api/documents/refresh, which carries its own
// cooldown instead of an admin check.
func (s *Server) handleAdminIndex(w http.ResponseWriter, r *http.Request) {
	logger := s.deps.Logger

	user, ok := auth.UserFrom(r.Context())
	if !ok {
		writeError(w, logger, http.StatusInternalServerError, "internal error")
		return
	}
	if !user.IsAdmin {
		writeError(w, logger, http.StatusForbidden, "admin access required")
		return
	}

	if s.deps.Enqueuer == nil || len(s.deps.IndexSources) == 0 {
		writeError(w, logger, http.StatusServiceUnavailable, "indexing is not configured on this server")
		return
	}

	// Checked unconditionally, before anything is queued, and NOT inside the
	// body decode. It was inside, behind an "empty body returns early" branch —
	// which meant the documented protection never ran on the primary path, since
	// `make index` and every CSRF payload send no body at all.
	//
	// The check is what keeps this POST out of the CORS-"simple" set, so a
	// browser must preflight it. Without it, any page the developer visits while
	// `make dev` is running can auto-submit a bodyless form at
	// localhost:8080 and queue three crawls — hundreds of upstream API calls and
	// OpenAI spend. hostCheck does not help here: the browser sends
	// Host: localhost, which is legitimately allowed.
	if !hasJSONContentType(r) {
		writeError(w, logger, http.StatusUnsupportedMediaType, "content-type must be application/json")
		return
	}

	requested, ok := decodeIndexRequest(w, r, logger)
	if !ok {
		return
	}

	sources := s.deps.IndexSources
	if requested != "" {
		if !slices.Contains(sources, requested) {
			writeError(w, logger, http.StatusBadRequest,
				"unknown source "+requested+"; expected one of "+strings.Join(s.deps.IndexSources, ", "))
			return
		}
		sources = []string{requested}
	}

	queued := make([]string, 0, len(sources))
	for _, source := range sources {
		if err := s.deps.Enqueuer.EnqueueIndexSource(r.Context(), source); err != nil {
			logger.Error("admin: failed to queue indexing", "source", source, "error", err)
			// Partial success is reported as failure rather than hidden: the
			// caller asked for a full reindex, and a 202 listing two of three
			// sources reads as "done" to a script.
			writeError(w, logger, http.StatusInternalServerError, "failed to queue indexing")
			return
		}
		queued = append(queued, source)
	}

	logger.Info("admin: queued indexing", "sources", strings.Join(queued, ", "))
	writeJSON(w, logger, http.StatusAccepted, indexResponse{Queued: queued})
}

// decodeIndexRequest reads the optional body, writing the error response itself
// when it is malformed. The bool reports whether the caller should continue.
//
// The body is genuinely optional — `make index` sends `{}` and a bare POST sends
// nothing — so an empty request is the common case rather than an error. The
// content-type check lives in the caller, because it has to run whether or not a
// body is present.
func decodeIndexRequest(w http.ResponseWriter, r *http.Request, logger *slog.Logger) (string, bool) {
	if r.Body == nil || r.ContentLength == 0 {
		return "", true
	}

	var body indexRequest
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxAdminBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, logger, http.StatusBadRequest, "invalid JSON body")
		return "", false
	}
	return strings.ToLower(strings.TrimSpace(body.Source)), true
}
