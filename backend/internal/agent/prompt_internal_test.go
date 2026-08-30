package agent

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// Day 8 locked design decision: splitting the system prompt into fragments
// must not change what a demo-mode run sees. The prompt is evidence (run_started
// stores it verbatim) and the eval baselines were recorded against the
// pre-split text, so demo mode has to be BYTE-identical to the single constant
// it replaced. testdata/system_prompt_demo.golden is that constant, extracted
// from the pre-Day-8 prompt.go on main.

func TestDemoSystemPromptIsByteIdenticalToThePreSplitPrompt(t *testing.T) {
	golden, err := os.ReadFile("testdata/system_prompt_demo.golden")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if systemPrompt != string(golden) {
		t.Errorf("the reassembled demo prompt differs from the pre-split constant\n"+
			"len(got) = %d, len(want) = %d\nfirst divergence: %s",
			len(systemPrompt), len(golden), firstDivergence(systemPrompt, string(golden)))
	}

	// And buildSystemPrompt in demo mode renders exactly what the pre-split
	// buildSystemPrompt(today, toolNames) rendered.
	got := buildSystemPrompt("2026-08-21", []string{"jira_search_issues", "notion_search"}, Sources{Mode: ModeDemo})
	want := string(golden) + "\n\nToday's date is 2026-08-21." +
		"\nTools available: jira_search_issues, notion_search."
	if got != want {
		t.Errorf("demo-mode buildSystemPrompt differs from the pre-split rendering\nfirst divergence: %s",
			firstDivergence(got, want))
	}
}

// firstDivergence renders where two strings first differ, for a readable
// failure on a 10KB prompt.
func firstDivergence(a, b string) string {
	limit := min(len(a), len(b))
	for i := range limit {
		if a[i] != b[i] {
			start := max(0, i-40)
			return fmt.Sprintf("at byte %d: got %q, want %q",
				i, a[start:min(len(a), i+40)], b[start:min(len(b), i+40)])
		}
	}
	return "one is a prefix of the other"
}

// REQ-8.4: the user-mode source guide is assembled from the run's connected
// sources — the agent is promised exactly the systems it has, the demo
// knowledge base does not exist, and the multi-source hop discipline appears
// only when there is more than one source to hop between.
func TestBuildSystemPromptUserModeCarriesOnlyConnectedSources(t *testing.T) {
	tests := []struct {
		name        string
		connected   []string
		wantParts   []string
		absentParts []string
	}{
		{
			name:      "jira only",
			connected: []string{"jira"},
			wantParts: []string{
				"For this run you can see JIRA — the system this user connected",
				"JIRA — the work itself",
				// Single source: the honest-gap rule replaces the cross-source hops.
				"say so plainly and answer the part your source does hold",
			},
			absentParts: []string{
				"NOTION — the written record",
				"GMAIL — anything that came from outside",
				"KNOWLEDGE BASE",
				"You can see three systems",
				"ask which of the other two would record the missing half",
			},
		},
		{
			name:      "jira and gmail",
			connected: []string{"gmail", "jira"},
			wantParts: []string{
				"GMAIL and JIRA — the systems this user connected",
				"JIRA — the work itself",
				"GMAIL — anything that came from outside",
				// Two sources: the hop discipline is back.
				"When a source gives you\n   half an answer",
			},
			absentParts: []string{
				"NOTION — the written record",
				"KNOWLEDGE BASE",
				"You can see three systems",
			},
		},
		{
			name:      "all three connected",
			connected: []string{"gmail", "jira", "notion"},
			wantParts: []string{
				"GMAIL, JIRA and NOTION",
				"JIRA — the work itself",
				"NOTION — the written record",
				"GMAIL — anything that came from outside",
			},
			// Even with all three live sources, the knowledge base indexes the
			// DEMO corpus and must not be promised on a user run.
			absentParts: []string{"KNOWLEDGE BASE"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildSystemPrompt("2026-08-21", nil, Sources{Mode: ModeUser, Connected: tt.connected})
			for _, part := range tt.wantParts {
				if !strings.Contains(got, part) {
					t.Errorf("user-mode prompt lacks %q", part)
				}
			}
			for _, part := range tt.absentParts {
				if strings.Contains(got, part) {
					t.Errorf("user-mode prompt still contains %q", part)
				}
			}
			if !strings.Contains(got, "Today's date is 2026-08-21.") {
				t.Error("user-mode prompt lacks the date line")
			}
		})
	}
}
