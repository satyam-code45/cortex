package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/agent"
	"cortex/internal/api"
	"cortex/internal/llm"
	"cortex/internal/tools"
)

// TEST-4.3 — trace assembly, end to end.
//
// A scripted fake-provider run is driven to completion through the real
// orchestrator, and then GET /api/runs/{id}/trace (REQ-4.4) is asked for the
// whole story. What the endpoint returns has to be a complete, interleaved
// timeline that matches run_events row for row, with the evidence and the
// citations resolved — a marker in the answer has to lead, through the trace, to
// a source with a real URL. That is the acceptance criterion for the day, and it
// is also what the Day 5 trace panel is built on.
//
// This lives in the agent test package because the scripted provider does: the
// run has to be real for the trace to be worth asserting on.

// ---------------------------------------------------------------------------
// the wire contract
// ---------------------------------------------------------------------------

// traceBody mirrors the documented JSON of GET /api/runs/{id}/trace. It is
// declared here, rather than decoding into agent.Trace, so the assertions are
// against the wire format the frontend consumes.
type traceBody struct {
	RunID          string  `json:"run_id"`
	ConversationID string  `json:"conversation_id"`
	Query          string  `json:"query"`
	Status         string  `json:"status"`
	Model          *string `json:"model"`
	Answer         *string `json:"answer"`
	Error          *string `json:"error"`

	CreatedAt  *time.Time `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at"`

	Totals struct {
		Iterations   int   `json:"iterations"`
		ToolCalls    int   `json:"tool_calls"`
		LLMCalls     int   `json:"llm_calls"`
		InputTokens  int   `json:"input_tokens"`
		OutputTokens int   `json:"output_tokens"`
		LatencyMS    int64 `json:"latency_ms"`
	} `json:"totals"`

	Timeline []struct {
		Seq     int32           `json:"seq"`
		Type    string          `json:"type"`
		At      *time.Time      `json:"at"`
		Payload json.RawMessage `json:"payload"`
	} `json:"timeline"`

	ToolCalls []struct {
		Seq           int32           `json:"seq"`
		Tool          string          `json:"tool"`
		Arguments     json.RawMessage `json:"arguments"`
		ResultSummary *string         `json:"result_summary"`
		EvidenceCount int32           `json:"evidence_count"`
		LatencyMS     *int32          `json:"latency_ms"`
		Status        string          `json:"status"`
		Error         *string         `json:"error"`
		At            *time.Time      `json:"at"`
	} `json:"tool_calls"`

	Evidence []struct {
		ID         string     `json:"id"`
		Seq        int32      `json:"seq"`
		Source     string     `json:"source"`
		ExternalID string     `json:"external_id"`
		Title      *string    `json:"title"`
		URL        *string    `json:"url"`
		Snippet    *string    `json:"snippet"`
		Timestamp  *time.Time `json:"source_timestamp"`
		ToolCallID *string    `json:"tool_call_id"`
	} `json:"evidence"`

	Citations []struct {
		Marker      string  `json:"marker"`
		Claim       *string `json:"claim"`
		EvidenceID  string  `json:"evidence_id"`
		EvidenceSeq int32   `json:"evidence_seq"`
		Source      string  `json:"source"`
		ExternalID  string  `json:"external_id"`
		Title       *string `json:"title"`
		URL         *string `json:"url"`
		Snippet     *string `json:"snippet"`
	} `json:"citations"`
}

// traceTestAPIToken authenticates trace requests; the bearer path acts as the
// router's BearerEmail, which is how these tests choose whose runs they see.
const traceTestAPIToken = "trace-test-token"

// traceRouter builds the real router against the test pool, acting as the user
// that owns the run.
func traceRouter(pool *pgxpool.Pool, email string) http.Handler {
	return api.NewRouter(api.Deps{
		DB:          pool,
		Model:       testModel,
		APIToken:    traceTestAPIToken,
		BearerEmail: email,
		Logger:      discardLogger(),
	})
}

