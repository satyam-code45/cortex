package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"cortex/internal/auth"
	"cortex/internal/connections"
	"cortex/internal/tools/jira"
	"cortex/internal/tools/notion"
)

// validateTimeout bounds the one live provider call a paste-flow makes before
// storing credentials — a hung provider should fail the request, not hold it.
const validateTimeout = 15 * time.Second

// sourceStatus is what GET /api/connections says about one source. Identity
// holds display facts captured at connect time; credentials appear in no
// response, ever.
type sourceStatus struct {
	Status    string          `json:"status"` // "connected" | "error" | "absent"
	Identity  json.RawMessage `json:"identity,omitempty"`
	UpdatedAt *time.Time      `json:"updated_at,omitempty"`
	LastError string          `json:"last_error,omitempty"`
	// WritesEnabled reports whether this connection may propose writes. Always
	// false for an absent source, and false by default for a present one: a read
	// connection never becomes a write connection without somebody asking.
	WritesEnabled bool `json:"writes_enabled"`
}

// connectionsResponse is the GET /api/connections body.
type connectionsResponse struct {
	Mode             string                  `json:"mode"` // "demo" | "user" | "none"
	UseDemoWorkspace bool                    `json:"use_demo_workspace"`
	Sources          map[string]sourceStatus `json:"sources"`
	// DemoAvailable reports whether this deployment has a demo workspace at
	// all. The frontend offers the "Try the demo workspace" card only when it
	// does — a deployment without one must not advertise a mode it cannot
	// enter.
	DemoAvailable bool `json:"demo_available"`
	// DemoSources names the sources the demo workspace covers, so the card can
	// say what trying it would actually show.
	DemoSources []string `json:"demo_sources,omitempty"`
	// IndexingAvailable reports whether this deployment has an indexed corpus:
	// the Sources view and its refresh are features of that, and the frontend
	// hides them when there is none.
	//
	// Deliberately not the same question as DemoAvailable, though it is easy to
	// assume so. Indexing needs BOTH a demo workspace to crawl AND the server's
	// own OpenAI key to embed it with, so a demo configured without that key
	// has demo_available true and indexing_available false — and gating the nav
	// on the wrong one of the two puts a Sources tab in front of a user whose
	// every refresh would answer 503.
	IndexingAvailable bool `json:"indexing_available"`
}

// jiraIdentity is the display-facts blob stored for a jira connection.
type jiraIdentity struct {
	SiteURL     string `json:"site_url"`
	AccountName string `json:"account_name"`
	Email       string `json:"email,omitempty"`
}

// notionIdentity is the display-facts blob stored for a notion connection.
type notionIdentity struct {
	BotName       string `json:"bot_name"`
	WorkspaceName string `json:"workspace_name,omitempty"`
}

// handleGetConnections reports the user's mode, demo toggle, and per-source
// connection status.
func (s *Server) handleGetConnections(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFrom(r.Context())
	if !ok {
		writeError(w, s.deps.Logger, http.StatusInternalServerError, "internal error")
		return
	}
	infos, err := s.deps.Connections.List(r.Context(), user.ID)
	if err != nil {
		s.deps.Logger.Error("connections: list", "error", err)
		writeError(w, s.deps.Logger, http.StatusInternalServerError, "internal error")
		return
	}
	useDemo, err := s.deps.Connections.UseDemo(r.Context(), user.ID)
	if err != nil {
		s.deps.Logger.Error("connections: demo toggle", "error", err)
		writeError(w, s.deps.Logger, http.StatusInternalServerError, "internal error")
		return
	}

	resp := connectionsResponse{
		Mode:              connections.Mode(len(infos), useDemo, s.deps.Connections.DemoAvailable()),
		UseDemoWorkspace:  useDemo,
		DemoAvailable:     s.deps.Connections.DemoAvailable(),
		DemoSources:       s.deps.Connections.DemoSources(),
		IndexingAvailable: len(s.deps.IndexSources) > 0,
		Sources: map[string]sourceStatus{
			connections.SourceJira:   {Status: "absent"},
			connections.SourceNotion: {Status: "absent"},
			connections.SourceGmail:  {Status: "absent"},
		},
	}
	for _, info := range infos {
		status := "connected"
		if info.Status == "error" {
			status = "error"
		}
		updatedAt := info.UpdatedAt
		resp.Sources[info.Source] = sourceStatus{
			Status:        status,
			Identity:      info.Identity,
			UpdatedAt:     &updatedAt,
			LastError:     info.LastError,
			WritesEnabled: info.WritesEnabled,
		}
	}
	writeJSON(w, s.deps.Logger, http.StatusOK, resp)
}

