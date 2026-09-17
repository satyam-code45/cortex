// Mirrors of the Go API's JSON shapes. Each type names the Go source it
// mirrors; when a Go struct changes, this file is the other half of the edit.
//
// Go pointer fields marshal to `null`, hence the `| null` unions. Timestamps
// are RFC 3339 strings.

// ---- REST responses ----

// chatResponse (backend/internal/api/chat.go)
export interface ChatResponse {
  conversation_id: string;
  run_id: string;
}

// runResponse (backend/internal/api/runs.go)
export interface RunResponse {
  run_id: string;
  conversation_id: string;
  status: RunStatus;
  answer: string | null;
  error: string | null;
  model?: string | null;
  latency_ms?: number | null;
  input_tokens?: number | null;
  output_tokens?: number | null;
}

// agent_runs.status CHECK constraint (backend/db/migrations/00001_core_tables.sql)
export type RunStatus =
  | "pending"
  | "running"
  | "awaiting_approval"
  | "completed"
  | "failed";

// conversationResponse (backend/internal/api/conversations.go)
export interface Conversation {
  id: string;
  title: string | null;
  created_at: string;
  updated_at: string;
}

// messageResponse (backend/internal/api/conversations.go)
export interface Message {
  id: string;
  role: "user" | "assistant";
  content: string;
  agent_run_id: string | null;
  created_at: string;
}

// errorResponse (backend/internal/api/api.go)
export interface ApiError {
  error: string;
}

// meResponse (backend/internal/api/auth.go)
export interface Me {
  email: string;
  name: string;
  avatar_url: string;
  has_llm_key: boolean;
  provider?: string;
}

// llmKeyResponse (backend/internal/api/settings.go)
export interface LLMKeyInfo {
  provider: string;
  last4: string;
}

// ---- Connections (backend/internal/api/connections.go) ----

// sourceStatus
export interface ConnectionSourceStatus {
  status: "connected" | "error" | "absent";
  // Display facts captured at connect time (site URL, account name, mailbox
  // address) — the API never returns credentials.
  identity?: Record<string, string>;
  updated_at?: string | null;
  last_error?: string;
  // writes_enabled: this connection may propose writes. False by default and
  // for every connection made before writes existed — a read connection never
  // becomes a write connection without somebody asking.
  writes_enabled: boolean;
}

// connectionsResponse
export interface ConnectionsInfo {
  // "none" is a new account: nothing connected and the demo not asked for, so
  // no run can start until the user connects a source or (where the deployment
  // offers one) opts into the demo workspace.
  mode: "demo" | "user" | "none";
  use_demo_workspace: boolean;
  sources: Record<SourceName, ConnectionSourceStatus>;
  // Whether this deployment has a demo workspace at all. A deployment without
  // one must not offer a mode it cannot enter.
  demo_available: boolean;
  demo_sources?: SourceName[];
  // Whether this deployment has an indexed corpus. Not the same question as
  // demo_available: indexing needs both a demo workspace to crawl and the
  // server's own OpenAI key to embed it, so a demo configured without that key
  // has demo_available true and indexing_available false. The Sources view is
  // a feature of the index, so it gates on this one.
  indexing_available: boolean;
}

// ---- Sources (backend/internal/api/documents.go) ----

export type SourceName = "jira" | "notion" | "gmail";

// Query half of GET /api/documents.
export interface DocumentsQuery {
  source?: SourceName;
  q?: string;
  limit?: number;
  offset?: number;
}

// documentRow
export interface DocumentRow {
  id: string;
  source: SourceName;
  title: string;
  url: string;
  snippet: string;
  source_timestamp: string | null;
}

// documentsResponse
export interface DocumentsResponse {
  documents: DocumentRow[];
  counts: Record<string, number>;
  last_indexed: Record<string, string>;
  last_refreshed: string | null;
}

// documentDetail
export interface DocumentDetail {
  id: string;
  source: SourceName;
  external_id: string;
  title: string;
  url: string;
  content: string;
  source_timestamp: string | null;
  updated_at: string;
}

// refreshResponse
export interface RefreshResponse {
  queued: string[];
}

// ---- Trace (backend/internal/agent/trace.go) ----

export interface Trace {
  run_id: string;
  conversation_id: string;
  query: string;
  status: RunStatus;
  model: string | null;
  answer: string | null;
  error: string | null;
  created_at: string | null;
  finished_at: string | null;
  totals: TraceTotals;
  timeline: TraceEvent[];
  tool_calls: TraceToolCall[];
  evidence: TraceEvidence[];
  citations: TraceCitation[];
}

export interface TraceTotals {
  iterations: number;
  tool_calls: number;
  llm_calls: number;
  input_tokens: number;
  output_tokens: number;
  latency_ms: number;
}

