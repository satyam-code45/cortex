"use client";

// The payload, rendered field by field.
//
// This component is the substance of the approval gate. A person is being asked
// to authorize something irreversible, so they see the actual request — every
// recipient, the whole subject line, the full body — not a description of it.
// Approving a summary is not approving the write, and a UI that only showed a
// summary would make the gate ceremonial.
//
// An unrecognized action falls back to formatted JSON rather than to nothing.
// A new write tool that shipped without a renderer here must still be reviewable,
// and raw JSON is ugly but complete; a blank card would be worse than ugly.

import type {
  AddCommentPayload,
  AppendToPagePayload,
  CreateIssuePayload,
  CreatePagePayload,
  SendEmailPayload,
  TransitionIssuePayload,
  UpdateIssuePayload,
} from "@/lib/types";

export function PayloadFields({
  action,
  payload,
}: {
  action: string;
  payload: unknown;
}) {
  switch (action) {
    case "gmail.send":
      return <SendEmailFields payload={payload as SendEmailPayload} />;
    case "jira.create_issue":
      return <CreateIssueFields payload={payload as CreateIssuePayload} />;
    case "jira.update_issue":
      return <UpdateIssueFields payload={payload as UpdateIssuePayload} />;
    case "jira.add_comment":
      return <AddCommentFields payload={payload as AddCommentPayload} />;
    case "jira.transition_issue":
      return (
        <TransitionIssueFields payload={payload as TransitionIssuePayload} />
      );
    case "notion.append_to_page":
      return <AppendToPageFields payload={payload as AppendToPagePayload} />;
    case "notion.create_page":
      return <CreatePageFields payload={payload as CreatePagePayload} />;
    default:
      return <RawPayload payload={payload} />;
  }
}

// Field is one labelled value. Long text keeps its line breaks — an email body
// reflowed into a paragraph is not the email that will be sent.
function Field({
  label,
  value,
  mono = false,
  block = false,
}: {
  label: string;
  value: string;
  mono?: boolean;
  block?: boolean;
}) {
  if (!value.trim()) return null;
  return (
    <div className={block ? "space-y-1" : "flex gap-2 text-sm"}>
      <dt className="shrink-0 text-xs font-medium tracking-wide text-muted-foreground uppercase">
        {label}
      </dt>
      <dd
        className={[
          "min-w-0 text-sm break-words",
          mono ? "font-mono text-xs" : "",
          block ? "whitespace-pre-wrap" : "",
        ]
          .filter(Boolean)
          .join(" ")}
      >
        {value}
      </dd>
    </div>
  );
}

function SendEmailFields({ payload }: { payload: SendEmailPayload }) {
  return (
    <dl className="space-y-2">
      <Field label="From" value={payload.from ?? ""} />
      {/* Recipients one per line, not comma-joined: a list of six addresses on
          one wrapped line is exactly where a wrong address hides. */}
      <Field label="To" value={(payload.to ?? []).join("\n")} block />
      <Field label="Cc" value={(payload.cc ?? []).join("\n")} block />
      <Field label="Subject" value={payload.subject ?? ""} />
      {payload.in_reply_to?.trim() ? (
        <Field label="Replying to" value="an existing thread" />
      ) : null}
      <Field label="Body" value={payload.body ?? ""} block />
    </dl>
  );
}

function CreateIssueFields({ payload }: { payload: CreateIssuePayload }) {
  const project = payload.project_name?.trim()
    ? `${payload.project_key} — ${payload.project_name}`
    : payload.project_key;
  return (
    <dl className="space-y-2">
      <Field label="Project" value={project ?? ""} />
      <Field label="Type" value={payload.issue_type ?? ""} />
      <Field label="Summary" value={payload.summary ?? ""} />
      <Field label="Labels" value={(payload.labels ?? []).join(", ")} />
      <Field label="Due" value={payload.due_date ?? ""} />
      <Field label="Description" value={payload.description ?? ""} block />
    </dl>
  );
}

function UpdateIssueFields({ payload }: { payload: UpdateIssuePayload }) {
  const fields = payload.fields ?? {};
  const before = payload.before ?? {};
  // Before and after together, because the request is a change, not a value. A
  // card that showed only "due date: 2026-10-01" hides the thing worth
  // reviewing — that it used to say July.
  const rows: { label: string; key: string; next: string }[] = [];
  if (fields.summary !== undefined)
    rows.push({ label: "Summary", key: "summary", next: fields.summary });
  if (fields.description !== undefined)
    rows.push({
      label: "Description",
      key: "description",
      next: fields.description,
    });
  if (fields.due_date !== undefined)
    rows.push({
      label: "Due date",
      key: "due_date",
      next: fields.due_date === "" ? "(cleared)" : fields.due_date,
    });
  if (fields.labels !== undefined)
    rows.push({
      label: "Labels",
      key: "labels",
      next: fields.labels.join(", ") || "(cleared)",
    });

  return (
    <dl className="space-y-3">
      <Field label="Issue" value={payload.key ?? ""} mono />
      {rows.map((row) => (
        <div key={row.key} className="space-y-1">
          <dt className="text-xs font-medium tracking-wide text-muted-foreground uppercase">
            {row.label}
          </dt>
          {before[row.key] !== undefined && (
            <dd className="text-sm whitespace-pre-wrap text-muted-foreground line-through">
              {before[row.key] || "(empty)"}
            </dd>
          )}
          <dd className="text-sm whitespace-pre-wrap">{row.next}</dd>
        </div>
      ))}
    </dl>
  );
}

function AddCommentFields({ payload }: { payload: AddCommentPayload }) {
  const issue = payload.issue_summary?.trim()
    ? `${payload.key} — ${payload.issue_summary}`
    : payload.key;
  return (
    <dl className="space-y-2">
      <Field label="Issue" value={issue ?? ""} />
      <Field label="Comment" value={payload.body ?? ""} block />
    </dl>
  );
}

function TransitionIssueFields({
  payload,
}: {
  payload: TransitionIssuePayload;
}) {
  return (
    <dl className="space-y-2">
      <Field label="Issue" value={payload.key ?? ""} mono />
      <Field label="From" value={payload.from_status ?? "(current status)"} />
      <Field label="To" value={payload.to_status ?? ""} />
    </dl>
  );
}

function AppendToPageFields({ payload }: { payload: AppendToPagePayload }) {
  return (
    <dl className="space-y-2">
      <Field label="Page" value={payload.page_title ?? payload.page_id ?? ""} />
      <Field label="Content" value={payload.markdown ?? ""} block />
    </dl>
  );
}

function CreatePageFields({ payload }: { payload: CreatePagePayload }) {
  return (
    <dl className="space-y-2">
      <Field
        label="Under"
        value={payload.parent_title ?? payload.parent_page_id ?? ""}
      />
      <Field label="Title" value={payload.title ?? ""} />
      <Field label="Content" value={payload.markdown ?? ""} block />
    </dl>
  );
}

// RawPayload is the fallback for an action with no renderer yet.
export function RawPayload({ payload }: { payload: unknown }) {
  return (
    <pre className="overflow-x-auto rounded-md bg-muted p-3 font-mono text-xs">
      {JSON.stringify(payload, null, 2)}
    </pre>
  );
}
