package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"cortex/internal/store"
)

// conversationResponse is one row of the GET /api/conversations body.
type conversationResponse struct {
	ID        uuid.UUID `json:"id"`
	Title     *string   `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// messageResponse is one row of the GET /api/conversations/{id}/messages body.
//
// AgentRunID is null for user messages and for assistant messages from before
// the message→run link existed; when set, it is how the frontend reaches
// GET /api/runs/{id}/trace to resolve the message's citation markers.
type messageResponse struct {
	ID         uuid.UUID  `json:"id"`
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	AgentRunID *uuid.UUID `json:"agent_run_id"`
	CreatedAt  time.Time  `json:"created_at"`
}

// handleListConversations lists the dev user's conversations, newest-updated
// first.
//
// GET /api/conversations → 200 [{"id","title","created_at","updated_at"}]
func (s *Server) handleListConversations(w http.ResponseWriter, r *http.Request) {
	logger := s.deps.Logger
	ctx := r.Context()

	q := store.New(s.deps.DB)
	user, err := q.UpsertUser(ctx, s.deps.DevUserEmail)
	if err != nil {
		logger.Error("conversations: failed to resolve user", "error", err)
		writeError(w, logger, http.StatusInternalServerError, "failed to list conversations")
		return
	}

	conversations, err := q.ListConversationsByUser(ctx, user.ID)
	if err != nil {
		logger.Error("conversations: failed to list conversations", "error", err)
		writeError(w, logger, http.StatusInternalServerError, "failed to list conversations")
		return
	}

	// Never null: an empty list is "no conversations yet", and making the
	// frontend distinguish [] from null would be a bug factory.
	out := make([]conversationResponse, 0, len(conversations))
	for _, c := range conversations {
		out = append(out, conversationResponse{
			ID:        c.ID,
			Title:     c.Title,
			CreatedAt: c.CreatedAt.Time.UTC(),
			UpdatedAt: c.UpdatedAt.Time.UTC(),
		})
	}
	writeJSON(w, logger, http.StatusOK, out)
}

// handleListConversationMessages lists one conversation's messages in order.
//
// GET /api/conversations/{id}/messages → 200 [{"id","role","content",
// "agent_run_id","created_at"}], 404 when the conversation is unknown or not
// the user's — the same answer for both, so the endpoint does not confirm which
// ids exist.
func (s *Server) handleListConversationMessages(w http.ResponseWriter, r *http.Request) {
	logger := s.deps.Logger
	ctx := r.Context()

	conversationID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, logger, http.StatusBadRequest, "conversation id must be a UUID")
		return
	}

	q := store.New(s.deps.DB)
	user, err := q.UpsertUser(ctx, s.deps.DevUserEmail)
	if err != nil {
		logger.Error("messages: failed to resolve user", "error", err)
		writeError(w, logger, http.StatusInternalServerError, "failed to list messages")
		return
	}

	// Ownership through the same query POST /api/chat uses; the list query
	// below is only reached for a conversation this user owns.
	if _, err := q.GetConversation(ctx, store.GetConversationParams{
		ID:     conversationID,
		UserID: user.ID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, logger, http.StatusNotFound, "conversation not found")
			return
		}
		logger.Error("messages: failed to load conversation",
			"conversation_id", conversationID, "error", err)
		writeError(w, logger, http.StatusInternalServerError, "failed to list messages")
		return
	}

	messages, err := q.ListMessagesByConversation(ctx, conversationID)
	if err != nil {
		logger.Error("messages: failed to list messages",
			"conversation_id", conversationID, "error", err)
		writeError(w, logger, http.StatusInternalServerError, "failed to list messages")
		return
	}

	out := make([]messageResponse, 0, len(messages))
	for _, m := range messages {
		out = append(out, messageResponse{
			ID:         m.ID,
			Role:       m.Role,
			Content:    m.Content,
			AgentRunID: m.AgentRunID,
			CreatedAt:  m.CreatedAt.Time.UTC(),
		})
	}
	writeJSON(w, logger, http.StatusOK, out)
}
