// TEST-8.5 — the connections page states (REQ-8.2/8.3): demo mode with
// nothing connected, connected cards showing identity, an errored connection
// offering reconnect, the demo-workspace toggle, and paste-form validation
// errors surfacing inline (the 422 carries the provider's own reason).
//
// The API module is mocked (the network contract is tested in Go);
// ApiRequestError stays real so the page's instanceof checks run.

import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ApiRequestError } from "@/lib/api";
import type { ConnectionsInfo } from "@/lib/types";
import ConnectionsPage from "./page";

const searchParams = vi.hoisted(() => ({ value: "" }));
vi.mock("next/navigation", () => ({
  useSearchParams: () => new URLSearchParams(searchParams.value),
}));

const api = vi.hoisted(() => ({
  getConnections: vi.fn(),
  putJiraConnection: vi.fn(),
  putNotionConnection: vi.fn(),
  deleteConnection: vi.fn(),
  putConnectionsMode: vi.fn(),
  gmailConnectUrl: vi.fn(() => "http://localhost:8080/api/connections/gmail/connect"),
}));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    getConnections: api.getConnections,
    putJiraConnection: api.putJiraConnection,
    putNotionConnection: api.putNotionConnection,
    deleteConnection: api.deleteConnection,
    putConnectionsMode: api.putConnectionsMode,
    gmailConnectUrl: api.gmailConnectUrl,
  };
});

// demoInfo is a fresh user: nothing connected, demo workspace.
const demoInfo = (): ConnectionsInfo => ({
  mode: "demo",
  use_demo_workspace: false,
  sources: {
    jira: { status: "absent" },
    notion: { status: "absent" },
    gmail: { status: "absent" },
  },
});

// jiraConnectedInfo is a user who pasted a working Jira token.
const jiraConnectedInfo = (): ConnectionsInfo => ({
  mode: "user",
  use_demo_workspace: false,
  sources: {
    jira: {
      status: "connected",
      identity: {
        site_url: "https://satyam.atlassian.net",
        account_name: "Satyam Jha",
      },
      updated_at: "2026-08-30T10:00:00Z",
    },
    notion: { status: "absent" },
    gmail: { status: "absent" },
  },
});

// gmailErroredInfo is a user whose Gmail refresh token was revoked.
// gmailConnectedInfo is a user who just finished the Gmail OAuth callback.
const gmailConnectedInfo = (): ConnectionsInfo => ({
  mode: "user",
  use_demo_workspace: false,
  sources: {
    jira: { status: "absent" },
    notion: { status: "absent" },
    gmail: {
      status: "connected",
      identity: { email: "satyam@gmail.example" },
      updated_at: "2026-08-30T10:00:00Z",
    },
  },
});

const gmailErroredInfo = (): ConnectionsInfo => ({
  mode: "user",
  use_demo_workspace: false,
  sources: {
    jira: { status: "absent" },
    notion: { status: "absent" },
    gmail: {
      status: "error",
      identity: { email: "satyam@gmail.example" },
      last_error: "oauth invalid_grant: Token has been expired or revoked.",
      updated_at: "2026-08-30T10:00:00Z",
    },
  },
});

beforeEach(() => {
  api.getConnections.mockReset();
  api.putJiraConnection.mockReset();
  api.putNotionConnection.mockReset();
  api.deleteConnection.mockReset();
  api.putConnectionsMode.mockReset();
  searchParams.value = "";
});

afterEach(() => {
  cleanup();
});

