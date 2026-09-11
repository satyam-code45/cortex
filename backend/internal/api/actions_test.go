package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/api"
)

// The approval endpoints — the human half of the write gate.
//
// These are the only routes through which a write can ever be authorized, so
// the properties under test are the ones an auditor would ask about: a decision
// can only be made by the row's owner, only once, only while the proposal is
// still decidable, and approving enqueues execution rather than performing it.
// Rows are read back with raw SQL so the assertions do not run through the same
// generated queries the handlers write with.

// seededAction is a pending proposal belonging to the bearer-authenticated user.
type seededAction struct {
	userID   uuid.UUID
	runID    uuid.UUID
	actionID uuid.UUID
}

const proposedEmail = `{"from":"me@example.com","to":["ines@example.com"],` +
	`"subject":"Sandbox notice","body":"We received it."}`

// insertActionUser creates (or reuses) a user by email and returns its id. The
// bearer middleware upserts on the same email, so seeding devUserEmail here is
// what makes the seeded action belong to the caller.
func insertActionUser(t *testing.T, pool *pgxpool.Pool, email string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO users (email) VALUES ($1)
		 ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email RETURNING id`,
		email).Scan(&id); err != nil {
		t.Fatalf("insert user %s: %v", email, err)
	}
	return id
}

// seedPendingAction inserts a paused run with one pending action on it.
//
// age backdates proposed_at, which is how a proposal past its TTL is staged.
func seedPendingAction(t *testing.T, pool *pgxpool.Pool, email, action, payload string, age time.Duration) seededAction {
	t.Helper()
	ctx := context.Background()
	userID := insertActionUser(t, pool, email)

	var conversationID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO conversations (user_id, title) VALUES ($1, 'writes') RETURNING id`,
		userID).Scan(&conversationID); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	var runID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO agent_runs (conversation_id, query, status, model)
		 VALUES ($1, 'reply to Ines', 'awaiting_approval', 'gpt-test') RETURNING id`,
		conversationID).Scan(&runID); err != nil {
		t.Fatalf("insert agent_run: %v", err)
	}
	return seededAction{
		userID:   userID,
		runID:    runID,
		actionID: addPendingAction(t, pool, userID, runID, action, payload, age),
	}
}

// addPendingAction adds one more pending proposal to an existing run.
func addPendingAction(
	t *testing.T,
	pool *pgxpool.Pool,
	userID, runID uuid.UUID,
	action, payload string,
	age time.Duration,
) uuid.UUID {
	t.Helper()
	source := action
	if i := strings.IndexByte(action, '.'); i > 0 {
		source = action[:i]
	}
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO agent_actions
		   (agent_run_id, user_id, source, action, proposed_payload, idempotency_key, proposed_at)
		 VALUES ($1, $2, $3, $4, $5, $6, now() - $7::interval) RETURNING id`,
		runID, userID, source, action, payload, "fixture-"+uuid.NewString(),
		fmt.Sprintf("%d milliseconds", age.Milliseconds())).Scan(&id); err != nil {
		t.Fatalf("insert agent_action: %v", err)
	}
	return id
}

// storedAction is the row as the database holds it.
type storedAction struct {
	status          string
	proposedPayload []byte
	finalPayload    []byte
	rejectReason    *string
	decidedBy       *uuid.UUID
	decidedAt       *time.Time
}

func readAction(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) storedAction {
	t.Helper()
	var row storedAction
	if err := pool.QueryRow(context.Background(),
		`SELECT status, proposed_payload, final_payload, reject_reason, decided_by, decided_at
		 FROM agent_actions WHERE id = $1`, id).
		Scan(&row.status, &row.proposedPayload, &row.finalPayload, &row.rejectReason,
			&row.decidedBy, &row.decidedAt); err != nil {
		t.Fatalf("read agent_action %s: %v", id, err)
	}
	return row
}

// newActionsRouter wires a router with the approval routes and a recording
// enqueuer. ttl is the ActionTTL the approve query enforces.
func newActionsRouter(t *testing.T, pool *pgxpool.Pool, ttl time.Duration) (http.Handler, *stubEnqueuer) {
	t.Helper()
	enq := &stubEnqueuer{}
	h := api.NewRouter(withTestAuth(api.Deps{
		DB:        pool,
		Enqueuer:  enq,
		Model:     testModel,
		Logger:    discardLogger(),
		ActionTTL: ttl,
	}))
	return h, enq
}

