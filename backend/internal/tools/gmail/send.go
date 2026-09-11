package gmail

import (
	"context"
	"encoding/base64"
	"fmt"
	"mime"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"cortex/internal/tools"
)

// Sending mail, as opposed to seeding it.
//
// InsertMessage, next door, puts a message straight into the mailbox with a
// backdated Date header: it is how the demo fixtures get there, and it delivers
// nothing to anybody. This file is the other thing entirely — mail that leaves
// the account and arrives in a stranger's inbox, which is why it is the one
// capability behind both a separate OAuth scope and the human approval gate.

// OutgoingMessage is a message to send.
//
// Every field is resolved before this struct is built: the sender address, the
// threading headers, the recipient list. Nothing here is derived at send time,
// because the payload a human approved is exactly what must go out.
type OutgoingMessage struct {
	// From is the sender, which Gmail will overwrite with the authenticated
	// account anyway; it is set explicitly so the approval card can state whose
	// address the mail will appear to come from.
	From string
	To   []string
	Cc   []string

	Subject string
	Body    string

	// ThreadID places the message in an existing Gmail thread. Empty starts a
	// new one.
	ThreadID string
	// InReplyTo and References carry the RFC 5322 threading headers. Gmail's
	// threadId alone is not enough: it groups the message in the sender's own
	// mailbox, while these headers are what every OTHER mail client in the
	// conversation uses to thread the reply. Sending one without the other
	// produces a reply that looks threaded to us and orphaned to the recipient.
	InReplyTo  string
	References string
}

// sendRequest is the body of POST /gmail/v1/users/me/messages/send.
type sendRequest struct {
	Raw      string `json:"raw"`
	ThreadID string `json:"threadId,omitempty"`
}

// Send delivers a message and returns its new message and thread ids.
//
// It performs no validation beyond what the wire format requires: by the time a
// message reaches here it has been validated at proposal time, shown to a human
// in full, and approved. Re-deriving anything here would mean sending something
// nobody saw.
func (c *Client) Send(ctx context.Context, msg OutgoingMessage) (messageID, threadID string, err error) {
	raw, err := buildOutgoing(msg, time.Now())
	if err != nil {
		return "", "", err
	}

	var sent message
	body := sendRequest{
		Raw:      base64.URLEncoding.EncodeToString([]byte(raw)),
		ThreadID: msg.ThreadID,
	}
	if err := c.postRaw(ctx, "/gmail/v1/users/me/messages/send", nil, body, &sent); err != nil {
		return "", "", fmt.Errorf("send message: %w", err)
	}
	return sent.ID, sent.ThreadID, nil
}

// ReplyContext is what a message being replied to contributes to the reply.
type ReplyContext struct {
	// ThreadID is the thread to send into.
	ThreadID string
	// MessageID is the original's RFC 5322 Message-ID header, which becomes the
	// reply's In-Reply-To.
	MessageID string
	// References is the original's References chain with its Message-ID
	// appended — the full ancestry, which is what long threads need to nest
	// correctly rather than fanning out.
	References string
	// Subject is the original subject, so a reply can inherit it.
	Subject string
	// To is the original's sender, the natural recipient of a reply.
	To string
}

// LookupReplyContext reads the threading facts off a message.
//
// A read, and deliberately called at PROPOSAL time rather than at send time: the
// resolved thread and Message-ID become part of the payload a human approves, so
// what executes is fully determined. Resolving it during execution would mean
// the approved payload and the sent message could differ — if the thread moved,
// or the message was deleted in between — which is precisely the gap the
// approval gate is supposed to close.
func (c *Client) LookupReplyContext(ctx context.Context, messageID string) (*ReplyContext, error) {
	id, err := normalizeMessageID(messageID)
	if err != nil {
		return nil, err
	}

	query := url.Values{}
	query.Set("format", "metadata")
	query["metadataHeaders"] = []string{"Message-ID", "References", "Subject", "From"}

	var msg message
	if err := c.get(ctx, "/gmail/v1/users/me/messages/"+url.PathEscape(id), query, &msg); err != nil {
		return nil, fmt.Errorf("look up message %s: %w", id, err)
	}

	rfcID := msg.header("Message-ID")
	references := strings.TrimSpace(msg.header("References"))
	switch {
	case rfcID == "":
		// Nothing to thread against. Not an error: the reply still sends, it
		// just starts its own chain, and saying so beats refusing.
		references = ""
	case references == "":
		references = rfcID
	default:
		references = references + " " + rfcID
	}

	return &ReplyContext{
		ThreadID:   msg.ThreadID,
		MessageID:  rfcID,
		References: references,
		Subject:    msg.header("Subject"),
		To:         msg.header("From"),
	}, nil
}

