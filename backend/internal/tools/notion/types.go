package notion

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"cortex/internal/tools"
)

// Notion REST response shapes, narrowed to the fields Cortex reads.
//
// As in the Jira package, these structs are the compaction boundary: a field
// that is not declared here never reaches the model.

// richText is one span of Notion rich text.
//
// PlainText is the flattened content Notion computes for us, which is why the
// nested text/mention/equation variants do not need modelling: a mention of a
// person or a date still arrives with its rendered text, so a plan that says
// "owner: @Priya Raman" reads correctly without understanding mentions.
type richText struct {
	Type        string `json:"type"`
	PlainText   string `json:"plain_text"`
	Href        string `json:"href"`
	Annotations struct {
		Bold          bool `json:"bold"`
		Italic        bool `json:"italic"`
		Strikethrough bool `json:"strikethrough"`
		Code          bool `json:"code"`
	} `json:"annotations"`
}

// textContent is the block payload shared by every text-bearing block type.
type textContent struct {
	RichText []richText `json:"rich_text"`
	// Checked is set on to_do blocks.
	Checked bool `json:"checked"`
	// Language is set on code blocks.
	Language string `json:"language"`
	// Title is set on child_page blocks, which carry no rich text.
	Title string `json:"title"`
}

// block is one Notion block. The payload lives under a key named for the
// block's own type, so it is decoded from the raw map rather than a fixed field.
type block struct {
	Object      string `json:"object"`
	ID          string `json:"id"`
	Type        string `json:"type"`
	HasChildren bool   `json:"has_children"`

	// Content is the type-named payload, populated by UnmarshalJSON.
	Content textContent `json:"-"`
	// Children are fetched separately and attached by the caller.
	Children []block `json:"-"`
}

// blockListResponse is the body of GET /v1/blocks/{id}/children.
type blockListResponse struct {
	Results    []block `json:"results"`
	NextCursor string  `json:"next_cursor"`
	HasMore    bool    `json:"has_more"`
}

// page is a Notion page object.
type page struct {
	Object         string                  `json:"object"`
	ID             string                  `json:"id"`
	URL            string                  `json:"url"`
	CreatedTime    string                  `json:"created_time"`
	LastEditedTime string                  `json:"last_edited_time"`
	InTrash        bool                    `json:"in_trash"`
	Properties     map[string]pageProperty `json:"properties"`
	Parent         struct {
		Type   string `json:"type"`
		PageID string `json:"page_id"`
	} `json:"parent"`
}

// pageProperty is one property of a page. Only the title is read: a page
// parented by another page has exactly one, and a page parented by a data
// source has many, of which only the title is worth spending tokens on.
type pageProperty struct {
	Type  string     `json:"type"`
	Title []richText `json:"title"`
}

// searchResponse is the body of POST /v1/search.
type searchResponse struct {
	Results    []page `json:"results"`
	NextCursor string `json:"next_cursor"`
	HasMore    bool   `json:"has_more"`
}

// title returns the page's title, or a fallback when it has none.
//
// An untitled page is real — Notion creates one whenever someone hits the "new
// page" button — and rendering it as an empty string would produce a citation
// with no label.
func (p *page) title() string {
	for _, prop := range p.Properties {
		if prop.Type != "title" {
			continue
		}
		if text := flattenPlain(prop.Title); text != "" {
			return text
		}
	}
	return "(untitled)"
}

// flattenPlain concatenates rich text without formatting, for titles.
func flattenPlain(spans []richText) string {
	var b strings.Builder
	for _, span := range spans {
		b.WriteString(span.PlainText)
	}
	return strings.TrimSpace(b.String())
}

// idPattern matches a Notion object id: a UUID with or without dashes.
//
// Ids are interpolated into request paths, so they are validated rather than
// merely escaped. A strict check also gives the model a precise correction when
// it passes a page *title* where an id belongs, which is the mistake it
// actually makes.
var idPattern = regexp.MustCompile(`^[0-9a-fA-F]{32}$|^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// normalizeID validates a page or block id and renders it in dashed form.
//
// Notion accepts both spellings and returns the dashed one, so normalizing here
// means the dedupe key in the agent loop sees one id rather than two forms of
// the same one.
func normalizeID(id string) (string, error) {
	trimmed := strings.TrimSpace(id)

	// A pasted Notion URL ends in the id, so accept that rather than making the
	// model reverse-engineer it. Order matters: take the last path segment
	// first, then strip a title slug only when what follows the final dash is
	// actually a 32-hex id. Trimming at the last dash unconditionally would
	// reduce a perfectly good dashed UUID to its final group and then reject it.
	if idx := strings.LastIndex(trimmed, "/"); idx >= 0 {
		trimmed = trimmed[idx+1:]
	}
	trimmed = strings.TrimSuffix(trimmed, "?pvs=4")
	if !idPattern.MatchString(trimmed) {
		if idx := strings.LastIndex(trimmed, "-"); idx >= 0 {
			if candidate := trimmed[idx+1:]; idPattern.MatchString(candidate) {
				trimmed = candidate
			}
		}
	}

	if !idPattern.MatchString(trimmed) {
		// Wrapped so the loop does not retry: an invalid id stays invalid.
		return "", fmt.Errorf("%q is not a valid Notion page id; expected a UUID like "+
			"3c34fab1-2b9b-80d6-8cfb-c64d5b8cebe0, which notion_search returns for every result: %w",
			id, tools.ErrInvalidArgument)
	}

	compact := strings.ToLower(strings.ReplaceAll(trimmed, "-", ""))
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		compact[0:8], compact[8:12], compact[12:16], compact[16:20], compact[20:32]), nil
}

// parseNotionTime parses an ISO-8601 timestamp, returning nil when it is absent
// or unparseable.
//
// Nil rather than the zero time, for the same reason as Jira: Evidence.Timestamp
// is rendered in citations, and a missing timestamp should read as unknown
// rather than as the year 1.
func parseNotionTime(value string) *time.Time {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
		if parsed, err := time.Parse(layout, trimmed); err == nil {
			return &parsed
		}
	}
	return nil
}

// formatDate renders a Notion timestamp as a bare date, which is the resolution
// the agent reasons at. Unparseable values fall back to the raw string so
// information is never silently dropped.
func formatDate(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}
	if parsed := parseNotionTime(value); parsed != nil {
		return parsed.Format("2006-01-02")
	}
	return value
}

// snippet shortens text for an EvidenceItem. The cap lives with the evidence
// schema it serves, in package tools.
func snippet(text string) string { return tools.Snippet(text) }
