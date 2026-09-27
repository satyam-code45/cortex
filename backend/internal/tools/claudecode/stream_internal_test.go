package claudecode

import (
	"strings"
	"testing"

	"cortex/internal/tools"
)

func TestStepDetailsNameRepoRelativePaths(t *testing.T) {
	root := "/home/me/.cortex-worktrees/shop-cortex-SHOP-31-abc123"
	for in, want := range map[string]string{
		root + "/cart/total.go":          "cart/total.go",
		"find " + root + " -name '*.go'": "find . -name '*.go'",
		"go test ./...":                  "go test ./...",
	} {
		if got := relPaths(in, root); got != want {
			t.Errorf("relPaths(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStreamSkipsLinesThatAreNotJSON(t *testing.T) {
	in := "warning: something on stdout\n" +
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"a.go"}}]}}` + "\n" +
		"{not json\n" +
		`{"type":"result","subtype":"success","result":"done","session_id":"s"}` // no trailing newline
	var steps []tools.Step
	res, err := readStream(strings.NewReader(in), "", func(s tools.Step) { steps = append(steps, s) })
	if err != nil {
		t.Fatal(err)
	}
	if !res.Seen || res.Text != "done" || res.SessionID != "s" {
		t.Errorf("result = %+v, want the final line read", res)
	}
	if len(steps) != 1 || steps[0].Tool != "Read" || steps[0].Detail != "a.go" {
		t.Errorf("steps = %+v, want the one Read", steps)
	}
}
