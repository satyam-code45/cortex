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
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/agent"
	"cortex/internal/api"
	"cortex/internal/config"
	"cortex/internal/jobs"
	"cortex/internal/llm"
	"cortex/internal/rag"
	"cortex/internal/tools"
	"cortex/internal/tools/gmail"
	"cortex/internal/tools/jira"
	"cortex/internal/tools/notion"
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

	notionClient, err := notion.NewClient(notion.Config{
		Token:  cfg.NotionToken,
		Logger: logger,
	})
	if err != nil {
		return err
	}

	gmailClient, err := buildGmailClient(cfg, logger)
	if err != nil {
		return err
	}

	// The indexing sources are the same three clients the tools use, wrapped so
	// the RAG layer can crawl them. Sharing the client means the index is built
	// from exactly the text the agent reads live — and, for Gmail, that the crawl
	// honours GMAIL_QUERY_SCOPE, so a scoped deployment cannot quietly embed
	// personal mail.
	indexer, err := rag.New(rag.Config{
		DB:       pool,
		Embedder: provider,
		Sources: []tools.DocumentSource{
			jira.NewSource(jiraClient, cfg.IndexMaxDocuments, cfg.JiraProjects, logger),
			notion.NewSource(notionClient, cfg.IndexMaxDocuments, logger),
			gmail.NewSource(gmailClient, cfg.IndexMaxDocuments, logger),
		},
		MaxDocuments: cfg.IndexMaxDocuments,
		// Passed explicitly rather than left to the zero value. rag.Config
		// treats a zero OverlapTokens as "no overlap" — a legitimate thing for a
		// caller to ask for — so omitting it here silently indexed production
		// with none, against REQ-4.5's 500/50.
		ChunkTokens:   rag.DefaultChunkTokens,
		OverlapTokens: rag.DefaultOverlapTokens,
		Logger:        logger,
	})
	if err != nil {
		return err
	}

	knowledgeBase, err := rag.NewSearchTool(rag.SearchConfig{
		DB:       pool,
		Embedder: provider,
		Sources:  indexer.Sources(),
		Logger:   logger,
	})
	if err != nil {
		return err
	}

	// Nine tools: eight live ones across three sources, plus the knowledge base
	// over all three. The registry is assembled in one place so a missing source
	// is a startup failure rather than a silently smaller tool set: an agent that
	// never learns email exists will still answer a question whose answer is only
	// in email, and it will answer it wrongly.
	registry, err := tools.NewRegistry(slices.Concat(
		jira.NewTools(jiraClient),
		notion.NewTools(notionClient),
		gmail.NewTools(gmailClient),
		[]tools.Tool{knowledgeBase},
	)...)
	if err != nil {
		return err
	}
	logger.Info("tools registered", "count", registry.Len(), "names", strings.Join(registry.Names(), ", "))

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

	indexWorker, err := jobs.NewIndexSourceWorker(indexer, logger)
	if err != nil {
		return err
	}

	// One process runs both the API and the workers (idea.md §1.1: one binary,
	// one database). River polls Postgres for jobs, so there is nothing to
	// coordinate between them beyond sharing the pool.
	queue, err := jobs.New(jobs.Config{
		Pool:         pool,
		Worker:       worker,
		IndexWorker:  indexWorker,
		MaxWorkers:   cfg.AgentRunWorkers,
		IndexWorkers: cfg.IndexWorkers,
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
		"tools", registry.Len())

	// There is no authentication yet, and Day 4 widened what an unauthenticated
	// reader gets: GET /api/runs/{id}/trace returns full tool observations
	// (verbatim email and ticket bodies), and POST /api/admin/index queues paid
	// crawls. The loopback default is what closes all of that, so leaving it is a
	// deliberate exposure and should not be silent. hostCheck still rejects a
	// forged Host, but it cannot tell a LAN peer from localhost.
	if !isLoopback(cfg.Host) {
		logger.Warn("server is bound beyond loopback and the API has no authentication",
			"host", cfg.Host,
			"exposed", "GET /api/runs/{id}/trace, POST /api/chat, POST /api/admin/index")
	}

	handler := api.NewRouter(api.Deps{
		DB:             pool,
		Enqueuer:       queue,
		Model:          cfg.LLMModel,
		DevUserEmail:   devUserEmail,
		IndexSources:   indexer.Sources(),
		FrontendOrigin: cfg.FrontendOrigin,
		Logger:         logger,
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

// buildGmailClient wires the cached refresh token into a Gmail client.
//
// This is the one credential the server cannot obtain for itself: the OAuth
// flow needs a human at a browser, so cmd/gmail-auth performs it once and
// leaves a refresh token behind. Failing here — loudly, naming the command that
// fixes it — is the whole point. The alternative, starting without Gmail, gives
// an agent that cannot see a third of the evidence and has no way to know it.
func buildGmailClient(cfg *config.Config, logger *slog.Logger) (*gmail.Client, error) {
	creds, err := gmail.LoadCredentials(cfg.GmailCredentialsPath)
	if err != nil {
		return nil, err
	}
	token, err := gmail.LoadToken(cfg.GmailTokenPath)
	if err != nil {
		return nil, err
	}
	source, err := gmail.NewTokenSource(gmail.TokenSourceConfig{
		Credentials: creds,
		Token:       token,
		TokenPath:   cfg.GmailTokenPath,
	})
	if err != nil {
		return nil, err
	}
	return gmail.NewClient(gmail.Config{
		TokenSource: source,
		QueryScope:  cfg.GmailQueryScope,
		Logger:      logger,
	})
}
