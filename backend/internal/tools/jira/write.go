package jira

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Write operations, used only by cmd/seed.
//
// No agent tool calls anything in this file. The agent's Jira surface is
// read-only by design (idea.md §25 tool guardrails); seeding is an operator
// action performed by a CLI, which is a different trust boundary entirely.
//
// These exist because Jira history cannot be fabricated. The REST API refuses
// to backdate `created` or to insert changelog rows, so the only way
// jira_get_issue_history has anything to report is for the seeder to *perform*
// the edits: set a due date, then change it; move an issue through its statuses
// one transition at a time. Every mutation here is a fact the agent later
// discovers.

// projectKeyPattern matches a Jira project key: uppercase, starts with a letter.
var projectKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)

// maxSummaryPages bounds the idempotency scan. Three projects of ~100 issues at
// 50 per page is 2 pages each; 200 is a runaway backstop, not a real limit.
const maxSummaryPages = 200

// KanbanTemplateKey creates a team-managed kanban board, which is what a free
// site offers. SimpleTemplateKey is the fallback if a site rejects it.
const (
	KanbanTemplateKey = "com.pyxis.greenhopper.jira:gh-simplified-agility-kanban"
	SimpleTemplateKey = "com.pyxis.greenhopper.jira:gh-simplified-basic"
)

// Project is a Jira project, narrowed to what the seeder needs.
//
// ID is a json.Number because Jira is not consistent about it: POST
// /rest/api/3/project returns it as a JSON number, while GET
// /rest/api/3/project/{key} returns the same value as a string. Declaring it as
// either concrete type makes one of the two endpoints fail to decode.
type Project struct {
	ID   json.Number `json:"id"`
	Key  string      `json:"key"`
	Name string      `json:"name"`
}

// Account describes the account an API token authenticates as. Email may be
// empty — Atlassian privacy settings can hide it even on /myself.
type Account struct {
	AccountID   string
	DisplayName string
	Email       string
}

// Myself returns the account the API token authenticates as. It doubles as
// the live credential check for the paste-a-key connection flow (Day 8): a
// bad token surfaces here as an APIError before anything is stored.
func (c *Client) Myself(ctx context.Context) (Account, error) {
	var user userValue
	if err := c.get(ctx, "/rest/api/3/myself", nil, &user); err != nil {
		return Account{}, fmt.Errorf("get current user: %w", err)
	}
	if user.AccountID == "" {
		return Account{}, errors.New("jira: /myself returned no accountId")
	}
	return Account{
		AccountID:   user.AccountID,
		DisplayName: user.DisplayName,
		Email:       user.EmailAddress,
	}, nil
}

// CurrentUser returns the account the API token authenticates as.
//
// The seeder needs it for leadAccountId when creating a project, and as the
// assignee for every issue: a free site has exactly one real user, so intended
// owners are recorded in labels and descriptions instead.
func (c *Client) CurrentUser(ctx context.Context) (accountID, displayName string, err error) {
	account, err := c.Myself(ctx)
	if err != nil {
		return "", "", err
	}
	return account.AccountID, account.DisplayName, nil
}

// GetProject looks up a project by key. It returns (nil, nil) when the project
// does not exist, so the seeder can tell "create it" from "something broke".
func (c *Client) GetProject(ctx context.Context, key string) (*Project, error) {
	if !projectKeyPattern.MatchString(key) {
		return nil, fmt.Errorf("jira: %q is not a valid project key", key)
	}
	var project Project
	err := c.get(ctx, "/rest/api/3/project/"+url.PathEscape(key), nil, &project)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.NotFound() {
			return nil, nil
		}
		return nil, fmt.Errorf("get project %s: %w", key, err)
	}
	return &project, nil
}

// projectSearchResponse is the body of GET /rest/api/3/project/search.
type projectSearchResponse struct {
	Values []Project `json:"values"`
	IsLast bool      `json:"isLast"`
}

