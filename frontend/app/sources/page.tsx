"use client";

// Sources: every indexed Jira issue, Notion page and Gmail message in one
// list. It reads our documents table (15-minute-fresh local copy), never the
// live APIs — the honest "last refreshed" stamp is the trade's other half.

import { useCallback, useEffect, useRef, useState } from "react";

import { DocumentDrawer } from "@/components/sources/DocumentDrawer";
import { DocumentRow } from "@/components/sources/DocumentRow";
import { RefreshButton } from "@/components/sources/RefreshButton";
import { SourceTabs } from "@/components/sources/SourceTabs";
import { Input } from "@/components/ui/input";
import { ApiRequestError, listDocuments, refreshDocuments } from "@/lib/api";
import type { DocumentsResponse, SourceName } from "@/lib/types";

// searchDebounceMs delays the query until typing pauses; every keystroke as a
// request would mostly cancel itself.
const searchDebounceMs = 300;

export default function SourcesPage() {
  const [source, setSource] = useState<SourceName | null>(null);
  const [search, setSearch] = useState("");
  const [data, setData] = useState<DocumentsResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [openId, setOpenId] = useState<string | null>(null);

  const [refreshing, setRefreshing] = useState(false);
  const [cooldownSeconds, setCooldownSeconds] = useState<number | null>(null);
  const [refreshNote, setRefreshNote] = useState<string | null>(null);

  // The latest request wins: a slow response for a stale query must not
  // overwrite the current one's results.
  const requestSeq = useRef(0);
  const load = useCallback((src: SourceName | null, q: string) => {
    const seq = ++requestSeq.current;
    listDocuments({ source: src ?? undefined, q: q || undefined }).then(
      (res) => {
        if (requestSeq.current !== seq) return;
        setData(res);
        setError(null);
      },
      (err: unknown) => {
        if (requestSeq.current !== seq) return;
        setError(
          err instanceof ApiRequestError
            ? err.message
            : "failed to load documents",
        );
      },
    );
  }, []);

  // Debounced reload on filter/search changes; immediate on first render.
  useEffect(() => {
    const timer = setTimeout(
      () => load(source, search.trim()),
      search ? searchDebounceMs : 0,
    );
    return () => clearTimeout(timer);
  }, [source, search, load]);

  const refresh = useCallback(async () => {
    setRefreshing(true);
    setRefreshNote(null);
    try {
      await refreshDocuments();
      setRefreshNote("Refresh queued — new content appears as the crawl finishes.");
      // Re-read shortly after so last_refreshed moves once the jobs finish.
      setTimeout(() => load(source, search.trim()), 5000);
    } catch (err) {
      if (err instanceof ApiRequestError && err.status === 429) {
        setCooldownSeconds(err.retryAfterSeconds ?? 60);
      } else {
        setRefreshNote(
          err instanceof ApiRequestError ? err.message : "refresh failed",
        );
      }
    } finally {
      setRefreshing(false);
    }
  }, [load, search, source]);

  return (
    <div className="flex h-full">
      <main className="flex min-w-0 flex-1 flex-col">
        <div className="flex flex-wrap items-center gap-3 border-b p-3">
          <SourceTabs
            active={source}
            counts={data?.counts ?? {}}
            onSelect={setSource}
          />
          <Input
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder="Search titles and content…"
            className="max-w-xs"
          />
          <div className="ml-auto">
            <RefreshButton
              lastRefreshed={data?.last_refreshed ?? null}
              cooldownSeconds={cooldownSeconds}
              refreshing={refreshing}
              onRefresh={() => void refresh()}
              onCooldownEnd={() => setCooldownSeconds(null)}
            />
          </div>
        </div>
        {(error ?? refreshNote) && (
          <p className="border-b px-4 py-2 text-sm text-muted-foreground">
            {error ?? refreshNote}
          </p>
        )}
        <div className="min-h-0 flex-1 overflow-y-auto">
          {data === null ? (
            <p className="p-4 text-sm text-muted-foreground">Loading…</p>
          ) : data.documents.length === 0 ? (
            <p className="p-4 text-sm text-muted-foreground">
              No documents match.
            </p>
          ) : (
            data.documents.map((doc) => (
              <DocumentRow key={doc.id} document={doc} onOpen={setOpenId} />
            ))
          )}
        </div>
      </main>
      {openId && (
        <DocumentDrawer documentId={openId} onClose={() => setOpenId(null)} />
      )}
    </div>
  );
}
