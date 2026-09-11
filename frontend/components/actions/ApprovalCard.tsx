"use client";

// The approval card: the one place a human stands between the agent and a write.
//
// Three decisions are offered, and the middle one is the interesting one:
// approve, approve after editing, or decline with a reason. Editing matters
// because the common case is not "this is wrong" but "this is nearly right" —
// the agent found the facts and drafted the wording, and a person adjusts a
// sentence. Forcing that into reject-and-retry would waste the investigation
// and, worse, train the person to approve things they would have changed.
//
// Only the long-form text is editable. Recipients, issue keys and statuses are
// shown but fixed: they were validated against the live system when the proposal
// was made, and a hand-edited recipient list has never been through that check.
// Something structurally wrong is better declined with a reason, which the agent
// is told and can act on.

import { useState } from "react";

import {
  actionLabel,
  bodyField,
  consequence,
  sourceLabel,
} from "@/components/actions/actionCopy";
import { PayloadFields } from "@/components/actions/PayloadFields";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ApiRequestError, approveAction, rejectAction } from "@/lib/api";
import type { AgentAction } from "@/lib/types";

export function ApprovalCard({
  action,
  onDecided,
}: {
  action: AgentAction;
  // Called after a decision lands, so the owner can refetch the actions and
  // reopen the run's event stream.
  onDecided: () => void;
}) {
  const body = bodyField(action.action);
  const proposed = action.proposed_payload as Record<string, unknown>;
  const originalText = body ? String(proposed[body.key] ?? "") : "";

  const [draft, setDraft] = useState(originalText);
  const [rejecting, setRejecting] = useState(false);
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState<"approve" | "reject" | null>(null);
  const [error, setError] = useState<string | null>(null);

  const edited = body !== null && draft !== originalText;

  async function approve() {
    setBusy("approve");
    setError(null);
    try {
      // Only send a payload when something actually changed. An unedited
      // approval sends nothing, so the backend approves the stored proposal
      // byte for byte — a round trip through a JSON serializer is how a payload
      // gets subtly altered by accident, and the row records whether a person
      // edited it.
      await approveAction(
        action.id,
        edited && body ? { ...proposed, [body.key]: draft } : undefined,
      );
      onDecided();
    } catch (err) {
      setBusy(null);
      setError(
        err instanceof ApiRequestError ? err.message : "could not approve",
      );
    }
  }

  async function reject() {
    if (!reason.trim()) {
      setError("Say why — the agent is told your reason, so it can answer without this.");
      return;
    }
    setBusy("reject");
    setError(null);
    try {
      await rejectAction(action.id, reason.trim());
      onDecided();
    } catch (err) {
      setBusy(null);
      setError(
        err instanceof ApiRequestError ? err.message : "could not decline",
      );
    }
  }

  return (
    <section className="rounded-lg border border-amber-500/40 bg-amber-500/5 p-4">
      <header className="flex flex-wrap items-center gap-2">
        <Badge variant="outline">{sourceLabel(action.source)}</Badge>
        <h3 className="text-sm font-semibold">{actionLabel(action.action)}</h3>
        <span className="ml-auto text-xs text-muted-foreground">
          Nothing has happened yet
        </span>
      </header>

      <p className="mt-2 text-sm font-medium">
        {consequence(action.action, proposed)}
      </p>

      <div className="mt-3 border-t pt-3">
        {/* The payload in full. When there is an editable body it is rendered
            below as a textarea instead, so the same text never appears twice. */}
        <PayloadFields
          action={action.action}
          payload={body ? { ...proposed, [body.key]: "" } : proposed}
        />

        {body && (
          <div className="mt-3 space-y-1">
            <label
              htmlFor={`body-${action.id}`}
              className="text-xs font-medium tracking-wide text-muted-foreground uppercase"
            >
              {body.label}
              {edited && (
                <span className="ml-2 normal-case text-amber-600">edited</span>
              )}
            </label>
            <textarea
              id={`body-${action.id}`}
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              rows={Math.min(18, Math.max(5, draft.split("\n").length + 1))}
              className="w-full rounded-md border bg-background p-2 font-sans text-sm outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50"
            />
            {edited && (
              <p className="text-xs text-muted-foreground">
                Your edited version is what will be carried out, not what the
                agent proposed. Both are kept in the audit trail.
              </p>
            )}
          </div>
        )}
      </div>

      {rejecting && (
        <div className="mt-3 space-y-1">
          <label
            htmlFor={`reason-${action.id}`}
            className="text-xs font-medium tracking-wide text-muted-foreground uppercase"
          >
            Why are you declining?
          </label>
          <textarea
            id={`reason-${action.id}`}
            aria-describedby={error ? `error-${action.id}` : undefined}
            autoFocus
            value={reason}
            onChange={(e) => setReason(e.target.value)}
            rows={2}
            placeholder="e.g. we already emailed her yesterday"
            className="w-full rounded-md border bg-background p-2 text-sm outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50"
          />
          <p className="text-xs text-muted-foreground">
            The agent is told your reason and continues its answer with it, so a
            specific one gets you a better answer.
          </p>
        </div>
      )}

      {/* role="alert" so the validation message ("Say why…") is announced:
          without it a screen-reader user clicking Confirm decline on an empty
          reason gets no feedback at all — the button simply appears inert. */}
      {error && (
        <p
          id={`error-${action.id}`}
          role="alert"
          className="mt-2 text-sm text-destructive"
        >
          {error}
        </p>
      )}

      <footer className="mt-3 flex flex-wrap gap-2">
        <Button onClick={approve} disabled={busy !== null}>
          {busy === "approve"
            ? "Carrying out…"
            : edited
              ? "Approve edited version"
              : "Approve"}
        </Button>
        {rejecting ? (
          <>
            <Button
              variant="destructive"
              onClick={reject}
              disabled={busy !== null}
            >
              {busy === "reject" ? "Declining…" : "Confirm decline"}
            </Button>
            <Button
              variant="ghost"
              onClick={() => {
                setRejecting(false);
                setError(null);
              }}
              disabled={busy !== null}
            >
              Back
            </Button>
          </>
        ) : (
          <Button
            variant="outline"
            onClick={() => setRejecting(true)}
            disabled={busy !== null}
          >
            Decline…
          </Button>
        )}
      </footer>
    </section>
  );
}
