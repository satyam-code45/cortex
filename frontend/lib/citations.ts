// Citation marker parsing: pure functions, no DOM, so vitest needs no jsdom.
//
// The backend guarantees that a stored answer's [n]
// markers are renumbered 1..N and every one is backed by a citation row. The
// parser still refuses to trust that: a marker is only a chip when it resolves
// against the run's trace, and everything else — [abc], [0], [99] with no
// matching evidence — stays plain text rather than a dead chip.

import type { Trace, TraceCitation, TraceEvidence } from "./types";

// One piece of an answer: literal markdown text, or a citation chip.
export type AnswerSegment =
  | { kind: "text"; text: string }
  | { kind: "citation"; marker: number; evidence: TraceEvidence };

// A [n] marker: one or more digits, no sign, no spaces. [0] is excluded — the
// backend numbers evidence from 1, so a zero can only be literal text.
const markerPattern = /\[([1-9][0-9]*)\]/g;

// parseCitations splits answer markdown into text and chip segments, resolving
// each [n] against the trace. Markers that do not resolve are left in the text
// exactly as written. Adjacent text pieces are merged so the segment list is
// canonical: no empty segments, never two text segments in a row.
export function parseCitations(
  markdown: string,
  trace: Trace | null,
): AnswerSegment[] {
  const segments: AnswerSegment[] = [];
  let text = "";

  markerPattern.lastIndex = 0;
  let cursor = 0;
  for (const match of markdown.matchAll(markerPattern)) {
    const marker = Number(match[1]);
    const evidence = trace ? resolveMarker(marker, trace) : null;

    text += markdown.slice(cursor, match.index);
    cursor = match.index + match[0].length;

    if (evidence === null) {
      text += match[0];
      continue;
    }
    if (text !== "") {
      segments.push({ kind: "text", text });
      text = "";
    }
    segments.push({ kind: "citation", marker, evidence });
  }
  text += markdown.slice(cursor);
  if (text !== "") {
    segments.push({ kind: "text", text });
  }
  return segments;
}

// rewriteMarkersToLinks rewrites each resolvable [n] marker to a markdown link
// on a reserved fragment (`[n](#cite-n)`), leaving everything else byte-for-
// byte intact. This is how markers survive markdown parsing as addressable
// nodes: the renderer swaps those links for chips, and unresolvable markers
// stay the literal text the model wrote.
//
// Built on parseCitations rather than a second regex so the tested parser is
// the only authority on what counts as a marker.
//
// Known limitation: the rewrite happens on raw markdown, before parsing, so a
// resolvable number inside a code span (`retries[1]`) is rewritten too. Doing
// better means a remark plugin over text nodes; not worth it while answers are
// prose.
export function rewriteMarkersToLinks(
  markdown: string,
  trace: Trace | null,
): string {
  return parseCitations(markdown, trace)
    .map((s) =>
      s.kind === "text" ? s.text : `[${s.marker}](#cite-${s.marker})`,
    )
    .join("");
}

// resolveMarker maps a marker number to the evidence behind it, or null when
// the trace does not back it.
//
// The citation rows are the authority: they are what the validator persisted,
// and their marker strings are exactly "[n]". A marker with no citation row
// resolves to nothing — deliberately, so a number the model wrote without
// evidence behind it renders as plain text rather than a dead chip. The
// evidenceFromCitation fallback below covers only the inverse defect: a
// citation row whose evidence_seq is missing from the trace's evidence list.
export function resolveMarker(
  marker: number,
  trace: Trace,
): TraceEvidence | null {
  const citation = trace.citations.find((c) => c.marker === `[${marker}]`);
  if (citation) {
    return (
      trace.evidence.find((e) => e.seq === citation.evidence_seq) ??
      evidenceFromCitation(citation)
    );
  }
  return null;
}

// evidenceFromCitation rebuilds an evidence item from the citation row's own
// denormalized copy, for the (unexpected) case where the citation's
// evidence_seq is missing from the trace's evidence list.
function evidenceFromCitation(c: TraceCitation): TraceEvidence {
  return {
    id: c.evidence_id,
    seq: c.evidence_seq,
    source: c.source,
    external_id: c.external_id,
    title: c.title,
    url: c.url,
    snippet: c.snippet,
    source_timestamp: c.source_timestamp,
    tool_call_id: null,
  };
}
