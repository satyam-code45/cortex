// Package repohygiene checks properties of the repository tree itself rather
// than of any Go package: the public tree must not reference the private
// planning workspace it was built from, and the public-facing artifacts
// (license, README anchors, demo script) must be present.
package repohygiene

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// privatePatterns are the extended regexes that must never match a file.
//
// Each pattern string is split mid-word so that this file, once tracked, can
// never match its own patterns — the check stays self-enforcing without having
// to exempt itself, and an exempt file in a test whose whole point is "nothing
// is exempt" would be a hole.
//
// The day-number pattern is deliberately broader than the leak it hunts: it
// also fires on ordinary prose that happens to number a day, in a future doc or
// seeded fixture. That trade is intentional — a false positive costs one
// reworded sentence, a missed leak ships the private plan — so the failure
// message says what to do about it. (This very comment tripped it while being
// written, which is the check working.)
var privatePatterns = []struct {
	name    string
	pattern string
}{
	{
		name: "internal requirement and test ids",
		// The suffix class allows a letter, not just a digit: the ids that
		// actually leaked were review findings numbered per day with a letter
		// (a bug id, a follow-up test id), and a digit-only class walked past
		// every one of them.
		pattern: "RE" + "Q-[0-9]+\\.[0-9A-Za-z]|TE" + "ST-[0-9]+\\.[0-9A-Za-z]" +
			"|BU" + "G-[0-9]+\\.[0-9A-Za-z]|EV" + "AL-[0-9]+\\.[0-9A-Za-z]",
	},
	{
		name:    "private planning document names",
		pattern: "PL" + "AN\\.md|id" + "ea\\.md|CLA" + "UDE\\.md",
	},
	{
		name:    "per-feature spec paths",
		pattern: "spe" + "cs/day",
	},
	{
		name:    "agent configuration directory",
		pattern: "\\.cla" + "ude/",
	},
	{
		name:    "day-number plan references",
		pattern: "\\bDa" + "y [0-9]",
	},
}

// TestWorkingTreeHasNoPrivatePlanningReferences enforces the privacy scrub as
// an invariant: nothing that can reach a commit carries an identifier pointing
// at the local-only planning workspace.
//
// --untracked matters more than it looks. Without it git grep searches only
// files already in the index, so a file added by the very change under review
// is invisible and the check passes vacuously for exactly the content most
// likely to leak. With it, files that are merely new are searched too, while
// ignored files — the planning workspace itself, .env, the cached OAuth token —
// stay excluded, because --untracked honours .gitignore and .git/info/exclude.
func TestWorkingTreeHasNoPrivatePlanningReferences(t *testing.T) {
	root := repoRoot(t)

	for _, tc := range privatePatterns {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("git", "grep", "--untracked", "-I", "-l", "-E", tc.pattern)
			cmd.Dir = root
			out, err := cmd.Output()
			if err == nil {
				t.Errorf("these files reference the private planning workspace, "+
					"which must not appear in a public repository (pattern %q):\n%s\n"+
					"Rewrite the reference to state what it means in plain English "+
					"rather than deleting the sentence — the constraint it records "+
					"is usually worth keeping.",
					tc.pattern, strings.TrimSpace(string(out)))
				return
			}
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
				return // no matches — the tree is clean
			}
			t.Fatalf("git grep %q failed: %v (stderr: %s)", tc.pattern, err, gitStderr(err))
		})
	}
}

// TestPublicRepoArtifactsExist pins the durable structural facts of the public
// repo: an MIT license, a README carrying the architecture diagram and the
// trace-panel image slot, and the demo script.
//
// Scope worth being explicit about: the README row asserts the image is
// REFERENCED, not that the image file exists. Capturing docs/trace-panel.png
// needs a running app and a human at a screen, so it cannot be a precondition
// of the test suite — while it is missing the README renders a broken image on
// GitHub, and only a human can close that.
func TestPublicRepoArtifactsExist(t *testing.T) {
	root := repoRoot(t)

	tests := []struct {
		path        string
		mustContain []string
	}{
		{path: "LICENSE", mustContain: []string{"MIT License"}},
		{path: "README.md", mustContain: []string{"```mermaid", "docs/trace-panel.png"}},
		{path: filepath.Join("docs", "demo.md"), mustContain: nil},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(root, tc.path))
			if err != nil {
				t.Fatalf("required public artifact is missing: %v", err)
			}
			if len(strings.TrimSpace(string(raw))) == 0 {
				t.Fatalf("%s exists but is empty", tc.path)
			}
			for _, want := range tc.mustContain {
				if !strings.Contains(string(raw), want) {
					t.Errorf("%s does not contain %q", tc.path, want)
				}
			}
		})
	}
}

// repoRoot locates this repository's root, skipping when the test runs outside
// a git worktree (for example from an exported source archive).
//
// The root is resolved from this package's own directory and cross-checked
// against a file only this repository has: if cortex is ever vendored inside an
// enclosing repository, `git rev-parse` would otherwise report the outer tree
// and the grep would fail naming foreign files.
func repoRoot(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("not inside a git worktree: %v", err)
	}
	root := strings.TrimSpace(string(out))
	if _, err := os.Stat(filepath.Join(root, "backend", "go.mod")); err != nil {
		t.Fatalf("resolved repository root %q is not this repository "+
			"(no backend/go.mod); is it vendored inside an outer git repository?", root)
	}
	return root
}

// gitStderr extracts captured stderr from an exec error, for diagnostics.
func gitStderr(err error) string {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return strings.TrimSpace(string(exitErr.Stderr))
	}
	return ""
}
