package eval_test

import (
	"strings"
	"testing"

	"cortex/internal/eval"
)

// Summary aggregation: the summary table reports pass rates per
// metric and category.

func TestSummarize(t *testing.T) {
	pass := eval.MetricResult{Pass: true}
	fail := eval.MetricResult{Pass: false}

	results := []eval.CaseResult{
		{
			CaseID:   "single-a",
			Category: eval.CategorySingleHop,
			Metrics: map[string]eval.MetricResult{
				eval.MetricCorrectness:  pass,
				eval.MetricFaithfulness: pass,
				eval.MetricEfficiency:   pass,
			},
		},
		{
			CaseID:   "single-b",
			Category: eval.CategorySingleHop,
			Metrics: map[string]eval.MetricResult{
				eval.MetricCorrectness:  fail,
				eval.MetricFaithfulness: pass,
				// Efficiency deliberately ungraded (harness error): it must
				// not count toward the metric's total.
			},
		},
		{
			CaseID:   "neg-a",
			Category: eval.CategoryNegative,
			Metrics: map[string]eval.MetricResult{
				eval.MetricCorrectness: pass,
			},
		},
	}

	s := eval.Summarize(results)

	if s.Cases != 3 {
		t.Errorf("Cases = %d, want 3", s.Cases)
	}

	wantOverall := map[string]eval.PassRate{
		eval.MetricCorrectness:  {Passed: 2, Total: 3},
		eval.MetricFaithfulness: {Passed: 2, Total: 2},
		eval.MetricEfficiency:   {Passed: 1, Total: 1},
	}
	for metric, want := range wantOverall {
		if got := s.Overall[metric]; got != want {
			t.Errorf("Overall[%s] = %+v, want %+v", metric, got, want)
		}
	}
	// A metric no case graded must not appear with a phantom total.
	if got := s.Overall[eval.MetricCitationSupport]; got.Total != 0 {
		t.Errorf("Overall[%s].Total = %d, want 0 (nothing graded it)", eval.MetricCitationSupport, got.Total)
	}

	if got := s.ByCategory[eval.CategorySingleHop][eval.MetricCorrectness]; got != (eval.PassRate{Passed: 1, Total: 2}) {
		t.Errorf("ByCategory[single-hop][correctness] = %+v, want 1/2", got)
	}
	if got := s.ByCategory[eval.CategoryNegative][eval.MetricCorrectness]; got != (eval.PassRate{Passed: 1, Total: 1}) {
		t.Errorf("ByCategory[negative][correctness] = %+v, want 1/1", got)
	}
	if _, present := s.ByCategory[eval.CategoryMultiHop]; present {
		t.Error("ByCategory contains multi-hop although no case had that category")
	}
}

func TestPassRateRate(t *testing.T) {
	tests := []struct {
		name string
		rate eval.PassRate
		want float64
	}{
		{name: "half", rate: eval.PassRate{Passed: 1, Total: 2}, want: 0.5},
		{name: "all", rate: eval.PassRate{Passed: 3, Total: 3}, want: 1},
		// Empty cells must not divide by zero.
		{name: "empty", rate: eval.PassRate{}, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rate.Rate(); got != tt.want {
				t.Errorf("Rate() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Render is what the acceptance test reads off the terminal; the numbers in it
// must be the aggregate's, and an ungraded cell must read as absent, not 0%.
func TestSummaryRender(t *testing.T) {
	s := eval.Summarize([]eval.CaseResult{
		{
			Category: eval.CategorySingleHop,
			Metrics: map[string]eval.MetricResult{
				eval.MetricCorrectness: {Pass: true},
			},
		},
		{
			Category: eval.CategorySingleHop,
			Metrics: map[string]eval.MetricResult{
				eval.MetricCorrectness: {Pass: false},
			},
		},
	})

	table := s.Render()
	for _, want := range []string{
		eval.MetricCorrectness,
		eval.CategorySingleHop,
		"1/2 (50%)",
		// Metrics nothing graded render as an empty cell.
		"-",
	} {
		if !strings.Contains(table, want) {
			t.Errorf("rendered table missing %q:\n%s", want, table)
		}
	}
}
