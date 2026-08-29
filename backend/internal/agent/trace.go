package agent

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"cortex/internal/store"
)

// The trace: everything a run did, in one JSON document.
//
// This is the observability feature (idea.md §15) and it is assembled here
// rather than in the HTTP handler for one reason: the event payload types are
// unexported types in this package, and decoding them a second time in
// internal/api would create a parallel definition of the transcript format that
// drifts the first time a payload field is added. The handler's job is to load
// rows and marshal what this returns.
//
// The timeline carries each event's stored payload verbatim. That is deliberate:
// REQ-4.4 asks for arguments, latency and status per tool call, and those are
// already exactly what the transcript records — re-projecting them into a second
// shape would mean the trace and the replay could disagree about what happened,
// which defeats the point of having a transcript at all.

// Trace is the full record of one agent run.
type Trace struct {
	RunID          uuid.UUID `json:"run_id"`
	ConversationID uuid.UUID `json:"conversation_id"`
	Query          string    `json:"query"`
	Status         string    `json:"status"`
	Model          *string   `json:"model"`
	Answer         *string   `json:"answer"`
	Error          *string   `json:"error"`

	CreatedAt  *time.Time `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at"`

	Totals    TraceTotals     `json:"totals"`
	Timeline  []TraceEvent    `json:"timeline"`
	ToolCalls []TraceToolCall `json:"tool_calls"`
	Evidence  []TraceEvidence `json:"evidence"`
	Citations []TraceCitation `json:"citations"`
}

// TraceTotals is the cost of the run.
type TraceTotals struct {
	Iterations   int   `json:"iterations"`
	ToolCalls    int   `json:"tool_calls"`
	LLMCalls     int   `json:"llm_calls"`
	InputTokens  int   `json:"input_tokens"`
	OutputTokens int   `json:"output_tokens"`
	LatencyMS    int64 `json:"latency_ms"`
}

// TraceEvent is one entry of the interleaved timeline.
//
// Payload is the run_events row's jsonb, unmodified. Its shape depends on Type
// and is documented by the payload structs in events.go.
type TraceEvent struct {
	Seq     int32           `json:"seq"`
	Type    string          `json:"type"`
	At      *time.Time      `json:"at"`
	Payload json.RawMessage `json:"payload"`
}

// TraceToolCall is one row of the normalized tool_calls table.
type TraceToolCall struct {
	Seq           int32           `json:"seq"`
	Tool          string          `json:"tool"`
	Arguments     json.RawMessage `json:"arguments"`
	ResultSummary *string         `json:"result_summary"`
	EvidenceCount int32           `json:"evidence_count"`
	LatencyMS     *int32          `json:"latency_ms"`
	Status        string          `json:"status"`
	Error         *string         `json:"error"`
	At            *time.Time      `json:"at"`
}

// TraceEvidence is one citable source the run touched. Seq is the citation
// number: a [n] marker in the answer resolves to the evidence item with Seq n.
type TraceEvidence struct {
	ID         uuid.UUID  `json:"id"`
	Seq        int32      `json:"seq"`
	Source     string     `json:"source"`
	ExternalID string     `json:"external_id"`
	Title      *string    `json:"title"`
	URL        *string    `json:"url"`
	Snippet    *string    `json:"snippet"`
	Timestamp  *time.Time `json:"source_timestamp"`
	ToolCallID *uuid.UUID `json:"tool_call_id"`
}

// TraceCitation binds a marker in the answer to the evidence behind it.
type TraceCitation struct {
	Marker      string     `json:"marker"`
	Claim       *string    `json:"claim"`
	EvidenceID  uuid.UUID  `json:"evidence_id"`
	EvidenceSeq int32      `json:"evidence_seq"`
	Source      string     `json:"source"`
	ExternalID  string     `json:"external_id"`
	Title       *string    `json:"title"`
	URL         *string    `json:"url"`
	Snippet     *string    `json:"snippet"`
	Timestamp   *time.Time `json:"source_timestamp"`
}

