"use client";

// One row of the Sources listing: source badge, title, snippet, humanized
// date. Clicking opens the detail drawer.

import { Badge } from "@/components/ui/badge";
import type { DocumentRow as Row } from "@/lib/types";
import { humanizeDate } from "@/lib/humanize";

export function DocumentRow({
  document,
  onOpen,
}: {
  document: Row;
  onOpen: (id: string) => void;
}) {
  return (
    <button
      onClick={() => onOpen(document.id)}
      className="flex w-full flex-col gap-1 border-b px-4 py-3 text-left transition-colors hover:bg-muted/50"
    >
      <div className="flex items-center gap-2">
        <Badge variant="secondary" className="capitalize">
          {document.source}
        </Badge>
        <span className="min-w-0 flex-1 truncate text-sm font-medium">
          {document.title || "(untitled)"}
        </span>
        <span className="shrink-0 text-xs text-muted-foreground">
          {humanizeDate(document.source_timestamp)}
        </span>
      </div>
      {document.snippet && (
        <p className="line-clamp-2 text-sm text-muted-foreground">
          {document.snippet}
        </p>
      )}
    </button>
  );
}