// listProjects returns every project visible to the authenticated account.
//
// /project/search rather than the deprecated /project: the latter returns an
// unbounded array, and a site with hundreds of projects would put all of them
// into the prompt.
func (c *Client) listProjects(ctx context.Context) ([]Project, error) {
	query := url.Values{}
	query.Set("maxResults", strconv.Itoa(maxProjectsListed))
	query.Set("orderBy", "key")

	var resp projectSearchResponse
	if err := c.get(ctx, "/rest/api/3/project/search", query, &resp); err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	return resp.Values, nil
}

// ProjectSpec describes a project to create.
type ProjectSpec struct {
	Key           string
	Name          string
	Description   string
	LeadAccountID string
}

// CreateProject creates a team-managed software project.
//
// Template keys are the fragile part of project creation — a site can reject
// one it does not offer — so a rejection falls back to the simpler template
// before giving up.
func (c *Client) CreateProject(ctx context.Context, spec ProjectSpec) (*Project, error) {
	if !projectKeyPattern.MatchString(spec.Key) {
		return nil, fmt.Errorf("jira: %q is not a valid project key (2-10 uppercase chars)", spec.Key)
	}

	attempt := func(templateKey string) (*Project, error) {
		body := map[string]any{
			"key":                spec.Key,
			"name":               spec.Name,
			"projectTypeKey":     "software",
			"projectTemplateKey": templateKey,
			"assigneeType":       "PROJECT_LEAD",
		}
		if spec.Description != "" {
			body["description"] = spec.Description
		}
		if spec.LeadAccountID != "" {
			body["leadAccountId"] = spec.LeadAccountID
		}
		var project Project
		if err := c.post(ctx, "/rest/api/3/project", body, &project); err != nil {
			return nil, err
		}
		return &project, nil
	}

	project, err := attempt(KanbanTemplateKey)
	if err == nil {
		return project, nil
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == 400 {
		c.logger.Warn("jira: kanban template rejected, retrying with the basic template",
			"project", spec.Key, "error", err)
		project, fallbackErr := attempt(SimpleTemplateKey)
		if fallbackErr == nil {
			return project, nil
		}
		// Both causes, not just the first: if the fallback failed for an unrelated
		// reason, reporting only the original 400 points at the wrong thing.
		return nil, fmt.Errorf("create project %s: %w", spec.Key, errors.Join(err, fallbackErr))
	}
	return nil, fmt.Errorf("create project %s: %w", spec.Key, err)
}

// IssueSpec describes an issue to create.
type IssueSpec struct {
	ProjectKey  string
	Summary     string
	Description string
	IssueType   string
	Priority    string
	Labels      []string
	DueDate     string // YYYY-MM-DD, empty for none
}

// CreateIssue creates one issue and returns its key.
//
// Optional fields are dropped and the request retried once if Jira rejects
// them: which fields a project exposes depends on its template and screen
// configuration, and a team-managed board that has no priority field would
// otherwise fail the entire seed over a field nothing depends on.
func (c *Client) CreateIssue(ctx context.Context, spec IssueSpec) (string, error) {
	if spec.Summary == "" {
		return "", errors.New("jira: issue summary is required")
	}

	build := func(stage int) map[string]any {
		fields := map[string]any{
			"project":   map[string]string{"key": spec.ProjectKey},
			"summary":   spec.Summary,
			"issuetype": map[string]string{"name": issueTypeOr(spec.IssueType)},
		}
		if spec.Description != "" {
			fields["description"] = TextToADF(spec.Description)
		}
		// Stage 0 is everything. Stage 1 drops priority only. Stage 2 drops the
		// labels and the due date too.
		if stage < 1 && spec.Priority != "" {
			fields["priority"] = map[string]string{"name": spec.Priority}
		}
		if stage < 2 {
			if len(spec.Labels) > 0 {
				fields["labels"] = sanitizeLabels(spec.Labels)
			}
			if spec.DueDate != "" {
				fields["duedate"] = spec.DueDate
			}
		}
		return map[string]any{"fields": fields}
	}

	var created struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}

	// Optional fields are shed in stages if Jira rejects them, because which
	// fields a project exposes depends on its template and screen configuration —
	// a team-managed board with no priority field would otherwise fail the whole
	// seed over a field nothing depends on.
	//
	// The ordering matters and is not cosmetic. `priority` is decorative, but the
	// owner label is the only structured record of the intended assignee (REQ-2.5)
	// and the due date is the "original deadline" that evals/ground_truth.json
	// asserts. Shedding those silently would leave the changelog reading
	// "none → 2026-07-10" and make the eval fact unanswerable, so they go last and
	// loudly.
	firstErr := c.post(ctx, "/rest/api/3/issue", build(0), &created)
	if firstErr == nil {
		return created.Key, nil
	}
	var apiErr *APIError
	if !errors.As(firstErr, &apiErr) || apiErr.StatusCode != 400 {
		return "", fmt.Errorf("create issue in %s: %w", spec.ProjectKey, firstErr)
	}

	for stage := 1; stage <= 2; stage++ {
		if stage == 1 {
			c.logger.Warn("jira: issue rejected, retrying without priority",
				"project", spec.ProjectKey, "summary", spec.Summary, "error", firstErr)
		} else {
			c.logger.Error("jira: issue rejected, retrying without labels and due date — "+
				"the intended owner and the original deadline will NOT be recorded on this issue",
				"project", spec.ProjectKey, "summary", spec.Summary)
		}
		if err := c.post(ctx, "/rest/api/3/issue", build(stage), &created); err == nil {
			return created.Key, nil
		}
	}
	return "", fmt.Errorf("create issue in %s: %w", spec.ProjectKey, firstErr)
}

