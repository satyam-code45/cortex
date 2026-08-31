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

// The context overflow guard.
//
// A long run over a small ContextTokenBudget must compact its OLDEST tool
// observations into utility-model summaries while sparing the newest ones,
// bring the transcript estimate back under budget before the next generation,
// and record the rewrite as a context_compaction event carrying the full
// replacement text — so ReconstructTranscript rebuilds the compacted
// transcript exactly (the replay invariant).

// compactionBudget is deliberately tiny next to the huge first observation and
// comfortably above the system prompt plus the recent observations, so exactly
// one compaction pass is both necessary and sufficient.
const compactionBudget = 6000

// newCompactionOrchestrator builds an orchestrator with the small budget. The
// tool-content cap is raised so the per-result summarizer stays out of
// the way: this test is about the transcript-level guard.
func newCompactionOrchestrator(t *testing.T, pool *pgxpool.Pool, provider llm.Provider, registry *tools.Registry) *agent.Orchestrator {
	t.Helper()
	o, err := agent.New(agent.Config{
		DB:                  pool,
		Provider:            provider,
		Registry:            registry,
		Model:               testModel,
		UtilityModel:        testUtilityModel,
		MaxToolContentChars: 200000,
		ContextTokenBudget:  compactionBudget,
		Logger:              discardLogger(),
		Now:                 func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("build orchestrator: %v", err)
	}
	return o
}

// compactionTool answers "huge" with an observation far over the budget and
// anything else with a small, distinct one.
func compactionTool() *fakeTool {
	return &fakeTool{
		name: "jira_search_issues",
		run: func(_ int, args json.RawMessage) (tools.Result, error) {
			var parsed struct {
				Q string `json:"q"`
			}
			_ = json.Unmarshal(args, &parsed)
			content := fmt.Sprintf("obs-%s ", parsed.Q)
			if parsed.Q == "huge" {
				content += strings.Repeat("A", 40000)
			} else {
				content += strings.Repeat("x", 1500)
			}
			return tools.Result{
				Content:  content,
				Evidence: []tools.EvidenceItem{{Source: "jira", ExternalID: "ATLAS-" + parsed.Q}},
			}, nil
		},
	}
}

// estimateTranscriptTokens mirrors the guard's published accounting — system
// prompt, message contents, and tool-call names+arguments, at the usual four
// chars per token — so the test can check the budget independently of the
// implementation's own bookkeeping.
func estimateTranscriptTokens(call recordedCall) int {
	chars := len(call.system)
	for _, m := range call.messages {
		chars += len(m.Content)
		for _, tc := range m.ToolCalls {
			chars += len(tc.Name) + len(tc.Arguments)
		}
	}
	return chars / 4
}

func compactionScript(t *testing.T, summarizeStep providerStep) []providerStep {
	t.Helper()
	return []providerStep{
		// Iteration 1: the huge observation, destined for compaction.
		{kind: toolsCall, toolCalls: []llm.ToolCall{
			toolCall("c1", "jira_search_issues", `{"q":"huge"}`),
		}},
		// Iteration 2: four more observations — exactly the newest set the
		// guard must never touch.
		{kind: toolsCall, toolCalls: []llm.ToolCall{
			toolCall("c2", "jira_search_issues", `{"q":"s2"}`),
			toolCall("c3", "jira_search_issues", `{"q":"s3"}`),
			toolCall("c4", "jira_search_issues", `{"q":"s4"}`),
			toolCall("c5", "jira_search_issues", `{"q":"s5"}`),
		}},
		// Before iteration 3's generation the guard fires; this is its
		// utility-model summarization call.
		summarizeStep,
		// Iteration 3: the model answers over the compacted transcript.
		{
			kind: toolsCall,
			text: "ATLAS-huge is blocked; details in the follow-ups.",
			assert: func(t *testing.T, call recordedCall) {
				t.Helper()
				// The estimate is back under budget before this generation.
				if got := estimateTranscriptTokens(call); got > compactionBudget {
					t.Errorf("transcript estimate before the post-compaction generation = %d tokens, want <= %d",
						got, compactionBudget)
				}
				// The newest observations are spared, verbatim.
				for _, q := range []string{"s2", "s3", "s4", "s5"} {
					if m, ok := findToolMessage(call, "c"+q[1:]); !ok || !strings.Contains(m.Content, "obs-"+q) {
						t.Errorf("recent observation for %q was altered or lost", q)
					}
				}
			},
		},
	}
}

// findToolMessage returns the tool message answering the given tool_call_id.
func findToolMessage(call recordedCall, toolCallID string) (llm.Message, bool) {
	for _, m := range call.messages {
		if m.Role == llm.RoleTool && m.ToolCallID == toolCallID {
			return m, true
		}
	}
	return llm.Message{}, false
}

func TestRunCompactsOldObservationsOverBudget(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "which Atlas issues are blocked?")

	summarize := providerStep{
		kind: plainCall,
		text: "COMPACT-SUMMARY-HUGE",
		assert: func(t *testing.T, call recordedCall) {
			t.Helper()
			if call.model != testUtilityModel {
				t.Errorf("compaction summarization model = %q, want the utility model %q", call.model, testUtilityModel)
			}
			if len(call.messages) != 1 || !strings.Contains(call.messages[0].Content, "obs-huge") {
				t.Error("compaction summarizer was not given the oldest observation")
			}
			if strings.Contains(call.system, "investigative analyst") {
				t.Error("compaction summarization used the agent's system prompt")
			}
		},
	}

	provider := newFakeProvider(t, compactionScript(t, summarize)...)
	o := newCompactionOrchestrator(t, pool, provider, newRegistry(t, compactionTool()))
	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// loop ×3, compaction summary, completeness check, citation pass.
	if provider.callCount() != 6 {
		t.Fatalf("provider calls = %d, want 6", provider.callCount())
	}
	if run := loadRun(t, pool, seeded.runID); run.status != "completed" {
		t.Fatalf("status = %q, want completed; error = %v", run.status, run.errText)
	}

	// The post-compaction generation saw the summary in place of the huge
	// observation — marked as lossy, with the original text gone.
	postCompaction := provider.call(t, 3)
	compacted, ok := findToolMessage(postCompaction, "c1")
	if !ok {
		t.Fatal("the compacted observation's tool message vanished from the transcript")
	}
	if !strings.Contains(compacted.Content, "COMPACT-SUMMARY-HUGE") {
		t.Errorf("compacted observation = %q, want the utility model's summary", truncateForMessage(compacted.Content))
	}
	if !strings.Contains(compacted.Content, "compacted") {
		t.Errorf("compacted observation does not mark itself as lossy: %q", truncateForMessage(compacted.Content))
	}
	if strings.Contains(compacted.Content, strings.Repeat("A", 2000)) {
		t.Error("compacted observation still carries the original bulk")
	}

	// The context_compaction event: full replacement text, budget accounting.
	events := loadEvents(t, pool, seeded.runID)
	compactions := eventsOfType(events, agent.EventContextCompaction)
	if len(compactions) != 1 {
		t.Fatalf("context_compaction events = %d, want exactly 1 (types: %v)", len(compactions), eventTypes(events))
	}
	payload := decodePayload(t, compactions[0])
	if budget, _ := payload["budget"].(float64); int(budget) != compactionBudget {
		t.Errorf("payload budget = %v, want %d", payload["budget"], compactionBudget)
	}
	before, _ := payload["tokens_before"].(float64)
	after, _ := payload["tokens_after"].(float64)
	if int(before) <= compactionBudget {
		t.Errorf("tokens_before = %v, want > %d (compaction fired below budget?)", before, compactionBudget)
	}
	if int(after) > compactionBudget {
		t.Errorf("tokens_after = %v, want <= %d (transcript must end under budget)", after, compactionBudget)
	}
	entries, _ := payload["compactions"].([]any)
	if len(entries) != 1 {
		t.Fatalf("payload compactions = %d entries, want 1", len(entries))
	}
	entry, _ := entries[0].(map[string]any)
	if entry["tool_call_id"] != "c1" {
		t.Errorf("compacted tool_call_id = %v, want c1 (the OLDEST observation)", entry["tool_call_id"])
	}
	// The payload carries the replacement verbatim — byte-identical to what
	// the live transcript now holds. This is what makes replay a substitution
	// rather than a re-derivation.
	if content, _ := entry["content"].(string); content != compacted.Content {
		t.Errorf("event replacement text differs from the live transcript's:\n got %q\nwant %q",
			truncateForMessage(content), truncateForMessage(compacted.Content))
	}
	if originalChars, _ := entry["original_chars"].(float64); int(originalChars) < 40000 {
		t.Errorf("original_chars = %v, want >= 40000 (the observation that was replaced)", entry["original_chars"])
	}

	// The summarization is accounted for under its own purpose.
	purposes := purposesOf(loadLLMCalls(t, pool, seeded.runID))
	want := []string{agent.PurposeAgentLoop, agent.PurposeAgentLoop, agent.PurposeContextCompaction,
		agent.PurposeAgentLoop, agent.PurposeCompletenessCheck, agent.PurposeAnswerCitations}
	if !equalStrings(purposes, want) {
		t.Errorf("llm_calls purposes = %v, want %v", purposes, want)
	}

	// The replay invariant: the transcript rebuilt from run_events alone is
	// EXACTLY the one the loop handed the provider on its last call.
	final := provider.lastCall(t)
	system, messages, err := agent.ReconstructTranscript(events)
	if err != nil {
		t.Fatalf("ReconstructTranscript: %v", err)
	}
	if system != final.system {
		t.Error("reconstructed system prompt differs from the one the loop used")
	}
	assertSameTranscript(t, messages, final.messages)
}

