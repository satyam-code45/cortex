// Package app assembles the object graph shared by every Cortex command that
// runs the real agent: the LLM provider, the three source clients, the RAG
// indexer and search tool, the tool registry, and the orchestrator.
//
// cmd/server adds the queue, the workers, and the HTTP router on top;
// cmd/eval drives the orchestrator directly. The graph lives in one place
// because identity matters for the eval's validity: an eval wired with a
// different tool set measures a different agent, and a silently smaller
// registry answers email questions wrongly without ever saying so.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/google/uuid"

	"cortex/internal/agent"
	"cortex/internal/config"
	"cortex/internal/connections"
	"cortex/internal/keys"
	"cortex/internal/llm"
	"cortex/internal/rag"
	"cortex/internal/tools"
	"cortex/internal/tools/gmail"
	"cortex/internal/tools/jira"
	"cortex/internal/tools/notion"
)

// dbConnectTimeout bounds the startup connectivity check.
const dbConnectTimeout = 10 * time.Second

// Options selects per-command behavior of the shared graph.
type Options struct {
	// BYOK makes the orchestrator run each investigation as its owner
	// (cmd/server): on the owner's stored LLM key, and against the owner's
	// connected sources — or the full demo workspace when they have
	// none. Off — cmd/eval — every run uses the server's key and the full
	// demo registry: the eval is a server-initiated operation and must never
	// borrow a user's key or credentials, in either direction.
	BYOK bool
}

// Deps is the assembled graph.
//
// The demo-workspace fields — JiraClient, NotionClient, GmailClient and
// Registry — are nil when their source is not configured, and Registry is nil
// whenever no demo source is. That is a supported deployment, not a partial
// build: users answer from their own connections, and every consumer reads nil
// as "this deployment has no demo workspace" rather than failing.
//
// Provider and Indexer are nil-able for a second, independent reason: they are
// the parts funded by the server's own OPENAI_API_KEY, and without one the
// deployment keeps everything users pay for themselves and loses only the
// indexed corpus.
type Deps struct {
	Pool *pgxpool.Pool
	// Provider is the server's own LLM client, used for embeddings and by
	// non-BYOK processes. Nil when OPENAI_API_KEY is unset.
	Provider     llm.Provider
	Keys         *keys.Service
	Connections  *connections.Service
	JiraClient   *jira.Client
	NotionClient *notion.Client
	GmailClient  *gmail.Client
	Indexer      *rag.Indexer
	Registry     *tools.Registry
	Orchestrator *agent.Orchestrator
	// ConnectionBuilder builds per-user tool and writer registries. Exposed so
	// the server can wire the write execution worker and the write-readiness
	// check to the same construction path a run uses — a write must go out
	// through exactly the client its proposal was validated against.
	//
	// Nil when BYOK is off (cmd/eval), which correctly means that process can
	// neither propose nor execute a write.
	ConnectionBuilder *connections.RegistryBuilder
}

// Build connects to Postgres (with a ping check), constructs every source
// client, and wires the orchestrator. The caller owns Close.
func Build(ctx context.Context, cfg *config.Config, logger *slog.Logger, opts Options) (*Deps, error) {
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	// From here on, a failure must release the pool: Build owns it until it
	// hands Deps back.
	deps, err := build(ctx, pool, cfg, logger, opts)
	if err != nil {
		pool.Close()
		return nil, err
	}
	return deps, nil
}

// Close releases the resources Build acquired.
func (d *Deps) Close() {
	d.Pool.Close()
}

