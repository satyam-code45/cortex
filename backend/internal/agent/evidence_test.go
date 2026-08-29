package agent_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/llm"
	"cortex/internal/tools"
)

// REQ-4.2 — evidence persistence, with A1's per-run numbering and A2's per-run
// dedupe.
//
// The numbers are the citation markers, so they have to be assigned once per
// document per run. Two tool calls that both surface ATLAS-1 must produce one
// number: two would let the model cite the same document as [1] in one sentence
// and [4] in the next, and a reader following the markers would think there were
// two sources behind the claim.

// evidenceRow is one persisted evidence item.
type evidenceRow struct {
	seq        int32
	source     string
	externalID string
	title      *string
	url        *string
	toolCallID *uuid.UUID
}

func loadEvidence(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) []evidenceRow {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT seq, source, external_id, title, url, tool_call_id
		 FROM evidence WHERE agent_run_id = $1 ORDER BY seq`, runID)
	if err != nil {
		t.Fatalf("query evidence: %v", err)
	}
	defer rows.Close()

	var out []evidenceRow
	for rows.Next() {
		var row evidenceRow
		if err := rows.Scan(&row.seq, &row.source, &row.externalID,
			&row.title, &row.url, &row.toolCallID); err != nil {
			t.Fatalf("scan evidence: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate evidence: %v", err)
	}
	return out
}

// toolCallRow is one persisted tool call.
type toolCallRow struct {
	id            uuid.UUID
	seq           int32
	name          string
	evidenceCount int32
	status        string
	latencyMS     *int32
	summary       *string
}

func loadToolCalls(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) []toolCallRow {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT id, seq, tool_name, evidence_count, status, latency_ms, result_summary
		 FROM tool_calls WHERE agent_run_id = $1 ORDER BY seq`, runID)
	if err != nil {
		t.Fatalf("query tool_calls: %v", err)
	}
	defer rows.Close()

	var out []toolCallRow
	for rows.Next() {
		var row toolCallRow
		if err := rows.Scan(&row.id, &row.seq, &row.name, &row.evidenceCount,
			&row.status, &row.latencyMS, &row.summary); err != nil {
			t.Fatalf("scan tool_call: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tool_calls: %v", err)
	}
	return out
}

