// Command server runs the Cortex HTTP API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/agent"
	"cortex/internal/api"
	"cortex/internal/config"
	"cortex/internal/jobs"
	"cortex/internal/llm"
	"cortex/internal/tools"
	"cortex/internal/tools/jira"
)

const (
	// devUserEmail is the single hardcoded user until auth lands.
	devUserEmail = "dev@cortex.local"

	// dbConnectTimeout bounds the startup connectivity check.
	dbConnectTimeout = 10 * time.Second
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

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()

	pingCtx, cancelPing := context.WithTimeout(ctx, dbConnectTimeout)
	defer cancelPing()
	if err := pool.Ping(pingCtx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	logger.Info("connected to database")

	provider := llm.NewOpenAI(llm.OpenAIConfig{
		APIKey:         cfg.OpenAIAPIKey,
		BaseURL:        cfg.OpenAIBaseURL,
		DefaultModel:   cfg.LLMModel,
		EmbeddingModel: cfg.EmbeddingModel,
	})

	jiraClient, err := jira.NewClient(jira.Config{
		BaseURL:  cfg.JiraBaseURL,
		Email:    cfg.JiraEmail,
		APIToken: cfg.JiraAPIToken,
		Logger:   logger,
	})
	if err != nil {
		return err
	}

	registry, err := tools.NewRegistry(jira.NewTools(jiraClient)...)
	if err != nil {
		return err
	}

	orchestrator, err := agent.New(agent.Config{
		DB:            pool,
		Provider:      provider,
		Registry:      registry,
		Model:         cfg.LLMModel,
		UtilityModel:  cfg.LLMUtilityModel,
		MaxIterations: cfg.MaxIterations,
		Logger:        logger,
	})
	if err != nil {
		return err
	}

	worker, err := jobs.NewAgentRunWorker(orchestrator, logger)
	if err != nil {
		return err
	}

	// One process runs both the API and the workers (idea.md §1.1: one binary,
	// one database). River polls Postgres for jobs, so there is nothing to
	// coordinate between them beyond sharing the pool.
	queue, err := jobs.New(jobs.Config{
		Pool:       pool,
		Worker:     worker,
		MaxWorkers: cfg.AgentRunWorkers,
		Logger:     logger,
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
		"queue", jobs.AgentRunQueue, "workers", cfg.AgentRunWorkers, "tools", registry.Len())

	handler := api.NewRouter(api.Deps{
		DB:           pool,
		Enqueuer:     queue,
		Model:        cfg.LLMModel,
		DevUserEmail: devUserEmail,
		Logger:       logger,
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
