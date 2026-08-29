"use client";

// Markdown rendering for assistant answers, with [n] markers as citation
// chips.
//
// Chips have to live inside markdown structure — a marker sits mid-paragraph,
// inside list items, next to bold text — so the answer cannot be split around
// them and rendered piecewise. Instead, lib/citations.ts rewrites every
// resolvable marker to a link on a reserved fragment (`[n](#cite-n)`), and the
// link renderer here swaps those for CitationChip. Markers that do not resolve
// are left untouched and render as the literal text the model wrote.
//
// All marker recognition lives in lib/citations.ts — the tested module — so
// this component holds no regex of its own to drift.

import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";

import { CitationChip } from "@/components/chat/CitationChip";
import { resolveMarker, rewriteMarkersToLinks } from "@/lib/citations";
import type { Trace } from "@/lib/types";

const citeHref = "#cite-";

export function AnswerMarkdown({
  markdown,
  trace,
}: {
  markdown: string;
  trace: Trace | null;
}) {
  const source = rewriteMarkersToLinks(markdown, trace);

  return (
    <div className="prose-chat">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        components={{
          a: ({ href, children }) => {
            if (trace && href?.startsWith(citeHref)) {
              const marker = Number(href.slice(citeHref.length));
              const evidence = resolveMarker(marker, trace);
              if (evidence) {
                return <CitationChip marker={marker} evidence={evidence} />;
              }
            }
            return (
              <a href={href} target="_blank" rel="noreferrer">
                {children}
              </a>
            );
          },
        }}
      >
        {source}
      </ReactMarkdown>
    </div>
  );
}
