package claudecode

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"cortex/internal/tools"
)

// Repo is one repository a runner offers, by name, from its local config.
type Repo struct {
	Path string `json:"path"`
	// Tools is the set Claude Code can use at all (--tools). Allow
	// pre-approves narrower patterns, e.g. "Bash(go test:*)"; anything outside
	// Allow that needs permission is denied, because nobody is there to ask.
	Tools []string `json:"tools,omitempty"`
	Allow []string `json:"allow,omitempty"`
}

// DefaultTools can read and edit files and launch subagents, nothing else.
var DefaultTools = []string{"Read", "Edit", "Write", "Glob", "Grep", "Task"}

// WriterConfig configures the runner-side executor.
type WriterConfig struct {
	Repos       map[string]Repo
	WorktreeDir string
	// Bin is the claude executable; "claude" on PATH by default. Tests point
	// it at a fake.
	Bin          string
	Model        string
	MaxTurns     int
	MaxBudgetUSD float64
	// Timeout stays under River's one-hour stuck-job rescue.
	Timeout  time.Duration
	MaxSteps int
	Logger   *slog.Logger
}

// Writer performs approved delegations. It implements tools.ProgressWriter.
type Writer struct{ cfg WriterConfig }

// NewWriter validates cfg and fills defaults.
func NewWriter(cfg WriterConfig) (*Writer, error) {
	if len(cfg.Repos) == 0 {
		return nil, errors.New("claude_code: at least one repo is required")
	}
	for name, repo := range cfg.Repos {
		if !filepath.IsAbs(repo.Path) {
			return nil, fmt.Errorf("claude_code: repo %q path must be absolute", name)
		}
	}
	if !filepath.IsAbs(cfg.WorktreeDir) {
		return nil, errors.New("claude_code: WorktreeDir must be an absolute path")
	}
	if cfg.Bin == "" {
		cfg.Bin = "claude"
	}
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = 60
	}
	if cfg.MaxBudgetUSD <= 0 {
		cfg.MaxBudgetUSD = 5
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 40 * time.Minute
	}
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = 300
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Writer{cfg: cfg}, nil
}

