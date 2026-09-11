// Turning an action payload into words a person can judge.
//
// The whole approval gate rests on one thing: that the person clicking approve
// understood what would happen. So nothing here summarizes a payload in place of
// showing it — the fields are always rendered in full alongside this copy. What
// these helpers produce is the consequence sentence, which is the part a summary
// genuinely helps with: "sends an email from you@example.com to 3 people" is
// something you can check at a glance, in a way that a rendered field list is
// not.

import type {
  ActionSource,
  ActionStatus,
  AddCommentPayload,
  AgentAction,
  AppendToPagePayload,
  CreateIssuePayload,
  CreatePagePayload,
  SendEmailPayload,
  TransitionIssuePayload,
  UpdateIssuePayload,
} from "@/lib/types";

// sourceLabel names the system a write targets.
export function sourceLabel(source: ActionSource): string {
  switch (source) {
    case "gmail":
      return "Gmail";
    case "jira":
      return "Jira";
    case "notion":
      return "Notion";
  }
}

// actionLabel names the operation, for a card heading.
export function actionLabel(action: string): string {
  switch (action) {
    case "gmail.send":
      return "Send an email";
    case "jira.create_issue":
      return "Create a Jira issue";
    case "jira.update_issue":
      return "Change a Jira issue";
    case "jira.add_comment":
      return "Comment on a Jira issue";
    case "jira.transition_issue":
      return "Move a Jira issue";
    case "notion.append_to_page":
      return "Add to a Notion page";
    case "notion.create_page":
      return "Create a Notion page";
    default:
      return action;
  }
}

// consequence is the sentence above the approve button.
//
// It names the irreversible part, in the second person, with the real numbers
// in it. "Approve action" tells a person nothing; "This will send an email from
// you@example.com to 3 recipients" is a thing they can agree or disagree with.
export function consequence(action: string, payload: unknown): string {
  switch (action) {
    case "gmail.send": {
      const p = payload as SendEmailPayload;
      const count = (p.to?.length ?? 0) + (p.cc?.length ?? 0);
      const who =
        count === 1 ? (p.to?.[0] ?? "1 recipient") : `${count} recipients`;
      const from = p.from?.trim() ? p.from : "this mailbox";
      return `This will send an email from ${from} to ${who}. It cannot be unsent.`;
    }
    case "jira.create_issue": {
      const p = payload as CreateIssuePayload;
      const where = p.project_name?.trim()
        ? `${p.project_key} (${p.project_name})`
        : p.project_key;
      return `This will create a ${p.issue_type} in ${where}, visible to everyone with access to that project.`;
    }
    case "jira.update_issue": {
      const p = payload as UpdateIssuePayload;
      const changed = changedFields(p).join(", ");
      return `This will change ${changed} on ${p.key}, replacing the current values.`;
    }
    case "jira.add_comment": {
      const p = payload as AddCommentPayload;
      return `This will post a comment on ${p.key} under your own account, visible to everyone on the issue.`;
    }
    case "jira.transition_issue": {
      const p = payload as TransitionIssuePayload;
      const from = p.from_status?.trim() ? ` from ${p.from_status}` : "";
      return `This will move ${p.key}${from} to ${p.to_status}, which may notify people watching it.`;
    }
    case "notion.append_to_page": {
      const p = payload as AppendToPagePayload;
      const where = p.page_title?.trim() ? `“${p.page_title}”` : "the page";
      return `This will add content to the end of ${where}. Existing content is not touched.`;
    }
    case "notion.create_page": {
      const p = payload as CreatePagePayload;
      const where = p.parent_title?.trim()
        ? `under “${p.parent_title}”`
        : "in Notion";
      return `This will create the page “${p.title}” ${where}.`;
    }
    default:
      return "This will be carried out as written below.";
  }
}

// changedFields lists the fields a Jira update touches, in a stable order.
export function changedFields(payload: UpdateIssuePayload): string[] {
  const fields = payload.fields ?? {};
  const names: string[] = [];
  if (fields.summary !== undefined) names.push("summary");
  if (fields.description !== undefined) names.push("description");
  if (fields.due_date !== undefined) names.push("due date");
  if (fields.labels !== undefined) names.push("labels");
  return names.length > 0 ? names : ["nothing"];
}

// bodyField names the payload key holding the long-form text a person is most
// likely to want to edit before approving, and null when there is none.
//
// Only one field per action is editable, deliberately. Editing a recipient list
// or an issue key in a textarea is how a careful review turns into a
// malformed payload; the wording is the part a human actually improves, and
// anything structural is better rejected with a reason so the agent proposes it
// properly.
export function bodyField(action: string): { key: string; label: string } | null {
  switch (action) {
    case "gmail.send":
      return { key: "body", label: "Body" };
    case "jira.create_issue":
      return { key: "description", label: "Description" };
    case "jira.add_comment":
      return { key: "body", label: "Comment" };
    case "notion.append_to_page":
    case "notion.create_page":
      return { key: "markdown", label: "Content" };
    default:
      return null;
  }
}

// statusLabel and statusVariant render a lifecycle status for a badge.
export function statusLabel(status: ActionStatus): string {
  switch (status) {
    case "pending":
      return "Awaiting your decision";
    case "approved":
      return "Approved — carrying out";
    case "executing":
      return "In progress";
    case "executed":
      return "Done";
    case "rejected":
      return "Declined";
    case "failed":
      return "Failed";
    case "expired":
      return "Expired undecided";
  }
}

export function statusVariant(
  status: ActionStatus,
): "default" | "secondary" | "destructive" | "outline" {
  switch (status) {
    case "executed":
      return "default";
    case "failed":
      return "destructive";
    case "rejected":
    case "expired":
      return "outline";
    default:
      return "secondary";
  }
}

// resultLink pulls a URL out of an executed action's result, so the audit view
// can link to the thing that was created.
export function resultLink(action: AgentAction): string | null {
  const url = action.result?.detail?.["url"];
  return typeof url === "string" && url.startsWith("http") ? url : null;
}
