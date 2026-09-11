package jira_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"cortex/internal/tools"
	"cortex/internal/tools/jira"
)

// The Jira write tools.
//
// The property every test here asserts, one way or another: calling a write
// tool changes nothing on the site. The tools read — an issue type has to exist
// on the target project, a transition has to be available from where the issue
// sits — and then hand back the finished request as a proposal. The fake site
// refuses every non-GET and records it, so "performed no side effect" is an
// assertion about the wire rather than about the code's intentions.
//
// The mutating half is exercised separately, through the Writers, which is what
// the execution job calls after a human approves — and it is handed the FINAL
// payload, which may differ from what the agent proposed.

const (
	pathProjectAtlas = "/rest/api/3/project/ATLAS"
	pathIssueCreate  = "/rest/api/3/issue"
	pathTransitions  = "/rest/api/3/issue/ATLAS-101/transitions"
	projectAtlasBody = `{"id":"10000","key":"ATLAS","name":"Atlas","issueTypes":[` +
		`{"id":"10001","name":"Task","subtask":false},` +
		`{"id":"10002","name":"Bug","subtask":false},` +
		`{"id":"10003","name":"Story","subtask":false}]}`
	transitionsBody = `{"transitions":[` +
		`{"id":"21","name":"Start progress","to":{"name":"In Progress"}},` +
		`{"id":"31","name":"Done","to":{"name":"Done"}}]}`
)

// writeRoutes is the read surface the write tools use at proposal time. Each
// path is scripted generously enough for the tool that needs it: creating an
// issue reads the project twice (once to confirm it exists, once for its issue
// types).
func writeRoutes() map[string]*route {
	return map[string]*route{
		pathProjectAtlas: sequenceRoute(
			response{body: projectAtlasBody},
			response{body: projectAtlasBody},
		),
		pathIssue:       fixtureRoute("issue_atlas_101.json", "issue_atlas_101.json"),
		pathTransitions: sequenceRoute(response{body: transitionsBody}, response{body: transitionsBody}),
	}
}

// writeTool returns one write tool by name.
func writeTool(t *testing.T, f *fakeJira, name string) (*jira.Client, tools.Tool) {
	t.Helper()
	client := f.client()
	for _, tool := range jira.NewWriteTools(client) {
		if tool.Name() == name {
			return client, tool
		}
	}
	t.Fatalf("tool %q is not in the Jira write tool set", name)
	return nil, nil
}

// writer returns one executor by action name.
func writerFor(t *testing.T, client *jira.Client, action string) tools.Writer {
	t.Helper()
	for _, w := range jira.NewWriters(client) {
		if w.Action() == action {
			return w
		}
	}
	t.Fatalf("no writer for action %q", action)
	return nil
}

// assertNoMutations fails when the fake site saw anything other than a read.
func assertNoMutations(t *testing.T, f *fakeJira) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, req := range f.requests {
		if req.method != http.MethodGet {
			t.Errorf("the write tool sent %s %s; proposing must perform no side effect",
				req.method, req.path)
		}
	}
}

// decodePayload decodes a proposal payload.
func decodeProposal(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode proposal payload %s: %v", raw, err)
	}
	return out
}

// ---------------------------------------------------------------------------
// proposing
// ---------------------------------------------------------------------------

// Every Jira write tool returns a proposal naming its capability,
// tells the model plainly that nothing has happened, and mutates nothing.
func TestJiraWriteToolsProposeWithoutChangingAnything(t *testing.T) {
	tests := []struct {
		name       string
		tool       string
		args       string
		wantAction string
		wantFields map[string]any
	}{
		{
			name: "create issue",
			tool: "jira_create_issue",
			args: `{"project_key":"atlas","issue_type":"Task","summary":"Track sandbox certification",
			        "description":"From ATLAS-102.","labels":["compliance"],"due_date":"2026-10-01"}`,
			wantAction: "jira.create_issue",
			wantFields: map[string]any{
				"project_key": "ATLAS",
				"issue_type":  "Task",
				"summary":     "Track sandbox certification",
				"description": "From ATLAS-102.",
				"due_date":    "2026-10-01",
			},
		},
		{
			name:       "update issue",
			tool:       "jira_update_issue",
			args:       `{"key":"atlas-101","fields":{"due_date":"2026-10-01"}}`,
			wantAction: "jira.update_issue",
			wantFields: map[string]any{"key": "ATLAS-101"},
		},
		{
			name:       "add comment",
			tool:       "jira_add_comment",
			args:       `{"key":"ATLAS-101","body":"  Vendor confirmed the sandbox notice.  "}`,
			wantAction: "jira.add_comment",
			wantFields: map[string]any{"key": "ATLAS-101", "body": "Vendor confirmed the sandbox notice."},
		},
		{
			name:       "transition issue",
			tool:       "jira_transition_issue",
			args:       `{"key":"ATLAS-101","to_status":"in progress"}`,
			wantAction: "jira.transition_issue",
			wantFields: map[string]any{"key": "ATLAS-101", "to_status": "In Progress", "transition_id": "21"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeJira(t, writeRoutes())
			_, tool := writeTool(t, f, tt.tool)

			result, err := tool.Execute(context.Background(), json.RawMessage(tt.args))
			if err != nil {
				t.Fatalf("%s: %v", tt.tool, err)
			}
			if result.Proposal == nil {
				t.Fatalf("%s returned no proposal; a write tool must never act", tt.tool)
			}
			if result.Proposal.Source != "jira" {
				t.Errorf("proposal source = %q, want jira", result.Proposal.Source)
			}
			if result.Proposal.Action != tt.wantAction {
				t.Errorf("proposal action = %q, want %q", result.Proposal.Action, tt.wantAction)
			}
			if strings.TrimSpace(result.Proposal.Summary) == "" {
				t.Error("the proposal has no summary; the audit list and the card headline it")
			}
			// The observation must leave no room to believe the write happened.
			for _, want := range []string{"PROPOSED, NOT YET DONE", "Nothing has been sent"} {
				if !strings.Contains(result.Content, want) {
					t.Errorf("observation %q does not contain %q", result.Content, want)
				}
			}

			payload := decodeProposal(t, result.Proposal.Payload)
			for key, want := range tt.wantFields {
				if got := payload[key]; got != want {
					t.Errorf("payload[%q] = %v, want %v (payload %v)", key, got, want, payload)
				}
			}
			assertNoMutations(t, f)
		})
	}
}

// The proposal is complete and final: a display-only field resolved at proposal
// time is what lets the approval card name the project, issue or status in words
// rather than as an opaque key.
func TestJiraProposalsCarryTheContextTheCardRenders(t *testing.T) {
	tests := []struct {
		name      string
		tool      string
		args      string
		field     string
		wantValue any
	}{
		{
			name: "create names the project", tool: "jira_create_issue",
			args:  `{"project_key":"ATLAS","summary":"Track certification"}`,
			field: "project_name", wantValue: "Atlas",
		},
		{
			name: "comment names the issue", tool: "jira_add_comment",
			args:  `{"key":"ATLAS-101","body":"noted"}`,
			field: "issue_summary", wantValue: "Checkout fails on expired payment tokens",
		},
		{
			name: "transition names where it is now", tool: "jira_transition_issue",
			args:  `{"key":"ATLAS-101","to_status":"Done"}`,
			field: "from_status", wantValue: "Blocked",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeJira(t, writeRoutes())
			_, tool := writeTool(t, f, tt.tool)
			result, err := tool.Execute(context.Background(), json.RawMessage(tt.args))
			if err != nil {
				t.Fatalf("%s: %v", tt.tool, err)
			}
			payload := decodeProposal(t, result.Proposal.Payload)
			if got := payload[tt.field]; got != tt.wantValue {
				t.Errorf("payload[%q] = %v, want %v", tt.field, got, tt.wantValue)
			}
			assertNoMutations(t, f)
		})
	}
}

// An update proposal records what it is replacing, read at proposal time. A card
// that showed only "due date: 2026-10-01" hides the thing worth reviewing.
func TestAnUpdateProposalRecordsTheCurrentValues(t *testing.T) {
	f := newFakeJira(t, writeRoutes())
	_, tool := writeTool(t, f, "jira_update_issue")

	result, err := tool.Execute(context.Background(), json.RawMessage(
		`{"key":"ATLAS-101","fields":{"summary":"Checkout fails on expired tokens","due_date":"2026-10-01"}}`))
	if err != nil {
		t.Fatalf("jira_update_issue: %v", err)
	}
	payload := decodeProposal(t, result.Proposal.Payload)
	before, ok := payload["before"].(map[string]any)
	if !ok {
		t.Fatalf("payload has no before block (%v); the card cannot show what changes", payload)
	}
	if before["due_date"] != "2026-07-31" {
		t.Errorf("before[due_date] = %v, want the issue's current due date", before["due_date"])
	}
	if before["summary"] != "Checkout fails on expired payment tokens" {
		t.Errorf("before[summary] = %v, want the issue's current summary", before["summary"])
	}
	// The fields the update does not mention are absent, not blanked.
	if _, present := before["description"]; present {
		t.Errorf("before carries description (%v) for an update that does not change it", before)
	}
	assertNoMutations(t, f)
}

// ---------------------------------------------------------------------------
// validation at proposal time
// ---------------------------------------------------------------------------

// The tool validates against the live project, and a validation
// failure comes back as a correctable argument error naming what is valid —
// never as a 400 after somebody has approved the issue.
func TestJiraWriteToolsRefuseAnInvalidRequest(t *testing.T) {
	tests := []struct {
		name      string
		tool      string
		args      string
		wantParts []string
	}{
		{
			name: "issue type the project does not offer", tool: "jira_create_issue",
			args:      `{"project_key":"ATLAS","issue_type":"Epic","summary":"Track certification"}`,
			wantParts: []string{"Epic", "Task", "Bug", "Story"},
		},
		{
			name: "no summary", tool: "jira_create_issue",
			args:      `{"project_key":"ATLAS","summary":"   "}`,
			wantParts: []string{"summary"},
		},
		{
			name: "no project key", tool: "jira_create_issue",
			args:      `{"summary":"Track certification"}`,
			wantParts: []string{"project key"},
		},
		{
			name: "due date that is not a date", tool: "jira_create_issue",
			args:      `{"project_key":"ATLAS","summary":"Track it","due_date":"next Friday"}`,
			wantParts: []string{"YYYY-MM-DD"},
		},
		{
			name: "update that changes nothing", tool: "jira_update_issue",
			args:      `{"key":"ATLAS-101","fields":{}}`,
			wantParts: []string{"changes no fields"},
		},
		{
			name: "empty comment", tool: "jira_add_comment",
			args:      `{"key":"ATLAS-101","body":"  "}`,
			wantParts: []string{"empty"},
		},
		{
			name: "unreachable status", tool: "jira_transition_issue",
			args:      `{"key":"ATLAS-101","to_status":"Cancelled"}`,
			wantParts: []string{"Cancelled", "In Progress", "Done"},
		},
		{
			name: "no target status", tool: "jira_transition_issue",
			args:      `{"key":"ATLAS-101","to_status":""}`,
			wantParts: []string{"status"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeJira(t, writeRoutes())
			_, tool := writeTool(t, f, tt.tool)

			result, err := tool.Execute(context.Background(), json.RawMessage(tt.args))
			if err == nil {
				t.Fatalf("%s accepted an invalid request and proposed %v", tt.tool, result.Proposal)
			}
			if !errors.Is(err, tools.ErrInvalidArgument) {
				t.Errorf("error = %v, want it marked ErrInvalidArgument so the loop does not retry it", err)
			}
			if result.Proposal != nil {
				t.Error("a refused request still produced a proposal")
			}
			for _, want := range tt.wantParts {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q, so the model cannot correct it", err, want)
				}
			}
			assertNoMutations(t, f)
		})
	}
}