func TestEvidenceIsNumberedAndDeduplicatedPerRun(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "what is blocking Atlas?")

	shared := tools.EvidenceItem{
		Source: "jira", ExternalID: "ATLAS-1",
		Title: "Sandbox down", URL: "https://example.atlassian.net/browse/ATLAS-1",
	}

	search := &fakeTool{
		name: "jira_search_issues",
		run: func(_ int, _ json.RawMessage) (tools.Result, error) {
			return tools.Result{
				Content: "ATLAS-1 sandbox down; ATLAS-2 vendor slipped",
				Evidence: []tools.EvidenceItem{
					shared,
					{Source: "jira", ExternalID: "ATLAS-2", Title: "Vendor slipped",
						URL: "https://example.atlassian.net/browse/ATLAS-2"},
				},
			}, nil
		},
	}
	// A different tool that surfaces the same issue again, plus a new document.
	detail := &fakeTool{
		name: "jira_get_issue",
		run: func(_ int, _ json.RawMessage) (tools.Result, error) {
			return tools.Result{
				Content: "ATLAS-1 in detail, linked to the Q2 plan",
				Evidence: []tools.EvidenceItem{
					shared,
					{Source: "notion", ExternalID: "page-atlas-plan", Title: "Atlas Q2 Plan",
						URL: "https://www.notion.so/page-atlas-plan"},
				},
			}, nil
		},
	}

	provider := newFakeProvider(t,
		providerStep{
			kind:      toolsCall,
			toolCalls: []llm.ToolCall{toolCall("call_1", "jira_search_issues", `{"q":"atlas"}`)},
		},
		providerStep{
			kind:      toolsCall,
			toolCalls: []llm.ToolCall{toolCall("call_2", "jira_get_issue", `{"q":"ATLAS-1"}`)},
		},
		providerStep{kind: toolsCall, text: "ATLAS-1 blocks the Q2 plan."},
		providerStep{kind: toolsCall, text: "ATLAS-1 blocks the Q2 plan."},
	)

	o := newOrchestrator(t, pool, provider, newRegistry(t, search, detail), orchestratorOptions{})
	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Four evidence items were handed over; three distinct documents were
	// touched, so three rows numbered 1, 2, 3.
	evidence := loadEvidence(t, pool, seeded.runID)
	want := []struct {
		seq        int32
		source     string
		externalID string
	}{
		{1, "jira", "ATLAS-1"},
		{2, "jira", "ATLAS-2"},
		{3, "notion", "page-atlas-plan"},
	}
	if len(evidence) != len(want) {
		t.Fatalf("evidence rows = %d, want %d (the repeated document must reuse its number)\n%+v",
			len(evidence), len(want), evidence)
	}
	for i, w := range want {
		got := evidence[i]
		if got.seq != w.seq {
			t.Errorf("evidence[%d].seq = %d, want %d", i, got.seq, w.seq)
		}
		if got.source != w.source || got.externalID != w.externalID {
			t.Errorf("evidence[%d] = %s/%s, want %s/%s", i, got.source, got.externalID, w.source, w.externalID)
		}
		if got.url == nil || *got.url == "" {
			t.Errorf("evidence[%d] has no URL", i)
		}
		if got.title == nil || *got.title == "" {
			t.Errorf("evidence[%d] has no title", i)
		}
	}

	calls := loadToolCalls(t, pool, seeded.runID)
	if len(calls) != 2 {
		t.Fatalf("tool_calls = %d, want 2\n%+v", len(calls), calls)
	}
	for i, call := range calls {
		if call.seq != int32(i+1) {
			t.Errorf("tool_calls[%d].seq = %d, want %d", i, call.seq, i+1)
		}
		if call.status != "ok" {
			t.Errorf("tool_calls[%d].status = %q, want ok", i, call.status)
		}
		if call.latencyMS == nil {
			t.Errorf("tool_calls[%d] has no latency", i)
		}
		if call.summary == nil || *call.summary == "" {
			t.Errorf("tool_calls[%d] has no result summary", i)
		}
		// The count is what the tool handed over, not how many rows were new:
		// a trace claiming zero evidence for a call that gave the model two
		// citable documents would be a lie.
		if call.evidenceCount != 2 {
			t.Errorf("tool_calls[%d].evidence_count = %d, want 2", i, call.evidenceCount)
		}
	}

	// A2: tool_call_id records the call that FIRST surfaced the item, so the
	// repeated document still points at the search, not at the later lookup.
	if evidence[0].toolCallID == nil || *evidence[0].toolCallID != calls[0].id {
		t.Errorf("the repeated document is attributed to %v, want the first tool call %s",
			evidence[0].toolCallID, calls[0].id)
	}
	if evidence[2].toolCallID == nil || *evidence[2].toolCallID != calls[1].id {
		t.Errorf("the notion page is attributed to %v, want the second tool call %s",
			evidence[2].toolCallID, calls[1].id)
	}
}

// A tool that fails still gets a row: "which tools fail most" is one of the
// questions tool_calls exists to answer, and a failed call with no row makes the
// trace look like it never happened.
func TestFailedToolCallIsRecordedWithItsError(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "what is blocking Atlas?")

	broken := &fakeTool{
		name: "jira_search_issues",
		run: func(_ int, _ json.RawMessage) (tools.Result, error) {
			return tools.Result{}, context.DeadlineExceeded
		},
	}

	provider := newFakeProvider(t,
		providerStep{
			kind:      toolsCall,
			toolCalls: []llm.ToolCall{toolCall("call_1", "jira_search_issues", `{"q":"atlas"}`)},
		},
		providerStep{kind: toolsCall, text: "I could not reach Jira."},
		providerStep{kind: toolsCall, text: "I could not reach Jira."},
	)

	o := newOrchestrator(t, pool, provider, newRegistry(t, broken), orchestratorOptions{})
	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	calls := loadToolCalls(t, pool, seeded.runID)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %d, want 1\n%+v", len(calls), calls)
	}
	if calls[0].status != "error" {
		t.Errorf("status = %q, want error", calls[0].status)
	}
	if calls[0].evidenceCount != 0 {
		t.Errorf("evidence_count = %d, want 0 on a failed call", calls[0].evidenceCount)
	}
	var errText *string
	if err := pool.QueryRow(context.Background(),
		`SELECT error FROM tool_calls WHERE agent_run_id = $1`, seeded.runID).Scan(&errText); err != nil {
		t.Fatalf("load tool_call error: %v", err)
	}
	if errText == nil || *errText == "" {
		t.Error("a failed tool call recorded no error text")
	}
	if got := loadEvidence(t, pool, seeded.runID); len(got) != 0 {
		t.Errorf("evidence = %+v, want none from a failed call", got)
	}
}
