"use client";

// The pending-approval queue for one paused run.
//
// It fetches its own actions rather than being handed them, which is a
// deliberate exception to the page owning cross-cutting state. The reason is
// that the authoritative record of what is pending is the agent_actions rows,
// not the event stream: a run can be paused for hours, decided from another tab,
// or revisited after a reload, and only the rows know the current status and
// whether a proposal has since expired. Rendering approve buttons from replayed
// events would offer decisions that no longer exist.
//
// The stream still drives WHEN to fetch — a paused stream is the trigger — and
// a decision refetches and asks the page to reopen the stream, so the resumed
// run's events flow again.

import { useCallback, useEffect, useState } from "react";

import { ApprovalCard } from "@/components/actions/ApprovalCard";
import { ApiRequestError, listActions } from "@/lib/api";
import type { AgentAction } from "@/lib/types";

export function ApprovalQueue({
  runId,
  paused,
  onDecided,
}: {
  runId: string;
  // paused: the run's stream has closed on run_paused. Refetching is keyed to
  // this so a running run makes no requests at all.
  paused: boolean;
  // onDecided asks the owner to reopen the run's event stream, so the resumed
  // run streams normally.
  onDecided: () => void;
}) {
  const [actions, setActions] = useState<AgentAction[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(() => {
    listActions().then(
      (all) => {
        setActions(all.filter((a) => a.agent_run_id === runId));
        setError(null);
      },
      (err: unknown) => {
        setActions([]);
        setError(
          err instanceof ApiRequestError
            ? `could not load the pending actions: ${err.message}`
            : "could not load the pending actions",
        );
      },
    );
  }, [runId]);

  useEffect(() => {
    if (paused) load();
  }, [paused, load]);

  const handleDecided = useCallback(() => {
    // Refetch before reopening: the card that was just decided must stop
    // offering buttons even if the resumed stream takes a moment to arrive.
    load();
    onDecided();
  }, [load, onDecided]);

  if (!paused) return null;

  const decidable = (actions ?? []).filter((a) => a.editable);

  if (error) {
    return <p className="px-4 py-3 text-sm text-destructive">{error}</p>;
  }
  if (actions === null) {
    return (
      <p className="px-4 py-3 text-sm text-muted-foreground">
        Loading what needs your decision…
      </p>
    );
  }
  if (decidable.length === 0) {
    // Paused with nothing decidable: every proposal has been decided and the
    // run is being resumed, or they expired. Saying so beats an empty gap where
    // the cards were.
    return (
      <p className="px-4 py-3 text-sm text-muted-foreground">
        Nothing left to decide — picking the investigation back up.
      </p>
    );
  }

  return (
    <div className="space-y-3 px-4 py-3">
      <p className="text-sm font-medium">
        {decidable.length === 1
          ? "The agent wants to do one thing before it answers. Nothing happens until you decide."
          : `The agent wants to do ${decidable.length} things before it answers. Nothing happens until you decide.`}
      </p>
      {decidable.map((action) => (
        <ApprovalCard
          key={action.id}
          action={action}
          onDecided={handleDecided}
        />
      ))}
    </div>
  );
}
