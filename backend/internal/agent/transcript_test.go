package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"cortex/internal/agent"
	"cortex/internal/llm"
	"cortex/internal/store"
	"cortex/internal/tools"
)

// TEST-2.5 — run_events reconstruction.
//
// REQ-2.3 requires run_events payloads to be "complete enough to reconstruct the
// loop transcript", because Day 6's pause/resume has no in-process state to
// resume from: continuing a run means rebuilding the conversation from the log.
// The test for that is an equality, not an inspection — rebuild the transcript
// from the rows alone and compare it to the one the loop actually handed the
// provider, which the fake recorded.

// TestReconstructTranscriptMatchesTheLoop runs a two-hop investigation and
// rebuilds it from run_events.
func TestReconstructTranscriptMatchesTheLoop(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "which Atlas issues are blocked, and why?",
		historyMessage{role: "user", content: "hello"},
		historyMessage{role: "assistant", content: "hi — ask me about the projects."},
	)

	search := &fakeTool{
		name: "jira_search_issues",
		run: func(_ int, _ json.RawMessage) (tools.Result, error) {
			return tools.Result{
				Content:  "ATLAS-1 [Blocked] payments sandbox\nATLAS-2 [Blocked] vendor contract",
				Evidence: []tools.EvidenceItem{{Source: "jira", ExternalID: "ATLAS-1"}},
			}, nil
		},
	}
	comments := &fakeTool{
		name:   "jira_get_comments",
		schema: json.RawMessage(`{"type":"object","properties":{"key":{"type":"string"}},"required":["key"]}`),
		run: func(_ int, args json.RawMessage) (tools.Result, error) {
			return tools.Result{
				Content:  "comment thread for " + string(args),
				Evidence: []tools.EvidenceItem{{Source: "jira", ExternalID: "ATLAS-1"}},
			}, nil
		},
	}

	provider := newFakeProvider(t,
		// Iteration 1: one search.
		providerStep{kind: toolsCall, text: "Let me find the blocked issues.", toolCalls: []llm.ToolCall{
			toolCall("call_search", "jira_search_issues", `{"q":"project = ATLAS AND status = Blocked"}`),
		}},
		// Iteration 2: two comment reads in one turn, so the reconstruction has
		// to keep several tool messages in the right order against the right ids.
		providerStep{kind: toolsCall, toolCalls: []llm.ToolCall{
			toolCall("call_c1", "jira_get_comments", `{"key":"ATLAS-1"}`),
			toolCall("call_c2", "jira_get_comments", `{"key": "ATLAS-2"}`),
		}},
		// Iteration 3: the answer.
		providerStep{kind: toolsCall, text: "ATLAS-1 and ATLAS-2 are blocked on external dependencies."},
	)

	o := newOrchestrator(t, pool, provider, newRegistry(t, search, comments), orchestratorOptions{})
	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if provider.callCount() != 4 {
		t.Fatalf("provider calls = %d, want 4", provider.callCount())
	}

	// The transcript the loop actually used is the request behind its last
	// generation — every earlier turn is a prefix of it.
	final := provider.lastCall(t)

	events := loadEvents(t, pool, seeded.runID)
	system, messages, err := agent.ReconstructTranscript(events)
	if err != nil {
		t.Fatalf("ReconstructTranscript: %v", err)
	}

	if system != final.system {
		t.Errorf("reconstructed system prompt does not match the one the loop used:\n got %q\nwant %q",
			truncateForMessage(system), truncateForMessage(final.system))
	}
	assertSameTranscript(t, messages, final.messages)

	// Sanity: the transcript is the real thing, not two empty slices matching.
	// 2 history turns + the question + (assistant, observation) + (assistant,
	// observation, observation) = 8, plus the draft answer and the completeness
	// check injected before that answer is accepted = 10. Both of those last two
	// are turns the model saw, so a replay that omits them resumes from a
	// conversation that never happened.
	if len(messages) != 10 {
		t.Errorf("reconstructed %d messages, want 10: %s", len(messages), describeTranscript(messages))
	}
}

// A run cut off by the iteration cap must be reconstructable too.
//
// REQ-2.2 step 4 appends a "best effort from evidence so far" user instruction
// to the transcript before the final Generate call, and REQ-2.3 requires
// run_events payloads to be "complete enough to reconstruct the loop
// transcript". That instruction is a turn the model saw, so a replay that omits
// it continues from a conversation that never happened.
func TestReconstructTranscriptCoversForcedAnswerRun(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "why is everything late?")

	tool := &fakeTool{name: "jira_search_issues"}
	provider := newFakeProvider(t,
		providerStep{kind: toolsCall, toolCalls: []llm.ToolCall{
			toolCall("c1", "jira_search_issues", `{"q":"one"}`),
		}},
		providerStep{kind: plainCall, text: "best effort answer"},
	)

	o := newOrchestrator(t, pool, provider, newRegistry(t, tool), orchestratorOptions{maxIterations: 1})
	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	final := provider.lastCall(t)
	events := loadEvents(t, pool, seeded.runID)
	system, messages, err := agent.ReconstructTranscript(events)
	if err != nil {
		t.Fatalf("ReconstructTranscript: %v", err)
	}
	if system != final.system {
		t.Errorf("reconstructed system prompt differs from the one the loop used")
	}
	assertSameTranscript(t, messages, final.messages)
}

