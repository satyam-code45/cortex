package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"cortex/internal/agent"
	"cortex/internal/llm"
	"cortex/internal/tools"
)

// TEST-3.4 — a three-hop investigation across Jira, Notion and Gmail.
//
// This is the Day 3 acceptance test in miniature, with the model replaced by a
// script. The chain REQ-3.3 designs is the point: Jira records that the refunds
// integration is "blocked on the provider" and never names it; the Notion plan
// names Nordwind Payments; and only the Gmail thread carries Nordwind's own
// delay announcement, its reason, and the date the launch moved to. No single
// source answers the question, so what is being asserted is that the loop
// carries each observation forward into the next turn and that the final answer
// is built out of all three.

// hopTool builds a scripted tool that returns one source's fact plus its
// evidence. Evidence is mandatory on every result (CLAUDE.md) because Day 4's
// citations are a join back onto these rows.
func hopTool(name, argument, content string, evidence tools.EvidenceItem) *fakeTool {
	return &fakeTool{
		name: name,
		schema: json.RawMessage(fmt.Sprintf(
			`{"type":"object","properties":{%q:{"type":"string"}},"required":[%q]}`,
			argument, argument)),
		run: func(_ int, _ json.RawMessage) (tools.Result, error) {
			return tools.Result{
				Content:  content,
				Evidence: []tools.EvidenceItem{evidence},
			}, nil
		},
	}
}

