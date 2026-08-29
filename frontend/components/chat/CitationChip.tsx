"use client";

// A citation marker rendered as a clickable chip. The popover shows where the
// claim came from — source icon, title, snippet — and links out to the real
// Jira issue / Notion page / Gmail message.

import { BookOpen, FileText, Mail, SquareKanban } from "lucide-react";
import type { ComponentType } from "react";

import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";
import type { TraceEvidence } from "@/lib/types";

const sourceIcons: Record<string, ComponentType<{ className?: string }>> = {
  jira: SquareKanban,
  notion: FileText,
  gmail: Mail,
  knowledge_base: BookOpen,
};

export function CitationChip({
  marker,
  evidence,
}: {
  marker: number;
  evidence: TraceEvidence;
}) {
  const Icon = sourceIcons[evidence.source] ?? BookOpen;
  // This anchor is the one link that bypasses react-markdown's URL sanitizer,
  // and the URL ultimately comes from tool evidence — attacker-adjacent data.
  // Today every source builds it server-side, but nothing on the path checks
  // the scheme, so a stored javascript: URL would become one-click XSS here.
  const safeURL =
    evidence.url && /^https?:\/\//i.test(evidence.url) ? evidence.url : null;

  return (
    <Popover>
      <PopoverTrigger
        className="mx-0.5 inline-flex size-4.5 -translate-y-px items-center justify-center rounded-full bg-primary/10 align-middle text-[10px] font-semibold text-primary hover:bg-primary/20"
        aria-label={`Citation ${marker}: ${evidence.title ?? evidence.source}`}
      >
        {marker}
      </PopoverTrigger>
      <PopoverContent className="w-80 text-sm" align="start">
        <div className="flex items-start gap-2">
          <Icon className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
          <div className="min-w-0">
            <p className="font-medium">{evidence.title ?? evidence.external_id}</p>
            <p className="text-xs uppercase tracking-wide text-muted-foreground">
              {evidence.source}
            </p>
          </div>
        </div>
        {evidence.snippet && (
          <p className="mt-2 line-clamp-4 text-muted-foreground">
            {evidence.snippet}
          </p>
        )}
        {safeURL && (
          <a
            href={safeURL}
            target="_blank"
            rel="noreferrer"
            className="mt-2 inline-block text-xs font-medium text-primary underline underline-offset-2"
          >
            Open source ↗
          </a>
        )}
      </PopoverContent>
    </Popover>
  );
}
