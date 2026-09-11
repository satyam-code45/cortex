"use client";

// The Actions page: every write Cortex has been asked to perform, and what
// happened to it.
//
// Read-only, and that is the design rather than a missing feature. An executed
// write cannot be undone from here or anywhere else — a sent email is sent — so
// the recourse for a mistake is knowing exactly what happened, who authorized
// it, and when. That is what this page is for, which is also why it shows the
// proposal beside the approved version: the question an auditor asks first is
// whether a person changed it before saying yes.

import { useCallback, useEffect, useState } from "react";

import { PayloadFields } from "@/components/actions/PayloadFields";
import {
  actionLabel,
  resultLink,
  sourceLabel,
  statusLabel,
  statusVariant,
} from "@/components/actions/actionCopy";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ApiRequestError, listActions } from "@/lib/api";
import { humanizeDate } from "@/lib/humanize";
import type { ActionStatus, AgentAction } from "@/lib/types";

// decisionLabel names what happened to an action, for the row's decided-at
// timestamp.
//
// Expiry stamps decided_at without anybody having decided anything, so it needs
// its own word. A failed action is different and stays "Approved": a person did
// approve it, and the failure is shown separately — the approval is still the
// true thing about that timestamp.
function decisionLabel(status: ActionStatus): string {
  switch (status) {
    case "rejected":
      return "Declined";
    case "expired":
      return "Expired";
    default:
      return "Approved";
  }
}

export default function ActionsPage() {
  const [actions, setActions] = useState<AgentAction[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [expanded, setExpanded] = useState<string | null>(null);

  const load = useCallback(() => {
    listActions().then(
      (list) => {
        setActions(list);
        setError(null);
      },
      (err: unknown) => {
        // An empty list and a failed request must not look the same: "no
        // actions yet" is a claim about the audit trail, and making it while
        // the request failed would be affirmatively wrong.
        setActions([]);
        setError(
          err instanceof ApiRequestError
            ? `could not load the audit trail: ${err.message}`
            : "could not load the audit trail",
        );
      },
    );
  }, []);

  useEffect(load, [load]);

  return (
    <div className="mx-auto w-full max-w-3xl px-6 py-8">
      <header className="flex items-start gap-4">
        <div>
          <h1 className="text-lg font-semibold tracking-tight">Actions</h1>
          <p className="mt-1 max-w-xl text-sm text-muted-foreground">
            Every write the agent has proposed, and what you decided. Nothing
            here happened without an approval, and nothing here can be undone —
            this is the record.
          </p>
        </div>
        <Button variant="outline" onClick={load} className="ml-auto">
          Refresh
        </Button>
      </header>

      {error && <p className="mt-6 text-sm text-destructive">{error}</p>}

      {actions === null && (
        <p className="mt-6 text-sm text-muted-foreground">Loading…</p>
      )}

      {actions !== null && actions.length === 0 && !error && (
        <p className="mt-6 text-sm text-muted-foreground">
          No actions yet. When you ask the agent to send an email or file an
          issue, the request appears in the chat for you to approve — and lands
          here afterwards.
        </p>
      )}

      <ul className="mt-6 space-y-3">
        {(actions ?? []).map((action) => {
          const open = expanded === action.id;
          const link = resultLink(action);
          return (
            <li key={action.id} className="rounded-lg border p-4">
              <div className="flex flex-wrap items-center gap-2">
                <Badge variant="outline">{sourceLabel(action.source)}</Badge>
                <span className="text-sm font-medium">
                  {actionLabel(action.action)}
                </span>
                <Badge variant={statusVariant(action.status)}>
                  {statusLabel(action.status)}
                </Badge>
                {action.edited && (
                  <Badge variant="secondary">edited before approval</Badge>
                )}
                <Button
                  variant="ghost"
                  size="sm"
                  className="ml-auto"
                  aria-expanded={open}
                  aria-controls={`payload-${action.id}`}
                  onClick={() => setExpanded(open ? null : action.id)}
                >
                  {open ? "Hide payload" : "Show payload"}
                </Button>
              </div>

              <dl className="mt-2 grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs text-muted-foreground">
                <dt>Proposed</dt>
                <dd>{humanizeDate(action.proposed_at)}</dd>
                {action.decided_at && (
                  <>
                    {/* Not every decided_at is a decision somebody made:
                        expiry stamps it too, with no decider. On a page whose
                        whole purpose is who authorized what, labelling that
                        "Approved by you" would be the audit trail asserting
                        something false. */}
                    <dt>{decisionLabel(action.status)}</dt>
                    {/* When a person did decide it, that person is always the
                        run's owner — only they can decide their own actions —
                        so "you" is more useful than the id the API returns. */}
                    <dd>
                      {humanizeDate(action.decided_at)}
                      {action.decided_by ? " by you" : ""}
                    </dd>
                  </>
                )}
                {action.executed_at && (
                  <>
                    <dt>Carried out</dt>
                    <dd>{humanizeDate(action.executed_at)}</dd>
                  </>
                )}
              </dl>

              {action.result?.summary && (
                <p className="mt-2 text-sm">
                  {action.result.summary}
                  {link && (
                    <>
                      {" "}
                      <a
                        href={link}
                        target="_blank"
                        rel="noreferrer"
                        className="text-primary underline"
                      >
                        open it
                      </a>
                    </>
                  )}
                </p>
              )}
              {action.reject_reason && (
                <p className="mt-2 text-sm">
                  <span className="text-muted-foreground">Your reason: </span>
                  {action.reject_reason}
                </p>
              )}
              {action.error && (
                <p className="mt-2 text-sm text-destructive">{action.error}</p>
              )}

              {open && (
                <div
                  id={`payload-${action.id}`}
                  className="mt-3 space-y-4 border-t pt-3"
                >
                  <div>
                    <p className="mb-2 text-xs font-medium tracking-wide text-muted-foreground uppercase">
                      What the agent proposed
                    </p>
                    <PayloadFields
                      action={action.action}
                      payload={action.proposed_payload}
                    />
                  </div>
                  {/* The approved version appears only when it differs. Showing
                      two identical payloads side by side would bury the one case
                      that matters. */}
                  {action.edited && action.final_payload !== undefined && (
                    <div>
                      <p className="mb-2 text-xs font-medium tracking-wide text-muted-foreground uppercase">
                        What you approved
                      </p>
                      <PayloadFields
                        action={action.action}
                        payload={action.final_payload}
                      />
                    </div>
                  )}
                </div>
              )}
            </li>
          );
        })}
      </ul>
    </div>
  );
}
