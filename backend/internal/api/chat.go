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

	"cortex/internal/llm"
	"cortex/internal/store"
)

const (
	// chatSystemPrompt is today's placeholder persona. The agent's real system
	// prompt arrives with the orchestrator on Day 2.
	chatSystemPrompt = "You are Cortex, an AI analyst for an engineering organization. " +
		"Answer clearly and concisely. If you do not know something, say so."

	// llmTimeout bounds a single generation call.
	llmTimeout = 60 * time.Second

	// persistTimeout bounds the write-back that records a run's outcome. It
	// runs on a context detached from the request so a client disconnect
	// cannot leave a run stuck in 'running'.
	persistTimeout = 5 * time.Second

	// maxTitleRunes caps the conversation title derived from the first message.
	maxTitleRunes = 80

	// llmPurposeChat labels llm_calls rows made by this handler.
	llmPurposeChat = "chat"
)

// chatRequest is the POST /api/chat request body.
type chatRequest struct {
	ConversationID string `json:"conversation_id"`
	Message        string `json:"message"`
}

// chatResponse is the POST /api/chat success body.
type chatResponse struct {
	ConversationID uuid.UUID `json:"conversation_id"`
	Answer         string    `json:"answer"`
	RunID          uuid.UUID `json:"run_id"`
}

// handleChat answers one message: it records the turn, makes a single LLM
// call, and persists the result.
//
// POST /api/chat {"conversation_id"?, "message"} → 200 {"conversation_id","answer","run_id"}
//
// The two database transactions deliberately sit either side of the LLM call
// rather than wrapping it: holding a transaction open across a multi-second
// network call would pin a pool connection for its whole duration. Writing the
// agent_run row up front is also what lets a failed call be reported as a
// stored, inspectable run rather than vanishing.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	logger := s.deps.Logger
	ctx := r.Context()

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, logger, http.StatusBadRequest, "invalid JSON body")
		return
	}
	message := strings.TrimSpace(req.Message)
	if message == "" {
		writeError(w, logger, http.StatusBadRequest, "message is required")
		return
	}

	var conversationID uuid.UUID
	if req.ConversationID != "" {
		id, err := uuid.Parse(req.ConversationID)
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, "conversation_id must be a UUID")
			return
		}
		conversationID = id
	}

	started, err := s.startRun(ctx, conversationID, message)
	if err != nil {
		var notFound *conversationNotFoundError
		if errors.As(err, &notFound) {
			writeError(w, logger, http.StatusNotFound, "conversation not found")
			return
		}
		logger.Error("chat: failed to start run", "error", err)
		writeError(w, logger, http.StatusInternalServerError, "failed to start run")
		return
	}

	llmCtx, cancel := context.WithTimeout(ctx, llmTimeout)
	defer cancel()

	callStart := time.Now()
	resp, llmErr := s.deps.Provider.Generate(llmCtx, llm.Request{
		Model:    s.deps.Model,
		System:   chatSystemPrompt,
		Messages: started.history,
	})
	latency := time.Since(callStart)

	// Record the outcome on a context detached from the request: the run must
	// reach a terminal state even if the client has already hung up.
	persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer persistCancel()

	if llmErr != nil {
		logger.Error("chat: llm call failed", "run_id", started.runID, "error", llmErr)
		if err := s.failRun(persistCtx, started, llmErr, latency); err != nil {
			logger.Error("chat: failed to record failed run", "run_id", started.runID, "error", err)
		}
		writeError(w, logger, http.StatusBadGateway, "llm request failed")
		return
	}

	if err := s.completeRun(persistCtx, started, resp, latency); err != nil {
		logger.Error("chat: failed to record completed run", "run_id", started.runID, "error", err)
		writeError(w, logger, http.StatusInternalServerError, "failed to persist answer")
		return
	}

	writeJSON(w, logger, http.StatusOK, chatResponse{
		ConversationID: started.conversationID,
		Answer:         resp.Text,
		RunID:          started.runID,
	})
}

// startedRun is the state carried from the opening transaction through to the
// write-back.
type startedRun struct {
	conversationID uuid.UUID
	runID          uuid.UUID
	// history is the conversation so far, including the message just stored.
	history []llm.Message
	// nextSeq is the seq to use for the next run_events row.
	nextSeq int32
}

// conversationNotFoundError reports a conversation_id that does not exist or
// does not belong to the current user.
type conversationNotFoundError struct {
	id uuid.UUID
}

func (e *conversationNotFoundError) Error() string {
	return fmt.Sprintf("conversation %s not found", e.id)
}

