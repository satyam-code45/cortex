package notion_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"cortex/internal/tools"
	"cortex/internal/tools/notion"
)

// The Notion write tools.
//
// Two tools, neither of which changes anything when called. They read the target
// page — which doubles as the access check, because for an integration token a
// missing page almost always means the page was never shared with it — and hand
// back the finished markdown as a proposal.
//
// Notion is also the source with no capability endpoint: nothing reports whether
// the integration may insert content, so the refusal can only surface at
// execution. That makes the error mapping part of the feature, not cosmetics: a
// 403 restricted_resource has to come back naming the checkbox to tick.

// notionWriteRoutes is the read surface the write tools use at proposal time.
func notionWriteRoutes() map[string]*route {
	return map[string]*route{
		pathPage: fixtureRoute("page_atlas_plan.json", "page_atlas_plan.json"),
	}
}

// notionWriteTool returns one write tool by name.
func notionWriteTool(t *testing.T, f *fakeNotion, name string) (*notion.Client, tools.Tool) {
	t.Helper()
	client := f.client()
	for _, tool := range notion.NewWriteTools(client) {
		if tool.Name() == name {
			return client, tool
		}
	}
	t.Fatalf("tool %q is not in the Notion write tool set", name)
	return nil, nil
}

// notionWriter returns one executor by action name.
func notionWriter(t *testing.T, client *notion.Client, action string) tools.Writer {
	t.Helper()
	for _, w := range notion.NewWriters(client) {
		if w.Action() == action {
			return w
		}
	}
	t.Fatalf("no writer for action %q", action)
	return nil
}