// RepoNames is what the runner advertises; paths never leave this machine.
func (w *Writer) RepoNames() []string {
	names := make([]string, 0, len(w.cfg.Repos))
	for name := range w.cfg.Repos {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (w *Writer) Action() string { return ActionDelegate }

func (w *Writer) Execute(ctx context.Context, payload json.RawMessage) (tools.WriteOutcome, error) {
	return w.ExecuteWithProgress(ctx, payload, func(tools.Step) {})
}

// ExecuteWithProgress runs one delegation end to end.
func (w *Writer) ExecuteWithProgress(ctx context.Context, raw json.RawMessage, emit func(tools.Step)) (tools.WriteOutcome, error) {
	var p Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return tools.WriteOutcome{}, fmt.Errorf("claude_code: decode payload: %w", err)
	}
	// Re-checked here: the approved payload may have been edited.
	if err := p.Validate(w.RepoNames()); err != nil {
		return tools.WriteOutcome{}, err
	}
	repo := w.cfg.Repos[p.Repo]

	branch, err := branchName(p.IssueKey)
	if err != nil {
		return tools.WriteOutcome{}, err
	}
	dir := filepath.Join(w.cfg.WorktreeDir, p.Repo+"-"+strings.ReplaceAll(branch, "/", "-"))
	if err := os.MkdirAll(w.cfg.WorktreeDir, 0o755); err != nil {
		return tools.WriteOutcome{}, fmt.Errorf("claude_code: create worktree dir: %w", err)
	}
	if _, err := git(ctx, repo.Path, "worktree", "add", "-b", branch, dir, p.BaseBranch); err != nil {
		return tools.WriteOutcome{}, fmt.Errorf("claude_code: create worktree from %s: %w", p.BaseBranch, err)
	}

	res, runErr := w.run(ctx, dir, repo, p, branch, w.capped(emit))

	// Recorded before judging the run, so partial work is never lost.
	detail := map[string]any{
		"branch":         branch,
		"base_branch":    p.BaseBranch,
		"session_id":     res.SessionID,
		"num_turns":      res.NumTurns,
		"duration_ms":    res.DurationMS,
		"total_cost_usd": res.TotalCostUSD,
		"subagents":      nonNil(res.Subagents),
	}
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	changed, err := w.commit(commitCtx, dir, p, res, detail)
	if err != nil {
		// The edits are still in the worktree; say where rather than lose them.
		return tools.WriteOutcome{}, fmt.Errorf("claude_code: %w (the changes are left uncommitted in %s)", err, dir)
	}

	if runErr != nil {
		if changed {
			return tools.WriteOutcome{}, fmt.Errorf("%w (partial work committed on %s)", runErr, branch)
		}
		w.discard(commitCtx, repo.Path, dir, branch)
		return tools.WriteOutcome{}, runErr
	}

	switch {
	case strings.HasPrefix(res.Subtype, "error_max"):
		detail["outcome"] = "max_turns"
	case changed:
		detail["outcome"] = "changes"
	default:
		detail["outcome"] = "no_changes"
	}
	if changed {
		// The branch holds the commit; the checkout is not needed any more.
		if _, err := git(commitCtx, repo.Path, "worktree", "remove", dir); err != nil {
			w.cfg.Logger.Warn("could not remove worktree", "dir", dir, "error", err)
		}
	} else {
		w.discard(commitCtx, repo.Path, dir, branch)
		delete(detail, "branch")
	}

	summary := strings.TrimSpace(res.Text)
	if summary == "" {
		summary = "Claude Code finished without a summary."
	}
	return tools.WriteOutcome{Summary: summary, Detail: detail}, nil
}

// run starts claude and reads its stream until it exits.
func (w *Writer) run(ctx context.Context, dir string, repo Repo, p Payload, branch string, emit func(tools.Step)) (runResult, error) {
	ctx, cancel := context.WithTimeout(ctx, w.cfg.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, w.cfg.Bin, w.args(repo, p, branch)...)
	cmd.Dir = dir
	cmd.Env = childEnv(os.Environ())
	cmd.Stdin = strings.NewReader(p.Prompt)
	var stderr tailBuffer
	cmd.Stderr = &stderr
	// Own process group, so a timeout also stops the subprocesses Claude
	// Code started.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return runResult{}, fmt.Errorf("claude_code: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return runResult{}, fmt.Errorf("claude_code: start %s: %w", w.cfg.Bin, err)
	}
	res, readErr := readStream(stdout, dir, emit)
	waitErr := cmd.Wait()

	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return res, fmt.Errorf("claude_code: timed out after %s", w.cfg.Timeout)
	case errors.Is(ctx.Err(), context.Canceled):
		return res, errors.New("claude_code: the runner was stopped before Claude Code finished")
	case !res.Seen:
		msg := strings.TrimSpace(stderr.String())
		if readErr != nil {
			msg = strings.TrimSpace(readErr.Error() + "\n" + msg)
		}
		if waitErr != nil {
			return res, fmt.Errorf("claude_code: exited without a result (%v): %s", waitErr, msg)
		}
		return res, fmt.Errorf("claude_code: exited without a result: %s", msg)
	case res.IsError && !strings.HasPrefix(res.Subtype, "error_max"):
		return res, fmt.Errorf("claude_code: %s: %s", res.Subtype, strings.TrimSpace(res.Text))
	}
	return res, nil
}

