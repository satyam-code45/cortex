// Typed client for the Go API. One tiny wrapper over fetch: every endpoint is
// JSON in/out, errors come back as {"error": "..."} with a non-2xx status.

import type {
  ChatResponse,
  ConnectionsInfo,
  ConnectionSourceStatus,
  Conversation,
  DocumentDetail,
  DocumentsQuery,
  DocumentsResponse,
  LLMKeyInfo,
  Me,
  Message,
  RefreshResponse,
  RunResponse,
  SourceName,
  Trace,
} from "./types";

// Trailing slash trimmed so callers can write `${API_URL}/api/...` regardless
// of how the env var was typed.
export const API_URL = (
  process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:8080"
).replace(/\/+$/, "");

// ApiRequestError carries the server's error message and the HTTP status, so
// UI code can tell "run not found" from "the backend is down". For 429s,
// retryAfterSeconds carries the server's cooldown (the refresh countdown).
export class ApiRequestError extends Error {
  readonly status: number;
  readonly retryAfterSeconds: number | null;

  constructor(
    status: number,
    message: string,
    retryAfterSeconds: number | null = null,
  ) {
    super(message);
    this.name = "ApiRequestError";
    this.status = status;
    this.retryAfterSeconds = retryAfterSeconds;
  }
}

// Cross-cutting auth routing, handled once here rather than at every call
// site: a 401 anywhere means "signed out" and a 409 llm_key_required anywhere
// means "go add a key". Injectable so tests can observe the routing without a
// browser; the defaults are hard navigations, which also reset all app state.
export type AuthRouting = {
  onUnauthorized: () => void;
  onLLMKeyRequired: () => void;
};

let routing: AuthRouting = {
  onUnauthorized: () => {
    if (typeof window !== "undefined" && window.location.pathname !== "/login")
      window.location.assign("/login");
  },
  onLLMKeyRequired: () => {
    if (typeof window !== "undefined")
      window.location.assign("/settings?reason=llm_key_required");
  },
};

// setAuthRouting replaces the routing handlers, returning the previous ones
// (tests restore them).
export function setAuthRouting(next: AuthRouting): AuthRouting {
  const prev = routing;
  routing = next;
  return prev;
}

// Paths whose 401 must NOT bounce to /login: /api/auth/me returning 401 is the
// normal signed-out probe the login page itself makes.
const authProbePaths = new Set(["/api/auth/me"]);

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  let res: Response;
  try {
    // credentials:"include" on every call — the session is a cookie on the
    // API origin, which a cross-origin fetch omits by default.
    res = await fetch(`${API_URL}${path}`, { credentials: "include", ...init });
  } catch {
    throw new ApiRequestError(0, "cannot reach the API server");
  }
  if (!res.ok) {
    let message = `request failed (${res.status})`;
    let retryAfterSeconds: number | null = null;
    try {
      const body = (await res.json()) as {
        error?: string;
        retry_after_seconds?: number;
      };
      if (body.error) message = body.error;
      if (typeof body.retry_after_seconds === "number")
        retryAfterSeconds = body.retry_after_seconds;
    } catch {
      // Non-JSON error body; the status-derived message stands.
    }
    if (res.status === 401 && !authProbePaths.has(path)) {
      routing.onUnauthorized();
    }
    if (res.status === 409 && message === "llm_key_required") {
      routing.onLLMKeyRequired();
    }
    // Still thrown after routing, so in-flight callers settle instead of
    // hanging while the navigation happens.
    throw new ApiRequestError(res.status, message, retryAfterSeconds);
  }
  if (res.status === 204) {
    return undefined as T;
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

// URL of the SSE stream for a run; the consumer opens it with EventSource
// (withCredentials — the session cookie is the stream's auth).
export function runEventsUrl(runId: string): string {
  return `${API_URL}/api/runs/${runId}/events`;
}

// GET /api/auth/me — the signed-in identity, or a 401 for the login redirect.
export function getMe(): Promise<Me> {
  return request<Me>("/api/auth/me");
}

// POST /api/auth/logout — revokes the session server-side.
export function logout(): Promise<void> {
  return request<void>("/api/auth/logout", { method: "POST" });
}

// URL the "Continue with Google" button navigates to (a top-level redirect,
// not a fetch — the whole point is leaving for Google's consent screen).
export function googleLoginUrl(): string {
  return `${API_URL}/api/auth/google/login`;
}

// GET /api/settings/llm-key — provider + last4 of the stored key; 404 if none.
export function getLLMKey(): Promise<LLMKeyInfo> {
  return request<LLMKeyInfo>("/api/settings/llm-key");
}

// PUT /api/settings/llm-key — validates the key live, then stores it. A 422
// carries the provider's reason.
export function putLLMKey(provider: string, key: string): Promise<LLMKeyInfo> {
  return request<LLMKeyInfo>("/api/settings/llm-key", {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ provider, key }),
  });
}

