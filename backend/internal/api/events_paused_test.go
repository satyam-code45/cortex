package api_test

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"cortex/internal/api"
)

// The pause closes the stream.
//
// A paused run is not finished, but nothing more will be written to it until a
// person decides — which can take hours, far longer than the five-minute stream
// cap, and much longer than a connection and a poll loop should be held open
// for. So run_paused is a stream-closing event and an already-paused run drains
// and closes, rather than idling until the cap and then looking like a failure.
//
// The browser keeps its approval cards on screen and opens a fresh stream after
// posting a decision. That stream replays from the start, which is harmless
// because the client dedupes by seq — and is asserted here, because a replay
// that skipped the pre-pause events would leave a reloaded page with no
// transcript.

// The events the approval UI is built on reach the
// client, and run_paused ends the stream.
func TestRunEventsClosesOnRunPaused(t *testing.T) {
	pool := testPool(t)
	t.Cleanup(api.SetSSETimingsForTest(15*time.Millisecond, time.Hour, 10*time.Second))

	_, runID := seedRunForUser(t, pool, devUserEmail, "running", nil, nil)
	insertRunEvent(t, pool, runID, 1, "run_started", `{"query":"reply to Ines"}`)

	srv := httptest.NewServer(newChatRouter(pool, &stubEnqueuer{}))
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/runs/"+runID.String()+"/events", nil)
	if err != nil {
		t.Fatalf("build events request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testAPIToken)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	br := bufio.NewReader(resp.Body)
	if f, err := readSSEFrame(br); err != nil || f.event != "run_started" {
		t.Fatalf("frame 1 = %+v, err %v; want run_started", f, err)
	}

	// The proposal reaches the client, carrying what the card renders.
	insertRunEvent(t, pool, runID, 2, "action_proposed",
		`{"action_id":"11111111-1111-4111-8111-111111111111","source":"gmail","action":"gmail.send",`+
			`"summary":"send an email to ines@example.com","payload":{"subject":"Sandbox notice"}}`)
	f2, err := readSSEFrame(br)
	if err != nil {
		t.Fatalf("read the proposal frame: %v", err)
	}
	if f2.event != "action_proposed" {
		t.Fatalf("frame 2 = %q, want action_proposed", f2.event)
	}
	assertPayloadField(t, f2, "action", "gmail.send")

	insertRunEvent(t, pool, runID, 3, "run_paused",
		`{"iteration":1,"waiting":[{"action_id":"11111111-1111-4111-8111-111111111111"}]}`)
	f3, err := readSSEFrame(br)
	if err != nil {
		t.Fatalf("read the pause frame: %v", err)
	}
	if f3.event != "run_paused" {
		t.Fatalf("frame 3 = %q, want run_paused", f3.event)
	}

	// And that is the end of the stream: a human decision cannot be waited for
	// on an open connection.
	if extra, err := readSSEFrame(br); !errors.Is(err, io.EOF) {
		t.Errorf("after run_paused got frame %+v, err %v; want EOF", extra, err)
	}
}

// A run already sitting in awaiting_approval drains its transcript and closes
// immediately: a client that reloads the page while a proposal is pending gets
// the whole story back and no hanging request.
func TestRunEventsAnAwaitingApprovalRunDrainsAndCloses(t *testing.T) {
	pool := testPool(t)
	t.Cleanup(api.SetSSETimingsForTest(10*time.Millisecond, time.Hour, 3*time.Second))

	_, runID := seedRunForUser(t, pool, devUserEmail, "awaiting_approval", nil, nil)
	insertRunEvent(t, pool, runID, 1, "run_started", `{}`)
	insertRunEvent(t, pool, runID, 2, "action_proposed", `{"action":"gmail.send"}`)
	insertRunEvent(t, pool, runID, 3, "run_paused", `{"waiting":[]}`)

	h := newChatRouter(pool, &stubEnqueuer{})
	rec := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(rec, localRequest(http.MethodGet, "/api/runs/"+runID.String()+"/events", nil))
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if elapsed > time.Second {
		t.Errorf("a paused run's stream took %v to close; a decision can take hours, so it must "+
			"not be waited for on an open connection", elapsed)
	}
	frames := readAllSSEFrames(t, rec.Body)
	if len(frames) != 3 {
		t.Fatalf("frames = %+v, want the three stored events replayed in full", frames)
	}
	if frames[0].event != "run_started" || frames[2].event != "run_paused" {
		t.Errorf("frames = %+v, want the transcript from the start through the pause", frames)
	}
}
