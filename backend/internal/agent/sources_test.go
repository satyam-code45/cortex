package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/agent"
	"cortex/internal/llm"
	"cortex/internal/tools"
)

// The worker builds each run's tool registry from the run OWNER's
// connections, asserted from outside the orchestrator: the resolver
// is asked for exactly the run's owner, the run executes against the registry
// it returned (never the demo one), run_started records an honest sources map,
// and a run with no usable sources fails with the reconnect pointer instead of
// borrowing demo data.

// newSourcesOrchestrator builds an orchestrator with a per-user registry
// resolver. The demo registry carries demoTool, so any demo-tool execution
// during a user-mode run is observable.
func newSourcesOrchestrator(
	t *testing.T,
	pool *pgxpool.Pool,
	provider llm.Provider,
	demoRegistry *tools.Registry,
	forUser func(ctx context.Context, userID uuid.UUID) (*tools.Registry, agent.Sources, error),
) *agent.Orchestrator {
	t.Helper()
	o, err := agent.New(agent.Config{
		DB:              pool,
		Provider:        provider,
		Registry:        demoRegistry,
		RegistryForUser: forUser,
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

// runStartedOf returns the run_started payload of a run's transcript.
func runStartedOf(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) map[string]any {
	t.Helper()
	events := loadEvents(t, pool, runID)
	started := eventsOfType(events, agent.EventRunStarted)
	if len(started) == 0 {
		t.Fatalf("no run_started event (events: %v)", eventTypes(events))
	}
	return decodePayload(t, started[0])
}

// sourcesOf decodes the sources map from a run_started payload.
func sourcesOf(t *testing.T, payload map[string]any) (mode string, connected []any) {
	t.Helper()
	raw, ok := payload["sources"].(map[string]any)
	if !ok {
		t.Fatalf("run_started payload carries no sources map: %v", payload["sources"])
	}
	mode, _ = raw["mode"].(string)
	connected, ok = raw["connected"].([]any)
	if !ok {
		t.Fatalf("run_started sources.connected = %v (%T), want a JSON array (never null)",
			raw["connected"], raw["connected"])
	}
	return mode, connected
}

func TestRunUsesTheOwnersRegistryAndRecordsUserSources(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "what projects do I have?")

	demoTool := &fakeTool{name: "search_knowledge_base"}
	userTool := &fakeTool{name: "jira_list_projects"}

	provider := newFakeProvider(t,
		providerStep{kind: toolsCall, toolCalls: []llm.ToolCall{
			{ID: "call-1", Name: "jira_list_projects", Arguments: json.RawMessage(`{"q":"projects"}`)},
		}},
		providerStep{kind: toolsCall, text: "you have one project: SATY"},
	)

	var askedFor []uuid.UUID
	o := newSourcesOrchestrator(t, pool, provider, newRegistry(t, demoTool),
		func(_ context.Context, userID uuid.UUID) (*tools.Registry, agent.Sources, error) {
			askedFor = append(askedFor, userID)
			return newRegistry(t, userTool), agent.Sources{Mode: agent.ModeUser, Connected: []string{"jira"}}, nil
		})

	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(askedFor) != 1 || askedFor[0] != seeded.userID {
		t.Errorf("registry was resolved for %v, want exactly the run owner %s", askedFor, seeded.userID)
	}
	if userTool.executions() != 1 {
		t.Errorf("the owner's tool ran %d time(s), want 1", userTool.executions())
	}
	if demoTool.executions() != 0 {
		t.Errorf("a demo tool ran %d time(s) during a user-mode run, want 0", demoTool.executions())
	}

	run := loadRun(t, pool, seeded.runID)
	if run.status != "completed" {
		t.Fatalf("run status = %q, want completed (error %v)", run.status, run.errText)
	}

	payload := runStartedOf(t, pool, seeded.runID)
	mode, connected := sourcesOf(t, payload)
	if mode != "user" {
		t.Errorf("run_started sources.mode = %q, want user", mode)
	}
	if len(connected) != 1 || connected[0] != "jira" {
		t.Errorf("run_started sources.connected = %v, want [jira]", connected)
	}

	// The recorded tool inventory is the user registry's, and the recorded
	// system prompt promises exactly the connected source.
	toolsRecorded, _ := payload["tools"].([]any)
	if len(toolsRecorded) != 1 || toolsRecorded[0] != "jira_list_projects" {
		t.Errorf("run_started tools = %v, want the owner's registry [jira_list_projects]", toolsRecorded)
	}
	systemPrompt, _ := payload["system_prompt"].(string)
	if !strings.Contains(systemPrompt, "For this run you can see JIRA") {
		t.Error("run_started system prompt does not tell the agent which sources exist for this run")
	}
	if strings.Contains(systemPrompt, "You can see three systems") {
		t.Error("a user-mode run was promised the full three-system demo prompt")
	}
}

func TestRunWithoutAResolverRecordsDemoSources(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "which Atlas issues are blocked?")

	provider := newFakeProvider(t,
		providerStep{kind: toolsCall, text: "nothing is blocked"},
	)
	o := newSourcesOrchestrator(t, pool, provider,
		newRegistry(t, &fakeTool{name: "jira_search_issues"}), nil)

	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run := loadRun(t, pool, seeded.runID)
	if run.status != "completed" {
		t.Fatalf("run status = %q, want completed (error %v)", run.status, run.errText)
	}

	mode, connected := sourcesOf(t, runStartedOf(t, pool, seeded.runID))
	if mode != "demo" {
		t.Errorf("run_started sources.mode = %q, want demo", mode)
	}
	if len(connected) != 0 {
		t.Errorf("run_started sources.connected = %v, want empty in demo mode", connected)
	}
}

func TestRunWhenTheResolverReturnsDemoModeUsesItsRegistry(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "which Atlas issues are blocked?")

	demoTool := &fakeTool{name: "search_knowledge_base"}
	demoRegistry := newRegistry(t, demoTool)
	provider := newFakeProvider(t,
		providerStep{kind: toolsCall, toolCalls: []llm.ToolCall{
			{ID: "call-1", Name: "search_knowledge_base", Arguments: json.RawMessage(`{"q":"blocked"}`)},
		}},
		providerStep{kind: toolsCall, text: "answered from the demo workspace"},
	)
	o := newSourcesOrchestrator(t, pool, provider, demoRegistry,
		func(_ context.Context, _ uuid.UUID) (*tools.Registry, agent.Sources, error) {
			// Zero connections (or the demo toggle): the factory hands back the
			// full demo registry.
			return demoRegistry, agent.Sources{Mode: agent.ModeDemo}, nil
		})

	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run := loadRun(t, pool, seeded.runID); run.status != "completed" {
		t.Fatalf("run status = %q, want completed (error %v)", run.status, run.errText)
	}
	if demoTool.executions() != 1 {
		t.Errorf("demo tool ran %d time(s), want 1 — demo mode gets the full demo registry", demoTool.executions())
	}
	mode, _ := sourcesOf(t, runStartedOf(t, pool, seeded.runID))
	if mode != "demo" {
		t.Errorf("run_started sources.mode = %q, want demo", mode)
	}
}

