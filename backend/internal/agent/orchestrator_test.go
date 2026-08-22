package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/agent"
	"cortex/internal/llm"
	"cortex/internal/tools"
)

// TEST-2.1 — the agent loop, driven by a scripted fake Provider.
//
// The five behaviours the spec names are the five ways a real run goes sideways:
// the model answers (happy path), it never stops (iteration cap), it repeats
// itself (dedupe), it calls a tool wrongly (invalid args), and a tool fails
// (execution error). REQ-2.2's governing rule is that only the first of those
// ends the run normally and none of the others crashes it — each becomes an
// observation the model can recover from.

const (
	testModel        = "gpt-test-reasoner"
	testUtilityModel = "gpt-test-utility"
)

// orchestratorOptions are the knobs a test varies.
type orchestratorOptions struct {
	maxIterations       int
	maxToolContentChars int
	utilityModel        string
}

func newOrchestrator(
	t *testing.T,
	pool *pgxpool.Pool,
	provider llm.Provider,
	registry *tools.Registry,
	opts orchestratorOptions,
) *agent.Orchestrator {
	t.Helper()
	utility := opts.utilityModel
	if utility == "" {
		utility = testUtilityModel
	}
	o, err := agent.New(agent.Config{
		DB:                  pool,
		Provider:            provider,
		Registry:            registry,
		Model:               testModel,
		UtilityModel:        utility,
		MaxIterations:       opts.maxIterations,
		MaxToolContentChars: opts.maxToolContentChars,
		Logger:              discardLogger(),
		Now:                 func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("build orchestrator: %v", err)
	}
	return o
}

// toolCall builds a model tool-call request.
func toolCall(id, name, args string) llm.ToolCall {
	return llm.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}
}

// ---------------------------------------------------------------------------
// happy path
// ---------------------------------------------------------------------------

