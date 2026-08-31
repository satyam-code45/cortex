package eval_test

import (
	"strings"
	"testing"

	"cortex/internal/eval"
	"cortex/internal/store"
)

// The mechanical half of grading — the graders that need no judge.
//
// The matching rule for expected_sources is pinned: an expected source
// matches an evidence row when it is a case-insensitive substring of the row's
// external_id, title, or URL — because only Jira evidence carries readable
// external ids, and case authors write Notion page titles and Gmail subjects.

func strPtr(s string) *string { return &s }

func evidenceRow(externalID string, title, url *string) store.Evidence {
	return store.Evidence{Source: "jira", ExternalID: externalID, Title: title, Url: url}
}

func TestMatchSources(t *testing.T) {
	evidence := []store.Evidence{
		evidenceRow("ATLAS-145", strPtr("Payment integration deadline"), strPtr("https://example.atlassian.net/browse/ATLAS-145")),
		// Notion-style: opaque external id, human-readable title.
		evidenceRow("9c2f1b7e-aaaa-bbbb-cccc-000000000001", strPtr("Project Atlas Launch Plan"), nil),
		// Gmail-style: opaque external id, subject as title, no URL.
		evidenceRow("18f3a9d77c1b2e04", strPtr("Re: Vendor sandbox delay"), nil),
		// Row with neither title nor URL must not panic the matcher.
		evidenceRow("BEACON-7", nil, nil),
	}

	tests := []struct {
		name        string
		expected    []string
		wantMatched []string
		wantMissing []string
	}{
		{
			name:        "exact external id",
			expected:    []string{"ATLAS-145"},
			wantMatched: []string{"ATLAS-145"},
		},
		{
			name:        "case-insensitive external id",
			expected:    []string{"atlas-145"},
			wantMatched: []string{"atlas-145"},
		},
		{
			name:        "substring of external id",
			expected:    []string{"BEACON"},
			wantMatched: []string{"BEACON"},
		},
		{
			name:        "title substring, mixed case (A3)",
			expected:    []string{"launch plan"},
			wantMatched: []string{"launch plan"},
		},
		{
			name:        "gmail subject via title (A3)",
			expected:    []string{"Vendor sandbox delay"},
			wantMatched: []string{"Vendor sandbox delay"},
		},
		{
			name:        "url substring (A3)",
			expected:    []string{"browse/atlas-145"},
			wantMatched: []string{"browse/atlas-145"},
		},
		{
			name:        "surrounding whitespace is trimmed",
			expected:    []string{"  ATLAS-145  "},
			wantMatched: []string{"  ATLAS-145  "},
		},
		{
			name:        "unmatched source is reported missing",
			expected:    []string{"COMET-9"},
			wantMissing: []string{"COMET-9"},
		},
		{
			// A blank expected source matching every row would make source
			// overlap vacuously passable; it must match nothing.
			name:        "blank expected source never matches",
			expected:    []string{"   "},
			wantMissing: []string{"   "},
		},
		{
			name:        "mixed matched and missing keep their order",
			expected:    []string{"ATLAS-145", "COMET-9", "Launch Plan"},
			wantMatched: []string{"ATLAS-145", "Launch Plan"},
			wantMissing: []string{"COMET-9"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matched, missing := eval.MatchSources(tt.expected, evidence)
			if !equalStrings(matched, tt.wantMatched) {
				t.Errorf("matched = %v, want %v", matched, tt.wantMatched)
			}
			if !equalStrings(missing, tt.wantMissing) {
				t.Errorf("missing = %v, want %v", missing, tt.wantMissing)
			}
		})
	}
}

func TestGradeSourceOverlap(t *testing.T) {
	evidence := []store.Evidence{
		evidenceRow("ATLAS-145", strPtr("Payment integration deadline"), nil),
		evidenceRow("ATLAS-9", nil, nil),
	}

	tests := []struct {
		name       string
		c          eval.Case
		evidence   []store.Evidence
		wantPass   bool
		wantDetail []string
	}{
		{
			// Negative cases carry no expected sources; overlap passes
			// vacuously — the honesty of the answer is correctness's job.
			name:     "no expected sources passes vacuously",
			c:        eval.Case{Category: eval.CategoryNegative},
			evidence: nil,
			wantPass: true,
		},
		{
			name:     "all expected sources matched",
			c:        eval.Case{ExpectedSources: []string{"ATLAS-145", "ATLAS-9"}},
			evidence: evidence,
			wantPass: true,
		},
		{
			name:       "one missing source fails and is named",
			c:          eval.Case{ExpectedSources: []string{"ATLAS-145", "COMET-1"}},
			evidence:   evidence,
			wantPass:   false,
			wantDetail: []string{"COMET-1", "1/2"},
		},
		{
			name:       "expected sources against no evidence fails",
			c:          eval.Case{ExpectedSources: []string{"ATLAS-145"}},
			evidence:   nil,
			wantPass:   false,
			wantDetail: []string{"ATLAS-145"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := eval.GradeSourceOverlap(tt.c, tt.evidence)
			if got.Pass != tt.wantPass {
				t.Errorf("Pass = %v, want %v (detail: %s)", got.Pass, tt.wantPass, got.Detail)
			}
			for _, want := range tt.wantDetail {
				if !strings.Contains(got.Detail, want) {
					t.Errorf("Detail = %q, want it to mention %q", got.Detail, want)
				}
			}
			if got.Err != "" {
				t.Errorf("Err = %q on a mechanical metric, want empty", got.Err)
			}
		})
	}
}

func TestGradeEfficiency(t *testing.T) {
	tests := []struct {
		name      string
		budget    int
		toolCalls int
		wantPass  bool
	}{
		{name: "under budget passes", budget: 6, toolCalls: 3, wantPass: true},
		{name: "exactly on budget passes", budget: 6, toolCalls: 6, wantPass: true},
		{name: "over budget fails", budget: 6, toolCalls: 7, wantPass: false},
		{name: "zero tool calls passes", budget: 1, toolCalls: 0, wantPass: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := eval.GradeEfficiency(eval.Case{MaxToolCalls: tt.budget}, tt.toolCalls)
			if got.Pass != tt.wantPass {
				t.Errorf("Pass = %v, want %v (detail: %s)", got.Pass, tt.wantPass, got.Detail)
			}
			if got.Detail == "" {
				t.Error("Detail is empty; the verdict must state calls vs budget")
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
