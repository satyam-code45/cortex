package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"cortex/internal/llm"
	"cortex/internal/store"
)

// The run_events transcript.
//
// run_events is an append-only log with a gap-free `seq` per run, and it is the
// system's only durable record of *how* an answer was reached. Two later
// features are built directly on it, which is why its completeness is a hard
// requirement rather than a nice-to-have:
//
//   - The trace panel streams these rows to the browser as they land.
//   - Pause/resume and human-in-the-loop approval gates reconstruct the
//     model's message transcript from them and continue the loop from there.
//
// The second is the demanding one. It means every event that contributed to the
// conversation the model saw has to carry enough to rebuild that turn exactly:
// full tool arguments, the observation text as the model received it, and the
// assistant's own tool-call requests. ReconstructTranscript below is that
// rebuild, and it is production code — not a test helper — because resuming a
// run is the same operation as replaying one.
const (
	// EventRunStarted opens a run. Payload carries the query, the system prompt,
	// and the conversation history the loop began from.
	EventRunStarted = "run_started"
	// EventLLMCall records one generation: the request shape and the response,
	// including any tool calls the model asked for.
	EventLLMCall = "llm_call"
	// EventToolCallStarted records a tool invocation with its full arguments.
	EventToolCallStarted = "tool_call_started"
	// EventToolCallFinished records the observation handed back to the model.
	EventToolCallFinished = "tool_call_finished"
	// EventContextCompaction records the context guard rewriting old tool
	// observations into summaries. The payload carries the FULL replacement
	// text per tool_call_id, so replay is a verbatim substitution — nothing
	// is re-derived, and the fencing format can evolve without breaking the
	// replay of old runs.
	EventContextCompaction = "context_compaction"
	// EventCitations records the outcome of the citation pass: which markers
	// survived, what they point at, and how many the validator dropped.
	EventCitations = "citations"
	// EventAnswer records the final answer text.
	EventAnswer = "answer"
	// EventRunFinished closes a successful run.
	EventRunFinished = "run_finished"
	// EventRunFailed closes a failed run.
	EventRunFailed = "run_failed"

	// EventActionProposed records a write the agent wants performed, with the
	// complete payload it proposed. Observability, not conversation: the
	// observation the model actually saw is in the accompanying
	// tool_call_finished event, and it is that text a replay feeds back.
	EventActionProposed = "action_proposed"
	// EventRunPaused records the loop stopping to wait for a human. It closes
	// the event stream — nothing further will happen on this run until somebody
	// decides — so the browser keeps the approval cards on screen and reopens
	// the stream after it posts a decision.
	EventRunPaused = "run_paused"
	// EventActionDecided records a human's approve or reject, with who decided
	// and when. This is the audit half of the gate: the row carries the same
	// facts, and the event puts them in the run's own timeline.
	EventActionDecided = "action_decided"
	// EventActionExecuted records a write that landed, with what the upstream
	// system returned.
	EventActionExecuted = "action_executed"
	// EventActionFailed records a write that was attempted and failed. Distinct
	// from a rejection: somebody approved this one, and it did not happen.
	EventActionFailed = "action_failed"
	// EventRunResumed records the loop picking back up after every proposal was
	// decided, and carries the decisions being fed back to the model.
	EventRunResumed = "run_resumed"
)

// LLM call purposes recorded on llm_calls.purpose and in llm_call events.
const (
	// PurposeAgentLoop is a normal reasoning turn inside the loop.
	PurposeAgentLoop = "agent_loop"
	// PurposeToolOutputSummary is the utility-model call that compresses an
	// oversized tool result.
	PurposeToolOutputSummary = "tool_output_summary"
	// PurposeContextCompaction is the utility-model call that summarizes one
	// old observation when the transcript nears the context token budget.
	PurposeContextCompaction = "context_compaction_summary"
	// PurposeFinalAnswer is the forced answer after the iteration cap is hit.
	PurposeFinalAnswer = "final_answer"

	// PurposeResume is the first call after a run was paused for a human
	// decision on a proposed write.
	//
	// It has its own value because it must not be mistaken for a completeness
	// check. Both open with an injected turn, so inferring the purpose from
	// "is there something injected" labelled every resumed run's first call a
	// completeness check — which made the trace wrong and, worse, made
	// replayProgress believe the check had already happened, so a run that
	// paused twice would accept its first draft unreviewed.
	PurposeResume = "resume"
	// PurposeCompletenessCheck is the one call per run that re-reads the model's
	// own draft answer against the question before it is accepted.
	PurposeCompletenessCheck = "completeness_check"

	// PurposeAnswerCitations is the structured call that attaches evidence
	// markers to the accepted answer.
	PurposeAnswerCitations = "answer_citations"
)

