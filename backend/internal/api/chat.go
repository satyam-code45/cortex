package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"cortex/internal/auth"
	"cortex/internal/llm"
	"cortex/internal/store"
)

// errLLMKeyRequired means the user has no stored LLM key; the handler maps it
// to 409 {"error":"llm_key_required"} and the frontend routes that to settings.
var errLLMKeyRequired = errors.New("llm key required")

// errRunLimitExceeded means the user hit RUNS_PER_USER_PER_HOUR; mapped to 429.
var errRunLimitExceeded = errors.New("run limit exceeded")

const (
	// maxTitleRunes caps the conversation title derived from the first message.
	maxTitleRunes = 80

	// maxRequestBytes caps the request body. Without it a single large POST is
	// read entirely into memory before validation can reject it.
	maxRequestBytes = 64 << 10

	// maxMessageRunes caps a single user message. The stored message is
	// replayed to the model on every later turn of the conversation, so an
	// oversized one is not a one-off cost — it is charged again on each turn,
	// and on every iteration of the agent loop.
	maxMessageRunes = 8000

	// statusPending is the initial state of an enqueued run.
	statusPending = "pending"
)

// chatRequest is the POST /api/chat request body.
type chatRequest struct {
	ConversationID string `json:"conversation_id"`
	Message        string `json:"message"`
}

// chatResponse is the POST /api/chat success body.
//
// There is deliberately no answer field. An agent run makes a dozen LLM calls
// and several Jira round trips, which is far longer than a request should be
// held open — so the handler returns a run to poll and the work happens on the
// queue.
type chatResponse struct {
	ConversationID uuid.UUID `json:"conversation_id"`
	RunID          uuid.UUID `json:"run_id"`
}

// handleChat records a question and queues an agent run for it.
//
// POST /api/chat {"conversation_id"?, "message"} → 202 {"conversation_id","run_id"}
//
// Poll GET /api/runs/{id} for the outcome. Day 5 replaces polling with SSE over
// the same run_events the worker is already writing.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	logger := s.deps.Logger
	ctx := r.Context()

	// Requiring a JSON content type keeps this endpoint out of the set of
	// CORS-"simple" requests, so a browser must preflight it. Without the
	// check, any page the user visits could POST here cross-origin with no
	// preflight and spend real money on LLM calls.
	if !hasJSONContentType(r) {
		writeError(w, logger, http.StatusUnsupportedMediaType, "content-type must be application/json")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, logger, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, logger, http.StatusBadRequest, "invalid JSON body")
		return
	}
	message := strings.TrimSpace(req.Message)
	if message == "" {
		writeError(w, logger, http.StatusBadRequest, "message is required")
		return
	}
	if utf8.RuneCountInString(message) > maxMessageRunes {
		writeError(w, logger, http.StatusBadRequest,
			fmt.Sprintf("message must be at most %d characters", maxMessageRunes))
		return
	}

	// A pointer, not uuid.Nil, to mean "not supplied". Using the zero UUID as the
	// sentinel collided with a real input: an explicit
	// "conversation_id":"00000000-0000-0000-0000-000000000000" parsed to uuid.Nil
	// and was then indistinguishable from an absent field, so instead of 404ing it
	// silently started a brand-new conversation.
	var conversationID *uuid.UUID
	if req.ConversationID != "" {
		id, err := uuid.Parse(req.ConversationID)
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, "conversation_id must be a UUID")
			return
		}
		if id == uuid.Nil {
			writeError(w, logger, http.StatusBadRequest, "conversation_id must not be the zero UUID")
			return
		}
		conversationID = &id
	}

	queued, err := s.enqueueRun(ctx, conversationID, message)
	if err != nil {
		var notFound *conversationNotFoundError
		switch {
		case errors.As(err, &notFound):
			writeError(w, logger, http.StatusNotFound, "conversation not found")
		case errors.Is(err, errLLMKeyRequired):
			// The exact body the frontend's API client routes to settings on.
			writeError(w, logger, http.StatusConflict, "llm_key_required")
		case errors.Is(err, errRunLimitExceeded):
			writeError(w, logger, http.StatusTooManyRequests,
				fmt.Sprintf("run limit reached (%d per hour) — try again later", s.deps.RunsPerUserPerHour))
		default:
			logger.Error("chat: failed to enqueue run", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "failed to enqueue run")
		}
		return
	}

	logger.Info("chat: run enqueued",
		"run_id", queued.runID, "conversation_id", queued.conversationID)
	writeJSON(w, logger, http.StatusAccepted, chatResponse{
		ConversationID: queued.conversationID,
		RunID:          queued.runID,
	})
}