// The canonical run: the model calls a tool, reads the observation, and answers.
//
// The event sequence asserted here is the Day 5 SSE contract (REQ-2.3), so the
// names and the order are part of the interface, not an implementation detail.
func TestRunHappyPath(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "which Atlas issues are blocked?")

	tool := &fakeTool{
		name: "jira_search_issues",
		run: func(_ int, _ json.RawMessage) (tools.Result, error) {
			return tools.Result{
				Content: "ATLAS-1 [Blocked] payments sandbox down",
				Evidence: []tools.EvidenceItem{{
					Source:     "jira",
					ExternalID: "ATLAS-1",
					Title:      "payments sandbox down",
					URL:        "https://example.atlassian.net/browse/ATLAS-1",
				}},
			}, nil
		},
	}

	provider := newFakeProvider(t,
		providerStep{
			kind:      toolsCall,
			toolCalls: []llm.ToolCall{toolCall("call_1", "jira_search_issues", `{"q":"blocked"}`)},
			assert: func(t *testing.T, call recordedCall) {
				t.Helper()
				// The loop must send the investigator persona and the tool
				// inventory (REQ-2.2 step 1).
				if call.system == "" {
					t.Error("first call had an empty system prompt")
				}
				if len(call.tools) != 1 || call.tools[0].Name != "jira_search_issues" {
					t.Errorf("tool definitions = %+v, want the registry's one tool", call.tools)
				}
				if len(call.messages) != 1 || call.messages[0].Role != llm.RoleUser {
					t.Fatalf("first call messages = %+v, want just the user question", call.messages)
				}
				if call.messages[0].Content != "which Atlas issues are blocked?" {
					t.Errorf("first user message = %q, want the seeded question", call.messages[0].Content)
				}
				if call.model != testModel {
					t.Errorf("model = %q, want the reasoning model %q", call.model, testModel)
				}
			},
			inputTokens:  100,
			outputTokens: 20,
		},
		providerStep{
			kind: toolsCall,
			text: "ATLAS-1 is blocked on the payments sandbox.",
			assert: func(t *testing.T, call recordedCall) {
				t.Helper()
				// The assistant's tool request and the observation must both be
				// in the transcript, in that order: a tool message that answers
				// no visible tool call is rejected by the provider.
				if len(call.messages) != 3 {
					t.Fatalf("second call messages = %d, want 3 (question, assistant tool call, observation)", len(call.messages))
				}
				if call.messages[1].Role != llm.RoleAssistant || len(call.messages[1].ToolCalls) != 1 {
					t.Errorf("messages[1] = %+v, want the assistant's tool call", call.messages[1])
				}
				if call.messages[2].Role != llm.RoleTool {
					t.Errorf("messages[2].Role = %q, want %q", call.messages[2].Role, llm.RoleTool)
				}
				if call.messages[2].ToolCallID != "call_1" {
					t.Errorf("observation tool_call_id = %q, want call_1", call.messages[2].ToolCallID)
				}
				if !strings.Contains(call.messages[2].Content, "ATLAS-1 [Blocked]") {
					t.Errorf("observation = %q, want the tool's content", call.messages[2].Content)
				}
			},
			inputTokens:  200,
			outputTokens: 30,
		},
	)

	o := newOrchestrator(t, pool, provider, newRegistry(t, tool), orchestratorOptions{})
	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// One reasoning turn, one answer, one completeness check on that answer.
	if provider.callCount() != 3 {
		t.Errorf("provider calls = %d, want 3", provider.callCount())
	}
	if tool.executions() != 1 {
		t.Errorf("tool executions = %d, want 1", tool.executions())
	}

	// REQ-2.2 step 5: answer, status and token totals persisted.
	run := loadRun(t, pool, seeded.runID)
	if run.status != "completed" {
		t.Errorf("status = %q, want completed", run.status)
	}
	if run.answer == nil || *run.answer != "ATLAS-1 is blocked on the payments sandbox." {
		t.Errorf("answer = %v, want the model's text", run.answer)
	}
	if run.errText != nil {
		t.Errorf("error = %q on a completed run, want null", *run.errText)
	}
	if !run.finished {
		t.Error("finished_at is not set on a completed run")
	}
	if run.inputTokens == nil || *run.inputTokens != 300 {
		t.Errorf("input_tokens = %v, want 300 (100+200)", run.inputTokens)
	}
	if run.outputTokens == nil || *run.outputTokens != 50 {
		t.Errorf("output_tokens = %v, want 50 (20+30)", run.outputTokens)
	}
	if run.latencyMS == nil {
		t.Error("latency_ms was not recorded")
	}

	// The assistant's answer joins the conversation.
	msgs := messagesOf(t, pool, seeded.conversationID)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (question + answer)", len(msgs))
	}
	if msgs[1].role != "assistant" || msgs[1].content != "ATLAS-1 is blocked on the payments sandbox." {
		t.Errorf("last message = %+v, want the assistant's answer", msgs[1])
	}

	// REQ-2.3: the canonical event set, in order, with a gap-free seq.
	events := loadEvents(t, pool, seeded.runID)
	want := []string{
		agent.EventRunStarted,
		agent.EventLLMCall,
		agent.EventToolCallStarted,
		agent.EventToolCallFinished,
		agent.EventLLMCall,
		// The completeness check: a generation that reviews the draft answer.
		agent.EventLLMCall,
		agent.EventAnswer,
		agent.EventRunFinished,
	}
	if got := eventTypes(events); !equalStrings(got, want) {
		t.Errorf("event types = %v, want %v", got, want)
	}
	for i, e := range events {
		if int(e.Seq) != i+1 {
			t.Errorf("events[%d].seq = %d, want %d (seq must be gap-free)", i, e.Seq, i+1)
		}
	}

	// REQ-2.3 amendment: Day 1's names are superseded and must not survive.
	for _, e := range events {
		if e.Type == "llm_call_completed" || e.Type == "run_completed" {
			t.Errorf("event type %q is a superseded Day 1 name", e.Type)
		}
	}

	// The tool_call_started payload carries full arguments; tool_call_finished
	// carries the observation and the evidence. Both are what a replay needs.
	started := decodePayload(t, eventsOfType(events, agent.EventToolCallStarted)[0])
	if started["tool"] != "jira_search_issues" {
		t.Errorf("tool_call_started.tool = %v, want jira_search_issues", started["tool"])
	}
	if args, ok := started["arguments"].(map[string]any); !ok || args["q"] != "blocked" {
		t.Errorf("tool_call_started.arguments = %v, want the full arguments", started["arguments"])
	}

	finished := decodePayload(t, eventsOfType(events, agent.EventToolCallFinished)[0])
	if finished["status"] != "ok" {
		t.Errorf("tool_call_finished.status = %v, want ok", finished["status"])
	}
	if obs, _ := finished["observation"].(string); !strings.Contains(obs, "ATLAS-1 [Blocked]") {
		t.Errorf("tool_call_finished.observation = %q, want the tool content", obs)
	}
	if count, _ := finished["evidence_count"].(float64); count != 1 {
		t.Errorf("tool_call_finished.evidence_count = %v, want 1", finished["evidence_count"])
	}
	evidence, ok := finished["evidence"].([]any)
	if !ok || len(evidence) != 1 {
		t.Fatalf("tool_call_finished.evidence = %v, want one item", finished["evidence"])
	}
	item, _ := evidence[0].(map[string]any)
	if item["external_id"] != "ATLAS-1" || item["source"] != "jira" {
		t.Errorf("evidence item = %v, want the jira ATLAS-1 item", item)
	}

	answer := decodePayload(t, eventsOfType(events, agent.EventAnswer)[0])
	if answer["answer"] != "ATLAS-1 is blocked on the payments sandbox." {
		t.Errorf("answer event = %v, want the answer text", answer["answer"])
	}
	if forced, _ := answer["forced"].(bool); forced {
		t.Error("answer event says forced=true, but the model volunteered the answer")
	}

	// Every generation is accounted for on llm_calls with its purpose.
	calls := loadLLMCalls(t, pool, seeded.runID)
	if len(calls) != 3 {
		t.Fatalf("llm_calls = %d, want 3", len(calls))
	}
	if last := calls[len(calls)-1]; last.purpose != agent.PurposeCompletenessCheck {
		t.Errorf("final llm_call purpose = %q, want %q", last.purpose, agent.PurposeCompletenessCheck)
	}
	for i, c := range calls[:len(calls)-1] {
		if c.purpose != agent.PurposeAgentLoop {
			t.Errorf("llm_calls[%d].purpose = %q, want %q", i, c.purpose, agent.PurposeAgentLoop)
		}
		if c.model != testModel {
			t.Errorf("llm_calls[%d].model = %q, want %q", i, c.model, testModel)
		}
	}
}