// AddComment posts a comment. text is converted to ADF, which REST v3 requires.
func (c *Client) AddComment(ctx context.Context, issueKey, text string) error {
	key, err := normalizeIssueKey(issueKey)
	if err != nil {
		return err
	}
	body := map[string]any{"body": TextToADF(text)}
	if err := c.post(ctx, "/rest/api/3/issue/"+url.PathEscape(key)+"/comment", body, nil); err != nil {
		return fmt.Errorf("add comment to %s: %w", key, err)
	}
	return nil
}

// SetDueDate sets or changes an issue's due date.
//
// Called once per recorded change rather than once with the final value: each
// call is what produces a changelog row, and those rows are the only evidence
// that a deadline ever moved.
func (c *Client) SetDueDate(ctx context.Context, issueKey, dueDate string) error {
	key, err := normalizeIssueKey(issueKey)
	if err != nil {
		return err
	}
	body := map[string]any{"fields": map[string]any{"duedate": dueDate}}
	if err := c.put(ctx, "/rest/api/3/issue/"+url.PathEscape(key), body, nil); err != nil {
		return fmt.Errorf("set due date on %s: %w", key, err)
	}
	return nil
}

// Transition is an available workflow transition.
type Transition struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	To   struct {
		Name string `json:"name"`
	} `json:"to"`
}

// Transitions lists the transitions currently available on an issue.
//
// Transition IDs are per-project in a team-managed site, so they cannot be
// hardcoded — and which are available depends on the issue's current status,
// which is why this is re-read before each move.
func (c *Client) Transitions(ctx context.Context, issueKey string) ([]Transition, error) {
	key, err := normalizeIssueKey(issueKey)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Transitions []Transition `json:"transitions"`
	}
	path := "/rest/api/3/issue/" + url.PathEscape(key) + "/transitions"
	if err := c.get(ctx, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("list transitions for %s: %w", key, err)
	}
	return resp.Transitions, nil
}

