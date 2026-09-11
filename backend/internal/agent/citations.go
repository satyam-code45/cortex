package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"cortex/internal/llm"
	"cortex/internal/store"
)

// The citation pass.
//
// The loop produces prose. This turns that prose into prose whose claims are
// individually traceable: a second, structured call re-reads the answer against
// the numbered list of everything the run actually fetched, and returns the same
// answer with inline [n] markers plus a mapping from each marker to the evidence
// it rests on.
//
// Two design points are worth stating, because both are easy to get wrong:
//
//   - The model never sees an evidence UUID. It cites by the sequence number it
//     was shown (evidence.seq, assigned when the tool call landed). Handing a
//     model opaque identifiers and asking it to copy them back is how you get
//     citations that point at nothing.
//   - The pass cannot fail the run. By the time it runs, the investigation has
//     already made a dozen paid calls and produced a usable answer; losing that
//     because a citation call timed out or returned malformed JSON would be a
//     spectacularly bad trade. Every failure path stores the uncited draft.

// maxEvidenceInPrompt caps the numbered list handed to the citation call.
//
// A long investigation can touch a hundred documents, and the list is sent in
// full alongside the whole transcript. The cap is on the *listed* evidence only:
// the rows all remain in the database and in the trace, they simply cannot be
// cited. Ordering is by seq, so what survives is what the run found first, which
// is also what its narrative is built on.
const maxEvidenceInPrompt = 40

