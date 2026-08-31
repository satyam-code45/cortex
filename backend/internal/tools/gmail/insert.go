package gmail

import (
	"context"
	"encoding/base64"
	"fmt"
	"mime"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Message insertion, used only by the seeder.
//
// Fixtures are *inserted*, not sent. Sending would stamp every message with the
// moment the seeder ran, which destroys the only thing that makes them useful:
// the vendor's delay notice has to sit in June, before the launch-date decision
// it caused, or a question about what happened when has no answer to find.
// messages.insert with internalDateSource=dateHeader honours the Date header we
// write, so the timeline is real.

// Gmail's system label ids. They are literal strings in the API, not opaque
// ids that have to be looked up the way a user label does.
const (
	// LabelInbox is what makes a message reachable by an ordinary search.
	//
	// This is not cosmetic. A message inserted with only a custom label lands in
	// the mailbox but outside the scope a normal query reaches: searching
	// "Nordwind" finds nothing while `in:anywhere Nordwind` finds everything.
	// The agent has no way to phrase its way out of that, so a fixture without
	// INBOX is a fixture the agent cannot investigate — which is exactly how
	// the cross-source acceptance test was once blocked.
	LabelInbox = "INBOX"
	// LabelUnread makes a seeded fixture read like mail that actually arrived.
	LabelUnread = "UNREAD"
	// LabelTrash and LabelSpam mark a message the operator has thrown away. A
	// fixture in either is treated as absent, so deleting the fixtures and
	// re-seeding is a repair path rather than a no-op.
	LabelTrash = "TRASH"
	LabelSpam  = "SPAM"
)

// FixtureLabelIDs returns the labels every seeded fixture must carry.
//
// INBOX first and unconditionally: the fixture label is how a graded run is
// scoped, but INBOX is what makes the message exist as far as search is
// concerned. Passing the fixture label alone is the bug this function exists to
// make impossible.
func FixtureLabelIDs(fixtureLabelID string) []string {
	ids := []string{LabelInbox, LabelUnread}
	if strings.TrimSpace(fixtureLabelID) != "" {
		ids = append(ids, fixtureLabelID)
	}
	return ids
}

// FixtureMessage is one seeded email.
type FixtureMessage struct {
	From    string
	To      string
	Cc      string
	Subject string
	Date    time.Time
	Body    string
}

// insertRequest is the body of POST /gmail/v1/users/me/messages.
type insertRequest struct {
	Raw      string   `json:"raw"`
	LabelIDs []string `json:"labelIds,omitempty"`
}

// InsertMessage places a message directly in the mailbox, dated by its own Date
// header, and returns the new message id.
func (c *Client) InsertMessage(ctx context.Context, msg FixtureMessage, labelIDs []string) (string, error) {
	raw, err := BuildRFC5322(msg)
	if err != nil {
		return "", err
	}

	query := url.Values{}
	// Without this Gmail dates the message "now" and files it at the top of the
	// mailbox regardless of what its Date header says.
	query.Set("internalDateSource", "dateHeader")

	body := insertRequest{
		Raw:      base64.URLEncoding.EncodeToString([]byte(raw)),
		LabelIDs: labelIDs,
	}

	var inserted message
	if err := c.postRaw(ctx, "/gmail/v1/users/me/messages", query, body, &inserted); err != nil {
		return "", err
	}
	return inserted.ID, nil
}

// BuildRFC5322 renders a fixture as a raw MIME message.
//
// Deliberately a single text/plain part rather than a multipart/alternative:
// the fixtures exist to be read, and a plain part is what the extraction path
// treats as authoritative. The transfer encoding is chosen from the content —
// 7bit when everything is ASCII, base64 otherwise — because declaring 7bit over
// bytes that are not is how a message arrives with its accents mangled.
func BuildRFC5322(msg FixtureMessage) (string, error) {
	if strings.TrimSpace(msg.From) == "" || strings.TrimSpace(msg.To) == "" {
		return "", fmt.Errorf("gmail: fixture %q needs both From and To", msg.Subject)
	}
	date := msg.Date
	if date.IsZero() {
		return "", fmt.Errorf("gmail: fixture %q has no date; an undated fixture cannot anchor a timeline", msg.Subject)
	}

	body := strings.ReplaceAll(msg.Body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\n", "\r\n")

	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", encodeAddress(msg.From))
	fmt.Fprintf(&b, "To: %s\r\n", encodeAddress(msg.To))
	if strings.TrimSpace(msg.Cc) != "" {
		fmt.Fprintf(&b, "Cc: %s\r\n", encodeAddress(msg.Cc))
	}
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", msg.Subject))
	fmt.Fprintf(&b, "Date: %s\r\n", date.Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=\"UTF-8\"\r\n")

	if isASCII(body) {
		b.WriteString("Content-Transfer-Encoding: 7bit\r\n\r\n")
		b.WriteString(body)
	} else {
		b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
		encoded := base64.StdEncoding.EncodeToString([]byte(body))
		for start := 0; start < len(encoded); start += 76 {
			b.WriteString(encoded[start:min(start+76, len(encoded))])
			b.WriteString("\r\n")
		}
	}
	return b.String(), nil
}

// encodeAddress RFC 2047-encodes the display-name part of an address, leaving
// the addr-spec alone — an encoded-word inside angle brackets is not a valid
// address and Gmail rejects the whole message.
func encodeAddress(address string) string {
	trimmed := strings.TrimSpace(address)
	open := strings.LastIndex(trimmed, "<")
	if open <= 0 || !strings.HasSuffix(trimmed, ">") {
		return trimmed
	}
	name := strings.Trim(strings.TrimSpace(trimmed[:open]), `"`)
	if name == "" || isASCII(name) {
		return trimmed
	}
	return mime.QEncoding.Encode("utf-8", name) + " " + trimmed[open:]
}

// isASCII reports whether every byte is 7-bit.
func isASCII(text string) bool {
	for i := range len(text) {
		if text[i] > 127 {
			return false
		}
	}
	return true
}

// EnsureLabel returns the id of the named label, creating it when absent.
//
// The label is what GMAIL_QUERY_SCOPE narrows to, so a graded run can be
// confined to seeded fixtures without the tools losing the ability to search
// the whole mailbox by default.
func (c *Client) EnsureLabel(ctx context.Context, name string) (string, error) {
	var list labelListResponse
	if err := c.get(ctx, "/gmail/v1/users/me/labels", nil, &list); err != nil {
		return "", err
	}
	for _, label := range list.Labels {
		if strings.EqualFold(label.Name, name) {
			return label.ID, nil
		}
	}

	body := map[string]string{
		"name":                  name,
		"labelListVisibility":   "labelShow",
		"messageListVisibility": "show",
	}
	var created labelResponse
	if err := c.postRaw(ctx, "/gmail/v1/users/me/labels", nil, body, &created); err != nil {
		return "", fmt.Errorf("create label %q: %w", name, err)
	}
	return created.ID, nil
}

// SeededMessage is what FindBySubject reports about an already-present fixture.
type SeededMessage struct {
	// ID is the Gmail message id.
	ID string
	// InInbox reports whether the message carries the INBOX label, and so
	// whether an ordinary search can reach it at all.
	InInbox bool
}

// FindBySubject reports whether a message with this exact subject already
// exists, so a re-run of the seeder converges instead of inserting duplicates.
//
// Gmail has no natural key for an inserted message, and insert is not
// idempotent: without this check, running the seeder twice produces two copies
// of every fixture and an agent that cites whichever it happened to read.
//
// The lookup deliberately uses in:anywhere: a fixture seeded before the INBOX
// fix is invisible to a default query, and a duplicate check that cannot see it
// would insert a second copy on every run.
//
// Trashed and spammed messages are then filtered back out. Emptying the Bin is
// not instant — Gmail keeps a deleted message for 30 days — so without this,
// "delete the fixtures and re-seed" does nothing at all: every message is still
// found, every insert is skipped, and the operator is told to delete messages
// they have already deleted.
func (c *Client) FindBySubject(ctx context.Context, subject string) (*SeededMessage, bool, error) {
	// Gmail has no escape syntax inside a quoted phrase, so %q — which would
	// emit backslashes — silently matches nothing. Stripping the quotes is
	// lossless here: the subject is read back and compared exactly below.
	scoped, err := c.scopedQuery(`in:anywhere subject:"` + strings.ReplaceAll(subject, `"`, " ") + `"`)
	if err != nil {
		return nil, false, err
	}
	query := url.Values{}
	query.Set("q", scoped)
	query.Set("maxResults", "5")
	query.Set("includeSpamTrash", "true")

	var list listResponse
	if err := c.get(ctx, "/gmail/v1/users/me/messages", query, &list); err != nil {
		return nil, false, err
	}
	// Gmail's subject: operator matches on words rather than the exact string,
	// so a hit is confirmed by reading the subject back.
	for _, ref := range list.Messages {
		msg, err := c.getMessage(ctx, ref.ID, "metadata")
		if err != nil {
			return nil, false, err
		}
		if !strings.EqualFold(strings.TrimSpace(msg.subject()), strings.TrimSpace(subject)) {
			continue
		}
		if slices.Contains(msg.LabelIDs, LabelTrash) || slices.Contains(msg.LabelIDs, LabelSpam) {
			// Thrown away: treat it as gone and let the caller re-insert.
			continue
		}
		{
			return &SeededMessage{
				ID:      msg.ID,
				InInbox: slices.Contains(msg.LabelIDs, LabelInbox),
			}, true, nil
		}
	}
	return nil, false, nil
}

// profileResponse is the body of GET /gmail/v1/users/me/profile.
type profileResponse struct {
	EmailAddress string `json:"emailAddress"`
}

// UserEmail returns the address of the authenticated mailbox.
//
// The seeder substitutes it for the {{me}} placeholder in the committed
// fixtures, which is what keeps a real address out of the repository while
// still producing messages actually addressed to whoever runs the seed.
func (c *Client) UserEmail(ctx context.Context) (string, error) {
	var profile profileResponse
	if err := c.get(ctx, "/gmail/v1/users/me/profile", nil, &profile); err != nil {
		return "", err
	}
	if profile.EmailAddress == "" {
		return "", fmt.Errorf("gmail: the profile endpoint returned no address")
	}
	return profile.EmailAddress, nil
}

// CountMatching reports how many messages a query returns, without fetching any
// of them.
//
// The seeder uses it to verify the invariant that actually matters: a fixture
// must be findable by the kind of query the agent will type. Asserting on the
// INBOX label instead is asserting on a proxy — and a proxy that, as it turned
// out, can be false while search works perfectly well.
func (c *Client) CountMatching(ctx context.Context, query string) (int, error) {
	scoped, err := c.scopedQuery(query)
	if err != nil {
		return 0, err
	}
	q := url.Values{}
	q.Set("q", scoped)
	q.Set("maxResults", "50")

	var list listResponse
	if err := c.get(ctx, "/gmail/v1/users/me/messages", q, &list); err != nil {
		return 0, err
	}
	return len(list.Messages), nil
}
