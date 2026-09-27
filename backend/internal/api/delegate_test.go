package api_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/api"
)

const proposedDelegation = `{"repo":"cortex","issue_key":"PAY-142","base_branch":"main",` +
	`"prompt":"Fix the retry bug described in PAY-142."}`

// Approving claude_code.delegate records the
// decision and enqueues NO job at all — the owner's runner polls for approved
// rows. Approving gmail.send still enqueues exactly one execution.
func TestApprovingADelegationEnqueuesNothingWhileOtherWritesStillQueue(t *testing.T) {
	cases := []struct {
		action     string
		payload    string
		wantQueued bool
	}{
		{"claude_code.delegate", proposedDelegation, false},
		{"gmail.send", proposedEmail, true},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			pool := testPool(t)
			h, enq := newActionsRouter(t, pool, time.Hour)
			seeded := seedPendingAction(t, pool, devUserEmail, tc.action, tc.payload, 0)

			rec := postAction(t, pool, h, "/api/actions/"+seeded.actionID.String()+"/approve", "{}")
			if rec.Code != http.StatusOK {
				t.Fatalf("approve status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
			}
			if row := readAction(t, pool, seeded.actionID); row.status != "approved" {
				t.Errorf("status = %q, want approved", row.status)
			}
			queued := enq.queuedWrites()
			if tc.wantQueued && (len(queued) != 1 || queued[0] != seeded.actionID) {
				t.Errorf("queued writes = %v, want exactly [%s]", queued, seeded.actionID)
			}
			if !tc.wantQueued && len(queued) != 0 {
				t.Errorf("queued writes = %v, want none: a server job cannot perform a delegation", queued)
			}
			if resumed := enq.queuedResumes(); len(resumed) != 0 {
				t.Errorf("queued resumes = %v, want none on approval", resumed)
			}
			if n := queryInt(t, pool, `SELECT count(*) FROM river_job`); n != 0 {
				t.Errorf("river_job rows = %d, want 0 (the stub enqueuer is the only way in)", n)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The events stream of a paused run stays open while one of its
// actions is approved/executing, and drains and closes otherwise.
// ---------------------------------------------------------------------------

// seedPausedDelegation stages the event log a real paused-then-approved
// delegation has: the proposal, the pause, then the decision.
func seedPausedDelegation(t *testing.T, pool *pgxpool.Pool, actionStatus string) (runID, actionID uuid.UUID) {
	t.Helper()
	seeded := seedPendingAction(t, pool, devUserEmail, "claude_code.delegate", proposedDelegation, 0)
	insertRunEvent(t, pool, seeded.runID, 1, "run_started", `{}`)
	insertRunEvent(t, pool, seeded.runID, 2, "action_proposed", `{"action":"claude_code.delegate"}`)
	insertRunEvent(t, pool, seeded.runID, 3, "run_paused", `{"waiting":[]}`)
	if actionStatus != "pending" {
		insertRunEvent(t, pool, seeded.runID, 4, "action_decided", `{"status":"approved"}`)
		if _, err := pool.Exec(context.Background(),
			`UPDATE agent_actions SET status = $2, decided_at = now(), final_payload = proposed_payload,
			        executed_at = CASE WHEN $2 = 'executed' THEN now() END
			  WHERE id = $1`, seeded.actionID, actionStatus); err != nil {
			t.Fatalf("force action status %s: %v", actionStatus, err)
		}
	}
	return seeded.runID, seeded.actionID
}

func openEventStream(t *testing.T, srv *httptest.Server, runID uuid.UUID, lastEventID string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/runs/"+runID.String()+"/events", nil)
	if err != nil {
		t.Fatalf("build events request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testAPIToken)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	return resp
}

// frameResult is one readSSEFrame outcome, delivered through a channel so a
// stream that wrongly stays silent fails the test rather than hanging it.
type frameResult struct {
	f   sseFrame
	err error
}

func frameReader(br *bufio.Reader) <-chan frameResult {
	ch := make(chan frameResult, 64)
	go func() {
		defer close(ch)
		for {
			f, err := readSSEFrame(br)
			ch <- frameResult{f, err}
			if err != nil {
				return
			}
		}
	}()
	return ch
}

func nextFrame(t *testing.T, ch <-chan frameResult) frameResult {
	t.Helper()
	select {
	case r, ok := <-ch:
		if !ok {
			return frameResult{err: io.EOF}
		}
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("no frame within 3s")
		return frameResult{}
	}
}

// The case the browser actually produces: after posting a decision it opens a
// FRESH stream (no Last-Event-ID), which replays from seq 1 — through the
// run_paused event — and must still stay open to deliver the progress.
func TestRunEventsStaysOpenWhileADelegationExecutes(t *testing.T) {
	for _, status := range []string{"approved", "executing"} {
		t.Run(status, func(t *testing.T) {
			pool := testPool(t)
			t.Cleanup(api.SetSSETimingsForTest(15*time.Millisecond, time.Hour, 10*time.Second))
			runID, actionID := seedPausedDelegation(t, pool, status)

			srv := httptest.NewServer(newChatRouter(pool, &stubEnqueuer{}))
			t.Cleanup(srv.Close)
			frames := frameReader(bufio.NewReader(openEventStream(t, srv, runID, "").Body))

			for _, want := range []string{"run_started", "action_proposed", "run_paused", "action_decided"} {
				r := nextFrame(t, frames)
				if r.err != nil {
					t.Fatalf("stream ended before %s: %v — a paused run with an %s action must stay open",
						want, r.err, status)
				}
				if r.f.event != want {
					t.Fatalf("frame = %q, want %q", r.f.event, want)
				}
			}

			// Progress arrives live on the open stream.
			insertRunEvent(t, pool, runID, 5, "delegate_progress",
				`{"action_id":"`+actionID.String()+`","kind":"tool_use","tool":"Read"}`)
			if r := nextFrame(t, frames); r.err != nil || r.f.event != "delegate_progress" {
				t.Fatalf("live frame = %+v, err %v; want delegate_progress", r.f, r.err)
			}

			// The delegation settles and the resumed run finishes: the stream closes.
			if _, err := pool.Exec(context.Background(),
				`UPDATE agent_actions SET status = 'executed', executed_at = now() WHERE id = $1`, actionID); err != nil {
				t.Fatalf("settle action: %v", err)
			}
			insertRunEvent(t, pool, runID, 6, "action_executed", `{}`)
			insertRunEvent(t, pool, runID, 7, "run_resumed", `{}`)
			insertRunEvent(t, pool, runID, 8, "run_finished", `{"iterations":2}`)
			for _, want := range []string{"action_executed", "run_resumed", "run_finished"} {
				if r := nextFrame(t, frames); r.err != nil || r.f.event != want {
					t.Fatalf("frame = %+v, err %v; want %s", r.f, r.err, want)
				}
			}
			if r := nextFrame(t, frames); !errors.Is(r.err, io.EOF) {
				t.Errorf("after run_finished got %+v, err %v; want EOF", r.f, r.err)
			}
		})
	}
}

// An EventSource auto-reconnect sends Last-Event-ID past the pause; that
// stream must stay open too while the delegation executes.
func TestRunEventsReconnectPastThePauseStaysOpenWhileExecuting(t *testing.T) {
	pool := testPool(t)
	t.Cleanup(api.SetSSETimingsForTest(15*time.Millisecond, time.Hour, 10*time.Second))
	runID, actionID := seedPausedDelegation(t, pool, "executing")

	srv := httptest.NewServer(newChatRouter(pool, &stubEnqueuer{}))
	t.Cleanup(srv.Close)
	frames := frameReader(bufio.NewReader(openEventStream(t, srv, runID, "4").Body))

	insertRunEvent(t, pool, runID, 5, "delegate_progress",
		`{"action_id":"`+actionID.String()+`","kind":"text"}`)
	if r := nextFrame(t, frames); r.err != nil || r.f.event != "delegate_progress" {
		t.Fatalf("frame = %+v, err %v; want delegate_progress on the open stream", r.f, r.err)
	}
}

// With nothing in flight — the action still pending, or already executed —
// the paused run drains its transcript and closes at once.
func TestRunEventsPausedRunWithNothingInFlightDrainsAndCloses(t *testing.T) {
	for _, status := range []string{"pending", "executed", "failed"} {
		t.Run(status, func(t *testing.T) {
			pool := testPool(t)
			t.Cleanup(api.SetSSETimingsForTest(10*time.Millisecond, time.Hour, 3*time.Second))
			runID, _ := seedPausedDelegation(t, pool, status)

			h := newChatRouter(pool, &stubEnqueuer{})
			rec := httptest.NewRecorder()
			start := time.Now()
			// Last-Event-ID past the pause, so it is the settled check — not the
			// run_paused event — that has to close the stream.
			req := localRequest(http.MethodGet, "/api/runs/"+runID.String()+"/events", nil)
			req.Header.Set("Last-Event-ID", "3")
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Errorf("stream took %v to close with nothing in flight, want immediate", elapsed)
			}
		})
	}
}
