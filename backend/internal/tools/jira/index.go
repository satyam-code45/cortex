package jira

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"cortex/internal/tools"
)

// The indexing crawl.
//
// It lives here, not in internal/rag, so it can reuse searchIssues and the
// comment fetch the tools already use. The indexed copy of an issue is built
// from the same fields and the same ADF flattening the live tool returns, which
// is what stops the archive and the live read from disagreeing about what a
// ticket says.

const (
	// IndexMaxIssues is the default ceiling on one crawl.
	IndexMaxIssues = 400

	// indexFieldSet is what the crawl requests per issue. It is wider than
	// searchFields because an indexed document is read for meaning, not scanned
	// as a list: the description is the whole point of indexing an issue.
	indexFieldSet = "key,summary,description,status,assignee,reporter,priority,issuetype,resolution,project,parent,duedate,created,updated,labels"

	// indexOrder sorts the crawl. Most-recently-updated first means a crawl cut
	// off by the cap keeps the live end of the project rather than its archive.
	//
	// It is only the ORDER BY half: a JQL query that is *nothing but* a sort is
	// rejected outright by Atlassian —
	//
	//     HTTP 400: Unbounded JQL queries are not allowed here.
	//               Please add a search restriction to your query.
	//
	// — which is what this crawl shipped with, so every Jira index job failed its
	// five attempts and indexed nothing. buildJQL supplies the restriction.
	indexOrder = "ORDER BY updated DESC"
)

// Source adapts a Client to the indexing pipeline.
type Source struct {
	client *Client
	limit  int
	// projects restricts the crawl. Empty means "every project this account can
	// see", which is discovered at crawl time rather than assumed.
	projects []string
	logger   *slog.Logger
}

var _ tools.DocumentSource = (*Source)(nil)

