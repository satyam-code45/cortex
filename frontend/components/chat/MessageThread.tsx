"use client";

// The message thread: stored history plus the in-flight run's states.
//
// Assistant messages carry agent_run_id (added by migration 004); the thread
// fetches that run's trace lazily so citation markers resolve to chips even in
// a re-opened conversation. Messages without a run link — user messages, rows
// from before the migration — render their markers as plain text.

import { useEffect, useRef } from "react";

import { AnswerMarkdown } from "@/components/chat/AnswerMarkdown";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Skeleton } from "@/components/ui/skeleton";
import type { Message, Trace } from "@/lib/types";
import { cn } from "@/lib/utils";

interface Props {
  // null while loading a selected conversation.
  messages: Message[] | null;
  investigating: boolean;
  // The live run's answer, shown until the reloaded history covers it.
  liveAnswer: string | null;
  runError: string | null;
  traces: Record<string, Trace>;
  activeRunId: string | null;
  onSelectRun: (runId: string) => void;
  onNeedTrace: (runId: string) => void;
  hasConversation: boolean;
}

export function MessageThread({
  messages,
  investigating,
  liveAnswer,
  runError,
  traces,
  activeRunId,
  onSelectRun,
  onNeedTrace,
  hasConversation,
}: Props) {
  // Fetch traces for answers whose chips cannot resolve yet.
  useEffect(() => {
    for (const m of messages ?? []) {
      if (m.role === "assistant" && m.agent_run_id && !traces[m.agent_run_id]) {
        onNeedTrace(m.agent_run_id);
      }
    }
  }, [messages, traces, onNeedTrace]);

  // Keep the newest message in view as the conversation grows.
  const endRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    endRef.current?.scrollIntoView({ block: "end" });
  }, [messages, investigating, liveAnswer, runError]);

  if (!hasConversation) {
    return (
      <div className="flex flex-1 items-center justify-center p-8">
        <div className="max-w-md text-center">
          <h2 className="text-lg font-semibold">Ask Cortex anything</h2>
          <p className="mt-2 text-sm text-muted-foreground">
            It investigates live Jira, Notion and Gmail data, shows you every
            step in the trace panel, and cites its sources.
          </p>
        </div>
      </div>
    );
  }

  // The stored history already contains the live answer once the terminal
  // reload lands; until then the streamed answer is appended locally.
  const showLiveAnswer =
    liveAnswer !== null &&
    !(messages ?? []).some(
      (m) => m.role === "assistant" && m.agent_run_id === activeRunId,
    );

  return (
    <ScrollArea className="min-h-0 flex-1">
      <div className="mx-auto flex max-w-3xl flex-col gap-4 p-4">
        {messages === null ? (
          <>
            <Skeleton className="h-10 w-2/3 self-end" />
            <Skeleton className="h-24 w-full" />
          </>
        ) : (
          messages.map((m) =>
            m.role === "user" ? (
              <div
                key={m.id}
                className="max-w-[85%] self-end rounded-lg bg-primary px-3 py-2 text-sm whitespace-pre-wrap text-primary-foreground"
              >
                {m.content}
              </div>
            ) : (
              // A div, not a button: the bubble contains citation chips
              // (buttons) and links, and interactive elements must not nest.
              // The click is a convenience duplicated by an explicit "trace"
              // affordance below for keyboard users.
              <div
                key={m.id}
                onClick={() => m.agent_run_id && onSelectRun(m.agent_run_id)}
                className={cn(
                  "max-w-full self-start rounded-lg border px-3 py-2 text-left text-sm",
                  m.agent_run_id !== null && "cursor-pointer",
                  m.agent_run_id === activeRunId && activeRunId !== null
                    ? "border-primary/40 bg-accent/50"
                    : "hover:bg-accent/30",
                )}
              >
                <AnswerMarkdown
                  markdown={m.content}
                  trace={m.agent_run_id ? (traces[m.agent_run_id] ?? null) : null}
                />
                {m.agent_run_id && (
                  <button
                    onClick={(e) => {
                      e.stopPropagation();
                      onSelectRun(m.agent_run_id!);
                    }}
                    className="mt-1.5 text-xs text-muted-foreground underline underline-offset-2 hover:text-foreground"
                  >
                    show trace
                  </button>
                )}
              </div>
            ),
          )
        )}

        {showLiveAnswer && (
          <div className="max-w-full self-start rounded-lg border border-primary/40 bg-accent/50 px-3 py-2 text-sm">
            <AnswerMarkdown
              markdown={liveAnswer}
              trace={activeRunId ? (traces[activeRunId] ?? null) : null}
            />
          </div>
        )}

        {investigating && (
          <div className="flex items-center gap-2 self-start rounded-lg border px-3 py-2 text-sm text-muted-foreground">
            <span className="size-2 animate-pulse rounded-full bg-primary" />
            investigating…
          </div>
        )}

        {runError && (
          <div className="max-w-full self-start rounded-lg border border-destructive/50 bg-destructive/10 px-3 py-2 text-sm">
            <p className="font-medium text-destructive">Run failed</p>
            <p className="mt-1 text-muted-foreground">{runError}</p>
          </div>
        )}

        <div ref={endRef} />
      </div>
    </ScrollArea>
  );
}
