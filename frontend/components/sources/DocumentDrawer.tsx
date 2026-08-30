"use client";

// The detail drawer: full content of one document plus the "Open in …" link
// back to the live source. A fixed right-side panel rather than a modal — the
// listing stays visible and clickable behind it.

import { useEffect, useState } from "react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ApiRequestError, getDocument } from "@/lib/api";
import { humanizeDate } from "@/lib/humanize";
import type { DocumentDetail } from "@/lib/types";

const sourceLabels: Record<string, string> = {
  jira: "Open in Jira",
  notion: "Open in Notion",
  gmail: "Open in Gmail",
};

export function DocumentDrawer({
  documentId,
  onClose,
}: {
  documentId: string;
  onClose: () => void;
}) {
  // State keyed by the document it belongs to and reset during render when
  // the key changes (the React "adjust state when props change" pattern), so
  // the effect never writes state synchronously.
  const [state, setState] = useState<{
    id: string;
    doc: DocumentDetail | null;
    error: string | null;
  }>({ id: documentId, doc: null, error: null });
  if (state.id !== documentId) {
    setState({ id: documentId, doc: null, error: null });
  }
  const { doc, error } = state.id === documentId ? state : { doc: null, error: null };

  useEffect(() => {
    getDocument(documentId).then(
      (loaded) =>
        setState((prev) =>
          prev.id === documentId ? { ...prev, doc: loaded } : prev,
        ),
      (err: unknown) =>
        setState((prev) =>
          prev.id === documentId
            ? {
                ...prev,
                error:
                  err instanceof ApiRequestError
                    ? err.message
                    : "failed to load",
              }
            : prev,
        ),
    );
  }, [documentId]);

  return (
    <aside className="flex w-96 shrink-0 flex-col border-l xl:w-[30rem]">
      <div className="flex items-center gap-2 border-b p-3">
        {doc && (
          <Badge variant="secondary" className="capitalize">
            {doc.source}
          </Badge>
        )}
        <span className="min-w-0 flex-1 truncate text-sm font-medium">
          {doc?.title ?? "…"}
        </span>
        <Button variant="ghost" size="sm" onClick={onClose}>
          Close
        </Button>
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto p-4">
        {error && <p className="text-sm text-destructive">{error}</p>}
        {doc && (
          <div className="flex flex-col gap-3">
            <div className="flex items-center gap-3 text-xs text-muted-foreground">
              {doc.source_timestamp && (
                <span>{humanizeDate(doc.source_timestamp)}</span>
              )}
              {doc.url && (
                <a
                  href={doc.url}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="text-primary underline"
                >
                  {sourceLabels[doc.source] ?? "Open source"}
                </a>
              )}
            </div>
            <pre className="whitespace-pre-wrap font-sans text-sm">
              {doc.content}
            </pre>
          </div>
        )}
      </div>
    </aside>
  );
}