func build(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config, logger *slog.Logger, opts Options) (*Deps, error) {
	pingCtx, cancelPing := context.WithTimeout(ctx, dbConnectTimeout)
	defer cancelPing()
	if err := pool.Ping(pingCtx); err != nil {
		return nil, fmt.Errorf("ping database: %w", err)
	}
	logger.Info("connected to database")

	// The server's own provider, which pays for embeddings and nothing else:
	// every investigation runs on its owner's stored key. Without a server key
	// it stays nil, and the two things that need it — the indexing crawl and
	// the knowledge_base tool — are dropped below rather than built around a
	// client that would 401 on first use.
	//
	// Declared as the interface, not the concrete type, so that "no server key"
	// is a nil the callers below can actually test. A *llm.OpenAI assigned into
	// an llm.Provider is non-nil however empty it is.
	var provider llm.Provider
	if cfg.OpenAIAPIKey != "" {
		provider = llm.NewOpenAI(llm.OpenAIConfig{
			APIKey:         cfg.OpenAIAPIKey,
			BaseURL:        cfg.OpenAIBaseURL,
			DefaultModel:   cfg.LLMModel,
			EmbeddingModel: cfg.EmbeddingModel,
		})
	}

	// Each demo client is built only if its source is configured. A deployment
	// with none of them is a complete product — every user answers from their
	// own connections — so a nil client here is a supported state that the
	// registry, indexer and API all read as "this demo source does not exist".
	var err error
	var jiraClient *jira.Client
	if cfg.DemoJira {
		jiraClient, err = jira.NewClient(jira.Config{
			BaseURL:  cfg.JiraBaseURL,
			Email:    cfg.JiraEmail,
			APIToken: cfg.JiraAPIToken,
			// The demo site belongs to whoever runs this deployment and its API
			// token can see every project on it, so JIRA_PROJECTS is enforced on
			// the client rather than left to the model's JQL. This is the Jira
			// half of what GMAIL_QUERY_SCOPE does for the demo mailbox.
			Projects: cfg.JiraProjects,
			Logger:   logger,
		})
		if err != nil {
			return nil, err
		}
	}

	var notionClient *notion.Client
	if cfg.DemoNotion {
		notionClient, err = notion.NewClient(notion.Config{
			Token:  cfg.NotionToken,
			Logger: logger,
		})
		if err != nil {
			return nil, err
		}
	}

	var gmailClient *gmail.Client
	if cfg.DemoGmail {
		gmailClient, err = buildGmailClient(cfg, logger)
		if err != nil {
			return nil, err
		}
	}

	// The indexing sources are the same three clients the tools use, wrapped so
	// the RAG layer can crawl them. Sharing the client means the index is built
	// from exactly the text the agent reads live — and, for Gmail, that the crawl
	// honours GMAIL_QUERY_SCOPE, so a scoped deployment cannot quietly embed
	// personal mail.
	var indexSources []tools.DocumentSource
	if jiraClient != nil {
		indexSources = append(indexSources, jira.NewSource(jiraClient, cfg.IndexMaxDocuments, cfg.JiraProjects, logger))
	}
	if notionClient != nil {
		indexSources = append(indexSources, notion.NewSource(notionClient, cfg.IndexMaxDocuments, logger))
	}
	if gmailClient != nil {
		indexSources = append(indexSources, gmail.NewSource(gmailClient, cfg.IndexMaxDocuments, logger))
	}

	// No demo sources means nothing to crawl, and rag.New rightly refuses to
	// build an indexer over an empty source list. The indexer is therefore nil
	// on a demo-less deployment, and the server registers no indexing worker
	// and reports no indexable sources — the Sources view and its refresh are
	// demo features, so they disappear rather than erroring.
	//
	// Indexing also needs the server key, since embedding a corpus is the one
	// thing the server pays for itself. Missing key and demo sources present is
	// a legitimate configuration — show the demo, don't fund a corpus over it —
	// so it loses the knowledge base and keeps everything else.
	var indexer *rag.Indexer
	switch {
	case len(indexSources) > 0 && provider == nil:
		logger.Warn("indexing disabled: OPENAI_API_KEY is not set, so the demo corpus cannot be " +
			"embedded — the demo's live source tools still work, but search_knowledge_base is not registered")
	case len(indexSources) > 0:
		indexer, err = rag.New(rag.Config{
			DB:           pool,
			Embedder:     provider,
			Sources:      indexSources,
			MaxDocuments: cfg.IndexMaxDocuments,
			// Passed explicitly rather than left to the zero value. rag.Config
			// treats a zero OverlapTokens as "no overlap" — a legitimate thing for a
			// caller to ask for — so omitting it here silently indexed production
			// with none, instead of the intended 500-token chunks / 50 overlap.
			ChunkTokens:   rag.DefaultChunkTokens,
			OverlapTokens: rag.DefaultOverlapTokens,
			Logger:        logger,
		})
		if err != nil {
			return nil, err
		}
	}

	// The demo registry covers exactly the demo sources that exist, plus the
	// knowledge base over them. With no demo sources there is no demo registry
	// at all: demo mode is unreachable, and the connections builder reads a nil
	// here as "this deployment has no demo workspace".
	//
	// The knowledge base is part of the demo registry and only of it, because
	// the indexed corpus IS the demo workspace — offering it to a user
	// answering from their own Jira would let demo content into an answer about
	// their real data.
	var registry *tools.Registry
	if cfg.HasDemoWorkspace() {
		var demoTools []tools.Tool
		if jiraClient != nil {
			demoTools = append(demoTools, jira.NewTools(jiraClient)...)
		}
		if notionClient != nil {
			demoTools = append(demoTools, notion.NewTools(notionClient)...)
		}
		if gmailClient != nil {
			demoTools = append(demoTools, gmail.NewTools(gmailClient)...)
		}

		// The knowledge base exists only where the corpus does. It searches
		// what the indexer wrote and embeds each query to do it, so an indexer
		// that was never built means there is nothing to search and no way to
		// search it — registering the tool anyway would put a name in the
		// model's tool list whose every call fails.
		if indexer != nil {
			knowledgeBase, kbErr := rag.NewSearchTool(rag.SearchConfig{
				DB:       pool,
				Embedder: provider,
				Sources:  indexer.Sources(),
				Logger:   logger,
			})
			if kbErr != nil {
				return nil, kbErr
			}
			demoTools = append(demoTools, knowledgeBase)
		}

		registry, err = tools.NewRegistry(demoTools...)
		if err != nil {
			return nil, err
		}
	}

	// cmd/eval runs every case against the demo workspace by design — it is a
	// server-initiated operation and must never borrow a user's credentials — so
	// without one it has nothing to evaluate. Said here, where the cause is
	// obvious, rather than as agent.New's generic "one of Registry or
	// RegistryForUser is required".
	if !opts.BYOK && registry == nil {
		return nil, errors.New("app: no demo workspace is configured, so there is nothing for a non-BYOK process (cmd/eval) to run against; configure a demo source or run the server instead")
	}
	// The same reasoning for the key. A non-BYOK process has no run owner to
	// borrow one from, so the server key is the only thing that can pay for its
	// completions and its judges — and unlike the server, it cannot degrade to
	// a smaller feature set and still be doing its job.
	if !opts.BYOK && provider == nil {
		return nil, errors.New("app: OPENAI_API_KEY is not set, and a non-BYOK process (cmd/eval) has no run owner whose key it could spend instead; set it to run the eval suite")
	}

	cipher, err := keys.NewCipher(cfg.LLMKeyEncryptionSecret)
	if err != nil {
		return nil, err
	}
	keyService := keys.NewService(pool, cipher)
	connectionService := connections.NewService(pool, cipher, cfg.DemoSources()...)

	var connectionBuilder *connections.RegistryBuilder

	agentConfig := agent.Config{
		DB:                   pool,
		Provider:             provider,
		Registry:             registry,
		Model:                cfg.LLMModel,
		UtilityModel:         cfg.LLMUtilityModel,
		MaxIterations:        cfg.MaxIterations,
		ContextTokenBudget:   cfg.ContextTokenBudget,
		WritesPerUserPerHour: cfg.WritesPerUserPerHour,
		Logger:               logger,
	}
	if opts.BYOK {
		// The completion/embedding cut: completions run
		// on the run owner's key via this factory; embeddings — the indexer
		// above and the knowledge_base query embedder below — stay on the
		// server's `provider`, because they read the server's own index and the
		// registry (with its per-upstream pacing state) is shared across runs.
		agentConfig.ProviderForUser = func(ctx context.Context, userID uuid.UUID) (llm.Provider, error) {
			userProvider, key, err := keyService.Get(ctx, userID)
			// Only conditions that cannot heal on retry are marked
			// ErrLLMKeyUnavailable (the orchestrator fails the run for those);
			// a transient database error passes through plain, and River
			// retries the job.
			if errors.Is(err, keys.ErrNoKey) || errors.Is(err, keys.ErrUnusableKey) {
				return nil, fmt.Errorf("%w: %w", agent.ErrLLMKeyUnavailable, err)
			}
			if err != nil {
				return nil, err
			}
			if userProvider != "openai" {
				return nil, fmt.Errorf("%w: provider %q is not implemented", agent.ErrLLMKeyUnavailable, userProvider)
			}
			return llm.NewOpenAI(llm.OpenAIConfig{
				APIKey:         key,
				BaseURL:        cfg.OpenAIBaseURL,
				DefaultModel:   cfg.LLMModel,
				EmbeddingModel: cfg.EmbeddingModel,
			}), nil
		}

		// The same cut for tools: each run's registry is built from
		// its owner's source connections — all demo or all theirs, never
		// mixed. The demo registry instance is shared so its per-upstream
		// pacing state stays shared across demo-mode runs.
		builder, err := connections.NewRegistryBuilder(connections.RegistryBuilderConfig{
			Service:                 connectionService,
			Demo:                    registry,
			GoogleClientID:          cfg.GoogleOAuthClientID,
			GoogleClientSecret:      cfg.GoogleOAuthClientSecret,
			GmailSendAllowedDomains: cfg.GmailSendAllowedDomains,
			Logger:                  logger,
		})
		if err != nil {
			return nil, err
		}
		agentConfig.RegistryForUser = builder.ForUser
		connectionBuilder = builder
	}
	orchestrator, err := agent.New(agentConfig)
	if err != nil {
		return nil, err
	}

	return &Deps{
		Pool:              pool,
		Provider:          provider,
		Keys:              keyService,
		Connections:       connectionService,
		JiraClient:        jiraClient,
		NotionClient:      notionClient,
		GmailClient:       gmailClient,
		Indexer:           indexer,
		Registry:          registry,
		Orchestrator:      orchestrator,
		ConnectionBuilder: connectionBuilder,
	}, nil
}

// buildGmailClient wires the cached refresh token into a Gmail client.
//
// This is the one credential the process cannot obtain for itself: the OAuth
// flow needs a human at a browser, so cmd/gmail-auth performs it once and
// leaves a refresh token behind. Failing here — loudly, naming the command that
// fixes it — is the whole point. The alternative, starting without Gmail, gives
// an agent that cannot see a third of the evidence and has no way to know it.
func buildGmailClient(cfg *config.Config, logger *slog.Logger) (*gmail.Client, error) {
	creds, err := gmail.LoadCredentials(cfg.GmailCredentialsPath)
	if err != nil {
		return nil, err
	}
	token, err := gmail.LoadToken(cfg.GmailTokenPath, cfg.GmailTokenJSON)
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