// runStartedPayload is the payload of EventRunStarted.
type runStartedPayload struct {
	ConversationID uuid.UUID      `json:"conversation_id"`
	Query          string         `json:"query"`
	Model          string         `json:"model"`
	MaxIterations  int            `json:"max_iterations"`
	SystemPrompt   string         `json:"system_prompt"`
	Tools          []string       `json:"tools"`
	Sources        sourcesPayload `json:"sources"`
	History        []eventMessage `json:"history"`
}

// sourcesPayload records which workspace the run's tools reached — the demo
// workspace or the owner's connected sources — so a trace is honest about
// what it searched. Events written before per-user connections existed
// unmarshal it to the zero value, which readers treat as "recorded before
// sources were tracked".
type sourcesPayload struct {
	Mode      string   `json:"mode"`
	Connected []string `json:"connected"`
}

// connectedOrEmpty keeps the recorded connected list a JSON array, never null.
func connectedOrEmpty(connected []string) []string {
	if connected == nil {
		return []string{}
	}
	return connected
}

// llmCallPayload is the payload of EventLLMCall.
type llmCallPayload struct {
	Purpose   string `json:"purpose"`
	Model     string `json:"model"`
	Iteration int    `json:"iteration"`
	// InjectedMessages are turns the loop itself added to the transcript
	// immediately before this call, rather than turns produced by the model or by
	// a tool. Today that is the forced-answer instruction at the iteration cap.
	//
	// They have to be recorded or the transcript cannot be rebuilt: a replay of a
	// capped run would omit the very instruction that produced its answer, and
	// a resume would continue from a conversation that never happened.
	InjectedMessages []eventMessage  `json:"injected_messages,omitempty"`
	MessageCount     int             `json:"message_count"`
	Text             string          `json:"text"`
	ToolCalls        []eventToolCall `json:"tool_calls"`
	InputTokens      int             `json:"input_tokens"`
	OutputTokens     int             `json:"output_tokens"`
	LatencyMS        int64           `json:"latency_ms"`
}

// toolCallStartedPayload is the payload of EventToolCallStarted.
type toolCallStartedPayload struct {
	Iteration  int             `json:"iteration"`
	ToolCallID string          `json:"tool_call_id"`
	Tool       string          `json:"tool"`
	Arguments  json.RawMessage `json:"arguments"`
	// CanonicalArguments is the dedupe key, kept so a replay can tell a cache
	// hit from a fresh execution without recomputing it.
	CanonicalArguments string `json:"canonical_arguments"`
}

// toolCallFinishedPayload is the payload of EventToolCallFinished.
//
// Observation is the exact text appended to the model's transcript — after any
// truncation. That is what makes a replay faithful: resuming from the
// pre-truncation content would feed the model a conversation it never had.
type toolCallFinishedPayload struct {
	Iteration  int    `json:"iteration"`
	ToolCallID string `json:"tool_call_id"`
	Tool       string `json:"tool"`

	Observation string `json:"observation"`
	// RawContentLength is the pre-truncation size, so a trace can show what was
	// dropped even though the transcript carries the shortened form.
	RawContentLength int  `json:"raw_content_length"`
	Truncated        bool `json:"truncated"`
	Summarized       bool `json:"summarized"`

	EvidenceCount int             `json:"evidence_count"`
	Evidence      []eventEvidence `json:"evidence,omitempty"`
	Status        string          `json:"status"`
	Error         string          `json:"error,omitempty"`
	CacheHit      bool            `json:"cache_hit"`
	LatencyMS     int64           `json:"latency_ms"`
}

// contextCompactionPayload is the payload of EventContextCompaction.
type contextCompactionPayload struct {
	Iteration    int `json:"iteration"`
	TokensBefore int `json:"tokens_before"`
	TokensAfter  int `json:"tokens_after"`
	Budget       int `json:"budget"`
	// Compactions is ordered oldest-first, matching the order the live loop
	// applied them.
	Compactions []compactionEntry `json:"compactions"`
}

// compactionEntry is one observation rewritten by the context guard.
type compactionEntry struct {
	ToolCallID string `json:"tool_call_id"`
	// Content is the complete replacement observation, byte-identical to what
	// now sits in the live transcript — fence and compaction marker included.
	Content string `json:"content"`
	// OriginalChars is the size of the observation that was replaced, so a
	// trace can show what compaction cost without storing the original twice
	// (it is already in this run's tool_call_finished event).
	OriginalChars int `json:"original_chars"`
}

