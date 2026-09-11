// Command server runs the Cortex HTTP API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"cortex/internal/api"
	"cortex/internal/app"
	"cortex/internal/auth"
	"cortex/internal/config"
	"cortex/internal/jobs"
)

const (
	// shutdownTimeout is how long in-flight requests get to drain.
	shutdownTimeout = 10 * time.Second
	// readHeaderTimeout guards against slow-header (Slowloris) clients.
	readHeaderTimeout = 10 * time.Second
	// readTimeout bounds the whole request read, closing the slow-body variant
	// of the same attack: complete headers followed by a byte every 30s.
	readTimeout = 30 * time.Second
	// writeTimeout must stay above the handler's own LLM timeout, or a slow but
	// successful answer would be cut off mid-response.
	writeTimeout = 90 * time.Second
	// idleTimeout reaps keep-alive connections; it otherwise defaults to
	// readTimeout and never fires when that is zero.
	idleTimeout = 120 * time.Second
	// queueDrainTimeout is how long in-flight agent runs get to finish on
	// shutdown. It is generous on purpose: a run that is killed has already paid
	// for its LLM calls, so finishing is cheaper than retrying from the start.
	queueDrainTimeout = 60 * time.Second
)

// apiOrigin is the scheme-and-host this API is reached at, used to build the
// OAuth redirect URIs.
//
// Google matches redirect URIs as exact strings, so these have to be the URL a
// browser actually arrives on: a hosted deployment that kept the localhost form
// would send the user's browser to their own machine after consent. Falls back
// to localhost so local development needs no configuration.
func apiOrigin(cfg *config.Config) string {
	if cfg.APIPublicURL != "" {
		return cfg.APIPublicURL
	}
	return "http://localhost:" + cfg.Port
}

// publicHostname is the bare hostname of APIPublicURL, for the Host-header
// allowlist. Empty when this is a local run, where only loopback is answered to.
func publicHostname(cfg *config.Config) string {
	if cfg.APIPublicURL == "" {
		return ""
	}
	parsed, err := url.Parse(cfg.APIPublicURL)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

// cookieSecure reports whether the auth cookies must be marked Secure.
//
// Not inferable from the request: behind a platform proxy TLS terminates at the
// edge and the request reaches this process as plain HTTP, so r.TLS is nil on a
// site the browser loaded over https. The configured public URL is what says
// how the browser actually reached us.
func cookieSecure(cfg *config.Config) bool {
	return strings.HasPrefix(cfg.APIPublicURL, "https://")
}

// cookieCrossSite reports whether the frontend is on a different host than the
// API, which is what forces SameSite=None on the auth cookies.
//
// Compared by host rather than by registrable domain, which errs toward None:
// api.example.com and app.example.com are technically one site and would work
// with Lax, but treating them as cross-site only costs the Lax CSRF hint, which
// the single-origin CORS grant and the JSON content-type requirement already
// cover. Getting it wrong the other way costs a login that always answers 401.
//
// Empty APIPublicURL means a local run, where the frontend and API are both on
// localhost and genuinely same-site.
func cookieCrossSite(cfg *config.Config) bool {
	if cfg.APIPublicURL == "" {
		return false
	}
	api, err := url.Parse(cfg.APIPublicURL)
	if err != nil {
		return false
	}
	frontend, err := url.Parse(cfg.FrontendOrigin)
	if err != nil {
		return false
	}
	return !strings.EqualFold(api.Hostname(), frontend.Hostname())
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("server exited with error", "error", err)
		os.Exit(1)
	}
	logger.Info("server stopped")
}