// assertNotionReadOnly fails when the fake workspace saw anything but a read.
func assertNotionReadOnly(t *testing.T, f *fakeNotion) {
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

func decodeNotionProposal(t *testing.T, raw json.RawMessage) map[string]any {
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

// Both Notion write tools propose and mutate nothing.
func TestNotionWriteToolsProposeWithoutChangingAnything(t *testing.T) {
	tests := []struct {
		name       string
		tool       string
		args       string
		wantAction string
		wantFields map[string]any
	}{
		{
			name:       "append to page",
			tool:       "notion_append_to_page",
			args:       `{"page_id":"` + testPageID + `","markdown":"## Sandbox\n- vendor confirmed"}`,
			wantAction: "notion.append_to_page",
			wantFields: map[string]any{
				"page_id":    testPageID,
				"markdown":   "## Sandbox\n- vendor confirmed",
				"page_title": testPageTitle,
			},
		},
		{
			name: "create page",
			tool: "notion_create_page",
			args: `{"parent_page_id":"` + testPageID + `","title":"Sandbox certification",
			        "markdown":"Tracking the vendor notice."}`,
			wantAction: "notion.create_page",
			wantFields: map[string]any{
				"parent_page_id": testPageID,
				"title":          "Sandbox certification",
				"markdown":       "Tracking the vendor notice.",
				"parent_title":   testPageTitle,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeNotion(t, notionWriteRoutes())
			_, tool := notionWriteTool(t, f, tt.tool)

			result, err := tool.Execute(context.Background(), json.RawMessage(tt.args))
			if err != nil {
				t.Fatalf("%s: %v", tt.tool, err)
			}
			if result.Proposal == nil {
				t.Fatalf("%s returned no proposal; a write tool must never act", tt.tool)
			}
			if result.Proposal.Source != "notion" {
				t.Errorf("proposal source = %q, want notion", result.Proposal.Source)
			}
			if result.Proposal.Action != tt.wantAction {
				t.Errorf("proposal action = %q, want %q", result.Proposal.Action, tt.wantAction)
			}
			if strings.TrimSpace(result.Proposal.Summary) == "" {
				t.Error("the proposal has no summary")
			}
			for _, want := range []string{"PROPOSED, NOT YET DONE", "Nothing has been sent"} {
				if !strings.Contains(result.Content, want) {
					t.Errorf("observation %q does not contain %q", result.Content, want)
				}
			}
			payload := decodeNotionProposal(t, result.Proposal.Payload)
			for key, want := range tt.wantFields {
				if got := payload[key]; got != want {
					t.Errorf("payload[%q] = %v, want %v (payload %v)", key, got, want, payload)
				}
			}
			assertNotionReadOnly(t, f)
		})
	}
}

// Validation happens at proposal time, as a correctable argument
// error, so a malformed request never reaches a person to approve.
func TestNotionWriteToolsRefuseAnInvalidRequest(t *testing.T) {
	tests := []struct {
		name      string
		tool      string
		args      string
		wantParts []string
	}{
		{
			name: "append with nothing to add", tool: "notion_append_to_page",
			args:      `{"page_id":"` + testPageID + `","markdown":"   "}`,
			wantParts: []string{"nothing to append"},
		},
		{
			name: "append to an id that is not a page id", tool: "notion_append_to_page",
			args:      `{"page_id":"the Q3 plan","markdown":"content"}`,
			wantParts: []string{"not a valid Notion page id"},
		},
		{
			name: "create with no title", tool: "notion_create_page",
			args:      `{"parent_page_id":"` + testPageID + `","title":" ","markdown":"body"}`,
			wantParts: []string{"no title"},
		},
		{
			name: "create with no content", tool: "notion_create_page",
			args:      `{"parent_page_id":"` + testPageID + `","title":"Sandbox","markdown":""}`,
			wantParts: []string{"no content"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeNotion(t, notionWriteRoutes())
			_, tool := notionWriteTool(t, f, tt.tool)

			result, err := tool.Execute(context.Background(), json.RawMessage(tt.args))
			if err == nil {
				t.Fatalf("%s accepted an invalid request and proposed %v", tt.tool, result.Proposal)
			}
			if !errors.Is(err, tools.ErrInvalidArgument) {
				t.Errorf("error = %v, want ErrInvalidArgument", err)
			}
			if result.Proposal != nil {
				t.Error("a refused request still produced a proposal")
			}
			for _, want := range tt.wantParts {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
			assertNotionReadOnly(t, f)
		})
	}
}

// A page the integration cannot see is a dead end at proposal time, not after
// an approval: for an integration token a 404 almost always means the page was
// never shared with it.
func TestAppendRefusesAPageTheIntegrationCannotRead(t *testing.T) {
	f := newFakeNotion(t, map[string]*route{
		pathPage: bodyRoute(http.StatusNotFound, `{"object":"error","status":404,`+
			`"code":"object_not_found","message":"Could not find page with ID."}`),
	})
	_, tool := notionWriteTool(t, f, "notion_append_to_page")

	result, err := tool.Execute(context.Background(),
		json.RawMessage(`{"page_id":"`+testPageID+`","markdown":"content"}`))
	if err == nil {
		t.Fatalf("append proposed against an unreadable page: %v", result.Proposal)
	}
	assertNotionReadOnly(t, f)
}

// ---------------------------------------------------------------------------
// the executors
// ---------------------------------------------------------------------------

// The executor performs the approved payload — here an edited body —
// and reports what landed so the resumed run can say it precisely.
func TestNotionWritersPerformTheApprovedPayload(t *testing.T) {
	t.Run("append", func(t *testing.T) {
		f := newFakeNotion(t, map[string]*route{
			pathRootBlocks: bodyRoute(http.StatusOK, `{"object":"list","results":[]}`),
		})
		client := f.client()
		w := notionWriter(t, client, "notion.append_to_page")

		approved := `{"page_id":"` + testPageID + `","markdown":"## Sandbox\n\nEdited by a human.",` +
			`"page_title":"` + testPageTitle + `"}`
		outcome, err := w.Execute(context.Background(), json.RawMessage(approved))
		if err != nil {
			t.Fatalf("execute notion.append_to_page: %v", err)
		}
		if outcome.Detail["page_id"] != testPageID {
			t.Errorf("outcome detail = %v, want the page it landed on", outcome.Detail)
		}
		if !strings.Contains(outcome.Summary, testPageTitle) {
			t.Errorf("outcome summary = %q, want it to name the page", outcome.Summary)
		}

		reqs := f.requestsTo(pathRootBlocks)
		if len(reqs) != 1 {
			t.Fatalf("append requests = %d, want exactly 1", len(reqs))
		}
		if reqs[0].method != http.MethodPatch {
			t.Errorf("append used %s, want PATCH", reqs[0].method)
		}
		if !strings.Contains(reqs[0].body, "Edited by a human") {
			t.Errorf("the request body %q does not carry the approved text", reqs[0].body)
		}
	})

	t.Run("create page", func(t *testing.T) {
		f := newFakeNotion(t, map[string]*route{
			"/v1/pages": bodyRoute(http.StatusOK, `{"object":"page","id":"`+testPageID+`",`+
				`"url":"`+testPageURL+`"}`),
		})
		client := f.client()
		w := notionWriter(t, client, "notion.create_page")

		approved := `{"parent_page_id":"` + testPageID + `","title":"Sandbox certification",` +
			`"markdown":"Approved body."}`
		outcome, err := w.Execute(context.Background(), json.RawMessage(approved))
		if err != nil {
			t.Fatalf("execute notion.create_page: %v", err)
		}
		if outcome.Detail["url"] != testPageURL {
			t.Errorf("outcome detail = %v, want the new page's URL so a reader can go and look", outcome.Detail)
		}
		if len(f.requestsTo("/v1/pages")) != 1 {
			t.Errorf("create requests = %d, want exactly 1", len(f.requestsTo("/v1/pages")))
		}
	})
}

// Notion's 403 restricted_resource is mapped to an instruction naming
// the capability to grant. It is the one source whose permission problem can
// only surface at execution, so the message is the whole recourse.
func TestARestrictedResourceRefusalNamesTheMissingCapability(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		action string
		body   string
	}{
		{
			name: "append", path: pathRootBlocks, action: "notion.append_to_page",
			body: `{"page_id":"` + testPageID + `","markdown":"content"}`,
		},
		{
			name: "create page", path: "/v1/pages", action: "notion.create_page",
			body: `{"parent_page_id":"` + testPageID + `","title":"Sandbox","markdown":"body"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeNotion(t, map[string]*route{
				tt.path: bodyRoute(http.StatusForbidden, `{"object":"error","status":403,`+
					`"code":"restricted_resource","message":"Insufficient permissions for this endpoint."}`),
			})
			client := f.client()
			w := notionWriter(t, client, tt.action)

			_, err := w.Execute(context.Background(), json.RawMessage(tt.body))
			if err == nil {
				t.Fatal("a refused write reported success")
			}
			for _, want := range []string{"Insert content", "Capabilities"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q — the person reading the action row "+
						"needs to know which checkbox to tick", err, want)
				}
			}
		})
	}
}

// Every Notion write tool has an executor, and appending is the only way the
// agent path can change a page: replacing content would destroy text somebody
// else wrote, which no approval dialog makes safe.
func TestNotionWriteToolsAndWritersMatch(t *testing.T) {
	f := newFakeNotion(t, nil)
	client := f.client()

	names := map[string]bool{}
	for _, tool := range notion.NewWriteTools(client) {
		if _, ok := tool.(tools.Proposing); !ok {
			t.Errorf("%s is not marked as proposing", tool.Name())
		}
		names[tool.Name()] = true
	}
	wantTools := []string{"notion_append_to_page", "notion_create_page"}
	if len(names) != len(wantTools) {
		t.Errorf("write tools = %v, want exactly %v", names, wantTools)
	}
	for _, name := range wantTools {
		if !names[name] {
			t.Errorf("%s is missing from the write tool set", name)
		}
	}

	actions := map[string]bool{}
	for _, w := range notion.NewWriters(client) {
		actions[w.Action()] = true
	}
	for _, action := range []string{"notion.append_to_page", "notion.create_page"} {
		if !actions[action] {
			t.Errorf("no executor for %s", action)
		}
	}
	if len(actions) != 2 {
		t.Errorf("writers = %v, want exactly the two append/create executors", actions)
	}
	for name := range names {
		for _, forbidden := range []string{"replace", "delete", "archive"} {
			if strings.Contains(name, forbidden) {
				t.Errorf("%s exists; the agent path must not be able to destroy content", name)
			}
		}
	}
}
