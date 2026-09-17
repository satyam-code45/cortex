package jira_test

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	"cortex/internal/tools"
	"cortex/internal/tools/jira"
)

// Project confinement for the demo workspace.
//
// The demo Jira site belongs to whoever runs the deployment, and its API token
// can see every project on it. Anyone who can sign in can ask a question, and
// the question becomes JQL written by the model — from text that may itself
// have come out of a Jira comment or an email. So the confinement cannot be a
// line in the system prompt or a convention the tool descriptions ask for. It
// is enforced on the client, on the way out, and these tests are what say so.
//
// A user's own connected Jira is deliberately unscoped: it is their site, their
// token, and their whole backlog to search.

// jqlSentTo returns the jql parameter of each search request the fake received.
func jqlSentTo(t *testing.T, f *fakeJira) []string {
	t.Helper()
	var sent []string
	for _, r := range f.requestsTo(pathSearchJQL) {
		sent = append(sent, r.query.Get("jql"))
	}
	return sent
}

func TestNewClientRequiresAnExplicitScopeDecision(t *testing.T) {
	t.Parallel()

	base := jira.Config{BaseURL: "https://example.atlassian.net", Email: "a@b.c", APIToken: "t"}

	tests := []struct {
		name          string
		projects      []string
		allowUnscoped bool
		wantErr       string
	}{
		{
			// The default must not be "can read everything". A caller that
			// forgets the pin gets an error, not a client with the run of the
			// site.
			name:    "neither a scope nor an explicit opt-out is refused",
			wantErr: "Projects is required",
		},
		{
			name:          "an explicit opt-out is accepted",
			allowUnscoped: true,
		},
		{
			name:     "a scope is accepted",
			projects: []string{"ATLAS"},
		},
		{
			// Asking for both says the caller does not know which it wants,
			// and guessing either way would be a silent security decision.
			name:          "a scope contradicting the opt-out is refused",
			projects:      []string{"ATLAS"},
			allowUnscoped: true,
			wantErr:       "contradicts",
		},
		{
			// Keys are interpolated into JQL, not parameterized, so a
			// malformed one is refused at construction rather than escaped at
			// use.
			name:     "a malformed project key is refused",
			projects: []string{"not a key"},
			wantErr:  "not a valid project key",
		},
		{
			name:     "a JQL injection attempt in a key is refused",
			projects: []string{"ATLAS) OR (project IS NOT EMPTY"},
			wantErr:  "not a valid project key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := base
			cfg.Projects = tt.projects
			cfg.AllowUnscoped = tt.allowUnscoped

			client, err := jira.NewClient(cfg)
			switch {
			case tt.wantErr != "":
				if err == nil {
					t.Fatalf("NewClient() error = nil, want one containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("NewClient() error = %q, want it to contain %q", err, tt.wantErr)
				}
			case err != nil:
				t.Fatalf("NewClient() error = %v, want nil", err)
			default:
				if got, want := len(client.Projects()), len(tt.projects); got != want {
					t.Errorf("Projects() length = %d, want %d", got, want)
				}
			}
		})
	}
}