// putJiraConnectionRequest is the PUT /api/connections/jira body.
type putJiraConnectionRequest struct {
	BaseURL  string `json:"base_url"`
	Email    string `json:"email"`
	APIToken string `json:"api_token"`
}

// handlePutJiraConnection validates a pasted Jira token with one live call to
// /rest/api/3/myself and stores it encrypted. 422 carries Atlassian's own
// reason so the user can fix their paste; nothing is stored on failure and
// nothing about the token is ever logged.
func (s *Server) handlePutJiraConnection(w http.ResponseWriter, r *http.Request) {
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
	var req putJiraConnectionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, logger, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Email = strings.TrimSpace(req.Email)
	req.APIToken = strings.TrimSpace(req.APIToken)

	baseURL, err := connections.ValidateJiraBaseURL(req.BaseURL)
	if err != nil {
		writeError(w, logger, http.StatusUnprocessableEntity, err.Error())
		return
	}
	req.BaseURL = baseURL
	if req.Email == "" || req.APIToken == "" {
		writeError(w, logger, http.StatusUnprocessableEntity, "email and api_token are required")
		return
	}

	client, err := jira.NewClient(jira.Config{
		BaseURL:       req.BaseURL,
		Email:         req.Email,
		APIToken:      req.APIToken,
		AllowUnscoped: true, // validating the user's own credential against their own site
		Logger:        logger,
	})
	if err != nil {
		writeError(w, logger, http.StatusUnprocessableEntity, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), validateTimeout)
	defer cancel()
	account, err := client.Myself(ctx)
	if err != nil {
		var apiErr *jira.APIError
		if errors.As(err, &apiErr) {
			// Atlassian's own words about the user's own token — safe to
			// return, deliberately not logged.
			writeError(w, logger, http.StatusUnprocessableEntity, apiErr.Error())
			return
		}
		logger.Error("connections: jira validation call failed", "error", err)
		writeError(w, logger, http.StatusBadGateway, "could not reach your Jira site to validate the token — check the URL and try again")
		return
	}

	identity := jiraIdentity{SiteURL: req.BaseURL, AccountName: account.DisplayName, Email: account.Email}
	creds := connections.JiraCredentials{BaseURL: req.BaseURL, Email: req.Email, APIToken: req.APIToken}
	if err := s.deps.Connections.Save(r.Context(), user.ID, connections.SourceJira, creds, identity); err != nil {
		logger.Error("connections: save jira", "error", err)
		writeError(w, logger, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, logger, http.StatusOK, sourceStatus{Status: "connected", Identity: marshalOrNull(logger, identity)})
}

// putNotionConnectionRequest is the PUT /api/connections/notion body.
type putNotionConnectionRequest struct {
	Token string `json:"token"`
}

// handlePutNotionConnection validates a pasted Notion integration token with
// one live call to /v1/users/me and stores it encrypted. Same contract as the
// jira flow: 422 with the provider's reason, nothing stored on failure.
func (s *Server) handlePutNotionConnection(w http.ResponseWriter, r *http.Request) {
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
	var req putNotionConnectionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, logger, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Token = strings.TrimSpace(req.Token)
	if req.Token == "" {
		writeError(w, logger, http.StatusUnprocessableEntity, "token is required")
		return
	}

	client, err := notion.NewClient(notion.Config{
		Token:   req.Token,
		BaseURL: s.deps.NotionBaseURL,
		Logger:  logger,
	})
	if err != nil {
		writeError(w, logger, http.StatusUnprocessableEntity, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), validateTimeout)
	defer cancel()
	bot, err := client.CurrentBot(ctx)
	if err != nil {
		var apiErr *notion.APIError
		if errors.As(err, &apiErr) {
			writeError(w, logger, http.StatusUnprocessableEntity, apiErr.Error())
			return
		}
		logger.Error("connections: notion validation call failed", "error", err)
		writeError(w, logger, http.StatusBadGateway, "could not reach Notion to validate the token — try again")
		return
	}

	identity := notionIdentity{BotName: bot.Name, WorkspaceName: bot.WorkspaceName}
	creds := connections.NotionCredentials{Token: req.Token}
	if err := s.deps.Connections.Save(r.Context(), user.ID, connections.SourceNotion, creds, identity); err != nil {
		logger.Error("connections: save notion", "error", err)
		writeError(w, logger, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, logger, http.StatusOK, sourceStatus{Status: "connected", Identity: marshalOrNull(logger, identity)})
}

// handleDeleteConnection removes one connection. 404 when none was stored —
// and for unknown source names, which keeps /connections/mode unreachable
// here (PUT and DELETE differ anyway; this is belt and braces).
func (s *Server) handleDeleteConnection(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFrom(r.Context())
	if !ok {
		writeError(w, s.deps.Logger, http.StatusInternalServerError, "internal error")
		return
	}
	source := chi.URLParam(r, "source")
	if !connections.ValidSource(source) {
		writeError(w, s.deps.Logger, http.StatusNotFound, "unknown source")
		return
	}
	err := s.deps.Connections.Delete(r.Context(), user.ID, source)
	if errors.Is(err, connections.ErrNoConnection) {
		writeError(w, s.deps.Logger, http.StatusNotFound, "no connection on file")
		return
	}
	if err != nil {
		s.deps.Logger.Error("connections: delete", "error", err)
		writeError(w, s.deps.Logger, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// putConnectionsModeRequest is the PUT /api/connections/mode body.
type putConnectionsModeRequest struct {
	UseDemoWorkspace bool `json:"use_demo_workspace"`
}

// handlePutConnectionsMode flips the "Use demo workspace" toggle.
func (s *Server) handlePutConnectionsMode(w http.ResponseWriter, r *http.Request) {
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
	var req putConnectionsModeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, logger, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// Switching the toggle ON needs something to switch on to. The UI only
	// renders the control when a demo exists, so this refuses a request the
	// product cannot produce — but it is the API that decides, and a stored
	// true on a deployment with no demo is exactly the stale row that used to
	// leave a user unable to chat and unable to see the control that would fix
	// it. Switching OFF is always allowed: withdrawing is never blocked.
	if req.UseDemoWorkspace && !s.deps.Connections.DemoAvailable() {
		writeError(w, logger, http.StatusConflict,
			"this deployment has no demo workspace — connect your own source instead")
		return
	}
	if err := s.deps.Connections.SetUseDemo(r.Context(), user.ID, req.UseDemoWorkspace); err != nil {
		logger.Error("connections: set demo toggle", "error", err)
		writeError(w, logger, http.StatusInternalServerError, "internal error")
		return
	}
	// Return the fresh overview so the page can re-render without a second
	// request.
	s.handleGetConnections(w, r)
}

// marshalOrNull marshals a value the handler itself just built; a failure is
// a programming error worth logging, and the response degrades to null (the
// connection is already stored by the time this runs).
func marshalOrNull(logger *slog.Logger, v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		logger.Error("connections: marshal identity", "error", err)
		return nil
	}
	return b
}