// buildOutgoing renders an OutgoingMessage as a raw MIME message.
//
// Separate from BuildRFC5322 rather than a parameter on it. The fixture builder
// takes one To, demands an explicit Date (an undated fixture cannot anchor a
// timeline), and knows nothing about threading; outgoing mail takes recipient
// lists, is always dated now, and lives or dies on its threading headers. Fusing
// the two would produce a function whose every argument is conditional.
func buildOutgoing(msg OutgoingMessage, now time.Time) (string, error) {
	to := joinAddresses(msg.To)
	if to == "" {
		return "", fmt.Errorf("gmail: a message needs at least one recipient: %w", tools.ErrInvalidArgument)
	}
	if strings.TrimSpace(msg.From) == "" {
		return "", fmt.Errorf("gmail: a message needs a sender: %w", tools.ErrInvalidArgument)
	}

	body := strings.ReplaceAll(msg.Body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\n", "\r\n")

	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", encodeAddress(msg.From))
	fmt.Fprintf(&b, "To: %s\r\n", to)
	if cc := joinAddresses(msg.Cc); cc != "" {
		fmt.Fprintf(&b, "Cc: %s\r\n", cc)
	}
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", msg.Subject))
	fmt.Fprintf(&b, "Date: %s\r\n", now.Format(time.RFC1123Z))
	if inReplyTo := strings.TrimSpace(msg.InReplyTo); inReplyTo != "" {
		fmt.Fprintf(&b, "In-Reply-To: %s\r\n", inReplyTo)
	}
	if references := strings.TrimSpace(msg.References); references != "" {
		fmt.Fprintf(&b, "References: %s\r\n", references)
	}
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

// joinAddresses renders an address list as a MIME header value, encoding each
// display name and dropping blanks.
func joinAddresses(addresses []string) string {
	encoded := make([]string, 0, len(addresses))
	for _, address := range addresses {
		if trimmed := strings.TrimSpace(address); trimmed != "" {
			encoded = append(encoded, encodeAddress(trimmed))
		}
	}
	return strings.Join(encoded, ", ")
}

// ParseRecipients validates and normalizes a recipient list.
//
// Called at proposal time, so a malformed address is a correctable observation
// the model can fix on its next iteration rather than a failure discovered after
// a human already approved the message.
func ParseRecipients(field string, addresses []string) ([]string, error) {
	out := make([]string, 0, len(addresses))
	for _, address := range addresses {
		trimmed := strings.TrimSpace(address)
		if trimmed == "" {
			continue
		}
		parsed, err := mail.ParseAddress(trimmed)
		if err != nil {
			return nil, fmt.Errorf("gmail: %s contains %q, which is not a valid email address: %w",
				field, trimmed, tools.ErrInvalidArgument)
		}
		out = append(out, parsed.String())
	}
	return out, nil
}

// AddressDomain returns the lowercased domain of an address, for the send
// allowlist check.
func AddressDomain(address string) (string, error) {
	parsed, err := mail.ParseAddress(strings.TrimSpace(address))
	if err != nil {
		return "", fmt.Errorf("gmail: %q is not a valid email address: %w", address, tools.ErrInvalidArgument)
	}
	at := strings.LastIndex(parsed.Address, "@")
	if at < 0 || at == len(parsed.Address)-1 {
		return "", fmt.Errorf("gmail: %q has no domain: %w", address, tools.ErrInvalidArgument)
	}
	return strings.ToLower(parsed.Address[at+1:]), nil
}