func getTrace(t *testing.T, h http.Handler, runID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/runs/"+runID+"/trace", nil)
	// The host check rejects anything a real browser would not have dialled.
	req.Host = "localhost:8080"
	req.Header.Set("Authorization", "Bearer "+traceTestAPIToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// userEmail reads back the email seedRun generated, so the router can be the
// owner of the run.
func userEmail(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) string {
	t.Helper()
	var email string
	if err := pool.QueryRow(context.Background(),
		`SELECT email FROM users WHERE id = $1`, userID).Scan(&email); err != nil {
		t.Fatalf("load user email: %v", err)
	}
	return email
}

// ---------------------------------------------------------------------------
// the run under trace
// ---------------------------------------------------------------------------

// citationResponse is the structured output of the citation pass. The markers
// are deliberately out of order and not 1-based: A3 says the validator
// renumbers by first appearance, and the trace has to show the renumbered form.
const citationResponse = `{
  "answer_markdown": "Atlas Q2 is behind: the payments sandbox is down [3] and the vendor slipped [1].",
  "citations": [
    {"marker": "[3]", "evidence_id": 3, "claim": "the payments sandbox is down"},
    {"marker": "[1]", "evidence_id": 1, "claim": "the vendor slipped"}
  ]
}`

const citedAnswer = "Atlas Q2 is behind: the payments sandbox is down [1] and the vendor slipped [2]."

func TestTraceEndpointReturnsTheWholeRun(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "why is Atlas Q2 behind?")

	sourceTime := time.Date(2026, 5, 4, 9, 30, 0, 0, time.UTC)

	jira := &fakeTool{
		name: "jira_search_issues",
		run: func(_ int, _ json.RawMessage) (tools.Result, error) {
			return tools.Result{
				Content: "ATLAS-1 [Blocked] vendor slipped; ATLAS-2 [In Progress] migration",
				Evidence: []tools.EvidenceItem{
					{
						Source: "jira", ExternalID: "ATLAS-1",
						Title:   "Vendor slipped the integration date",
						URL:     "https://example.atlassian.net/browse/ATLAS-1",
						Snippet: "The vendor moved delivery to July.",
					},
					{
						Source: "jira", ExternalID: "ATLAS-2",
						Title: "Migration in progress",
						URL:   "https://example.atlassian.net/browse/ATLAS-2",
					},
				},
			}, nil
		},
	}
	notion := &fakeTool{
		name: "notion_search_pages",
		run: func(_ int, _ json.RawMessage) (tools.Result, error) {
			return tools.Result{
				Content: "Atlas Q2 plan: payments sandbox is down since April.",
				Evidence: []tools.EvidenceItem{{
					Source: "notion", ExternalID: "page-atlas-plan",
					Title:     "Atlas Q2 Plan",
					URL:       "https://www.notion.so/page-atlas-plan",
					Snippet:   "Payments sandbox is down.",
					Timestamp: &sourceTime,
				}},
			}, nil
		},
	}

	provider := newFakeProvider(t,
		providerStep{
			kind:         toolsCall,
			toolCalls:    []llm.ToolCall{toolCall("call_1", "jira_search_issues", `{"q":"atlas blocked"}`)},
			inputTokens:  100,
			outputTokens: 10,
		},
		providerStep{
			kind:         toolsCall,
			toolCalls:    []llm.ToolCall{toolCall("call_2", "notion_search_pages", `{"q":"atlas q2 plan"}`)},
			inputTokens:  120,
			outputTokens: 12,
		},
		providerStep{
			kind:         toolsCall,
			text:         "Atlas Q2 is behind: the payments sandbox is down and the vendor slipped.",
			inputTokens:  140,
			outputTokens: 14,
		},
		providerStep{
			// The completeness check re-reads the draft and keeps it.
			kind:         toolsCall,
			text:         "Atlas Q2 is behind: the payments sandbox is down and the vendor slipped.",
			inputTokens:  160,
			outputTokens: 16,
			assert: func(t *testing.T, call recordedCall) {
				t.Helper()
				if _, ok := answerUnderReview(call); !ok {
					t.Error("call #4 was not the completeness check")
				}
			},
		},
		providerStep{
			kind:         structuredCall,
			text:         citationResponse,
			inputTokens:  180,
			outputTokens: 18,
			assert: func(t *testing.T, call recordedCall) {
				t.Helper()
				if call.schema == nil {
					t.Fatal("the citation pass did not ask for structured output")
				}
				// REQ-4.3: the pass sees the numbered evidence list, which is
				// the only handle the model has on a citation.
				last := call.messages[len(call.messages)-1].Content
				for _, want := range []string{"[1] jira ATLAS-1", "[3] notion page-atlas-plan"} {
					if !strings.Contains(last, want) {
						t.Errorf("citation prompt is missing the numbered evidence line %q\n%s", want, last)
					}
				}
			},
		},
	)

	o := newOrchestrator(t, pool, provider, newRegistry(t, jira, notion), orchestratorOptions{})
	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The answer that was stored is the rewritten one (REQ-4.3): markers
	// included, renumbered 1..N.
	run := loadRun(t, pool, seeded.runID)
	if run.status != "completed" {
		t.Fatalf("status = %q, want completed", run.status)
	}
	if run.answer == nil || *run.answer != citedAnswer {
		t.Fatalf("stored answer = %v\nwant %q", run.answer, citedAnswer)
	}
	msgs := messagesOf(t, pool, seeded.conversationID)
	if len(msgs) != 2 || msgs[1].content != citedAnswer {
		t.Errorf("assistant message = %+v, want the cited answer", msgs[len(msgs)-1])
	}

	// -----------------------------------------------------------------------
	// the trace
	// -----------------------------------------------------------------------

	h := traceRouter(pool, userEmail(t, pool, seeded.userID))
	rec := getTrace(t, h, seeded.runID.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var got traceBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode trace %q: %v", rec.Body.String(), err)
	}

	if got.RunID != seeded.runID.String() {
		t.Errorf("run_id = %q, want %q", got.RunID, seeded.runID)
	}
	if got.ConversationID != seeded.conversationID.String() {
		t.Errorf("conversation_id = %q, want %q", got.ConversationID, seeded.conversationID)
	}
	if got.Query != seeded.query {
		t.Errorf("query = %q, want %q", got.Query, seeded.query)
	}
	if got.Status != "completed" {
		t.Errorf("status = %q, want completed", got.Status)
	}
	if got.Model == nil || *got.Model != testModel {
		t.Errorf("model = %v, want %q", got.Model, testModel)
	}
	if got.Answer == nil || *got.Answer != citedAnswer {
		t.Errorf("answer = %v, want the cited answer", got.Answer)
	}
	if got.Error != nil {
		t.Errorf("error = %q on a completed run, want null", *got.Error)
	}
	if got.CreatedAt == nil || got.FinishedAt == nil {
		t.Errorf("created_at = %v, finished_at = %v; both must be set on a completed run",
			got.CreatedAt, got.FinishedAt)
	}

	// -----------------------------------------------------------------------
	// timeline: complete, in seq order, matching run_events exactly
	// -----------------------------------------------------------------------

	events := loadEvents(t, pool, seeded.runID)
	wantTypes := []string{
		agent.EventRunStarted,
		agent.EventLLMCall,
		agent.EventToolCallStarted,
		agent.EventToolCallFinished,
		agent.EventLLMCall,
		agent.EventToolCallStarted,
		agent.EventToolCallFinished,
		agent.EventLLMCall, // the draft answer
		agent.EventLLMCall, // the completeness check
		agent.EventLLMCall, // the citation pass
		agent.EventCitations,
		agent.EventAnswer,
		agent.EventRunFinished,
	}
	if stored := eventTypes(events); !equalStrings(stored, wantTypes) {
		t.Fatalf("run_events types = %v, want %v", stored, wantTypes)
	}
	if len(got.Timeline) != len(events) {
		t.Fatalf("timeline has %d entries, want %d (one per run_events row)\ngot %v",
			len(got.Timeline), len(events), traceTypes(got))
	}
	for i, entry := range got.Timeline {
		stored := events[i]
		if entry.Seq != stored.Seq {
			t.Errorf("timeline[%d].seq = %d, want %d", i, entry.Seq, stored.Seq)
		}
		if entry.Type != stored.Type {
			t.Errorf("timeline[%d].type = %q, want %q", i, entry.Type, stored.Type)
		}
		if entry.At == nil {
			t.Errorf("timeline[%d] (%s) has no timestamp", i, entry.Type)
		}
		if !sameJSON(t, entry.Payload, stored.Payload) {
			t.Errorf("timeline[%d] (%s) payload differs from the stored event\ngot  %s\nwant %s",
				i, entry.Type, entry.Payload, stored.Payload)
		}
	}
	// The interleaving is the point: the tool entries sit between the model
	// turns that asked for them, in the order they happened.
	if timelineTypes := traceTypes(got); !equalStrings(timelineTypes, wantTypes) {
		t.Errorf("timeline order = %v, want %v", timelineTypes, wantTypes)
	}

	// REQ-4.4 asks the timeline to carry arguments, latency and status per tool
	// call. They live in the payloads the transcript already records.
	for i, entry := range got.Timeline {
		payload := map[string]any{}
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			t.Fatalf("timeline[%d] payload is not an object: %v", i, err)
		}
		switch entry.Type {
		case agent.EventToolCallStarted:
			if _, ok := payload["arguments"]; !ok {
				t.Errorf("timeline[%d] (%s) carries no arguments", i, entry.Type)
			}
		case agent.EventToolCallFinished:
			if payload["status"] != "ok" {
				t.Errorf("timeline[%d] status = %v, want ok", i, payload["status"])
			}
			if _, ok := payload["latency_ms"]; !ok {
				t.Errorf("timeline[%d] (%s) carries no latency", i, entry.Type)
			}
		}
	}

	// -----------------------------------------------------------------------
	// tool calls: the normalized projection (REQ-4.2)
	// -----------------------------------------------------------------------

	if len(got.ToolCalls) != 2 {
		t.Fatalf("tool_calls = %d, want 2\n%+v", len(got.ToolCalls), got.ToolCalls)
	}
	wantCalls := []struct {
		seq      int32
		tool     string
		argument string
		evidence int32
	}{
		{seq: 1, tool: "jira_search_issues", argument: "atlas blocked", evidence: 2},
		{seq: 2, tool: "notion_search_pages", argument: "atlas q2 plan", evidence: 1},
	}
	for i, want := range wantCalls {
		call := got.ToolCalls[i]
		if call.Seq != want.seq {
			t.Errorf("tool_calls[%d].seq = %d, want %d", i, call.Seq, want.seq)
		}
		if call.Tool != want.tool {
			t.Errorf("tool_calls[%d].tool = %q, want %q", i, call.Tool, want.tool)
		}
		var args map[string]any
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			t.Errorf("tool_calls[%d].arguments is not an object: %v", i, err)
		} else if args["q"] != want.argument {
			t.Errorf("tool_calls[%d].arguments = %v, want q=%q", i, args, want.argument)
		}
		if call.Status != "ok" {
			t.Errorf("tool_calls[%d].status = %q, want ok", i, call.Status)
		}
		if call.Error != nil {
			t.Errorf("tool_calls[%d].error = %q on a successful call, want null", i, *call.Error)
		}
		if call.LatencyMS == nil {
			t.Errorf("tool_calls[%d].latency_ms is null; REQ-4.4 requires the latency", i)
		}
		if call.EvidenceCount != want.evidence {
			t.Errorf("tool_calls[%d].evidence_count = %d, want %d", i, call.EvidenceCount, want.evidence)
		}
		if call.ResultSummary == nil || strings.TrimSpace(*call.ResultSummary) == "" {
			t.Errorf("tool_calls[%d].result_summary is empty", i)
		}
		if call.At == nil {
			t.Errorf("tool_calls[%d].at is null", i)
		}
	}

	// -----------------------------------------------------------------------
	// evidence: numbered per run, deduplicated, resolved (A1, A2)
	// -----------------------------------------------------------------------

	if len(got.Evidence) != 3 {
		t.Fatalf("evidence = %d, want 3\n%+v", len(got.Evidence), got.Evidence)
	}
	wantEvidence := []struct {
		seq        int32
		source     string
		externalID string
		title      string
		url        string
	}{
		{1, "jira", "ATLAS-1", "Vendor slipped the integration date", "https://example.atlassian.net/browse/ATLAS-1"},
		{2, "jira", "ATLAS-2", "Migration in progress", "https://example.atlassian.net/browse/ATLAS-2"},
		{3, "notion", "page-atlas-plan", "Atlas Q2 Plan", "https://www.notion.so/page-atlas-plan"},
	}
	for i, want := range wantEvidence {
		item := got.Evidence[i]
		if item.Seq != want.seq {
			t.Errorf("evidence[%d].seq = %d, want %d", i, item.Seq, want.seq)
		}
		if item.Source != want.source || item.ExternalID != want.externalID {
			t.Errorf("evidence[%d] = %s/%s, want %s/%s",
				i, item.Source, item.ExternalID, want.source, want.externalID)
		}
		if item.Title == nil || *item.Title != want.title {
			t.Errorf("evidence[%d].title = %v, want %q", i, item.Title, want.title)
		}
		// The acceptance test: every marker resolves to evidence with a real URL.
		if item.URL == nil || *item.URL != want.url {
			t.Errorf("evidence[%d].url = %v, want %q", i, item.URL, want.url)
		}
		if item.ID == "" || item.ID == uuid.Nil.String() {
			t.Errorf("evidence[%d].id = %q, want a real id", i, item.ID)
		}
		if item.ToolCallID == nil {
			t.Errorf("evidence[%d] is not linked to the tool call that produced it", i)
		}
	}
	if got.Evidence[2].Timestamp == nil || !got.Evidence[2].Timestamp.Equal(sourceTime) {
		t.Errorf("evidence[2].source_timestamp = %v, want %v", got.Evidence[2].Timestamp, sourceTime)
	}

	// -----------------------------------------------------------------------
	// citations: renumbered, and resolved to their evidence
	// -----------------------------------------------------------------------

	if len(got.Citations) != 2 {
		t.Fatalf("citations = %d, want 2\n%+v", len(got.Citations), got.Citations)
	}
	byEvidenceID := make(map[string]int32, len(got.Evidence))
	for _, item := range got.Evidence {
		byEvidenceID[item.ID] = item.Seq
	}
	wantCitations := []struct {
		marker      string
		evidenceSeq int32
		source      string
		externalID  string
		claim       string
	}{
		{"[1]", 3, "notion", "page-atlas-plan", "the payments sandbox is down"},
		{"[2]", 1, "jira", "ATLAS-1", "the vendor slipped"},
	}
	for i, want := range wantCitations {
		c := got.Citations[i]
		if c.Marker != want.marker {
			t.Errorf("citations[%d].marker = %q, want %q", i, c.Marker, want.marker)
		}
		if c.EvidenceSeq != want.evidenceSeq {
			t.Errorf("citations[%d].evidence_seq = %d, want %d", i, c.EvidenceSeq, want.evidenceSeq)
		}
		if c.Source != want.source || c.ExternalID != want.externalID {
			t.Errorf("citations[%d] resolves to %s/%s, want %s/%s",
				i, c.Source, c.ExternalID, want.source, want.externalID)
		}
		if c.Claim == nil || *c.Claim != want.claim {
			t.Errorf("citations[%d].claim = %v, want %q", i, c.Claim, want.claim)
		}
		if c.URL == nil || *c.URL == "" {
			t.Errorf("citations[%d] resolves to evidence with no URL", i)
		}
		if c.Title == nil || *c.Title == "" {
			t.Errorf("citations[%d] resolves to evidence with no title", i)
		}
		// The marker must be in the answer, and the evidence it names must be
		// one of the run's own items with the matching number.
		if got.Answer == nil || !strings.Contains(*got.Answer, c.Marker) {
			t.Errorf("citations[%d].marker %s does not appear in the answer", i, c.Marker)
		}
		seq, ok := byEvidenceID[c.EvidenceID]
		if !ok {
			t.Errorf("citations[%d].evidence_id %s is not in the trace's evidence list", i, c.EvidenceID)
		} else if seq != c.EvidenceSeq {
			t.Errorf("citations[%d].evidence_seq = %d, but evidence %s has seq %d",
				i, c.EvidenceSeq, c.EvidenceID, seq)
		}
	}

	// -----------------------------------------------------------------------
	// totals
	// -----------------------------------------------------------------------

	if got.Totals.LLMCalls != 5 {
		t.Errorf("totals.llm_calls = %d, want 5 (three loop turns, the completeness check, the citation pass)",
			got.Totals.LLMCalls)
	}
	if got.Totals.ToolCalls != 2 {
		t.Errorf("totals.tool_calls = %d, want 2", got.Totals.ToolCalls)
	}
	if got.Totals.InputTokens != 700 {
		t.Errorf("totals.input_tokens = %d, want 700 (100+120+140+160+180)", got.Totals.InputTokens)
	}
	if got.Totals.OutputTokens != 70 {
		t.Errorf("totals.output_tokens = %d, want 70 (10+12+14+16+18)", got.Totals.OutputTokens)
	}
	finished := decodePayload(t, eventsOfType(events, agent.EventRunFinished)[0])
	wantIterations := int(finished["iterations"].(float64))
	if got.Totals.Iterations != wantIterations {
		t.Errorf("totals.iterations = %d, want %d (the run_finished payload)",
			got.Totals.Iterations, wantIterations)
	}
	if run.latencyMS != nil && got.Totals.LatencyMS != int64(*run.latencyMS) {
		t.Errorf("totals.latency_ms = %d, want %d", got.Totals.LatencyMS, *run.latencyMS)
	}
}