export interface TraceEvent {
  seq: number;
  type: RunEventType;
  at: string | null;
  payload: unknown;
}

export interface TraceToolCall {
  seq: number;
  tool: string;
  arguments: unknown;
  result_summary: string | null;
  evidence_count: number;
  latency_ms: number | null;
  status: string;
  error: string | null;
  at: string | null;
}

export interface TraceEvidence {
  id: string;
  seq: number;
  source: string;
  external_id: string;
  title: string | null;
  url: string | null;
  snippet: string | null;
  source_timestamp: string | null;
  tool_call_id: string | null;
}

export interface TraceCitation {
  marker: string;
  claim: string | null;
  evidence_id: string;
  evidence_seq: number;
  source: string;
  external_id: string;
  title: string | null;
  url: string | null;
  snippet: string | null;
  source_timestamp: string | null;
}

// ---- Run events (backend/internal/agent/events.go) ----
//
// The event types the orchestrator writes, streamed over SSE as
// `event: <type>` with the payload as `data:`. There is no answer_delta —
// token-level streaming is a non-goal.
//
// The last six are the approval gate. A run that proposes a write emits
// action_proposed for each one and then run_paused, which CLOSES the stream:
// nothing further happens until a person decides, and that can take hours. The
// decision reopens a fresh stream, which replays from the start (a new
// EventSource cannot send Last-Event-ID) — harmless, because events are deduped
// by seq.

export type RunEventType =
  | "run_started"
  | "llm_call"
  | "tool_call_started"
  | "tool_call_finished"
  | "citations"
  | "answer"
  | "run_finished"
  | "run_failed"
  | "action_proposed"
  | "run_paused"
  | "action_decided"
  | "action_executed"
  | "action_failed"
  | "run_resumed";

// runStartedPayload
export interface RunStartedPayload {
  conversation_id: string;
  query: string;
  model: string;
  max_iterations: number;
  system_prompt: string;
  tools: string[];
  // Absent on runs recorded before per-user source connections existed.
  sources?: RunSources;
  history: EventMessage[];
}

// sourcesPayload (backend/internal/agent/events.go)
export interface RunSources {
  mode: "demo" | "user" | "";
  connected: string[];
}

// llmCallPayload
export interface LLMCallPayload {
  purpose: string;
  model: string;
  iteration: number;
  injected_messages?: EventMessage[];
  message_count: number;
  text: string;
  tool_calls: EventToolCall[];
  input_tokens: number;
  output_tokens: number;
  latency_ms: number;
}

// toolCallStartedPayload
export interface ToolCallStartedPayload {
  iteration: number;
  tool_call_id: string;
  tool: string;
  arguments: unknown;
  canonical_arguments: string;
}

// toolCallFinishedPayload
export interface ToolCallFinishedPayload {
  iteration: number;
  tool_call_id: string;
  tool: string;
  observation: string;
  raw_content_length: number;
  truncated: boolean;
  summarized: boolean;
  evidence_count: number;
  evidence?: EventEvidence[];
  status: string;
  error?: string;
  cache_hit: boolean;
  latency_ms: number;
}

// citationsPayload
export interface CitationsPayload {
  citations: EventCitation[];
  dropped: number;
  evidence_count: number;
  skipped?: string;
}

// answerPayload
export interface AnswerPayload {
  answer: string;
  iterations: number;
  forced: boolean;
}

// runFinishedPayload
export interface RunFinishedPayload {
  iterations: number;
  tool_calls: number;
  input_tokens: number;
  output_tokens: number;
  latency_ms: number;
}

// runFailedPayload
export interface RunFailedPayload {
  error: string;
  iterations: number;
  latency_ms: number;
}

// actionProposedPayload
//
// payload is the proposal verbatim, and the approval card renders it field by
// field from here. Never summarized: approving a paraphrase of an email is not
// approving the email.
export interface ActionProposedPayload {
  iteration: number;
  action_id: string;
  tool_call_id: string;
  tool: string;
  source: ActionSource;
  action: string;
  summary: string;
  payload: unknown;
}

// runPausedPayload
export interface RunPausedPayload {
  iteration: number;
  waiting: PausedAction[];
  iterations_used: number;
  input_tokens: number;
  output_tokens: number;
}

// pausedAction
export interface PausedAction {
  action_id: string;
  source: ActionSource;
  action: string;
  summary: string;
}

// actionDecidedPayload
//
// decided_by is a user id, and only the action's own owner can decide it — the
// endpoints enforce that in SQL — so it always resolves to the run's owner. The
// UI says "you" rather than rendering a UUID at somebody.
export interface ActionDecidedPayload {
  action_id: string;
  action: string;
  status: ActionStatus;
  reason?: string;
  decided_by?: string;
  decided_at: string;
  edited: boolean;
}

