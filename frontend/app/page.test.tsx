// Onboarding on the chat page.
//
// A new account has connected nothing, so there is nothing for a run to search
// and the server answers 409 no_sources_connected. The page must say that
// before the user composes a question: it points at Connections and refuses to
// send, rather than letting a run be attempted and fail.
//
// The API module and the SSE hook are mocked — the network contract is tested
// in Go and EventSource does not exist in jsdom — but the page itself is real.

import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { ConnectionsInfo, Me } from "@/lib/types";
import Home from "./page";

const api = vi.hoisted(() => ({
  getConnections: vi.fn(),
  listConversations: vi.fn(),
  listMessages: vi.fn(),
  getTrace: vi.fn(),
  sendChat: vi.fn(),
}));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    getConnections: api.getConnections,
    listConversations: api.listConversations,
    listMessages: api.listMessages,
    getTrace: api.getTrace,
    sendChat: api.sendChat,
  };
});

// The signed-in user, with a key: this file is about sources, and a missing key
// has its own call-to-action that would otherwise mask it.
const authState = vi.hoisted(() => ({
  me: null as Me | null,
}));
vi.mock("@/components/nav/AppShell", () => ({
  useAuthContext: () => ({
    me: authState.me,
    loading: false,
    refresh: () => {},
  }),
}));

vi.mock("@/lib/useRunStream", () => ({
  useRunStream: () => ({
    events: [],
    answer: null,
    error: null,
    status: "terminal",
    waiting: [],
    reopen: () => {},
  }),
}));

const signedInUser: Me = {
  email: "new.user@example.com",
  name: "New User",
  avatar_url: "",
  has_llm_key: true,
};

// freshAccount has connected nothing and has not opted into any demo.
const freshAccount = (): ConnectionsInfo => ({
  mode: "none",
  demo_available: false,
  indexing_available: false,
  use_demo_workspace: false,
  sources: {
    jira: { status: "absent", writes_enabled: false },
    notion: { status: "absent", writes_enabled: false },
    gmail: { status: "absent", writes_enabled: false },
  },
});

const connectedAccount = (): ConnectionsInfo => ({
  ...freshAccount(),
  mode: "user",
  sources: {
    ...freshAccount().sources,
    jira: {
      status: "connected",
      writes_enabled: false,
      identity: { site_url: "https://own.atlassian.net", account_name: "New User" },
      updated_at: "2026-09-01T10:00:00Z",
    },
  },
});

beforeEach(() => {
  authState.me = signedInUser;
  api.getConnections.mockReset();
  api.listConversations.mockReset().mockResolvedValue([]);
  api.listMessages.mockReset().mockResolvedValue([]);
  api.getTrace.mockReset();
  api.sendChat.mockReset();
});

afterEach(() => {
  cleanup();
});

describe("Home onboarding with no connected sources", () => {
  // Deliberately an in-place empty state with a link, not a redirect: the spec
  // asks for both ("route them to Connections" and "render the empty state the
  // 409 implies"), and a user who lands on Chat and is bounced elsewhere cannot
  // see what the app is. The link is the routing.
  it("offers a user with zero connections a way to Connections and will not let them ask", async () => {
    api.getConnections.mockResolvedValue(freshAccount());
    render(<Home />);

    const link = await screen.findByRole("link", {
      name: /connect Jira, Notion or Gmail/i,
    });
    expect(link).toHaveAttribute("href", "/connections");
    expect(
      screen.getByText(/Connect a source to start asking questions/i),
    ).toBeInTheDocument();

    // The input is not merely decorated: a question that cannot run must not be
    // sendable, because the only outcome would be a 409.
    expect(screen.getByRole("textbox")).toBeDisabled();
  });

  it("says nothing about connecting once the user has a source", async () => {
    api.getConnections.mockResolvedValue(connectedAccount());
    render(<Home />);

    await waitFor(() => expect(api.getConnections).toHaveBeenCalled());
    await waitFor(() => expect(screen.getByRole("textbox")).toBeEnabled());
    expect(
      screen.queryByText(/Connect a source to start asking questions/i),
    ).toBeNull();
  });

  it("does not disable the input when the connections lookup fails", async () => {
    // The server is the authority: it answers 409 if there is genuinely
    // nothing to search. A failed probe must not lock a working account out of
    // its own chat box.
    api.getConnections.mockRejectedValue(new Error("network down"));
    render(<Home />);

    await waitFor(() => expect(api.getConnections).toHaveBeenCalled());
    await waitFor(() => expect(screen.getByRole("textbox")).toBeEnabled());
    expect(
      screen.queryByText(/Connect a source to start asking questions/i),
    ).toBeNull();
  });
});
