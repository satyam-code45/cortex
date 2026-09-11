package gmail_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"cortex/internal/tools"
	"cortex/internal/tools/gmail"
)

// The send tool proposes; it never sends.
//
// The sharpest assertion in this file is a count of zero: executing the tool
// must leave the fake Gmail API with no mutating request on it at all. That is
// what "the agent loop cannot cause a side effect" means in practice, and it is
// checkable from outside the package.

const pathSend = "/gmail/v1/users/me/messages/send"

// writeTool builds the send tool against the fake API with the given config.
func writeTool(t *testing.T, f *fakeGmail, cfg gmail.WriteConfig) tools.Tool {
	t.Helper()
	set := gmail.NewWriteTools(f.client(testQueryScope), cfg)
	if len(set) != 1 {
		t.Fatalf("write tool set has %d tools, want 1", len(set))
	}
	if set[0].Name() != "gmail_send_email" {
		t.Fatalf("write tool is named %q, want gmail_send_email", set[0].Name())
	}
	return set[0]
}

// sendWriter builds the executor that performs an approved send.
func sendWriter(t *testing.T, f *fakeGmail, cfg gmail.WriteConfig) tools.Writer {
	t.Helper()
	set := gmail.NewWriters(f.client(testQueryScope), cfg)
	if len(set) != 1 {
		t.Fatalf("writer set has %d writers, want 1", len(set))
	}
	if set[0].Action() != "gmail.send" {
		t.Fatalf("writer performs %q, want gmail.send", set[0].Action())
	}
	return set[0]
}

// decodeProposal unwraps a proposal payload into a map.
func decodeProposal(t *testing.T, result tools.Result) map[string]any {
	t.Helper()
	if result.Proposal == nil {
		t.Fatal("the tool returned no proposal; a write tool must record what it would do")
	}
	var payload map[string]any
	if err := json.Unmarshal(result.Proposal.Payload, &payload); err != nil {
		t.Fatalf("decode proposal payload: %v", err)
	}
	return payload
}

