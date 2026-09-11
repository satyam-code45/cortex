package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"cortex/internal/actions"
	"cortex/internal/agent"
	"cortex/internal/auth"
	"cortex/internal/store"
	"cortex/internal/tools"
)

// The approval endpoints: the human half of the write gate.
//
// Three handlers, and between them they are the only way a write ever happens.
// Two properties are load-bearing:
//
//   - Ownership is enforced in SQL, by a predicate on the row, not by a check in
//     Go. Every one of these takes a caller-supplied UUID, and an action row is
//     permission to send mail from somebody's account; a forgotten Go-side check
//     would be an access-control hole with a very direct consequence.
//   - Approving does not perform the write. It records the decision and queues a
//     job, inside one transaction. A handler that sent the email inline would
//     send a second one on every retried or double-clicked request, and there
//     would be nothing to compare-and-set against.
//
// The audit list is read-only by design. An executed write cannot be undone from
// here or anywhere else — the record of what happened is the recourse, which is
// exactly why the record is complete.

// maxActionListLimit caps the audit list.
const maxActionListLimit = 200

// defaultActionListLimit is the audit list's page size when none is asked for.
const defaultActionListLimit = 50

// actionResponse is one action as the API reports it.
//
// Both payloads are sent in full, and that is the point of the whole feature:
// the approval card renders the proposal field by field, and the audit view shows
// what was proposed beside what was approved so a reader can see whether a
// person changed it. Summarizing either would defeat the purpose.
type actionResponse struct {
	ID              uuid.UUID       `json:"id"`
	AgentRunID      uuid.UUID       `json:"agent_run_id"`
	Source          string          `json:"source"`
	Action          string          `json:"action"`
	Status          string          `json:"status"`
	ProposedPayload json.RawMessage `json:"proposed_payload"`
	FinalPayload    json.RawMessage `json:"final_payload,omitempty"`
	RejectReason    string          `json:"reject_reason,omitempty"`
	Result          json.RawMessage `json:"result,omitempty"`
	Error           string          `json:"error,omitempty"`
	ProposedAt      time.Time       `json:"proposed_at"`
	DecidedAt       *time.Time      `json:"decided_at,omitempty"`
	ExecutedAt      *time.Time      `json:"executed_at,omitempty"`
	DecidedBy       *uuid.UUID      `json:"decided_by,omitempty"`
	// Edited is computed rather than stored, so the UI never has to diff two
	// JSON documents to answer the question an auditor asks first.
	Edited bool `json:"edited"`
	// Editable tells the UI whether this row can still be acted on, so the
	// decision of what to render is made once, here, against the same TTL the
	// approve query enforces — instead of the browser guessing from a timestamp.
	Editable bool `json:"editable"`
}

// handleListActions serves GET /api/actions — the audit view.
func (s *Server) handleListActions(w http.ResponseWriter, r *http.Request) {
	logger := s.deps.Logger
	user, ok := auth.UserFrom(r.Context())
	if !ok {
		writeError(w, logger, http.StatusInternalServerError, "internal error")
		return
	}

	limit := defaultActionListLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeError(w, logger, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = min(parsed, maxActionListLimit)
	}

	rows, err := store.New(s.deps.DB).ListAgentActionsForUser(r.Context(), store.ListAgentActionsForUserParams{
		UserID: user.ID,
		Limit:  int32(limit),
	})
	if err != nil {
		logger.Error("actions: list", "error", err)
		writeError(w, logger, http.StatusInternalServerError, "internal error")
		return
	}

	out := make([]actionResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, s.actionResponse(row))
	}
	writeJSON(w, logger, http.StatusOK, map[string]any{"actions": out})
}

// approveRequest is the body of POST /api/actions/{id}/approve.
type approveRequest struct {
	// FinalPayload replaces the proposal. Absent means "approve exactly what was
	// proposed", which is the common case and must not require the client to
	// echo a payload back.
	FinalPayload json.RawMessage `json:"final_payload"`
}

// handleApproveAction serves POST /api/actions/{id}/approve.
func (s *Server) handleApproveAction(w http.ResponseWriter, r *http.Request) {
	logger := s.deps.Logger
	user, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	// An empty body is legitimate here ("approve exactly what was proposed"),
	// so the content type is required only when something was actually sent.
	// Every other mutating endpoint guards this; the endpoint that sends email
	// should not be the exception.
	if r.Header.Get("Content-Type") != "" && !hasJSONContentType(r) {
		writeError(w, logger, http.StatusUnsupportedMediaType, "expected application/json")
		return
	}
	actionID, ok := s.actionID(w, r)
	if !ok {
		return
	}

	// An empty body is the ordinary case — "approve exactly what was proposed" —
	// so io.EOF is not an error here. Decoding is attempted unconditionally
	// rather than gated on ContentLength, which is -1 for a chunked request and
	// would silently discard an edited payload.
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	var req approveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, logger, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// The proposal is read first so the edited payload can be validated against
	// it and the ownership check happens before anything else. Reading and then
	// writing is not a race: the approve UPDATE re-checks the status in its own
	// predicate, so a decision landing in between makes this one lose cleanly.
	existing, err := store.New(s.deps.DB).GetAgentActionForUser(r.Context(), store.GetAgentActionForUserParams{
		ID:     actionID,
		UserID: user.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Deliberately the same answer as a row belonging to somebody else: an
		// existing-but-not-yours action must not be distinguishable from one
		// that never existed.
		writeError(w, logger, http.StatusNotFound, "no such action")
		return
	}
	if err != nil {
		logger.Error("actions: load for approve", "error", err)
		writeError(w, logger, http.StatusInternalServerError, "internal error")
		return
	}

	final := existing.ProposedPayload
	if len(req.FinalPayload) > 0 {
		// A JSON object specifically, not merely valid JSON. Every write payload
		// is an object, and accepting a bare number or string here would store
		// something that only fails to decode later — after a person has already
		// approved it, which is the worst moment to discover it.
		if !isJSONObject(req.FinalPayload) {
			writeError(w, logger, http.StatusBadRequest, "final_payload must be a JSON object")
			return
		}
		final = req.FinalPayload
	}

	// The transaction is managed here rather than through store.WithTx because
	// the enqueue needs the pgx.Tx itself: the job row has to be written inside
	// this transaction, so the write becomes executable only when the approval
	// commits. Same shape as the chat handler's atomic enqueue.
	decided, err := s.approveInTx(r, actionID, user.ID, final)
	if errors.Is(err, pgx.ErrNoRows) {
		s.reportUndecidable(r, w, actionID, user.ID)
		return
	}
	if err != nil {
		logger.Error("actions: approve", "error", err, "action_id", actionID)
		writeError(w, logger, http.StatusInternalServerError, "internal error")
		return
	}

	logger.Info("action approved", "action_id", actionID, "run_id", decided.AgentRunID,
		"action", decided.Action, "edited", payloadEdited(decided))
	writeJSON(w, logger, http.StatusOK, s.actionResponse(decided))
}

// approveInTx records the approval and queues the write in one transaction.
func (s *Server) approveInTx(
	r *http.Request,
	actionID, userID uuid.UUID,
	final json.RawMessage,
) (store.AgentAction, error) {
	ctx := r.Context()

	tx, err := s.deps.DB.Begin(ctx)
	if err != nil {
		return store.AgentAction{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op once committed

	q := store.New(tx)
	row, err := q.ApproveAgentAction(ctx, store.ApproveAgentActionParams{
		ID:           actionID,
		UserID:       userID,
		FinalPayload: final,
		DecidedBy:    &userID,
		// The cutoff, not a duration: the age check and the status check share
		// one predicate, so a row cannot expire in the gap between them.
		ProposedAt: pgtype.Timestamptz{
			Time:  actions.Cutoff(s.deps.ActionTTL, time.Now().UTC()),
			Valid: true,
		},
	})
	if err != nil {
		return store.AgentAction{}, err
	}

	if err := agent.RecordActionDecided(ctx, q, row.AgentRunID, row.ID, row.Action,
		row.Status, "", &userID, row.DecidedAt.Time, payloadEdited(row)); err != nil {
		return store.AgentAction{}, err
	}
	if err := s.deps.Enqueuer.EnqueueExecuteWrite(ctx, tx, actionID); err != nil {
		return store.AgentAction{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return store.AgentAction{}, fmt.Errorf("commit transaction: %w", err)
	}
	return row, nil
}

// rejectRequest is the body of POST /api/actions/{id}/reject.
type rejectRequest struct {
	Reason string `json:"reason"`
}

// handleRejectAction serves POST /api/actions/{id}/reject.
//
// The reason is required, and not as ceremony: it is the only thing the agent has
// to work with. Told "rejected" it can do nothing but stop; told "we already
// emailed them yesterday" it can answer the question properly.
func (s *Server) handleRejectAction(w http.ResponseWriter, r *http.Request) {
	logger := s.deps.Logger
	user, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	// A rejection always carries a reason, so a body is mandatory and so is
	// its content type.
	if !hasJSONContentType(r) {
		writeError(w, logger, http.StatusUnsupportedMediaType, "expected application/json")
		return
	}
	actionID, ok := s.actionID(w, r)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	var req rejectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, logger, http.StatusBadRequest, "invalid JSON body")
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		writeError(w, logger, http.StatusBadRequest, "reason is required — the agent is told why, so it can answer without this action")
		return
	}

	// Ownership first, so a reject cannot be used to probe for the existence of
	// somebody else's action.
	if _, err := store.New(s.deps.DB).GetAgentActionForUser(r.Context(), store.GetAgentActionForUserParams{
		ID:     actionID,
		UserID: user.ID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, logger, http.StatusNotFound, "no such action")
			return
		}
		logger.Error("actions: load for reject", "error", err)
		writeError(w, logger, http.StatusInternalServerError, "internal error")
		return
	}

	decided, err := s.rejectInTx(r, actionID, user.ID, reason)
	if errors.Is(err, pgx.ErrNoRows) {
		s.reportUndecidable(r, w, actionID, user.ID)
		return
	}
	if err != nil {
		logger.Error("actions: reject", "error", err, "action_id", actionID)
		writeError(w, logger, http.StatusInternalServerError, "internal error")
		return
	}

	logger.Info("action rejected", "action_id", actionID, "run_id", decided.AgentRunID)
	writeJSON(w, logger, http.StatusOK, s.actionResponse(decided))
}

// rejectInTx records the rejection and, when nothing else on the run is
// outstanding, queues its continuation — both in one transaction.
func (s *Server) rejectInTx(
	r *http.Request,
	actionID, userID uuid.UUID,
	reason string,
) (store.AgentAction, error) {
	ctx := r.Context()

	tx, err := s.deps.DB.Begin(ctx)
	if err != nil {
		return store.AgentAction{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op once committed

	q := store.New(tx)
	row, err := q.RejectAgentAction(ctx, store.RejectAgentActionParams{
		ID:           actionID,
		UserID:       userID,
		RejectReason: &reason,
		DecidedBy:    &userID,
	})
	if err != nil {
		return store.AgentAction{}, err
	}
	if err := agent.RecordActionDecided(ctx, q, row.AgentRunID, row.ID, row.Action,
		row.Status, reason, &userID, row.DecidedAt.Time, false); err != nil {
		return store.AgentAction{}, err
	}

	// A rejection settles this action, so the run may now be able to continue —
	// but only if nothing else on it is outstanding. Counted inside this
	// transaction so the rejection just written is included, and enqueued inside
	// it too so the resume job cannot start before that write is visible.
	//
	// The lock serializes this decision per run: two actions settling at once
	// would each see the other as outstanding and neither would resume the run.
	if _, err := q.LockAgentRunForSettlement(ctx, row.AgentRunID); err != nil {
		return store.AgentAction{}, fmt.Errorf("lock run for settlement: %w", err)
	}
	unsettled, err := q.CountUnsettledActionsByRun(ctx, row.AgentRunID)
	if err != nil {
		return store.AgentAction{}, fmt.Errorf("count unsettled actions: %w", err)
	}
	if unsettled == 0 {
		if err := s.deps.Enqueuer.EnqueueResumeRun(ctx, tx, row.AgentRunID); err != nil {
			return store.AgentAction{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return store.AgentAction{}, fmt.Errorf("commit transaction: %w", err)
	}
	return row, nil
}

// reportUndecidable answers a decision that matched no row, saying which of the
// three reasons applies.
//
// Worth the extra read. "Conflict" alone leaves a person staring at a button
// that did nothing; "somebody already approved this" and "this expired
// yesterday" are different situations with different next steps.
func (s *Server) reportUndecidable(r *http.Request, w http.ResponseWriter, actionID, userID uuid.UUID) {
	logger := s.deps.Logger

	row, err := store.New(s.deps.DB).GetAgentActionForUser(r.Context(), store.GetAgentActionForUserParams{
		ID:     actionID,
		UserID: userID,
	})
	if err != nil {
		writeError(w, logger, http.StatusNotFound, "no such action")
		return
	}

	if row.Status == actions.StatusPending {
		// Still pending, so the status guard was not what refused: the age was.
		//
		// The row is marked expired here, not merely refused, and the
		// distinction matters: a row left pending is a row that a clock skew, a
		// widened ACTION_TTL or a restart with a different config could find
		// decidable again. Marking it makes this refusal the permanent answer.
		if err := s.expireStale(r, actionID); err != nil {
			// The refusal below still stands — the age check already rejected
			// the approval, and nothing was queued. Only the bookkeeping
			// failed, so it is logged rather than turned into a 500 that would
			// suggest the write might yet happen.
			logger.Error("actions: mark refused approval expired", "error", err, "action_id", actionID)
		}
		writeError(w, logger, http.StatusConflict,
			"this request expired before it was decided, so it can no longer be carried out")
		return
	}
	writeError(w, logger, http.StatusConflict,
		fmt.Sprintf("this request has already been decided (%s)", row.Status))
}

// expireStale marks a pending-but-overdue proposal expired and releases its run.
//
// The marking cannot be done on its own. The hourly sweep is what would
// otherwise have retired this row, and it only ever looks at PENDING rows
// (ListExpiredPendingActions) — so a row moved to 'expired' here without its run
// being released would leave that run parked in 'awaiting_approval' with nothing
// left in the system able to find it. That would trade a stale-but-recoverable
// row for a permanently stuck investigation.
//
// So this takes the same three steps the sweep takes, in one transaction: expire
// the row, tell the agent the write never happened, and resume the run once
// nothing else on it is outstanding.
//
// pgx.ErrNoRows is not an error to report: it means the row was decided in the
// moment between the refusal and this update, which is somebody else's
// successful decision and nothing to correct.
func (s *Server) expireStale(r *http.Request, actionID uuid.UUID) error {
	ctx := r.Context()

	tx, err := s.deps.DB.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op once committed

	q := store.New(tx)
	row, err := q.ExpireAgentAction(ctx, actionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("expire proposal: %w", err)
	}
	if err := agent.RecordActionFailed(ctx, q, row.AgentRunID, row.ID, row.Action,
		actions.ExpiredObservation); err != nil {
		return err
	}

	// Serialized per run for the same reason as the rejection path: concurrent
	// settlements must not each conclude the other is still outstanding.
	if _, err := q.LockAgentRunForSettlement(ctx, row.AgentRunID); err != nil {
		return fmt.Errorf("lock run for settlement: %w", err)
	}
	unsettled, err := q.CountUnsettledActionsByRun(ctx, row.AgentRunID)
	if err != nil {
		return fmt.Errorf("count unsettled actions: %w", err)
	}
	if unsettled == 0 {
		if err := s.deps.Enqueuer.EnqueueResumeRun(ctx, tx, row.AgentRunID); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// requireHuman refuses the static operator token on a decision endpoint.
//
// The one guarantee the approval design makes that is NOT configurable is that a
// write happens only after a person read the payload and approved it. The
// bearer token is a shared machine credential — documented for `make index`,
// carried by scripts, present in shell history — and admitting it here would
// let a token holder enable writes, drive a proposal onto the queue and approve
// it with an arbitrary payload, all recorded as the operator's own decision. No
// human would have seen anything. Being admin is not the point; being a person
// is, so this is checked separately from IsAdmin.
func (s *Server) requireHuman(w http.ResponseWriter, r *http.Request) (auth.User, bool) {
	user, ok := auth.UserFrom(r.Context())
	if !ok {
		writeError(w, s.deps.Logger, http.StatusInternalServerError, "internal error")
		return auth.User{}, false
	}
	if user.Machine {
		s.deps.Logger.Warn("refused a write decision made with the operator token",
			"path", r.URL.Path)
		writeError(w, s.deps.Logger, http.StatusForbidden,
			"a write can only be approved or declined by a signed-in person, "+
				"not with the operator API token")
		return auth.User{}, false
	}
	return user, true
}

// actionID parses and validates the path parameter.
func (s *Server) actionID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, s.deps.Logger, http.StatusBadRequest, "action id must be a uuid")
		return uuid.Nil, false
	}
	return id, true
}

// actionResponse shapes one row for the API.
func (s *Server) actionResponse(row store.AgentAction) actionResponse {
	out := actionResponse{
		ID:              row.ID,
		AgentRunID:      row.AgentRunID,
		Source:          row.Source,
		Action:          row.Action,
		Status:          row.Status,
		ProposedPayload: json.RawMessage(row.ProposedPayload),
		ProposedAt:      row.ProposedAt.Time,
		DecidedBy:       row.DecidedBy,
		Edited:          payloadEdited(row),
	}
	if len(row.FinalPayload) > 0 {
		out.FinalPayload = json.RawMessage(row.FinalPayload)
	}
	if len(row.Result) > 0 {
		out.Result = json.RawMessage(row.Result)
	}
	if row.RejectReason != nil {
		out.RejectReason = *row.RejectReason
	}
	if row.Error != nil {
		out.Error = *row.Error
	}
	if row.DecidedAt.Valid {
		decided := row.DecidedAt.Time
		out.DecidedAt = &decided
	}
	if row.ExecutedAt.Valid {
		executed := row.ExecutedAt.Time
		out.ExecutedAt = &executed
	}
	out.Editable = row.Status == actions.StatusPending &&
		!actions.Expired(row.ProposedAt.Time, s.deps.ActionTTL, time.Now().UTC())
	return out
}

// payloadEdited reports whether a human changed the payload before approving.
//
// Canonicalized before comparing, so a client that re-serialized the payload
// with different key ordering or whitespace — which every JSON library does
// differently — is not recorded as having edited it.
func payloadEdited(row store.AgentAction) bool {
	if len(row.FinalPayload) == 0 {
		return false
	}
	proposed, err := tools.CanonicalJSON(row.ProposedPayload)
	if err != nil {
		return false
	}
	final, err := tools.CanonicalJSON(row.FinalPayload)
	if err != nil {
		return false
	}
	return proposed != final
}

// isJSONObject reports whether raw is a JSON object rather than some other
// valid JSON value.
func isJSONObject(raw json.RawMessage) bool {
	var out map[string]any
	return json.Unmarshal(raw, &out) == nil
}