// NewSource builds the indexing source. limit <= 0 uses IndexMaxIssues.
//
// projects scopes the crawl to those project keys. Empty is the default and
// means every visible project: this is a system that reads real data, so the
// crawl reaches everything the account does unless someone narrows it
// deliberately — the same shape as GMAIL_QUERY_SCOPE. Narrowing it is how an
// unrelated project (a site's pre-existing sample project, say) is kept out of
// the vector store.
func NewSource(c *Client, limit int, projects []string, logger *slog.Logger) *Source {
	if limit <= 0 {
		limit = IndexMaxIssues
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Source{client: c, limit: limit, projects: projects, logger: logger}
}

// Name identifies the source.
func (s *Source) Name() string { return sourceJira }

// FetchAll crawls issues and folds each one's comment thread into its document.
//
// One document per issue, comments included, rather than one per comment. A
// comment is rarely self-contained — "agreed, let's push to the 14th" means
// nothing without the ticket it is on — so embedding them separately produces
// chunks that retrieve well and explain nothing. Keeping the thread with the
// issue also means a citation points at the ticket, which is the thing a reader
// can actually open.
//
// This is the expensive crawl: one extra request per issue for its comments. It
// is bounded by the document cap, and by the throttle in internal/tools/httpx.
func (s *Source) FetchAll(ctx context.Context) ([]tools.Document, error) {
	jql, err := s.buildJQL(ctx)
	if err != nil {
		return nil, err
	}

	issues, err := s.searchForIndex(ctx, jql)
	if err != nil {
		return nil, err
	}

	documents := make([]tools.Document, 0, len(issues))
	for _, iss := range issues {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		comments, _, err := s.client.fetchComments(ctx, iss.Key)
		if err != nil {
			// One unreadable thread must not cost the crawl: the issue itself is
			// still worth indexing, and the next run picks the comments up.
			s.logger.Warn("jira: indexing an issue without its comments",
				"issue", iss.Key, "error", err)
		}

		documents = append(documents, tools.Document{
			Source:     sourceJira,
			ExternalID: iss.Key,
			Title:      fmt.Sprintf("%s %s", iss.Key, iss.Fields.Summary),
			URL:        s.client.BrowseURL(iss.Key),
			Content:    renderIssueDocument(iss, comments),
			Metadata: map[string]any{
				"status":   iss.Fields.Status.name("unknown"),
				"assignee": iss.Fields.Assignee.display("unassigned"),
				"type":     iss.Fields.IssueType.name("unknown"),
				"labels":   iss.Fields.Labels,
				"due_date": iss.Fields.DueDate,
				"comments": len(comments),
			},
			Timestamp: parseJiraTime(iss.Fields.Updated),
		})
	}
	return documents, nil
}

// buildJQL produces the crawl query.
//
// The restriction is a project list rather than a date window. Both satisfy
// Atlassian's "unbounded query" rule, but a project list says what it means —
// "everything in the projects we index" — where `updated >= -520w` is a
// stand-in for "everything" that silently starts dropping issues the day the
// data outlives the window. It also gives the scoping in NewSource somewhere to
// land.
//
// When no projects are configured the list is discovered from the site, so the
// default is still every project this account can see.
func (s *Source) buildJQL(ctx context.Context) (string, error) {
	keys := s.projects
	if len(keys) == 0 {
		projects, err := s.client.listProjects(ctx)
		if err != nil {
			return "", fmt.Errorf("jira: discover projects to index: %w", err)
		}
		for _, p := range projects {
			keys = append(keys, p.Key)
		}
	}

	// Keys are interpolated into JQL, so they are validated rather than escaped.
	// A configured key comes from a human editing .env and a discovered one from
	// the API, but neither is a reason to build a query out of unchecked input.
	valid := make([]string, 0, len(keys))
	for _, key := range keys {
		normalized := strings.ToUpper(strings.TrimSpace(key))
		if normalized == "" {
			continue
		}
		if !projectKeyPattern.MatchString(normalized) {
			s.logger.Warn("jira: skipping an invalid project key for the index crawl", "key", key)
			continue
		}
		valid = append(valid, normalized)
	}

	if len(valid) == 0 {
		// Better than falling back to an unbounded query, which the API rejects
		// anyway: this names the actual problem — the account can see no projects,
		// or every configured key was malformed.
		return "", fmt.Errorf("jira: no valid projects to index (configured: %d)", len(s.projects))
	}

	return fmt.Sprintf("project IN (%s) %s", strings.Join(valid, ", "), indexOrder), nil
}

// searchForIndex runs the crawl query with the wider field set.
//
// searchIssues is not reused directly because it requests searchFields, which
// omits the description — the single most valuable thing on the issue to index.
func (s *Source) searchForIndex(ctx context.Context, jql string) ([]issue, error) {
	var (
		collected []issue
		pageToken string
	)

	for len(collected) < s.limit {
		query := url.Values{}
		query.Set("jql", jql)
		query.Set("fields", indexFieldSet)
		query.Set("maxResults", strconv.Itoa(min(s.limit-len(collected), searchPageSize)))
		if pageToken != "" {
			query.Set("nextPageToken", pageToken)
		}

		var page searchResponse
		if err := s.client.get(ctx, "/rest/api/3/search/jql", query, &page); err != nil {
			return nil, fmt.Errorf("jira: list issues for indexing: %w", err)
		}
		collected = append(collected, page.Issues...)

		if page.NextPageToken == "" || page.IsLast || len(page.Issues) == 0 {
			break
		}
		pageToken = page.NextPageToken
	}

	return collected[:min(len(collected), s.limit)], nil
}

// renderIssueDocument flattens an issue and its thread into indexable text.
//
// The header carries the structured fields as prose rather than as a table:
// they are embedded along with everything else, and "Status: Blocked" retrieves
// for a question about blocked work in a way a bare column value does not.
func renderIssueDocument(iss issue, comments []comment) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# %s: %s\n\n", iss.Key, iss.Fields.Summary)
	fmt.Fprintf(&b, "Project: %s. Type: %s. Status: %s. Priority: %s.\n",
		projectName(iss.Fields.Project),
		iss.Fields.IssueType.name("unknown"),
		iss.Fields.Status.name("unknown"),
		iss.Fields.Priority.name("none"))
	fmt.Fprintf(&b, "Assignee: %s. Reporter: %s. Due: %s. Updated: %s.\n",
		iss.Fields.Assignee.display("unassigned"),
		iss.Fields.Reporter.display("unknown"),
		formatDate(iss.Fields.DueDate),
		formatDate(iss.Fields.Updated))
	if len(iss.Fields.Labels) > 0 {
		fmt.Fprintf(&b, "Labels: %s.\n", strings.Join(iss.Fields.Labels, ", "))
	}
	if iss.Fields.Parent != nil && iss.Fields.Parent.Key != "" {
		fmt.Fprintf(&b, "Parent: %s %s.\n", iss.Fields.Parent.Key, iss.Fields.Parent.Fields.Summary)
	}

	if description := ADFToText(iss.Fields.Description); strings.TrimSpace(description) != "" {
		b.WriteString("\n## Description\n\n")
		b.WriteString(description)
		b.WriteString("\n")
	}

	if len(comments) > 0 {
		b.WriteString("\n## Comments\n")
		for _, c := range comments {
			body := strings.TrimSpace(ADFToText(c.Body))
			if body == "" {
				continue
			}
			// A blank line before each comment so the chunker's paragraph
			// preference splits between comments rather than through one.
			fmt.Fprintf(&b, "\n%s — %s:\n%s\n",
				formatDate(c.Created), c.Author.display("unknown"), body)
		}
	}

	return b.String()
}

// projectName reads a possibly-absent project reference.
func projectName(p *projectValue) string {
	if p == nil || p.Name == "" {
		return "unknown"
	}
	return p.Name
}