// The guard must keep working exactly when the provider is degraded: a failed
// compaction summarization degrades to a marked hard cut, the event still
// carries the replacement verbatim, and replay still matches.
func TestRunCompactionFallsBackWhenSummarizerFails(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "which Atlas issues are blocked?")

	summarize := providerStep{kind: plainCall, err: errors.New("openai: utility model unavailable")}

	provider := newFakeProvider(t, compactionScript(t, summarize)...)
	o := newCompactionOrchestrator(t, pool, provider, newRegistry(t, compactionTool()))
	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run := loadRun(t, pool, seeded.runID); run.status != "completed" {
		t.Fatalf("status = %q, want completed (a failed compaction summary must not fail the run); error = %v",
			run.status, run.errText)
	}

	postCompaction := provider.call(t, 3)
	compacted, ok := findToolMessage(postCompaction, "c1")
	if !ok {
		t.Fatal("the compacted observation's tool message vanished from the transcript")
	}
	if len(compacted.Content) >= 40000 {
		t.Errorf("observation was not shortened by the fallback (%d chars)", len(compacted.Content))
	}
	if !strings.Contains(compacted.Content, "obs-huge") {
		t.Error("the hard-cut fallback lost the head of the observation")
	}
	if !strings.Contains(compacted.Content, "dropped during compaction") {
		t.Errorf("fallback observation does not say the rest was dropped: %q", truncateForMessage(compacted.Content))
	}

	events := loadEvents(t, pool, seeded.runID)
	compactions := eventsOfType(events, agent.EventContextCompaction)
	if len(compactions) != 1 {
		t.Fatalf("context_compaction events = %d, want 1", len(compactions))
	}
	entries, _ := decodePayload(t, compactions[0])["compactions"].([]any)
	if len(entries) != 1 {
		t.Fatalf("payload compactions = %d entries, want 1", len(entries))
	}
	entry, _ := entries[0].(map[string]any)
	if content, _ := entry["content"].(string); content != compacted.Content {
		t.Error("event replacement text differs from the live transcript's after the fallback")
	}

	final := provider.lastCall(t)
	system, messages, err := agent.ReconstructTranscript(events)
	if err != nil {
		t.Fatalf("ReconstructTranscript: %v", err)
	}
	if system != final.system {
		t.Error("reconstructed system prompt differs from the one the loop used")
	}
	assertSameTranscript(t, messages, final.messages)
}
