package jira

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"cortex/internal/tools"
)

// Jira REST v3 response shapes, narrowed to the fields Cortex reads.
//
// These structs are the compaction boundary: a field that is not declared here
// never reaches the model, which is the point. Adding a field costs tokens on
// every iteration of every run that touches the tool, so each one has to earn
// its place.

// namedValue covers the many Jira sub-objects whose only interesting field is a
// display name: status, priority, issue type, resolution.
type namedValue struct {
	Name string `json:"name"`
}

// name safely reads a possibly-absent named value.
func (n *namedValue) name(fallback string) string {
	if n == nil || n.Name == "" {
		return fallback
	}
	return n.Name
}

// userValue is a Jira user reference.
type userValue struct {
	AccountID   string `json:"accountId"`
	DisplayName string `json:"displayName"`
	// EmailAddress is set only on /myself (and even there Atlassian privacy
	// settings may hide it); issue/comment user references omit it.
	EmailAddress string `json:"emailAddress"`
}

// display safely reads a possibly-absent user's display name.
func (u *userValue) display(fallback string) string {
	if u == nil || u.DisplayName == "" {
		return fallback
	}
	return u.DisplayName
}

// projectValue identifies the project an issue belongs to.
type projectValue struct {
	ID   string `json:"id"`
	Key  string `json:"key"`
	Name string `json:"name"`
}

// parentValue is an issue's parent (epic or story), summary included.
type parentValue struct {
	Key    string `json:"key"`
	Fields struct {
		Summary string `json:"summary"`
	} `json:"fields"`
}

// issueFields is the `fields` object of an issue.
type issueFields struct {
	Summary     string          `json:"summary"`
	Description json.RawMessage `json:"description"`
	Status      *namedValue     `json:"status"`
	Priority    *namedValue     `json:"priority"`
	IssueType   *namedValue     `json:"issuetype"`
	Resolution  *namedValue     `json:"resolution"`
	Assignee    *userValue      `json:"assignee"`
	Reporter    *userValue      `json:"reporter"`
	Project     *projectValue   `json:"project"`
	Parent      *parentValue    `json:"parent"`
	DueDate     string          `json:"duedate"`
	Created     string          `json:"created"`
	Updated     string          `json:"updated"`
	Labels      []string        `json:"labels"`
}

// issue is a single issue as returned by the search and get endpoints.
type issue struct {
	ID     string      `json:"id"`
	Key    string      `json:"key"`
	Fields issueFields `json:"fields"`
}

// searchResponse is the body of GET /rest/api/3/search/jql.
//
// The legacy /rest/api/3/search endpoint was removed by Atlassian (CHANGE-2046)
// and now returns 410 Gone. Its replacement is cursor-paginated and reports no
// total, so nothing here may depend on knowing the result count up front.
type searchResponse struct {
	Issues        []issue `json:"issues"`
	NextPageToken string  `json:"nextPageToken"`
	IsLast        bool    `json:"isLast"`
}

// changelogItem is one field change within a changelog entry.
type changelogItem struct {
	Field      string `json:"field"`
	FieldID    string `json:"fieldId"`
	FieldType  string `json:"fieldtype"`
	From       string `json:"from"`
	FromString string `json:"fromString"`
	To         string `json:"to"`
	ToString   string `json:"toString"`
}

// changelogEntry is one recorded edit, which may touch several fields at once.
type changelogEntry struct {
	ID      string          `json:"id"`
	Author  *userValue      `json:"author"`
	Created string          `json:"created"`
	Items   []changelogItem `json:"items"`
}

// changelogResponse is the body of GET /rest/api/3/issue/{key}/changelog.
type changelogResponse struct {
	Values     []changelogEntry `json:"values"`
	StartAt    int              `json:"startAt"`
	MaxResults int              `json:"maxResults"`
	Total      int              `json:"total"`
	IsLast     bool             `json:"isLast"`
}

// comment is one issue comment. Body is ADF.
type comment struct {
	ID      string          `json:"id"`
	Author  *userValue      `json:"author"`
	Body    json.RawMessage `json:"body"`
	Created string          `json:"created"`
	Updated string          `json:"updated"`
}

// commentsResponse is the body of GET /rest/api/3/issue/{key}/comment.
type commentsResponse struct {
	Comments   []comment `json:"comments"`
	StartAt    int       `json:"startAt"`
	MaxResults int       `json:"maxResults"`
	Total      int       `json:"total"`
}

// issueKeyPattern matches a Jira issue key, e.g. ATLAS-145.
//
// Keys are interpolated into request paths, so they are validated rather than
// merely escaped: a strict check both closes the traversal question and gives
// the model a precise correction when it invents something like "the payments
// ticket" as a key.
var issueKeyPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*-[1-9][0-9]*$`)

// normalizeIssueKey validates and upper-cases an issue key.
func normalizeIssueKey(key string) (string, error) {
	trimmed := strings.ToUpper(strings.TrimSpace(key))
	if !issueKeyPattern.MatchString(trimmed) {
		// Wrapped so the agent loop knows not to retry: the key will be just as
		// invalid on a second attempt.
		return "", fmt.Errorf("%q is not a valid Jira issue key; expected a form like ATLAS-145: %w",
			key, tools.ErrInvalidArgument)
	}
	return trimmed, nil
}

// jiraTimeLayouts are the timestamp formats Jira emits. The API uses a numeric
// zone offset without a colon, which time.RFC3339 does not accept, so the
// layouts are tried in order.
var jiraTimeLayouts = []string{
	"2006-01-02T15:04:05.000-0700",
	"2006-01-02T15:04:05-0700",
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02",
}

// parseJiraTime parses a Jira timestamp, returning nil when it is absent or
// unparseable.
//
// Nil rather than the zero time on purpose: Evidence.Timestamp is rendered in
// citations, and a missing timestamp should read as unknown rather than as the
// year 1.
func parseJiraTime(value string) *time.Time {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	for _, layout := range jiraTimeLayouts {
		if parsed, err := time.Parse(layout, trimmed); err == nil {
			return &parsed
		}
	}
	return nil
}

// formatDate renders a Jira timestamp as a bare date, which is all the agent
// reasons over. Unparseable values fall back to the raw string so information is
// never silently dropped.
func formatDate(value string) string {
	if strings.TrimSpace(value) == "" {
		return "none"
	}
	if parsed := parseJiraTime(value); parsed != nil {
		return parsed.Format("2006-01-02")
	}
	return value
}

// snippet shortens text for an EvidenceItem. The cap lives with the evidence
// schema it serves, in package tools.
func snippet(text string) string { return tools.Snippet(text) }
