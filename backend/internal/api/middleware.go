package api

import (
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

// allowedHostnames are the names this server will answer to.
//
// The check exists to close DNS rebinding. Binding to loopback keeps the API off
// the network, and requiring a JSON content type on POST /api/chat means a
// browser must preflight it (no CORS headers are ever sent, so the preflight
// fails) — but neither stops a page on attacker.com whose short-TTL record flips
// to 127.0.0.1. That page becomes same-origin with this server and can then read
// every conversation through GET /api/runs/{id} and queue unbounded paid agent
// runs. A browser always sends the name it dialled in Host, and it is not a
// header script can forge, so comparing it is the cheap and correct defence.
var allowedHostnames = map[string]struct{}{
	"localhost": {},
	"127.0.0.1": {},
	"[::1]":     {},
	"::1":       {},
}

// hostCheck rejects requests whose Host header is not one this server answers to.
func hostCheck(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host := r.Host
			// Host carries the port when it is non-default; only the name matters.
			if name, _, err := net.SplitHostPort(host); err == nil {
				host = name
			}
			if _, ok := allowedHostnames[host]; !ok {
				logger.Warn("rejected request with unexpected Host header",
					"host", r.Host, "path", r.URL.Path)
				writeError(w, logger, http.StatusMisdirectedRequest,
					"unexpected Host header")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requestLogger logs one line per request once it completes, including the
// status, byte count, and duration. It relies on chi's WrapResponseWriter to
// observe the status the handler wrote.
func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			start := time.Now()

			defer func() {
				logger.LogAttrs(r.Context(), slog.LevelInfo, "http request",
					slog.String("request_id", middleware.GetReqID(r.Context())),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.Int("status", ww.Status()),
					slog.Int("bytes", ww.BytesWritten()),
					slog.Duration("duration", time.Since(start)),
				)
			}()

			next.ServeHTTP(ww, r)
		})
	}
}
