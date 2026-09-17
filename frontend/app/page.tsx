"use client";

// The one page: chat on the left, live agent trace on the right.
//
// This component owns the cross-cutting state — which conversation is open,
// which run the trace panel is bound to, and the per-run trace cache — and
// hands the pieces down. There is no state library on purpose: one page, one
// owner.

import { useCallback, useEffect, useRef, useState } from "react";

import Link from "next/link";

import { ApprovalQueue } from "@/components/actions/ApprovalQueue";
import { ConversationList } from "@/components/chat/ConversationList";
import { MessageInput } from "@/components/chat/MessageInput";
import { MessageThread } from "@/components/chat/MessageThread";
import { useAuthContext } from "@/components/nav/AppShell";
import { TracePanel } from "@/components/trace/TracePanel";
import {
  ApiRequestError,
  getConnections,
  getTrace,
  listConversations,
  listMessages,
  sendChat,
} from "@/lib/api";
import type {
  ConnectionsInfo,
  Conversation,
  Message,
  Trace,
} from "@/lib/types";
import { useRunStream } from "@/lib/useRunStream";

export default function Home() {
  const { me } = useAuthContext();

  // undefined while loading, null when the lookup failed. A failed lookup must
  // not disable the input: the server is the authority and answers 409 if there
  // is genuinely nothing to search.
  const [connections, setConnections] = useState<ConnectionsInfo | null | undefined>(
    undefined,
  );
  useEffect(() => {
    let cancelled = false;
    void getConnections()
      .then((info) => {
        if (!cancelled) setConnections(info);
      })
      .catch(() => {
        if (!cancelled) setConnections(null);
      });
    return () => {
      cancelled = true;
    };
  }, []);
  const [conversations, setConversations] = useState<Conversation[] | null>(
    null,
  );
  const [activeConversationId, setActiveConversationId] = useState<
    string | null
  >(null);
  const [messages, setMessages] = useState<Message[] | null>(null);
  // The run the trace panel is bound to: the in-flight run after a send, or a
  // finished run selected by clicking its answer in the thread.
  const [activeRunId, setActiveRunId] = useState<string | null>(null);
  const [traces, setTraces] = useState<Record<string, Trace>>({});
  const [sendError, setSendError] = useState<string | null>(null);
  // Backend-unreachable style failures on the read paths; without it a dead
  // API renders as "No conversations yet", which is affirmatively wrong.
  const [loadError, setLoadError] = useState<string | null>(null);
  const [sending, setSending] = useState(false);
  // The run we are waiting on, so a historical run's replayed run_finished is
  // not mistaken for our send completing.
  const awaitingRunRef = useRef<string | null>(null);

  const refreshConversations = useCallback(() => {
    listConversations().then(
      (list) => {
        setConversations(list);
        setLoadError(null);
      },
      (err: unknown) => {
        setConversations((prev) => prev ?? []);
        setLoadError(
          err instanceof ApiRequestError
            ? `failed to load conversations: ${err.message}`
            : "failed to load conversations",
        );
      },
    );
  }, []);

  useEffect(refreshConversations, [refreshConversations]);

  const loadMessages = useCallback((conversationId: string) => {
    listMessages(conversationId).then(
      (list) => {
        setMessages(list);
        setLoadError(null);
      },
      (err: unknown) => {
        setMessages([]);
        setLoadError(
          err instanceof ApiRequestError
            ? `failed to load messages: ${err.message}`
            : "failed to load messages",
        );
      },
    );
  }, []);

  // Which run ids are cached or already being fetched — a ref, not state,
  // because it exists to dedupe fetches, and checking `traces` itself would
  // refetch on every render while a request is still in flight.
  const traceFetchedRef = useRef<Set<string>>(new Set());
  const loadTrace = useCallback((runId: string, force = false) => {
    if (!force && traceFetchedRef.current.has(runId)) return;
    traceFetchedRef.current.add(runId);
    getTrace(runId).then(
      (trace) => setTraces((cache) => ({ ...cache, [runId]: trace })),
      () => {
        // Missing trace only downgrades chips to plain text; allow a retry.
        traceFetchedRef.current.delete(runId);
      },
    );
  }, []);

  // Called from the SSE event handler when the active run reaches a terminal
  // event: the stored messages and the full trace are the durable truth now.
  const handleTerminal = useCallback(
    (runId: string) => {
      loadTrace(runId, true);
      if (awaitingRunRef.current === runId) {
        awaitingRunRef.current = null;
        setSending(false);
        if (activeConversationId) loadMessages(activeConversationId);
        refreshConversations();
      }
    },
    [activeConversationId, loadMessages, loadTrace, refreshConversations],
  );

  const stream = useRunStream(activeRunId, handleTerminal);

  // Navigating away abandons the live view of an in-flight run (the run
  // itself continues on the server and its answer lands in the conversation).
  // sending/awaitingRunRef MUST be reset here: they are otherwise only
  // cleared by the stream's terminal event, and navigation just tore that
  // stream down — leaving them set would disable the input forever.
  const abandonLiveRun = useCallback(() => {
    awaitingRunRef.current = null;
    setSending(false);
    setActiveRunId(null);
    setSendError(null);
  }, []);

  const selectConversation = useCallback(
    (id: string) => {
      abandonLiveRun();
      setActiveConversationId(id);
      setMessages(null);
      loadMessages(id);
    },
    [abandonLiveRun, loadMessages],
  );

  const startNewConversation = useCallback(() => {
    abandonLiveRun();
    setActiveConversationId(null);
    setMessages([]);
  }, [abandonLiveRun]);

  const send = useCallback(
    async (text: string) => {
      setSendError(null);
      setSending(true);
      // Optimistic user message: the POST stores it, but the thread should
      // show it immediately. Removed again if the POST fails — otherwise the
      // thread shows a message the backend never stored, and a retry would
      // duplicate it.
      const localID = `local-${Date.now()}`;
      setMessages((prev) => [
        ...(prev ?? []),
        {
          id: localID,
          role: "user",
          content: text,
          agent_run_id: null,
          created_at: new Date().toISOString(),
        },
      ]);
      try {
        const res = await sendChat(text, activeConversationId ?? undefined);
        awaitingRunRef.current = res.run_id;
        setActiveConversationId(res.conversation_id);
        setActiveRunId(res.run_id);
        refreshConversations();
      } catch (err) {
        setSending(false);
        setMessages((prev) => (prev ?? []).filter((m) => m.id !== localID));
        setSendError(
          err instanceof ApiRequestError
            ? err.message
            : "failed to send message",
        );
      }
    },
    [activeConversationId, refreshConversations],
  );

  // A finished assistant message binds the trace panel to its run.
  const selectRun = useCallback(
    (runId: string) => {
      setActiveRunId(runId);
      loadTrace(runId);
    },
    [loadTrace],
  );

  // A paused run is not investigating and not finished: it is waiting for a
  // person. The thinking indicator must stop — an agent that appears to be
  // working while it is actually blocked on you is the worst of both — and the
  // input stays disabled, because the run is still going to continue.
  const investigating =
    sending &&
    stream.status !== "terminal" &&
    stream.status !== "paused" &&
    stream.answer === null;

  // Reopening the stream after a decision: the backend closed it on the pause,
  // and the resumed run's events arrive on a fresh connection.
  //
  // Depends on stream.reopen, not stream: useRunStream returns a fresh object
  // every render, so depending on the whole thing meant this memo never held
  // and every consumer re-rendered with a new callback identity. reopen itself
  // is stable.
  const reopen = stream.reopen;
  const handleDecided = useCallback(() => {
    reopen();
  }, [reopen]);

  // BYOK: chat runs on the user's own key, so no key = no input. A disabled
  // box with no explanation reads as a bug; the call-to-action is the state.
  const needsKey = me !== null && !me.has_llm_key;

  // The same for sources. A run needs something to search, and a new account
  // has nothing connected — the server answers 409 no_sources_connected, so the
  // input says why up front instead of letting the user compose a question that
  // cannot run. Rendered only once the connections state is known, so the input
  // is not briefly disabled on every load.
  const needsSources = connections !== undefined && connections?.mode === "none";

  return (
    <div className="flex h-full">
      <aside className="flex w-64 shrink-0 flex-col border-r">
        <ConversationList
          conversations={conversations}
          activeId={activeConversationId}
          onSelect={selectConversation}
          onNew={startNewConversation}
        />
      </aside>

      <main className="flex min-w-0 flex-1 flex-col border-r">
        <MessageThread
          messages={messages}
          investigating={investigating}
          liveAnswer={stream.answer}
          runError={stream.error}
          traces={traces}
          activeRunId={activeRunId}
          onSelectRun={selectRun}
          onNeedTrace={loadTrace}
          hasConversation={activeConversationId !== null || sending}
        />
        {activeRunId && (
          <ApprovalQueue
            runId={activeRunId}
            paused={stream.status === "paused"}
            onDecided={handleDecided}
          />
        )}
        {(sendError ?? loadError) && (
          <p className="border-t px-4 py-2 text-sm text-destructive">
            {sendError ?? loadError}
          </p>
        )}
        {!needsKey && needsSources && (
          <p className="border-t px-4 py-2 text-sm text-muted-foreground">
            Connect a source to start asking questions —{" "}
            <Link href="/connections" className="text-primary underline">
              connect Jira, Notion or Gmail
            </Link>
            . Cortex answers from the sources you connect; nothing is searched
            until you do.
          </p>
        )}
        {needsKey && (
          <p className="border-t px-4 py-2 text-sm text-muted-foreground">
            Add your API key to start asking questions —{" "}
            <Link href="/settings" className="text-primary underline">
              add it in Settings
            </Link>
            . Your key funds your own conversations; it is stored encrypted.
          </p>
        )}
        <MessageInput onSend={send} disabled={sending || needsKey || needsSources} />
      </main>

      <aside className="flex w-80 shrink-0 flex-col xl:w-[26rem]">
        <TracePanel
          runId={activeRunId}
          stream={stream}
          trace={activeRunId ? (traces[activeRunId] ?? null) : null}
        />
      </aside>
    </div>
  );
}