// citationSchema is the structured-output contract for the citation pass.
//
// Strict mode (see llm.Schema) requires every property to be listed in
// "required" and every object to set additionalProperties:false, which is why
// there are no optional fields here — an empty claim string is expressible, an
// absent one is not.
var citationSchema = llm.Schema{
	Name:        "cited_answer",
	Description: "The final answer with inline [n] markers and the evidence each marker cites.",
	Definition: json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["answer_markdown", "citations"],
  "properties": {
    "answer_markdown": {
      "type": "string",
      "description": "The answer in Markdown, with inline [n] markers placed immediately after the claims they support."
    },
    "citations": {
      "type": "array",
      "description": "One entry per distinct [n] marker used in answer_markdown.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["marker", "evidence_id", "claim"],
        "properties": {
          "marker": {
            "type": "string",
            "description": "The marker exactly as it appears in answer_markdown, e.g. \"[2]\"."
          },
          "evidence_id": {
            "type": "integer",
            "description": "The number of the evidence item from the EVIDENCE list that supports this claim."
          },
          "claim": {
            "type": "string",
            "description": "The specific sentence or clause in the answer that this evidence supports."
          }
        }
      }
    }
  }
}`),
}

// citationDraft is what the model returns.
type citationDraft struct {
	AnswerMarkdown string          `json:"answer_markdown"`
	Citations      []draftCitation `json:"citations"`
}

// draftCitation is one model-proposed citation. EvidenceID is the number from
// the prompt's evidence list, not a database id.
type draftCitation struct {
	Marker     string `json:"marker"`
	EvidenceID int    `json:"evidence_id"`
	Claim      string `json:"claim"`
}

// resolvedCitation is a citation that survived validation, renumbered.
type resolvedCitation struct {
	// Marker is the rewritten inline marker, e.g. "[1]".
	Marker string
	// EvidenceSeq is the evidence.seq the marker points at.
	EvidenceSeq int
	Claim       string
}

// citation is a resolvedCitation bound to its database row, ready to persist.
type citation struct {
	Marker      string
	EvidenceID  uuid.UUID
	EvidenceSeq int
	Claim       string
}

// citationOutcome is what the citation pass hands to complete.
type citationOutcome struct {
	// Answer is the text to store: rewritten with markers when the pass ran,
	// the untouched draft when it did not.
	Answer    string
	Citations []citation
	// Dropped counts citations the validator refused.
	Dropped int
	// EvidenceCount is how many evidence items the pass was offered. It is the
	// denominator that makes Dropped readable.
	EvidenceCount int
	// Skipped explains why no citations were produced, and is empty when the
	// pass ran normally. It is persisted in the run event so a trace shows the
	// difference between "nothing was citable" and "the pass never ran".
	Skipped string
}

// markerPattern matches an inline citation marker.
var markerPattern = regexp.MustCompile(`\[(\d+)\]`)

// cite runs the citation pass over a draft answer.
//
// It never returns an error: every failure degrades to the uncited draft with a
// reason recorded on the outcome.
func (o *Orchestrator) cite(ctx context.Context, state *runState, draft string) citationOutcome {
	// evidenceCount is captured as the rows load, so a failure AFTER that point
	// still reports how much evidence the run actually had. Reporting zero there
	// would say "nothing was citable" about a run with a dozen sources, which is
	// the opposite of what happened and defeats the field's purpose.
	evidenceCount := 0
	uncited := func(reason string) citationOutcome {
		return citationOutcome{Answer: draft, Skipped: reason, EvidenceCount: evidenceCount}
	}

	var rows []store.Evidence
	if err := o.withTx(ctx, func(q store.Querier) error {
		loaded, err := q.ListEvidenceByRun(ctx, state.runID)
		if err != nil {
			return fmt.Errorf("list evidence: %w", err)
		}
		rows = loaded
		return nil
	}); err != nil {
		o.logger.Error("agent: could not load evidence for citation pass",
			"run_id", state.runID, "error", err)
		return uncited("evidence could not be loaded")
	}
	if len(rows) == 0 {
		// A run that answered from the conversation alone, or one whose every
		// tool call failed. Nothing to cite is a legitimate outcome, not an error.
		return uncited("the run gathered no evidence")
	}
	evidenceCount = len(rows)
	if len(rows) > maxEvidenceInPrompt {
		o.logger.Info("agent: truncating evidence list for citation pass",
			"run_id", state.runID, "total", len(rows), "listed", maxEvidenceInPrompt)
		rows = rows[:maxEvidenceInPrompt]
		evidenceCount = len(rows)
	}

	resp, err := o.generateCitations(ctx, state, draft, rows)
	if err != nil {
		o.logger.Warn("agent: citation pass failed, storing the uncited answer",
			"run_id", state.runID, "error", err)
		return uncited("the citation call failed")
	}

	var parsed citationDraft
	if err := json.Unmarshal([]byte(resp.Text), &parsed); err != nil {
		o.logger.Warn("agent: citation pass returned unparseable JSON, storing the uncited answer",
			"run_id", state.runID, "error", err)
		return uncited("the citation call returned unparseable output")
	}
	if strings.TrimSpace(parsed.AnswerMarkdown) == "" {
		o.logger.Warn("agent: citation pass returned an empty answer, storing the uncited draft",
			"run_id", state.runID)
		return uncited("the citation call returned an empty answer")
	}

	answer, resolved, dropped := applyCitations(parsed, len(rows))
	if dropped > 0 {
		// The hallucinated-citation guard firing. Logged at warn because it is
		// the signal that the model is inventing sources, which is the exact
		// failure citations exist to make visible.
		o.logger.Warn("agent: dropped citations that did not resolve to evidence",
			"run_id", state.runID, "dropped", dropped, "kept", len(resolved), "evidence", len(rows))
	}

	byMarkerSeq := make(map[int]uuid.UUID, len(rows))
	for _, row := range rows {
		byMarkerSeq[int(row.Seq)] = row.ID
	}
	out := citationOutcome{Answer: answer, Dropped: dropped, EvidenceCount: len(rows)}
	for _, c := range resolved {
		id, ok := byMarkerSeq[c.EvidenceSeq]
		if !ok {
			// Unreachable today: applyCitations range-checks against
			// len(rows), and InsertEvidence numbers a run's evidence
			// contiguously from 1, so a number in range always names a row.
			//
			// Logged rather than passed over silently because the failure it
			// would represent is invisible from the outside: the answer keeps a
			// rendered [n] marker while no citation row is written for it, so the
			// trace shows a marker resolving to nothing. If evidence numbering
			// ever gains gaps, this is the line that says so.
			o.logger.Error("agent: citation marker resolved to no evidence row",
				"run_id", state.runID, "marker", c.Marker, "evidence_seq", c.EvidenceSeq)
			continue
		}
		out.Citations = append(out.Citations, citation{
			Marker:      c.Marker,
			EvidenceID:  id,
			EvidenceSeq: c.EvidenceSeq,
			Claim:       c.Claim,
		})
	}
	return out
}

// applyCitations validates and renumbers a model-produced citation draft.
//
// It is a pure function over the model's output — no database, no clock — which
// is what makes the hallucinated-citation guard testable in isolation.
//
// Three things happen, in this order:
//
//  1. A citation whose evidence_id is not a number in 1..evidenceCount is
//     dropped. This is the guard: the model is perfectly capable of citing
//     evidence [9] in a run that gathered four items.
//  2. The answer is scanned for [n] markers in order of appearance. A marker
//     backed by a surviving citation is renumbered to the next free number
//     starting at 1; a marker backed by nothing is deleted from the text.
//     Renumbering is what makes the rendered answer read [1][2][3] rather than
//     whatever numbers the model happened to pick, and it guarantees the markers
//     and the citation list agree.
//  3. A citation whose marker never appears in the text is dropped: it cannot be
//     rendered, so persisting it would put a row in the trace that no part of
//     the answer points at.
//
// Returns the rewritten answer, the surviving citations in marker order, and how
// many were dropped.
func applyCitations(draft citationDraft, evidenceCount int) (string, []resolvedCitation, int) {
	dropped := 0

	// The model addresses each citation by the marker it wrote into the text,
	// so the text is the join key. A repeated marker is a contradiction (one
	// marker cannot cite two things) and the first wins.
	byMarker := make(map[int]draftCitation, len(draft.Citations))
	for _, c := range draft.Citations {
		number, ok := markerNumber(c.Marker)
		if !ok || c.EvidenceID < 1 || c.EvidenceID > evidenceCount {
			dropped++
			continue
		}
		if _, duplicate := byMarker[number]; duplicate {
			dropped++
			continue
		}
		byMarker[number] = c
	}

	var (
		resolved []resolvedCitation
		assigned = make(map[int]int, len(byMarker))
		removed  bool
	)
	answer := markerPattern.ReplaceAllStringFunc(draft.AnswerMarkdown, func(match string) string {
		number, ok := markerNumber(match)
		if !ok {
			return match
		}
		if renumbered, seen := assigned[number]; seen {
			return fmt.Sprintf("[%d]", renumbered)
		}
		source, backed := byMarker[number]
		if !backed {
			removed = true
			return ""
		}
		renumbered := len(resolved) + 1
		assigned[number] = renumbered
		resolved = append(resolved, resolvedCitation{
			Marker:      fmt.Sprintf("[%d]", renumbered),
			EvidenceSeq: source.EvidenceID,
			Claim:       source.Claim,
		})
		return fmt.Sprintf("[%d]", renumbered)
	})

	// Citations the model listed but never placed in the text.
	dropped += len(byMarker) - len(assigned)

	if removed {
		answer = tidyAfterRemoval(answer)
	}
	return answer, resolved, dropped
}

// markerNumber extracts the digits from a marker, accepting both "[3]" and "3"
// because models write it both ways.
func markerNumber(marker string) (int, bool) {
	trimmed := strings.TrimSpace(marker)
	trimmed = strings.TrimSuffix(strings.TrimPrefix(trimmed, "["), "]")
	number, err := strconv.Atoi(strings.TrimSpace(trimmed))
	if err != nil || number < 1 {
		return 0, false
	}
	return number, true
}

var (
	spaceBeforePunctuation = regexp.MustCompile(`(\S)[ \t]+([.,;:!?)\]])`)
	repeatedSpace          = regexp.MustCompile(`(\S)[ \t]{2,}`)
)

// tidyAfterRemoval repairs the whitespace a deleted marker leaves behind.
//
// Only called when a marker was actually removed: "the answer [4]." becoming
// "the answer ." is the artefact being fixed.
//
// Both patterns require a non-space character immediately before the run they
// collapse, which confines them to gaps left mid-line and keeps them away from
// leading indentation. That matters more than it looks: the answer is Markdown,
// and a rule that collapsed runs of spaces anywhere would re-indent a nested
// list item up to its parent level and turn a four-space code block into a
// paragraph — reformatting the model's deliberate structure while cleaning up
// after ourselves. Trailing whitespace is left alone for the same reason: two
// spaces at end of line is a Markdown hard break.
func tidyAfterRemoval(answer string) string {
	answer = spaceBeforePunctuation.ReplaceAllString(answer, "$1$2")
	answer = repeatedSpace.ReplaceAllString(answer, "$1 ")
	return answer
}

// generateCitations makes the structured provider call.
//
// The injected turns are appended to state.messages here, exactly as generate
// does, and recorded on the llm_call event. Both halves matter: pause/resume
// rebuilds the transcript from these events to continue a run, so a turn that
// entered the conversation without being recorded makes the replay diverge
// from what the model actually saw.
func (o *Orchestrator) generateCitations(
	ctx context.Context,
	state *runState,
	draft string,
	rows []store.Evidence,
) (llm.Response, error) {
	// The evidence list is fenced for the same reason tool observations are, and
	// the stakes here are higher rather than lower: this call's answer_markdown
	// REPLACES the stored answer, so text that talks the model into rewriting it
	// changes what the user is shown while the trace still looks like a normal
	// run. Every snippet in the list is third-party text — a Jira comment, an
	// email body, an indexed chunk — that anyone with access to those systems can
	// write.
	injected := []llm.Message{
		{Role: llm.RoleAssistant, Content: draft},
		{Role: llm.RoleUser, Content: citationInstruction + "\n\nEVIDENCE\n" +
			fence("evidence", "", "untrusted", renderEvidence(rows))},
	}
	state.messages = append(state.messages, injected...)

	callCtx, cancel := context.WithTimeout(ctx, llmTimeout)
	defer cancel()

	request := llm.Request{Model: o.model, System: state.system, Messages: state.messages}

	start := o.now()
	resp, err := state.provider.GenerateStructured(callCtx, request, citationSchema)
	latency := o.now().Sub(start)
	if err != nil {
		return llm.Response{}, &providerError{err: fmt.Errorf("citation generation: %w", err)}
	}

	state.inputTokens += resp.InputTokens
	state.outputTokens += resp.OutputTokens

	if err := o.recordLLMCall(ctx, state, state.iterations, PurposeAnswerCitations,
		o.model, request, resp, latency, injected); err != nil {
		return llm.Response{}, err
	}
	return resp, nil
}

// renderEvidence formats the numbered evidence list the model cites against.
//
// The number leading each line is evidence.seq, and it is the only handle the
// model is given — everything else on the line exists so it can tell the items
// apart and place each marker on the right claim.
func renderEvidence(rows []store.Evidence) string {
	var b strings.Builder
	for _, row := range rows {
		fmt.Fprintf(&b, "[%d] %s %s", row.Seq, row.Source, row.ExternalID)
		if title := deref(row.Title); title != "" {
			fmt.Fprintf(&b, " — %s", title)
		}
		if row.SourceTimestamp.Valid {
			fmt.Fprintf(&b, " (%s)", row.SourceTimestamp.Time.UTC().Format(time.DateOnly))
		}
		b.WriteString("\n")
		if snippet := deref(row.Snippet); snippet != "" {
			fmt.Fprintf(&b, "    %s\n", snippet)
		}
	}
	return b.String()
}

// deref reads a nullable text column.
func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