func (w *Writer) args(repo Repo, p Payload, branch string) []string {
	toolset := repo.Tools
	if len(toolset) == 0 {
		toolset = DefaultTools
	}
	args := []string{
		"-p",
		"--output-format", "stream-json", "--verbose",
		"--permission-mode", "acceptEdits",
		"--tools", strings.Join(toolset, ","),
		// The owner's personal hooks, plugins and claude.ai connectors stay
		// out: an injected prompt must not reach them.
		"--setting-sources", "project",
		"--strict-mcp-config",
		"--disallowedTools", "WebFetch,WebSearch",
		"--max-turns", strconv.Itoa(w.cfg.MaxTurns),
		"--max-budget-usd", strconv.FormatFloat(w.cfg.MaxBudgetUSD, 'f', 2, 64),
		"--append-system-prompt", fmt.Sprintf(
			"You are working on Jira issue %s in a git worktree on branch %s. Do not run git commit, "+
				"push or checkout: the runner commits your changes when you finish. End with a short "+
				"summary: what you changed and why, what you ran to check it and the result, and "+
				"anything left unresolved.", p.IssueKey, branch),
	}
	if len(repo.Allow) > 0 {
		args = append(args, "--allowedTools", strings.Join(repo.Allow, ","))
	}
	if w.cfg.Model != "" {
		args = append(args, "--model", w.cfg.Model)
	}
	return args
}

// capped forwards the first MaxSteps steps and counts the rest.
func (w *Writer) capped(emit func(tools.Step)) func(tools.Step) {
	n := 0
	return func(s tools.Step) {
		n++
		switch {
		case n < w.cfg.MaxSteps:
			emit(s)
		case n == w.cfg.MaxSteps:
			emit(tools.Step{Kind: "text", Detail: "further steps are not shown; the full session is on the runner"})
		}
	}
}

// commit stages and commits everything in the worktree. It reports whether
// there was anything to commit and fills files_changed, diffstat, commit_sha.
func (w *Writer) commit(ctx context.Context, dir string, p Payload, res runResult, detail map[string]any) (bool, error) {
	if _, err := git(ctx, dir, "add", "-A"); err != nil {
		return false, fmt.Errorf("stage changes: %w", err)
	}
	names, err := git(ctx, dir, "diff", "--cached", "--name-only")
	if err != nil {
		return false, fmt.Errorf("list changes: %w", err)
	}
	if names == "" {
		return false, nil
	}
	stat, _ := git(ctx, dir, "diff", "--cached", "--shortstat")
	detail["files_changed"] = strings.Split(names, "\n")
	detail["diffstat"] = stat

	subject := p.IssueKey + ": " + firstLine(res.Text, 60)
	if strings.TrimSpace(res.Text) == "" {
		subject = p.IssueKey + ": changes from Claude Code"
	}
	if _, err := git(ctx, dir, "commit", "-q", "-m", subject, "-m", "Delegated from Cortex."); err != nil {
		return true, fmt.Errorf("commit: %w", err)
	}
	sha, _ := git(ctx, dir, "rev-parse", "--short", "HEAD")
	detail["commit_sha"] = sha
	return true, nil
}

// discard removes a worktree and branch that hold nothing worth keeping.
func (w *Writer) discard(ctx context.Context, repoPath, dir, branch string) {
	if _, err := git(ctx, repoPath, "worktree", "remove", "--force", dir); err != nil {
		w.cfg.Logger.Warn("could not remove worktree", "dir", dir, "error", err)
	}
	if _, err := git(ctx, repoPath, "branch", "-D", branch); err != nil {
		w.cfg.Logger.Warn("could not delete branch", "branch", branch, "error", err)
	}
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(errOut.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func branchName(issueKey string) (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("claude_code: branch suffix: %w", err)
	}
	return "cortex/" + strings.ToLower(issueKey) + "-" + hex.EncodeToString(b[:]), nil
}

// childEnvKeys is everything Claude Code inherits. Database URLs, Cortex
// secrets and provider keys stay behind.
var childEnvKeys = []string{
	"HOME", "PATH", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "TERM", "TMPDIR",
	"CLAUDE_CONFIG_DIR", "ANTHROPIC_API_KEY",
}

func childEnv(env []string) []string {
	out := make([]string, 0, len(childEnvKeys))
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if slices.Contains(childEnvKeys, key) {
			out = append(out, kv)
		}
	}
	return out
}

func firstLine(s string, n int) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	s = strings.TrimRight(strings.TrimSpace(s), ".")
	if len(s) > n {
		cut := n
		for cut > 0 && (s[cut]&0xC0) == 0x80 {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// tailBuffer keeps the last 4 KB of stderr for error messages.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 4096 {
		t.b = t.b[len(t.b)-4096:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }
