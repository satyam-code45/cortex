package api

import (
	"errors"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"cortex/internal/store"
)

const (
	// maxDocumentsLimit caps one page of the Sources listing.
	maxDocumentsLimit = 100
	// defaultDocumentsLimit is the page size when the caller names none.
	defaultDocumentsLimit = 25
	// snippetRunes is the length of the match-context snippet in a listing row.
	snippetRunes = 200
)

// documentRow is one row of GET /api/documents.
type documentRow struct {
	ID              uuid.UUID  `json:"id"`
	Source          string     `json:"source"`
	Title           string     `json:"title"`
	URL             string     `json:"url"`
	Snippet         string     `json:"snippet"`
	SourceTimestamp *time.Time `json:"source_timestamp"`
}

// documentsResponse is the GET /api/documents body: the page plus the counts
// and freshness stamps the Sources view renders around it.
type documentsResponse struct {
	Documents []documentRow `json:"documents"`
	// Counts is per-source result counts under the same filters as the page,
	// so the tabs and the list never disagree.
	Counts map[string]int64 `json:"counts"`
	// LastIndexed is when each source's content last actually changed.
	LastIndexed map[string]time.Time `json:"last_indexed"`
	// LastRefreshed is when the last index crawl finished, changed or not —
	// the stamp next to the Refresh button. Null before the first crawl.
	LastRefreshed *time.Time `json:"last_refreshed"`
}

// handleListDocuments lists indexed documents for the Sources view.
//
// GET /api/documents?source=&q=&limit=&offset= → 200 documentsResponse
func (s *Server) handleListDocuments(w http.ResponseWriter, r *http.Request) {
	logger := s.deps.Logger
	ctx := r.Context()

	var source *string
	if v := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("source"))); v != "" {
		if !slices.Contains(s.deps.IndexSources, v) {
			writeError(w, logger, http.StatusBadRequest,
				"unknown source "+v+"; expected one of "+strings.Join(s.deps.IndexSources, ", "))
			return
		}
		source = &v
	}
	var query *string
	if v := strings.TrimSpace(r.URL.Query().Get("q")); v != "" {
		query = &v
	}
	limit := clampInt(r.URL.Query().Get("limit"), defaultDocumentsLimit, 1, maxDocumentsLimit)
	offset := clampInt(r.URL.Query().Get("offset"), 0, 0, math.MaxInt32)

	q := store.New(s.deps.DB)
	rows, err := q.ListDocuments(ctx, store.ListDocumentsParams{
		Source: source,
		Query:  query,
		Limit:  int32(limit),
		Offset: int32(offset),
	})
	if err != nil {
		logger.Error("documents: list", "error", err)
		writeError(w, logger, http.StatusInternalServerError, "failed to list documents")
		return
	}
	// Counts share the search filter but never the source filter: the tabs
	// show what each source holds under the current search, and a tab count
	// that zeroes out the moment another tab is selected reads as a data bug.
	counts, err := q.CountDocumentsFiltered(ctx, store.CountDocumentsFilteredParams{
		Query: query,
	})
	if err != nil {
		logger.Error("documents: count", "error", err)
		writeError(w, logger, http.StatusInternalServerError, "failed to list documents")
		return
	}
	lastIndexed, err := q.SourceLastIndexed(ctx)
	if err != nil {
		logger.Error("documents: last indexed", "error", err)
		writeError(w, logger, http.StatusInternalServerError, "failed to list documents")
		return
	}

	resp := documentsResponse{
		Documents:   make([]documentRow, 0, len(rows)),
		Counts:      make(map[string]int64, len(counts)),
		LastIndexed: make(map[string]time.Time, len(lastIndexed)),
	}
	for _, row := range rows {
		var ts *time.Time
		if row.SourceTimestamp.Valid {
			t := row.SourceTimestamp.Time.UTC()
			ts = &t
		}
		snippetQuery := ""
		if query != nil {
			snippetQuery = *query
		}
		resp.Documents = append(resp.Documents, documentRow{
			ID:              row.ID,
			Source:          row.Source,
			Title:           row.Title,
			URL:             row.Url,
			Snippet:         snippetAround(row.Content, snippetQuery),
			SourceTimestamp: ts,
		})
	}
	for _, c := range counts {
		resp.Counts[c.Source] = c.Count
	}
	for _, li := range lastIndexed {
		resp.LastIndexed[li.Source] = li.LastIndexed.Time.UTC()
	}
	// Enqueuer nil = indexing not configured (the refresh handler 503s); the
	// listing still works, just without a freshness stamp.
	if s.deps.Enqueuer != nil {
		if finishedAt, ok, err := s.deps.Enqueuer.NewestFinalizedIndexJob(ctx); err != nil {
			// The stamp is decoration on a listing that already succeeded.
			logger.Warn("documents: last refreshed lookup", "error", err)
		} else if ok {
			t := finishedAt.UTC()
			resp.LastRefreshed = &t
		}
	}
	writeJSON(w, logger, http.StatusOK, resp)
}

