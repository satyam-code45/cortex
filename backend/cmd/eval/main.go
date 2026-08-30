// Command eval runs the evaluation suite against the real agent.
//
// Each case in evals/cases/*.yaml becomes one agent run — the same
// orchestrator, tool registry, and credentials the server uses, assembled by
// internal/app — and is then graded: correctness, faithfulness and citation
// support by utility-model judges, source overlap and efficiency mechanically.
// Per-case detail goes to evals/results/*.jsonl; the summary is printed and
// persisted to eval_runs so quality is comparable across commits.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"cortex/internal/app"
	"cortex/internal/config"
	"cortex/internal/eval"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("eval failed", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	concurrency := flag.Int("concurrency", 4, "cases run in parallel")
	caseID := flag.String("case", "", "run only the case with this id")
	userEmail := flag.String("user", "eval@cortex.local", "email owning the eval's conversations")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// The full server graph, credentials included: the eval runs the real
	// agent against live sources, or it is not measuring the product.
	// BYOK stays off: the eval is a server-initiated operation, so it runs on
	// the server's key — never a user's.
	deps, err := app.Build(ctx, cfg, logger, app.Options{})
	if err != nil {
		return err
	}
	defer deps.Close()

	cases, err := eval.LoadCases(config.RepoPath("./evals/cases"))
	if err != nil {
		return err
	}
	if *caseID != "" {
		selected := cases[:0]
		for _, c := range cases {
			if c.ID == *caseID {
				selected = append(selected, c)
			}
		}
		if len(selected) == 0 {
			return fmt.Errorf("no case with id %q in evals/cases", *caseID)
		}
		cases = selected[:1]
	}

	// Judges run on the main model, not the utility model. The judge gates the
	// product's quality claim, and gpt-4o-mini graded inconsistently in
	// practice — inventing requirements the question never asked and refusing
	// to read a change record "X → Y" as attesting the prior value X. A noisy
	// judge is worse than a slightly costlier one: its verdicts are the
	// numbers on the README.
	grader, err := eval.NewGrader(deps.Provider, cfg.LLMModel, logger)
	if err != nil {
		return err
	}
	runner, err := eval.NewRunner(eval.RunnerConfig{
		DB:           deps.Pool,
		Orchestrator: deps.Orchestrator,
		Grader:       grader,
		Model:        cfg.LLMModel,
		Concurrency:  *concurrency,
		UserEmail:    *userEmail,
		Logger:       logger,
	})
	if err != nil {
		return err
	}

	startedAt := time.Now()
	sha := gitSHA()
	logger.Info("eval starting",
		"cases", len(cases), "concurrency", *concurrency, "git_sha", sha,
		"model", cfg.LLMModel, "judge_model", cfg.LLMModel)

	results := runner.Run(ctx, cases)

	path, err := eval.WriteJSONL(config.RepoPath("./evals/results"), sha, startedAt, results)
	if err != nil {
		return err
	}
	summary := eval.Summarize(results)
	// Only a full-suite run earns an eval_runs row: the table exists to show
	// regressions across runs, and a -case row (1 case, most metrics absent)
	// would sit in that trend indistinguishable from a collapsed full pass.
	// A run where every case failed without a single tool call measured the
	// provider's availability, not the agent — the JSONL keeps the evidence,
	// but the trend table must not carry a 0-score row (EVAL-6.A).
	switch {
	case *caseID != "":
		logger.Info("eval: single-case run not persisted to eval_runs", "case", *caseID)
	case isProviderOutage(results):
		logger.Warn("eval: every case failed with zero tool calls — provider outage, not persisting to eval_runs")
	default:
		if err := eval.Persist(ctx, deps.Pool, sha, startedAt, summary); err != nil {
			return err
		}
	}

	fmt.Printf("\neval: %d cases, git %s, results %s\n\n", summary.Cases, sha, path)
	fmt.Println(summary.Render())
	return nil
}

// isProviderOutage reports whether a run's failures are wholly infrastructure:
// every case failed before making a single tool call. Graded answers — even
// bad ones — always leave tool calls behind, so this shape means the LLM
// provider was down, not that the agent regressed.
func isProviderOutage(results []eval.CaseResult) bool {
	if len(results) == 0 {
		return false
	}
	for _, r := range results {
		if r.Status != "failed" || r.ToolCalls != 0 {
			return false
		}
	}
	return true
}

// gitSHA identifies the commit under evaluation. `make eval` always runs
// inside the repository, so git is authoritative; GIT_SHA covers running a
// deployed binary outside one. Never an error — an unknown SHA is not worth
// aborting a paid run over.
func gitSHA() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err == nil {
		if sha := strings.TrimSpace(string(out)); sha != "" {
			return sha
		}
	}
	if sha := strings.TrimSpace(os.Getenv("GIT_SHA")); sha != "" {
		return sha
	}
	return "unknown"
}
