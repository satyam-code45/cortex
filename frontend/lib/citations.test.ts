// TEST-5.3 — the citation marker parser (REQ-5.3).
//
// Spec contract: [1][2]-style markers in the answer markdown are extracted and
// mapped to the run trace's evidence — resolution goes through trace.citations
// (whose marker strings are exactly "[n]") to trace.evidence by evidence_seq.
// Malformed markers ([abc], [0], [1.5]) and numbers no citation row backs stay
// literal text, never dead chips.

import { describe, expect, it } from "vitest";
import { parseCitations, resolveMarker } from "./citations";
import type { Trace, TraceCitation, TraceEvidence } from "./types";

function makeEvidence(seq: number, overrides: Partial<TraceEvidence> = {}): TraceEvidence {
  return {
    id: `evidence-${seq}`,
    seq,
    source: "jira",
    external_id: `ATLAS-${seq}`,
    title: `Evidence ${seq}`,
    url: `https://example.atlassian.net/browse/ATLAS-${seq}`,
    snippet: `snippet ${seq}`,
    source_timestamp: null,
    tool_call_id: null,
    ...overrides,
  };
}

function makeCitation(marker: number, evidenceSeq: number): TraceCitation {
  return {
    marker: `[${marker}]`,
    claim: `claim for [${marker}]`,
    evidence_id: `evidence-${evidenceSeq}`,
    evidence_seq: evidenceSeq,
    source: "jira",
    external_id: `ATLAS-${evidenceSeq}`,
    title: `Evidence ${evidenceSeq}`,
    url: `https://example.atlassian.net/browse/ATLAS-${evidenceSeq}`,
    snippet: `snippet ${evidenceSeq}`,
    source_timestamp: null,
  };
}

function makeTrace(evidence: TraceEvidence[], citations: TraceCitation[]): Trace {
  return {
    run_id: "00000000-0000-0000-0000-000000000001",
    conversation_id: "00000000-0000-0000-0000-000000000002",
    query: "why is ATLAS-1 blocked?",
    status: "completed",
    model: "gpt-4o",
    answer: null,
    error: null,
    created_at: null,
    finished_at: null,
    totals: {
      iterations: 1,
      tool_calls: 1,
      llm_calls: 1,
      input_tokens: 0,
      output_tokens: 0,
      latency_ms: 0,
    },
    timeline: [],
    tool_calls: [],
    evidence,
    citations,
  };
}

// The common trace: evidence 1 and 2, cited as [1] and [2].
const trace = makeTrace(
  [makeEvidence(1), makeEvidence(2)],
  [makeCitation(1, 1), makeCitation(2, 2)],
);

describe("parseCitations", () => {
  it("extracts adjacent [1][2] markers as two chips mapped to their evidence", () => {
    const segments = parseCitations("Blocked on the sandbox [1][2].", trace);

    expect(segments).toEqual([
      { kind: "text", text: "Blocked on the sandbox " },
      { kind: "citation", marker: 1, evidence: makeEvidence(1) },
      { kind: "citation", marker: 2, evidence: makeEvidence(2) },
      { kind: "text", text: "." },
    ]);
  });

  it("keeps surrounding text intact and in order", () => {
    const segments = parseCitations("A [1] b [2] c", trace);

    expect(segments).toEqual([
      { kind: "text", text: "A " },
      { kind: "citation", marker: 1, evidence: makeEvidence(1) },
      { kind: "text", text: " b " },
      { kind: "citation", marker: 2, evidence: makeEvidence(2) },
      { kind: "text", text: " c" },
    ]);
  });

  it("handles a marker at the very start and end without empty segments", () => {
    const segments = parseCitations("[1] middle [2]", trace);

    expect(segments).toEqual([
      { kind: "citation", marker: 1, evidence: makeEvidence(1) },
      { kind: "text", text: " middle " },
      { kind: "citation", marker: 2, evidence: makeEvidence(2) },
    ]);
  });

  it.each([
    ["[abc]", "letters"],
    ["[0]", "zero — evidence numbering starts at 1"],
    ["[]", "empty brackets"],
    ["[ 1 ]", "spaces inside the brackets"],
    ["[-1]", "sign"],
    ["[1.5]", "non-integer"],
  ])("leaves the malformed marker %s as literal text (%s)", (marker) => {
    const segments = parseCitations(`before ${marker} after [1]`, trace);

    expect(segments).toEqual([
      { kind: "text", text: `before ${marker} after ` },
      { kind: "citation", marker: 1, evidence: makeEvidence(1) },
    ]);
  });

  it("leaves a well-formed marker with no citation row as literal text", () => {
    const segments = parseCitations("See [99] and [1].", trace);

    expect(segments).toEqual([
      { kind: "text", text: "See [99] and " },
      { kind: "citation", marker: 1, evidence: makeEvidence(1) },
      { kind: "text", text: "." },
    ]);
  });

  it("treats every marker as text when there is no trace", () => {
    const markdown = "Blocked [1][2], see [3].";
    expect(parseCitations(markdown, null)).toEqual([
      { kind: "text", text: markdown },
    ]);
  });

  it("returns no segments for an empty answer", () => {
    expect(parseCitations("", trace)).toEqual([]);
  });

  it("never emits empty or adjacent text segments", () => {
    // Mixes resolvable, unresolvable, and malformed markers back to back.
    const segments = parseCitations("[1][99][abc][2]", trace);

    expect(segments).toEqual([
      { kind: "citation", marker: 1, evidence: makeEvidence(1) },
      { kind: "text", text: "[99][abc]" },
      { kind: "citation", marker: 2, evidence: makeEvidence(2) },
    ]);
    for (const s of segments) {
      if (s.kind === "text") expect(s.text).not.toBe("");
    }
    for (let i = 1; i < segments.length; i++) {
      expect(segments[i - 1].kind === "text" && segments[i].kind === "text").toBe(false);
    }
  });
});

describe("resolveMarker", () => {
  it("resolves through the citation row's evidence_seq, not the marker number", () => {
    // Marker [1] deliberately cites evidence seq 2: the citation row is the
    // authority, so the chip must carry evidence 2.
    const crossed = makeTrace(
      [makeEvidence(1), makeEvidence(2)],
      [makeCitation(1, 2)],
    );

    expect(resolveMarker(1, crossed)).toEqual(makeEvidence(2));
  });

  it("returns null when no citation row backs the marker", () => {
    expect(resolveMarker(99, trace)).toBeNull();
    expect(resolveMarker(0, trace)).toBeNull();
  });

  it("falls back to the citation row's denormalized copy when the evidence list misses the seq", () => {
    const sparse = makeTrace([], [makeCitation(1, 7)]);

    const resolved = resolveMarker(1, sparse);
    expect(resolved).not.toBeNull();
    expect(resolved).toMatchObject({
      id: "evidence-7",
      seq: 7,
      source: "jira",
      external_id: "ATLAS-7",
      title: "Evidence 7",
      url: "https://example.atlassian.net/browse/ATLAS-7",
      snippet: "snippet 7",
      tool_call_id: null,
    });
  });
});