// TransitionToStatus moves an issue to the named status.
//
// The match is on the transition's destination status, not its label: boards
// name the transition after the action ("Start progress") while the fixture
// names the state ("In Progress"). Falls back to matching the transition name
// so an unusual board still works.
func (c *Client) TransitionToStatus(ctx context.Context, issueKey, statusName string) error {
	key, err := normalizeIssueKey(issueKey)
	if err != nil {
		return err
	}
	available, err := c.Transitions(ctx, key)
	if err != nil {
		return err
	}

	// Both passes compare the same way: trimmed and case-insensitive. They used to
	// differ, so a fixture value with stray whitespace took the wrong branch.
	target := strings.ToLower(strings.TrimSpace(statusName))
	match := func(candidate string) bool {
		return strings.ToLower(strings.TrimSpace(candidate)) == target
	}
	var chosen *Transition
	for i := range available {
		if match(available[i].To.Name) {
			chosen = &available[i]
			break
		}
	}
	if chosen == nil {
		for i := range available {
			if match(available[i].Name) {
				chosen = &available[i]
				break
			}
		}
	}
	if chosen == nil {
		names := make([]string, 0, len(available))
		for _, t := range available {
			names = append(names, fmt.Sprintf("%s→%s", t.Name, t.To.Name))
		}
		return fmt.Errorf("no transition on %s leads to status %q (available: %s)",
			key, statusName, strings.Join(names, ", "))
	}

	body := map[string]any{"transition": map[string]string{"id": chosen.ID}}
	path := "/rest/api/3/issue/" + url.PathEscape(key) + "/transitions"
	if err := c.post(ctx, path, body, nil); err != nil {
		return fmt.Errorf("transition %s to %s: %w", key, statusName, err)
	}
	return nil
}

// ListIssueSummaries maps every existing summary in a project to its issue key.
//
// This is what makes the seeder idempotent, and it is one paged search rather
// than a lookup per issue: `summary ~ "..."` is a fuzzy text match in JQL, so
// per-issue existence checks would be both ~100 extra requests and wrong.
// Comparing exact summaries in Go is cheaper and precise.
func (c *Client) ListIssueSummaries(ctx context.Context, projectKey string) (map[string]string, error) {
	if !projectKeyPattern.MatchString(projectKey) {
		return nil, fmt.Errorf("jira: %q is not a valid project key", projectKey)
	}

	byName := make(map[string]string)
	pageToken := ""
	// Bounded, and guarded against a stuck cursor: the exit condition is driven
	// entirely by an external API's response, and a deployment that keeps echoing
	// the same nextPageToken with a non-empty page would otherwise spin until the
	// seeder's 30-minute budget expired while the map grew.
	for page := 0; page < maxSummaryPages; page++ {
		query := url.Values{}
		query.Set("jql", fmt.Sprintf("project = %s ORDER BY created ASC", projectKey))
		query.Set("fields", "summary")
		query.Set("maxResults", strconv.Itoa(searchPageSize))
		if pageToken != "" {
			query.Set("nextPageToken", pageToken)
		}

		var resp searchResponse
		if err := c.get(ctx, "/rest/api/3/search/jql", query, &resp); err != nil {
			return nil, fmt.Errorf("list existing issues in %s: %w", projectKey, err)
		}
		for _, iss := range resp.Issues {
			// First writer wins: if a summary was somehow duplicated by an
			// interrupted run, keep the oldest and leave the duplicate alone
			// rather than compounding it.
			if _, seen := byName[iss.Fields.Summary]; !seen {
				byName[iss.Fields.Summary] = iss.Key
			}
		}
		if resp.NextPageToken == "" || resp.IsLast || len(resp.Issues) == 0 {
			break
		}
		if resp.NextPageToken == pageToken {
			return nil, fmt.Errorf("list existing issues in %s: cursor did not advance", projectKey)
		}
		pageToken = resp.NextPageToken
	}
	return byName, nil
}

// issueTypeOr defaults a blank issue type to Task, which every software
// template provides.
func issueTypeOr(issueType string) string {
	if strings.TrimSpace(issueType) == "" {
		return "Task"
	}
	return issueType
}

// sanitizeLabels makes labels acceptable to Jira, which rejects whitespace.
// Spaces become hyphens rather than being dropped, so "Priya Raman" stays
// legible as "priya-raman".
func sanitizeLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	seen := make(map[string]struct{}, len(labels))
	for _, label := range labels {
		cleaned := strings.ToLower(strings.Join(strings.Fields(label), "-"))
		cleaned = strings.Trim(cleaned, "-")
		if cleaned == "" {
			continue
		}
		if _, dup := seen[cleaned]; dup {
			continue
		}
		seen[cleaned] = struct{}{}
		out = append(out, cleaned)
	}
	return out
}
