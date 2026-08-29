package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"cortex/internal/agent"
	"cortex/internal/llm"
	"cortex/internal/tools"
)

// TEST-6.2 (orchestrator level, spec A4) and TEST-6.3 (spec A5).
//
// The one-retry-for-transient / no-retry-for-permanent policy is pinned by
// TestRunToolExecutionErrorBecomesObservation in orchestrator_test.go; this
// file adds A4's named opt-out — an error advertising Permanent() — and A5's
// total-outage guard.

// permanentAwareError mimics the integrations' API error types: Permanent()
// is the opt-out executeWithRetry consults, so a 400 on a malformed JQL is not
// retried while a 503 is.
type permanentAwareError struct {
	msg       string
	permanent bool
}

func (e *permanentAwareError) Error() string   { return e.msg }
func (e *permanentAwareError) Permanent() bool { return e.permanent }

// A tool error that reports Permanent() true gets no second attempt; one that
// reports false gets exactly one (TEST-6.2, A4).
func TestRunRetryPolicyHonorsPermanentOptOut(t *testing.T) {
	tests := []struct {
		name           string
		err            error
		wantExecutions int
	}{
		{
			name:           "Permanent() true is not retried",
			err:            &permanentAwareError{msg: "jira: HTTP 400: bad jql", permanent: true},
			wantExecutions: 1,
		},
		{
			name:           "Permanent() false is retried once",
			err:            &permanentAwareError{msg: "jira: HTTP 429: throttled", permanent: false},
			wantExecutions: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := testPool(t)
			seeded := seedRun(t, pool, "which Atlas issues are blocked?")

			tool := &fakeTool{
				name: "jira_search_issues",
				run: func(_ int, _ json.RawMessage) (tools.Result, error) {
					return tools.Result{}, tt.err
				},
			}

			provider := newFakeProvider(t,
				providerStep{kind: toolsCall, toolCalls: []llm.ToolCall{
					toolCall("c1", "jira_search_issues", `{"q":"blocked"}`),
				}},
				providerStep{kind: toolsCall, text: "could not search"},
			)

			o := newOrchestrator(t, pool, provider, newRegistry(t, tool), orchestratorOptions{})
			if err := o.Run(context.Background(), seeded.runID); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if tool.executions() != tt.wantExecutions {
				t.Errorf("tool executions = %d, want %d", tool.executions(), tt.wantExecutions)
			}
			if run := loadRun(t, pool, seeded.runID); run.status != "completed" {
				t.Errorf("status = %q, want completed (one failing tool call must not fail the run)", run.status)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TEST-6.3 — total-outage guard (spec A5b)
// ---------------------------------------------------------------------------

// When every tool call in the run has failed and the streak reaches the
// threshold, the run fails with a clear "external sources unreachable" error
// instead of spending the remaining paid iterations against sources that are
// down.
func TestRunTotalToolOutageFailsRun(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "which Atlas issues are blocked?")

	tool := &fakeTool{
		name: "jira_search_issues",
		run: func(_ int, _ json.RawMessage) (tools.Result, error) {
			// Transient, so each call also consumes its one retry — a genuine
			// outage has been probed hard before the guard trips.
			return tools.Result{}, errors.New("jira: HTTP 503: service unavailable")
		},
	}

	// One failing call per iteration, scripted out to the full iteration
	// budget: if the guard did not exist, the loop would burn all of these
	// and then force a best-effort answer.
	steps := make([]providerStep, 0, agent.DefaultMaxIterations)
	for i := 1; i <= agent.DefaultMaxIterations; i++ {
		steps = append(steps, providerStep{kind: toolsCall, toolCalls: []llm.ToolCall{
			toolCall(fmt.Sprintf("c%d", i), "jira_search_issues", fmt.Sprintf(`{"q":"probe %d"}`, i)),
		}})
	}
	provider := newFakeProvider(t, steps...)

	o := newOrchestrator(t, pool, provider, newRegistry(t, tool), orchestratorOptions{})
	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run returned an error; an outage must be recorded, not returned: %v", err)
	}

	run := loadRun(t, pool, seeded.runID)
	if run.status != "failed" {
		t.Fatalf("status = %q, want failed", run.status)
	}
	if run.errText == nil || !strings.Contains(*run.errText, "unreachable") {
		t.Errorf("error = %v, want a clear external-sources-unreachable message", run.errText)
	}
	if run.answer != nil {
		t.Errorf("answer = %q on a total outage, want null", *run.answer)
	}
	if !run.finished {
		t.Error("finished_at not set on the failed run")
	}

	// The remaining iterations were NOT spent: the guard cut the run short
	// well before the iteration cap, and no forced best-effort answer was
	// generated against sources that are down.
	if provider.callCount() >= agent.DefaultMaxIterations {
		t.Errorf("provider calls = %d; the guard did not save the remaining iterations (cap %d)",
			provider.callCount(), agent.DefaultMaxIterations)
	}
	for _, call := range loadLLMCalls(t, pool, seeded.runID) {
		if call.purpose != agent.PurposeAgentLoop {
			t.Errorf("llm_calls contains purpose %q; an outage run must not reach %q or the citation pass",
				call.purpose, agent.PurposeFinalAnswer)
		}
	}

	// Every attempted call is still on the trace — the guard sits after the
	// failed call's events are durably recorded — and the run closes with
	// run_failed.
	events := loadEvents(t, pool, seeded.runID)
	finished := eventsOfType(events, agent.EventToolCallFinished)
	if len(finished) == 0 {
		t.Fatal("no tool_call_finished events; the failed attempts must stay on the trace")
	}
	for _, e := range finished {
		if payload := decodePayload(t, e); payload["status"] != "error" {
			t.Errorf("tool_call_finished.status = %v, want error", payload["status"])
		}
	}
	// Each failed call was transient, so it consumed exactly one retry.
	if tool.executions() != 2*len(finished) {
		t.Errorf("tool executions = %d, want %d (each of %d failed calls retried once)",
			tool.executions(), 2*len(finished), len(finished))
	}
	if len(eventsOfType(events, agent.EventRunFailed)) != 1 {
		t.Errorf("event types = %v, want exactly one %s", eventTypes(events), agent.EventRunFailed)
	}
	if len(eventsOfType(events, agent.EventAnswer)) != 0 {
		t.Error("an answer event was recorded on a total outage")
	}
}

// One successful tool call anywhere in the run proves the environment is
// reachable, so later failures are ordinary observations — the guard must not
// fire however long the failure streak gets afterwards.
func TestRunToolSuccessDisablesOutageGuard(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "which Atlas issues are blocked?")

	tool := &fakeTool{
		name: "jira_search_issues",
		run: func(_ int, args json.RawMessage) (tools.Result, error) {
			if strings.Contains(string(args), "works") {
				return tools.Result{
					Content:  "ATLAS-1 [Blocked] payments sandbox",
					Evidence: []tools.EvidenceItem{{Source: "jira", ExternalID: "ATLAS-1"}},
				}, nil
			}
			return tools.Result{}, errors.New("jira: HTTP 503: service unavailable")
		},
	}

	provider := newFakeProvider(t,
		providerStep{kind: toolsCall, toolCalls: []llm.ToolCall{
			toolCall("ok1", "jira_search_issues", `{"q":"works"}`),
		}},
		// Five consecutive failures — past any sensible threshold — after the
		// one success.
		providerStep{kind: toolsCall, toolCalls: []llm.ToolCall{
			toolCall("f1", "jira_search_issues", `{"q":"fail 1"}`),
			toolCall("f2", "jira_search_issues", `{"q":"fail 2"}`),
			toolCall("f3", "jira_search_issues", `{"q":"fail 3"}`),
		}},
		providerStep{kind: toolsCall, toolCalls: []llm.ToolCall{
			toolCall("f4", "jira_search_issues", `{"q":"fail 4"}`),
			toolCall("f5", "jira_search_issues", `{"q":"fail 5"}`),
		}},
		providerStep{kind: toolsCall, text: "ATLAS-1 is blocked; the other searches failed upstream."},
	)

	o := newOrchestrator(t, pool, provider, newRegistry(t, tool), orchestratorOptions{})
	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	run := loadRun(t, pool, seeded.runID)
	if run.status != "completed" {
		t.Fatalf("status = %q, want completed (one success disables the outage guard); error = %v",
			run.status, run.errText)
	}
	if run.answer == nil || !strings.Contains(*run.answer, "ATLAS-1") {
		t.Errorf("answer = %v, want the model's answer", run.answer)
	}
}

// Validation failures — a hallucinated tool, schema-invalid arguments — are the
// model's mistake, not the environment's: a streak of them must not report
// "external sources unreachable" for sources that were never contacted.
func TestRunValidationFailuresDoNotTripOutageGuard(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "which Atlas issues are blocked?")

	tool := &fakeTool{name: "jira_search_issues"}

	provider := newFakeProvider(t,
		providerStep{kind: toolsCall, toolCalls: []llm.ToolCall{
			toolCall("h1", "made_up_tool_1", `{"q":"a"}`),
			toolCall("h2", "made_up_tool_2", `{"q":"b"}`),
			toolCall("h3", "made_up_tool_3", `{"q":"c"}`),
			toolCall("h4", "made_up_tool_4", `{"q":"d"}`),
			toolCall("h5", "made_up_tool_5", `{"q":"e"}`),
		}},
		providerStep{kind: toolsCall, text: "I kept calling tools that do not exist; correcting course."},
	)

	o := newOrchestrator(t, pool, provider, newRegistry(t, tool), orchestratorOptions{})
	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	run := loadRun(t, pool, seeded.runID)
	if run.status != "completed" {
		t.Errorf("status = %q, want completed (validation failures are not an outage); error = %v",
			run.status, run.errText)
	}
	if tool.executions() != 0 {
		t.Errorf("tool executions = %d, want 0", tool.executions())
	}
}