// AssembleTrace builds the trace for one run from its rows.
//
// Pure: no database, no clock. Everything it needs is passed in, which is what
// lets the assembly be tested against a hand-built event list as well as against
// a real run.
//
// events must be ordered by seq.
func AssembleTrace(
	run store.AgentRun,
	events []store.RunEvent,
	toolCalls []store.ToolCall,
	evidence []store.Evidence,
	citations []store.ListCitationsByRunRow,
) Trace {
	trace := Trace{
		RunID:          run.ID,
		ConversationID: run.ConversationID,
		Query:          run.Query,
		Status:         run.Status,
		Model:          run.Model,
		Answer:         run.Answer,
		Error:          run.Error,
		CreatedAt:      timePtr(run.CreatedAt),
		FinishedAt:     timePtr(run.FinishedAt),
		Totals:         traceTotals(run, events),
		Timeline:       make([]TraceEvent, 0, len(events)),
		ToolCalls:      make([]TraceToolCall, 0, len(toolCalls)),
		Evidence:       make([]TraceEvidence, 0, len(evidence)),
		Citations:      make([]TraceCitation, 0, len(citations)),
	}

	for _, event := range events {
		trace.Timeline = append(trace.Timeline, TraceEvent{
			Seq:     event.Seq,
			Type:    event.Type,
			At:      timePtr(event.CreatedAt),
			Payload: json.RawMessage(event.Payload),
		})
	}

	for _, call := range toolCalls {
		trace.ToolCalls = append(trace.ToolCalls, TraceToolCall{
			Seq:           call.Seq,
			Tool:          call.ToolName,
			Arguments:     json.RawMessage(call.Arguments),
			ResultSummary: call.ResultSummary,
			EvidenceCount: call.EvidenceCount,
			LatencyMS:     call.LatencyMs,
			Status:        call.Status,
			Error:         call.Error,
			At:            timePtr(call.CreatedAt),
		})
	}

	for _, item := range evidence {
		trace.Evidence = append(trace.Evidence, TraceEvidence{
			ID:         item.ID,
			Seq:        item.Seq,
			Source:     item.Source,
			ExternalID: item.ExternalID,
			Title:      item.Title,
			URL:        item.Url,
			Snippet:    item.Snippet,
			Timestamp:  timePtr(item.SourceTimestamp),
			ToolCallID: item.ToolCallID,
		})
	}

	for _, c := range citations {
		trace.Citations = append(trace.Citations, TraceCitation{
			Marker:      c.Marker,
			Claim:       c.ClaimText,
			EvidenceID:  c.EvidenceID,
			EvidenceSeq: c.EvidenceSeq,
			Source:      c.Source,
			ExternalID:  c.ExternalID,
			Title:       c.Title,
			URL:         c.Url,
			Snippet:     c.Snippet,
			Timestamp:   timePtr(c.SourceTimestamp),
		})
	}

	return trace
}

// traceTotals derives the run's cost.
//
// The terminal event is authoritative when it exists: the orchestrator wrote it
// from the state it actually accumulated. The agent_runs row is the fallback,
// and it is a real fallback rather than a defensive one — a failed run has no
// run_finished event at all, and a run still in flight has neither. Counting the
// events themselves is what makes a live trace (Day 5 streams this while the run
// is working) report sensible numbers instead of zeros.
func traceTotals(run store.AgentRun, events []store.RunEvent) TraceTotals {
	totals := TraceTotals{}

	for _, event := range events {
		switch event.Type {
		case EventLLMCall:
			totals.LLMCalls++
			var payload llmCallPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				continue
			}
			totals.InputTokens += payload.InputTokens
			totals.OutputTokens += payload.OutputTokens
			if payload.Iteration > totals.Iterations {
				totals.Iterations = payload.Iteration
			}
		case EventToolCallStarted:
			totals.ToolCalls++
		case EventRunFinished:
			var payload runFinishedPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				continue
			}
			totals.Iterations = payload.Iterations
			totals.LatencyMS = payload.LatencyMS
		case EventRunFailed:
			var payload runFailedPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				continue
			}
			totals.Iterations = payload.Iterations
			totals.LatencyMS = payload.LatencyMS
		}
	}

	// The stored totals win where they exist: they are what the run was billed
	// for, and a summarization call outside the transcript contributes to them.
	if run.InputTokens != nil {
		totals.InputTokens = int(*run.InputTokens)
	}
	if run.OutputTokens != nil {
		totals.OutputTokens = int(*run.OutputTokens)
	}
	if run.LatencyMs != nil {
		totals.LatencyMS = int64(*run.LatencyMs)
	}
	return totals
}

// timePtr renders a nullable timestamp as *time.Time so JSON carries null rather
// than the zero time — "not finished" and "finished in the year 1" are different
// claims.
func timePtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time.UTC()
	return &t
}