// REQ-4.4: ownership is enforced in the query. A trace exposes far more than the
// answer does, and the run id comes straight from the caller.
func TestTraceIsScopedToTheOwner(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "whose run is this?")

	h := traceRouter(pool, "someone-else@cortex.test")
	rec := getTrace(t, h, seeded.runID.String())
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for another user's trace (body %q)", rec.Code, rec.Body.String())
	}
}

func TestTraceRejectsAMalformedRunID(t *testing.T) {
	pool := testPool(t)
	h := traceRouter(pool, "dev@cortex.local")

	rec := getTrace(t, h, "not-a-uuid")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
}

// A run that never produced a tool call still has a trace: empty lists, not
// nulls, so the frontend can render it without a special case.
func TestTraceOfAnUntouchedRunIsEmptyNotNull(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "pending run")

	h := traceRouter(pool, userEmail(t, pool, seeded.userID))
	rec := getTrace(t, h, seeded.runID.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode trace: %v", err)
	}
	for _, field := range []string{"timeline", "tool_calls", "evidence", "citations"} {
		value, ok := raw[field]
		if !ok {
			t.Errorf("trace has no %q field", field)
			continue
		}
		if string(value) != "[]" {
			t.Errorf("%s = %s, want [] on a run that has done nothing", field, value)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func traceTypes(body traceBody) []string {
	out := make([]string, 0, len(body.Timeline))
	for _, entry := range body.Timeline {
		out = append(out, entry.Type)
	}
	return out
}

// sameJSON compares two JSON documents by value, so key order and whitespace do
// not make the comparison brittle.
func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var left, right any
	if err := json.Unmarshal(a, &left); err != nil {
		t.Fatalf("decode %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &right); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return reflect.DeepEqual(left, right)
}
