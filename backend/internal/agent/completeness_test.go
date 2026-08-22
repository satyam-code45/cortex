package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"cortex/internal/agent"
	"cortex/internal/llm"
	"cortex/internal/tools"
)

// resultTool is a tool that always succeeds with one line of content and one
// piece of evidence.
func resultTool(name, content string) *fakeTool {
	return &fakeTool{
		name: name,
		// The shared default schema requires a "q" field, and the validator
		// rejects undeclared properties, so the arguments these tests script
		// would be turned into error observations and the tool would never run.
		// Declaring both argument names keeps the focus on whether the hop
		// happens rather than on argument validation.
		schema: json.RawMessage(
			`{"type":"object","properties":{"jql":{"type":"string"},"query":{"type":"string"}}}`),
		run: func(_ int, _ json.RawMessage) (tools.Result, error) {
			return tools.Result{
				Content: content,
				Evidence: []tools.EvidenceItem{{
					Source:     "test",
					ExternalID: name,
					Title:      content,
				}},
			}, nil
		},
	}
}

// The completeness check: before the first answer of a run is accepted, the
// model is asked to re-read it against the question.
//
// This exists because prose did not work. Two rounds of increasingly explicit
// system-prompt instruction still produced live runs that read a document
// saying the notice arrived by email, said so in the answer, and never opened
// the mailbox — the third hop landed in one run out of three, twice measured.
// So the behaviour is enforced by the loop, and these tests hold it there.

// A draft answer is not the run's answer: the model gets one chance to notice a
// gap, and what it says the second time is what the user gets.
func TestCompletenessCheckCanReplaceTheAnswer(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "why did the launch slip?")

	search := resultTool("jira_search_issues", "ATLAS-1 blocked on the provider")
	mail := resultTool("gmail_search", "2026-06-08 Nordwind: v3 refunds sandbox delayed")

	provider := newFakeProvider(t,
		providerStep{kind: toolsCall, toolCalls: []llm.ToolCall{
			toolCall("call_1", "jira_search_issues", `{"jql":"project = ATLAS"}`),
		}},
		// The draft stops at the lead instead of following it.
		providerStep{kind: toolsCall, text: "ATLAS-1 is blocked on the provider; the notice came by email."},
		// Handed the check, it goes and reads the email.
		providerStep{
			kind: toolsCall,
			assert: func(t *testing.T, call recordedCall) {
				t.Helper()
				last := call.messages[len(call.messages)-1]
				if last.Role != llm.RoleUser || !strings.Contains(last.Content, completenessCheckMarker) {
					t.Errorf("call #3 did not receive the completeness check; last turn = %s %q",
						last.Role, truncateForMessage(last.Content))
				}
				prev := call.messages[len(call.messages)-2]
				if prev.Role != llm.RoleAssistant || !strings.Contains(prev.Content, "came by email") {
					t.Errorf("the check must review the draft answer; previous turn = %s %q",
						prev.Role, truncateForMessage(prev.Content))
				}
			},
			toolCalls: []llm.ToolCall{
				toolCall("call_2", "gmail_search", `{"query":"Nordwind"}`),
			},
		},
		providerStep{kind: toolsCall, text: "The launch slipped because Nordwind delayed the sandbox, notified 2026-06-08."},
	)

	o := newOrchestrator(t, pool, provider, newRegistry(t, search, mail), orchestratorOptions{})
	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	run := loadRun(t, pool, seeded.runID)
	if run.status != "completed" {
		t.Fatalf("status = %q, want completed", run.status)
	}
	if run.answer == nil || !strings.Contains(*run.answer, "2026-06-08") {
		t.Errorf("answer = %v, want the post-check answer carrying the date it went and fetched",
			run.answer)
	}
	if mail.executions() != 1 {
		t.Errorf("gmail executions = %d, want 1 — the check exists to make this hop happen", mail.executions())
	}
}

// The check must fire once. It is a guard against an answer that skipped a
// lead, not a negotiation: re-checking every answer would let a stubborn model
// burn the whole iteration budget re-drafting.
func TestCompletenessCheckRunsOnlyOnce(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "what is blocked?")

	search := resultTool("jira_search_issues", "ATLAS-1 blocked")

	provider := newFakeProvider(t,
		providerStep{kind: toolsCall, toolCalls: []llm.ToolCall{
			toolCall("call_1", "jira_search_issues", `{"jql":"project = ATLAS"}`),
		}},
		providerStep{kind: toolsCall, text: "first draft"},
		// After the check it answers again; that answer is final even though it
		// is still thin.
		providerStep{kind: toolsCall, text: "second draft"},
	)

	o := newOrchestrator(t, pool, provider, newRegistry(t, search), orchestratorOptions{})
	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := provider.callCount(); got != 3 {
		t.Errorf("provider calls = %d, want 3 — a second check would mean the guard is unbounded", got)
	}
	if run := loadRun(t, pool, seeded.runID); run.answer == nil || *run.answer != "second draft" {
		t.Errorf("answer = %v, want %q", run.answer, "second draft")
	}

	purposes := purposesOf(loadLLMCalls(t, pool, seeded.runID))
	var checks int
	for _, p := range purposes {
		if p == agent.PurposeCompletenessCheck {
			checks++
		}
	}
	if checks != 1 {
		t.Errorf("completeness_check llm_calls = %d, want exactly 1 (purposes: %v)", checks, purposes)
	}
}

// Both injected turns have to reach run_events. The draft is what the check is
// reviewing, so a replay missing it resumes from a conversation that never
// happened — the invariant TEST-2.5 protects.
func TestCompletenessCheckTurnsAreReconstructable(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "why did it slip?")

	search := resultTool("jira_search_issues", "ATLAS-1 blocked")

	provider := newFakeProvider(t,
		providerStep{kind: toolsCall, toolCalls: []llm.ToolCall{
			toolCall("call_1", "jira_search_issues", `{"jql":"project = ATLAS"}`),
		}},
		providerStep{kind: toolsCall, text: "draft answer"},
		providerStep{kind: toolsCall, text: "final answer"},
	)

	o := newOrchestrator(t, pool, provider, newRegistry(t, search), orchestratorOptions{})
	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	_, messages, err := agent.ReconstructTranscript(loadEvents(t, pool, seeded.runID))
	if err != nil {
		t.Fatalf("ReconstructTranscript: %v", err)
	}
	assertSameTranscript(t, messages, provider.lastCall(t).messages)

	var sawDraft, sawCheck bool
	for _, m := range messages {
		if m.Role == llm.RoleAssistant && m.Content == "draft answer" {
			sawDraft = true
		}
		if m.Role == llm.RoleUser && strings.Contains(m.Content, completenessCheckMarker) {
			sawCheck = true
		}
	}
	if !sawDraft {
		t.Error("the draft answer is missing from the reconstructed transcript")
	}
	if !sawCheck {
		t.Error("the completeness-check instruction is missing from the reconstructed transcript")
	}
}
