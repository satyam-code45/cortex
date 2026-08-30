package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"cortex/internal/agent"
	"cortex/internal/auth"
	"cortex/internal/store"
)

// handleGetRunTrace returns everything one run did.
//
// GET /api/runs/{id}/trace → 200 {query, status, totals, timeline, tool_calls,
// evidence, citations}
//
// This is the observability endpoint (idea.md §15) and the backend half of
// Day 5's trace panel: it answers "why did the agent say that?" with the
// transcript, the tool arguments and latencies, the evidence, and the mapping
// from each [n] marker in the answer to the source behind it.
//
// The assembly lives in internal/agent (agent.AssembleTrace) because the event
// payload shapes are defined there; this handler only loads rows.
func (s *Server) handleGetRunTrace(w http.ResponseWriter, r *http.Request) {
	logger := s.deps.Logger
	ctx := r.Context()

	runID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, logger, http.StatusBadRequest, "run id must be a UUID")
		return
	}

	q := store.New(s.deps.DB)

	user, ok := auth.UserFrom(ctx)
	if !ok {
		writeError(w, logger, http.StatusInternalServerError, "failed to load trace")
		return
	}

	// Ownership is part of the lookup, exactly as in handleGetRun: the run id is
	// caller-supplied, and a trace exposes far more than the answer does.
	run, err := q.GetAgentRunForUser(ctx, store.GetAgentRunForUserParams{
		ID:     runID,
		UserID: user.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, logger, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		logger.Error("trace: failed to load run", "run_id", runID, "error", err)
		writeError(w, logger, http.StatusInternalServerError, "failed to load trace")
		return
	}

	// Four reads rather than one join: they return different cardinalities, and
	// a single query would multiply the transcript by the evidence count. The
	// ownership check above has already gated all of them.
	events, err := q.ListRunEventsByRun(ctx, runID)
	if err != nil {
		logger.Error("trace: failed to load run events", "run_id", runID, "error", err)
		writeError(w, logger, http.StatusInternalServerError, "failed to load trace")
		return
	}
	toolCalls, err := q.ListToolCallsByRun(ctx, runID)
	if err != nil {
		logger.Error("trace: failed to load tool calls", "run_id", runID, "error", err)
		writeError(w, logger, http.StatusInternalServerError, "failed to load trace")
		return
	}
	evidence, err := q.ListEvidenceByRun(ctx, runID)
	if err != nil {
		logger.Error("trace: failed to load evidence", "run_id", runID, "error", err)
		writeError(w, logger, http.StatusInternalServerError, "failed to load trace")
		return
	}
	citations, err := q.ListCitationsByRun(ctx, runID)
	if err != nil {
		logger.Error("trace: failed to load citations", "run_id", runID, "error", err)
		writeError(w, logger, http.StatusInternalServerError, "failed to load trace")
		return
	}

	writeJSON(w, logger, http.StatusOK,
		agent.AssembleTrace(run, events, toolCalls, evidence, citations))
}