func TestRunMultiHopAcrossThreeSources(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool,
		"Why was the payment integration delayed and how did it affect the launch date?")

	const (
		jiraObservation = "ATLAS-101 [Blocked] Refunds integration — latest comment: " +
			"\"still blocked on the provider; no sandbox yet\". The provider is not named."
		notionObservation = "# Atlas Q2 Plan\nPayments vendor for refunds is Nordwind Payments (v3 API). " +
			"Refunds sandbox promised 2026-06-15; launch planned for 2026-07-01."
		gmailObservation = "From: ana.vogt@nordwind.example\nDate: 2026-06-12\n" +
			"Subject: Nordwind v3 refunds sandbox: revised availability\n\n" +
			"The v3 refunds sandbox will not be available until 2026-07-15 — our certification " +
			"partner delayed the PCI re-audit."
		finalAnswer = "The refunds work (ATLAS-101) was blocked on the payments vendor, which the " +
			"Atlas Q2 Plan in Notion names as Nordwind Payments. Nordwind's own email of 2026-06-12 " +
			"gives the cause: their certification partner delayed the PCI re-audit, pushing the v3 " +
			"refunds sandbox from 2026-06-15 to 2026-07-15. The launch moved from 2026-07-01 to " +
			"2026-08-14 as a result."
	)

	jiraTool := hopTool("jira_search_issues", "jql", jiraObservation, tools.EvidenceItem{
		Source:     "jira",
		ExternalID: "ATLAS-101",
		Title:      "Refunds integration",
		URL:        "https://vantagelabs.atlassian.net/browse/ATLAS-101",
	})
	notionTool := hopTool("notion_get_page", "page_id", notionObservation, tools.EvidenceItem{
		Source:     "notion",
		ExternalID: "3c34fab1-2b9b-80d6-8cfb-c64d5b8cebe0",
		Title:      "Atlas Q2 Plan",
		URL:        "https://www.notion.so/Atlas-Q2-Plan-3c34fab12b9b80d68cfbc64d5b8cebe0",
	})
	gmailTool := hopTool("gmail_get_message", "message_id", gmailObservation, tools.EvidenceItem{
		Source:     "gmail",
		ExternalID: "18f2a3b4c5d6e7f8",
		Title:      "Nordwind v3 refunds sandbox: revised availability",
		URL:        "https://mail.google.com/mail/u/0/#all/18f2a3b4c5d6e7f8",
	})

	// assertToolsOffered checks that every iteration is handed the whole
	// inventory: REQ-3.4 registers all three sources, and a loop that narrows
	// the tool list after the first hop can never make the second one.
	assertToolsOffered := func(t *testing.T, call recordedCall) {
		t.Helper()
		offered := make([]string, 0, len(call.tools))
		for _, def := range call.tools {
			offered = append(offered, def.Name)
		}
		sort.Strings(offered)
		want := []string{"gmail_get_message", "jira_search_issues", "notion_get_page"}
		if !equalStrings(offered, want) {
			t.Errorf("tool definitions = %v, want all three sources %v", offered, want)
		}
	}

	provider := newFakeProvider(t,
		// Hop 1 — Jira. The ticket says "blocked on the provider" and stops.
		providerStep{
			kind:      toolsCall,
			toolCalls: []llm.ToolCall{toolCall("call_jira", "jira_search_issues", `{"jql":"project = ATLAS AND status = Blocked"}`)},
			assert: func(t *testing.T, call recordedCall) {
				t.Helper()
				assertToolsOffered(t, call)
				if len(call.messages) != 1 || call.messages[0].Role != llm.RoleUser {
					t.Fatalf("first call messages = %+v, want just the user question", call.messages)
				}
			},
			inputTokens:  100,
			outputTokens: 10,
		},
		// Hop 2 — Notion. Only reachable because the Jira observation is in
		// context and named no provider.
		providerStep{
			kind:      toolsCall,
			toolCalls: []llm.ToolCall{toolCall("call_notion", "notion_get_page", `{"page_id":"3c34fab1-2b9b-80d6-8cfb-c64d5b8cebe0"}`)},
			assert: func(t *testing.T, call recordedCall) {
				t.Helper()
				assertToolsOffered(t, call)
				if !containsObservation(call.messages, "blocked on the provider") {
					t.Errorf("hop 2 did not see the Jira observation; messages = %+v", call.messages)
				}
			},
			inputTokens:  200,
			outputTokens: 20,
		},
		// Hop 3 — Gmail. The vendor's name came from Notion, so the search for
		// its mail could not have been written before hop 2.
		providerStep{
			kind:      toolsCall,
			toolCalls: []llm.ToolCall{toolCall("call_gmail", "gmail_get_message", `{"message_id":"18f2a3b4c5d6e7f8"}`)},
			assert: func(t *testing.T, call recordedCall) {
				t.Helper()
				assertToolsOffered(t, call)
				if !containsObservation(call.messages, "Nordwind Payments") {
					t.Errorf("hop 3 did not see the Notion observation; messages = %+v", call.messages)
				}
			},
			inputTokens:  300,
			outputTokens: 30,
		},
		// Synthesis. All three observations must still be in context.
		providerStep{
			kind: toolsCall,
			text: finalAnswer,
			assert: func(t *testing.T, call recordedCall) {
				t.Helper()
				for _, fact := range []string{
					"blocked on the provider",
					"Nordwind Payments",
					"PCI re-audit",
				} {
					if !containsObservation(call.messages, fact) {
						t.Errorf("synthesis turn is missing the observation containing %q", fact)
					}
				}
				if n := countRole(call.messages, llm.RoleTool); n != 3 {
					t.Errorf("tool observations in context = %d, want 3 (one per source)", n)
				}
			},
			inputTokens:  400,
			outputTokens: 40,
		},
	)

	o := newOrchestrator(t, pool, provider,
		newRegistry(t, jiraTool, notionTool, gmailTool), orchestratorOptions{})
	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// All three sources were actually consulted, exactly once each.
	for _, tool := range []*fakeTool{jiraTool, notionTool, gmailTool} {
		if n := tool.executions(); n != 1 {
			t.Errorf("%s executions = %d, want 1", tool.name, n)
		}
	}
	// Three hops, the synthesis, the completeness check that re-reads the
	// synthesis before it is accepted, and the Day 4 citation pass.
	if provider.callCount() != 6 {
		t.Errorf("provider calls = %d, want 6 (three hops, the synthesis, the completeness check, the citation pass)",
			provider.callCount())
	}

	// The answer synthesizes all three observations: the ticket, the vendor
	// name that exists only in Notion, and the cause and dates that exist only
	// in email.
	run := loadRun(t, pool, seeded.runID)
	if run.status != "completed" {
		t.Fatalf("status = %q, want completed", run.status)
	}
	if run.answer == nil {
		t.Fatal("answer is null on a completed run")
	}
	for _, fact := range []string{
		"ATLAS-101",    // Jira
		"Nordwind",     // Notion
		"PCI re-audit", // Gmail: the root cause that exists nowhere else
		"2026-07-15",   // Gmail: the revised sandbox date
		"2026-08-14",   // the effect on the launch date
	} {
		if !strings.Contains(*run.answer, fact) {
			t.Errorf("answer does not carry %q, so it does not synthesize all three sources:\n%s",
				fact, *run.answer)
		}
	}

	// The transcript records three tool calls, in source order, each with its
	// observation and its evidence — that is what Day 4 cites and Day 6 replays.
	events := loadEvents(t, pool, seeded.runID)
	want := []string{
		agent.EventRunStarted,
		agent.EventLLMCall,
		agent.EventToolCallStarted,
		agent.EventToolCallFinished,
		agent.EventLLMCall,
		agent.EventToolCallStarted,
		agent.EventToolCallFinished,
		agent.EventLLMCall,
		agent.EventToolCallStarted,
		agent.EventToolCallFinished,
		agent.EventLLMCall,
		// The completeness check is a second generation with no tool calls,
		// between the draft answer and the accepted one.
		agent.EventLLMCall,
		// The citation pass: a third tool-free generation, then its outcome.
		agent.EventLLMCall,
		agent.EventCitations,
		agent.EventAnswer,
		agent.EventRunFinished,
	}
	if got := eventTypes(events); !equalStrings(got, want) {
		t.Errorf("event types = %v,\nwant %v", got, want)
	}

	started := eventsOfType(events, agent.EventToolCallStarted)
	if len(started) != 3 {
		t.Fatalf("tool_call_started events = %d, want 3", len(started))
	}
	wantOrder := []string{"jira_search_issues", "notion_get_page", "gmail_get_message"}
	for i, event := range started {
		payload := decodePayload(t, event)
		if payload["tool"] != wantOrder[i] {
			t.Errorf("tool_call_started[%d].tool = %v, want %s", i, payload["tool"], wantOrder[i])
		}
	}

	// Evidence covers all three sources: the acceptance criterion is a run
	// whose events show tool calls to ≥2 of the 3 sources, and a citation can
	// only be built from evidence that was actually recorded.
	finished := eventsOfType(events, agent.EventToolCallFinished)
	if len(finished) != 3 {
		t.Fatalf("tool_call_finished events = %d, want 3", len(finished))
	}
	var sources []string
	for _, event := range finished {
		payload := decodePayload(t, event)
		if payload["status"] != "ok" {
			t.Errorf("tool_call_finished.status = %v, want ok", payload["status"])
		}
		items, ok := payload["evidence"].([]any)
		if !ok || len(items) != 1 {
			t.Fatalf("tool_call_finished.evidence = %v, want one item", payload["evidence"])
		}
		item, _ := items[0].(map[string]any)
		source, _ := item["source"].(string)
		sources = append(sources, source)
		if url, _ := item["url"].(string); url == "" {
			t.Errorf("evidence for source %q has no URL; a citation cannot link through", source)
		}
	}
	sort.Strings(sources)
	if !equalStrings(sources, []string{"gmail", "jira", "notion"}) {
		t.Errorf("evidence sources = %v, want one from each of jira, notion and gmail", sources)
	}

	// Token accounting sums every hop, which is what a per-run cost figure is
	// built from.
	if run.inputTokens == nil || *run.inputTokens != 1000 {
		t.Errorf("input_tokens = %v, want 1000 (100+200+300+400)", run.inputTokens)
	}
	if run.outputTokens == nil || *run.outputTokens != 100 {
		t.Errorf("output_tokens = %v, want 100 (10+20+30+40)", run.outputTokens)
	}

	// The synthesized answer joins the conversation.
	msgs := messagesOf(t, pool, seeded.conversationID)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (question + answer)", len(msgs))
	}
	if msgs[1].role != "assistant" || msgs[1].content != finalAnswer {
		t.Errorf("last message = %+v, want the synthesized answer", msgs[1])
	}
}

