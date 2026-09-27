package claudecode_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"cortex/internal/tools"
	"cortex/internal/tools/claudecode"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitCmd(dir, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// writerWithEnvSpy builds a writer over a real one-commit repo whose fake
// claude dumps its environment and argv to files outside the worktree.
func writerWithEnvSpy(t *testing.T) (*claudecode.Writer, string) {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "config", "user.name", "Cortex Test")
	git(t, repo, "config", "user.email", "test@cortex.test")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "a.txt")
	git(t, repo, "commit", "-q", "-m", "initial")

	rec := t.TempDir()
	script := "#!/bin/sh\n" +
		"env > '" + rec + "/env'\n" +
		"printf '%s\\n' \"$@\" > '" + rec + "/args'\n" +
		"cat > /dev/null\n" +
		`echo '{"type":"result","subtype":"success","is_error":false,"num_turns":1,"duration_ms":1,"total_cost_usd":0,"session_id":"s","result":"Nothing to do."}'` + "\n"
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	w, err := claudecode.NewWriter(claudecode.WriterConfig{
		Repos:       map[string]claudecode.Repo{"cortex": {Path: repo}},
		WorktreeDir: filepath.Join(t.TempDir(), "wt"),
		Bin:         bin,
		Timeout:     30 * time.Second,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	return w, rec
}

// The spawned claude never receives DATABASE_URL or any CORTEX_ /
// OPENAI_ variable, even when the runner's own environment has them.
func TestClaudeCodeNeverInheritsCortexSecrets(t *testing.T) {
	secrets := map[string]string{
		"DATABASE_URL":              "postgres://cortex:hunter2@db/cortex",
		"TEST_DATABASE_URL":         "postgres://cortex:hunter2@db/cortex_test",
		"OPENAI_API_KEY":            "sk-test-openai",
		"OPENAI_BASE_URL":           "https://example.invalid",
		"CORTEX_API_TOKEN":          "cortex-operator-token",
		"CORTEX_SESSION_SECRET":     "cortex-session",
		"RUNNER_USER_EMAIL":         "owner@cortex.test",
		"LLM_KEY_ENCRYPTION_SECRET": "00ff",
	}
	for k, v := range secrets {
		t.Setenv(k, v)
	}
	w, rec := writerWithEnvSpy(t)

	payload := json.RawMessage(`{"repo":"cortex","issue_key":"PAY-1","prompt":"look"}`)
	if _, err := w.ExecuteWithProgress(context.Background(), payload, func(tools.Step) {}); err != nil {
		t.Fatalf("ExecuteWithProgress: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(rec, "env"))
	if err != nil {
		t.Fatalf("the fake never recorded its environment: %v", err)
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			env[k] = v
		}
	}

	for k := range env {
		if k == "DATABASE_URL" || strings.HasPrefix(k, "CORTEX_") || strings.HasPrefix(k, "OPENAI_") {
			t.Errorf("claude received %s", k)
		}
	}
	for k, v := range secrets {
		for ek, ev := range env {
			if strings.Contains(ev, v) {
				t.Errorf("claude's %s carries the value of the runner's %s", ek, k)
			}
		}
	}
	// What it needs to authenticate is still there.
	for _, k := range []string{"HOME", "PATH"} {
		if env[k] == "" {
			t.Errorf("claude did not receive %s, which its login needs", k)
		}
	}
}

// The fence Claude Code runs inside, as seen on claude's argv.
func TestClaudeCodeRunsFenced(t *testing.T) {
	w, rec := writerWithEnvSpy(t)
	payload := json.RawMessage(`{"repo":"cortex","issue_key":"PAY-1","prompt":"look"}`)
	if _, err := w.Execute(context.Background(), payload); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(rec, "args"))
	if err != nil {
		t.Fatalf("the fake never recorded its argv: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(raw)), "\n")
	flag := func(name string) (string, bool) {
		i := slices.Index(args, name)
		if i < 0 || i+1 >= len(args) {
			return "", i >= 0
		}
		return args[i+1], true
	}

	if !slices.Contains(args, "-p") {
		t.Error("claude was not run headless (-p)")
	}
	if slices.Contains(args, "--dangerously-skip-permissions") {
		t.Error("claude was run with --dangerously-skip-permissions")
	}
	for name, want := range map[string]string{
		"--output-format":   "stream-json",
		"--permission-mode": "acceptEdits",
		"--setting-sources": "project",
		"--max-turns":       "60",
	} {
		if got, _ := flag(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if _, ok := flag("--strict-mcp-config"); !ok {
		t.Error("--strict-mcp-config is missing: the owner's MCP connectors would be reachable")
	}
	if got, _ := flag("--disallowedTools"); !strings.Contains(got, "WebFetch") || !strings.Contains(got, "WebSearch") {
		t.Errorf("--disallowedTools = %q, want WebFetch and WebSearch", got)
	}
	got, _ := flag("--tools")
	for _, bad := range []string{"Bash", "WebFetch", "WebSearch"} {
		if slices.Contains(strings.Split(got, ","), bad) {
			t.Errorf("--tools = %q offers %s by default", got, bad)
		}
	}
	if _, ok := flag("--max-budget-usd"); !ok {
		t.Error("--max-budget-usd is missing")
	}
}

// gitCmd runs git in dir, ignoring GIT_* variables a surrounding git hook set;
// they would otherwise aim the command at the repository being committed to.
func gitCmd(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	return cmd
}
