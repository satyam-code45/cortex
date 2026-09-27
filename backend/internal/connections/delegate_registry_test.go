package connections_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/connections"
	"cortex/internal/store"
)

// The delegate tool exists only while the user's runner heartbeat
// is fresh (within 2 minutes), and its repo enum is exactly the runner's repos.

const delegateToolName = "delegate_to_claude_code"

func newRunnerAwareBuilder(t *testing.T, pool *pgxpool.Pool) (*connections.RegistryBuilder, *connections.Service) {
	t.Helper()
	svc := newTestService(t, pool)
	builder, err := connections.NewRegistryBuilder(connections.RegistryBuilderConfig{
		Service:     svc,
		Demo:        newDemoRegistry(t),
		RunnerRepos: connections.OnlineRunnerRepos(store.New(pool)),
		Logger:      discardLogger(),
	})
	if err != nil {
		t.Fatalf("build registry builder: %v", err)
	}
	return builder, svc
}

func insertRunner(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, name string, repos []string, age time.Duration) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO code_runners (user_id, name, repos, last_seen_at)
		 VALUES ($1, $2, $3, now() - $4::interval)`,
		userID, name, repos, fmt.Sprintf("%d seconds", int(age.Seconds()))); err != nil {
		t.Fatalf("insert code_runner: %v", err)
	}
}

// repoEnum reads the repo enum out of the delegate tool's JSON schema.
func repoEnum(t *testing.T, schema json.RawMessage) []string {
	t.Helper()
	var s struct {
		Properties struct {
			Repo struct {
				Enum []string `json:"enum"`
			} `json:"repo"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schema, &s); err != nil {
		t.Fatalf("decode delegate tool schema %s: %v", schema, err)
	}
	return s.Properties.Repo.Enum
}

func TestDelegateToolIsOfferedOnlyWithAFreshRunner(t *testing.T) {
	cases := []struct {
		name      string
		runner    bool
		age       time.Duration
		repos     []string
		wantTool  bool
		otherUser bool // the fresh runner belongs to somebody else
	}{
		{name: "no runner", wantTool: false},
		{name: "runner last seen 3 minutes ago", runner: true, age: 3 * time.Minute,
			repos: []string{"cortex"}, wantTool: false},
		{name: "fresh heartbeat", runner: true, age: 0,
			repos: []string{"cortex", "scratch"}, wantTool: true},
		{name: "heartbeat 90s ago is still fresh", runner: true, age: 90 * time.Second,
			repos: []string{"scratch"}, wantTool: true},
		{name: "another user's fresh runner", runner: true, otherUser: true,
			repos: []string{"cortex"}, wantTool: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := testPool(t)
			jiraSite := newFakeJiraSite(t)
			builder, svc := newRunnerAwareBuilder(t, pool)
			userID := insertUser(t, pool)
			saveJira(t, svc, userID, jiraSite.server.URL, "satyam@example.com", "tok")
			if tc.runner {
				owner := userID
				if tc.otherUser {
					owner = insertUser(t, pool)
				}
				insertRunner(t, pool, owner, "laptop", tc.repos, tc.age)
			}

			registry, _, err := builder.ForUser(context.Background(), userID)
			if err != nil {
				t.Fatalf("ForUser: %v", err)
			}
			tool, ok := registry.Get(delegateToolName)
			if ok != tc.wantTool {
				t.Fatalf("delegate tool present = %v, want %v (tools %v)", ok, tc.wantTool, registry.Names())
			}
			if !ok {
				return
			}
			got := repoEnum(t, tool.Schema())
			want := slices.Clone(tc.repos)
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("repo enum = %v, want the runner's repos %v", got, want)
			}
			// It is a write: proposed, never performed by the loop.
			if !slices.Contains(registry.WriteNames(), delegateToolName) {
				t.Errorf("WriteNames() = %v, want the delegate tool counted as a write", registry.WriteNames())
			}
		})
	}
}

// Demo mode answers from the shared demo workspace: no delegation to anyone's
// machine, even with a fresh runner online.
func TestDelegateToolIsNeverOfferedInDemoMode(t *testing.T) {
	pool := testPool(t)
	builder, svc := newRunnerAwareBuilder(t, pool)
	userID := insertUser(t, pool)
	if err := svc.SetUseDemo(context.Background(), userID, true); err != nil {
		t.Fatalf("SetUseDemo: %v", err)
	}
	insertRunner(t, pool, userID, "laptop", []string{"cortex"}, 0)

	registry, _, err := builder.ForUser(context.Background(), userID)
	if err != nil {
		t.Fatalf("ForUser: %v", err)
	}
	if _, ok := registry.Get(delegateToolName); ok {
		t.Errorf("demo registry offers %s (tools %v)", delegateToolName, registry.Names())
	}
}

// The tool refuses a repo the runner does not offer, and the proposal summary
// names the issue and repo.
func TestDelegateToolValidatesAgainstTheRunnersRepos(t *testing.T) {
	pool := testPool(t)
	jiraSite := newFakeJiraSite(t)
	builder, svc := newRunnerAwareBuilder(t, pool)
	userID := insertUser(t, pool)
	saveJira(t, svc, userID, jiraSite.server.URL, "satyam@example.com", "tok")
	insertRunner(t, pool, userID, "laptop", []string{"cortex"}, 0)

	registry, _, err := builder.ForUser(context.Background(), userID)
	if err != nil {
		t.Fatalf("ForUser: %v", err)
	}
	tool, ok := registry.Get(delegateToolName)
	if !ok {
		t.Fatalf("delegate tool missing (tools %v)", registry.Names())
	}

	if _, err := tool.Execute(context.Background(), json.RawMessage(
		`{"repo":"elsewhere","issue_key":"PAY-142","prompt":"fix it"}`)); err == nil {
		t.Error("a repo the runner does not offer was accepted")
	}

	res, err := tool.Execute(context.Background(), json.RawMessage(
		`{"repo":"cortex","issue_key":"PAY-142","prompt":"Fix the retry bug described in PAY-142."}`))
	if err != nil {
		t.Fatalf("valid delegation refused: %v", err)
	}
	if res.Proposal == nil {
		t.Fatal("valid delegation returned no proposal")
	}
	if res.Proposal.Source != "claude_code" || res.Proposal.Action != "claude_code.delegate" {
		t.Errorf("proposal = %s/%s, want claude_code/claude_code.delegate", res.Proposal.Source, res.Proposal.Action)
	}
	var payload map[string]string
	if err := json.Unmarshal(res.Proposal.Payload, &payload); err != nil {
		t.Fatalf("decode proposal payload: %v", err)
	}
	if payload["base_branch"] != "main" {
		t.Errorf("base_branch = %q, want the default main", payload["base_branch"])
	}
	for _, want := range []string{"PAY-142", "cortex", "Claude Code"} {
		if !strings.Contains(res.Proposal.Summary, want) {
			t.Errorf("summary %q does not mention %q", res.Proposal.Summary, want)
		}
	}
}
