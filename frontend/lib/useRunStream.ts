"use client";

// useRunStream subscribes to a run's SSE stream and accumulates its events.
//
// Native EventSource, no library: auto-reconnect and Last-Event-ID resume are
// built into the browser, and the backend's `id: <seq>` lines are written for
// exactly that mechanism. The hook closes the stream itself on a terminal
// event — the transcript is complete then and the caller switches to the trace
// endpoint for anything further.

import { useEffect, useRef, useState } from "react";

import { runEventsUrl } from "./api";
import type {
  AnswerPayload,
  RunEvent,
  RunEventType,
  RunFailedPayload,
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
];

export interface RunStream {
  // Every event received so far, in seq order.
  events: RunEvent[];
  // The final answer markdown, once the answer event arrives.
  answer: string | null;
  // The failure message, once run_failed arrives.
  error: string | null;
  // terminal: run_finished or run_failed has been seen; the stream is closed.
  status: "connecting" | "streaming" | "terminal";
}

const initial: RunStream = {
  events: [],
  answer: null,
  error: null,
  status: "connecting",
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
  const [state, setState] = useState<{ key: string | null; stream: RunStream }>(
    { key: runId, stream: initial },
  );
  if (state.key !== runId) {
    setState({ key: runId, stream: initial });
  }

  // Seen seqs, so EventSource's replay-after-reconnect cannot duplicate cards.
  const seenRef = useRef<Set<number>>(new Set());
  // The latest onTerminal, without making it an effect dependency — the
  // subscription must not be torn down because a parent re-rendered.
  const onTerminalRef = useRef(onTerminal);
  useEffect(() => {
    onTerminalRef.current = onTerminal;
  }, [onTerminal]);

  useEffect(() => {
    if (!runId) return;

    seenRef.current = new Set();
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
      if (terminal) source.close();

      setState((prev) => {
        // A late event from a stream the UI has already moved away from.
        if (prev.key !== runId) return prev;
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
            status: terminal ? "terminal" : "streaming",
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
  }, [runId]);

  return state.key === runId ? state.stream : initial;
}

// insertBySeq appends an event, keeping seq order even if a reconnect replays
// rows out of order relative to what an earlier connection delivered.
function insertBySeq(events: RunEvent[], event: RunEvent): RunEvent[] {
  const next = [...events, event];
  next.sort((a, b) => a.seq - b.seq);
  return next;
}
