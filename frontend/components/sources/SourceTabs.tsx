"use client";

// Filter tabs: All · Jira · Notion · Gmail, each with its result count under
// the current filters (the counts come from the same query as the list, so
// they never disagree with it).

import type { SourceName } from "@/lib/types";
import { cn } from "@/lib/utils";

const tabs: { key: SourceName | null; label: string }[] = [
  { key: null, label: "All" },
  { key: "jira", label: "Jira" },
  { key: "notion", label: "Notion" },
  { key: "gmail", label: "Gmail" },
];

export function SourceTabs({
  active,
  counts,
  onSelect,
}: {
  active: SourceName | null;
  counts: Record<string, number>;
  onSelect: (source: SourceName | null) => void;
}) {
  const total = Object.values(counts).reduce((sum, n) => sum + n, 0);
  return (
    <div role="tablist" className="flex items-center gap-1">
      {tabs.map((tab) => {
        const count = tab.key === null ? total : (counts[tab.key] ?? 0);
        const selected = active === tab.key;
        return (
          <button
            key={tab.label}
            role="tab"
            aria-selected={selected}
            onClick={() => onSelect(tab.key)}
            className={cn(
              "rounded-md px-2.5 py-1 text-sm transition-colors",
              selected
                ? "bg-muted font-medium text-foreground"
                : "text-muted-foreground hover:bg-muted hover:text-foreground",
            )}
          >
            {tab.label}
            <span className="ml-1.5 text-xs tabular-nums text-muted-foreground">
              {count}
            </span>
          </button>
        );
      })}
    </div>
  );
}
