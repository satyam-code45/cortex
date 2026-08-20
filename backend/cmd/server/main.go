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

	"cortex/internal/api"
	"cortex/internal/config"
	"cortex/internal/llm"
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

	handler := api.NewRouter(api.Deps{
		DB:           pool,
		Provider:     provider,
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
	return <-serveErr
}