describe("ConnectionsPage states", () => {
  it("shows a loading state until the overview answers", () => {
    api.getConnections.mockReturnValue(new Promise(() => {})); // never settles
    render(<ConnectionsPage />);
    expect(screen.getByText("Loading…")).toBeInTheDocument();
  });

  it("renders demo mode for a fresh user: demo banner, three cards, no toggle", async () => {
    api.getConnections.mockResolvedValue(demoInfo());
    render(<ConnectionsPage />);

    expect(await screen.findByText(/demo workspace/)).toBeInTheDocument();
    expect(
      screen.getByText(/connect a source below to query your own data/),
    ).toBeInTheDocument();
    // Nothing connected: the demo toggle would be meaningless and is hidden.
    expect(screen.queryByLabelText(/Use demo workspace/)).toBeNull();
    // The three cards.
    expect(screen.getByRole("heading", { name: "Jira" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Notion" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Gmail" })).toBeInTheDocument();
    // Gmail offers the first-time connect, not a reconnect.
    expect(
      screen.getByRole("button", { name: "Connect Gmail" }),
    ).toBeInTheDocument();
    // The help link points at Atlassian's token page.
    expect(screen.getByRole("link", { name: /id.atlassian.com/ })).toHaveAttribute(
      "href",
      expect.stringContaining("id.atlassian.com"),
    );
  });

  it("renders a connected Jira card with its identity and a disconnect", async () => {
    api.getConnections.mockResolvedValue(jiraConnectedInfo());
    render(<ConnectionsPage />);

    expect(await screen.findByText("Connected")).toBeInTheDocument();
    expect(
      screen.getByText(/https:\/\/satyam\.atlassian\.net · Satyam Jha/),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/your connected sources/),
    ).toBeInTheDocument();
    // With a connection, the demo toggle appears, unchecked.
    expect(screen.getByLabelText(/Use demo workspace/)).not.toBeChecked();

    api.deleteConnection.mockResolvedValue(undefined);
    api.getConnections.mockResolvedValue(demoInfo());
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "Disconnect" }));

    await waitFor(() =>
      expect(api.deleteConnection).toHaveBeenCalledWith("jira"),
    );
    // The reload shows the demo state again.
    expect(
      await screen.findByText(/connect a source below to query your own data/),
    ).toBeInTheDocument();
  });

  it("renders an errored Gmail connection as needs-reconnecting with the reason and a Reconnect button", async () => {
    api.getConnections.mockResolvedValue(gmailErroredInfo());
    render(<ConnectionsPage />);

    expect(await screen.findByText("Needs reconnecting")).toBeInTheDocument();
    expect(
      screen.getByText(/Token has been expired or revoked/),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Reconnect Gmail" }),
    ).toBeInTheDocument();
    // An errored connection still counts as a connection: the page stays in
    // user mode, never a silent demo swap.
    expect(screen.getByText(/your connected sources/)).toBeInTheDocument();
  });

  it("flips the demo toggle through the API and renders the returned overview", async () => {
    api.getConnections.mockResolvedValue(jiraConnectedInfo());
    api.putConnectionsMode.mockResolvedValue({
      ...jiraConnectedInfo(),
      mode: "demo",
      use_demo_workspace: true,
    });
    const user = userEvent.setup();
    render(<ConnectionsPage />);

    await user.click(await screen.findByLabelText(/Use demo workspace/));

    await waitFor(() =>
      expect(api.putConnectionsMode).toHaveBeenCalledWith(true),
    );
    expect(await screen.findByText(/the demo toggle is on/)).toBeInTheDocument();
    expect(screen.getByLabelText(/Use demo workspace/)).toBeChecked();
  });
});

