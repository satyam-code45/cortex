package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/go-chi/chi/v5"

	"cortex/internal/agent"
	"cortex/internal/connections"
)

// PUT /api/connections/{source}/writes — turning writes on or off for one
// source.
//
// A separate endpoint from connecting, deliberately, and the sequence matters:
// connecting a source gives Cortex the ability to read it, and nothing else. The
// ability to change somebody's Jira or send mail as them is granted by this call
// and only by this call, one source at a time, by the person whose account it
// is.
//
// Enabling is checked against the credential first, so a user is never told
// writes are on for a connection that cannot perform them. What can be checked
// differs per source and the reasons are in connections.CheckWriteReadiness.
// Gmail is the special case: sending needs a scope only a fresh consent screen
// can grant, so this endpoint refuses and points at the reconnect flow rather
// than silently enabling something that would fail after somebody approved it.
//
// Disabling is unconditional. Revoking permission must never depend on an
// upstream system being reachable.

// putConnectionWritesRequest is the PUT body.
type putConnectionWritesRequest struct {
	Enabled bool `json:"enabled"`
}

// handlePutConnectionWrites enables or disables writes for one source.
func (s *Server) handlePutConnectionWrites(w http.ResponseWriter, r *http.Request) {
	logger := s.deps.Logger
	// Granting write capability is part of the same human-only guarantee as
	// approving a write: a token holder who could switch writes on has done
	// most of the work of sending mail as the owner.
	user, ok := s.requireHuman(w, r)
	if !ok {
		return
	}

	source := chi.URLParam(r, "source")
	if !connections.ValidSource(source) {
		writeError(w, logger, http.StatusBadRequest, "unknown source")
		return
	}

	if !hasJSONContentType(r) {
		writeError(w, logger, http.StatusUnsupportedMediaType, "content-type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	var req putConnectionWritesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, logger, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// Demo mode has no write path at all, and refusing here is the second of two
	// independent guards rather than the only one — a demo-mode run is handed the
	// shared demo registry, which contains no write tool whatsoever. The demo
	// workspace is a real Jira site and a real mailbox belonging to a real
	// person; a signed-in stranger must not be able to propose a write against
	// it, let alone execute one.
	if req.Enabled {
		infos, err := s.deps.Connections.List(r.Context(), user.ID)
		if err != nil {
			logger.Error("connections: list for writes toggle", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}
		useDemo, err := s.deps.Connections.UseDemo(r.Context(), user.ID)
		if err != nil {
			logger.Error("connections: demo toggle for writes toggle", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}
		if connections.Mode(len(infos), useDemo, s.deps.Connections.DemoAvailable()) == agent.ModeDemo {
			writeError(w, logger, http.StatusConflict,
				"writes are not available on the demo workspace — it belongs to somebody else. "+
					"Connect your own source and switch off the demo workspace first")
			return
		}

		// Refuse a source the user has not connected before consulting the
		// readiness check. The check would reach the same answer — it returns
		// ErrNoConnection — but the list is already in hand, and asking whether
		// an absent credential can write is a question with no meaning.
		if !slices.ContainsFunc(infos, func(info connections.Info) bool { return info.Source == source }) {
			writeError(w, logger, http.StatusNotFound, "no connection for that source")
			return
		}

		if s.deps.WriteReadiness == nil {
			writeError(w, logger, http.StatusServiceUnavailable, "writes are not available on this deployment")
			return
		}
		switch err := s.deps.WriteReadiness(r.Context(), user.ID, source); {
		case errors.Is(err, connections.ErrNoConnection):
			writeError(w, logger, http.StatusNotFound, "no connection for that source")
			return
		case errors.Is(err, connections.ErrWritesUnavailable):
			// The message is written for the person reading it and names what to
			// do — grant a Jira permission, or reconnect Gmail.
			writeError(w, logger, http.StatusConflict, unwrapUnavailable(err))
			return
		case err != nil:
			// Unreachable right now, which is not the same as unauthorized.
			logger.Error("connections: write readiness check failed", "error", err, "source", source)
			writeError(w, logger, http.StatusBadGateway,
				"could not confirm this connection can perform writes — try again in a moment")
			return
		}
	}

	if err := s.deps.Connections.SetWritesEnabled(r.Context(), user.ID, source, req.Enabled); err != nil {
		if errors.Is(err, connections.ErrNoConnection) {
			writeError(w, logger, http.StatusNotFound, "no connection for that source")
			return
		}
		logger.Error("connections: set writes enabled", "error", err, "source", source)
		writeError(w, logger, http.StatusInternalServerError, "internal error")
		return
	}

	logger.Info("connection writes toggled", "source", source, "enabled", req.Enabled, "user_id", user.ID)
	s.handleGetConnections(w, r)
}

// unwrapUnavailable strips the sentinel prefix so the message reaching the user
// is the sentence written for them, not a wrapped error chain.
func unwrapUnavailable(err error) string {
	msg := err.Error()
	const prefix = "connections: writes unavailable: "
	if len(msg) > len(prefix) && msg[:len(prefix)] == prefix {
		return msg[len(prefix):]
	}
	return msg
}