// DELETE /api/settings/llm-key — removes the stored key.
export function deleteLLMKey(): Promise<void> {
  return request<void>("/api/settings/llm-key", { method: "DELETE" });
}

// GET /api/connections — mode, demo toggle, and per-source connection status.
export function getConnections(): Promise<ConnectionsInfo> {
  return request<ConnectionsInfo>("/api/connections");
}

// PUT /api/connections/jira — validates the pasted token live against the
// user's own Jira site, then stores it encrypted. A 422 carries Atlassian's
// reason.
export function putJiraConnection(
  baseUrl: string,
  email: string,
  apiToken: string,
): Promise<ConnectionSourceStatus> {
  return request<ConnectionSourceStatus>("/api/connections/jira", {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ base_url: baseUrl, email, api_token: apiToken }),
  });
}

// PUT /api/connections/notion — validates the pasted integration token live,
// then stores it encrypted. A 422 carries Notion's reason.
export function putNotionConnection(
  token: string,
): Promise<ConnectionSourceStatus> {
  return request<ConnectionSourceStatus>("/api/connections/notion", {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ token }),
  });
}

// DELETE /api/connections/{source} — disconnects one source.
export function deleteConnection(source: SourceName): Promise<void> {
  return request<void>(`/api/connections/${source}`, { method: "DELETE" });
}

// PUT /api/connections/mode — flips the "Use demo workspace" toggle; returns
// the fresh overview.
export function putConnectionsMode(
  useDemoWorkspace: boolean,
): Promise<ConnectionsInfo> {
  return request<ConnectionsInfo>("/api/connections/mode", {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ use_demo_workspace: useDemoWorkspace }),
  });
}

// URL the "Connect Gmail" button navigates to (a top-level redirect, like
// googleLoginUrl — the flow goes through Google's consent screen and comes
// back to /connections).
export function gmailConnectUrl(): string {
  return `${API_URL}/api/connections/gmail/connect`;
}

// GET /api/documents — the Sources listing with counts and freshness stamps.
export function listDocuments(
  query: DocumentsQuery = {},
): Promise<DocumentsResponse> {
  const params = new URLSearchParams();
  if (query.source) params.set("source", query.source);
  if (query.q) params.set("q", query.q);
  if (query.limit !== undefined) params.set("limit", String(query.limit));
  if (query.offset !== undefined) params.set("offset", String(query.offset));
  const qs = params.toString();
  return request<DocumentsResponse>(`/api/documents${qs ? `?${qs}` : ""}`);
}

// GET /api/documents/{id} — one document in full.
export function getDocument(id: string): Promise<DocumentDetail> {
  return request<DocumentDetail>(`/api/documents/${id}`);
}

// POST /api/documents/refresh — queues a reindex; a 429's retryAfterSeconds
// drives the cooldown countdown.
export function refreshDocuments(): Promise<RefreshResponse> {
  return request<RefreshResponse>("/api/documents/refresh", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: "{}",
  });
}