// postAction drives one decision request.
func postAction(
	t *testing.T,
	pool *pgxpool.Pool,
	h http.Handler,
	target, body string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := humanRequest(t, pool, http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// decodeAction decodes one action from a handler response.
func decodeAction(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode action response %q: %v", rec.Body.String(), err)
	}
	return out
}

// ---------------------------------------------------------------------------
// approving
// ---------------------------------------------------------------------------

// Approving records the decision and queues exactly one
// execution — the endpoint never performs the write itself, which is what makes
// exactly-once achievable at all.
func TestApprovingAProposalQueuesExactlyOneExecution(t *testing.T) {
	pool := testPool(t)
	h, enq := newActionsRouter(t, pool, time.Hour)
	seeded := seedPendingAction(t, pool, devUserEmail, "gmail.send", proposedEmail, 0)

	rec := postAction(t, pool, h, "/api/actions/"+seeded.actionID.String()+"/approve", "{}")
	if rec.Code != http.StatusOK {
		t.Fatalf("approve status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}

	row := readAction(t, pool, seeded.actionID)
	if row.status != "approved" {
		t.Errorf("status = %q, want approved", row.status)
	}
	// The payload that will execute defaults to the proposal, so an unedited
	// approval does not depend on the client echoing it back.
	if !jsonEqual(t, row.finalPayload, row.proposedPayload) {
		t.Errorf("final_payload = %s, want the proposal %s", row.finalPayload, row.proposedPayload)
	}
	if row.decidedBy == nil || *row.decidedBy != seeded.userID {
		t.Errorf("decided_by = %v, want the caller %s", row.decidedBy, seeded.userID)
	}
	if row.decidedAt == nil {
		t.Error("decided_at is null on an approved row")
	}

	if queued := enq.queuedWrites(); len(queued) != 1 || queued[0] != seeded.actionID {
		t.Errorf("queued writes = %v, want exactly [%s]", queued, seeded.actionID)
	}
	// Approval alone must not resume the run: the answer has to report what
	// actually happened, which is only known once the write job has run.
	if resumed := enq.queuedResumes(); len(resumed) != 0 {
		t.Errorf("queued resumes = %v, want none until the write has executed", resumed)
	}

	// The decision is in the run's timeline, so the trace can show who decided.
	if n := queryInt(t, pool,
		`SELECT count(*) FROM run_events WHERE agent_run_id = $1 AND type = 'action_decided'`,
		seeded.runID); n != 1 {
		t.Errorf("action_decided events = %d, want 1", n)
	}

	body := decodeAction(t, rec)
	if body["edited"] != false {
		t.Errorf("edited = %v on an untouched approval, want false", body["edited"])
	}
}

// The edited payload is what gets stored for execution, and the
// proposal is kept beside it so the audit trail shows both.
func TestApprovingWithAnEditedPayloadStoresTheEditedOne(t *testing.T) {
	pool := testPool(t)
	h, enq := newActionsRouter(t, pool, time.Hour)
	seeded := seedPendingAction(t, pool, devUserEmail, "gmail.send", proposedEmail, 0)

	edited := `{"from":"me@example.com","to":["ines@example.com"],` +
		`"subject":"Sandbox notice","body":"Thanks Ines - received, and we are on it."}`
	rec := postAction(t, pool, h, "/api/actions/"+seeded.actionID.String()+"/approve",
		`{"final_payload":`+edited+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}

	row := readAction(t, pool, seeded.actionID)
	if !jsonEqual(t, row.finalPayload, []byte(edited)) {
		t.Errorf("final_payload = %s, want the human's edit %s", row.finalPayload, edited)
	}
	if !jsonEqual(t, row.proposedPayload, []byte(proposedEmail)) {
		t.Errorf("proposed_payload = %s, want the agent's original request preserved", row.proposedPayload)
	}

	body := decodeAction(t, rec)
	if body["edited"] != true {
		t.Errorf("edited = %v, want true so the audit view can say a person changed it", body["edited"])
	}
	if len(enq.queuedWrites()) != 1 {
		t.Errorf("queued writes = %v, want one execution of the edited payload", enq.queuedWrites())
	}
}

// A payload that is valid JSON but not an object is refused before anything is
// recorded: every write payload is an object, and storing a bare value would
// only fail to decode after a person had already approved it.
func TestApproveRejectsAPayloadThatIsNotAnObject(t *testing.T) {
	pool := testPool(t)
	h, enq := newActionsRouter(t, pool, time.Hour)
	seeded := seedPendingAction(t, pool, devUserEmail, "gmail.send", proposedEmail, 0)

	for _, body := range []string{`{"final_payload":42}`, `{"final_payload":"a string"}`, `{"final_payload":[]}`} {
		rec := postAction(t, pool, h, "/api/actions/"+seeded.actionID.String()+"/approve", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("approve with %s = %d, want 400 (body %q)", body, rec.Code, rec.Body.String())
		}
	}
	if row := readAction(t, pool, seeded.actionID); row.status != "pending" {
		t.Errorf("status = %q after refused approvals, want pending", row.status)
	}
	if len(enq.queuedWrites()) != 0 {
		t.Errorf("queued writes = %v, want none", enq.queuedWrites())
	}
}

// Two concurrent approvals resolve to one winner — one 200, one 409,
// and exactly one execution queued. The guard is the conditional UPDATE, so the
// database decides the race rather than the handler.
func TestConcurrentApprovalsOverHTTPResolveToOneWinner(t *testing.T) {
	pool := testPool(t)
	h, enq := newActionsRouter(t, pool, time.Hour)
	seeded := seedPendingAction(t, pool, devUserEmail, "gmail.send", proposedEmail, 0)

	// One session, minted before the goroutines start: both requests are the
	// same person clicking twice, and creating the session inside the race would
	// be testing the session insert rather than the approval.
	cookie, _ := createSessionForEmail(t, pool, devUserEmail)

	const attempts = 2
	codes := make([]int, attempts)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req := anonymousRequest(http.MethodPost,
				"/api/actions/"+seeded.actionID.String()+"/approve", strings.NewReader("{}"))
			req.AddCookie(cookie)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			codes[i] = rec.Code
		}()
	}
	close(start)
	wg.Wait()

	ok, conflict := 0, 0
	for _, code := range codes {
		switch code {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
		default:
			t.Errorf("unexpected approve status %d, want 200 or 409", code)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Errorf("statuses = %v, want exactly one 200 and one 409", codes)
	}
	if queued := enq.queuedWrites(); len(queued) != 1 {
		t.Errorf("queued writes = %v, want exactly one — a second would be a second email", queued)
	}
}

// A decided proposal cannot be decided again, in either direction. Approving
// after a rejection would execute a write a person declined.
func TestADecidedProposalCannotBeDecidedAgain(t *testing.T) {
	tests := []struct {
		name  string
		first string
		body  string
		then  string
		body2 string
	}{
		{name: "approve then approve", first: "approve", body: "{}", then: "approve", body2: "{}"},
		{
			name: "reject then approve", first: "reject", body: `{"reason":"already emailed her"}`,
			then: "approve", body2: "{}",
		},
		{
			name: "approve then reject", first: "approve", body: "{}",
			then: "reject", body2: `{"reason":"changed my mind"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := testPool(t)
			h, enq := newActionsRouter(t, pool, time.Hour)
			seeded := seedPendingAction(t, pool, devUserEmail, "gmail.send", proposedEmail, 0)
			target := "/api/actions/" + seeded.actionID.String() + "/"

			if rec := postAction(t, pool, h, target+tt.first, tt.body); rec.Code != http.StatusOK {
				t.Fatalf("first %s = %d, want 200 (body %q)", tt.first, rec.Code, rec.Body.String())
			}
			statusAfterFirst := readAction(t, pool, seeded.actionID).status

			rec := postAction(t, pool, h, target+tt.then, tt.body2)
			if rec.Code != http.StatusConflict {
				t.Fatalf("second %s = %d, want 409 (body %q)", tt.then, rec.Code, rec.Body.String())
			}
			if got := rec.Body.String(); !strings.Contains(got, "already been decided") {
				t.Errorf("conflict body = %q, want it to say the request was already decided", got)
			}
			if got := readAction(t, pool, seeded.actionID).status; got != statusAfterFirst {
				t.Errorf("status = %q after a refused second decision, want %q", got, statusAfterFirst)
			}
			if got := len(enq.queuedWrites()); got > 1 {
				t.Errorf("queued writes = %d, want at most the one from the first decision", got)
			}
		})
	}
}

// A proposal past its TTL cannot be approved. The refusal is what
// stops a stale proposal executing, and the reason is spelled out — "already
// decided" and "expired yesterday" are different situations.
func TestApprovingAProposalPastItsTTLIsRefused(t *testing.T) {
	pool := testPool(t)
	h, enq := newActionsRouter(t, pool, time.Hour)
	seeded := seedPendingAction(t, pool, devUserEmail, "gmail.send", proposedEmail, 2*time.Hour)

	rec := postAction(t, pool, h, "/api/actions/"+seeded.actionID.String()+"/approve", "{}")
	if rec.Code != http.StatusConflict {
		t.Fatalf("approve of an expired proposal = %d, want 409 (body %q)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); !strings.Contains(got, "expired") {
		t.Errorf("conflict body = %q, want it to say the request expired", got)
	}
	if row := readAction(t, pool, seeded.actionID); row.finalPayload != nil {
		t.Errorf("final_payload = %s on an expired proposal, want none", row.finalPayload)
	}
	if len(enq.queuedWrites()) != 0 {
		t.Errorf("queued writes = %v, want none — an expired proposal can never execute", enq.queuedWrites())
	}
}

// The approve endpoint must refuse — and mark expired — any pending row
// already past its TTL. The marking is the half that makes the refusal
// permanent: a row left pending is a row the next sweep, or a clock skew, could
// still find decidable.
func TestApprovingAnExpiredProposalMarksItExpired(t *testing.T) {
	pool := testPool(t)
	h, _ := newActionsRouter(t, pool, time.Hour)
	seeded := seedPendingAction(t, pool, devUserEmail, "gmail.send", proposedEmail, 2*time.Hour)

	if rec := postAction(t, pool, h, "/api/actions/"+seeded.actionID.String()+"/approve", "{}"); rec.Code != http.StatusConflict {
		t.Fatalf("approve of an expired proposal = %d, want 409 (body %q)", rec.Code, rec.Body.String())
	}
	if got := readAction(t, pool, seeded.actionID).status; got != "expired" {
		t.Errorf("status = %q after the approve endpoint refused a stale proposal, want expired", got)
	}
}

// ---------------------------------------------------------------------------
// rejecting
// ---------------------------------------------------------------------------

// A rejection needs a reason, because the reason is the only thing
// the agent has to work with when it resumes.
func TestRejectingRequiresAReason(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "absent", body: `{}`},
		{name: "empty", body: `{"reason":""}`},
		{name: "whitespace", body: `{"reason":"   "}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := testPool(t)
			h, enq := newActionsRouter(t, pool, time.Hour)
			seeded := seedPendingAction(t, pool, devUserEmail, "gmail.send", proposedEmail, 0)

			rec := postAction(t, pool, h, "/api/actions/"+seeded.actionID.String()+"/reject", tt.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("reject without a reason = %d, want 400 (body %q)", rec.Code, rec.Body.String())
			}
			if row := readAction(t, pool, seeded.actionID); row.status != "pending" {
				t.Errorf("status = %q, want the proposal left pending", row.status)
			}
			if len(enq.queuedResumes()) != 0 {
				t.Errorf("queued resumes = %v, want none", enq.queuedResumes())
			}
		})
	}
}

// A rejection is terminal for the action, never
// executed, and resumes the run so the agent can answer without it.
func TestRejectingRecordsTheReasonAndResumesTheRun(t *testing.T) {
	pool := testPool(t)
	h, enq := newActionsRouter(t, pool, time.Hour)
	seeded := seedPendingAction(t, pool, devUserEmail, "jira.create_issue",
		`{"project_key":"ATLAS","issue_type":"Task","summary":"Track certification"}`, 0)

	rec := postAction(t, pool, h, "/api/actions/"+seeded.actionID.String()+"/reject",
		`{"reason":"  we already track this under ATLAS-88  "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reject status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}

	row := readAction(t, pool, seeded.actionID)
	if row.status != "rejected" {
		t.Errorf("status = %q, want rejected", row.status)
	}
	if row.rejectReason == nil || *row.rejectReason != "we already track this under ATLAS-88" {
		t.Errorf("reject_reason = %v, want the trimmed reason", row.rejectReason)
	}
	if row.decidedBy == nil || *row.decidedBy != seeded.userID {
		t.Errorf("decided_by = %v, want the caller", row.decidedBy)
	}
	if row.finalPayload != nil {
		t.Errorf("final_payload = %s on a rejected row, want none — nothing was approved", row.finalPayload)
	}
	if len(enq.queuedWrites()) != 0 {
		t.Errorf("queued writes = %v, want none — a rejected proposal must never execute", enq.queuedWrites())
	}
	if resumed := enq.queuedResumes(); len(resumed) != 1 || resumed[0] != seeded.runID {
		t.Errorf("queued resumes = %v, want exactly [%s]", resumed, seeded.runID)
	}
}

// A run may propose several writes and waits for all of them, so
// deciding one of two must not resume the run.
func TestRejectingOneOfTwoProposalsDoesNotResumeTheRun(t *testing.T) {
	pool := testPool(t)
	h, enq := newActionsRouter(t, pool, time.Hour)
	seeded := seedPendingAction(t, pool, devUserEmail, "gmail.send", proposedEmail, 0)
	second := addPendingAction(t, pool, seeded.userID, seeded.runID, "jira.create_issue",
		`{"project_key":"ATLAS","summary":"Track certification"}`, 0)

	rec := postAction(t, pool, h, "/api/actions/"+seeded.actionID.String()+"/reject", `{"reason":"not needed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reject status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if resumed := enq.queuedResumes(); len(resumed) != 0 {
		t.Errorf("queued resumes = %v, want none while the second proposal is undecided", resumed)
	}

	rec = postAction(t, pool, h, "/api/actions/"+second.String()+"/reject", `{"reason":"nor this"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("second reject status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if resumed := enq.queuedResumes(); len(resumed) != 1 || resumed[0] != seeded.runID {
		t.Errorf("queued resumes = %v, want one resume once every proposal is settled", resumed)
	}
}

// ---------------------------------------------------------------------------
// ownership
// ---------------------------------------------------------------------------

// An action row is permission to send mail from somebody's account, so it must
// be invisible and undecidable to anybody else — and indistinguishable from a
// row that never existed.
func TestAnotherUsersActionCannotBeSeenOrDecided(t *testing.T) {
	pool := testPool(t)
	h, enq := newActionsRouter(t, pool, time.Hour)
	// The caller is devUserEmail; the proposal belongs to somebody else.
	insertActionUser(t, pool, devUserEmail)
	other := seedPendingAction(t, pool, "stranger@cortex.test", "gmail.send", proposedEmail, 0)

	target := "/api/actions/" + other.actionID.String() + "/"
	for _, tt := range []struct{ route, body string }{
		{route: "approve", body: "{}"},
		{route: "reject", body: `{"reason":"not mine"}`},
	} {
		rec := postAction(t, pool, h, target+tt.route, tt.body)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s of another user's action = %d, want 404 (body %q)", tt.route, rec.Code, rec.Body.String())
		}
	}
	if row := readAction(t, pool, other.actionID); row.status != "pending" {
		t.Errorf("status = %q, want the other user's proposal untouched", row.status)
	}
	if len(enq.queuedWrites()) != 0 || len(enq.queuedResumes()) != 0 {
		t.Errorf("jobs queued for another user's action: writes %v resumes %v",
			enq.queuedWrites(), enq.queuedResumes())
	}

	// And it is absent from the audit list.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localRequest(http.MethodGet, "/api/actions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/actions = %d (body %q)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), other.actionID.String()) {
		t.Errorf("the audit list carries another user's action: %q", rec.Body.String())
	}
}

// A malformed id is a 400, not a lookup.
func TestActionIDMustBeAUUID(t *testing.T) {
	pool := testPool(t)
	h, _ := newActionsRouter(t, pool, time.Hour)

	rec := postAction(t, pool, h, "/api/actions/not-a-uuid/approve", "{}")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("approve with a bad id = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
}

// A write can only be decided by a person, so the static operator token is
// refused even though it is an admin credential.
//
// The token is what scripts carry: it sits in shell history, in a Makefile, in
// CI config. Admitting it here would mean a token holder could enable writes,
// drive a proposal onto the queue and approve it with a payload of their choice,
// recorded as the owner's own decision — with no human having read anything.
// That is the one property the approval gate is not allowed to lose, so it is
// checked separately from whether the caller is an admin.
func TestTheOperatorTokenCannotDecideAWrite(t *testing.T) {
	tests := []struct {
		name   string
		suffix string
		body   string
	}{
		{name: "approve", suffix: "/approve", body: "{}"},
		{name: "reject", suffix: "/reject", body: `{"reason":"not now"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := testPool(t)
			h, enq := newActionsRouter(t, pool, time.Hour)
			seeded := seedPendingAction(t, pool, devUserEmail, "gmail.send", proposedEmail, 0)

			// localRequest carries the bearer token, which is the point here.
			req := localRequest(http.MethodPost,
				"/api/actions/"+seeded.actionID.String()+tt.suffix, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body %q)", rec.Code, rec.Body.String())
			}
			if row := readAction(t, pool, seeded.actionID); row.status != "pending" {
				t.Errorf("status = %q, want it to stay pending — the token decided nothing", row.status)
			}
			if len(enq.queuedWrites()) != 0 {
				t.Errorf("queued writes = %v, want none", enq.queuedWrites())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// the audit list
// ---------------------------------------------------------------------------

// The Actions page is the audit view: every action across runs, newest first,
// with both payloads in full, the decision and who made it.
func TestTheAuditListReportsEveryActionWithBothPayloads(t *testing.T) {
	pool := testPool(t)
	h, _ := newActionsRouter(t, pool, time.Hour)
	seeded := seedPendingAction(t, pool, devUserEmail, "gmail.send", proposedEmail, 0)
	older := addPendingAction(t, pool, seeded.userID, seeded.runID, "jira.create_issue",
		`{"project_key":"ATLAS","summary":"Track certification"}`, 30*time.Minute)

	// Decide one of them, with an edit, so the list has both halves to report.
	edited := `{"from":"me@example.com","to":["ines@example.com"],` +
		`"subject":"Sandbox notice","body":"Edited before approval."}`
	if rec := postAction(t, pool, h, "/api/actions/"+seeded.actionID.String()+"/approve",
		`{"final_payload":`+edited+`}`); rec.Code != http.StatusOK {
		t.Fatalf("approve status = %d (body %q)", rec.Code, rec.Body.String())
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localRequest(http.MethodGet, "/api/actions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/actions = %d (body %q)", rec.Code, rec.Body.String())
	}
	var body struct {
		Actions []struct {
			ID              uuid.UUID       `json:"id"`
			AgentRunID      uuid.UUID       `json:"agent_run_id"`
			Source          string          `json:"source"`
			Action          string          `json:"action"`
			Status          string          `json:"status"`
			ProposedPayload json.RawMessage `json:"proposed_payload"`
			FinalPayload    json.RawMessage `json:"final_payload"`
			DecidedBy       *uuid.UUID      `json:"decided_by"`
			DecidedAt       *time.Time      `json:"decided_at"`
			Edited          bool            `json:"edited"`
			Editable        bool            `json:"editable"`
		} `json:"actions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode actions list %q: %v", rec.Body.String(), err)
	}
	if len(body.Actions) != 2 {
		t.Fatalf("actions = %d, want 2", len(body.Actions))
	}
	// Newest first.
	if body.Actions[0].ID != seeded.actionID || body.Actions[1].ID != older {
		t.Errorf("order = %v, want the newest proposal first", []uuid.UUID{body.Actions[0].ID, body.Actions[1].ID})
	}

	decided := body.Actions[0]
	if decided.Status != "approved" || !decided.Edited {
		t.Errorf("decided row = status %q edited %v, want approved and edited", decided.Status, decided.Edited)
	}
	if !jsonEqual(t, decided.ProposedPayload, []byte(proposedEmail)) {
		t.Errorf("proposed_payload = %s, want the agent's request in full", decided.ProposedPayload)
	}
	if !jsonEqual(t, decided.FinalPayload, []byte(edited)) {
		t.Errorf("final_payload = %s, want the approved version in full", decided.FinalPayload)
	}
	if decided.DecidedBy == nil || *decided.DecidedBy != seeded.userID || decided.DecidedAt == nil {
		t.Errorf("decider = %v at %v, want the caller and a timestamp", decided.DecidedBy, decided.DecidedAt)
	}
	if decided.Editable {
		t.Error("a decided row reports itself editable; the UI would offer buttons that do nothing")
	}
	if decided.AgentRunID != seeded.runID {
		t.Errorf("agent_run_id = %s, want %s", decided.AgentRunID, seeded.runID)
	}
	if !body.Actions[1].Editable {
		t.Error("a fresh pending row reports itself not editable")
	}
}

// A pending row past its TTL is reported as no longer actionable, so the browser
// does not have to guess from a timestamp.
func TestTheAuditListMarksAnExpiredProposalUneditable(t *testing.T) {
	pool := testPool(t)
	h, _ := newActionsRouter(t, pool, time.Hour)
	seedPendingAction(t, pool, devUserEmail, "gmail.send", proposedEmail, 90*time.Minute)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localRequest(http.MethodGet, "/api/actions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/actions = %d (body %q)", rec.Code, rec.Body.String())
	}
	var body struct {
		Actions []struct {
			Status   string `json:"status"`
			Editable bool   `json:"editable"`
		} `json:"actions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode actions list: %v", err)
	}
	if len(body.Actions) != 1 {
		t.Fatalf("actions = %d, want 1", len(body.Actions))
	}
	if body.Actions[0].Editable {
		t.Error("a proposal past its TTL is reported editable; approving it would be refused")
	}
}

// The list is read-only: there is no route that mutates an action other than the
// two decision endpoints, and an executed write cannot be undone from here.
func TestTheAuditListOffersNoMutation(t *testing.T) {
	pool := testPool(t)
	h, _ := newActionsRouter(t, pool, time.Hour)
	seeded := seedPendingAction(t, pool, devUserEmail, "gmail.send", proposedEmail, 0)

	for _, tt := range []struct{ method, target string }{
		{method: http.MethodDelete, target: "/api/actions/" + seeded.actionID.String()},
		{method: http.MethodPut, target: "/api/actions/" + seeded.actionID.String()},
		{method: http.MethodPost, target: "/api/actions"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, localRequest(tt.method, tt.target, strings.NewReader("{}")))
		if rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404/405 — the audit view is read-only",
				tt.method, tt.target, rec.Code)
		}
	}
}

// The decision endpoints require authentication like everything else: an
// anonymous approve must not reach the database.
func TestDecisionsRequireAuthentication(t *testing.T) {
	pool := testPool(t)
	h, enq := newActionsRouter(t, pool, time.Hour)
	seeded := seedPendingAction(t, pool, devUserEmail, "gmail.send", proposedEmail, 0)

	req := anonymousRequest(http.MethodPost,
		"/api/actions/"+seeded.actionID.String()+"/approve", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous approve = %d, want 401 (body %q)", rec.Code, rec.Body.String())
	}
	if row := readAction(t, pool, seeded.actionID); row.status != "pending" {
		t.Errorf("status = %q, want pending", row.status)
	}
	if len(enq.queuedWrites()) != 0 {
		t.Errorf("queued writes = %v, want none", enq.queuedWrites())
	}
}

// jsonEqual compares two JSON documents structurally, so key ordering and
// whitespace do not decide the assertion.
func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("decode %q: %v", a, err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("decode %q: %v", b, err)
	}
	return fmt.Sprintf("%#v", av) == fmt.Sprintf("%#v", bv)
}
