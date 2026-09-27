package claudecode_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cortex/internal/tools"
	"cortex/internal/tools/claudecode"
)

// repoWithFake builds a one-commit repo and a writer whose fake claude runs
// script (in the worktree) before printing a successful result.
func repoWithFake(t *testing.T, script string) (repo string, w *claudecode.Writer, worktrees string) {
	t.Helper()
	repo = t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "config", "user.name", "Cortex Test")
	git(t, repo, "config", "user.email", "test@cortex.test")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "a.txt")
	git(t, repo, "commit", "-q", "-m", "initial")

	bin := filepath.Join(t.TempDir(), "claude")
	body := "#!/bin/sh\ncat > /dev/null\n" + script + "\n" +
		`printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":1,"session_id":"s","result":"Subject: did it\nDone."}'` + "\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	worktrees = filepath.Join(t.TempDir(), "wt")
	w, err := claudecode.NewWriter(claudecode.WriterConfig{
		Repos:       map[string]claudecode.Repo{"app": {Path: repo}},
		WorktreeDir: worktrees,
		Bin:         bin,
		Timeout:     30 * time.Second,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return repo, w, worktrees
}

func delegate(t *testing.T, w *claudecode.Writer, base string) (tools.WriteOutcome, error) {
	t.Helper()
	raw, _ := json.Marshal(claudecode.Payload{Repo: "app", IssueKey: "APP-1", BaseBranch: base, Prompt: "fix it"})
	return w.ExecuteWithProgress(context.Background(), raw, func(tools.Step) {})
}

// The runner's own git commands never run hooks: the repo's hooks path is
// relative, so it resolves inside the worktree the agent just edited, and a
// hook would run the agent's code with the runner's secrets.
func TestRunnerGitNeverRunsRepoHooks(t *testing.T) {
	canary := filepath.Join(t.TempDir(), "hook-ran")
	hook := "#!/bin/sh\necho \"$DATABASE_URL\" > '" + canary + "'\n"
	// The agent rewrites the hooks inside its worktree, as injected text could ask it to.
	repo, w, _ := repoWithFake(t, "mkdir -p hooks && printf '%s' '"+hook+"' > hooks/pre-commit && "+
		"cp hooks/pre-commit hooks/post-checkout && chmod +x hooks/* && echo b > b.txt")
	for _, name := range []string{"pre-commit", "post-checkout"} {
		if err := os.MkdirAll(filepath.Join(repo, "hooks"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "hooks", name), []byte(hook), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repo, "add", "hooks")
	git(t, repo, "commit", "-q", "-m", "hooks")
	git(t, repo, "config", "core.hooksPath", "hooks")
	t.Setenv("DATABASE_URL", "postgres://secret@db/cortex")

	out, err := delegate(t, w, "main")
	if err != nil {
		t.Fatalf("delegation failed: %v", err)
	}
	if out.Detail["outcome"] != "changes" {
		t.Errorf("outcome = %v, want changes", out.Detail["outcome"])
	}
	if _, err := os.Stat(canary); err == nil {
		got, _ := os.ReadFile(canary)
		t.Fatalf("a repo hook ran during the runner's git commands (saw %q)", strings.TrimSpace(string(got)))
	}
}

// Only a local branch can be the base: a remote ref is whatever someone else
// pushed, and nothing is created when the base is refused.
func TestDelegationRefusesABaseThatIsNotALocalBranch(t *testing.T) {
	repo, w, worktrees := repoWithFake(t, "echo b > b.txt")
	git(t, repo, "update-ref", "refs/remotes/origin/contrib", "HEAD")

	for _, base := range []string{"origin/contrib", "no-such-branch"} {
		if _, err := delegate(t, w, base); err == nil || !strings.Contains(err.Error(), "not a local branch") {
			t.Errorf("base %q: err = %v, want a refusal naming a local branch", base, err)
		}
	}
	if entries, _ := os.ReadDir(worktrees); len(entries) > 0 {
		t.Errorf("a refused base still created worktrees: %v", entries)
	}
	if branches := git(t, repo, "branch", "--list", "cortex/*"); branches != "" {
		t.Errorf("a refused base still created branches: %s", branches)
	}
}

// A commit the agent made itself is its work too; a clean index afterwards
// must not read as "no changes" and delete the branch.
func TestAgentsOwnCommitIsKept(t *testing.T) {
	repo, w, _ := repoWithFake(t, "echo b > b.txt && git add b.txt && git commit -q -m 'agent commit'")

	out, err := delegate(t, w, "main")
	if err != nil {
		t.Fatalf("delegation failed: %v", err)
	}
	if out.Detail["outcome"] != "changes" {
		t.Fatalf("outcome = %v, want changes", out.Detail["outcome"])
	}
	branch, _ := out.Detail["branch"].(string)
	if branch == "" {
		t.Fatal("no branch reported for committed work")
	}
	if log := git(t, repo, "log", "--format=%s", "main.."+branch); !strings.Contains(log, "agent commit") {
		t.Errorf("the agent's commit is gone from %s: %q", branch, log)
	}
}
