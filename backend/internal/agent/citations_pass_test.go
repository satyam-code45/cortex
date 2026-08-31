package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/agent"
	"cortex/internal/llm"
	"cortex/internal/tools"
)

// The citation pass, at the level the run sees it.
//
// The pure validator is covered in citations_internal_test.go. This file covers
// the rest of the contract: the pass runs after the loop, its output lands on
// agent_runs.answer and in the conversation, and — the part that matters most in
// production — it cannot fail the run. By the time it runs, the investigation
// has already made a dozen paid calls; losing that because a citation call
// returned malformed JSON would be a spectacularly bad trade, so every failure
// path stores the uncited draft with a citations event recording why.

const draftAnswer = "The payments sandbox is down and the vendor slipped."

// citationRow is one persisted citation, joined to its evidence.
type citationRow struct {
	marker      string
	claim       *string
	evidenceSeq int32
	source      string
	externalID  string
	url         *string
}

func loadCitations(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) []citationRow {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT c.marker, c.claim_text, e.seq, e.source, e.external_id, e.url
		 FROM citations c JOIN evidence e ON e.id = c.evidence_id
		 WHERE c.agent_run_id = $1
		 ORDER BY c.marker`, runID)
	if err != nil {
		t.Fatalf("query citations: %v", err)
	}
	defer rows.Close()

	var out []citationRow
	for rows.Next() {
		var row citationRow
		if err := rows.Scan(&row.marker, &row.claim, &row.evidenceSeq,
			&row.source, &row.externalID, &row.url); err != nil {
			t.Fatalf("scan citation: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate citations: %v", err)
	}
	return out
}

// twoEvidenceTool hands the run two citable documents.
func twoEvidenceTool() *fakeTool {
	return &fakeTool{
		name: "jira_search_issues",
		run: func(_ int, _ json.RawMessage) (tools.Result, error) {
			return tools.Result{
				Content: "ATLAS-1 sandbox down; ATLAS-2 vendor slipped",
				Evidence: []tools.EvidenceItem{
					{
						Source: "jira", ExternalID: "ATLAS-1",
						Title: "Sandbox down", URL: "https://example.atlassian.net/browse/ATLAS-1",
					},
					{
						Source: "jira", ExternalID: "ATLAS-2",
						Title: "Vendor slipped", URL: "https://example.atlassian.net/browse/ATLAS-2",
					},
				},
			}, nil
		},
	}
}

// investigationSteps are the two loop turns every test here shares: one tool
// call, then the draft answer.
func investigationSteps() []providerStep {
	return []providerStep{
		{
			kind:      toolsCall,
			toolCalls: []llm.ToolCall{toolCall("call_1", "jira_search_issues", `{"q":"atlas"}`)},
		},
		{kind: toolsCall, text: draftAnswer},
		{kind: toolsCall, text: draftAnswer}, // the completeness check keeps it
	}
}

// The hallucinated-citation guard, end to end: only the citation that resolves
// to real evidence survives into the citations table, and the stored answer
// carries the renumbered marker.
func TestCitationPassDropsHallucinatedEvidenceEndToEnd(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "what is blocking Atlas?")

	steps := append(investigationSteps(), providerStep{
		kind: structuredCall,
		text: `{"answer_markdown":"The payments sandbox is down [4] and the vendor slipped [2].",
		        "citations":[{"marker":"[4]","evidence_id":9,"claim":"the payments sandbox is down"},
		                     {"marker":"[2]","evidence_id":2,"claim":"the vendor slipped"}]}`,
	})

	provider := newFakeProvider(t, steps...)
	o := newOrchestrator(t, pool, provider, newRegistry(t, twoEvidenceTool()), orchestratorOptions{})
	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Evidence 9 does not exist in a run that gathered two items, so its marker
	// is removed and the survivor is renumbered to [1].
	wantAnswer := "The payments sandbox is down and the vendor slipped [1]."
	run := loadRun(t, pool, seeded.runID)
	if run.status != "completed" {
		t.Fatalf("status = %q, want completed", run.status)
	}
	if run.answer == nil || *run.answer != wantAnswer {
		t.Errorf("answer = %v\nwant %q", run.answer, wantAnswer)
	}

	citations := loadCitations(t, pool, seeded.runID)
	if len(citations) != 1 {
		t.Fatalf("citations = %+v, want only the one backed by real evidence", citations)
	}
	got := citations[0]
	if got.marker != "[1]" {
		t.Errorf("marker = %q, want the renumbered %q", got.marker, "[1]")
	}
	if got.evidenceSeq != 2 || got.externalID != "ATLAS-2" {
		t.Errorf("citation resolves to evidence %d (%s), want seq 2 (ATLAS-2)", got.evidenceSeq, got.externalID)
	}
	if got.url == nil || *got.url == "" {
		t.Error("the cited evidence has no URL; the marker resolves to nothing a reader can open")
	}

	// The event records the drop, which is how a trace shows the guard firing.
	events := loadEvents(t, pool, seeded.runID)
	citationEvents := eventsOfType(events, agent.EventCitations)
	if len(citationEvents) != 1 {
		t.Fatalf("citations events = %d, want 1", len(citationEvents))
	}
	payload := decodePayload(t, citationEvents[0])
	if dropped, _ := payload["dropped"].(float64); dropped != 1 {
		t.Errorf("citations event dropped = %v, want 1", payload["dropped"])
	}
	if count, _ := payload["evidence_count"].(float64); count != 2 {
		t.Errorf("citations event evidence_count = %v, want 2", payload["evidence_count"])
	}
	if skipped, ok := payload["skipped"]; ok && skipped != "" {
		t.Errorf("citations event skipped = %v on a pass that ran normally", skipped)
	}
}

// A failed or unparseable citation pass must not fail the run. The
// draft is stored uncited and the event says why.
func TestCitationPassFailureStoresTheUncitedDraft(t *testing.T) {
	tests := []struct {
		name string
		step providerStep
	}{
		{
			name: "the citation call fails",
			step: providerStep{kind: structuredCall, err: errors.New("upstream is down")},
		},
		{
			name: "the citation call returns unparseable output",
			step: providerStep{kind: structuredCall, text: "I am afraid I cannot do that."},
		},
		{
			name: "the citation call returns an empty answer",
			step: providerStep{kind: structuredCall, text: `{"answer_markdown":"","citations":[]}`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := testPool(t)
			seeded := seedRun(t, pool, "what is blocking Atlas?")

			provider := newFakeProvider(t, append(investigationSteps(), tt.step)...)
			o := newOrchestrator(t, pool, provider, newRegistry(t, twoEvidenceTool()), orchestratorOptions{})
			if err := o.Run(context.Background(), seeded.runID); err != nil {
				t.Fatalf("Run: %v", err)
			}

			run := loadRun(t, pool, seeded.runID)
			if run.status != "completed" {
				t.Fatalf("status = %q, want completed: a citation failure must not fail the run", run.status)
			}
			if run.answer == nil || *run.answer != draftAnswer {
				t.Errorf("answer = %v, want the uncited draft %q", run.answer, draftAnswer)
			}
			if got := loadCitations(t, pool, seeded.runID); len(got) != 0 {
				t.Errorf("citations = %+v, want none", got)
			}
			// The evidence still stands: only the citation mapping was lost.
			if got := queryEvidenceCount(t, pool, seeded.runID); got != 2 {
				t.Errorf("evidence rows = %d, want 2 (the tool call still happened)", got)
			}

			events := loadEvents(t, pool, seeded.runID)
			citationEvents := eventsOfType(events, agent.EventCitations)
			if len(citationEvents) != 1 {
				t.Fatalf("citations events = %d, want 1 recording the failure", len(citationEvents))
			}
			payload := decodePayload(t, citationEvents[0])
			skipped, _ := payload["skipped"].(string)
			if skipped == "" {
				t.Errorf("citations event does not say why nothing was cited: %v", payload)
			}

			// The assistant message must match the stored answer, or the reader
			// and the trace disagree.
			msgs := messagesOf(t, pool, seeded.conversationID)
			if len(msgs) < 2 || msgs[len(msgs)-1].content != draftAnswer {
				t.Errorf("assistant message = %+v, want the uncited draft", msgs[len(msgs)-1])
			}
		})
	}
}

// A run that gathered no evidence cites nothing, and that is a legitimate
// outcome rather than a failure — but the transcript has to say so.
func TestCitationPassIsSkippedWhenThereIsNoEvidence(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "hello")

	provider := newFakeProvider(t,
		providerStep{kind: toolsCall, text: draftAnswer},
		providerStep{kind: toolsCall, text: draftAnswer},
	)
	o := newOrchestrator(t, pool, provider, newRegistry(t, twoEvidenceTool()), orchestratorOptions{})
	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// No evidence means no citation call at all: the two scripted loop turns are
	// the whole run.
	if got := provider.callCount(); got != 2 {
		t.Errorf("provider calls = %d, want 2 (no citation pass without evidence)", got)
	}
	run := loadRun(t, pool, seeded.runID)
	if run.answer == nil || *run.answer != draftAnswer {
		t.Errorf("answer = %v, want the draft", run.answer)
	}
	events := loadEvents(t, pool, seeded.runID)
	citationEvents := eventsOfType(events, agent.EventCitations)
	if len(citationEvents) != 1 {
		t.Fatalf("citations events = %d, want 1", len(citationEvents))
	}
	payload := decodePayload(t, citationEvents[0])
	if skipped, _ := payload["skipped"].(string); skipped == "" {
		t.Errorf("citations event does not record why the pass was skipped: %v", payload)
	}
}

func queryEvidenceCount(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM evidence WHERE agent_run_id = $1`, runID).Scan(&n); err != nil {
		t.Fatalf("count evidence: %v", err)
	}
	return n
}
