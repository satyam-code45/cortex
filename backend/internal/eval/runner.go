package eval

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/agent"
	"cortex/internal/llm"
	"cortex/internal/store"
)

// evalUserEmail keeps eval traffic under its own user, so eval conversations
// never appear in the dev user's conversation list in the UI.
const evalUserEmail = "eval@cortex.local"

// statusPending mirrors the chat handler's initial run status.
const statusPending = "pending"

// RunnerConfig configures a Runner.
type RunnerConfig struct {
	DB           *pgxpool.Pool
	Orchestrator *agent.Orchestrator
	Grader       *Grader
	// Model is recorded on each agent_runs row, mirroring the chat handler.
	Model string
	// Concurrency is the worker-pool size; the -concurrency flag, default 4.
	Concurrency int
	// UserEmail owns the eval's conversations; the -user flag, defaulting to
	// evalUserEmail so eval artifacts keep landing under the same user.
	UserEmail string
	Logger    *slog.Logger
}

// Runner executes eval cases through the real agent.
//
// It drives the orchestrator directly rather than through River: queue
// semantics are not what an eval measures, and the graded artifact — the
// agent_runs, evidence, citations, and tool_calls rows — is identical either
// way.
type Runner struct {
	db          *pgxpool.Pool
	orch        *agent.Orchestrator
	grader      *Grader
	model       string
	concurrency int
	userEmail   string
	logger      *slog.Logger
}

