package agent

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"cortex/internal/actions"
	"cortex/internal/store"
	"cortex/internal/tools"
)

// delegate_progress events change neither the replayed loop state
// (resume.go), the reconstructed transcript, nor the trace totals. Claude
// Code's steps must never read as the Cortex agent's own tool calls.
func TestDelegateProgressLeavesReplayAndTraceTotalsUnchanged(t *testing.T) {
	actionID := uuid.New()
	progress := func(step tools.Step) store.RunEvent {
		raw, err := json.Marshal(delegateProgressPayload{ActionID: actionID, Action: "claude_code.delegate", Step: step})
		if err != nil {
			t.Fatalf("marshal progress: %v", err)
		}
		return store.RunEvent{Type: EventDelegateProgress, Payload: raw}
	}

	base := []store.RunEvent{
		{Type: EventRunStarted, Payload: []byte(`{"system_prompt":"S","history":[{"role":"user","content":"fix PAY-142"}]}`)},
		{Type: EventLLMCall, Payload: []byte(`{"purpose":"agent_step","iteration":1,"input_tokens":100,"output_tokens":20}`)},
		{Type: EventToolCallStarted, Payload: []byte(`{"iteration":1,"tool_call_id":"c1","tool":"jira_get_issue","arguments":{}}`)},
		{Type: EventToolCallFinished, Payload: []byte(`{"iteration":1,"tool_call_id":"c1","tool":"jira_get_issue","observation":"the issue","status":"ok"}`)},
		{Type: EventRunPaused, Payload: []byte(`{"iteration":1,"waiting":[]}`)},
		{Type: EventActionDecided, Payload: []byte(`{"status":"approved"}`)},
	}
	// The delegation's steps, shaped like tool calls on purpose: a tool name,
	// ids, a result — everything a careless counter might pick up.
	steps := []store.RunEvent{
		progress(tools.Step{ID: "toolu_1", Kind: "tool_use", Tool: "Agent", Detail: "Explore: find it"}),
		progress(tools.Step{ID: "toolu_2", ParentID: "toolu_1", Kind: "tool_use", Tool: "Read", Detail: "retry.go"}),
		progress(tools.Step{ID: "toolu_2:result", ParentID: "toolu_2", Kind: "tool_result", Detail: "package pay"}),
		progress(tools.Step{ID: "toolu_1:result", ParentID: "toolu_1", Kind: "tool_result", Detail: "found it"}),
		progress(tools.Step{Kind: "text", Detail: "Fixed."}),
	}
	tail := []store.RunEvent{
		{Type: EventActionExecuted, Payload: []byte(`{"summary":"Fixed."}`)},
	}

	seqd := func(parts ...[]store.RunEvent) []store.RunEvent {
		var out []store.RunEvent
		for _, p := range parts {
			out = append(out, p...)
		}
		for i := range out {
			out[i].Seq = int32(i + 1)
		}
		return out
	}
	without := seqd(base, tail)
	with := seqd(base, steps, tail)

	sysA, msgsA, errA := ReconstructTranscript(without)
	sysB, msgsB, errB := ReconstructTranscript(with)
	if errA != nil || errB != nil {
		t.Fatalf("ReconstructTranscript errors: without=%v with=%v", errA, errB)
	}
	if sysA != sysB || !reflect.DeepEqual(msgsA, msgsB) {
		t.Errorf("transcript changed by delegate_progress:\nwithout: %+v\nwith:    %+v", msgsA, msgsB)
	}

	pA, pB := replayProgress(without), replayProgress(with)
	if !reflect.DeepEqual(pA, pB) {
		t.Errorf("replayed progress changed by delegate_progress: without %+v, with %+v", pA, pB)
	}
	if pB.toolCalls != 1 || pB.toolSuccesses != 1 {
		t.Errorf("replayed tool calls = %d (%d ok), want 1 (1 ok): only the agent's own call counts",
			pB.toolCalls, pB.toolSuccesses)
	}

	run := store.AgentRun{}
	tA, tB := traceTotals(run, without), traceTotals(run, with)
	if tA != tB {
		t.Errorf("trace totals changed by delegate_progress: without %+v, with %+v", tA, tB)
	}
	if tB.ToolCalls != 1 {
		t.Errorf("trace ToolCalls = %d, want 1", tB.ToolCalls)
	}

	// The steps still reach the trace timeline: the panel renders them.
	trace := AssembleTrace(run, with, nil, nil, nil)
	n := 0
	for _, e := range trace.Timeline {
		if e.Type == EventDelegateProgress {
			n++
		}
	}
	if n != len(steps) {
		t.Errorf("timeline delegate_progress events = %d, want %d", n, len(steps))
	}
}

// "The resumed agent is told this [summary] and
// it is what the chat shows." Claude Code's closing summary (what changed, why,
// tests run, what is unresolved) routinely runs to several hundred characters;
// the resumed agent must be told all of it, not a clipped first line.
func TestTheResumedAgentIsToldTheWholeDelegationSummary(t *testing.T) {
	summary := "Changed retry.go so the backoff resets after a successful attempt; the old code kept " +
		"doubling it forever, which is what PAY-142 describes. Added TestBackoffResets in " +
		"retry_test.go. Ran go test ./internal/pay/... and it passes. Unresolved: the jitter " +
		"constant is still hard-coded; worth a config knob later."
	result, err := json.Marshal(map[string]any{
		"summary": summary,
		"detail":  map[string]any{"branch": "cortex/pay-142-3f9a", "outcome": "changes"},
	})
	if err != nil {
		t.Fatal(err)
	}
	row := store.AgentAction{
		ID:              uuid.New(),
		Source:          "claude_code",
		Action:          "claude_code.delegate",
		Status:          actions.StatusExecuted,
		ProposedPayload: []byte(`{"repo":"cortex","issue_key":"PAY-142","base_branch":"main","prompt":"fix it"}`),
		FinalPayload:    []byte(`{"repo":"cortex","issue_key":"PAY-142","base_branch":"main","prompt":"fix it"}`),
		Result:          result,
	}
	sentence := decisionSentence(row)
	for _, want := range []string{"retry.go", "TestBackoffResets", "Unresolved: the jitter constant"} {
		if !strings.Contains(sentence, want) {
			t.Errorf("the resume turn omits %q from Claude Code's summary:\n%s", want, sentence)
		}
	}
}