// queuedRun identifies the run created by enqueueRun.
type queuedRun struct {
	conversationID uuid.UUID
	runID          uuid.UUID
}

// conversationNotFoundError reports a conversation_id that does not exist or
// does not belong to the current user.
type conversationNotFoundError struct {
	id uuid.UUID
}

func (e *conversationNotFoundError) Error() string {
	return fmt.Sprintf("conversation %s not found", e.id)
}

// enqueueRun stores the question and queues the job that will answer it, in one
// transaction.
//
// The single transaction is the point of the design. River writes its job row
// through the same pgx.Tx as our inserts, so all four effects — the user, the
// conversation, the message, the agent_run, and the queued job — commit together
// or not at all. Neither of the two failure modes a two-step version has can
// happen: a run row with no job to execute it (a question that silently never
// gets answered), or a job referencing a run that was rolled back (a worker
// looking up a row that does not exist).
func (s *Server) enqueueRun(ctx context.Context, conversationID *uuid.UUID, message string) (queuedRun, error) {
	tx, err := s.deps.DB.Begin(ctx)
	if err != nil {
		return queuedRun{}, fmt.Errorf("begin transaction: %w", err)
	}
	// Roll back on a context that cannot already be cancelled: pgx kills the
	// connection outright when it cannot send the ROLLBACK, forcing a fresh
	// handshake instead of returning it to the pool.
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op once the tx is committed

	q := store.New(tx)

	user, ok := auth.UserFrom(ctx)
	if !ok {
		return queuedRun{}, errors.New("no authenticated user in context")
	}

	// BYOK (REQ-7.2): a run without a stored key would only fail in the worker,
	// after a row and a job exist — check at enqueue time so a keyless user
	// gets a 409 and nothing is created. The worker still decrypts at execution
	// time; this is the fail-fast, not the source of truth.
	if _, err := q.GetUserLLMKey(ctx, user.ID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return queuedRun{}, errLLMKeyRequired
		}
		return queuedRun{}, fmt.Errorf("check llm key: %w", err)
	}

	// Per-user rate limit (REQ-7.3): the LLM spend is the user's own key, but
	// every run also consumes the server's Jira/Notion/Gmail quotas. The count
	// is not serializable with the insert below, so two concurrent requests can
	// both pass at N-1 — acceptable for a courtesy limit; the hard costs are
	// bounded by MAX_ITERATIONS per run anyway.
	if s.deps.RunsPerUserPerHour > 0 {
		count, err := q.CountUserRunsSince(ctx, store.CountUserRunsSinceParams{
			UserID:    user.ID,
			CreatedAt: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour).UTC(), Valid: true},
		})
		if err != nil {
			return queuedRun{}, fmt.Errorf("count user runs: %w", err)
		}
		if count >= int64(s.deps.RunsPerUserPerHour) {
			return queuedRun{}, errRunLimitExceeded
		}
	}

	var conversation store.Conversation
	if conversationID == nil {
		title := truncateRunes(message, maxTitleRunes)
		conversation, err = q.CreateConversation(ctx, store.CreateConversationParams{
			UserID: user.ID,
			Title:  &title,
		})
		if err != nil {
			return queuedRun{}, fmt.Errorf("create conversation: %w", err)
		}
	} else {
		conversation, err = q.GetConversation(ctx, store.GetConversationParams{
			ID:     *conversationID,
			UserID: user.ID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return queuedRun{}, &conversationNotFoundError{id: *conversationID}
		}
		if err != nil {
			return queuedRun{}, fmt.Errorf("get conversation: %w", err)
		}
	}

	if _, err := q.InsertMessage(ctx, store.InsertMessageParams{
		ConversationID: conversation.ID,
		Role:           string(llm.RoleUser),
		Content:        message,
	}); err != nil {
		return queuedRun{}, fmt.Errorf("insert user message: %w", err)
	}

	model := s.deps.Model
	run, err := q.InsertAgentRun(ctx, store.InsertAgentRunParams{
		ConversationID: conversation.ID,
		Query:          message,
		Status:         statusPending,
		Model:          &model,
	})
	if err != nil {
		return queuedRun{}, fmt.Errorf("insert agent run: %w", err)
	}

	if err := s.deps.Enqueuer.EnqueueAgentRun(ctx, tx, run.ID); err != nil {
		return queuedRun{}, fmt.Errorf("enqueue agent run: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return queuedRun{}, fmt.Errorf("commit transaction: %w", err)
	}
	return queuedRun{conversationID: conversation.ID, runID: run.ID}, nil
}

// truncateRunes shortens s to at most max runes, appending an ellipsis when it
// had to cut. It counts runes, not bytes, so multi-byte text is not split.
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return strings.TrimSpace(string(runes[:max])) + "…"
}