// NewRunner validates the configuration and builds a Runner.
func NewRunner(cfg RunnerConfig) (*Runner, error) {
	if cfg.DB == nil {
		return nil, errors.New("eval: DB is required")
	}
	if cfg.Orchestrator == nil {
		return nil, errors.New("eval: Orchestrator is required")
	}
	if cfg.Grader == nil {
		return nil, errors.New("eval: Grader is required")
	}
	if cfg.Model == "" {
		return nil, errors.New("eval: Model is required")
	}
	concurrency := cfg.Concurrency
	if concurrency <= 0 {
		concurrency = 4
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	userEmail := cfg.UserEmail
	if userEmail == "" {
		userEmail = evalUserEmail
	}
	return &Runner{
		db:          cfg.DB,
		orch:        cfg.Orchestrator,
		grader:      cfg.Grader,
		model:       cfg.Model,
		concurrency: concurrency,
		userEmail:   userEmail,
		logger:      logger,
	}, nil
}

// Run executes every case and grades it. Results come back sorted by case ID
// so the output is deterministic regardless of worker-pool scheduling.
func (r *Runner) Run(ctx context.Context, cases []Case) []CaseResult {
	pending := make(chan Case)
	results := make(chan CaseResult)

	var wg sync.WaitGroup
	for range r.concurrency {
		wg.Go(func() {
			for c := range pending {
				results <- r.runCase(ctx, c)
			}
		})
	}
	go func() {
		for _, c := range cases {
			pending <- c
		}
		close(pending)
		wg.Wait()
		close(results)
	}()

	collected := make([]CaseResult, 0, len(cases))
	for result := range results {
		r.logger.Info("eval: case graded",
			"case", result.CaseID, "status", result.Status, "tool_calls", result.ToolCalls)
		collected = append(collected, result)
	}
	sort.Slice(collected, func(i, j int) bool { return collected[i].CaseID < collected[j].CaseID })
	return collected
}

// runCase is one case end to end: create the run rows, execute the agent,
// read the outcome back, grade it.
func (r *Runner) runCase(ctx context.Context, c Case) CaseResult {
	result := CaseResult{CaseID: c.ID, Category: c.Category, Metrics: make(map[string]MetricResult)}

	runID, userID, err := r.setupRun(ctx, c)
	if err != nil {
		r.logger.Error("eval: case setup failed", "case", c.ID, "error", err)
		result.HarnessError = fmt.Sprintf("setup: %v", err)
		return result
	}
	result.RunID = runID

	// The same call the River worker makes. Run only returns an error when
	// the outcome could not be recorded; an ordinary failure is already in
	// agent_runs as status=failed and is graded below like any other outcome.
	if err := r.orch.Run(ctx, runID); err != nil {
		r.logger.Error("eval: agent run unrecordable", "case", c.ID, "run_id", runID, "error", err)
		result.HarnessError = fmt.Sprintf("run: %v", err)
		return result
	}

	q := store.New(r.db)
	run, err := q.GetAgentRunForUser(ctx, store.GetAgentRunForUserParams{ID: runID, UserID: userID})
	if err != nil {
		result.HarnessError = fmt.Sprintf("read back run: %v", err)
		return result
	}
	evidence, err := q.ListEvidenceByRun(ctx, runID)
	if err != nil {
		result.HarnessError = fmt.Sprintf("read back evidence: %v", err)
		return result
	}
	citations, err := q.ListCitationsByRun(ctx, runID)
	if err != nil {
		result.HarnessError = fmt.Sprintf("read back citations: %v", err)
		return result
	}
	toolCalls, err := q.ListToolCallsByRun(ctx, runID)
	if err != nil {
		result.HarnessError = fmt.Sprintf("read back tool calls: %v", err)
		return result
	}
	// The faithfulness judge grades against what the model actually read, not
	// only the ~300-char evidence snippets. The observations are rebuilt from
	// the run's event log — the same replay pause/resume relies on.
	events, err := q.ListRunEventsByRun(ctx, runID)
	if err != nil {
		result.HarnessError = fmt.Sprintf("read back run events: %v", err)
		return result
	}
	_, transcript, err := agent.ReconstructTranscript(events)
	if err != nil {
		result.HarnessError = fmt.Sprintf("reconstruct transcript: %v", err)
		return result
	}
	var observations []string
	for _, m := range transcript {
		if m.Role == llm.RoleTool {
			observations = append(observations, m.Content)
		}
	}

	result.Status = run.Status
	result.ToolCalls = len(toolCalls)
	if run.Answer != nil {
		result.Answer = *run.Answer
	}
	if run.Error != nil {
		result.RunError = *run.Error
	}
	if run.LatencyMs != nil {
		result.LatencyMS = int64(*run.LatencyMs)
	}
	if run.InputTokens != nil {
		result.InputTokens = int(*run.InputTokens)
	}
	if run.OutputTokens != nil {
		result.OutputTokens = int(*run.OutputTokens)
	}

	if run.Status != "completed" || result.Answer == "" {
		// A failed run answered nothing: every metric fails, carrying the
		// run's own error so the JSONL says why.
		detail := "run failed: " + result.RunError
		for _, metric := range MetricNames {
			result.Metrics[metric] = MetricResult{Pass: false, Detail: detail}
		}
		return result
	}

	result.Metrics[MetricCorrectness] = r.grader.Correctness(ctx, c, result.Answer)
	result.Metrics[MetricFaithfulness] = r.grader.Faithfulness(ctx, c, result.Answer, evidence, observations)
	result.Metrics[MetricCitationSupport] = r.grader.CitationSupport(ctx, c, result.Answer, citations)
	result.Metrics[MetricCitationSources] = GradeSourceOverlap(c, evidence)
	result.Metrics[MetricEfficiency] = GradeEfficiency(c, len(toolCalls))
	return result
}

// setupRun creates the rows one agent run needs, in one transaction —
// mirroring the chat handler's enqueueRun minus the queue. A fresh
// conversation per case is the isolation: the orchestrator loads history by
// conversation, so each run's transcript starts with exactly its own question,
// and parallel cases share nothing but the pool.
func (r *Runner) setupRun(ctx context.Context, c Case) (runID, userID uuid.UUID, err error) {
	err = store.WithTx(ctx, r.db, func(q store.Querier) error {
		user, err := q.UpsertUser(ctx, r.userEmail)
		if err != nil {
			return fmt.Errorf("upsert eval user: %w", err)
		}
		userID = user.ID

		title := "eval: " + c.ID
		conversation, err := q.CreateConversation(ctx, store.CreateConversationParams{
			UserID: user.ID,
			Title:  &title,
		})
		if err != nil {
			return fmt.Errorf("create conversation: %w", err)
		}

		if _, err := q.InsertMessage(ctx, store.InsertMessageParams{
			ConversationID: conversation.ID,
			Role:           string(llm.RoleUser),
			Content:        c.Question,
		}); err != nil {
			return fmt.Errorf("insert question message: %w", err)
		}

		run, err := q.InsertAgentRun(ctx, store.InsertAgentRunParams{
			ConversationID: conversation.ID,
			Query:          c.Question,
			Status:         statusPending,
			Model:          &r.model,
		})
		if err != nil {
			return fmt.Errorf("insert agent run: %w", err)
		}
		runID = run.ID
		return nil
	})
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	return runID, userID, nil
}