func TestSendEmailProposesWithoutTouchingTheMailbox(t *testing.T) {
	f := newFakeGmail(t, map[string]*route{})
	tool := writeTool(t, f, gmail.WriteConfig{Sender: "satyam@example.com"})

	result, err := tool.Execute(context.Background(), json.RawMessage(`{
		"to": ["ines@vantage.test", "ops@vantage.test"],
		"cc": ["lead@vantage.test"],
		"subject": "Sandbox notice received",
		"body_markdown": "Hi Ines,\n\nWe have the sandbox notice. Certification is in hand.\n\nSatyam"
	}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Nothing was sent, and nothing was even read: this call had no reply to
	// resolve, so the mailbox was not touched at all.
	if got := f.requestCount(); got != 0 {
		t.Errorf("HTTP requests = %d, want 0 — proposing must not touch the mailbox", got)
	}

	if result.Proposal.Source != "gmail" || result.Proposal.Action != "gmail.send" {
		t.Errorf("proposal identifies %s/%s, want gmail/gmail.send",
			result.Proposal.Source, result.Proposal.Action)
	}
	// The observation leaves no room to believe the mail went out.
	for _, want := range []string{"PROPOSED, NOT YET DONE", "Nothing has been sent", "waiting for a person"} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("observation %q does not contain %q", result.Content, want)
		}
	}
	// A proposal is not a claim about the data, so it carries no evidence.
	if len(result.Evidence) != 0 {
		t.Errorf("proposal carries %d evidence items, want 0", len(result.Evidence))
	}

	// The payload is the complete message: every field a person needs to judge
	// it is resolved here, not deferred to execution.
	payload := decodeProposal(t, result)
	if payload["from"] != "satyam@example.com" {
		t.Errorf("from = %v, want the authenticated mailbox", payload["from"])
	}
	// Addresses are normalized rather than echoed, so the check is on the
	// address itself rather than on the exact rendering.
	to, _ := payload["to"].([]any)
	if len(to) != 2 ||
		!strings.Contains(to[0].(string), "ines@vantage.test") ||
		!strings.Contains(to[1].(string), "ops@vantage.test") {
		t.Errorf("to = %v, want both recipients", payload["to"])
	}
	cc, _ := payload["cc"].([]any)
	if len(cc) != 1 || !strings.Contains(cc[0].(string), "lead@vantage.test") {
		t.Errorf("cc = %v, want the carbon copy", payload["cc"])
	}
	if payload["subject"] != "Sandbox notice received" {
		t.Errorf("subject = %v, want the requested subject", payload["subject"])
	}
	if body, _ := payload["body"].(string); !strings.Contains(body, "Certification is in hand") {
		t.Errorf("body = %v, want the drafted message verbatim", payload["body"])
	}
	// The summary names the consequence and the address it will come from.
	for _, want := range []string{"satyam@example.com", "ines@vantage.test", "Sandbox notice received"} {
		if !strings.Contains(result.Proposal.Summary, want) {
			t.Errorf("summary %q does not mention %q", result.Proposal.Summary, want)
		}
	}
}

// Threading is resolved at proposal time, which is a READ. What executes must be
// fully determined by the payload a person approved.
func TestSendEmailResolvesAReplyWithReadsOnly(t *testing.T) {
	const original = `{
		"id": "18f2a3b4c5d6e7f8",
		"threadId": "thread-77",
		"payload": {"headers": [
			{"name": "Message-ID", "value": "<orig@vantage.test>"},
			{"name": "References", "value": "<older@vantage.test>"},
			{"name": "Subject", "value": "Sandbox notice"},
			{"name": "From", "value": "Ines Brandt <ines@vantage.test>"}
		]}
	}`

	f := newFakeGmail(t, map[string]*route{
		pathMessage1: bodyRoute(200, original),
	})
	tool := writeTool(t, f, gmail.WriteConfig{Sender: "satyam@example.com"})

	result, err := tool.Execute(context.Background(), json.RawMessage(`{
		"reply_to_message_id": "18f2a3b4c5d6e7f8",
		"body_markdown": "Received, thank you."
	}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// One read, and no write.
	if got := len(f.requestsTo(pathMessage1)); got != 1 {
		t.Errorf("reads of the message being replied to = %d, want 1", got)
	}
	if got := len(f.requestsTo(pathSend)); got != 0 {
		t.Fatalf("send requests = %d, want 0 — the tool sent the mail instead of proposing it", got)
	}
	for _, req := range f.requestsTo(pathMessage1) {
		if req.method != "GET" {
			t.Errorf("the reply lookup used %s, want GET", req.method)
		}
	}

	payload := decodeProposal(t, result)
	if payload["thread_id"] != "thread-77" {
		t.Errorf("thread_id = %v, want the original thread", payload["thread_id"])
	}
	if payload["in_reply_to"] != "<orig@vantage.test>" {
		t.Errorf("in_reply_to = %v, want the original Message-ID", payload["in_reply_to"])
	}
	if refs, _ := payload["references"].(string); !strings.Contains(refs, "<older@vantage.test>") ||
		!strings.Contains(refs, "<orig@vantage.test>") {
		t.Errorf("references = %v, want the full ancestry", payload["references"])
	}
	if payload["subject"] != "Re: Sandbox notice" {
		t.Errorf("subject = %v, want the inherited subject prefixed with Re:", payload["subject"])
	}
	// The recipient defaults to whoever wrote the message being replied to,
	// rather than being re-derived by the model.
	to, _ := payload["to"].([]any)
	if len(to) != 1 || !strings.Contains(to[0].(string), "ines@vantage.test") {
		t.Errorf("to = %v, want the original sender", payload["to"])
	}
}

// The recipient allowlist is enforced at proposal time, so an out-of-policy
// address is a correctable observation rather than a failure discovered after
// somebody approved the message.
func TestRecipientsOutsideTheAllowlistAreRefusedAtProposalTime(t *testing.T) {
	tests := []struct {
		name    string
		allowed []string
		args    string
		wantErr bool
	}{
		{
			name:    "no allowlist means any domain",
			allowed: nil,
			args:    `{"to":["stranger@elsewhere.test"],"subject":"Hi","body_markdown":"Hello"}`,
		},
		{
			name:    "an allowed domain passes",
			allowed: []string{"vantage.test"},
			args:    `{"to":["ines@vantage.test"],"subject":"Hi","body_markdown":"Hello"}`,
		},
		{
			name:    "a recipient outside the allowlist is refused",
			allowed: []string{"vantage.test"},
			args:    `{"to":["attacker@evil.test"],"subject":"Hi","body_markdown":"Hello"}`,
			wantErr: true,
		},
		{
			name:    "a carbon copy outside the allowlist is refused too",
			allowed: []string{"vantage.test"},
			args:    `{"to":["ines@vantage.test"],"cc":["attacker@evil.test"],"subject":"Hi","body_markdown":"Hello"}`,
			wantErr: true,
		},
		{
			name:    "the allowlist is case-insensitive on the domain",
			allowed: []string{"VANTAGE.test"},
			args:    `{"to":["ines@Vantage.TEST"],"subject":"Hi","body_markdown":"Hello"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGmail(t, map[string]*route{})
			tool := writeTool(t, f, gmail.WriteConfig{
				Sender:         "satyam@vantage.test",
				AllowedDomains: tc.allowed,
			})

			result, err := tool.Execute(context.Background(), json.RawMessage(tc.args))
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("Execute accepted a recipient outside the allowlist (proposal %+v)", result.Proposal)
			case tc.wantErr && !errors.Is(err, tools.ErrInvalidArgument):
				t.Errorf("error = %v, want it marked as a correctable argument error", err)
			case !tc.wantErr && err != nil:
				t.Fatalf("Execute: %v", err)
			}
			if tc.wantErr && result.Proposal != nil {
				t.Error("a refused recipient still produced a proposal")
			}
			if got := f.requestCount(); got != 0 {
				t.Errorf("HTTP requests = %d, want 0", got)
			}
		})
	}
}

func TestSendEmailRefusesAnIncompleteMessage(t *testing.T) {
	tests := []struct {
		name string
		args string
	}{
		{name: "empty body", args: `{"to":["ines@vantage.test"],"subject":"Hi","body_markdown":"   "}`},
		{name: "no recipient", args: `{"subject":"Hi","body_markdown":"Hello"}`},
		{name: "no subject", args: `{"to":["ines@vantage.test"],"body_markdown":"Hello"}`},
		{name: "unparseable address", args: `{"to":["not an address"],"subject":"Hi","body_markdown":"Hello"}`},
		{
			name: "too many recipients to review",
			args: `{"to":["a1@v.test","a2@v.test","a3@v.test","a4@v.test","a5@v.test","a6@v.test",` +
				`"a7@v.test","a8@v.test","a9@v.test","b1@v.test","b2@v.test","b3@v.test","b4@v.test",` +
				`"b5@v.test","b6@v.test","b7@v.test","b8@v.test","b9@v.test","c1@v.test","c2@v.test",` +
				`"c3@v.test","c4@v.test","c5@v.test","c6@v.test","c7@v.test","c8@v.test"],` +
				`"subject":"Hi","body_markdown":"Hello"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGmail(t, map[string]*route{})
			tool := writeTool(t, f, gmail.WriteConfig{Sender: "satyam@vantage.test"})

			result, err := tool.Execute(context.Background(), json.RawMessage(tc.args))
			if err == nil {
				t.Fatalf("Execute accepted an incomplete message (proposal %+v)", result.Proposal)
			}
			if !errors.Is(err, tools.ErrInvalidArgument) {
				t.Errorf("error = %v, want it marked as a correctable argument error", err)
			}
			if got := f.requestCount(); got != 0 {
				t.Errorf("HTTP requests = %d, want 0", got)
			}
		})
	}
}

// The executor is the only thing that sends, and it sends exactly what it was
// handed — the payload a person approved.
func TestTheExecutorSendsTheApprovedPayload(t *testing.T) {
	f := newFakeGmail(t, map[string]*route{
		pathSend: bodyRoute(200, `{"id":"msg-9001","threadId":"thread-77"}`),
	})
	writer := sendWriter(t, f, gmail.WriteConfig{Sender: "satyam@vantage.test"})

	outcome, err := writer.Execute(context.Background(), json.RawMessage(`{
		"from": "satyam@vantage.test",
		"to": ["ines@vantage.test"],
		"subject": "Sandbox notice received",
		"body": "Edited by a human before approving.",
		"thread_id": "thread-77",
		"in_reply_to": "<orig@vantage.test>",
		"references": "<orig@vantage.test>"
	}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	sent := f.requestsTo(pathSend)
	if len(sent) != 1 {
		t.Fatalf("send requests = %d, want exactly 1", len(sent))
	}
	if sent[0].method != "POST" {
		t.Errorf("send used %s, want POST", sent[0].method)
	}

	var body struct {
		Raw      string `json:"raw"`
		ThreadID string `json:"threadId"`
	}
	if err := json.Unmarshal([]byte(sent[0].body), &body); err != nil {
		t.Fatalf("decode send body: %v", err)
	}
	if body.ThreadID != "thread-77" {
		t.Errorf("threadId = %q, want the approved thread", body.ThreadID)
	}
	raw, err := base64.URLEncoding.DecodeString(body.Raw)
	if err != nil {
		t.Fatalf("decode raw message: %v", err)
	}
	mime := string(raw)
	for _, want := range []string{
		"ines@vantage.test",
		"Sandbox notice received",
		"Edited by a human before approving.",
		"<orig@vantage.test>",
	} {
		if !strings.Contains(mime, want) {
			t.Errorf("the sent message does not contain %q:\n%s", want, mime)
		}
	}

	// What came back is recorded so the audit trail can point at the message.
	if outcome.Detail["message_id"] != "msg-9001" {
		t.Errorf("outcome detail = %v, want the new message id", outcome.Detail)
	}
	if !strings.Contains(outcome.Summary, "ines@vantage.test") {
		t.Errorf("outcome summary = %q, want it to name who it went to", outcome.Summary)
	}
}

// A human may have edited the recipients before approving, and an edited payload
// has never been through the proposal-time check — so the policy is enforced
// once more immediately before the send.
func TestTheExecutorReChecksTheAllowlistOnAnEditedPayload(t *testing.T) {
	f := newFakeGmail(t, map[string]*route{
		pathSend: bodyRoute(200, `{"id":"msg-9002","threadId":"t"}`),
	})
	writer := sendWriter(t, f, gmail.WriteConfig{
		Sender:         "satyam@vantage.test",
		AllowedDomains: []string{"vantage.test"},
	})

	_, err := writer.Execute(context.Background(), json.RawMessage(`{
		"from": "satyam@vantage.test",
		"to": ["attacker@evil.test"],
		"subject": "Hi",
		"body": "Hello"
	}`))
	if err == nil {
		t.Fatal("the executor sent to an address outside the allowlist")
	}
	if got := len(f.requestsTo(pathSend)); got != 0 {
		t.Errorf("send requests = %d, want 0", got)
	}
}

// The send tool is marked as proposing, which is how the prompt and the registry
// know a run can propose writes before any tool has been called.
func TestTheSendToolIsMarkedAsProposing(t *testing.T) {
	f := newFakeGmail(t, map[string]*route{})
	tool := writeTool(t, f, gmail.WriteConfig{Sender: "satyam@vantage.test"})

	if _, ok := tool.(tools.Proposing); !ok {
		t.Error("gmail_send_email is not marked as proposing a write")
	}
	registry, err := tools.NewRegistry(append(gmail.NewTools(f.client(testQueryScope)), tool)...)
	if err != nil {
		t.Fatalf("build registry: %v", err)
	}
	names := registry.WriteNames()
	if len(names) != 1 || names[0] != "gmail_send_email" {
		t.Errorf("registry write tools = %v, want [gmail_send_email]", names)
	}
}