func TestScopedSearchConfinesEveryQueryToThePinnedProjects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		jql  string
		want string
	}{
		{
			name: "a bare condition is ANDed with the scope",
			jql:  "status = Blocked",
			want: "project IN (ATLAS, BEACON) AND (status = Blocked)",
		},
		{
			// The parentheses are the point. Without them the model's trailing
			// OR would bind looser than the AND and match every issue on the
			// site: `project IN (X) AND a OR b` is not confined.
			name: "a top-level OR cannot escape the scope",
			jql:  `project = SECRET OR text ~ "salary"`,
			want: `project IN (ATLAS, BEACON) AND (project = SECRET OR text ~ "salary")`,
		},
		{
			// JQL requires ORDER BY last, so the scope cannot simply be
			// prepended to a query that has one.
			name: "a trailing ORDER BY stays last",
			jql:  "status = Done ORDER BY updated DESC",
			want: "project IN (ATLAS, BEACON) AND (status = Done) ORDER BY updated DESC",
		},
		{
			name: "a lowercase order by is recognized too",
			jql:  "status = Done order by updated",
			want: "project IN (ATLAS, BEACON) AND (status = Done) order by updated",
		},
		{
			name: "an order-only query keeps the bare scope",
			jql:  "ORDER BY created DESC",
			want: "project IN (ATLAS, BEACON) ORDER BY created DESC",
		},
		{
			// Asking for a project outside the pin is not an error — it simply
			// cannot match, which is the honest outcome.
			name: "naming an unpinned project yields a contradiction, not an escape",
			jql:  "project = SECRET",
			want: "project IN (ATLAS, BEACON) AND (project = SECRET)",
		},
		{
			// "order by" inside quotes is a phrase being searched for, not an
			// ordering. Splitting there would move the closing parenthesis into
			// the middle of the condition.
			name: "an ORDER BY inside a quoted phrase is not treated as ordering",
			jql:  `summary ~ "order by priority"`,
			want: `project IN (ATLAS, BEACON) AND (summary ~ "order by priority")`,
		},
		{
			name: "a quoted phrase and a real ordering are told apart",
			jql:  `summary ~ "order by priority" ORDER BY updated DESC`,
			want: `project IN (ATLAS, BEACON) AND (summary ~ "order by priority") ORDER BY updated DESC`,
		},
		{
			// JQL takes single quotes too, and a scanner that only knew about
			// double ones split this inside the literal and produced a syntax
			// error out of a perfectly good search.
			name: "an ORDER BY inside a single-quoted phrase is not treated as ordering",
			jql:  `summary ~ 'order by priority'`,
			want: `project IN (ATLAS, BEACON) AND (summary ~ 'order by priority')`,
		},
		{
			name: "a double quote inside a single-quoted literal is ordinary text",
			jql:  `summary ~ 'the "final" plan'`,
			want: `project IN (ATLAS, BEACON) AND (summary ~ 'the "final" plan')`,
		},
		{
			name: "a backslash-escaped quote inside a literal is ordinary text",
			jql:  `summary ~ "the \"final\" plan"`,
			want: `project IN (ATLAS, BEACON) AND (summary ~ "the \"final\" plan")`,
		},
		{
			name: "parentheses inside a single-quoted literal do not count",
			jql:  `summary ~ 'Q2 plan (draft)'`,
			want: `project IN (ATLAS, BEACON) AND (summary ~ 'Q2 plan (draft)')`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeJira(t, map[string]*route{
				pathSearchJQL: fixtureRoute("search_jql_empty.json"),
			})
			tool := f.scopedToolSet("ATLAS", "BEACON")["jira_search_issues"]

			args, err := json.Marshal(map[string]any{"jql": tt.jql})
			if err != nil {
				t.Fatalf("marshal arguments: %v", err)
			}
			if _, err := tool.Execute(context.Background(), args); err != nil {
				t.Fatalf("Execute() error = %v", err)
			}

			sent := jqlSentTo(t, f)
			if len(sent) != 1 {
				t.Fatalf("search requests = %d, want 1", len(sent))
			}
			if sent[0] != tt.want {
				t.Errorf("jql sent =\n  %s\nwant\n  %s", sent[0], tt.want)
			}
		})
	}
}

