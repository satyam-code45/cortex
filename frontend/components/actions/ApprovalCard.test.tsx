// The approval card.
//
// The card is the whole gate as far as a person is concerned, so the tests are
// about what they see and what their click sends: the payload rendered field by
// field rather than summarized, an edit that becomes the payload which executes,
// and a decline that cannot be submitted without a reason — the reason is the
// only thing the agent has to work with when it resumes.
//
// The API module is mocked (the network contract is tested in Go); ApiRequestError
// stays real so the card's instanceof check runs.

import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { ApiRequestError } from "@/lib/api";
import type { AgentAction } from "@/lib/types";
import { ApprovalCard } from "./ApprovalCard";

const api = vi.hoisted(() => ({
  approveAction: vi.fn(),
  rejectAction: vi.fn(),
}));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    approveAction: api.approveAction,
    rejectAction: api.rejectAction,
  };
});

afterEach(() => {
  cleanup();
  api.approveAction.mockReset();
  api.rejectAction.mockReset();
});

// pendingAction builds one decidable row.
function pendingAction(overrides: Partial<AgentAction> = {}): AgentAction {
  return {
    id: "11111111-1111-4111-8111-111111111111",
    agent_run_id: "22222222-2222-4222-8222-222222222222",
    source: "gmail",
    action: "gmail.send",
    status: "pending",
    proposed_payload: {
      from: "satyam@example.com",
      to: ["ines.brandt@vendor.example", "ops@vendor.example"],
      cc: ["priya@example.com"],
      subject: "Sandbox notice received",
      body: "Hi Ines,\n\nWe received the sandbox notice. Thanks.",
    },
    proposed_at: "2026-09-11T09:00:00Z",
    edited: false,
    editable: true,
    ...overrides,
  };
}

