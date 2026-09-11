package api

import (
	"log/slog"
	"net"
	"net/http"
	"strings"
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
//
// extra is the deployment's own public hostname, empty for local development.
// A hosted deployment needs it or the check rejects every request including its
// own health check, which reads as "the service never started" — but it is
// passed in rather than wildcarded, because the whole value of this check is
// that the set of acceptable names is closed and known.
func hostCheck(logger *slog.Logger, extra string) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(allowedHostnames)+1)
	for name := range allowedHostnames {
		allowed[name] = struct{}{}
	}
	if extra = strings.TrimSpace(extra); extra != "" {
		allowed[strings.ToLower(extra)] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host := r.Host
			// Host carries the port when it is non-default; only the name matters.
			if name, _, err := net.SplitHostPort(host); err == nil {
				host = name
			}
			if _, ok := allowed[strings.ToLower(host)]; !ok {
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

// corsMiddleware allows the one configured frontend origin to call the API
// from a browser. Requests whose path is in exemptPaths never receive the
// grant — their preflights are answered 204 with no allow headers, which a
// browser treats as refusal.
//
// Hand-rolled rather than a dependency because the policy is a single exact
// origin: no wildcards, no credentials, no per-route variation beyond the
// exemption list. Everything a CORS library is for — origin lists, regexes,
// reflecting request headers — is exactly what this API must not do.
//
// The header is set on GETs too, not just preflighted POSTs: EventSource does
// not preflight its first connect, but the browser still refuses to deliver
// the stream unless the response itself carries Access-Control-Allow-Origin.
func corsMiddleware(allowedOrigin string, exemptPaths ...string) func(http.Handler) http.Handler {
	exempt := make(map[string]struct{}, len(exemptPaths))
	for _, p := range exemptPaths {
		exempt[p] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Vary unconditionally: whether the CORS headers appear depends on
			// the request Origin, so a cache must not serve one origin's
			// response to another.
			w.Header().Add("Vary", "Origin")

			_, isExempt := exempt[r.URL.Path]

			// The empty check keeps FrontendOrigin="" meaning "no CORS at
			// all": without it, a request with no Origin header would match
			// "" == "" and pick up empty-valued allow headers.
			if !isExempt && allowedOrigin != "" && r.Header.Get("Origin") == allowedOrigin {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", allowedOrigin)
				h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				// Sessions are cookies, and a browser neither sends them
				// cross-origin nor delivers the response without this grant.
				// Safe only because the origin above is a single exact match —
				// Allow-Credentials with a reflected or wildcarded origin would
				// hand every website the user's session.
				h.Set("Access-Control-Allow-Credentials", "true")
				// Last-Event-ID is not CORS-safelisted, and EventSource sends
				// it when it reconnects — only the FIRST connect skips the
				// preflight. Without it here, a dropped SSE connection
				// mid-run could never resume cross-origin: the reconnect's
				// preflight would fail and EventSource gives up for good.
				h.Set("Access-Control-Allow-Headers", "Content-Type, Last-Event-ID")
				// 10 minutes: long enough that the preflight is not paid on
				// every POST, short enough that a policy change lands the same
				// day it ships.
				h.Set("Access-Control-Max-Age", "600")
			}

			// Preflights end here for every origin. Answering 204 without the
			// allow headers is the standard way to refuse one: the browser
			// fails the actual request, and the handler never runs.
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
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
