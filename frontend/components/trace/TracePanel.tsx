"use client";

// The live agent trace panel: what the agent is doing, as it does it.
//
// It renders from run events in both modes. Live, the events arrive over SSE;
// for a finished or re-opened run they come from the trace API's timeline,
// which stores the identical payloads (the SSE stream and GET /trace read the
// same run_events rows). One derivation, two feeds — the panel cannot drift
// between live and browsable.

import { Brain, Quote } from "lucide-react";
import { useMemo } from "react";

import { Separator } from "@/components/ui/separator";
import { ScrollArea } from "@/components/ui/scroll-area";
import {
  ToolCallCard,
  type ToolCallView,
  formatLatency,
} from "@/components/trace/ToolCallCard";
import type {
  CitationsPayload,
  LLMCallPayload,
  RunEvent,
  RunFailedPayload,
  RunFinishedPayload,
  ToolCallFinishedPayload,
  ToolCallStartedPayload,
  Trace,
} from "@/lib/types";
import type { RunStream } from "@/lib/useRunStream";

interface Props {
  runId: string | null;
  stream: RunStream;
  trace: Trace | null;
}

// One row of the panel feed, in event order.
type FeedItem =
  | { kind: "tool_call"; seq: number; call: ToolCallView }
  | { kind: "llm_call"; seq: number; payload: LLMCallPayload }
  | { kind: "citations"; seq: number; payload: CitationsPayload };

interface PanelView {
  items: FeedItem[];
  phase: string;
  failedError: string | null;
  totals: {
    llmCalls: number;
    toolCalls: number;
    inputTokens: number;
    outputTokens: number;
    latencyMs: number | null;
  };
}

// deriveView folds the event list into the panel model: started tool calls
// become running cards that their finished event completes; llm_call and
// citations events become ticks; the terminal event settles phase and totals
// (mirroring the backend's traceTotals rule: the terminal event is
// authoritative where it exists).
function deriveView(events: RunEvent[]): PanelView {
  const items: FeedItem[] = [];
  const callIndex = new Map<string, number>();
  let phase = "Starting…";
  let failedError: string | null = null;
  const totals = {
    llmCalls: 0,
    toolCalls: 0,
    inputTokens: 0,
    outputTokens: 0,
    latencyMs: null as number | null,
  };

  for (const event of events) {
    switch (event.type) {
      case "run_started":
        phase = "Investigating…";
        break;
      case "llm_call": {
        const p = event.payload as LLMCallPayload;
        totals.llmCalls++;
        totals.inputTokens += p.input_tokens;
        totals.outputTokens += p.output_tokens;
        items.push({ kind: "llm_call", seq: event.seq, payload: p });
        phase =
          p.tool_calls.length > 0
            ? `Iteration ${p.iteration}: calling ${p.tool_calls.length} tool${p.tool_calls.length > 1 ? "s" : ""}`
            : "Drafting answer…";
        break;
      }
      case "tool_call_started": {
        const p = event.payload as ToolCallStartedPayload;
        totals.toolCalls++;
        callIndex.set(p.tool_call_id, items.length);
        items.push({
          kind: "tool_call",
          seq: event.seq,
          call: {
            toolCallId: p.tool_call_id,
            tool: p.tool,
            arguments: p.arguments,
            done: false,
            status: "",
          },
        });
        phase = `Running ${p.tool}…`;
        break;
      }
      case "tool_call_finished": {
        const p = event.payload as ToolCallFinishedPayload;
        const at = callIndex.get(p.tool_call_id);
        const done: ToolCallView = {
          toolCallId: p.tool_call_id,
          tool: p.tool,
          arguments: at !== undefined ? (items[at] as { call: ToolCallView }).call.arguments : null,
          done: true,
          status: p.status,
          error: p.error,
          latencyMs: p.latency_ms,
          evidenceCount: p.evidence_count,
          cacheHit: p.cache_hit,
        };
        if (at !== undefined) {
          items[at] = { kind: "tool_call", seq: (items[at] as FeedItem).seq, call: done };
        } else {
          // A resume can start mid-run: the started event is before our
          // window, so the finished one stands alone.
          items.push({ kind: "tool_call", seq: event.seq, call: done });
        }
        break;
      }
      case "citations": {
        const p = event.payload as CitationsPayload;
        items.push({ kind: "citations", seq: event.seq, payload: p });
        phase = "Attaching citations…";
        break;
      }
      case "answer":
        phase = "Answer ready";
        break;
      case "run_finished": {
        const p = event.payload as RunFinishedPayload;
        totals.inputTokens = p.input_tokens;
        totals.outputTokens = p.output_tokens;
        totals.latencyMs = p.latency_ms;
        phase = "Done";
        break;
      }
      case "run_failed": {
        const p = event.payload as RunFailedPayload;
        totals.latencyMs = p.latency_ms;
        failedError = p.error;
        phase = "Failed";
        break;
      }
    }
  }
  return { items, phase, failedError, totals };
}