// citationsPayload is the payload of EventCitations.
//
// Skipped is recorded rather than inferred from an empty Citations list: "the
// run gathered no evidence" and "the citation call timed out" look identical
// from the outside, and only one of them is a problem.
type citationsPayload struct {
	Citations []eventCitation `json:"citations"`
	Dropped   int             `json:"dropped"`
	Evidence  int             `json:"evidence_count"`
	Skipped   string          `json:"skipped,omitempty"`
}

// eventCitation is one persisted citation as stored in an event payload.
type eventCitation struct {
	Marker      string    `json:"marker"`
	EvidenceID  uuid.UUID `json:"evidence_id"`
	EvidenceSeq int       `json:"evidence_seq"`
	Claim       string    `json:"claim"`
}

// answerPayload is the payload of EventAnswer.
type answerPayload struct {
	Answer     string `json:"answer"`
	Iterations int    `json:"iterations"`
	Forced     bool   `json:"forced"`
}

// runFinishedPayload is the payload of EventRunFinished.
type runFinishedPayload struct {
	Iterations   int   `json:"iterations"`
	ToolCalls    int   `json:"tool_calls"`
	InputTokens  int   `json:"input_tokens"`
	OutputTokens int   `json:"output_tokens"`
	LatencyMS    int64 `json:"latency_ms"`
}

// runFailedPayload is the payload of EventRunFailed.
type runFailedPayload struct {
	Error      string `json:"error"`
	Iterations int    `json:"iterations"`
	LatencyMS  int64  `json:"latency_ms"`
}

// actionProposedPayload is the payload of EventActionProposed.
//
// Payload is the proposal verbatim. The approval UI renders it field by field
// from here rather than from a summary, because a human approving a paraphrase
// of an email has not approved the email — and because this event is the audit
// record of what the agent asked for, which must survive any later edit to the
// row's final payload.
type actionProposedPayload struct {
	Iteration  int             `json:"iteration"`
	ActionID   uuid.UUID       `json:"action_id"`
	ToolCallID string          `json:"tool_call_id"`
	Tool       string          `json:"tool"`
	Source     string          `json:"source"`
	Action     string          `json:"action"`
	Summary    string          `json:"summary"`
	Payload    json.RawMessage `json:"payload"`
}

// runPausedPayload is the payload of EventRunPaused.
type runPausedPayload struct {
	Iteration int `json:"iteration"`
	// Waiting lists the actions the run is blocked on, so a client that joined
	// the stream late knows what to render without a second request.
	Waiting []pausedAction `json:"waiting"`
	// IterationsUsed and the token totals are carried so a paused run's cost is
	// legible before it resumes.
	IterationsUsed int `json:"iterations_used"`
	InputTokens    int `json:"input_tokens"`
	OutputTokens   int `json:"output_tokens"`
}

// pausedAction identifies one action a paused run is waiting on.
type pausedAction struct {
	ActionID uuid.UUID `json:"action_id"`
	Source   string    `json:"source"`
	Action   string    `json:"action"`
	Summary  string    `json:"summary"`
}

// actionDecidedPayload is the payload of EventActionDecided.
type actionDecidedPayload struct {
	ActionID uuid.UUID `json:"action_id"`
	Action   string    `json:"action"`
	Status   string    `json:"status"`
	Reason   string    `json:"reason,omitempty"`
	// DecidedBy is the deciding user's id, and DecidedAt when. Both are on the
	// row too; they are duplicated here because the trace panel renders from
	// events alone, and a timeline that shows a pause with no visible decision is
	// missing the part a reader most wants.
	//
	// Only the action's own owner can decide it — the endpoints enforce that in
	// SQL — so the id always resolves to the run's owner, and the UI can say
	// "you" rather than rendering a UUID at somebody.
	DecidedBy *uuid.UUID `json:"decided_by,omitempty"`
	DecidedAt time.Time  `json:"decided_at"`
	// Edited reports whether the approved payload differs from the proposed
	// one. It is the single fact an auditor most wants at a glance, and
	// computing it here means the UI never has to diff two JSON blobs to
	// answer it.
	Edited bool `json:"edited"`
}

