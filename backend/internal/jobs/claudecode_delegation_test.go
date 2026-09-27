package jobs_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/actions"
	"cortex/internal/jobs"
	"cortex/internal/store"
	"cortex/internal/tools/claudecode"
)

// A fake `claude` (a shell script printing canned
// stream-json) drives the real Claude Code writer through the real
// ExecuteWriteWorker.Perform against a real git repository.

// A stream shaped like Claude Code 2.1.283's: init noise, a subagent call
// (tool_use "Agent") whose inner messages carry parent_tool_use_id, a
// block-array tool_result, a rate-limit event, and the final result line.
const cannedStream = `{"type":"system","subtype":"init","session_id":"sess-abc","tools":["Read","Edit"]}
{"type":"assistant","parent_tool_use_id":null,"message":{"content":[{"type":"text","text":"Looking into PAY-142."},{"type":"tool_use","id":"toolu_1","name":"Agent","input":{"subagent_type":"Explore","description":"find it"}}]}}
{"type":"assistant","parent_tool_use_id":"toolu_1","message":{"content":[{"type":"tool_use","id":"toolu_2","name":"Read","input":{"file_path":"bug.txt"}}]}}
{"type":"user","parent_tool_use_id":"toolu_1","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_2","content":"bug","is_error":false}]}}
{"type":"user","parent_tool_use_id":null,"message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"found it in bug.txt"}],"is_error":false}]}}
{"type":"assistant","parent_tool_use_id":null,"message":{"content":[{"type":"tool_use","id":"toolu_3","name":"Edit","input":{"file_path":"bug.txt"}}]}}
{"type":"user","parent_tool_use_id":null,"message":{"content":[{"type":"tool_result","tool_use_id":"toolu_3","content":"edited","is_error":false}]}}
{"type":"rate_limit_event"}
{"type":"result","subtype":"success","is_error":false,"num_turns":3,"duration_ms":1234,"total_cost_usd":0.03,"session_id":"sess-abc","result":"Fixed the bug."}`

const noChangeStream = `{"type":"system","subtype":"init","session_id":"sess-nc"}
{"type":"assistant","parent_tool_use_id":null,"message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"bug.txt"}}]}}
{"type":"user","parent_tool_use_id":null,"message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"bug","is_error":false}]}}
{"type":"result","subtype":"success","is_error":false,"num_turns":1,"duration_ms":10,"total_cost_usd":0.001,"session_id":"sess-nc","result":"No fix needed: the behaviour is correct."}`

// fakeClaude is a scripted claude executable plus the directory it records
// what it saw into (the child's environment is cleaned, so the paths are baked
// into the script rather than passed through env).
type fakeClaude struct {
	bin string
	rec string
}

// newFakeClaude writes a script that records its cwd, branch, argv, stdin and
// environment, runs body (e.g. an edit in its cwd), prints stream, then runs
// after and exits with code.
func newFakeClaude(t *testing.T, body, stream, after string, code int) fakeClaude {
	t.Helper()
	dir := t.TempDir()
	rec := filepath.Join(dir, "rec")
	if err := os.Mkdir(rec, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"echo run >> '" + rec + "/calls'\n" +
		"pwd > '" + rec + "/cwd'\n" +
		"git rev-parse --abbrev-ref HEAD > '" + rec + "/branch' 2>/dev/null\n" +
		"cat > '" + rec + "/prompt'\n" +
		"printf '%s\\n' \"$@\" > '" + rec + "/args'\n" +
		"env > '" + rec + "/env'\n" +
		body + "\n" +
		"cat <<'NDJSON'\n" + stream + "\nNDJSON\n" +
		after + "\n" +
		"exit " + strconv.Itoa(code) + "\n"
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	return fakeClaude{bin: bin, rec: rec}
}

func (f fakeClaude) read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.rec, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (f fakeClaude) calls(t *testing.T) int {
	t.Helper()
	s := f.read(t, "calls")
	if s == "" {
		return 0
	}
	return len(strings.Split(s, "\n"))
}

// gitRun runs git in dir and returns trimmed stdout.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitCmd(dir, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newGitRepo creates a repository with one commit on main.
func newGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "config", "user.name", "Cortex Test")
	gitRun(t, dir, "config", "user.email", "test@cortex.test")
	gitRun(t, dir, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, "bug.txt"), []byte("bug\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "bug.txt")
	gitRun(t, dir, "commit", "-q", "-m", "initial")
	return dir
}

