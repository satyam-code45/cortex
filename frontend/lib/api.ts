// Typed client for the Go API. One tiny wrapper over fetch: every endpoint is
// JSON in/out, errors come back as {"error": "..."} with a non-2xx status.

import type {
  ChatResponse,
  Conversation,
  Message,
  RunResponse,
  Trace,
} from "./types";

// Trailing slash trimmed so callers can write `${API_URL}/api/...` regardless
// of how the env var was typed.
export const API_URL = (
  process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:8080"
).replace(/\/+$/, "");

// ApiRequestError carries the server's error message and the HTTP status, so
// UI code can tell "run not found" from "the backend is down".
export class ApiRequestError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = "ApiRequestError";
    this.status = status;
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  let res: Response;
  try {
    res = await fetch(`${API_URL}${path}`, init);
  } catch {
    throw new ApiRequestError(0, "cannot reach the API server");
  }
  if (!res.ok) {
    let message = `request failed (${res.status})`;
    try {
      const body = (await res.json()) as { error?: string };
      if (body.error) message = body.error;
    } catch {
      // Non-JSON error body; the status-derived message stands.
    }
    throw new ApiRequestError(res.status, message);
  }
  return (await res.json()) as T;
}

// POST /api/chat — records the question and queues an agent run.
export function sendChat(
  message: string,
  conversationId?: string,
): Promise<ChatResponse> {
  return request<ChatResponse>("/api/chat", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(
      conversationId
        ? { conversation_id: conversationId, message }
        : { message },
    ),
  });
}

// GET /api/runs/{id} — run status + answer.
export function getRun(runId: string): Promise<RunResponse> {
  return request<RunResponse>(`/api/runs/${runId}`);
}

// GET /api/runs/{id}/trace — the full record of what a run did.
export function getTrace(runId: string): Promise<Trace> {
  return request<Trace>(`/api/runs/${runId}/trace`);
}

// GET /api/conversations — the user's conversations, newest-updated first.
export function listConversations(): Promise<Conversation[]> {
  return request<Conversation[]>("/api/conversations");
}

// GET /api/conversations/{id}/messages — one conversation's thread in order.
export function listMessages(conversationId: string): Promise<Message[]> {
  return request<Message[]>(`/api/conversations/${conversationId}/messages`);
}

// URL of the SSE stream for a run; the consumer opens it with EventSource.
export function runEventsUrl(runId: string): string {
  return `${API_URL}/api/runs/${runId}/events`;
}
