// Package eval is the harness that measures answer quality against ground
// truth: it runs YAML-defined cases through the real agent, grades each run
// with a mix of mechanical checks and utility-model judges, and reports pass
// rates per metric and per category.
package eval

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Case categories. A case's category decides how it is judged — a negative
// case passes correctness by honestly declining — and which row of the summary
// table it lands in.
const (
	CategorySingleHop   = "single-hop"
	CategoryMultiHop    = "multi-hop"
	CategoryCrossSource = "cross-source"
	CategoryNegative    = "negative"
)

// categories is the closed set, for validation.
var categories = map[string]bool{
	CategorySingleHop:   true,
	CategoryMultiHop:    true,
	CategoryCrossSource: true,
	CategoryNegative:    true,
}

// Case is one graded question, loaded from evals/cases/<id>.yaml.
type Case struct {
	ID       string `yaml:"id"`
	Question string `yaml:"question"`
	// ExpectedAnswer is what the correctness judge grades against. For a
	// negative case it describes the honest decline.
	ExpectedAnswer string `yaml:"expected_answer"`
	// ExpectedSources are matched case-insensitively as substrings of an
	// evidence row's external_id, title, or URL (spec A3): Jira keys are
	// assigned at seed time and Gmail/Notion ids are opaque, so case authors
	// use issue summaries, page titles, and mail subjects.
	ExpectedSources []string `yaml:"expected_sources"`
	Category        string   `yaml:"category"`
	// MaxToolCalls is the efficiency budget: the run passes the efficiency
	// metric when it used at most this many tool calls.
	MaxToolCalls int `yaml:"max_tool_calls"`
}

// LoadCases reads every *.yaml file in dir — one case per file — and validates
// the set. Files are read in sorted order so case order is stable across runs.
func LoadCases(dir string) ([]Case, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read cases directory: %w", err)
	}

	var names []string
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		// Both YAML spellings load; anything else in the directory is an
		// error rather than silently skipped — a case saved as .txt or a
		// stray file would otherwise drop out of the suite unnoticed, and
		// the printed case count is the only thing that would show it.
		if !strings.HasSuffix(entry.Name(), ".yaml") && !strings.HasSuffix(entry.Name(), ".yml") {
			return nil, fmt.Errorf("unexpected file %s in cases directory: cases must be *.yaml", entry.Name())
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("no *.yaml cases in %s", dir)
	}

	cases := make([]Case, 0, len(names))
	seen := make(map[string]string, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read case %s: %w", name, err)
		}

		var c Case
		decoder := yaml.NewDecoder(strings.NewReader(string(raw)))
		// A typoed field name silently becomes a default without this, and a
		// case with a misspelled max_tool_calls would grade efficiency
		// against zero forever.
		decoder.KnownFields(true)
		if err := decoder.Decode(&c); err != nil {
			return nil, fmt.Errorf("parse case %s: %w", name, err)
		}

		if err := validateCase(c); err != nil {
			return nil, fmt.Errorf("case %s: %w", name, err)
		}
		if prev, dup := seen[c.ID]; dup {
			return nil, fmt.Errorf("case %s: id %q already used by %s", name, c.ID, prev)
		}
		seen[c.ID] = name
		cases = append(cases, c)
	}
	return cases, nil
}

// validateCase enforces the invariants grading depends on.
func validateCase(c Case) error {
	if strings.TrimSpace(c.ID) == "" {
		return errors.New("id is required")
	}
	if strings.TrimSpace(c.Question) == "" {
		return errors.New("question is required")
	}
	if strings.TrimSpace(c.ExpectedAnswer) == "" {
		return errors.New("expected_answer is required")
	}
	if !categories[c.Category] {
		return fmt.Errorf("category %q is not one of single-hop, multi-hop, cross-source, negative", c.Category)
	}
	if c.MaxToolCalls <= 0 {
		return errors.New("max_tool_calls must be positive")
	}
	// A negative case asks about something absent from the data; expecting
	// sources for it is a contradiction, and would make source overlap
	// unpassable.
	if c.Category == CategoryNegative && len(c.ExpectedSources) > 0 {
		return errors.New("negative cases must not list expected_sources")
	}
	if c.Category != CategoryNegative && len(c.ExpectedSources) == 0 {
		return errors.New("non-negative cases must list at least one expected source")
	}
	return nil
}