// A source that answers nothing must not end the investigation: REQ-3.4's
// prompt rule is "try a second source when the first only partially answers",
// and the loop has to keep going for that to be possible.
func TestRunContinuesToASecondSourceAfterAnEmptyFirstOne(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "who is the payments vendor for Atlas?")

	jiraTool := hopTool("jira_search_issues", "jql",
		"No issues matched the JQL. Note: this does not establish that no such work exists.",
		tools.EvidenceItem{Source: "jira", ExternalID: "search", Title: "no matches", URL: "https://vantagelabs.atlassian.net/issues"})
	notionTool := hopTool("notion_search", "query",
		"1 Notion page(s) matching \"Atlas plan\":\nAtlas Q2 Plan — id: 3c34fab1-2b9b-80d6-8cfb-c64d5b8cebe0; last edited: 2026-06-18",
		tools.EvidenceItem{
			Source:     "notion",
			ExternalID: "3c34fab1-2b9b-80d6-8cfb-c64d5b8cebe0",
			Title:      "Atlas Q2 Plan",
			URL:        "https://www.notion.so/Atlas-Q2-Plan-3c34fab12b9b80d68cfbc64d5b8cebe0",
		})

	provider := newFakeProvider(t,
		providerStep{
			kind:      toolsCall,
			toolCalls: []llm.ToolCall{toolCall("call_1", "jira_search_issues", `{"jql":"text ~ vendor"}`)},
		},
		providerStep{
			kind:      toolsCall,
			toolCalls: []llm.ToolCall{toolCall("call_2", "notion_search", `{"query":"Atlas plan"}`)},
			assert: func(t *testing.T, call recordedCall) {
				t.Helper()
				if !containsObservation(call.messages, "No issues matched") {
					t.Error("the empty Jira observation was not carried into the next turn")
				}
			},
		},
		providerStep{
			kind: toolsCall,
			text: "Jira does not name a vendor; the Atlas Q2 Plan in Notion is the page to read next.",
		},
	)

	o := newOrchestrator(t, pool, provider, newRegistry(t, jiraTool, notionTool), orchestratorOptions{})
	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if n := jiraTool.executions(); n != 1 {
		t.Errorf("jira executions = %d, want 1", n)
	}
	if n := notionTool.executions(); n != 1 {
		t.Errorf("notion executions = %d, want 1 (an empty first source must not end the run)", n)
	}
	if run := loadRun(t, pool, seeded.runID); run.status != "completed" {
		t.Errorf("status = %q, want completed", run.status)
	}
}

// containsObservation reports whether any tool observation in the transcript
// carries the given text.
func containsObservation(messages []llm.Message, want string) bool {
	for _, m := range messages {
		if m.Role == llm.RoleTool && strings.Contains(m.Content, want) {
			return true
		}
	}
	return false
}

// countRole counts messages with the given role.
func countRole(messages []llm.Message, role llm.Role) int {
	n := 0
	for _, m := range messages {
		if m.Role == role {
			n++
		}
	}
	return n
}
