"use client";

// One tool call in the trace panel: name, compact arguments, running/done
// state, latency, evidence count.

import { Check, CircleAlert, Loader2 } from "lucide-react";

import { Badge } from "@/components/ui/badge";

export interface ToolCallView {
  toolCallId: string;
  tool: string;
  arguments: unknown;
  done: boolean;
  status: string; // "ok" | "error" once done
  error?: string;
  latencyMs?: number;
  evidenceCount?: number;
  cacheHit?: boolean;
}

// compactArgs renders tool arguments on one line, capped: the trace panel is a
// feed, not an inspector, and the full arguments stay available in the trace
// API for anyone digging.
function compactArgs(args: unknown): string {
  let text: string;
  try {
    text = typeof args === "string" ? args : JSON.stringify(args);
  } catch {
    return "…";
  }
  if (!text) return "";
  return text.length > 120 ? text.slice(0, 120) + "…" : text;
}

export function ToolCallCard({ call }: { call: ToolCallView }) {
  const failed = call.done && call.status !== "ok";
  return (
    <div className="rounded-md border p-2 text-sm">
      <div className="flex items-center gap-2">
        {!call.done ? (
          <Loader2 className="size-3.5 shrink-0 animate-spin text-primary" />
        ) : failed ? (
          <CircleAlert className="size-3.5 shrink-0 text-destructive" />
        ) : (
          <Check className="size-3.5 shrink-0 text-primary" />
        )}
        <span className="truncate font-mono text-xs font-medium">
          {call.tool}
        </span>
        <span className="ml-auto flex shrink-0 items-center gap-1.5">
          {call.cacheHit && <Badge variant="outline">cached</Badge>}
          {call.done && call.evidenceCount !== undefined && (
            <Badge variant="secondary">
              {call.evidenceCount} evidence
            </Badge>
          )}
          {call.done && call.latencyMs !== undefined && (
            <span className="text-xs tabular-nums text-muted-foreground">
              {formatLatency(call.latencyMs)}
            </span>
          )}
        </span>
      </div>
      <p className="mt-1 truncate font-mono text-xs text-muted-foreground">
        {compactArgs(call.arguments)}
      </p>
      {failed && call.error && (
        <p className="mt-1 text-xs text-destructive">{call.error}</p>
      )}
    </div>
  );
}

export function formatLatency(ms: number): string {
  return ms >= 1000 ? `${(ms / 1000).toFixed(1)}s` : `${ms}ms`;
}