func TestScopedSearchRejectsUnbalancedParentheses(t *testing.T) {
	t.Parallel()

	// Unbalanced input is the one shape that could move where the wrapping
	// parenthesis closes, so it is refused rather than repaired — and refused
	// as an invalid argument, so the agent loop reports it to the model instead
	// of retrying an identically broken query.
	f := newFakeJira(t, map[string]*route{})
	tool := f.scopedToolSet("ATLAS")["jira_search_issues"]

	args := json.RawMessage(`{"jql":"status = Done)"}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("Execute() error = nil, want an invalid-argument error")
	}
	if !errors.Is(err, tools.ErrInvalidArgument) {
		t.Errorf("error = %v, want it to wrap tools.ErrInvalidArgument", err)
	}
	if f.requestCount() != 0 {
		t.Errorf("requests = %d, want 0 — the query must never reach Jira", f.requestCount())
	}
}

func TestUnscopedSearchPassesTheQueryThrough(t *testing.T) {
	t.Parallel()

	// A user's own connected Jira: no rewriting at all, or their own projects
	// would silently vanish from their own answers.
	f := newFakeJira(t, map[string]*route{
		pathSearchJQL: fixtureRoute("search_jql_empty.json"),
	})
	_, byName := f.toolSet()

	args := json.RawMessage(`{"jql":"status = Blocked ORDER BY updated DESC"}`)
	if _, err := byName["jira_search_issues"].Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	sent := jqlSentTo(t, f)
	if len(sent) != 1 {
		t.Fatalf("search requests = %d, want 1", len(sent))
	}
	if want := "status = Blocked ORDER BY updated DESC"; sent[0] != want {
		t.Errorf("jql sent = %q, want %q unchanged", sent[0], want)
	}
}

func TestScopedIssueReadsRefuseAKeyOutsideThePin(t *testing.T) {
	t.Parallel()

	// Search is not the only way to reach an issue: keys are guessable, so
	// confining search alone would leave SECRET-1 as reachable as ATLAS-1.
	for _, name := range []string{"jira_get_issue", "jira_get_issue_history", "jira_get_comments"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeJira(t, map[string]*route{})
			tool := f.scopedToolSet("ATLAS")[name]

			args := json.RawMessage(`{"key":"SECRET-1"}`)
			_, err := tool.Execute(context.Background(), args)
			if err == nil {
				t.Fatal("Execute() error = nil, want a refusal")
			}
			if !errors.Is(err, tools.ErrInvalidArgument) {
				t.Errorf("error = %v, want it to wrap tools.ErrInvalidArgument", err)
			}
			if !strings.Contains(err.Error(), "ATLAS") {
				t.Errorf("error = %q, want it to name the projects that ARE readable", err)
			}
			if f.requestCount() != 0 {
				t.Errorf("requests = %d, want 0 — the key must never reach Jira", f.requestCount())
			}
		})
	}
}

func TestScopedIssueReadsAllowAKeyInsideThePin(t *testing.T) {
	t.Parallel()

	f := newFakeJira(t, map[string]*route{
		pathIssue: fixtureRoute("issue_atlas_101.json"),
	})
	// Lower case on purpose: the pin is normalized, so the check must be too.
	tool := f.scopedToolSet("atlas")["jira_get_issue"]

	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"key":"ATLAS-101"}`)); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := len(f.requestsTo(pathIssue)); got != 1 {
		t.Errorf("issue requests = %d, want 1", got)
	}
}

func TestScopedListProjectsReportsOnlyThePinnedProjects(t *testing.T) {
	t.Parallel()

	// Listing is how the model learns which keys exist. A scoped client that
	// listed the whole site would hand it the keys to projects it cannot read,
	// and would disclose the existence of projects that are not part of the
	// demo.
	const sitePayload = `{"values":[
		{"key":"ATLAS","name":"Atlas"},
		{"key":"SECRET","name":"Acquisition"},
		{"key":"BEACON","name":"Beacon"}
	]}`

	f := newFakeJira(t, map[string]*route{
		projectSearchPath: bodyRoute(200, sitePayload),
	})
	tool := f.scopedToolSet("ATLAS", "BEACON")["jira_list_projects"]

	result, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if strings.Contains(result.Content, "SECRET") || strings.Contains(result.Content, "Acquisition") {
		t.Errorf("content names an unpinned project:\n%s", result.Content)
	}
	for _, want := range []string{"ATLAS", "BEACON"} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("content is missing pinned project %s:\n%s", want, result.Content)
		}
	}
	for _, item := range result.Evidence {
		if item.ExternalID == "SECRET" {
			t.Error("evidence cites an unpinned project")
		}
	}
}

