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
	"slices"
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

// Deps is the assembled graph. Every field is non-nil after a successful Build.
type Deps struct {
	Pool         *pgxpool.Pool
	Provider     llm.Provider
	Keys         *keys.Service
	Connections  *connections.Service
	JiraClient   *jira.Client
	NotionClient *notion.Client
	GmailClient  *gmail.Client
	Indexer      *rag.Indexer
	Registry     *tools.Registry
	Orchestrator *agent.Orchestrator
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
		return nil, err
	}

	notionClient, err := notion.NewClient(notion.Config{
		Token:  cfg.NotionToken,
		Logger: logger,
	})
	if err != nil {
		return nil, err
	}

	gmailClient, err := buildGmailClient(cfg, logger)
	if err != nil {
		return nil, err
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
		// with none, instead of the intended 500-token chunks / 50 overlap.
		ChunkTokens:   rag.DefaultChunkTokens,
		OverlapTokens: rag.DefaultOverlapTokens,
		Logger:        logger,
	})
	if err != nil {
		return nil, err
	}

	knowledgeBase, err := rag.NewSearchTool(rag.SearchConfig{
		DB:       pool,
		Embedder: provider,
		Sources:  indexer.Sources(),
		Logger:   logger,
	})
	if err != nil {
		return nil, err
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
		return nil, err
	}

	cipher, err := keys.NewCipher(cfg.LLMKeyEncryptionSecret)
	if err != nil {
		return nil, err
	}
	keyService := keys.NewService(pool, cipher)
	connectionService := connections.NewService(pool, cipher)

	agentConfig := agent.Config{
		DB:                 pool,
		Provider:           provider,
		Registry:           registry,
		Model:              cfg.LLMModel,
		UtilityModel:       cfg.LLMUtilityModel,
		MaxIterations:      cfg.MaxIterations,
		ContextTokenBudget: cfg.ContextTokenBudget,
		Logger:             logger,
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
			Service:            connectionService,
			Demo:               registry,
			GoogleClientID:     cfg.GoogleOAuthClientID,
			GoogleClientSecret: cfg.GoogleOAuthClientSecret,
			Logger:             logger,
		})
		if err != nil {
			return nil, err
		}
		agentConfig.RegistryForUser = builder.ForUser
	}
	orchestrator, err := agent.New(agentConfig)
	if err != nil {
		return nil, err
	}

	return &Deps{
		Pool:         pool,
		Provider:     provider,
		Keys:         keyService,
		Connections:  connectionService,
		JiraClient:   jiraClient,
		NotionClient: notionClient,
		GmailClient:  gmailClient,
		Indexer:      indexer,
		Registry:     registry,
		Orchestrator: orchestrator,
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