export function TracePanel({ runId, stream, trace }: Props) {
  // Live events win while they exist; the trace API's identical timeline backs
  // the panel for a browsed historical run.
  const events = useMemo<RunEvent[]>(() => {
    if (stream.events.length > 0) return stream.events;
    if (trace) {
      return trace.timeline.map((e) => ({
        seq: e.seq,
        type: e.type,
        payload: e.payload as RunEvent["payload"],
      }));
    }
    return [];
  }, [stream.events, trace]);

  const view = useMemo(() => deriveView(events), [events]);

  if (!runId) {
    return (
      <div className="flex flex-1 items-center justify-center p-6">
        <p className="max-w-52 text-center text-sm text-muted-foreground">
          The agent&apos;s investigation appears here, step by step, when you
          ask a question.
        </p>
      </div>
    );
  }

  const loading = events.length === 0;

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="border-b p-3">
        <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
          Agent trace
        </p>
        <p className="mt-1 flex items-center gap-2 text-sm font-medium">
          {view.phase !== "Done" && view.phase !== "Failed" && !loading && (
            <span className="size-2 animate-pulse rounded-full bg-primary" />
          )}
          {loading ? "Waiting for events…" : view.phase}
        </p>
      </div>

      <ScrollArea className="min-h-0 flex-1">
        <div className="flex flex-col gap-2 p-3">
          {view.items.map((item) =>
            item.kind === "tool_call" ? (
              <ToolCallCard key={item.seq} call={item.call} />
            ) : item.kind === "llm_call" ? (
              <div
                key={item.seq}
                className="flex items-center gap-2 px-1 text-xs text-muted-foreground"
              >
                <Brain className="size-3.5 shrink-0" />
                <span className="truncate">
                  {item.payload.purpose} · {item.payload.model}
                </span>
                <span className="ml-auto shrink-0 tabular-nums">
                  {item.payload.input_tokens + item.payload.output_tokens} tok ·{" "}
                  {formatLatency(item.payload.latency_ms)}
                </span>
              </div>
            ) : (
              <div
                key={item.seq}
                className="flex items-center gap-2 px-1 text-xs text-muted-foreground"
              >
                <Quote className="size-3.5 shrink-0" />
                <span>
                  {item.payload.skipped
                    ? `citations skipped: ${item.payload.skipped}`
                    : `${item.payload.citations.length} citations (${item.payload.dropped} dropped)`}
                </span>
              </div>
            ),
          )}
          {view.failedError && (
            <div className="rounded-md border border-destructive/50 bg-destructive/10 p-2 text-xs text-destructive">
              {view.failedError}
            </div>
          )}
        </div>
      </ScrollArea>

      <Separator />
      <footer className="flex items-center gap-3 p-3 text-xs tabular-nums text-muted-foreground">
        <span>{view.totals.llmCalls} LLM calls</span>
        <span>{view.totals.toolCalls} tools</span>
        <span>
          {view.totals.inputTokens + view.totals.outputTokens} tokens
        </span>
        {view.totals.latencyMs !== null && (
          <span className="ml-auto">{formatLatency(view.totals.latencyMs)}</span>
        )}
      </footer>
    </div>
  );
}