// Prior conversation turns are replayed to the model (REQ-2.2 step 1).
func TestRunLoadsConversationHistory(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "and which of those slipped?",
		historyMessage{role: "user", content: "which Atlas issues are blocked?"},
		historyMessage{role: "assistant", content: "ATLAS-1 and ATLAS-2."},
	)

	provider := newFakeProvider(t, providerStep{
		kind: toolsCall,
		text: "ATLAS-2 slipped twice.",
		assert: func(t *testing.T, call recordedCall) {
			t.Helper()
			if len(call.messages) != 3 {
				t.Fatalf("messages = %d, want 3 (two history turns + the question)", len(call.messages))
			}
			wantRoles := []llm.Role{llm.RoleUser, llm.RoleAssistant, llm.RoleUser}
			for i, role := range wantRoles {
				if call.messages[i].Role != role {
					t.Errorf("messages[%d].Role = %q, want %q", i, call.messages[i].Role, role)
				}
			}
			if call.messages[2].Content != "and which of those slipped?" {
				t.Errorf("last message = %q, want the new question", call.messages[2].Content)
			}
		},
	})

	o := newOrchestrator(t, pool, provider, newRegistry(t, &fakeTool{name: "jira_get_issue"}), orchestratorOptions{})
	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run := loadRun(t, pool, seeded.runID); run.status != "completed" {
		t.Errorf("status = %q, want completed", run.status)
	}
}

// ---------------------------------------------------------------------------
// iteration cap
// ---------------------------------------------------------------------------

// A model that never stops calling tools must be cut off at MAX_ITERATIONS and
// then asked for a best-effort answer with no tools attached (REQ-2.2 step 4).
func TestRunMaxIterationCap(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "why is everything late?")

	tool := &fakeTool{name: "jira_search_issues"}

	provider := newFakeProvider(t,
		providerStep{kind: toolsCall, toolCalls: []llm.ToolCall{toolCall("c1", "jira_search_issues", `{"q":"one"}`)}},
		providerStep{kind: toolsCall, toolCalls: []llm.ToolCall{toolCall("c2", "jira_search_issues", `{"q":"two"}`)}},
		providerStep{
			// No tools: the forced answer must not be able to keep investigating.
			kind: plainCall,
			text: "Best effort: two searches ran, and I could not finish.",
			assert: func(t *testing.T, call recordedCall) {
				t.Helper()
				if len(call.tools) != 0 {
					t.Errorf("forced answer call carried %d tool definitions, want 0", len(call.tools))
				}
				last := call.messages[len(call.messages)-1]
				if last.Role != llm.RoleUser {
					t.Errorf("last message role = %q, want a user instruction", last.Role)
				}
				if !strings.Contains(strings.ToLower(last.Content), "limit") {
					t.Errorf("forced instruction = %q, want it to state the investigation limit", last.Content)
				}
			},
		},
	)

	o := newOrchestrator(t, pool, provider, newRegistry(t, tool),
		orchestratorOptions{maxIterations: 2})
	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if provider.callCount() != 3 {
		t.Errorf("provider calls = %d, want 3 (2 capped iterations + 1 forced answer)", provider.callCount())
	}
	if tool.executions() != 2 {
		t.Errorf("tool executions = %d, want 2 (the cap bounds tool work too)", tool.executions())
	}

	run := loadRun(t, pool, seeded.runID)
	if run.status != "completed" {
		t.Errorf("status = %q, want completed (a capped run still answers)", run.status)
	}
	if run.answer == nil || !strings.Contains(*run.answer, "Best effort") {
		t.Errorf("answer = %v, want the forced answer", run.answer)
	}

	events := loadEvents(t, pool, seeded.runID)
	answer := decodePayload(t, eventsOfType(events, agent.EventAnswer)[0])
	if forced, _ := answer["forced"].(bool); !forced {
		t.Error("answer event forced = false, want true after the iteration cap")
	}
	if iterations, _ := answer["iterations"].(float64); iterations != 2 {
		t.Errorf("answer event iterations = %v, want 2", answer["iterations"])
	}

	purposes := purposesOf(loadLLMCalls(t, pool, seeded.runID))
	want := []string{agent.PurposeAgentLoop, agent.PurposeAgentLoop, agent.PurposeFinalAnswer}
	if !equalStrings(purposes, want) {
		t.Errorf("llm_calls purposes = %v, want %v", purposes, want)
	}
}

