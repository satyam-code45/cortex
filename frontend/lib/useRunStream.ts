"use client";

// useRunStream subscribes to a run's SSE stream and accumulates its events.
//
// Native EventSource, no library: auto-reconnect and Last-Event-ID resume are
// built into the browser, and the backend's `id: <seq>` lines are written for
// exactly that mechanism. The hook closes the stream itself on a terminal
// event — the transcript is complete then and the caller switches to the trace
// endpoint for anything further.
//
// A paused run is the third state, and it is not terminal. When the agent
// proposes a write the backend closes the stream: nothing more will happen until
// a person approves or rejects, which can take hours, and holding a connection
// open for that would be absurd. So the hook surfaces status "paused" along with
// the actions being waited on, and exposes reopen() for the caller to call after
// posting a decision.
//
// Deduping by seq is what makes reopen() safe. A fresh EventSource cannot send
// Last-Event-ID (only the browser's own automatic reconnect does), so the
// reopened stream replays the run from the beginning — and seenRef, which
// deliberately survives a reopen, drops everything already rendered.

import { useCallback, useEffect, useRef, useState } from "react";

import { runEventsUrl } from "./api";
import type {
  AnswerPayload,
  PausedAction,
  RunEvent,
  RunEventType,
  RunFailedPayload,
  RunPausedPayload,
} from "./types";

const eventTypes: RunEventType[] = [
  "run_started",
  "llm_call",
  "tool_call_started",
  "tool_call_finished",
  "citations",
  "answer",
  "run_finished",
  "run_failed",
  "action_proposed",
  "run_paused",
  "action_decided",
  "action_executed",
  "action_failed",
  "run_resumed",
];

export interface RunStream {
  // Every event received so far, in seq order.
  events: RunEvent[];
  // The final answer markdown, once the answer event arrives.
  answer: string | null;
  // The failure message, once run_failed arrives.
  error: string | null;
  // paused: run_paused has been seen and the stream is closed, but the run is
  // not finished — it is waiting for a person. terminal: run_finished or
  // run_failed; the transcript is complete.
  status: "connecting" | "streaming" | "paused" | "terminal";
  // The actions a paused run is waiting on, from the run_paused payload. Empty
  // unless status is "paused".
  waiting: PausedAction[];
  // reopen starts a fresh stream, for use after posting a decision. Safe to
  // call at any time: the replayed events are deduped by seq.
  reopen: () => void;
}

const initial: Omit<RunStream, "reopen"> = {
  events: [],
  answer: null,
  error: null,
  status: "connecting",
  waiting: [],
};

export function useRunStream(
  runId: string | null,
  // Called once when the run reaches run_finished/run_failed — from the event
  // callback, so the handler may set state freely.
  onTerminal?: (runId: string) => void,
): RunStream {
  // State is keyed by the run it belongs to, and reset during render when the
  // key changes — the React "adjust state when props change" pattern — so no
  // effect ever writes state synchronously.
  const [state, setState] = useState<{
    key: string | null;
    stream: Omit<RunStream, "reopen">;
  }>({ key: runId, stream: initial });
  if (state.key !== runId) {
    setState({ key: runId, stream: initial });
  }

  // Bumped by reopen() to re-run the subscription effect. A counter rather than
  // a boolean so consecutive reopens each take effect.
  const [generation, setGeneration] = useState(0);
  const reopen = useCallback(() => setGeneration((n) => n + 1), []);

  // Seen seqs, so a replayed stream cannot duplicate cards — whether the replay
  // came from EventSource's automatic reconnect or from reopen().
  const seenRef = useRef<Set<number>>(new Set());
  // Reset only when the run changes, never on a reopen: a reopened stream
  // replays from seq 1, and forgetting what was already rendered is exactly how
  // the transcript would double.
  const runIdRef = useRef(runId);
  if (runIdRef.current !== runId) {
    runIdRef.current = runId;
    seenRef.current = new Set();
  }
  // The latest onTerminal, without making it an effect dependency — the
  // subscription must not be torn down because a parent re-rendered.
  const onTerminalRef = useRef(onTerminal);
  useEffect(() => {
    onTerminalRef.current = onTerminal;
  }, [onTerminal]);

  useEffect(() => {
    if (!runId) return;

    const source = new EventSource(runEventsUrl(runId), { withCredentials: true });

    const onEvent = (type: RunEventType) => (e: MessageEvent<string>) => {
      const seq = Number(e.lastEventId);
      if (!Number.isFinite(seq) || seenRef.current.has(seq)) return;
      seenRef.current.add(seq);

      let payload: RunEvent["payload"];
      try {
        payload = JSON.parse(e.data) as RunEvent["payload"];
      } catch {
        return; // A payload that does not parse cannot be rendered.
      }
      const event: RunEvent = { seq, type, payload };
      const terminal = type === "run_finished" || type === "run_failed";
      const paused = type === "run_paused";
      // The backend closes the stream on all three; closing this side too makes
      // that explicit rather than relying on the server hanging up.
      if (terminal || paused) source.close();

      setState((prev) => {
        // A late event from a stream the UI has already moved away from.
        if (prev.key !== runId) return prev;
        let status = prev.stream.status;
        if (terminal) status = "terminal";
        else if (paused) status = "paused";
        else if (status !== "terminal") status = "streaming";
        return {
          key: prev.key,
          stream: {
            events: insertBySeq(prev.stream.events, event),
            answer:
              type === "answer"
                ? (payload as AnswerPayload).answer
                : prev.stream.answer,
            error:
              type === "run_failed"
                ? (payload as RunFailedPayload).error
                : prev.stream.error,
            status,
            waiting: paused
              ? ((payload as RunPausedPayload).waiting ?? [])
              : // A resume clears the pause; anything else leaves it alone, so
                // the cards stay on screen while a decision is in flight.
                type === "run_resumed"
                ? []
                : prev.stream.waiting,
          },
        };
      });
      if (terminal) onTerminalRef.current?.(runId);
    };

    for (const type of eventTypes) {
      source.addEventListener(type, onEvent(type));
    }
    source.onopen = () => {
      setState((prev) =>
        prev.key === runId && prev.stream.status === "connecting"
          ? { key: prev.key, stream: { ...prev.stream, status: "streaming" } }
          : prev,
      );
    };
    // No onerror handler closing the stream: EventSource reconnects on its own
    // with Last-Event-ID, which is the behaviour we want for a dropped
    // connection mid-run. Terminal states close it above.

    return () => source.close();
  }, [runId, generation]);

  const stream = state.key === runId ? state.stream : initial;
  return { ...stream, reopen };
}

// insertBySeq appends an event, keeping seq order even if a reconnect replays
// rows out of order relative to what an earlier connection delivered.
function insertBySeq(events: RunEvent[], event: RunEvent): RunEvent[] {
  const next = [...events, event];
  next.sort((a, b) => a.seq - b.seq);
  return next;
}