describe("ApprovalCard", () => {
  it("renders every payload field, not a summary of them", () => {
    api.approveAction.mockResolvedValue(pendingAction());
    render(<ApprovalCard action={pendingAction()} onDecided={vi.fn()} />);

    // Who it is from, who it goes to, and the subject, verbatim.
    expect(screen.getByText("satyam@example.com")).toBeInTheDocument();
    expect(
      screen.getByText(/ines\.brandt@vendor\.example/),
    ).toBeInTheDocument();
    expect(screen.getByText(/ops@vendor\.example/)).toBeInTheDocument();
    expect(screen.getByText(/priya@example\.com/)).toBeInTheDocument();
    expect(screen.getByText("Sandbox notice received")).toBeInTheDocument();

    // The body is editable in place, holding the proposed text.
    const body = screen.getByLabelText("Body") as HTMLTextAreaElement;
    expect(body.value).toBe(
      "Hi Ines,\n\nWe received the sandbox notice. Thanks.",
    );

    // The consequence is stated with the real numbers in it: "Approve action"
    // would tell a person nothing.
    expect(
      screen.getByText(/send an email from satyam@example\.com to 3 recipients/i),
    ).toBeInTheDocument();
    // And it is unambiguous that nothing has happened yet.
    expect(screen.getByText(/nothing has happened yet/i)).toBeInTheDocument();
  });

  it("approves the proposal unchanged by sending no payload at all", async () => {
    const user = userEvent.setup();
    const onDecided = vi.fn();
    api.approveAction.mockResolvedValue(pendingAction({ status: "approved" }));

    const action = pendingAction();
    render(<ApprovalCard action={action} onDecided={onDecided} />);

    await user.click(screen.getByRole("button", { name: "Approve" }));

    await waitFor(() => expect(onDecided).toHaveBeenCalledTimes(1));
    expect(api.approveAction).toHaveBeenCalledTimes(1);
    // undefined, not a re-serialized copy: a round trip through a JSON
    // serializer is how a payload gets subtly altered by accident.
    expect(api.approveAction).toHaveBeenCalledWith(action.id, undefined);
  });

  it("sends the edited body when a person changes it before approving", async () => {
    const user = userEvent.setup();
    const onDecided = vi.fn();
    api.approveAction.mockResolvedValue(pendingAction({ status: "approved" }));

    const action = pendingAction();
    render(<ApprovalCard action={action} onDecided={onDecided} />);

    const body = screen.getByLabelText(/Body/);
    await user.clear(body);
    await user.type(body, "Hi Ines, received - certification is in hand.");

    // The card says plainly that the edit is what will happen.
    expect(
      screen.getByRole("button", { name: "Approve edited version" }),
    ).toBeInTheDocument();
    await user.click(
      screen.getByRole("button", { name: "Approve edited version" }),
    );

    await waitFor(() => expect(onDecided).toHaveBeenCalledTimes(1));
    expect(api.approveAction).toHaveBeenCalledTimes(1);
    const [id, payload] = api.approveAction.mock.calls[0];
    expect(id).toBe(action.id);
    // The edited body is what executes, and every other field is carried over
    // untouched.
    expect(payload).toEqual({
      ...(action.proposed_payload as Record<string, unknown>),
      body: "Hi Ines, received - certification is in hand.",
    });
  });

  it("refuses to decline without a reason, then sends the reason given", async () => {
    const user = userEvent.setup();
    const onDecided = vi.fn();
    api.rejectAction.mockResolvedValue(pendingAction({ status: "rejected" }));

    const action = pendingAction();
    render(<ApprovalCard action={action} onDecided={onDecided} />);

    await user.click(screen.getByRole("button", { name: /Decline/ }));
    await user.click(screen.getByRole("button", { name: "Confirm decline" }));

    expect(api.rejectAction).not.toHaveBeenCalled();
    expect(onDecided).not.toHaveBeenCalled();
    expect(screen.getByText(/Say why/i)).toBeInTheDocument();

    await user.type(
      screen.getByLabelText(/Why are you declining/i),
      "  we already emailed her yesterday  ",
    );
    await user.click(screen.getByRole("button", { name: "Confirm decline" }));

    await waitFor(() => expect(onDecided).toHaveBeenCalledTimes(1));
    expect(api.rejectAction).toHaveBeenCalledWith(
      action.id,
      "we already emailed her yesterday",
    );
    // Declining never executes anything.
    expect(api.approveAction).not.toHaveBeenCalled();
  });

  it("keeps the card on screen and says why when a decision is refused", async () => {
    const user = userEvent.setup();
    const onDecided = vi.fn();
    api.approveAction.mockRejectedValue(
      new ApiRequestError(
        409,
        "this request expired before it was decided, so it can no longer be carried out",
      ),
    );

    render(<ApprovalCard action={pendingAction()} onDecided={vi.fn()} />);
    await user.click(screen.getByRole("button", { name: "Approve" }));

    await waitFor(() =>
      expect(screen.getByText(/expired before it was decided/i)).toBeInTheDocument(),
    );
    expect(onDecided).not.toHaveBeenCalled();
    // Still decidable: the buttons are back, not stuck mid-flight.
    expect(screen.getByRole("button", { name: "Approve" })).toBeEnabled();
  });

  it("renders a Jira proposal field by field with its own editable body", () => {
    const action = pendingAction({
      source: "jira",
      action: "jira.create_issue",
      proposed_payload: {
        project_key: "ATLAS",
        project_name: "Atlas",
        issue_type: "Task",
        summary: "Track sandbox certification",
        description: "Raised from ATLAS-102.",
        labels: ["compliance"],
        due_date: "2026-10-01",
      },
    });
    render(<ApprovalCard action={action} onDecided={vi.fn()} />);

    expect(screen.getByText("ATLAS — Atlas")).toBeInTheDocument();
    expect(screen.getByText("Task")).toBeInTheDocument();
    expect(screen.getByText("Track sandbox certification")).toBeInTheDocument();
    expect(screen.getByText("2026-10-01")).toBeInTheDocument();
    expect(
      screen.getByText(/create a Task in ATLAS \(Atlas\)/i),
    ).toBeInTheDocument();

    const description = screen.getByLabelText(
      /Description/,
    ) as HTMLTextAreaElement;
    expect(description.value).toBe("Raised from ATLAS-102.");
  });

  it("renders a proposal it has no dedicated view for as complete JSON", () => {
    const action = pendingAction({
      source: "notion",
      action: "notion.something_new",
      proposed_payload: { page_id: "abc", detail: "a field nobody wrote a renderer for" },
    });
    render(<ApprovalCard action={action} onDecided={vi.fn()} />);

    // Ugly but complete beats a blank card: a person must still be able to
    // review a write whose renderer has not shipped.
    expect(
      screen.getByText(/a field nobody wrote a renderer for/),
    ).toBeInTheDocument();
  });
});
