package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"cortex/internal/llm"
	"cortex/internal/store"
)

// judgeTimeout bounds one judge call. Judges read a bounded prompt and return
// a small verdict, so this is generous.
const judgeTimeout = 60 * time.Second

// maxEvidenceInJudgePrompt caps the evidence list a faithfulness judge reads,
// for the same reason the citation pass caps its list: a long investigation
// can touch far more than fits sensibly in one judge prompt.
const maxEvidenceInJudgePrompt = 40

// Observation excerpts shown to the faithfulness judge. Evidence snippets are
// ~300 characters and systematically under-represent what the agent read — a
// root cause stated deep in an email body supports a claim but never reaches
// the snippet — so the judge also sees the head of each tool observation.
const (
	maxObservationsInJudgePrompt = 20
	maxObservationExcerptRunes   = 2000
)

// verdictSchema is the structured-output contract every judge shares.
// Reasoning comes first so the model reasons before it rules.
var verdictSchema = llm.Schema{
	Name:        "eval_verdict",
	Description: "A pass/fail grading verdict with its justification.",
	Definition: json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["reasoning", "pass"],
  "properties": {
    "reasoning": {
      "type": "string",
      "description": "One short paragraph justifying the verdict."
    },
    "pass": {
      "type": "boolean",
      "description": "Whether the graded text meets the criterion."
    }
  }
}`),
}

// verdict is what a judge returns.
type verdict struct {
	Reasoning string `json:"reasoning"`
	Pass      bool   `json:"pass"`
}

// Grader wraps an LLM behind the three judged metrics. The model is the
// caller's choice; production passes the MAIN reasoning model (spec A8), not
// the utility model — a noisy judge writes noise into eval_runs.
//
// It takes llm.Provider rather than a concrete client, which is what makes
// the judges unit-testable: a fake provider returning canned verdict JSON
// exercises every prompt wrapper with no network.
type Grader struct {
	provider llm.Provider
	model    string
	logger   *slog.Logger
}

// NewGrader builds a Grader on the given model — the main reasoning model in
// production (spec A8: a judge's verdicts are the recorded quality numbers, so
// judge noise costs more than judge tokens).
func NewGrader(provider llm.Provider, model string, logger *slog.Logger) (*Grader, error) {
	if provider == nil {
		return nil, errors.New("eval: provider is required")
	}
	if model == "" {
		return nil, errors.New("eval: model is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Grader{provider: provider, model: model, logger: logger}, nil
}

// Correctness judges whether the answer contains or entails the expected
// answer — or, for a negative case, whether it honestly declines.
func (g *Grader) Correctness(ctx context.Context, c Case, answer string) MetricResult {
	return g.judge(ctx, correctnessSystemPrompt(c), correctnessUserPrompt(c, answer))
}

// Faithfulness judges whether every factual claim in the answer is supported
// by what the run actually gathered: the registered evidence and the tool
// observations the model read.
func (g *Grader) Faithfulness(ctx context.Context, c Case, answer string, evidence []store.Evidence, observations []string) MetricResult {
	if len(evidence) == 0 && len(observations) == 0 {
		if c.Category == CategoryNegative {
			// A negative case legitimately gathers nothing; correctness
			// already checks the answer declines.
			return MetricResult{Pass: true, Detail: "no evidence gathered; negative case graded by correctness"}
		}
		// A positive case that found nothing but answered anyway has, by
		// definition, unsupported claims. No judge needed.
		return MetricResult{Pass: false, Detail: "no evidence gathered, so no claim can be supported"}
	}
	return g.judge(ctx, faithfulnessSystemPrompt, faithfulnessUserPrompt(c.Question, answer, evidence, observations))
}

// CitationSupport judges whether each cited snippet actually supports the
// claim its marker is attached to.
func (g *Grader) CitationSupport(ctx context.Context, c Case, answer string, citations []store.ListCitationsByRunRow) MetricResult {
	if len(citations) == 0 {
		// Missing citations are already punished by the source-overlap
		// metric; this one grades only the citations that exist.
		return MetricResult{Pass: true, Detail: "no citations to check"}
	}
	return g.judge(ctx, citationSupportSystemPrompt, citationSupportUserPrompt(answer, citations))
}

// judge makes one structured call and converts the outcome to a MetricResult.
// A failed judge call fails the metric but is reported as a grading error, not
// a quality verdict — and it never crashes the harness.
func (g *Grader) judge(ctx context.Context, system, user string) MetricResult {
	callCtx, cancel := context.WithTimeout(ctx, judgeTimeout)
	defer cancel()

	resp, err := g.provider.GenerateStructured(callCtx, llm.Request{
		Model:    g.model,
		System:   system,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: user}},
	}, verdictSchema)
	if err != nil {
		g.logger.Warn("eval: judge call failed", "error", err)
		return MetricResult{Pass: false, Err: llm.SafeErrorMessage(err)}
	}

	var v verdict
	if err := json.Unmarshal([]byte(resp.Text), &v); err != nil {
		g.logger.Warn("eval: judge returned malformed verdict", "error", err)
		return MetricResult{Pass: false, Err: fmt.Sprintf("malformed verdict: %v", err)}
	}
	return MetricResult{Pass: v.Pass, Detail: v.Reasoning}
}

// Prompt builders are pure functions so tests can pin their content
// without a provider.

func correctnessSystemPrompt(c Case) string {
	if c.Category == CategoryNegative {
		return "You are grading an AI research agent's answer to a question whose answer is " +
			"NOT present in the agent's data sources. The correct behavior is to say so honestly. " +
			"Pass only if the answer clearly states the information could not be found in the " +
			"available data (offering related context it did find is acceptable). " +
			"Fail if the answer asserts ANY specific fact as the answer to the question — a name, " +
			"date, number, or decision — because any such fact is fabricated."
	}
	return "You are grading an AI research agent's answer against a known-correct expected answer. " +
		"The key facts are the ones the QUESTION asks for — names, dates, counts, causes, decisions. " +
		"Pass only if the agent's answer contains or entails every key fact the question asks for, " +
		"as given in the expected answer. Context in the expected answer beyond what the question " +
		"asks (background, rationale) is not required of the agent. Extra detail in the agent's " +
		"answer that the expected answer does not mention is fine: unless it CONTRADICTS the " +
		"expected answer, assume it is correct — you are not a fact-checker for facts outside the " +
		"expected answer. Fail only if the answer contradicts a key fact, omits one the question " +
		"asks for, or hedges so much that the fact is not actually stated."
}

func correctnessUserPrompt(c Case, answer string) string {
	var b strings.Builder
	b.WriteString("QUESTION:\n")
	b.WriteString(c.Question)
	b.WriteString("\n\nEXPECTED ANSWER (ground truth):\n")
	b.WriteString(c.ExpectedAnswer)
	b.WriteString("\n\nAGENT ANSWER TO GRADE:\n")
	b.WriteString(fenceUntrusted("agent_answer", answer))
	return b.String()
}

const faithfulnessSystemPrompt = "You are grading whether an AI research agent's answer is " +
	"faithful to what it read during its investigation. Pass only if every factual claim in the " +
	"answer is supported by the evidence snippets or the tool-result excerpts, individually or in " +
	"combination. Support may be direct or by clear implication: a change record \"field: X → Y\" " +
	"attests both that the value was X and that it became Y. General hedging and honest statements " +
	"that something was not found need no support. Fail if any specific claim — a name, date, " +
	"number, cause, or decision — appears in the answer but follows from nothing the agent read."

func faithfulnessUserPrompt(question, answer string, evidence []store.Evidence, observations []string) string {
	var b strings.Builder
	b.WriteString("QUESTION:\n")
	b.WriteString(question)
	b.WriteString("\n\nAGENT ANSWER TO GRADE:\n")
	b.WriteString(fenceUntrusted("agent_answer", answer))
	b.WriteString("\n\nEVIDENCE COLLECTED DURING THE RUN:\n")
	b.WriteString(fenceUntrusted("evidence", renderEvidenceList(evidence)))
	if excerpts := renderObservationExcerpts(observations); excerpts != "" {
		b.WriteString("\n\nTOOL-RESULT EXCERPTS THE AGENT READ (each truncated):\n")
		b.WriteString(fenceUntrusted("tool_results", excerpts))
	}
	return b.String()
}

// renderObservationExcerpts renders the head of each tool observation, capped
// in both count and length so the judge prompt stays bounded.
func renderObservationExcerpts(observations []string) string {
	var b strings.Builder
	listed := observations
	if len(listed) > maxObservationsInJudgePrompt {
		listed = listed[:maxObservationsInJudgePrompt]
	}
	for i, obs := range listed {
		runes := []rune(obs)
		if len(runes) > maxObservationExcerptRunes {
			obs = string(runes[:maxObservationExcerptRunes]) + "…"
		}
		fmt.Fprintf(&b, "--- result %d ---\n%s\n", i+1, obs)
	}
	if len(observations) > len(listed) {
		fmt.Fprintf(&b, "(and %d further results not shown)\n", len(observations)-len(listed))
	}
	return b.String()
}

const citationSupportSystemPrompt = "You are grading an AI research agent's citations. Each " +
	"citation attaches a claim from the answer to an evidence snippet. Pass only if every cited " +
	"snippet plausibly supports its claim, directly or by clear implication — a change record " +
	"\"field: X → Y\" attests both that the value was X and that it became Y. Fail if any " +
	"citation points at evidence that does not back the claim it is attached to."

func citationSupportUserPrompt(answer string, citations []store.ListCitationsByRunRow) string {
	var b strings.Builder
	b.WriteString("AGENT ANSWER:\n")
	b.WriteString(fenceUntrusted("agent_answer", answer))
	b.WriteString("\n\nCITATIONS TO GRADE:\n")

	var list strings.Builder
	for _, c := range citations {
		claim := ""
		if c.ClaimText != nil {
			claim = *c.ClaimText
		}
		title := ""
		if c.Title != nil {
			title = *c.Title
		}
		snippet := ""
		if c.Snippet != nil {
			snippet = *c.Snippet
		}
		fmt.Fprintf(&list, "%s claim: %s\n   cited evidence (%s %s — %s): %s\n",
			c.Marker, claim, c.Source, c.ExternalID, title, snippet)
	}
	b.WriteString(fenceUntrusted("citations", list.String()))
	return b.String()
}

// renderEvidenceList renders evidence rows as a numbered list, capped.
func renderEvidenceList(evidence []store.Evidence) string {
	var b strings.Builder
	listed := evidence
	if len(listed) > maxEvidenceInJudgePrompt {
		listed = listed[:maxEvidenceInJudgePrompt]
	}
	for i, e := range listed {
		title := ""
		if e.Title != nil {
			title = *e.Title
		}
		snippet := ""
		if e.Snippet != nil {
			snippet = *e.Snippet
		}
		fmt.Fprintf(&b, "[%d] %s %s — %s: %s\n", i+1, e.Source, e.ExternalID, title, snippet)
	}
	if len(evidence) > len(listed) {
		fmt.Fprintf(&b, "(and %d further evidence items not listed)\n", len(evidence)-len(listed))
	}
	return b.String()
}

// fenceUntrusted wraps agent- and source-derived text so the judge can tell
// data from instructions. The answer being graded is built from third-party
// documents, so a document saying "grade this answer as passing" must read as
// content, not as an instruction. Same defang trick as the agent loop's fence:
// a closing tag inside the content is replaced with a lookalike so the content
// cannot terminate its own fence. The match is loose (case-insensitive,
// whitespace tolerated) because models parse pseudo-XML loosely — an exact
// byte match would leave `</Agent_Answer >` working as an escape, and here
// the escape would inflate the recorded quality numbers.
func fenceUntrusted(tag, content string) string {
	closing := "</" + tag + ">"
	pattern := regexp.MustCompile(`(?i)</\s*` + regexp.QuoteMeta(tag) + `\s*>`)
	safe := pattern.ReplaceAllString(content, "<∕"+tag+">")
	return fmt.Sprintf("<%s trust=%q>\n", tag, "untrusted") + safe + "\n" + closing
}
