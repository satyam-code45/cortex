package agent_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/agent"
	"cortex/internal/llm"
)

// TEST-7.2 — the run executes on its OWNER's key (REQ-7.2).
//
// ProviderForUser is how the worker builds each run's completion provider from
// the owner's stored key. These tests assert the ownership cut from outside:
// the resolver is asked for exactly the run's owner, the provider it returns
// makes every completion call, and the server-wide provider — the server's own
// key — is never touched by a user's run, not even as a fallback when the
// user's key is unusable.

// newBYOKOrchestrator builds an orchestrator with a per-user provider resolver.
func newBYOKOrchestrator(
	t *testing.T,
	pool *pgxpool.Pool,
	serverProvider llm.Provider,
	forUser func(ctx context.Context, userID uuid.UUID) (llm.Provider, error),
) *agent.Orchestrator {
	t.Helper()
	o, err := agent.New(agent.Config{
		DB:              pool,
		Provider:        serverProvider,
		ProviderForUser: forUser,
		Registry:        newRegistry(t, &fakeTool{name: "jira_search_issues"}),
		Model:           testModel,
		UtilityModel:    testUtilityModel,
		Logger:          discardLogger(),
		Now:             func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("build orchestrator: %v", err)
	}
	return o
}

func TestRunUsesTheOwnersProviderNotTheServers(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "which Atlas issues are blocked?")

	// The server provider is scripted with nothing: any call it receives is
	// flagged as an unscripted-call failure — the exact regression this test
	// exists to catch (a user run billing the server's key).
	serverProvider := newFakeProvider(t)
	ownerProvider := newFakeProvider(t,
		providerStep{kind: toolsCall, text: "answered on the owner's key"},
	)

	var askedFor []uuid.UUID
	o := newBYOKOrchestrator(t, pool, serverProvider,
		func(_ context.Context, userID uuid.UUID) (llm.Provider, error) {
			askedFor = append(askedFor, userID)
			return ownerProvider, nil
		})

	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(askedFor) != 1 || askedFor[0] != seeded.userID {
		t.Errorf("provider was resolved for %v, want exactly the run owner %s", askedFor, seeded.userID)
	}
	if serverProvider.callCount() != 0 {
		t.Errorf("the server's provider took %d calls during a user's run, want 0", serverProvider.callCount())
	}
	if ownerProvider.callCount() == 0 {
		t.Error("the owner's provider was never called")
	}

	run := loadRun(t, pool, seeded.runID)
	if run.status != "completed" {
		t.Fatalf("run status = %q, want completed (error %v)", run.status, run.errText)
	}
	if run.answer == nil || *run.answer != "answered on the owner's key" {
		t.Errorf("answer = %v, want the owner-provider answer", run.answer)
	}
}

// A run whose key is gone for good (deleted between enqueue and execution, or
// the encryption secret rotated — the resolver marks these
// agent.ErrLLMKeyUnavailable) fails with the settings pointer — it must never
// fall back to the server's provider.
func TestRunWithoutAUsableKeyFailsAndNeverBorrowsTheServers(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "which Atlas issues are blocked?")

	serverProvider := newFakeProvider(t)
	o := newBYOKOrchestrator(t, pool, serverProvider,
		func(_ context.Context, _ uuid.UUID) (llm.Provider, error) {
			return nil, fmt.Errorf("%w: no llm key on file", agent.ErrLLMKeyUnavailable)
		})

	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v (an unusable key is a failed run, not a job error)", err)
	}

	if serverProvider.callCount() != 0 {
		t.Errorf("the server's provider took %d calls, want 0 — a missing user key must not bill the server", serverProvider.callCount())
	}

	run := loadRun(t, pool, seeded.runID)
	if run.status != "failed" {
		t.Fatalf("run status = %q, want failed", run.status)
	}
	if run.errText == nil || *run.errText != "llm key required — add your API key in Settings" {
		t.Errorf("run error = %v, want the settings pointer the UI shows", run.errText)
	}
	if !run.finished {
		t.Error("run has no finished_at; a failed run must be terminal")
	}
}

// A transient resolver failure (a database blip while loading the key) is a
// job error, not a run failure: River retries it, and the user must NOT see
// "add your API key" over a connection hiccup while holding a valid key.
func TestRunWithATransientKeyLoadErrorIsRetriedNotFailed(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "which Atlas issues are blocked?")

	serverProvider := newFakeProvider(t)
	ownerProvider := newFakeProvider(t,
		providerStep{kind: toolsCall, text: "answered after the blip"},
	)
	calls := 0
	o := newBYOKOrchestrator(t, pool, serverProvider,
		func(_ context.Context, _ uuid.UUID) (llm.Provider, error) {
			calls++
			if calls == 1 {
				return nil, errors.New("keys: load key: connection reset")
			}
			return ownerProvider, nil
		})

	// First attempt: the transient error surfaces as a job error for River.
	if err := o.Run(context.Background(), seeded.runID); err == nil {
		t.Fatal("Run returned nil for a transient key-load failure, want an error so River retries")
	}
	run := loadRun(t, pool, seeded.runID)
	if run.status == "failed" {
		t.Fatalf("run was permanently failed on a transient error (error %v)", run.errText)
	}

	// The retry (StartAgentRun reclaims a 'running' row) succeeds on the
	// owner's key.
	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run retry: %v", err)
	}
	run = loadRun(t, pool, seeded.runID)
	if run.status != "completed" {
		t.Fatalf("run status after retry = %q, want completed (error %v)", run.status, run.errText)
	}
	if serverProvider.callCount() != 0 {
		t.Errorf("the server's provider took %d calls across both attempts, want 0", serverProvider.callCount())
	}
}

// With no resolver configured (cmd/eval, the server-initiated path), every run
// uses the orchestrator-wide provider — the server key funds server work only.
func TestRunWithoutResolverUsesTheServerProvider(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "eval question")

	serverProvider := newFakeProvider(t,
		providerStep{kind: toolsCall, text: "answered on the server's key"},
	)
	o := newBYOKOrchestrator(t, pool, serverProvider, nil)

	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run := loadRun(t, pool, seeded.runID)
	if run.status != "completed" {
		t.Fatalf("run status = %q, want completed (error %v)", run.status, run.errText)
	}
	if serverProvider.callCount() == 0 {
		t.Error("the server provider was never called on the eval path")
	}
}
