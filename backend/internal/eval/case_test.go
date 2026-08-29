package eval_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cortex/internal/eval"
)

// TEST-6.1 — case loading and validation (REQ-6.1).
//
// A case file is the ground-truth contract the whole harness grades against, so
// a typoed field or a contradictory case must fail loading loudly rather than
// silently grading against a default forever.

// validCaseYAML is the REQ-6.1 example, verbatim in shape.
const validCaseYAML = `id: atlas-original-deadline
question: "What was the original deadline for the payment integration in Project Atlas?"
expected_answer: "June 15"
expected_sources: ["ATLAS-145"]
category: single-hop
max_tool_calls: 6
`

func writeCaseFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write case file %s: %v", name, err)
	}
}

func TestLoadCasesReadsValidCases(t *testing.T) {
	dir := t.TempDir()
	// Written out of sorted order to assert the stable-order guarantee.
	writeCaseFile(t, dir, "b-negative.yaml", `id: neg-cfo
question: "Who is the CFO of Vantage?"
expected_answer: "The available data does not name Vantage's CFO."
category: negative
max_tool_calls: 4
`)
	writeCaseFile(t, dir, "a-deadline.yaml", validCaseYAML)
	// Hidden files (an editor's swap file, .DS_Store) are not cases and are
	// skipped; any other non-YAML file is an error — see the rejection test.
	writeCaseFile(t, dir, ".hidden.swp", "not a case")

	cases, err := eval.LoadCases(dir)
	if err != nil {
		t.Fatalf("LoadCases: %v", err)
	}
	if len(cases) != 2 {
		t.Fatalf("loaded %d cases, want 2", len(cases))
	}
	// Sorted by filename, so runs are stable.
	if cases[0].ID != "atlas-original-deadline" || cases[1].ID != "neg-cfo" {
		t.Errorf("case order = [%s, %s], want [atlas-original-deadline, neg-cfo]", cases[0].ID, cases[1].ID)
	}

	got := cases[0]
	if got.Question == "" || got.ExpectedAnswer != "June 15" {
		t.Errorf("case fields not decoded: %+v", got)
	}
	if len(got.ExpectedSources) != 1 || got.ExpectedSources[0] != "ATLAS-145" {
		t.Errorf("ExpectedSources = %v, want [ATLAS-145]", got.ExpectedSources)
	}
	if got.Category != eval.CategorySingleHop {
		t.Errorf("Category = %q, want %q", got.Category, eval.CategorySingleHop)
	}
	if got.MaxToolCalls != 6 {
		t.Errorf("MaxToolCalls = %d, want 6", got.MaxToolCalls)
	}
	// The negative case is legal without expected_sources.
	if neg := cases[1]; neg.Category != eval.CategoryNegative || len(neg.ExpectedSources) != 0 {
		t.Errorf("negative case = %+v, want category negative with no expected sources", neg)
	}
}

func TestLoadCasesRejectsInvalidCases(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		// wantIn are substrings the error must carry so the author can find
		// the mistake.
		wantIn []string
	}{
		{
			// A typoed field silently becoming a default would grade
			// efficiency against zero forever.
			name: "unknown field is rejected",
			yaml: `id: x
question: "q?"
expected_answer: "a"
expected_sources: ["ATLAS-1"]
category: single-hop
max_toolcalls: 6
`,
			wantIn: []string{"max_toolcalls"},
		},
		{
			name: "missing id",
			yaml: `question: "q?"
expected_answer: "a"
expected_sources: ["ATLAS-1"]
category: single-hop
max_tool_calls: 6
`,
			wantIn: []string{"id"},
		},
		{
			name: "missing question",
			yaml: `id: x
expected_answer: "a"
expected_sources: ["ATLAS-1"]
category: single-hop
max_tool_calls: 6
`,
			wantIn: []string{"question"},
		},
		{
			name: "missing expected_answer",
			yaml: `id: x
question: "q?"
expected_sources: ["ATLAS-1"]
category: single-hop
max_tool_calls: 6
`,
			wantIn: []string{"expected_answer"},
		},
		{
			name: "unknown category",
			yaml: `id: x
question: "q?"
expected_answer: "a"
expected_sources: ["ATLAS-1"]
category: two-hop
max_tool_calls: 6
`,
			wantIn: []string{"two-hop"},
		},
		{
			name: "missing max_tool_calls",
			yaml: `id: x
question: "q?"
expected_answer: "a"
expected_sources: ["ATLAS-1"]
category: single-hop
`,
			wantIn: []string{"max_tool_calls"},
		},
		{
			// A negative case asks about something absent from the data;
			// expecting sources for it makes source overlap unpassable.
			name: "negative case with expected_sources",
			yaml: `id: x
question: "q?"
expected_answer: "not found"
expected_sources: ["ATLAS-1"]
category: negative
max_tool_calls: 6
`,
			wantIn: []string{"negative"},
		},
		{
			name: "non-negative case without expected_sources",
			yaml: `id: x
question: "q?"
expected_answer: "a"
category: multi-hop
max_tool_calls: 6
`,
			wantIn: []string{"expected source"},
		},
		{
			name:   "malformed yaml",
			yaml:   "id: [unclosed",
			wantIn: []string{"parse"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeCaseFile(t, dir, "case.yaml", tt.yaml)
			_, err := eval.LoadCases(dir)
			if err == nil {
				t.Fatalf("LoadCases accepted an invalid case:\n%s", tt.yaml)
			}
			for _, want := range tt.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to mention %q", err, want)
				}
			}
		})
	}
}