// actionExecutedPayload is the payload of EventActionExecuted.
type actionExecutedPayload struct {
	ActionID uuid.UUID      `json:"action_id"`
	Action   string         `json:"action"`
	Summary  string         `json:"summary"`
	Result   map[string]any `json:"result,omitempty"`
}

// actionFailedPayload is the payload of EventActionFailed.
type actionFailedPayload struct {
	ActionID uuid.UUID `json:"action_id"`
	Action   string    `json:"action"`
	Error    string    `json:"error"`
}

// runResumedPayload is the payload of EventRunResumed.
//
// Decisions is what the model is about to be told, recorded before the call that
// tells it. The text itself is replayed from the following llm_call's injected
// messages — this event is the human-readable "why did the loop start again",
// which the trace panel shows and a replay ignores.
type runResumedPayload struct {
	Iteration int              `json:"iteration"`
	Decisions []resumeDecision `json:"decisions"`
}

// resumeDecision is one decided action as reported back into the loop.
type resumeDecision struct {
	ActionID uuid.UUID `json:"action_id"`
	Action   string    `json:"action"`
	Status   string    `json:"status"`
	Outcome  string    `json:"outcome"`
}

// eventMessage is a conversation turn as stored in an event payload.
type eventMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// eventToolCall is a model tool-call request as stored in an event payload.
//
// Arguments is always valid JSON so the payload can be stored as jsonb and
// queried with jsonb operators. Models do emit malformed arguments — a truncated
// stream yields something like `{"q":` — and embedding that raw would make
// json.Marshal of the whole payload fail, which used to take the entire run down
// over one recoverable bad call.
//
// ArgumentsRaw preserves what the model literally emitted whenever it could not
// be stored as JSON. Without it a replay would feed the model a call it never
// made, and the transcript would stop being a faithful record of the run.
type eventToolCall struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Arguments    json.RawMessage `json:"arguments"`
	ArgumentsRaw string          `json:"arguments_raw,omitempty"`
}

// toEventToolCall converts a model tool call for storage, keeping the literal
// argument text when it is not valid JSON.
func toEventToolCall(tc llm.ToolCall) eventToolCall {
	out := eventToolCall{
		ID:        tc.ID,
		Name:      tc.Name,
		Arguments: normalizeArgs(tc.Arguments),
	}
	// Only keep the raw form when there is something to keep. Whitespace is not
	// valid JSON but is also not information, and storing " " would make
	// arguments() replay " " where normalizeArgs already gives a usable {}.
	if strings.TrimSpace(string(tc.Arguments)) != "" && !json.Valid(tc.Arguments) {
		out.ArgumentsRaw = string(tc.Arguments)
	}
	return out
}

// arguments returns the argument text to replay, preferring the literal form the
// model produced.
func (c eventToolCall) arguments() json.RawMessage {
	if c.ArgumentsRaw != "" {
		return json.RawMessage(c.ArgumentsRaw)
	}
	return c.Arguments
}

// eventEvidence is an evidence item as stored in an event payload.
//
// Evidence lives in two places on purpose, and both are load-bearing: the
// evidence table is what citations join against, while this copy inside the
// tool_result payload is what makes a run replayable from run_events alone.
// Dropping it would silently break transcript reconstruction.
type eventEvidence struct {
	Source     string `json:"source"`
	ExternalID string `json:"external_id"`
	Title      string `json:"title"`
	URL        string `json:"url"`
	Snippet    string `json:"snippet"`
	Timestamp  string `json:"timestamp,omitempty"`
}

// appendEvent writes the next run_events row.
//
// The sequence number is computed inside the INSERT (see
// db/queries/run_events.sql) rather than tracked in Go, so a resumed or retried
// run continues the transcript instead of colliding with the unique
// (agent_run_id, seq) constraint.
//
// To be precise about why this is safe, since it is easy to state wrongly: a
// transaction does NOT make coalesce(max(seq),0)+1 atomic at READ COMMITTED —
// two concurrent inserts for the same run would both compute N+1. What makes it
// safe is the unique constraint, which turns that race into a loud error rather
// than a silent gap, combined with StartAgentRun's claim ensuring only one worker
// is driving a given run at a time.
func appendEvent(ctx context.Context, q store.Querier, runID uuid.UUID, eventType string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s payload: %w", eventType, err)
	}
	if _, err := q.InsertRunEvent(ctx, store.InsertRunEventParams{
		AgentRunID: runID,
		Type:       eventType,
		Payload:    encoded,
	}); err != nil {
		return fmt.Errorf("insert %s event: %w", eventType, err)
	}
	return nil
}

