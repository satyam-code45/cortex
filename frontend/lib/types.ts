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
}

// connectionsResponse
export interface ConnectionsInfo {
  mode: "demo" | "user";
  use_demo_workspace: boolean;
  sources: Record<SourceName, ConnectionSourceStatus>;
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
// The eight event types the orchestrator writes, streamed over SSE as
// `event: <type>` with the payload as `data:`. There is no answer_delta —
// token-level streaming is a non-goal.

export type RunEventType =
  | "run_started"
  | "llm_call"
  | "tool_call_started"
  | "tool_call_finished"
  | "citations"
  | "answer"
  | "run_finished"
  | "run_failed";

// runStartedPayload
export interface RunStartedPayload {
  conversation_id: string;
  query: string;
  model: string;
  max_iterations: number;
  system_prompt: string;
  tools: string[];
  // Absent on runs recorded before Day 8.
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
    | RunFailedPayload;
}