func TestLoadCasesRejectsDuplicateIDs(t *testing.T) {
	dir := t.TempDir()
	writeCaseFile(t, dir, "a.yaml", validCaseYAML)
	writeCaseFile(t, dir, "b.yaml", validCaseYAML)

	_, err := eval.LoadCases(dir)
	if err == nil {
		t.Fatal("LoadCases accepted two cases with the same id")
	}
	// The error must name both files, or the author cannot fix it.
	for _, want := range []string{"atlas-original-deadline", "a.yaml", "b.yaml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

func TestLoadCasesRequiresAtLeastOneCase(t *testing.T) {
	if _, err := eval.LoadCases(t.TempDir()); err == nil {
		t.Error("LoadCases returned no error for a directory with no *.yaml cases")
	}
	if _, err := eval.LoadCases(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("LoadCases returned no error for a missing directory")
	}
}

// A stray non-YAML file in the cases directory is an error, not a skip: a
// case saved with the wrong extension would otherwise drop out of the suite
// with nothing but the printed case count to show for it.
func TestLoadCasesRejectsUnexpectedFiles(t *testing.T) {
	dir := t.TempDir()
	writeCaseFile(t, dir, "a-deadline.yaml", validCaseYAML)
	writeCaseFile(t, dir, "notes.txt", "no yaml here")
	_, err := eval.LoadCases(dir)
	if err == nil {
		t.Fatal("LoadCases returned no error for a directory holding notes.txt")
	}
	if !strings.Contains(err.Error(), "notes.txt") {
		t.Errorf("error = %q, want it to name notes.txt", err)
	}
}

// Both YAML spellings load, so a case saved as .yml is not silently dropped.
func TestLoadCasesAcceptsYMLExtension(t *testing.T) {
	dir := t.TempDir()
	writeCaseFile(t, dir, "a-deadline.yml", validCaseYAML)
	cases, err := eval.LoadCases(dir)
	if err != nil {
		t.Fatalf("LoadCases: %v", err)
	}
	if len(cases) != 1 || cases[0].ID != "atlas-original-deadline" {
		t.Fatalf("cases = %+v, want the one .yml case", cases)
	}
}

// The repository's real case set is itself a fixture the acceptance test runs,
// so it must load, validate, and meet REQ-6.1's size and category mix.
func TestRepositoryCasesAreValid(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "evals", "cases")
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("evals/cases not found at %s: %v", dir, err)
	}

	cases, err := eval.LoadCases(dir)
	if err != nil {
		t.Fatalf("LoadCases(%s): %v", dir, err)
	}
	if len(cases) < 20 {
		t.Errorf("repository has %d cases, REQ-6.1 requires >= 20", len(cases))
	}

	counts := map[string]int{}
	for _, c := range cases {
		counts[c.Category]++
	}
	for _, category := range []string{
		eval.CategorySingleHop, eval.CategoryMultiHop, eval.CategoryCrossSource, eval.CategoryNegative,
	} {
		if counts[category] == 0 {
			t.Errorf("no cases in category %q; REQ-6.1 requires all four", category)
		}
	}
	if counts[eval.CategoryNegative] < 3 {
		t.Errorf("negative cases = %d, REQ-6.1 asks for ~3", counts[eval.CategoryNegative])
	}
}
