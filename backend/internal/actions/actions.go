// Package actions owns the lifecycle of a proposed write: its statuses, the
// transitions between them, the key that makes a proposal deduplicable, and the
// registry that resolves an approved action to the code that performs it.
//
// It is a leaf package on purpose. The agent loop proposes, the HTTP handlers
// decide, and a job executes — three packages that must agree exactly on what
// "approved" means and which transitions are legal, without importing each
// other.
//
// The invariant the whole package exists to protect: a write happens at most
// once, and only after a human said so. Both halves of that are enforced by the
// same mechanism — every transition is a conditional UPDATE guarded on the
// expected current status, so the database, not the Go code, decides who wins a
// race. Two concurrent approvals resolve to one; a retried execution job finds
// the row already claimed and sends nothing.
package actions

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"

	"cortex/internal/tools"
)

// The status lifecycle.
//
//	pending -> approved -> executing -> executed | failed
//	pending -> rejected | expired
//
// There is no path back. A failed write is not returned to 'approved' for
// another attempt, because a failure may have landed upstream anyway — a
// timeout on a send is not proof no mail went out — and re-sending on that
// guess is worse than reporting the failure. A rejected proposal is never
// silently retried; the agent is told why and may propose something else.
const (
	// StatusPending is a proposal awaiting a human decision. The only status
	// from which anything else can happen.
	StatusPending = "pending"
	// StatusApproved means a human said yes and an execution job is queued.
	StatusApproved = "approved"
	// StatusRejected means a human said no, with a reason. Terminal.
	StatusRejected = "rejected"
	// StatusExecuting means an execution job has claimed the row and is about
	// to call the upstream system. It is the guard against a second call: the
	// claim happens before the request, so a retry finds nothing to claim.
	StatusExecuting = "executing"
	// StatusExecuted means the write landed and the result is recorded.
	StatusExecuted = "executed"
	// StatusFailed means the upstream call returned an error. Terminal.
	StatusFailed = "failed"
	// StatusExpired means nobody decided within the time limit. Terminal, and
	// it can never execute.
	StatusExpired = "expired"
)

// ExpiredObservation is what the agent is told about a proposal that timed out.
//
// Shared because expiry happens in two places — the hourly sweep and the approve
// endpoint refusing a stale row — and the agent's transcript should not be able
// to tell them apart. From its side they are the same event: the write was never
// carried out, and the answer has to say so.
const ExpiredObservation = "expired: nobody approved or rejected it within the time limit, " +
	"so it was never carried out"

// transitions is the legal-move table, keyed by current status.
//
// It exists so that "the lifecycle is one-way" is a fact something can be
// checked against rather than a property spread across a dozen SQL predicates.
// The conditional UPDATEs are still the enforcement — this is the specification
// they implement, and what a test asserts every illegal move against.
var transitions = map[string][]string{
	StatusPending:   {StatusApproved, StatusRejected, StatusExpired},
	StatusApproved:  {StatusExecuting},
	StatusExecuting: {StatusExecuted, StatusFailed},
	StatusRejected:  nil,
	StatusExecuted:  nil,
	StatusFailed:    nil,
	StatusExpired:   nil,
}

// CanTransition reports whether from -> to is a legal move.
func CanTransition(from, to string) bool {
	for _, allowed := range transitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// Terminal reports whether a status can never change again. Every row is
// immutable once it reaches one.
func Terminal(status string) bool {
	_, known := transitions[status]
	return known && len(transitions[status]) == 0
}

// IdempotencyKey derives the unique key for a proposal.
//
// Derived from the content rather than randomly generated, and that choice is
// what closes a specific hole. The agent-run job has two attempts: if a worker
// is killed mid-loop, River retries it and the run re-investigates from its
// conversation history. A model that proposed an email the first time will
// propose the same email again — and with a random key that would be a second
// pending row, so a human diligently approving both would send two copies.
// Hashing the run, the action and the canonicalized payload makes the second
// proposal collide with the first, and the insert's ON CONFLICT DO NOTHING hands
// back the original row, whatever state it has already reached.
//
// The run id is part of the key on purpose: the same email proposed by a
// genuinely later run is a different intent and deserves its own approval.
func IdempotencyKey(runID uuid.UUID, action string, payload []byte) (string, error) {
	canonical, err := tools.CanonicalJSON(payload)
	if err != nil {
		return "", fmt.Errorf("actions: canonicalize payload for idempotency key: %w", err)
	}
	sum := sha256.Sum256([]byte(runID.String() + "|" + action + "|" + canonical))
	return hex.EncodeToString(sum[:]), nil
}

// Expired reports whether a proposal made at proposedAt has passed its time
// limit. A ttl of zero or less disables expiry.
func Expired(proposedAt time.Time, ttl time.Duration, now time.Time) bool {
	if ttl <= 0 {
		return false
	}
	return now.Sub(proposedAt) >= ttl
}

// Cutoff is the oldest proposed_at a pending action may have and still be
// decidable. It is passed to the approve query so the TTL check and the status
// check are one predicate: checked in a separate SELECT, a row could expire in
// the window between the check and the write.
//
// A ttl of zero or less disables expiry, expressed as the zero time — every
// real proposed_at is after it.
func Cutoff(ttl time.Duration, now time.Time) time.Time {
	if ttl <= 0 {
		return time.Time{}
	}
	return now.Add(-ttl)
}

// Registry resolves an approved action name to the Writer that performs it.
//
// Built per user at execution time, from that user's connections: a Writer holds
// a client carrying their credentials, so it cannot be shared or cached across
// users. An unknown action name is a failed action rather than a panic —
// tool names outlive deployments, and a queued approval for a capability that
// has since been removed must report that clearly instead of crashing a worker.
type Registry struct {
	byAction map[string]tools.Writer
}

// NewRegistry builds a registry from the given writers, rejecting duplicates.
// A duplicate action name would silently shadow one capability with another,
// which is the kind of wiring mistake that should surface here rather than
// halfway through executing someone's approved email.
func NewRegistry(writers ...tools.Writer) (*Registry, error) {
	r := &Registry{byAction: make(map[string]tools.Writer, len(writers))}
	for _, w := range writers {
		action := w.Action()
		if action == "" {
			return nil, fmt.Errorf("actions: a writer has an empty action name")
		}
		if _, exists := r.byAction[action]; exists {
			return nil, fmt.Errorf("actions: duplicate writer for action %q", action)
		}
		r.byAction[action] = w
	}
	return r, nil
}

// Get returns the Writer for an action name.
func (r *Registry) Get(action string) (tools.Writer, bool) {
	w, ok := r.byAction[action]
	return w, ok
}

// Len reports how many writers are registered.
func (r *Registry) Len() int { return len(r.byAction) }