// A run whose owner has connections but every one of them is errored fails
// with the reconnect pointer. It must not fall back to the demo registry and
// must not spend a single LLM call.
func TestRunWithNoUsableSourcesFailsAndNeverBorrowsDemoData(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "what's in my mailbox?")

	demoTool := &fakeTool{name: "search_knowledge_base"}
	provider := newFakeProvider(t) // any call is an unscripted-call failure
	o := newSourcesOrchestrator(t, pool, provider, newRegistry(t, demoTool),
		func(_ context.Context, _ uuid.UUID) (*tools.Registry, agent.Sources, error) {
			empty := newRegistry(t)
			return empty, agent.Sources{Mode: agent.ModeUser, Connected: []string{}},
				fmt.Errorf("%w: every connection is errored", agent.ErrNoUsableSources)
		})

	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v (no usable sources is a failed run, not a job error)", err)
	}

	run := loadRun(t, pool, seeded.runID)
	if run.status != "failed" {
		t.Fatalf("run status = %q, want failed", run.status)
	}
	if run.errText == nil || *run.errText != "your connected sources need attention — reconnect them on the Connections page" {
		t.Errorf("run error = %v, want the reconnect pointer the UI shows", run.errText)
	}
	if !run.finished {
		t.Error("run has no finished_at; a failed run must be terminal")
	}
	if provider.callCount() != 0 {
		t.Errorf("the provider took %d calls, want 0 — nothing must run without usable sources", provider.callCount())
	}
	if demoTool.executions() != 0 {
		t.Errorf("a demo tool ran %d time(s), want 0 — demo data must never stand in", demoTool.executions())
	}

	// The failed run still has an honest trace: run_started records the
	// attempted user-mode sources.
	mode, connected := sourcesOf(t, runStartedOf(t, pool, seeded.runID))
	if mode != "user" {
		t.Errorf("run_started sources.mode = %q, want user", mode)
	}
	if len(connected) != 0 {
		t.Errorf("run_started sources.connected = %v, want empty (every source errored)", connected)
	}
}

// A transient resolver failure (database blip while loading connections) is a
// job error for River, not a permanently failed run.
func TestRunWithATransientRegistryErrorIsRetriedNotFailed(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "which Atlas issues are blocked?")

	provider := newFakeProvider(t,
		providerStep{kind: toolsCall, text: "answered after the blip"},
	)
	calls := 0
	o := newSourcesOrchestrator(t, pool, provider,
		newRegistry(t, &fakeTool{name: "jira_search_issues"}),
		func(_ context.Context, _ uuid.UUID) (*tools.Registry, agent.Sources, error) {
			calls++
			if calls == 1 {
				return nil, agent.Sources{}, errors.New("connections: list connections: connection reset")
			}
			return newRegistry(t, &fakeTool{name: "jira_list_projects"}),
				agent.Sources{Mode: agent.ModeUser, Connected: []string{"jira"}}, nil
		})

	if err := o.Run(context.Background(), seeded.runID); err == nil {
		t.Fatal("Run returned nil for a transient registry failure, want an error so River retries")
	}
	if run := loadRun(t, pool, seeded.runID); run.status == "failed" {
		t.Fatalf("run was permanently failed on a transient error (error %v)", run.errText)
	}

	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run retry: %v", err)
	}
	if run := loadRun(t, pool, seeded.runID); run.status != "completed" {
		t.Fatalf("run status after retry = %q, want completed (error %v)", run.status, run.errText)
	}
}
