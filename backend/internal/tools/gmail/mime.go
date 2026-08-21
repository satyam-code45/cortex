package gmail

import (
	"encoding/base64"
	"html"
	"io"
	"mime/quotedprintable"
	"regexp"
	"strings"
)

// MIME extraction.
//
// A modern email is a tree: multipart/mixed wrapping multipart/alternative
// wrapping a text/plain and a text/html saying the same thing, plus
// attachments. The model needs exactly one of those — the plain-text one — and
// handing it the HTML alternative instead means paying for markup and risking
// the agent quoting a stylesheet as evidence.
//
// So the rule is: prefer text/plain anywhere in the tree, fall back to
// text/html with its tags stripped, and never return an attachment's bytes.

// maxBodyRunes caps an extracted body. A mailing-list digest or a long reply
// chain can run to tens of thousands of characters, and the whole thing would
// be carried in the agent's context on every later iteration.
const maxBodyRunes = 8000

// headerValue reads a header by name, case-insensitively.
func headerValue(headers []messageHeader, name string) string {
	for _, h := range headers {
		if strings.EqualFold(h.Name, name) {
			return strings.TrimSpace(h.Value)
		}
	}
	return ""
}

// extractBody walks a message payload and returns its best plain-text body,
// plus whether the text had to be derived from HTML.
//
// The HTML flag is surfaced rather than hidden: stripped HTML is a lossy
// rendering, and a caller that knows it is reading one can say so instead of
// presenting it as the author's words verbatim.
func extractBody(payload *messagePart) (body string, fromHTML bool) {
	if payload == nil {
		return "", false
	}
	if plain := findPart(payload, "text/plain"); plain != "" {
		return truncateRunes(plain, maxBodyRunes), false
	}
	if htmlBody := findPart(payload, "text/html"); htmlBody != "" {
		return truncateRunes(stripHTML(htmlBody), maxBodyRunes), true
	}
	return "", false
}

// findPart searches the part tree depth-first for the first part of the given
// MIME type that carries inline content.
func findPart(part *messagePart, mimeType string) string {
	if part == nil {
		return ""
	}
	// An attachment is skipped even when it is text/plain: a .txt attachment is
	// not the message body, and returning it would misattribute its contents to
	// the sender's message.
	if part.Filename == "" && strings.HasPrefix(strings.ToLower(part.MimeType), mimeType) {
		if decoded := decodePartBody(part); strings.TrimSpace(decoded) != "" {
			return decoded
		}
	}
	for i := range part.Parts {
		if found := findPart(&part.Parts[i], mimeType); found != "" {
			return found
		}
	}
	return ""
}

// decodePartBody decodes one part's body data.
func decodePartBody(part *messagePart) string {
	if part.Body == nil || part.Body.Data == "" {
		return ""
	}
	raw, err := base64.URLEncoding.WithPadding(base64.NoPadding).
		DecodeString(strings.TrimRight(part.Body.Data, "="))
	if err != nil {
		// Some clients emit standard base64 with +/ instead of -_.
		raw, err = base64.StdEncoding.DecodeString(part.Body.Data)
		if err != nil {
			return ""
		}
	}
	return maybeDecodeQuotedPrintable(string(raw), headerValue(part.Headers, "Content-Transfer-Encoding"))
}

// softLineBreak matches a quoted-printable soft line break, which is the one
// artifact that ordinary decoded text effectively never contains.
var softLineBreak = regexp.MustCompile(`=\r?\n`)

// maybeDecodeQuotedPrintable undoes quoted-printable encoding, but only when
// the content still looks encoded.
//
// The Gmail API normally decodes the transfer encoding for us and hands back
// plain bytes, while still reporting the part's original
// Content-Transfer-Encoding. Decoding on the header alone would therefore
// corrupt any message legitimately containing "=" followed by two hex digits —
// a URL with %-escapes rewritten, say. Requiring a soft line break as
// corroboration makes a false positive essentially impossible, and the cost of
// a false negative is only that a rare already-decoded body keeps a stray "=3D".
func maybeDecodeQuotedPrintable(text, encoding string) string {
	if !strings.EqualFold(strings.TrimSpace(encoding), "quoted-printable") {
		return text
	}
	if !softLineBreak.MatchString(text) {
		return text
	}
	decoded, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(text)))
	if err != nil {
		return text
	}
	return string(decoded)
}

var (
	// scriptOrStyle matches blocks whose contents are code, not prose.
	scriptOrStyle = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	// blockBreak matches tags that end a visual line.
	blockBreak = regexp.MustCompile(`(?i)<(br|/p|/div|/tr|/h[1-6]|/li)\s*/?>`)
	// anyTag matches whatever markup is left.
	anyTag = regexp.MustCompile(`(?s)<[^>]*>`)
	// manyBlankLines matches three or more consecutive newlines.
	manyBlankLines = regexp.MustCompile(`\n{3,}`)
)

// stripHTML renders an HTML body as rough plain text.
//
// This is deliberately not an HTML parser. The output is only ever read by a
// model as prose, so structural fidelity buys nothing — what matters is that
// the words survive, the markup does not, and entities come back as characters
// rather than as "&amp;nbsp;" noise the model might quote.
func stripHTML(input string) string {
	text := scriptOrStyle.ReplaceAllString(input, " ")
	text = blockBreak.ReplaceAllString(text, "\n")
	text = anyTag.ReplaceAllString(text, "")
	text = html.UnescapeString(text)
	// Non-breaking spaces survive unescaping as U+00A0 and read as ordinary
	// spaces to a human but not to a tokenizer.
	text = strings.ReplaceAll(text, " ", " ")

	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(strings.Join(strings.Fields(line), " "))
	}
	return strings.TrimSpace(manyBlankLines.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

// truncateRunes caps text at n runes, marking that it was cut.
//
// The marker matters: without it the model reads a truncated thread as a
// complete one and can conclude that something was never replied to.
func truncateRunes(text string, n int) string {
	trimmed := strings.TrimSpace(text)
	runes := []rune(trimmed)
	if len(runes) <= n {
		return trimmed
	}
	return strings.TrimSpace(string(runes[:n])) + "\n\n[message truncated]"
}
