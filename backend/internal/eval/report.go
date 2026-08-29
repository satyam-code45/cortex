package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/store"
)

// CaseResult is one graded case — one line in the JSONL output.
type CaseResult struct {
	CaseID   string    `json:"case_id"`
	Category string    `json:"category"`
	RunID    uuid.UUID `json:"run_id"`
	// Status is the agent run's terminal status: completed or failed.
	Status       string                  `json:"status"`
	Answer       string                  `json:"answer,omitempty"`
	RunError     string                  `json:"run_error,omitempty"`
	ToolCalls    int                     `json:"tool_calls"`
	InputTokens  int                     `json:"input_tokens"`
	OutputTokens int                     `json:"output_tokens"`
	LatencyMS    int64                   `json:"latency_ms"`
	Metrics      map[string]MetricResult `json:"metrics"`
	// HarnessError is set when the harness itself could not run the case
	// (setup failed, read-back failed). Every metric fails; the error names
	// the harness, not the agent.
	HarnessError string `json:"harness_error,omitempty"`
}

// PassRate is passed-over-total for one metric.
type PassRate struct {
	Passed int `json:"passed"`
	Total  int `json:"total"`
}

// Rate renders as a fraction of 1, or 0 for an empty cell.
func (r PassRate) Rate() float64 {
	if r.Total == 0 {
		return 0
	}
	return float64(r.Passed) / float64(r.Total)
}

// Summary is the aggregate the CLI prints and eval_runs persists.
type Summary struct {
	Cases int `json:"cases"`
	// Overall is metric → pass rate across every case.
	Overall map[string]PassRate `json:"overall"`
	// ByCategory is category → metric → pass rate.
	ByCategory map[string]map[string]PassRate `json:"by_category"`
	// GradingErrors counts, per metric, cases where the grading itself failed
	// (a judge call errored). They are excluded from the pass rates: a burst
	// of 429s on the judge model must not be persisted to eval_runs as a
	// quality regression and invite fixes to an agent that was not wrong.
	GradingErrors map[string]int `json:"grading_errors,omitempty"`
}

// Summarize aggregates per-case results into pass rates.
func Summarize(results []CaseResult) Summary {
	s := Summary{
		Cases:      len(results),
		Overall:    make(map[string]PassRate),
		ByCategory: make(map[string]map[string]PassRate),
	}
	for _, r := range results {
		if s.ByCategory[r.Category] == nil {
			s.ByCategory[r.Category] = make(map[string]PassRate)
		}
		for _, metric := range MetricNames {
			m, graded := r.Metrics[metric]
			if !graded {
				continue
			}
			if m.Err != "" {
				// A grading error, not a verdict — see Summary.GradingErrors.
				if s.GradingErrors == nil {
					s.GradingErrors = make(map[string]int)
				}
				s.GradingErrors[metric]++
				continue
			}
			overall := s.Overall[metric]
			overall.Total++
			byCat := s.ByCategory[r.Category][metric]
			byCat.Total++
			if m.Pass {
				overall.Passed++
				byCat.Passed++
			}
			s.Overall[metric] = overall
			s.ByCategory[r.Category][metric] = byCat
		}
	}
	return s
}

// Render formats the summary as a fixed-width table.
func (s Summary) Render() string {
	// Categories in a stable, meaningful order; anything unexpected goes last.
	order := []string{CategorySingleHop, CategoryMultiHop, CategoryCrossSource, CategoryNegative}
	var extra []string
	for cat := range s.ByCategory {
		if !slices.Contains(order, cat) {
			extra = append(extra, cat)
		}
	}
	sort.Strings(extra)
	var columns []string
	for _, cat := range append(order, extra...) {
		if _, present := s.ByCategory[cat]; present {
			columns = append(columns, cat)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%-18s %-9s", "metric", "overall")
	for _, cat := range columns {
		fmt.Fprintf(&b, " %-13s", cat)
	}
	b.WriteString("\n")

	cell := func(r PassRate) string {
		if r.Total == 0 {
			return "-"
		}
		return fmt.Sprintf("%d/%d (%.0f%%)", r.Passed, r.Total, r.Rate()*100)
	}
	for _, metric := range MetricNames {
		fmt.Fprintf(&b, "%-18s %-9s", metric, cell(s.Overall[metric]))
		for _, cat := range columns {
			fmt.Fprintf(&b, " %-13s", cell(s.ByCategory[cat][metric]))
		}
		b.WriteString("\n")
	}
	if len(s.GradingErrors) > 0 {
		var parts []string
		for _, metric := range MetricNames {
			if n := s.GradingErrors[metric]; n > 0 {
				parts = append(parts, fmt.Sprintf("%s: %d", metric, n))
			}
		}
		fmt.Fprintf(&b, "\ngrading errors (excluded from the rates above): %s\n", strings.Join(parts, ", "))
	}
	return b.String()
}

// WriteJSONL writes one line per case result to
// <dir>/<started RFC3339>-<short sha>.jsonl and returns the path.
func WriteJSONL(dir, gitSHA string, startedAt time.Time, results []CaseResult) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create results directory: %w", err)
	}
	name := fmt.Sprintf("%s-%s.jsonl", startedAt.UTC().Format("2006-01-02T15-04-05Z"), gitSHA)
	path := filepath.Join(dir, name)

	f, err := os.Create(path)
	if err != nil {
		return "", fmt.Errorf("create results file: %w", err)
	}
	encoder := json.NewEncoder(f)
	for _, r := range results {
		if err := encoder.Encode(r); err != nil {
			f.Close() //nolint:errcheck // the encode error is the one worth reporting
			return "", fmt.Errorf("write result %s: %w", r.CaseID, err)
		}
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close results file: %w", err)
	}
	return path, nil
}

// Persist records the summary in eval_runs so regressions are visible across
// runs with a single SQL query.
func Persist(ctx context.Context, db *pgxpool.Pool, gitSHA string, startedAt time.Time, s Summary) error {
	metrics, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("marshal summary: %w", err)
	}
	q := store.New(db)
	if _, err := q.InsertEvalRun(ctx, store.InsertEvalRunParams{
		GitSha:    gitSHA,
		StartedAt: pgtype.Timestamptz{Time: startedAt.UTC(), Valid: true},
		Metrics:   metrics,
	}); err != nil {
		return fmt.Errorf("insert eval run: %w", err)
	}
	return nil
}
