package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"cortex/internal/auth"
	"cortex/internal/keys"
	"cortex/internal/llm"
)

// llmKeyResponse is what the API ever says about a stored key: provider and
// last4. The key itself is returned by no endpoint, logged by no handler.
type llmKeyResponse struct {
	Provider string `json:"provider"`
	Last4    string `json:"last4"`
}

// putLLMKeyRequest is the PUT /api/settings/llm-key body.
type putLLMKeyRequest struct {
	Provider string `json:"provider"`
	Key      string `json:"key"`
}

// handleGetLLMKey reports the stored key's provider and last4, 404 when none.
func (s *Server) handleGetLLMKey(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFrom(r.Context())
	if !ok {
		writeError(w, s.deps.Logger, http.StatusInternalServerError, "internal error")
		return
	}
	provider, last4, err := s.deps.Keys.Info(r.Context(), user.ID)
	if errors.Is(err, keys.ErrNoKey) {
		writeError(w, s.deps.Logger, http.StatusNotFound, "no llm key on file")
		return
	}
	if err != nil {
		s.deps.Logger.Error("settings: key info", "error", err)
		writeError(w, s.deps.Logger, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, s.deps.Logger, http.StatusOK, llmKeyResponse{Provider: provider, Last4: last4})
}

// handlePutLLMKey validates a key against its provider with one live call and
// stores it encrypted. 422 carries the provider's reason so the user can fix
// their key; nothing about the key is ever logged.
func (s *Server) handlePutLLMKey(w http.ResponseWriter, r *http.Request) {
	logger := s.deps.Logger
	user, ok := auth.UserFrom(r.Context())
	if !ok {
		writeError(w, logger, http.StatusInternalServerError, "internal error")
		return
	}
	if !hasJSONContentType(r) {
		writeError(w, logger, http.StatusUnsupportedMediaType, "content-type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	var req putLLMKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, logger, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Key = strings.TrimSpace(req.Key)

	switch req.Provider {
	case "openai":
	case "gemini":
		// The schema admits gemini so this is an API rejection, not a migration
		// waiting to happen — flipping it later is an implementation, not DDL.
		writeError(w, logger, http.StatusUnprocessableEntity, "gemini support is coming soon")
		return
	default:
		writeError(w, logger, http.StatusUnprocessableEntity, "provider must be one of: openai, gemini")
		return
	}
	if req.Key == "" {
		writeError(w, logger, http.StatusUnprocessableEntity, "key is required")
		return
	}

	if err := llm.ValidateOpenAIKey(r.Context(), req.Key, s.deps.OpenAIBaseURL); err != nil {
		var rejected *llm.KeyValidationError
		if errors.As(err, &rejected) {
			// The provider's own words about the user's own key — safe to
			// return, deliberately not logged.
			writeError(w, logger, http.StatusUnprocessableEntity, rejected.Reason)
			return
		}
		logger.Error("settings: key validation call failed", "error", err)
		writeError(w, logger, http.StatusBadGateway, "could not reach the provider to validate the key — try again")
		return
	}

	last4, err := s.deps.Keys.Save(r.Context(), user.ID, req.Provider, req.Key)
	if err != nil {
		logger.Error("settings: save key", "error", err)
		writeError(w, logger, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, logger, http.StatusOK, llmKeyResponse{Provider: req.Provider, Last4: last4})
}

// handleDeleteLLMKey removes the stored key. 404 when none was stored.
func (s *Server) handleDeleteLLMKey(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFrom(r.Context())
	if !ok {
		writeError(w, s.deps.Logger, http.StatusInternalServerError, "internal error")
		return
	}
	err := s.deps.Keys.Delete(r.Context(), user.ID)
	if errors.Is(err, keys.ErrNoKey) {
		writeError(w, s.deps.Logger, http.StatusNotFound, "no llm key on file")
		return
	}
	if err != nil {
		s.deps.Logger.Error("settings: delete key", "error", err)
		writeError(w, s.deps.Logger, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