// documentDetail is the GET /api/documents/{id} body.
type documentDetail struct {
	ID              uuid.UUID  `json:"id"`
	Source          string     `json:"source"`
	ExternalID      string     `json:"external_id"`
	Title           string     `json:"title"`
	URL             string     `json:"url"`
	Content         string     `json:"content"`
	SourceTimestamp *time.Time `json:"source_timestamp"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// handleGetDocument returns one document in full, for the detail drawer.
func (s *Server) handleGetDocument(w http.ResponseWriter, r *http.Request) {
	logger := s.deps.Logger

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, logger, http.StatusBadRequest, "document id must be a UUID")
		return
	}
	doc, err := store.New(s.deps.DB).GetDocumentByID(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, logger, http.StatusNotFound, "document not found")
		return
	}
	if err != nil {
		logger.Error("documents: get", "document_id", id, "error", err)
		writeError(w, logger, http.StatusInternalServerError, "failed to load document")
		return
	}

	var ts *time.Time
	if doc.SourceTimestamp.Valid {
		t := doc.SourceTimestamp.Time.UTC()
		ts = &t
	}
	writeJSON(w, logger, http.StatusOK, documentDetail{
		ID:              doc.ID,
		Source:          doc.Source,
		ExternalID:      doc.ExternalID,
		Title:           doc.Title,
		URL:             doc.Url,
		Content:         doc.Content,
		SourceTimestamp: ts,
		UpdatedAt:       doc.UpdatedAt.Time.UTC(),
	})
}

// refreshResponse is the POST /api/documents/refresh success body.
type refreshResponse struct {
	Queued []string `json:"queued"`
}

// refreshCooldownResponse is the 429 body; RetryAfterSeconds drives the UI's
// countdown.
type refreshCooldownResponse struct {
	Error             string `json:"error"`
	RetryAfterSeconds int    `json:"retry_after_seconds"`
}

// handleRefreshDocuments queues a reindex of every source, for any
// authenticated user — the admin endpoint's user-facing sibling. What replaces
// the admin check is a server-enforced cooldown: the three upstream APIs are
// paginated and rate-limited, so a refresh more often than
// INDEX_REFRESH_COOLDOWN buys 429s upstream, not freshness.
func (s *Server) handleRefreshDocuments(w http.ResponseWriter, r *http.Request) {
	logger := s.deps.Logger

	if s.deps.Enqueuer == nil || len(s.deps.IndexSources) == 0 {
		writeError(w, logger, http.StatusServiceUnavailable, "indexing is not configured on this server")
		return
	}
	if !hasJSONContentType(r) {
		// Same CSRF posture as POST /api/chat: forcing a preflight.
		writeError(w, logger, http.StatusUnsupportedMediaType, "content-type must be application/json")
		return
	}

	finishedAt, ok, err := s.deps.Enqueuer.NewestFinalizedIndexJob(r.Context())
	if err != nil {
		logger.Error("documents: cooldown lookup", "error", err)
		writeError(w, logger, http.StatusInternalServerError, "failed to refresh")
		return
	}
	if ok {
		elapsed := time.Since(finishedAt)
		if elapsed < s.deps.IndexRefreshCooldown {
			retryAfter := int(math.Ceil((s.deps.IndexRefreshCooldown - elapsed).Seconds()))
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			writeJSON(w, logger, http.StatusTooManyRequests, refreshCooldownResponse{
				Error:             "refresh_cooldown",
				RetryAfterSeconds: retryAfter,
			})
			return
		}
	}

	// A job may already be running (finalized-at only moves when one ends);
	// River's UniqueOpts dedupe makes enqueueing beside it a no-op, so this
	// needs no in-flight check of its own.
	queued := make([]string, 0, len(s.deps.IndexSources))
	for _, source := range s.deps.IndexSources {
		if err := s.deps.Enqueuer.EnqueueIndexSource(r.Context(), source); err != nil {
			logger.Error("documents: failed to queue refresh", "source", source, "error", err)
			writeError(w, logger, http.StatusInternalServerError, "failed to refresh")
			return
		}
		queued = append(queued, source)
	}
	logger.Info("documents: refresh queued", "sources", strings.Join(queued, ", "))
	writeJSON(w, logger, http.StatusAccepted, refreshResponse{Queued: queued})
}

// snippetAround windows content to snippetRunes around the first
// case-insensitive occurrence of query; from the start when query is empty or
// absent.
//
// The whole computation happens on runes. Byte indexes from a lowered copy
// cannot be used on the original: strings.ToLower changes byte lengths for
// case-expanding code points (Ⱥ is 2 bytes, ⱥ is 3), so a byte offset found in
// the lowered string can point past the end of — or mid-rune into — the
// original. unicode.ToLower per rune is a 1:1 mapping, keeping every index
// aligned with the original content.
func snippetAround(content, query string) string {
	contentRunes := []rune(strings.TrimSpace(content))

	start := 0
	if query != "" {
		if idx := runeIndexFold(contentRunes, []rune(query)); idx > 0 {
			// Back off a little so the match has leading context.
			start = max(idx-snippetRunes/4, 0)
		}
	}

	windowed := contentRunes[start:]
	truncated := false
	if len(windowed) > snippetRunes {
		windowed = windowed[:snippetRunes]
		truncated = true
	}
	snippet := string(windowed)
	if truncated {
		snippet += "…"
	}
	if start > 0 {
		snippet = "…" + snippet
	}
	return strings.ReplaceAll(snippet, "\n", " ")
}

// runeIndexFold returns the rune index of the first case-insensitive
// occurrence of needle in haystack, or -1.
func runeIndexFold(haystack, needle []rune) int {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return -1
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j, want := range needle {
			if unicode.ToLower(haystack[i+j]) != unicode.ToLower(want) {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// clampInt parses raw as an int, applying a default and bounds.
func clampInt(raw string, def, min, max int) int {
	if strings.TrimSpace(raw) == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
