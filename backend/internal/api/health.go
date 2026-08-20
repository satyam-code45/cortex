package api

import (
	"context"
	"net/http"
	"time"
)

// healthPingTimeout bounds the database check so an unreachable database fails
// the health check quickly instead of hanging it.
const healthPingTimeout = 2 * time.Second

// handleHealthz reports service health, including database reachability.
//
// GET /healthz → 200 {"status":"ok"} | 503 {"status":"degraded"}
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthPingTimeout)
	defer cancel()

	if err := s.deps.DB.Ping(ctx); err != nil {
		s.deps.Logger.Error("healthz: database ping failed", "error", err)
		writeJSON(w, s.deps.Logger, http.StatusServiceUnavailable, map[string]string{"status": "degraded"})
		return
	}
	writeJSON(w, s.deps.Logger, http.StatusOK, map[string]string{"status": "ok"})
}