// The scope wrapper must survive a query written to break out of it.
//
// scopedJQL wraps the model's condition in parentheses and ANDs it after
// `project IN (...)`. That holds only while Cortex agrees with Jira about where
// string literals begin and end: a scanner that can be driven into "inside a
// string" state while Jira is outside one will skip a `)` that Jira parses for
// real, closing the wrapper early. The query then reads
// `(project IN (DEMO) AND ...) OR (...)`, and the second disjunct is
// unconstrained — every issue on the site, no project key required.
//
// JQL accepts BOTH ' and " as delimiters and honours a backslash escape inside
// a literal, so each of these inputs desynchronizes a scanner that knows only
// about bare double quotes. The threat is not hypothetical: the JQL is written
// by a model from a question, and from whatever text the agent has already read
// out of a Jira comment or an email.
func TestScopedSearchCannotBeEscapedByQuoteTricks(t *testing.T) {
	t.Parallel()

	escapes := []struct {
		name string
		jql  string
	}{
		{
			name: "a single-quoted literal containing a double quote",
			jql:  `summary ~ 'a"b' ) OR created >= -3650d AND (summary ~ 'c"d'`,
		},
		{
			name: "a backslash-escaped quote inside a double-quoted literal",
			jql:  `summary ~ "a\"b" ) OR project = SECRET AND (summary ~ "c\"d"`,
		},
		{
			name: "an unterminated single-quoted literal",
			jql:  `summary ~ 'abc`,
		},
		{
			name: "a single quote hiding the closing parenthesis",
			jql:  `summary ~ 'x)' ) OR project IS NOT EMPTY AND (summary ~ 'y('`,
		},
	}

	for _, tc := range escapes {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeJira(t, map[string]*route{
				pathSearchJQL: fixtureRoute("search_jql_empty.json"),
			})
			tool := f.scopedToolSet("DEMO")["jira_search_issues"]

			args, err := json.Marshal(map[string]any{"jql": tc.jql})
			if err != nil {
				t.Fatalf("marshal arguments: %v", err)
			}
			_, execErr := tool.Execute(context.Background(), args)

			// Either outcome is acceptable — refusing the query outright, or
			// sending one that is still confined. What must never happen is a
			// query reaching Jira whose scope wrapper has been closed early.
			for _, sent := range jqlSentTo(t, f) {
				if !scopeSurvives(sent) {
					t.Errorf("the scope wrapper was escaped.\n  input: %s\n  sent:  %s", tc.jql, sent)
				}
			}
			if execErr == nil && f.requestCount() == 0 {
				t.Error("no error and no request: the query vanished")
			}
		})
	}
}

// scopeSurvives reports whether every part of the query outside a string
// literal is still inside the parenthesis scopedJQL opened after `AND`.
//
// Written independently of the production scanner on purpose: a bug shared
// between the code and its test would be invisible. This one lexes JQL the way
// Jira documents it — both delimiters, backslash escapes — and checks that the
// wrapper opened right after the scope prefix does not close before the end of
// the condition.
func scopeSurvives(sent string) bool {
	const scope = "project IN (DEMO)"
	const prefix = scope + " AND ("
	if !strings.HasPrefix(sent, prefix) {
		// No wrapper: the only legitimate shapes are the bare scope, optionally
		// followed by an ordering. An ordering constrains nothing, so it cannot
		// widen what the scope admits — but anything else after the scope was
		// not wrapped and is therefore outside it.
		//
		// Matched case-insensitively because JQL keywords are, and the checker
		// disagreeing with JQL on that is how a false alarm looks.
		rest := strings.TrimSpace(strings.TrimPrefix(sent, scope))
		return strings.HasPrefix(sent, scope) && (rest == "" || orderByOnly(rest))
	}

	depth := 1 // the wrapper's own open parenthesis
	var quote byte
	escaped := false
	for i := len(prefix); i < len(sent); i++ {
		c := sent[i]
		switch {
		case escaped:
			escaped = false
		case quote != 0:
			if c == '\\' {
				escaped = true
			} else if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				// The wrapper closed. Anything after it other than an ORDER BY
				// clause is outside the scope.
				rest := strings.TrimSpace(sent[i+1:])
				return rest == "" || orderByOnly(rest)
			}
		}
	}
	return false
}

// orderByOnly reports whether rest is nothing but an ORDER BY clause.
//
// JQL keywords are case-insensitive and accept any run of whitespace between
// the two words, so the checker has to allow both — a checker stricter than JQL
// reports escapes that are not escapes, which is how a real one gets lost in
// the noise.
var orderByClause = regexp.MustCompile(`(?i)^ORDER\s+BY\b`)

func orderByOnly(rest string) bool {
	return orderByClause.MatchString(rest)
}