type delegationRig struct {
	pool     *pgxpool.Pool
	repo     string
	worktree string
	worker   *jobs.ExecuteWriteWorker
	fixture  actionFixture
}

// newDelegationRig approves one claude_code.delegate action (with an edited
// prompt) and builds a worker whose registry holds only the Claude Code writer,
// as cmd/runner does. The queue is a real insert-only one, so the resume is
// really enqueued.
func newDelegationRig(t *testing.T, fake fakeClaude, timeout time.Duration) delegationRig {
	t.Helper()
	pool := testPool(t)
	repo := newGitRepo(t)
	worktree := filepath.Join(t.TempDir(), "worktrees")

	writer, err := claudecode.NewWriter(claudecode.WriterConfig{
		Repos:       map[string]claudecode.Repo{"cortex": {Path: repo}},
		WorktreeDir: worktree,
		Bin:         fake.bin,
		Timeout:     timeout,
		Logger:      discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	registry, err := actions.NewRegistry(writer)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	queue, err := jobs.New(jobs.Config{Pool: pool, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	worker, err := jobs.NewExecuteWriteWorker(pool,
		func(context.Context, uuid.UUID) (*actions.Registry, error) { return registry, nil },
		queue, discardLogger())
	if err != nil {
		t.Fatalf("worker: %v", err)
	}

	proposed := `{"repo":"cortex","issue_key":"PAY-142","base_branch":"main","prompt":"Fix the bug."}`
	fixture := seedProposedAction(t, pool, "claude_code.delegate", proposed)
	final := `{"repo":"cortex","issue_key":"PAY-142","base_branch":"main","prompt":"Fix the bug in bug.txt. Edited by a person."}`
	decider := fixture.userID
	if _, err := querier(pool).ApproveAgentAction(context.Background(), store.ApproveAgentActionParams{
		ID:           fixture.actionID,
		UserID:       decider,
		FinalPayload: []byte(final),
		DecidedBy:    &decider,
		ProposedAt:   pgtype.Timestamptz{Time: time.Time{}, Valid: true},
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	return delegationRig{pool: pool, repo: repo, worktree: worktree, worker: worker, fixture: fixture}
}

// storedResult is agent_actions.result.
type storedResult struct {
	Summary string         `json:"summary"`
	Detail  map[string]any `json:"detail"`
}

func decodeResult(t *testing.T, raw []byte) storedResult {
	t.Helper()
	var r storedResult
	if len(raw) == 0 {
		return r
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("decode result %s: %v", raw, err)
	}
	return r
}

type progressEvent struct {
	seq     int
	payload map[string]any
}

func progressEvents(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) []progressEvent {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT seq, payload FROM run_events WHERE agent_run_id = $1 AND type = 'delegate_progress' ORDER BY seq`, runID)
	if err != nil {
		t.Fatalf("query progress: %v", err)
	}
	defer rows.Close()
	var out []progressEvent
	for rows.Next() {
		var e progressEvent
		var raw []byte
		if err := rows.Scan(&e.seq, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &e.payload); err != nil {
			t.Fatalf("decode progress payload: %v", err)
		}
		out = append(out, e)
	}
	return out
}

func resumeJobs(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) int {
	t.Helper()
	return queryInt(t, pool,
		`SELECT count(*) FROM river_job WHERE kind = 'resume_run' AND args->>'run_id' = $1`, runID.String())
}

func eventSeq(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID, eventType string) int {
	t.Helper()
	return queryInt(t, pool,
		`SELECT coalesce(max(seq), -1) FROM run_events WHERE agent_run_id = $1 AND type = $2`, runID, eventType)
}

// The happy path: worktree, commit, result detail, ordered nested progress.
func TestADelegationRunsEndToEndOnAFreshBranch(t *testing.T) {
	fake := newFakeClaude(t, "echo fixed > bug.txt", cannedStream, "", 0)
	rig := newDelegationRig(t, fake, 30*time.Second)
	ctx := context.Background()

	if err := rig.worker.Perform(ctx, rig.fixture.actionID, 1); err != nil {
		t.Fatalf("Perform: %v", err)
	}

	// Claude Code ran once, in a fresh worktree on the new branch, and was
	// given the approved (edited) prompt on stdin.
	if n := fake.calls(t); n != 1 {
		t.Fatalf("claude invoked %d times, want 1", n)
	}
	cwd := fake.read(t, "cwd")
	if cwd == rig.repo || !strings.HasPrefix(cwd, rig.worktree+string(filepath.Separator)) {
		t.Errorf("claude ran in %q, want a fresh worktree under %q (never the checkout %q)", cwd, rig.worktree, rig.repo)
	}
	if got := fake.read(t, "prompt"); got != "Fix the bug in bug.txt. Edited by a person." {
		t.Errorf("stdin prompt = %q, want the approved (edited) prompt", got)
	}

	row := loadAction(t, rig.pool, rig.fixture.actionID)
	if row.status != actions.StatusExecuted {
		t.Fatalf("status = %q (error %v), want executed", row.status, row.errText)
	}
	res := decodeResult(t, row.result)
	if res.Summary != "Fixed the bug." {
		t.Errorf("result.summary = %q, want Claude Code's final message", res.Summary)
	}
	d := res.Detail
	branch, _ := d["branch"].(string)
	if !strings.HasPrefix(branch, "cortex/") || !strings.Contains(strings.ToUpper(branch), "PAY-142") {
		t.Errorf("detail.branch = %q, want cortex/<issue-key>-<short-id>", branch)
	}
	if got := fake.read(t, "branch"); got != branch {
		t.Errorf("claude's worktree was on %q, want the delegation branch %q", got, branch)
	}
	wantDetail := map[string]any{
		"base_branch":    "main",
		"session_id":     "sess-abc",
		"num_turns":      float64(3),
		"duration_ms":    float64(1234),
		"total_cost_usd": 0.03,
		"outcome":        "changes",
	}
	for k, want := range wantDetail {
		if d[k] != want {
			t.Errorf("detail.%s = %v, want %v", k, d[k], want)
		}
	}
	if subs, _ := d["subagents"].([]any); len(subs) != 1 || subs[0] != "Explore" {
		t.Errorf("detail.subagents = %v, want [Explore]", d["subagents"])
	}
	if files, _ := d["files_changed"].([]any); len(files) != 1 || files[0] != "bug.txt" {
		t.Errorf("detail.files_changed = %v, want [bug.txt]", d["files_changed"])
	}
	if stat, _ := d["diffstat"].(string); !strings.Contains(stat, "1 file changed") {
		t.Errorf("detail.diffstat = %q, want a git shortstat", stat)
	}

	// The runner committed the change on the branch; main is untouched and
	// nothing was pushed (the repo has no remote to push to, and none was added).
	sha, _ := d["commit_sha"].(string)
	if sha == "" || !strings.HasPrefix(gitRun(t, rig.repo, "rev-parse", branch), sha) {
		t.Errorf("detail.commit_sha = %q, want the branch head %s", sha, gitRun(t, rig.repo, "rev-parse", branch))
	}
	if got := gitRun(t, rig.repo, "show", branch+":bug.txt"); got != "fixed" {
		t.Errorf("bug.txt on %s = %q, want Claude Code's edit committed", branch, got)
	}
	if got := gitRun(t, rig.repo, "show", "main:bug.txt"); got != "bug" {
		t.Errorf("bug.txt on main = %q, want it untouched", got)
	}
	if n := gitRun(t, rig.repo, "rev-list", "--count", "main.."+branch); n != "1" {
		t.Errorf("commits on %s beyond main = %s, want 1", branch, n)
	}
	if remotes := gitRun(t, rig.repo, "remote"); remotes != "" {
		t.Errorf("remotes = %q, want none added", remotes)
	}

	// Progress: in seq order, in stream order, subagent steps under their parent.
	events := progressEvents(t, rig.pool, rig.fixture.runID)
	if len(events) == 0 {
		t.Fatal("no delegate_progress events were appended")
	}
	var uses [][2]string
	// Read back in seq order, so matching the stream's order below is what
	// proves the seqs were allocated in emission order.
	for _, e := range events {
		if e.payload["action_id"] != rig.fixture.actionID.String() {
			t.Errorf("progress action_id = %v, want %s", e.payload["action_id"], rig.fixture.actionID)
		}
		if e.payload["kind"] == "tool_use" {
			id, _ := e.payload["id"].(string)
			parent, _ := e.payload["parent_id"].(string)
			uses = append(uses, [2]string{id, parent})
		}
	}
	wantUses := [][2]string{{"toolu_1", ""}, {"toolu_2", "toolu_1"}, {"toolu_3", ""}}
	if len(uses) != len(wantUses) {
		t.Fatalf("tool_use steps = %v, want %v", uses, wantUses)
	}
	for i := range wantUses {
		if uses[i] != wantUses[i] {
			t.Errorf("tool_use step %d = %v, want %v", i, uses[i], wantUses[i])
		}
	}
	var sawBlockResult bool
	for _, e := range events {
		if detail, _ := e.payload["detail"].(string); strings.Contains(detail, "found it in bug.txt") {
			sawBlockResult = true
		}
	}
	if !sawBlockResult {
		t.Error("the block-array tool_result's text never reached a progress step")
	}
	if last, done := events[len(events)-1].seq, eventSeq(t, rig.pool, rig.fixture.runID, "action_executed"); done < 0 || last > done {
		t.Errorf("last progress seq %d, action_executed seq %d: progress must precede the settlement", last, done)
	}
	if n := resumeJobs(t, rig.pool, rig.fixture.runID); n != 1 {
		t.Errorf("resume_run jobs = %d, want 1 so the run reports the summary", n)
	}
}

// Spec: branch `cortex/<issue-key>-<short-id>`, example "cortex/PAY-142-3f9a",
// and the acceptance test reviews it with `git diff main...cortex/<ISSUE-KEY>-*`.
func TestADelegationBranchIsNamedAfterTheIssueKeyAsWritten(t *testing.T) {
	fake := newFakeClaude(t, "echo fixed > bug.txt", cannedStream, "", 0)
	rig := newDelegationRig(t, fake, 30*time.Second)
	if err := rig.worker.Perform(context.Background(), rig.fixture.actionID, 1); err != nil {
		t.Fatalf("Perform: %v", err)
	}
	branch, _ := decodeResult(t, loadAction(t, rig.pool, rig.fixture.actionID).result).Detail["branch"].(string)
	if !strings.HasPrefix(branch, "cortex/PAY-142-") {
		t.Errorf("branch = %q, want cortex/PAY-142-<short-id> so `git diff main...cortex/PAY-142-*` finds it", branch)
	}
}

// Claude Code exits non-zero → failed with stderr in error, and the
// run is resumed and told.
func TestADelegationThatExitsNonZeroFailsWithItsStderr(t *testing.T) {
	stream := `{"type":"system","subtype":"init","session_id":"sess-x"}`
	fake := newFakeClaude(t, "", stream, "echo 'claude: authentication expired, run /login' >&2", 1)
	rig := newDelegationRig(t, fake, 30*time.Second)

	if err := rig.worker.Perform(context.Background(), rig.fixture.actionID, 1); err != nil {
		t.Fatalf("Perform: %v (an upstream failure is recorded, not returned)", err)
	}
	row := loadAction(t, rig.pool, rig.fixture.actionID)
	if row.status != actions.StatusFailed {
		t.Fatalf("status = %q, want failed", row.status)
	}
	if row.errText == nil || !strings.Contains(*row.errText, "authentication expired") {
		t.Errorf("error = %v, want Claude Code's stderr in it", row.errText)
	}
	if eventSeq(t, rig.pool, rig.fixture.runID, "action_failed") < 0 {
		t.Error("no action_failed event: the run's timeline does not say the delegation failed")
	}
	if n := resumeJobs(t, rig.pool, rig.fixture.runID); n != 1 {
		t.Errorf("resume_run jobs = %d, want 1 so the run resumes and is told", n)
	}
	if n := gitRun(t, rig.repo, "rev-list", "--all", "--count"); n != "1" {
		t.Errorf("commits in repo = %s, want only the initial one after a failed run with no edits", n)
	}
}

// A Claude Code that never finishes is stopped at the timeout;
// the action is failed and result.detail.outcome is "timeout".
func TestADelegationThatTimesOutFailsWithOutcomeTimeout(t *testing.T) {
	stream := `{"type":"system","subtype":"init","session_id":"sess-t"}`
	fake := newFakeClaude(t, "", stream, "sleep 60", 0)
	rig := newDelegationRig(t, fake, time.Second)

	start := time.Now()
	if err := rig.worker.Perform(context.Background(), rig.fixture.actionID, 1); err != nil {
		t.Fatalf("Perform: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("Perform took %v with a 1s timeout: the child was not stopped", elapsed)
	}
	row := loadAction(t, rig.pool, rig.fixture.actionID)
	if row.status != actions.StatusFailed {
		t.Fatalf("status = %q, want failed", row.status)
	}
	if row.errText == nil || !strings.Contains(strings.ToLower(*row.errText), "timed out") {
		t.Errorf("error = %v, want it to say the delegation timed out", row.errText)
	}
	if n := resumeJobs(t, rig.pool, rig.fixture.runID); n != 1 {
		t.Errorf("resume_run jobs = %d, want 1", n)
	}
	if got := decodeResult(t, row.result).Detail["outcome"]; got != "timeout" {
		t.Errorf("result.detail.outcome = %v (result %s), want \"timeout\" per the spec", got, row.result)
	}
}

// No changes → executed, outcome no_changes, and no commit.
func TestADelegationWithNoChangesIsExecutedWithoutACommit(t *testing.T) {
	fake := newFakeClaude(t, "", noChangeStream, "", 0)
	rig := newDelegationRig(t, fake, 30*time.Second)

	if err := rig.worker.Perform(context.Background(), rig.fixture.actionID, 1); err != nil {
		t.Fatalf("Perform: %v", err)
	}
	row := loadAction(t, rig.pool, rig.fixture.actionID)
	if row.status != actions.StatusExecuted {
		t.Fatalf("status = %q (error %v), want executed: concluding no fix is needed is a result", row.status, row.errText)
	}
	res := decodeResult(t, row.result)
	if res.Detail["outcome"] != "no_changes" {
		t.Errorf("detail.outcome = %v, want no_changes", res.Detail["outcome"])
	}
	if sha, ok := res.Detail["commit_sha"]; ok && sha != "" {
		t.Errorf("detail.commit_sha = %v, want none", sha)
	}
	if !strings.Contains(res.Summary, "No fix needed") {
		t.Errorf("summary = %q, want Claude Code's final message", res.Summary)
	}
	if n := gitRun(t, rig.repo, "rev-list", "--all", "--count"); n != "1" {
		t.Errorf("commits in repo = %s, want only the initial one", n)
	}
	if n := resumeJobs(t, rig.pool, rig.fixture.runID); n != 1 {
		t.Errorf("resume_run jobs = %d, want 1", n)
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