// run wires the process together and blocks until shutdown completes.
//
// It takes the signal context first so that Ctrl-C during startup — for
// example while the database is unreachable — still aborts promptly.
func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// The shared graph — provider, source clients, indexer, tool registry,
	// orchestrator — is assembled by internal/app so cmd/eval runs exactly the
	// same agent this server does.
	// BYOK on: each run's completion calls are made on its owner's stored key.
	deps, err := app.Build(ctx, cfg, logger, app.Options{BYOK: true})
	if err != nil {
		return err
	}
	defer deps.Close()
	logger.Info("tools registered",
		"count", deps.Registry.Len(), "names", strings.Join(deps.Registry.Names(), ", "))

	worker, err := jobs.NewAgentRunWorker(deps.Orchestrator, logger)
	if err != nil {
		return err
	}

	indexWorker, err := jobs.NewIndexSourceWorker(deps.Indexer, logger)
	if err != nil {
		return err
	}

	resumeWorker, err := jobs.NewResumeRunWorker(deps.Orchestrator, logger)
	if err != nil {
		return err
	}

	// The write path exists only where per-user connections do. Without the
	// builder there is no credential to write with and no registry to resolve an
	// action against, so the workers are simply not registered — a deployment
	// that cannot write is one that has no way to.
	var writeWorker *jobs.ExecuteWriteWorker
	var expireWorker *jobs.ExpireActionsWorker
	if deps.ConnectionBuilder != nil {
		// The queue is injected by jobs.New: these workers enqueue onto the
		// queue they are registered with, which is a construction cycle broken
		// there rather than here.
		writeWorker, err = jobs.NewExecuteWriteWorker(deps.Pool,
			deps.ConnectionBuilder.WritersForUser, nil, logger)
		if err != nil {
			return err
		}
		expireWorker, err = jobs.NewExpireActionsWorker(deps.Pool, cfg.ActionTTL, nil, logger)
		if err != nil {
			return err
		}
	}

	// One process runs both the API and the workers (one binary, one
	// database). River polls Postgres for jobs, so there is nothing to
	// coordinate between them beyond sharing the pool.
	queue, err := jobs.New(jobs.Config{
		Pool:         deps.Pool,
		Worker:       worker,
		IndexWorker:  indexWorker,
		ResumeWorker: resumeWorker,
		WriteWorker:  writeWorker,
		ExpireWorker: expireWorker,
		MaxWorkers:   cfg.AgentRunWorkers,
		IndexWorkers: cfg.IndexWorkers,
		WriteWorkers: cfg.WriteActionWorkers,
		Logger:       logger,
	})
	if err != nil {
		return err
	}
	// Deliberately NOT the signal context. River v0.44 inherits the work context
	// from the start context unless SoftStopTimeout is set, so cancelling this ctx
	// would be equivalent to StopAndCancel: Ctrl-C would kill every in-flight
	// agent run mid-investigation, discarding LLM calls already paid for, and the
	// graceful drain in queue.Stop below would never get a chance to run.
	// Shutdown goes through queue.Stop and nothing else.
	if err := queue.Start(context.WithoutCancel(ctx)); err != nil {
		return err
	}
	logger.Info("queue workers started",
		"agent_queue", jobs.AgentRunQueue, "agent_workers", cfg.AgentRunWorkers,
		"index_queue", jobs.IndexSourceQueue, "index_workers", cfg.IndexWorkers,
		"write_queue", jobs.WriteActionQueue, "write_workers", cfg.WriteActionWorkers,
		"writes_enabled", writeWorker != nil,
		"tools", deps.Registry.Len())

	// Binding beyond loopback is only safe once the Host allowlist knows the
	// name this server is reached at. With API_PUBLIC_URL set that is handled;
	// without it, hostCheck answers 421 to every request that is not addressed
	// to localhost — including the platform's own health check, which reads as
	// a service that never started rather than as a misconfiguration. So the
	// warning fires for exactly that case and names the variable that fixes it.
	if !isLoopback(cfg.Host) && cfg.APIPublicURL == "" {
		logger.Warn("server is bound beyond loopback with no API_PUBLIC_URL set; "+
			"every request not addressed to localhost will be refused with 421",
			"host", cfg.Host)
	}

	handler := api.NewRouter(api.Deps{
		DB:             deps.Pool,
		Enqueuer:       queue,
		Model:          cfg.LLMModel,
		IndexSources:   deps.Indexer.Sources(),
		FrontendOrigin: cfg.FrontendOrigin,
		Logger:         logger,

		OIDC: &auth.OIDC{
			Client: auth.Client{
				ID:       cfg.GoogleOAuthClientID,
				Secret:   cfg.GoogleOAuthClientSecret,
				AuthURL:  auth.GoogleAuthURL,
				TokenURL: auth.GoogleTokenURL,
			},
			JWKS:        auth.NewJWKSCache(auth.GoogleJWKSURL, nil),
			RedirectURI: apiOrigin(cfg) + "/api/auth/google/callback",
		},
		Keys:                 deps.Keys,
		Connections:          deps.Connections,
		ConnectRedirectURI:   apiOrigin(cfg) + "/api/connections/gmail/callback",
		PublicHostname:       publicHostname(cfg),
		CookieSecure:         cookieSecure(cfg),
		CookieCrossSite:      cookieCrossSite(cfg),
		APIToken:             cfg.AuthAPIToken,
		BearerEmail:          cfg.DevUserEmail,
		AllowedEmails:        lowered(cfg.AuthAllowedEmails),
		AdminEmails:          lowered(cfg.AdminEmails),
		RunsPerUserPerHour:   cfg.RunsPerUserPerHour,
		IndexRefreshCooldown: cfg.IndexRefreshCooldown,
		ActionTTL:            cfg.ActionTTL,
		WriteReadiness:       writeReadiness(deps),
		OpenAIBaseURL:        cfg.OpenAIBaseURL,
	})

	srv := &http.Server{
		Addr:              net.JoinHostPort(cfg.Host, cfg.Port),
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	// ListenAndServe blocks, so it runs in its own goroutine and reports a
	// startup failure (a taken port, say) back through this channel.
	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", srv.Addr, "model", cfg.LLMModel)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining", "timeout", shutdownTimeout)
	}

	// The signal context is already cancelled, so the drain gets its own.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	// Workers drain after the HTTP server, so no new run is enqueued while the
	// in-flight ones are finishing.
	queueCtx, cancelQueue := context.WithTimeout(context.Background(), queueDrainTimeout)
	defer cancelQueue()
	if err := queue.Stop(queueCtx); err != nil {
		logger.Error("queue did not drain cleanly", "error", err)
	}

	return <-serveErr
}

// lowered lower-cases every entry, so the allowlist comparison matches the
// lower-cased email the callback extracts from the ID token.
func lowered(items []string) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = strings.ToLower(item)
	}
	return out
}

// isLoopback reports whether the configured bind address reaches only this
// machine. An unparseable or empty host is treated as exposed: warning about a
// safe binding is cheap, staying quiet about an unsafe one is not.
func isLoopback(host string) bool {
	switch host {
	case "localhost":
		return true
	case "":
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// writeReadiness adapts the connection builder's readiness check for the API,
// or returns nil when this deployment has no per-user connections — which the
// handler answers honestly rather than pretending writes can be enabled.
func writeReadiness(deps *app.Deps) func(ctx context.Context, userID uuid.UUID, source string) error {
	if deps.ConnectionBuilder == nil {
		return nil
	}
	return deps.ConnectionBuilder.CheckWriteReadiness
}
