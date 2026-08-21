package gmail

import (
	"fmt"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cortex/internal/tools"
)

// Gmail REST response shapes, narrowed to the fields Cortex reads.

// messageHeader is one RFC 5322 header.
type messageHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// messageBody is a part's inline content.
type messageBody struct {
	Size int    `json:"size"`
	Data string `json:"data"`
	// AttachmentID is set instead of Data when the content is large enough that
	// Gmail requires a separate fetch. Cortex never follows it: an attachment
	// is not the message.
	AttachmentID string `json:"attachmentId"`
}

// messagePart is one node of the MIME tree.
type messagePart struct {
	PartID   string          `json:"partId"`
	MimeType string          `json:"mimeType"`
	Filename string          `json:"filename"`
	Headers  []messageHeader `json:"headers"`
	Body     *messageBody    `json:"body"`
	Parts    []messagePart   `json:"parts"`
}

// message is one Gmail message.
type message struct {
	ID       string   `json:"id"`
	ThreadID string   `json:"threadId"`
	LabelIDs []string `json:"labelIds"`
	Snippet  string   `json:"snippet"`
	// InternalDate is Gmail's own receipt timestamp, in milliseconds since the
	// epoch, as a JSON string.
	InternalDate string       `json:"internalDate"`
	Payload      *messagePart `json:"payload"`
}

// messageRef is the id-only shape the list endpoint returns.
type messageRef struct {
	ID       string `json:"id"`
	ThreadID string `json:"threadId"`
}

// listResponse is the body of GET /gmail/v1/users/me/messages.
type listResponse struct {
	Messages           []messageRef `json:"messages"`
	NextPageToken      string       `json:"nextPageToken"`
	ResultSizeEstimate int          `json:"resultSizeEstimate"`
}

// labelResponse is one label from the labels endpoints.
type labelResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// labelListResponse is the body of GET /gmail/v1/users/me/labels.
type labelListResponse struct {
	Labels []labelResponse `json:"labels"`
}

// header reads one of the message's top-level headers.
func (m *message) header(name string) string {
	if m.Payload == nil {
		return ""
	}
	return headerValue(m.Payload.Headers, name)
}

// subject returns the message subject, or a stand-in when it has none.
//
// An empty subject is real and would otherwise render as a citation with no
// label.
func (m *message) subject() string {
	if s := m.header("Subject"); s != "" {
		return s
	}
	return "(no subject)"
}

// sentAt returns when the message was received, preferring Gmail's own
// internalDate over the Date header.
//
// internalDate is the trustworthy one: the Date header is written by the
// sender's client and can be wrong, absent, or in a zone nobody expected —
// and for the seeded fixtures the two agree by construction, because they are
// inserted with internalDateSource=dateHeader.
func (m *message) sentAt() *time.Time {
	if ms, err := strconv.ParseInt(strings.TrimSpace(m.InternalDate), 10, 64); err == nil && ms > 0 {
		when := time.UnixMilli(ms).UTC()
		return &when
	}
	if parsed := parseMailDate(m.header("Date")); parsed != nil {
		return parsed
	}
	return nil
}

// parseMailDate parses a Date header, returning nil when it is absent or
// unparseable.
//
// net/mail implements the RFC 5322 date grammar in full — the optional
// day-of-week, obsolete alphabetic zones, and trailing comments like
// "-0700 (CEST)" are all legal and all appear in real mail. A hand-rolled list
// of layouts gets the common four right and silently returns "unknown" for the
// rest.
func parseMailDate(value string) *time.Time {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	parsed, err := mail.ParseDate(trimmed)
	if err != nil {
		return nil
	}
	return &parsed
}

// formatDate renders a timestamp as a bare date, the resolution the agent
// reasons at.
func formatDate(when *time.Time) string {
	if when == nil {
		return "unknown"
	}
	return when.Format("2006-01-02")
}

// messageIDPattern matches a Gmail message id: lowercase hex, as Gmail issues.
//
// Ids are interpolated into request paths, so they are validated rather than
// merely escaped. The strict check also gives the model a precise correction
// when it passes a subject line where an id belongs.
var messageIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{1,32}$`)

// normalizeMessageID validates a message id.
func normalizeMessageID(id string) (string, error) {
	trimmed := strings.TrimSpace(id)
	if !messageIDPattern.MatchString(trimmed) {
		// Wrapped so the loop does not retry: an invalid id stays invalid.
		return "", fmt.Errorf("%q is not a valid Gmail message id; expected the hexadecimal id "+
			"gmail_search returns for every result, e.g. 18f2a3b4c5d6e7f8: %w", id, tools.ErrInvalidArgument)
	}
	return trimmed, nil
}

// webURL is the human-facing URL for a message. Evidence carries this so a
// citation can be clicked through to the original.
func webURL(messageID string) string {
	return "https://mail.google.com/mail/u/0/#all/" + messageID
}

// snippet shortens text for an EvidenceItem. The cap lives with the evidence
// schema it serves, in package tools.
func snippet(text string) string { return tools.Snippet(text) }
