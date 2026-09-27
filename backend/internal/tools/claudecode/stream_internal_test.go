package claudecode

import "testing"

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