// FuzzScopedJQLNeverEscapesTheScope is the guard the hand-written cases cannot
// be: the escapes above were each found only after someone thought of them, and
// the first version of this confinement passed a suite of six cases while being
// trivially bypassable.
//
// The invariant: whatever JQL goes in, either the tool refuses it, or every
// clause of what reaches Jira is inside the parenthesis opened after
// `project IN (DEMO) AND`. scopeSurvives checks that with its own JQL lexer,
// written independently of the production one — a bug shared between the code
// and its checker would be invisible to both.
//
// Run longer with:
//
//	go test ./internal/tools/jira/ -run Fuzz -fuzz FuzzScopedJQL -fuzztime 60s
func FuzzScopedJQLNeverEscapesTheScope(f *testing.F) {
	seeds := []string{
		`status = Blocked`,
		`summary ~ 'a"b' ) OR created >= -3650d AND (summary ~ 'c"d'`,
		`summary ~ "a\"b" ) OR project = SECRET AND (summary ~ "c\"d"`,
		`summary ~ 'x)' ) OR project IS NOT EMPTY AND (summary ~ 'y('`,
		`ORDER BY created DESC`,
		`summary ~ 'order by priority' ORDER BY updated`,
		`project = SECRET OR text ~ "salary"`,
		`) OR (`,
		`'`,
		`"`,
		`\`,
		`(((`,
		`summary ~ "a" ORDER BY "b) OR (c"`,
		// Keyword spelling edge cases: JQL keywords are case-insensitive and
		// take any run of whitespace between the words.
		`ORder BY`,
		`ORDER  BY`,
		// An ORDER BY nested inside parentheses is not the query's ordering.
		// Splitting there stranded the matching `)` in the tail and closed the
		// scope wrapper early.
		`(ORDER BY )0`,
		`(a AND (b ORDER BY c))`,
		`(status = Done) ORDER BY updated`,
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	client, err := jira.NewClient(jira.Config{
		BaseURL:  "https://example.atlassian.net",
		Email:    "a@b.c",
		APIToken: "t",
		Projects: []string{"DEMO"},
	})
	if err != nil {
		f.Fatalf("build scoped client: %v", err)
	}

	f.Fuzz(func(t *testing.T, jql string) {
		sent, err := jira.ExportScopedJQL(client, jql)
		if err != nil {
			return // refused, which is always a safe outcome
		}
		if !scopeSurvives(sent) {
			t.Errorf("the scope wrapper was escaped.\n  input: %q\n  sent:  %q", jql, sent)
		}
	})
}

// An over-long JQL query is refused before it is scanned.
//
// Belt and braces alongside the linear scan: the query is written by the model
// out of text the agent has read, so its length is not Cortex's to choose, and
// a bound that is obviously far above any real query costs nothing. Real JQL is
// tens of bytes.
func TestScopedSearchRefusesAnOverlongQuery(t *testing.T) {
	t.Parallel()

	f := newFakeJira(t, map[string]*route{})
	tool := f.scopedToolSet("DEMO")["jira_search_issues"]

	args, err := json.Marshal(map[string]any{
		"jql": "status = Done OR " + strings.Repeat("summary ~ 'x' OR ", 500) + "status = Open",
	})
	if err != nil {
		t.Fatalf("marshal arguments: %v", err)
	}

	_, execErr := tool.Execute(context.Background(), args)
	if execErr == nil {
		t.Fatal("Execute() error = nil, want a refusal")
	}
	if !errors.Is(execErr, tools.ErrInvalidArgument) {
		t.Errorf("error = %v, want it to wrap tools.ErrInvalidArgument", execErr)
	}
	if f.requestCount() != 0 {
		t.Errorf("requests = %d, want 0 — an over-long query must never reach Jira", f.requestCount())
	}
}

// A query at a realistic length is not caught by the bound.
func TestScopedSearchAcceptsAnOrdinaryQuery(t *testing.T) {
	t.Parallel()

	f := newFakeJira(t, map[string]*route{
		pathSearchJQL: fixtureRoute("search_jql_empty.json"),
	})
	tool := f.scopedToolSet("DEMO")["jira_search_issues"]

	args := json.RawMessage(`{"jql":"status = Blocked AND assignee IS NOT EMPTY ORDER BY updated DESC"}`)
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if f.requestCount() != 1 {
		t.Errorf("requests = %d, want 1", f.requestCount())
	}
}