// startRun opens the run: it resolves the user and conversation, stores the
// user's message, and inserts the agent_run row in 'running' state, all in one
// transaction. It returns the conversation history to send to the model.
func (s *Server) startRun(ctx context.Context, conversationID uuid.UUID, message string) (startedRun, error) {
	tx, err := s.deps.DB.Begin(ctx)
	if err != nil {
		return startedRun{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once the tx is committed

	q := store.New(tx)

	user, err := q.UpsertUser(ctx, s.deps.DevUserEmail)
	if err != nil {
		return startedRun{}, fmt.Errorf("upsert dev user: %w", err)
	}

	var conversation store.Conversation
	if conversationID == uuid.Nil {
		title := truncateRunes(message, maxTitleRunes)
		conversation, err = q.CreateConversation(ctx, store.CreateConversationParams{
			UserID: user.ID,
			Title:  &title,
		})
		if err != nil {
			return startedRun{}, fmt.Errorf("create conversation: %w", err)
		}
	} else {
		conversation, err = q.GetConversation(ctx, conversationID)
		if errors.Is(err, pgx.ErrNoRows) {
			return startedRun{}, &conversationNotFoundError{id: conversationID}
		}
		if err != nil {
			return startedRun{}, fmt.Errorf("get conversation: %w", err)
		}
		// Do not leak another user's conversation, even with one dev user today.
		if conversation.UserID != user.ID {
			return startedRun{}, &conversationNotFoundError{id: conversationID}
		}
	}

	if _, err := q.InsertMessage(ctx, store.InsertMessageParams{
		ConversationID: conversation.ID,
		Role:           string(llm.RoleUser),
		Content:        message,
	}); err != nil {
		return startedRun{}, fmt.Errorf("insert user message: %w", err)
	}

	model := s.deps.Model
	run, err := q.InsertAgentRun(ctx, store.InsertAgentRunParams{
		ConversationID: conversation.ID,
		Query:          message,
		Status:         "running",
		Model:          &model,
	})
	if err != nil {
		return startedRun{}, fmt.Errorf("insert agent run: %w", err)
	}

	started := startedRun{
		conversationID: conversation.ID,
		runID:          run.ID,
		nextSeq:        1,
	}
	if err := s.appendEvent(ctx, q, &started, "run_started", map[string]any{
		"conversation_id": conversation.ID,
		"query":           message,
		"model":           model,
	}); err != nil {
		return startedRun{}, err
	}

	// Read the history inside the transaction so it is a consistent snapshot
	// that includes the message just inserted.
	messages, err := q.ListMessagesByConversation(ctx, conversation.ID)
	if err != nil {
		return startedRun{}, fmt.Errorf("list messages: %w", err)
	}
	started.history = make([]llm.Message, 0, len(messages))
	for _, m := range messages {
		started.history = append(started.history, llm.Message{
			Role:    llm.Role(m.Role),
			Content: m.Content,
		})
	}

	if err := tx.Commit(ctx); err != nil {
		return startedRun{}, fmt.Errorf("commit transaction: %w", err)
	}
	return started, nil
}

// completeRun stores the assistant's answer, the llm_call metrics, and the
// terminal run state in one transaction.
func (s *Server) completeRun(ctx context.Context, started startedRun, resp llm.Response, latency time.Duration) error {
	tx, err := s.deps.DB.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once the tx is committed

	q := store.New(tx)
	latencyMs := int32(latency.Milliseconds())
	inputTokens := int32(resp.InputTokens)
	outputTokens := int32(resp.OutputTokens)

	if _, err := q.InsertMessage(ctx, store.InsertMessageParams{
		ConversationID: started.conversationID,
		Role:           string(llm.RoleAssistant),
		Content:        resp.Text,
	}); err != nil {
		return fmt.Errorf("insert assistant message: %w", err)
	}

	if _, err := q.InsertLLMCall(ctx, store.InsertLLMCallParams{
		AgentRunID:   started.runID,
		Purpose:      llmPurposeChat,
		Model:        s.deps.Model,
		InputTokens:  &inputTokens,
		OutputTokens: &outputTokens,
		LatencyMs:    &latencyMs,
	}); err != nil {
		return fmt.Errorf("insert llm call: %w", err)
	}

	if err := s.appendEvent(ctx, q, &started, "llm_call_completed", map[string]any{
		"purpose":       llmPurposeChat,
		"model":         s.deps.Model,
		"input_tokens":  resp.InputTokens,
		"output_tokens": resp.OutputTokens,
		"latency_ms":    latencyMs,
	}); err != nil {
		return err
	}
	if err := s.appendEvent(ctx, q, &started, "run_completed", map[string]any{
		"answer":     resp.Text,
		"latency_ms": latencyMs,
	}); err != nil {
		return err
	}

	if _, err := q.CompleteAgentRun(ctx, store.CompleteAgentRunParams{
		ID:           started.runID,
		Answer:       &resp.Text,
		LatencyMs:    &latencyMs,
		InputTokens:  &inputTokens,
		OutputTokens: &outputTokens,
	}); err != nil {
		return fmt.Errorf("complete agent run: %w", err)
	}

	if err := q.TouchConversation(ctx, started.conversationID); err != nil {
		return fmt.Errorf("touch conversation: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// failRun records a failed run and its event. The detailed error is stored for
// debugging; the client only sees a generic message.
func (s *Server) failRun(ctx context.Context, started startedRun, cause error, latency time.Duration) error {
	tx, err := s.deps.DB.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once the tx is committed

	q := store.New(tx)
	latencyMs := int32(latency.Milliseconds())
	reason := cause.Error()

	if _, err := q.FailAgentRun(ctx, store.FailAgentRunParams{
		ID:        started.runID,
		Error:     &reason,
		LatencyMs: &latencyMs,
	}); err != nil {
		return fmt.Errorf("fail agent run: %w", err)
	}

	if err := s.appendEvent(ctx, q, &started, "run_failed", map[string]any{
		"error":      reason,
		"latency_ms": latencyMs,
	}); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// appendEvent writes the next run_events row and advances the sequence. Events
// are the append-only transcript of a run: gap-free seq per run is what lets
// the trace be replayed in order.
func (s *Server) appendEvent(ctx context.Context, q *store.Queries, started *startedRun, eventType string, payload map[string]any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s payload: %w", eventType, err)
	}
	if _, err := q.InsertRunEvent(ctx, store.InsertRunEventParams{
		AgentRunID: started.runID,
		Seq:        started.nextSeq,
		Type:       eventType,
		Payload:    encoded,
	}); err != nil {
		return fmt.Errorf("insert %s event: %w", eventType, err)
	}
	started.nextSeq++
	return nil
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