describe("ConnectionsPage paste forms", () => {
  it("keeps the Jira submit disabled until all three fields are filled", async () => {
    api.getConnections.mockResolvedValue(demoInfo());
    const user = userEvent.setup();
    render(<ConnectionsPage />);
    await screen.findByRole("heading", { name: "Jira" });

    // Both paste forms submit with the same label; the Jira card renders first.
    const submit = screen.getAllByRole("button", {
      name: "Validate & connect",
    })[0];
    expect(submit).toBeDisabled();

    await user.type(screen.getByLabelText("Site URL"), "https://satyam.atlassian.net");
    await user.type(screen.getByLabelText("Email"), "satyam@example.com");
    expect(submit).toBeDisabled(); // token still missing
    await user.type(screen.getByLabelText("API token"), "jira-token");
    expect(submit).toBeEnabled();
  });

  it("submits the Jira form and shows a 422's provider reason inline", async () => {
    api.getConnections.mockResolvedValue(demoInfo());
    api.putJiraConnection.mockRejectedValue(
      new ApiRequestError(
        422,
        "Basic auth with password is not allowed on this instance.",
      ),
    );
    const user = userEvent.setup();
    render(<ConnectionsPage />);
    await screen.findByRole("heading", { name: "Jira" });

    await user.type(screen.getByLabelText("Site URL"), "https://satyam.atlassian.net");
    await user.type(screen.getByLabelText("Email"), "satyam@example.com");
    await user.type(screen.getByLabelText("API token"), "sk-bad");
    await user.click(
      screen.getAllByRole("button", { name: "Validate & connect" })[0],
    );

    await waitFor(() =>
      expect(api.putJiraConnection).toHaveBeenCalledWith(
        "https://satyam.atlassian.net",
        "satyam@example.com",
        "sk-bad",
      ),
    );
    // Atlassian's own words, inline in the card.
    expect(
      await screen.findByText(
        "Basic auth with password is not allowed on this instance.",
      ),
    ).toBeInTheDocument();
    // Nothing pretends to be connected.
    expect(screen.queryByText("Connected")).toBeNull();
  });

  it("connects Jira on success, clears the token field, and reloads the overview", async () => {
    api.getConnections.mockResolvedValueOnce(demoInfo());
    api.putJiraConnection.mockResolvedValue({
      status: "connected",
      identity: {
        site_url: "https://satyam.atlassian.net",
        account_name: "Satyam Jha",
      },
    });
    api.getConnections.mockResolvedValueOnce(jiraConnectedInfo());
    const user = userEvent.setup();
    render(<ConnectionsPage />);
    await screen.findByRole("heading", { name: "Jira" });

    await user.type(screen.getByLabelText("Site URL"), "https://satyam.atlassian.net");
    await user.type(screen.getByLabelText("Email"), "satyam@example.com");
    await user.type(screen.getByLabelText("API token"), "jira-token-42");
    await user.click(
      screen.getAllByRole("button", { name: "Validate & connect" })[0],
    );

    expect(await screen.findByText("Connected")).toBeInTheDocument();
    expect(
      screen.getByText(/https:\/\/satyam\.atlassian\.net · Satyam Jha/),
    ).toBeInTheDocument();
    // The pasted credential does not linger in the form.
    expect(screen.getByLabelText("API token")).toHaveValue("");
  });

  it("shows a Notion 422's provider reason inline", async () => {
    api.getConnections.mockResolvedValue(demoInfo());
    api.putNotionConnection.mockRejectedValue(
      new ApiRequestError(422, "API token is invalid."),
    );
    const user = userEvent.setup();
    render(<ConnectionsPage />);
    await screen.findByRole("heading", { name: "Notion" });

    const notionSubmit = screen
      .getAllByRole("button", { name: "Validate & connect" })
      .at(-1)!;
    expect(notionSubmit).toBeDisabled();
    await user.type(screen.getByLabelText("Integration token"), "ntn_bad");
    expect(notionSubmit).toBeEnabled();
    await user.click(notionSubmit);

    await waitFor(() =>
      expect(api.putNotionConnection).toHaveBeenCalledWith("ntn_bad"),
    );
    expect(await screen.findByText("API token is invalid.")).toBeInTheDocument();
  });
});

describe("ConnectionsPage Gmail callback banner", () => {
  it("shows the success banner after ?connected=gmail", async () => {
    searchParams.value = "connected=gmail";
    // After a successful callback the API reports gmail connected — the
    // banner is gated on that live state, so a disconnect on the same page
    // cannot leave a stale "connected" banner contradicting the card.
    api.getConnections.mockResolvedValue(gmailConnectedInfo());
    render(<ConnectionsPage />);
    expect(await screen.findByText("Gmail connected.")).toBeInTheDocument();
  });

  it("suppresses the success banner when gmail is no longer connected", async () => {
    searchParams.value = "connected=gmail";
    api.getConnections.mockResolvedValue(demoInfo());
    render(<ConnectionsPage />);
    await screen.findByText("Connect Gmail");
    expect(screen.queryByText("Gmail connected.")).not.toBeInTheDocument();
  });

  it("explains a declined consent after ?error=access_denied", async () => {
    searchParams.value = "error=access_denied";
    api.getConnections.mockResolvedValue(demoInfo());
    render(<ConnectionsPage />);
    expect(
      await screen.findByText(/Google access was declined/),
    ).toBeInTheDocument();
  });
});
