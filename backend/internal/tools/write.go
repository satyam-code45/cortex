package tools

import (
	"context"
	"encoding/json"
)

// The write half of the tool contract.
//
// A read tool answers a question; a write tool changes something in a system
// someone else depends on, and the two cannot share an execution model. The
// asymmetry is deliberate and it is the point of this file:
//
//   - A read tool's Execute performs the read and returns what it found.
//   - A write tool's Execute performs NOTHING. It validates the request and
//     returns a Proposal — the exact call it would make — which the agent loop
//     records and pauses on. A human decides. Only then does a separate job
//     call the matching Writer.
//
// The reason is not caution for its own sake. Everything the agent reads is
// text someone else wrote: anyone who can file a ticket or email the mailbox
// can put instructions in the transcript. The untrusted-content fence lets the
// model notice that; it cannot stop a model that has been talked into acting.
// Splitting "decide to write" from "write" puts a human between them, so a
// comment on a ticket can never become an email sent from the account owner's
// address.

// Proposal is a write a tool wants performed, returned instead of performing it.
//
// Payload is the complete, final request — not a sketch to be filled in later.
// A human approves this exact JSON (possibly after editing it), and that is what
// executes, so anything left to be decided downstream would be something nobody
// approved.
type Proposal struct {
	// Source is the system the write targets: "gmail", "jira" or "notion".
	Source string
	// Action names the capability, e.g. "gmail.send", "jira.create_issue". It is
	// the key that resolves to a Writer at execution time.
	Action string
	// Payload is the exact request to perform, as JSON.
	Payload json.RawMessage
	// Summary is one line of plain English naming the consequence, e.g.
	// "send an email to ines@example.com". It is what the audit list shows and
	// what the approval card headlines; the payload is still rendered in full
	// beside it, because approving a summary is not approving the write.
	Summary string
}

// Proposing marks a Tool whose Execute proposes a write instead of performing
// one.
//
// A marker method rather than an inferred property, because two things need to
// know the answer before any tool has been called. The system prompt must tell
// the model whether it can propose writes at all — a model told its tools are
// read-only while holding a send-email tool is being lied to, and one told the
// opposite while holding none will offer to do things it cannot. And the
// registry must be able to report, for a test and for the trace, that a run with
// writes disabled contains no write tool whatsoever.
type Proposing interface {
	Tool
	// ProposesWrite is a marker. It exists to be implemented, not called.
	ProposesWrite()
}

// WriteOutcome is what performing an approved action produced.
type WriteOutcome struct {
	// Summary is what the agent is told when its run resumes, e.g.
	// "created ATLAS-101". It goes into the transcript, so it reads as prose.
	Summary string
	// Detail is stored on the action row and rendered in the audit view: the
	// message id, issue key or page id that proves the write landed and lets a
	// reader go and look at it.
	Detail map[string]any
}

// Writer performs one approved action against its upstream system.
//
// It is deliberately separate from Tool. The Tool is what the model can call and
// is registered per run; the Writer is what the execution job calls after a human
// approves, and it must exist even when no agent run is in flight. Keeping them
// apart is also what makes "the loop never performs the side effect" a
// structural fact rather than a convention: the agent loop is handed a registry
// of Tools and never sees a Writer at all.
type Writer interface {
	// Action is the proposal action name this Writer performs.
	Action() string
	// Execute performs the write. payload is the human-approved final payload,
	// which may differ from what the agent proposed.
	Execute(ctx context.Context, payload json.RawMessage) (WriteOutcome, error)
}