// ReconstructTranscript rebuilds the message transcript a run showed the model,
// from its run_events rows alone.
//
// This is the property that makes pause/resume additive rather than a rewrite:
// a paused run has no in-process state, so continuing it means rebuilding the
// conversation from the log. The same function proves the log is complete —
// if the rebuilt transcript matches the one the loop actually used, nothing
// material went unrecorded.
//
// events must be ordered by seq. The system prompt is returned separately
// because llm.Request carries it out of band rather than as a message.
func ReconstructTranscript(events []store.RunEvent) (system string, messages []llm.Message, err error) {
	for _, event := range events {
		switch event.Type {
		case EventRunStarted:
			var payload runStartedPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return "", nil, fmt.Errorf("decode %s payload (seq %d): %w", event.Type, event.Seq, err)
			}
			// A run can legitimately be claimed more than once: StartAgentRun
			// matches 'pending' OR 'running' so that a River job retried after a
			// worker was killed mid-investigation can pick the run back up. Each
			// attempt appends its own run_started, and the loop starts over from
			// the conversation history.
			//
			// So a new run_started supersedes everything before it. Without this
			// reset a resumed run would rebuild as
			// history+attempt1+history+attempt2 — a conversation the model never
			// saw — and a resume would continue from it.
			system = payload.SystemPrompt
			messages = messages[:0]
			for _, m := range payload.History {
				messages = append(messages, llm.Message{Role: llm.Role(m.Role), Content: m.Content})
			}

		case EventLLMCall:
			var payload llmCallPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return "", nil, fmt.Errorf("decode %s payload (seq %d): %w", event.Type, event.Seq, err)
			}
			// Loop-injected turns come first: they were appended to the live
			// transcript before this call was made.
			for _, m := range payload.InjectedMessages {
				messages = append(messages, llm.Message{Role: llm.Role(m.Role), Content: m.Content})
			}
			// Beyond that, only tool-requesting turns become part of the
			// transcript. A text-only response is the run's final answer: it is
			// the model's last word rather than a turn the loop fed back in, so
			// replaying it would append a message the loop never sent.
			if len(payload.ToolCalls) == 0 {
				continue
			}
			calls := make([]llm.ToolCall, 0, len(payload.ToolCalls))
			for _, tc := range payload.ToolCalls {
				calls = append(calls, llm.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.arguments()})
			}
			messages = append(messages, llm.Message{
				Role:      llm.RoleAssistant,
				Content:   payload.Text,
				ToolCalls: calls,
			})

		case EventToolCallFinished:
			var payload toolCallFinishedPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return "", nil, fmt.Errorf("decode %s payload (seq %d): %w", event.Type, event.Seq, err)
			}
			messages = append(messages, llm.Message{
				Role:       llm.RoleTool,
				Content:    payload.Observation,
				ToolCallID: payload.ToolCallID,
			})

		case EventContextCompaction:
			var payload contextCompactionPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return "", nil, fmt.Errorf("decode %s payload (seq %d): %w", event.Type, event.Seq, err)
			}
			// Verbatim substitution by ToolCallID. The tool message always
			// exists by now: it was appended by an earlier tool_call_finished
			// event, and events are processed in seq order. A missing one
			// means a corrupted log, which must fail loudly — silently
			// skipping would hand a resumed run a transcript the model never
			// saw.
			for _, entry := range payload.Compactions {
				found := false
				for i := range messages {
					if messages[i].Role == llm.RoleTool && messages[i].ToolCallID == entry.ToolCallID {
						messages[i].Content = entry.Content
						found = true
						break
					}
				}
				if !found {
					return "", nil, fmt.Errorf(
						"context_compaction (seq %d) references tool call %s not in transcript",
						event.Seq, entry.ToolCallID)
				}
			}

		default:
			// run_started/llm_call/tool_call_finished/context_compaction are
			// the only events that contribute to the transcript.
			// tool_call_started, citations, answer, run_finished and
			// run_failed are observability, not conversation.
			//
			// The approval events are observability too, which is worth being
			// explicit about because it looks like an omission. A proposal
			// reaches the model as an ordinary tool observation, so
			// tool_call_finished already replays it; a human's decision reaches
			// it as an injected turn on the next llm_call, so that event already
			// replays it. action_proposed, run_paused, action_decided,
			// action_executed, action_failed and run_resumed are the audit and
			// timeline record of the same facts. Replaying them here would
			// duplicate every one of them into the transcript.
		}
	}
	return system, messages, nil
}
