package agent

import (
	"strings"
	"testing"
)

// TEST-4.2 — the citation validator.
//
// applyCitations is the hallucinated-citation guard, and it is unexported, so
// this is an internal test (the same arrangement internal/jobs uses for its
// timeout test). It is a pure function over the model's structured output, which
// is what makes the guard assertable without a database, a provider or a run.
//
// The contract is REQ-4.3 as amended by A3:
//
//   - evidence_id is the per-run evidence NUMBER, not a UUID. A number that is
//     not present in this run's evidence is dropped.
//   - a citation whose marker never appears in the answer is dropped: it cannot
//     be rendered.
//   - survivors are renumbered 1..N by first appearance in the answer text, the
//     inline [n] markers are rewritten to match, and markers left unbacked are
//     removed from the text.
//
// The renumbering is the reason the acceptance answer reads [1][2][3] rather
// than whatever numbers the model happened to pick.

// wantCitation is the expected shape of one surviving citation.
type wantCitation struct {
	marker      string
	evidenceSeq int
	claim       string
}

func TestApplyCitations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		draft         citationDraft
		evidenceCount int

		wantAnswer    string
		wantCitations []wantCitation
		wantDropped   int
	}{
		{
			// The guard. Evidence [9] does not exist in a run that gathered
			// three items, so the citation goes and its marker goes with it.
			name: "hallucinated evidence id is dropped",
			draft: citationDraft{
				AnswerMarkdown: "Atlas is behind [1] and the vendor slipped [2].",
				Citations: []draftCitation{
					{Marker: "[1]", EvidenceID: 1, Claim: "Atlas is behind"},
					{Marker: "[2]", EvidenceID: 9, Claim: "the vendor slipped"},
				},
			},
			evidenceCount: 3,
			wantAnswer:    "Atlas is behind [1] and the vendor slipped.",
			wantCitations: []wantCitation{
				{marker: "[1]", evidenceSeq: 1, claim: "Atlas is behind"},
			},
			wantDropped: 1,
		},
		{
			name: "every valid citation is kept",
			draft: citationDraft{
				AnswerMarkdown: "Sandbox is down [1]. The vendor slipped [2]. Q2 is at risk [3].",
				Citations: []draftCitation{
					{Marker: "[1]", EvidenceID: 1, Claim: "Sandbox is down"},
					{Marker: "[2]", EvidenceID: 2, Claim: "The vendor slipped"},
					{Marker: "[3]", EvidenceID: 3, Claim: "Q2 is at risk"},
				},
			},
			evidenceCount: 3,
			wantAnswer:    "Sandbox is down [1]. The vendor slipped [2]. Q2 is at risk [3].",
			wantCitations: []wantCitation{
				{marker: "[1]", evidenceSeq: 1, claim: "Sandbox is down"},
				{marker: "[2]", evidenceSeq: 2, claim: "The vendor slipped"},
				{marker: "[3]", evidenceSeq: 3, claim: "Q2 is at risk"},
			},
			wantDropped: 0,
		},
		{
			// A3: the rendered answer must read [1][2][3] whatever numbers the
			// model chose, and the citation list must agree with the text.
			name: "markers are renumbered by first appearance",
			draft: citationDraft{
				AnswerMarkdown: "The vendor slipped [7]. Atlas is behind [3].",
				Citations: []draftCitation{
					{Marker: "[3]", EvidenceID: 1, Claim: "Atlas is behind"},
					{Marker: "[7]", EvidenceID: 2, Claim: "The vendor slipped"},
				},
			},
			evidenceCount: 4,
			wantAnswer:    "The vendor slipped [1]. Atlas is behind [2].",
			wantCitations: []wantCitation{
				{marker: "[1]", evidenceSeq: 2, claim: "The vendor slipped"},
				{marker: "[2]", evidenceSeq: 1, claim: "Atlas is behind"},
			},
			wantDropped: 0,
		},
		{
			// Consistency across repeats: the same source cited twice keeps one
			// number, so the reader follows one marker to one entry.
			name: "a repeated marker keeps the number it was first given",
			draft: citationDraft{
				AnswerMarkdown: "Blocked [2]. Vendor slipped [5]. Still blocked [2].",
				Citations: []draftCitation{
					{Marker: "[2]", EvidenceID: 1, Claim: "Blocked"},
					{Marker: "[5]", EvidenceID: 3, Claim: "Vendor slipped"},
				},
			},
			evidenceCount: 3,
			wantAnswer:    "Blocked [1]. Vendor slipped [2]. Still blocked [1].",
			wantCitations: []wantCitation{
				{marker: "[1]", evidenceSeq: 1, claim: "Blocked"},
				{marker: "[2]", evidenceSeq: 3, claim: "Vendor slipped"},
			},
			wantDropped: 0,
		},
		{
			// Renumbering has to survive a gap: dropping the middle citation
			// must not leave [1] and [3] in the text.
			name: "surviving markers are contiguous after a drop in the middle",
			draft: citationDraft{
				AnswerMarkdown: "One [1]. Two [2]. Three [3].",
				Citations: []draftCitation{
					{Marker: "[1]", EvidenceID: 1, Claim: "One"},
					{Marker: "[2]", EvidenceID: 42, Claim: "Two"},
					{Marker: "[3]", EvidenceID: 2, Claim: "Three"},
				},
			},
			evidenceCount: 2,
			wantAnswer:    "One [1]. Two. Three [2].",
			wantCitations: []wantCitation{
				{marker: "[1]", evidenceSeq: 1, claim: "One"},
				{marker: "[2]", evidenceSeq: 2, claim: "Three"},
			},
			wantDropped: 1,
		},
		{
			// A citation the model listed but never placed cannot be rendered,
			// so it must not reach the citations table.
			name: "a citation whose marker is absent from the answer is dropped",
			draft: citationDraft{
				AnswerMarkdown: "Only one claim is made here [1].",
				Citations: []draftCitation{
					{Marker: "[1]", EvidenceID: 1, Claim: "Only one claim"},
					{Marker: "[4]", EvidenceID: 2, Claim: "A claim that was never written"},
				},
			},
			evidenceCount: 2,
			wantAnswer:    "Only one claim is made here [1].",
			wantCitations: []wantCitation{
				{marker: "[1]", evidenceSeq: 1, claim: "Only one claim"},
			},
			wantDropped: 1,
		},
		{
			// The mirror image: a marker in the text with no citation behind it
			// is removed rather than left pointing at nothing.
			name: "an unbacked marker is removed from the text",
			draft: citationDraft{
				AnswerMarkdown: "Backed [1] and unbacked [2].",
				Citations: []draftCitation{
					{Marker: "[1]", EvidenceID: 1, Claim: "Backed"},
				},
			},
			evidenceCount: 1,
			wantAnswer:    "Backed [1] and unbacked.",
			wantCitations: []wantCitation{
				{marker: "[1]", evidenceSeq: 1, claim: "Backed"},
			},
			wantDropped: 0,
		},
		{
			// A run that gathered nothing can cite nothing, whatever the model
			// claims.
			name: "every citation is dropped when the run has no evidence",
			draft: citationDraft{
				AnswerMarkdown: "A confident claim [1].",
				Citations: []draftCitation{
					{Marker: "[1]", EvidenceID: 1, Claim: "A confident claim"},
				},
			},
			evidenceCount: 0,
			wantAnswer:    "A confident claim.",
			wantCitations: nil,
			wantDropped:   1,
		},
		{
			name: "evidence id below one is not a valid number",
			draft: citationDraft{
				AnswerMarkdown: "Claim [1]. Other claim [2].",
				Citations: []draftCitation{
					{Marker: "[1]", EvidenceID: 0, Claim: "Claim"},
					{Marker: "[2]", EvidenceID: -3, Claim: "Other claim"},
				},
			},
			evidenceCount: 5,
			wantAnswer:    "Claim. Other claim.",
			wantCitations: nil,
			wantDropped:   2,
		},
		{
			name: "an uncited answer passes through untouched",
			draft: citationDraft{
				AnswerMarkdown: "No source was needed for this.",
				Citations:      nil,
			},
			evidenceCount: 3,
			wantAnswer:    "No source was needed for this.",
			wantCitations: nil,
			wantDropped:   0,
		},
		{
			// Markers are matched by the number the model wrote, not by string
			// identity, because models write the marker both ways.
			name: "a bare marker number still binds to the marker in the text",
			draft: citationDraft{
				AnswerMarkdown: "Atlas is behind [2].",
				Citations: []draftCitation{
					{Marker: "2", EvidenceID: 1, Claim: "Atlas is behind"},
				},
			},
			evidenceCount: 1,
			wantAnswer:    "Atlas is behind [1].",
			wantCitations: []wantCitation{
				{marker: "[1]", evidenceSeq: 1, claim: "Atlas is behind"},
			},
			wantDropped: 0,
		},
		{
			name: "a double-digit marker is renumbered like any other",
			draft: citationDraft{
				AnswerMarkdown: "First [12]. Second [3].",
				Citations: []draftCitation{
					{Marker: "[12]", EvidenceID: 11, Claim: "First"},
					{Marker: "[3]", EvidenceID: 2, Claim: "Second"},
				},
			},
			evidenceCount: 12,
			wantAnswer:    "First [1]. Second [2].",
			wantCitations: []wantCitation{
				{marker: "[1]", evidenceSeq: 11, claim: "First"},
				{marker: "[2]", evidenceSeq: 2, claim: "Second"},
			},
			wantDropped: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			answer, resolved, dropped := applyCitations(tt.draft, tt.evidenceCount)

			if answer != tt.wantAnswer {
				t.Errorf("answer  = %q\nwant     = %q", answer, tt.wantAnswer)
			}
			if dropped != tt.wantDropped {
				t.Errorf("dropped = %d, want %d", dropped, tt.wantDropped)
			}
			if len(resolved) != len(tt.wantCitations) {
				t.Fatalf("citations = %d (%+v), want %d", len(resolved), resolved, len(tt.wantCitations))
			}
			for i, want := range tt.wantCitations {
				got := resolved[i]
				if got.Marker != want.marker {
					t.Errorf("citations[%d].Marker = %q, want %q", i, got.Marker, want.marker)
				}
				if got.EvidenceSeq != want.evidenceSeq {
					t.Errorf("citations[%d].EvidenceSeq = %d, want %d", i, got.EvidenceSeq, want.evidenceSeq)
				}
				if got.Claim != want.claim {
					t.Errorf("citations[%d].Claim = %q, want %q", i, got.Claim, want.claim)
				}
			}

			// Two invariants that must hold for every input, because the trace
			// API and the rendered answer both depend on them:
			//
			//  1. markers persist as "[n]" (REQ-4.3),
			//  2. the numbers run 1..N in list order, and every one of them is
			//     actually present in the answer text.
			for i, c := range resolved {
				wantMarker := "[" + itoa(i+1) + "]"
				if c.Marker != wantMarker {
					t.Errorf("citations[%d].Marker = %q, want the renumbered %q", i, c.Marker, wantMarker)
				}
				if !strings.Contains(answer, c.Marker) {
					t.Errorf("citation marker %s does not appear in the answer %q", c.Marker, answer)
				}
			}
			// And no marker survives in the text without a citation behind it.
			for _, match := range markerPattern.FindAllString(answer, -1) {
				number, ok := markerNumber(match)
				if !ok || number < 1 || number > len(resolved) {
					t.Errorf("answer %q carries marker %s, which no citation backs", answer, match)
				}
			}
		})
	}
}

// itoa avoids pulling strconv into the assertions for one conversion.
func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}
