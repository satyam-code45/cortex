package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"cortex/internal/auth"
	"cortex/internal/store"
)

// runResponse is the GET /api/runs/{id} body.
//
// Answer and Error are pointers so that "not finished yet" is null rather than
// an empty string — a client polling this needs to tell a run still working from
// one that finished with nothing to say.
type runResponse struct {
	RunID          uuid.UUID `json:"run_id"`
	ConversationID uuid.UUID `json:"conversation_id"`
	Status         string    `json:"status"`
	Answer         *string   `json:"answer"`
	Error          *string   `json:"error"`
	Model          *string   `json:"model,omitempty"`
	LatencyMS      *int32    `json:"latency_ms,omitempty"`
	InputTokens    *int32    `json:"input_tokens,omitempty"`
	OutputTokens   *int32    `json:"output_tokens,omitempty"`
}

// handleGetRun reports the state of one agent run.
//
// GET /api/runs/{id} → 200 {"status","answer","error",...}
//
// This is the polling half of the async chat flow: POST /api/chat returns a
// run_id, and a client polls here until status is 'completed' or 'failed'.
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
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
		writeError(w, logger, http.StatusInternalServerError, "failed to load run")
		return
	}

	// Scoped by user through the conversation join (see
	// db/queries/agent_runs.sql). The run id comes straight from the caller, so
	// ownership has to be part of the lookup rather than a check someone can
	// forget to write.
	run, err := q.GetAgentRunForUser(ctx, store.GetAgentRunForUserParams{
		ID:     runID,
		UserID: user.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, logger, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		logger.Error("runs: failed to load run", "run_id", runID, "error", err)
		writeError(w, logger, http.StatusInternalServerError, "failed to load run")
		return
	}

	writeJSON(w, logger, http.StatusOK, runResponse{
		RunID:          run.ID,
		ConversationID: run.ConversationID,
		Status:         run.Status,
		Answer:         run.Answer,
		Error:          run.Error,
		Model:          run.Model,
		LatencyMS:      run.LatencyMs,
		InputTokens:    run.InputTokens,
		OutputTokens:   run.OutputTokens,
	})
}