// A malformed payload must be reported, not silently produce a short transcript
// that a resume would then continue from.
func TestReconstructTranscriptRejectsCorruptPayload(t *testing.T) {
	events := []store.RunEvent{{
		Seq:     1,
		Type:    agent.EventRunStarted,
		Payload: []byte(`{"system_prompt": 42}`),
	}}
	if _, _, err := agent.ReconstructTranscript(events); err == nil {
		t.Error("ReconstructTranscript accepted a corrupt run_started payload, want an error")
	}
}

// Observability-only events contribute no conversation turns.
func TestReconstructTranscriptIgnoresNonTranscriptEvents(t *testing.T) {
	events := []store.RunEvent{
		{Seq: 1, Type: agent.EventRunStarted, Payload: []byte(
			`{"system_prompt":"S","history":[{"role":"user","content":"q"}]}`)},
		{Seq: 2, Type: agent.EventToolCallStarted, Payload: []byte(`{"tool":"t","arguments":{}}`)},
		{Seq: 3, Type: agent.EventRunFinished, Payload: []byte(`{"iterations":1}`)},
		{Seq: 4, Type: "some_future_event", Payload: []byte(`{}`)},
	}
	system, messages, err := agent.ReconstructTranscript(events)
	if err != nil {
		t.Fatalf("ReconstructTranscript: %v", err)
	}
	if system != "S" {
		t.Errorf("system = %q, want %q", system, "S")
	}
	if len(messages) != 1 || messages[0].Role != llm.RoleUser || messages[0].Content != "q" {
		t.Errorf("messages = %+v, want just the user's question", messages)
	}
}

// assertSameTranscript compares a rebuilt transcript with the one the loop used.
//
// Tool-call arguments are compared canonically rather than byte for byte: the
// model's own spacing is not part of the conversation's meaning, and a resume
// only needs the same arguments, not the same whitespace.
func assertSameTranscript(t *testing.T, got, want []llm.Message) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("reconstructed %d messages, want %d\ngot:  %s\nwant: %s",
			len(got), len(want), describeTranscript(got), describeTranscript(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.Role != w.Role {
			t.Errorf("messages[%d].Role = %q, want %q", i, g.Role, w.Role)
		}
		if g.Content != w.Content {
			t.Errorf("messages[%d].Content = %q, want %q", i, truncateForMessage(g.Content), truncateForMessage(w.Content))
		}
		if g.ToolCallID != w.ToolCallID {
			t.Errorf("messages[%d].ToolCallID = %q, want %q", i, g.ToolCallID, w.ToolCallID)
		}
		if len(g.ToolCalls) != len(w.ToolCalls) {
			t.Errorf("messages[%d] has %d tool calls, want %d", i, len(g.ToolCalls), len(w.ToolCalls))
			continue
		}
		for j := range w.ToolCalls {
			gc, wc := g.ToolCalls[j], w.ToolCalls[j]
			if gc.ID != wc.ID || gc.Name != wc.Name {
				t.Errorf("messages[%d].ToolCalls[%d] = (%s, %s), want (%s, %s)",
					i, j, gc.ID, gc.Name, wc.ID, wc.Name)
			}
			gotArgs, err := tools.CanonicalJSON(gc.Arguments)
			if err != nil {
				t.Errorf("messages[%d].ToolCalls[%d] arguments are not valid JSON: %v", i, j, err)
				continue
			}
			wantArgs, err := tools.CanonicalJSON(wc.Arguments)
			if err != nil {
				t.Fatalf("scripted arguments are not valid JSON: %v", err)
			}
			if gotArgs != wantArgs {
				t.Errorf("messages[%d].ToolCalls[%d].Arguments = %s, want %s", i, j, gotArgs, wantArgs)
			}
		}
	}
}

// describeTranscript renders a transcript compactly for failure output.
func describeTranscript(messages []llm.Message) string {
	parts := make([]string, 0, len(messages))
	for _, m := range messages {
		label := string(m.Role)
		if len(m.ToolCalls) > 0 {
			names := make([]string, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				names = append(names, tc.Name)
			}
			label += "(" + strings.Join(names, ",") + ")"
		}
		if m.ToolCallID != "" {
			label += "[" + m.ToolCallID + "]"
		}
		parts = append(parts, label)
	}
	return "[" + strings.Join(parts, " ") + "]"
}