// A project or issue that does not exist is an argument error too, not an
// outage: the key will be just as wrong the second time.
func TestJiraWriteToolsRefuseAnUnknownTarget(t *testing.T) {
	tests := []struct {
		name   string
		routes map[string]*route
		tool   string
		args   string
	}{
		{
			name: "unknown project",
			routes: map[string]*route{
				"/rest/api/3/project/GHOST": bodyRoute(http.StatusNotFound,
					`{"errorMessages":["No project could be found with key 'GHOST'."]}`),
			},
			tool: "jira_create_issue",
			args: `{"project_key":"GHOST","summary":"Track certification"}`,
		},
		{
			name: "unknown issue",
			routes: map[string]*route{
				"/rest/api/3/issue/ATLAS-999": bodyRoute(http.StatusNotFound,
					`{"errorMessages":["Issue does not exist or you do not have permission to see it."]}`),
			},
			tool: "jira_add_comment",
			args: `{"key":"ATLAS-999","body":"noted"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeJira(t, tt.routes)
			_, tool := writeTool(t, f, tt.tool)

			_, err := tool.Execute(context.Background(), json.RawMessage(tt.args))
			if err == nil {
				t.Fatalf("%s proposed a write against a target that does not exist", tt.tool)
			}
			if !errors.Is(err, tools.ErrInvalidArgument) {
				t.Errorf("error = %v, want ErrInvalidArgument", err)
			}
			assertNoMutations(t, f)
		})
	}
}

// ---------------------------------------------------------------------------
// the executors
// ---------------------------------------------------------------------------

// The writer performs the payload it is handed — the human-approved
// one — not whatever the agent originally proposed.
func TestJiraWritersPerformTheApprovedPayload(t *testing.T) {
	f := newFakeJira(t, map[string]*route{
		pathIssueCreate: bodyRoute(http.StatusCreated, `{"id":"10999","key":"ATLAS-501"}`),
	})
	client := f.client()
	w := writerFor(t, client, "jira.create_issue")

	// The payload a person edited before approving.
	approved := `{"project_key":"ATLAS","issue_type":"Task","summary":"Track sandbox certification",
	              "description":"Edited by a human before approval."}`
	outcome, err := w.Execute(context.Background(), json.RawMessage(approved))
	if err != nil {
		t.Fatalf("execute jira.create_issue: %v", err)
	}
	if outcome.Detail["issue_key"] != "ATLAS-501" {
		t.Errorf("outcome detail = %v, want the created issue key", outcome.Detail)
	}
	if !strings.Contains(outcome.Summary, "ATLAS-501") {
		t.Errorf("outcome summary = %q, want it to name the created issue for the transcript", outcome.Summary)
	}

	posts := f.requestsTo(pathIssueCreate)
	if len(posts) != 1 {
		t.Fatalf("POST /rest/api/3/issue happened %d time(s), want exactly 1", len(posts))
	}
	if posts[0].method != http.MethodPost {
		t.Errorf("create used %s, want POST", posts[0].method)
	}
}

// Each write tool has exactly one executor, and the two sets name the same
// capabilities: an approved action with no writer is an approval nothing can
// carry out.
func TestEveryJiraWriteToolHasAnExecutor(t *testing.T) {
	f := newFakeJira(t, nil)
	client := f.client()

	toolActions := map[string]bool{}
	for _, tool := range jira.NewWriteTools(client) {
		if _, ok := tool.(tools.Proposing); !ok {
			t.Errorf("%s is not marked as proposing", tool.Name())
		}
		// The action name a tool proposes is discoverable only by calling it,
		// so the mapping is asserted from the writer side and by count here.
		toolActions[tool.Name()] = true
	}
	writerActions := map[string]bool{}
	for _, w := range jira.NewWriters(client) {
		writerActions[w.Action()] = true
	}
	want := []string{"jira.create_issue", "jira.update_issue", "jira.add_comment", "jira.transition_issue"}
	for _, action := range want {
		if !writerActions[action] {
			t.Errorf("no executor for %s; an approval for it could never be carried out", action)
		}
	}
	if len(writerActions) != len(want) {
		t.Errorf("writers = %v, want exactly %v", writerActions, want)
	}
	if len(toolActions) != len(want) {
		t.Errorf("write tools = %v, want %d of them", toolActions, len(want))
	}
	// Non-goal, asserted so it stays one: nothing here deletes.
	for name := range toolActions {
		if strings.Contains(name, "delete") {
			t.Errorf("%s exists; deleting is not in this system's vocabulary", name)
		}
	}
	for action := range writerActions {
		if strings.Contains(action, "delete") {
			t.Errorf("a writer for %s exists; deleting is not in this system's vocabulary", action)
		}
	}
}