// ---------------------------------------------------------------------------
// dedupe
// ---------------------------------------------------------------------------

// The same (name, canonicalized args) pair executes once per run, however the
// model spells it and however many iterations apart it asks (REQ-2.2 step 2).
func TestRunDedupesIdenticalToolCalls(t *testing.T) {
	schema := json.RawMessage(`{
	  "type": "object",
	  "properties": {"q": {"type": "string"}, "n": {"type": "integer"}},
	  "required": ["q"]
	}`)

	tests := []struct {
		name  string
		steps []providerStep
		// wantFinished is how many tool_call_finished events to expect.
		wantFinished int
	}{
		{
			name: "twice in one iteration, keys in a different order",
			steps: []providerStep{
				{kind: toolsCall, toolCalls: []llm.ToolCall{
					toolCall("c1", "jira_search_issues", `{"q":"blocked","n":10}`),
					toolCall("c2", "jira_search_issues", `{"n":10, "q":"blocked"}`),
				}},
				{kind: toolsCall, text: "done"},
			},
			wantFinished: 2,
		},
		{
			name: "again on a later iteration, respelled",
			steps: []providerStep{
				{kind: toolsCall, toolCalls: []llm.ToolCall{
					toolCall("c1", "jira_search_issues", `{"q":"blocked","n":10}`),
				}},
				{kind: toolsCall, toolCalls: []llm.ToolCall{
					toolCall("c2", "jira_search_issues", "{\n  \"n\": 10,\n  \"q\": \"blocked\"\n}"),
				}},
				{kind: toolsCall, text: "done"},
			},
			wantFinished: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := testPool(t)
			seeded := seedRun(t, pool, "which Atlas issues are blocked?")

			tool := &fakeTool{
				name:   "jira_search_issues",
				schema: schema,
				run: func(attempt int, _ json.RawMessage) (tools.Result, error) {
					return tools.Result{
						Content:  fmt.Sprintf("execution #%d", attempt),
						Evidence: []tools.EvidenceItem{{Source: "jira", ExternalID: "ATLAS-1"}},
					}, nil
				},
			}

			provider := newFakeProvider(t, tt.steps...)
			o := newOrchestrator(t, pool, provider, newRegistry(t, tool), orchestratorOptions{})
			if err := o.Run(context.Background(), seeded.runID); err != nil {
				t.Fatalf("Run: %v", err)
			}

			if tool.executions() != 1 {
				t.Errorf("tool executions = %d, want 1 (identical calls are deduped)", tool.executions())
			}

			events := loadEvents(t, pool, seeded.runID)
			finished := eventsOfType(events, agent.EventToolCallFinished)
			if len(finished) != tt.wantFinished {
				t.Fatalf("tool_call_finished events = %d, want %d (each call is still reported)",
					len(finished), tt.wantFinished)
			}

			first := decodePayload(t, finished[0])
			second := decodePayload(t, finished[1])
			if first["observation"] != second["observation"] {
				t.Errorf("observations differ: %q vs %q; the cached result must be returned verbatim",
					first["observation"], second["observation"])
			}
			if hit, _ := first["cache_hit"].(bool); hit {
				t.Error("first tool_call_finished reports cache_hit=true, want false")
			}
			if hit, _ := second["cache_hit"].(bool); !hit {
				t.Error("repeat tool_call_finished reports cache_hit=false, want true")
			}
			// The observation is fenced as untrusted, so assert on what the
			// dedupe is actually about: the repeat carries the FIRST execution's
			// result and never the second's.
			obs, _ := second["observation"].(string)
			if !strings.Contains(obs, "execution #1") {
				t.Errorf("repeat observation = %q, want the first execution's content", obs)
			}
			if strings.Contains(obs, "execution #2") {
				t.Errorf("repeat observation = %q, want the cached result, not a re-execution", obs)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// invalid arguments
// ---------------------------------------------------------------------------

// Arguments that do not match the schema become an observation the model can
// correct, and the loop continues. Nothing here may reach the tool.
func TestRunInvalidArgumentsBecomeObservations(t *testing.T) {
	schema := json.RawMessage(`{
	  "type": "object",
	  "properties": {"q": {"type": "string"}, "n": {"type": "integer"}},
	  "required": ["q"]
	}`)

	tests := []struct {
		name     string
		call     llm.ToolCall
		wantIn   []string
		wantTool string
	}{
		{
			name:     "required argument missing",
			call:     toolCall("c1", "jira_search_issues", `{"n":5}`),
			wantIn:   []string{"Error", "q"},
			wantTool: "jira_search_issues",
		},
		{
			name:     "wrong type",
			call:     toolCall("c1", "jira_search_issues", `{"q":123}`),
			wantIn:   []string{"Error", "q", "string"},
			wantTool: "jira_search_issues",
		},
		{
			name:     "unknown argument",
			call:     toolCall("c1", "jira_search_issues", `{"q":"blocked","project":"ATLAS"}`),
			wantIn:   []string{"Error", "project"},
			wantTool: "jira_search_issues",
		},
		{
			// Truncated argument JSON is a real provider failure mode (a cut-off
			// streamed tool call), and REQ-2.2 step 2 puts it in the same
			// category as any other bad argument: "invalid → error observation
			// back to the model, no crash".
			name:     "arguments are not valid JSON",
			call:     toolCall("c1", "jira_search_issues", `{"q":`),
			wantIn:   []string{"Error", "JSON"},
			wantTool: "jira_search_issues",
		},
		{
			name:     "hallucinated tool name",
			call:     toolCall("c1", "jira_delete_everything", `{"q":"blocked"}`),
			wantIn:   []string{"Error", "jira_delete_everything", "jira_search_issues"},
			wantTool: "jira_delete_everything",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := testPool(t)
			seeded := seedRun(t, pool, "which Atlas issues are blocked?")

			tool := &fakeTool{name: "jira_search_issues", schema: schema}

			provider := newFakeProvider(t,
				providerStep{kind: toolsCall, toolCalls: []llm.ToolCall{tt.call}},
				providerStep{
					kind: toolsCall,
					text: "I could not run that search.",
					assert: func(t *testing.T, call recordedCall) {
						t.Helper()
						// The loop continued: the error came back as a tool
						// observation on the same tool_call_id.
						last := call.messages[len(call.messages)-1]
						if last.Role != llm.RoleTool {
							t.Fatalf("last message role = %q, want a tool observation", last.Role)
						}
						if last.ToolCallID != tt.call.ID {
							t.Errorf("observation tool_call_id = %q, want %q", last.ToolCallID, tt.call.ID)
						}
						for _, want := range tt.wantIn {
							if !strings.Contains(last.Content, want) {
								t.Errorf("observation %q does not mention %q", last.Content, want)
							}
						}
					},
				},
			)

			o := newOrchestrator(t, pool, provider, newRegistry(t, tool), orchestratorOptions{})
			if err := o.Run(context.Background(), seeded.runID); err != nil {
				t.Fatalf("Run: %v", err)
			}

			if tool.executions() != 0 {
				t.Errorf("tool executions = %d, want 0 (invalid arguments never reach the tool)", tool.executions())
			}
			if provider.callCount() != 3 {
				t.Errorf("provider calls = %d, want 3 (the loop continues past a bad call, then the answer is checked)", provider.callCount())
			}

			run := loadRun(t, pool, seeded.runID)
			if run.status != "completed" {
				t.Errorf("status = %q, want completed (a bad tool call must not fail the run)", run.status)
			}
			if run.answer == nil || *run.answer != "I could not run that search." {
				t.Errorf("answer = %v, want the model's recovery answer", run.answer)
			}

			events := loadEvents(t, pool, seeded.runID)
			finished := eventsOfType(events, agent.EventToolCallFinished)
			if len(finished) != 1 {
				t.Fatalf("tool_call_finished events = %d, want 1", len(finished))
			}
			payload := decodePayload(t, finished[0])
			if payload["status"] != "error" {
				t.Errorf("tool_call_finished.status = %v, want error", payload["status"])
			}
			if payload["tool"] != tt.wantTool {
				t.Errorf("tool_call_finished.tool = %v, want %q", payload["tool"], tt.wantTool)
			}
			if count, _ := payload["evidence_count"].(float64); count != 0 {
				t.Errorf("evidence_count = %v on a failed call, want 0", payload["evidence_count"])
			}

			started := eventsOfType(events, agent.EventToolCallStarted)
			if len(started) != 1 {
				t.Errorf("tool_call_started events = %d, want 1 (an attempt is still recorded)", len(started))
			}
			// The payload must be valid JSON even when the model's arguments
			// were not, or the transcript row cannot be stored at all.
			if args := decodePayload(t, started[0])["arguments"]; args == nil {
				t.Error("tool_call_started.arguments is null, want at least an empty object")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// tool execution failure
// ---------------------------------------------------------------------------

// A failing tool becomes an observation too, and the retry policy distinguishes
// a permanent failure from a transient one (REQ-2.2 step 2).
func TestRunToolExecutionErrorBecomesObservation(t *testing.T) {
	permanentErr := fmt.Errorf("%q is not a valid Jira issue key: %w", "the payments ticket", tools.ErrInvalidArgument)

	tests := []struct {
		name           string
		run            func(attempt int, args json.RawMessage) (tools.Result, error)
		wantExecutions int
		wantIn         string
	}{
		{
			name: "permanent failure is not retried",
			run: func(_ int, _ json.RawMessage) (tools.Result, error) {
				return tools.Result{}, permanentErr
			},
			wantExecutions: 1,
			wantIn:         "not a valid Jira issue key",
		},
		{
			name: "transient failure is retried once",
			run: func(_ int, _ json.RawMessage) (tools.Result, error) {
				return tools.Result{}, errors.New("jira: HTTP 503: service unavailable")
			},
			wantExecutions: 2,
			wantIn:         "HTTP 503",
		},
		{
			name: "retry that succeeds returns the result",
			run: func(attempt int, _ json.RawMessage) (tools.Result, error) {
				if attempt == 1 {
					return tools.Result{}, errors.New("jira: HTTP 502: bad gateway")
				}
				return tools.Result{
					Content:  "ATLAS-9 [Blocked] vendor sandbox",
					Evidence: []tools.EvidenceItem{{Source: "jira", ExternalID: "ATLAS-9"}},
				}, nil
			},
			wantExecutions: 2,
			wantIn:         "ATLAS-9",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := testPool(t)
			seeded := seedRun(t, pool, "which Atlas issues are blocked?")

			tool := &fakeTool{name: "jira_search_issues", run: tt.run}

			provider := newFakeProvider(t,
				providerStep{kind: toolsCall, toolCalls: []llm.ToolCall{
					toolCall("c1", "jira_search_issues", `{"q":"blocked"}`),
				}},
				providerStep{
					kind: toolsCall,
					text: "Here is what I could establish.",
					assert: func(t *testing.T, call recordedCall) {
						t.Helper()
						last := call.messages[len(call.messages)-1]
						if last.Role != llm.RoleTool {
							t.Fatalf("last message role = %q, want a tool observation", last.Role)
						}
						if !strings.Contains(last.Content, tt.wantIn) {
							t.Errorf("observation %q does not mention %q", last.Content, tt.wantIn)
						}
					},
				},
			)

			o := newOrchestrator(t, pool, provider, newRegistry(t, tool), orchestratorOptions{})
			if err := o.Run(context.Background(), seeded.runID); err != nil {
				t.Fatalf("Run: %v", err)
			}

			if tool.executions() != tt.wantExecutions {
				t.Errorf("tool executions = %d, want %d", tool.executions(), tt.wantExecutions)
			}
			if run := loadRun(t, pool, seeded.runID); run.status != "completed" {
				t.Errorf("status = %q, want completed (a failing tool must not fail the run)", run.status)
			}
		})
	}
}

// A provider failure is different in kind: there is no observation to recover
// with, so the run is recorded as failed rather than crashing the job.
func TestRunProviderFailureIsRecorded(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "which Atlas issues are blocked?")

	provider := newFakeProvider(t, providerStep{err: errors.New("openai: 500 upstream exploded")})

	o := newOrchestrator(t, pool, provider, newRegistry(t, &fakeTool{name: "jira_search_issues"}), orchestratorOptions{})
	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run returned an error; an ordinary failure must be recorded, not returned: %v", err)
	}

	run := loadRun(t, pool, seeded.runID)
	if run.status != "failed" {
		t.Errorf("status = %q, want failed", run.status)
	}
	if run.errText == nil || *run.errText == "" {
		t.Error("error was not recorded on a failed run")
	}
	if run.answer != nil {
		t.Errorf("answer = %q on a failed run, want null", *run.answer)
	}
	if !run.finished {
		t.Error("finished_at is not set on a failed run")
	}

	events := loadEvents(t, pool, seeded.runID)
	if len(eventsOfType(events, agent.EventRunFailed)) != 1 {
		t.Errorf("event types = %v, want a %s event", eventTypes(events), agent.EventRunFailed)
	}
}

// A run that already reached a terminal state is skipped: River retries a job
// after a worker crash, and re-answering would spend money to overwrite an
// answer that already exists.
func TestRunSkipsTerminalRun(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "which Atlas issues are blocked?")
	if _, err := pool.Exec(context.Background(),
		`UPDATE agent_runs SET status = 'completed', answer = 'already answered', finished_at = now() WHERE id = $1`,
		seeded.runID); err != nil {
		t.Fatalf("mark run completed: %v", err)
	}

	provider := newFakeProvider(t)
	o := newOrchestrator(t, pool, provider, newRegistry(t, &fakeTool{name: "jira_search_issues"}), orchestratorOptions{})
	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if provider.callCount() != 0 {
		t.Errorf("provider calls = %d on a finished run, want 0", provider.callCount())
	}
	if run := loadRun(t, pool, seeded.runID); run.answer == nil || *run.answer != "already answered" {
		t.Errorf("answer = %v, want the existing answer left untouched", run.answer)
	}
	if events := loadEvents(t, pool, seeded.runID); len(events) != 0 {
		t.Errorf("events = %v, want none for a skipped run", eventTypes(events))
	}
}

// ---------------------------------------------------------------------------
// TEST-2.2 — truncation and utility-model summarization
// ---------------------------------------------------------------------------

// An oversized tool result keeps its head verbatim and has the overflow
// summarized by the utility model under purpose "tool_output_summary".
func TestRunSummarizesOversizedToolResult(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "which Atlas issues are blocked?")

	const budget = 100
	head := strings.Repeat("H", 60)
	tail := strings.Repeat("T", 440)
	oversized := head + tail // 500 runes, 5x the budget

	tool := &fakeTool{
		name: "jira_search_issues",
		run: func(_ int, _ json.RawMessage) (tools.Result, error) {
			return tools.Result{
				Content:  oversized,
				Evidence: []tools.EvidenceItem{{Source: "jira", ExternalID: "ATLAS-1"}},
			}, nil
		},
	}

	provider := newFakeProvider(t,
		providerStep{kind: toolsCall, toolCalls: []llm.ToolCall{
			toolCall("c1", "jira_search_issues", `{"q":"blocked"}`),
		}},
		providerStep{
			// The summarization call: the utility model, off the run
			// transcript, with the overflow as its only input.
			kind: plainCall,
			text: "SUMMARY",
			assert: func(t *testing.T, call recordedCall) {
				t.Helper()
				if call.model != testUtilityModel {
					t.Errorf("summarization model = %q, want the utility model %q", call.model, testUtilityModel)
				}
				if len(call.tools) != 0 {
					t.Errorf("summarization call carried %d tool definitions, want 0", len(call.tools))
				}
				if len(call.messages) != 1 {
					t.Fatalf("summarization messages = %d, want 1 (the overflow only; it must not see the conversation)",
						len(call.messages))
				}
				if call.messages[0].Content != tail {
					t.Errorf("summarization input = %q…, want exactly the overflow", truncateForMessage(call.messages[0].Content))
				}
				if strings.Contains(call.system, "investigative analyst") {
					t.Error("summarization used the agent's system prompt, want the compression instruction")
				}
			},
		},
		providerStep{
			kind: toolsCall,
			text: "ATLAS-1 is blocked.",
			assert: func(t *testing.T, call recordedCall) {
				t.Helper()
				observation := call.messages[len(call.messages)-1]
				if observation.Role != llm.RoleTool {
					t.Fatalf("last message role = %q, want a tool observation", observation.Role)
				}
				// The head is kept verbatim, but the observation is fenced as
				// untrusted, so it opens with the fence rather than the head.
				if !strings.Contains(observation.Content, head) {
					t.Error("observation does not carry the verbatim head of the result")
				}
				if !strings.HasPrefix(observation.Content, "<tool_result ") {
					t.Errorf("observation is not fenced as untrusted: %q",
						truncateForMessage(observation.Content))
				}
				if strings.Contains(observation.Content, tail) {
					t.Error("observation still carries the raw overflow; it must be summarized")
				}
				if !strings.Contains(observation.Content, "SUMMARY") {
					t.Errorf("observation %q does not contain the utility model's summary",
						truncateForMessage(observation.Content))
				}
				// The model must be able to tell a complete result from a
				// shortened one, or it will conclude "there are 40" from 40.
				if !strings.Contains(observation.Content, "summarized") &&
					!strings.Contains(observation.Content, "remaining") {
					t.Errorf("observation %q does not mark itself as shortened",
						truncateForMessage(observation.Content))
				}
			},
		},
	)

	o := newOrchestrator(t, pool, provider, newRegistry(t, tool),
		orchestratorOptions{maxToolContentChars: budget})
	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if provider.callCount() != 4 {
		t.Fatalf("provider calls = %d, want 4 (loop, summarize, loop, completeness check)", provider.callCount())
	}

	// TEST-2.2's purpose assertion, as persisted: llm_calls carries the
	// summarization under its own purpose and on the utility model.
	calls := loadLLMCalls(t, pool, seeded.runID)
	want := []string{agent.PurposeAgentLoop, agent.PurposeToolOutputSummary,
		agent.PurposeAgentLoop, agent.PurposeCompletenessCheck}
	if got := purposesOf(calls); !equalStrings(got, want) {
		t.Fatalf("llm_calls purposes = %v, want %v", got, want)
	}
	if calls[1].model != testUtilityModel {
		t.Errorf("summarization llm_call model = %q, want %q", calls[1].model, testUtilityModel)
	}

	events := loadEvents(t, pool, seeded.runID)
	var summaryEvents int
	for _, e := range eventsOfType(events, agent.EventLLMCall) {
		if decodePayload(t, e)["purpose"] == agent.PurposeToolOutputSummary {
			summaryEvents++
		}
	}
	if summaryEvents != 1 {
		t.Errorf("llm_call events with purpose %q = %d, want 1", agent.PurposeToolOutputSummary, summaryEvents)
	}

	finished := decodePayload(t, eventsOfType(events, agent.EventToolCallFinished)[0])
	if truncated, _ := finished["truncated"].(bool); !truncated {
		t.Error("tool_call_finished.truncated = false, want true")
	}
	if summarized, _ := finished["summarized"].(bool); !summarized {
		t.Error("tool_call_finished.summarized = false, want true")
	}
	if raw, _ := finished["raw_content_length"].(float64); int(raw) != len(oversized) {
		t.Errorf("tool_call_finished.raw_content_length = %v, want %d (the pre-truncation size)",
			finished["raw_content_length"], len(oversized))
	}
	// The stored observation is what the model saw, not the raw result: a replay
	// from the raw content would feed the model a conversation it never had.
	observation, _ := finished["observation"].(string)
	if strings.Contains(observation, tail) {
		t.Error("tool_call_finished.observation carries the raw overflow, want the shortened text")
	}
	if !strings.Contains(observation, "SUMMARY") {
		t.Errorf("tool_call_finished.observation = %q, want the summary", truncateForMessage(observation))
	}
}

// A result inside the budget is passed through untouched — no utility call, no
// truncation marker.
func TestRunDoesNotSummarizeResultWithinBudget(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "which Atlas issues are blocked?")

	content := strings.Repeat("x", 99)
	tool := &fakeTool{
		name: "jira_search_issues",
		run: func(_ int, _ json.RawMessage) (tools.Result, error) {
			return tools.Result{Content: content, Evidence: []tools.EvidenceItem{{Source: "jira", ExternalID: "A-1"}}}, nil
		},
	}

	provider := newFakeProvider(t,
		providerStep{kind: toolsCall, toolCalls: []llm.ToolCall{
			toolCall("c1", "jira_search_issues", `{"q":"blocked"}`),
		}},
		providerStep{
			kind: toolsCall,
			text: "done",
			assert: func(t *testing.T, call recordedCall) {
				t.Helper()
				// The observation is fenced as untrusted third-party text, so it
				// is the content wrapped, not the content verbatim. Assert the
				// fence is present AND that nothing inside it was altered.
				got := call.messages[len(call.messages)-1].Content
				if !strings.Contains(got, content) {
					t.Errorf("observation lost the tool's content: %q", truncateForMessage(got))
				}
				if !strings.HasPrefix(got, `<tool_result tool="jira_search_issues" trust="untrusted">`) {
					t.Errorf("observation is not fenced as untrusted: %q", truncateForMessage(got))
				}
			},
		},
	)

	o := newOrchestrator(t, pool, provider, newRegistry(t, tool),
		orchestratorOptions{maxToolContentChars: 100})
	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := purposesOf(loadLLMCalls(t, pool, seeded.runID)); !equalStrings(got,
		[]string{agent.PurposeAgentLoop, agent.PurposeAgentLoop, agent.PurposeCompletenessCheck}) {
		t.Errorf("llm_calls purposes = %v, want two agent_loop calls, the completeness check, and no summarization", got)
	}
	finished := decodePayload(t, eventsOfType(loadEvents(t, pool, seeded.runID), agent.EventToolCallFinished)[0])
	if truncated, _ := finished["truncated"].(bool); truncated {
		t.Error("tool_call_finished.truncated = true for a result inside the budget")
	}
}

// A failing summarization must degrade to a marked hard cut, not lose the run.
func TestRunFallsBackWhenSummarizationFails(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "which Atlas issues are blocked?")

	oversized := strings.Repeat("Z", 500)
	tool := &fakeTool{
		name: "jira_search_issues",
		run: func(_ int, _ json.RawMessage) (tools.Result, error) {
			return tools.Result{Content: oversized, Evidence: []tools.EvidenceItem{{Source: "jira", ExternalID: "A-1"}}}, nil
		},
	}

	provider := newFakeProvider(t,
		providerStep{kind: toolsCall, toolCalls: []llm.ToolCall{
			toolCall("c1", "jira_search_issues", `{"q":"blocked"}`),
		}},
		providerStep{kind: plainCall, err: errors.New("openai: utility model unavailable")},
		providerStep{
			kind: toolsCall,
			text: "partial answer",
			assert: func(t *testing.T, call recordedCall) {
				t.Helper()
				observation := call.messages[len(call.messages)-1].Content
				if len([]rune(observation)) >= len(oversized) {
					t.Errorf("observation was not shortened (%d runes)", len([]rune(observation)))
				}
				if !strings.Contains(observation, "truncated") {
					t.Errorf("observation %q does not say it was truncated", truncateForMessage(observation))
				}
			},
		},
	)

	o := newOrchestrator(t, pool, provider, newRegistry(t, tool),
		orchestratorOptions{maxToolContentChars: 100})
	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run := loadRun(t, pool, seeded.runID); run.status != "completed" {
		t.Errorf("status = %q, want completed (a failed summary must not fail the run)", run.status)
	}
	finished := decodePayload(t, eventsOfType(loadEvents(t, pool, seeded.runID), agent.EventToolCallFinished)[0])
	if summarized, _ := finished["summarized"].(bool); summarized {
		t.Error("tool_call_finished.summarized = true although summarization failed")
	}
	if truncated, _ := finished["truncated"].(bool); !truncated {
		t.Error("tool_call_finished.truncated = false, want true")
	}
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

func purposesOf(calls []llmCallRow) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.purpose)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// truncateForMessage keeps failure output readable when the value under test is
// a wall of padding characters.
func truncateForMessage(s string) string {
	runes := []rune(s)
	if len(runes) <= 120 {
		return s
	}
	return string(runes[:120]) + "…"
}
