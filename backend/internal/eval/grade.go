package eval

import (
	"fmt"
	"strings"

	"cortex/internal/store"
)

// Metric keys, as reported in the per-case JSONL and the summary table.
// Citation accuracy (REQ-6.2) is deliberately two metrics — the mechanical
// source overlap and the judged snippet support — so a regression names its
// half.
const (
	MetricCorrectness     = "correctness"
	MetricFaithfulness    = "faithfulness"
	MetricCitationSources = "citation_sources"
	MetricCitationSupport = "citation_support"
	MetricEfficiency      = "efficiency"
)

// MetricNames is the fixed reporting order.
var MetricNames = []string{
	MetricCorrectness,
	MetricFaithfulness,
	MetricCitationSources,
	MetricCitationSupport,
	MetricEfficiency,
}

// MetricResult is one graded metric for one case.
type MetricResult struct {
	Pass bool `json:"pass"`
	// Detail explains the verdict: the judge's reasoning, or which expected
	// sources went unmatched.
	Detail string `json:"detail,omitempty"`
	// Err is set when the grading itself failed (a judge call errored). The
	// metric counts as failed, but the reason is kept apart from a genuine
	// quality verdict.
	Err string `json:"error,omitempty"`
}

// MatchSources reports which expected sources were found among the run's
// evidence and which were not. A source matches an evidence row when it is a
// case-insensitive substring of the row's external_id, title, or URL (spec
// A3): Jira keys are assigned at seed time and Gmail/Notion external ids are
// opaque, so case authors write issue summaries, page titles, and mail
// subjects instead.
func MatchSources(expected []string, evidence []store.Evidence) (matched, missing []string) {
	for _, want := range expected {
		needle := strings.ToLower(strings.TrimSpace(want))
		found := false
		for _, e := range evidence {
			if evidenceMatches(needle, e) {
				found = true
				break
			}
		}
		if found {
			matched = append(matched, want)
		} else {
			missing = append(missing, want)
		}
	}
	return matched, missing
}

// evidenceMatches reports whether one evidence row carries the needle.
func evidenceMatches(needle string, e store.Evidence) bool {
	if needle == "" {
		return false
	}
	if strings.Contains(strings.ToLower(e.ExternalID), needle) {
		return true
	}
	if e.Title != nil && strings.Contains(strings.ToLower(*e.Title), needle) {
		return true
	}
	if e.Url != nil && strings.Contains(strings.ToLower(*e.Url), needle) {
		return true
	}
	return false
}

// GradeSourceOverlap passes when every expected source matched some evidence
// the run gathered. Negative cases have no expected sources and pass
// vacuously — the honesty of their answer is correctness's job.
func GradeSourceOverlap(c Case, evidence []store.Evidence) MetricResult {
	if len(c.ExpectedSources) == 0 {
		return MetricResult{Pass: true, Detail: "no expected sources"}
	}
	matched, missing := MatchSources(c.ExpectedSources, evidence)
	if len(missing) > 0 {
		return MetricResult{
			Pass: false,
			Detail: fmt.Sprintf("matched %d/%d expected sources; missing: %s",
				len(matched), len(c.ExpectedSources), strings.Join(missing, "; ")),
		}
	}
	return MetricResult{
		Pass:   true,
		Detail: fmt.Sprintf("matched all %d expected sources", len(matched)),
	}
}

// GradeEfficiency passes when the run stayed within its tool-call budget.
func GradeEfficiency(c Case, toolCalls int) MetricResult {
	if toolCalls > c.MaxToolCalls {
		return MetricResult{
			Pass:   false,
			Detail: fmt.Sprintf("%d tool calls, budget %d", toolCalls, c.MaxToolCalls),
		}
	}
	return MetricResult{
		Pass:   true,
		Detail: fmt.Sprintf("%d tool calls, budget %d", toolCalls, c.MaxToolCalls),
	}
}