// actionExecutedPayload
export interface ActionExecutedPayload {
  action_id: string;
  action: string;
  summary: string;
  result?: Record<string, unknown>;
}

// actionFailedPayload
export interface ActionFailedPayload {
  action_id: string;
  action: string;
  error: string;
}

// runResumedPayload
export interface RunResumedPayload {
  iteration: number;
  decisions: ResumeDecision[];
}

// resumeDecision
export interface ResumeDecision {
  action_id: string;
  action: string;
  status: ActionStatus;
  outcome: string;
}

// ---- Actions (backend/internal/api/actions.go) ----

export type ActionSource = "gmail" | "jira" | "notion";

// The one-way status lifecycle:
//   pending -> approved -> executing -> executed | failed
//   pending -> rejected | expired
export type ActionStatus =
  | "pending"
  | "approved"
  | "rejected"
  | "executing"
  | "executed"
  | "failed"
  | "expired";

// AgentAction is one row of the audit trail, as GET /api/actions reports it.
//
// Both payloads are present in full: the approval card needs the proposal, and
// the audit view shows it beside what was approved so a reader can see whether
// a person changed it before saying yes.
export interface AgentAction {
  id: string;
  agent_run_id: string;
  source: ActionSource;
  action: string;
  status: ActionStatus;
  proposed_payload: unknown;
  final_payload?: unknown;
  reject_reason?: string;
  result?: ActionResult;
  error?: string;
  proposed_at: string;
  decided_at?: string;
  executed_at?: string;
  decided_by?: string;
  // edited: the approved payload differs from the proposal. Computed by the
  // backend so the UI never has to diff two JSON documents.
  edited: boolean;
  // editable: still pending and not yet expired — the only state in which the
  // approve and reject buttons do anything.
  editable: boolean;
}

// The shape stored in agent_actions.result.
export interface ActionResult {
  summary: string;
  detail?: Record<string, unknown>;
}

// ---- Write payload shapes (backend/internal/tools/*/writetools.go) ----
//
// One interface per action, so the approval card can render named fields
// instead of a JSON blob. They are the proposed_payload of the matching action
// name; anything unrecognized falls back to a formatted JSON view.

// gmail.send
export interface SendEmailPayload {
  from: string;
  to: string[];
  cc?: string[];
  subject: string;
  body: string;
  thread_id?: string;
  in_reply_to?: string;
  references?: string;
}

// jira.create_issue
export interface CreateIssuePayload {
  project_key: string;
  issue_type: string;
  summary: string;
  description?: string;
  labels?: string[];
  due_date?: string;
  project_name?: string;
}

// jira.update_issue — only the fields present are changed.
export interface UpdateIssuePayload {
  key: string;
  fields: {
    summary?: string;
    description?: string;
    due_date?: string;
    labels?: string[];
  };
  // before: the current values of the fields being changed, read when the
  // proposal was made. It turns "set the due date to October" into "move it
  // from July to October" — the difference between a request a person can judge
  // and one they can only accept on faith.
  before?: Record<string, string>;
}

// jira.add_comment
export interface AddCommentPayload {
  key: string;
  body: string;
  issue_summary?: string;
}

// jira.transition_issue
export interface TransitionIssuePayload {
  key: string;
  to_status: string;
  transition_id: string;
  from_status?: string;
}

// notion.append_to_page
export interface AppendToPagePayload {
  page_id: string;
  markdown: string;
  page_title?: string;
}

// notion.create_page
export interface CreatePagePayload {
  parent_page_id: string;
  title: string;
  markdown: string;
  parent_title?: string;
}

// eventMessage
export interface EventMessage {
  role: string;
  content: string;
}

// eventToolCall
export interface EventToolCall {
  id: string;
  name: string;
  arguments: unknown;
  arguments_raw?: string;
}

// eventEvidence
export interface EventEvidence {
  source: string;
  external_id: string;
  title: string;
  url: string;
  snippet: string;
  timestamp?: string;
}

// eventCitation
export interface EventCitation {
  marker: string;
  evidence_id: string;
  evidence_seq: number;
  claim: string;
}

// One event as received over the SSE stream, tagged with its type and seq
// (the SSE `id:` field) by the client.
export interface RunEvent {
  seq: number;
  type: RunEventType;
  payload:
    | RunStartedPayload
    | LLMCallPayload
    | ToolCallStartedPayload
    | ToolCallFinishedPayload
    | CitationsPayload
    | AnswerPayload
    | RunFinishedPayload
    | RunFailedPayload
    | ActionProposedPayload
    | RunPausedPayload
    | ActionDecidedPayload
    | ActionExecutedPayload
    | ActionFailedPayload
    | RunResumedPayload;
}
