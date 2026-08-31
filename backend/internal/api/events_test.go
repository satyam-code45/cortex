package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/api"
)

// The SSE endpoint GET /api/runs/{id}/events.
//
// The wire contract under test:
//
//	id: <seq>
//	event: <type>
//	data: <payload JSON>
//
// events arrive in seq order, the stream closes after run_finished/run_failed,
// Last-Event-ID resumes from seq > that value (A4), a run that is already
// terminal drains and closes instead of idling, and an unknown or foreign run
// 404s as plain JSON before any SSE headers (A5).

// insertRunEvent seeds one run_events row directly; the handler under test
// only ever reads that table.
func insertRunEvent(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID, seq int, eventType, payload string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO run_events (agent_run_id, seq, type, payload) VALUES ($1, $2, $3, $4)`,
		runID, seq, eventType, payload); err != nil {
		t.Fatalf("insert run_event seq %d: %v", seq, err)
	}
}

// sseFrame is one parsed SSE event.
type sseFrame struct {
	id    string
	event string
	data  string
}

// readSSEFrame reads the next event from an SSE stream, skipping heartbeat
// comment lines. It returns io.EOF once the stream has closed cleanly.
func readSSEFrame(br *bufio.Reader) (sseFrame, error) {
	var f sseFrame
	var dataLines []string
	started := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if err == io.EOF && started {
				return sseFrame{}, fmt.Errorf("stream ended mid-frame: %+v", f)
			}
			return sseFrame{}, err
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case line == "":
			if started {
				f.data = strings.Join(dataLines, "\n")
				return f, nil
			}
			// A blank after a comment-only block: not a frame, keep reading.
		case strings.HasPrefix(line, ":"):
			// Heartbeat comment; EventSource ignores these and so do we.
		case strings.HasPrefix(line, "id: "):
			f.id = strings.TrimPrefix(line, "id: ")
			started = true
		case strings.HasPrefix(line, "event: "):
			f.event = strings.TrimPrefix(line, "event: ")
			started = true
		case strings.HasPrefix(line, "data: "):
			dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
			started = true
		default:
			return sseFrame{}, fmt.Errorf("unexpected SSE line %q", line)
		}
	}
}

// readAllSSEFrames drains a finished stream into its frames.
func readAllSSEFrames(t *testing.T, r io.Reader) []sseFrame {
	t.Helper()
	br := bufio.NewReader(r)
	var frames []sseFrame
	for {
		f, err := readSSEFrame(br)
		if errors.Is(err, io.EOF) {
			return frames
		}
		if err != nil {
			t.Fatalf("parse SSE stream: %v (after %d frames)", err, len(frames))
		}
		frames = append(frames, f)
	}
}

// assertPayloadField decodes a frame's data as JSON and checks one field —
// jsonb re-formats whitespace, so raw string comparison would be brittle.
func assertPayloadField(t *testing.T, f sseFrame, field, want string) {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(f.data), &payload); err != nil {
		t.Fatalf("event %s data %q is not JSON: %v", f.id, f.data, err)
	}
	if got, _ := payload[field].(string); got != want {
		t.Errorf("event %s payload[%q] = %q, want %q", f.id, field, got, want)
	}
}

// A running run whose events appear while the client is connected: each event
// must be delivered (in order, with seq as the id), and run_finished must end
// the stream. This is the live path the trace panel depends on, so it uses a
// real HTTP server — a ResponseRecorder cannot observe incremental flushes.
func TestRunEventsStreamsLiveAndClosesOnRunFinished(t *testing.T) {
	pool := testPool(t)
	t.Cleanup(api.SetSSETimingsForTest(15*time.Millisecond, time.Hour, 10*time.Second))

	_, runID := seedRunForUser(t, pool, devUserEmail, "running", nil, nil)
	insertRunEvent(t, pool, runID, 1, "run_started", `{"query":"why is ATLAS-1 blocked?"}`)

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
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/event-stream")
	}

	br := bufio.NewReader(resp.Body)

	// Frame 1 was on disk before the client connected.
	f1, err := readSSEFrame(br)
	if err != nil {
		t.Fatalf("read frame 1: %v", err)
	}
	if f1.id != "1" || f1.event != "run_started" {
		t.Fatalf("frame 1 = {id:%q event:%q}, want {id:1 event:run_started}", f1.id, f1.event)
	}
	assertPayloadField(t, f1, "query", "why is ATLAS-1 blocked?")

	// Frame 2 is inserted only after frame 1 arrived: receiving it proves the
	// handler keeps polling a live run rather than replaying a snapshot.
	insertRunEvent(t, pool, runID, 2, "tool_call_started", `{"tool":"jira_search"}`)
	f2, err := readSSEFrame(br)
	if err != nil {
		t.Fatalf("read frame 2: %v", err)
	}
	if f2.id != "2" || f2.event != "tool_call_started" {
		t.Fatalf("frame 2 = {id:%q event:%q}, want {id:2 event:tool_call_started}", f2.id, f2.event)
	}
	assertPayloadField(t, f2, "tool", "jira_search")

	insertRunEvent(t, pool, runID, 3, "run_finished", `{"status":"completed"}`)
	f3, err := readSSEFrame(br)
	if err != nil {
		t.Fatalf("read frame 3: %v", err)
	}
	if f3.id != "3" || f3.event != "run_finished" {
		t.Fatalf("frame 3 = {id:%q event:%q}, want {id:3 event:run_finished}", f3.id, f3.event)
	}

	// run_finished ends the stream: nothing but EOF may follow.
	if extra, err := readSSEFrame(br); !errors.Is(err, io.EOF) {
		t.Errorf("after run_finished got frame %+v, err %v; want EOF", extra, err)
	}
}

// A completed run replays its transcript and honors Last-Event-ID (A4): the
// stream resumes from seq > the header value instead of replaying everything,
// and an unparseable header falls back to the full replay.
func TestRunEventsReplayAndResumeFromLastEventID(t *testing.T) {
	pool := testPool(t)
	t.Cleanup(api.SetSSETimingsForTest(10*time.Millisecond, time.Hour, 3*time.Second))

	_, runID := seedRunForUser(t, pool, devUserEmail, "completed", nil, nil)
	transcript := []struct {
		seq       int
		eventType string
	}{
		{1, "run_started"},
		{2, "llm_call"},
		{3, "tool_call_started"},
		{4, "tool_call_finished"},
		{5, "run_finished"},
	}
	for _, e := range transcript {
		insertRunEvent(t, pool, runID, e.seq, e.eventType, fmt.Sprintf(`{"seq":%d}`, e.seq))
	}
	h := newChatRouter(pool, &stubEnqueuer{})

	tests := []struct {
		name        string
		lastEventID string
		wantIDs     []string
		wantEvents  []string
	}{
		{
			name:       "no header replays the full transcript in seq order",
			wantIDs:    []string{"1", "2", "3", "4", "5"},
			wantEvents: []string{"run_started", "llm_call", "tool_call_started", "tool_call_finished", "run_finished"},
		},
		{
			name:        "resume after seq 3 delivers only what follows",
			lastEventID: "3",
			wantIDs:     []string{"4", "5"},
			wantEvents:  []string{"tool_call_finished", "run_finished"},
		},
		{
			name:        "resume past the terminal event closes with nothing",
			lastEventID: "5",
			wantIDs:     nil,
			wantEvents:  nil,
		},
		{
			name:        "garbage header falls back to the full replay",
			lastEventID: "not-a-number",
			wantIDs:     []string{"1", "2", "3", "4", "5"},
			wantEvents:  []string{"run_started", "llm_call", "tool_call_started", "tool_call_finished", "run_finished"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := localRequest(http.MethodGet, "/api/runs/"+runID.String()+"/events", nil)
			if tt.lastEventID != "" {
				req.Header.Set("Last-Event-ID", tt.lastEventID)
			}
			rec := httptest.NewRecorder()
			start := time.Now()
			h.ServeHTTP(rec, req)
			elapsed := time.Since(start)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
			}
			// A terminal run must drain and close, not idle until the cap.
			if elapsed > time.Second {
				t.Errorf("stream on a terminal run took %v to close; want immediate", elapsed)
			}

			frames := readAllSSEFrames(t, rec.Body)
			var gotIDs, gotEvents []string
			for _, f := range frames {
				gotIDs = append(gotIDs, f.id)
				gotEvents = append(gotEvents, f.event)
			}
			if fmt.Sprint(gotIDs) != fmt.Sprint(tt.wantIDs) {
				t.Errorf("ids = %v, want %v", gotIDs, tt.wantIDs)
			}
			if fmt.Sprint(gotEvents) != fmt.Sprint(tt.wantEvents) {
				t.Errorf("events = %v, want %v", gotEvents, tt.wantEvents)
			}
		})
	}
}

// A run that is already terminal but whose stored events carry no terminal
// event (only possible mid-write or after manual surgery, but the handler
// promises it) still drains and closes immediately instead of polling a run
// that can gain no more rows.
func TestRunEventsTerminalRunDrainsAndClosesWithoutTerminalEvent(t *testing.T) {
	pool := testPool(t)
	t.Cleanup(api.SetSSETimingsForTest(10*time.Millisecond, time.Hour, 3*time.Second))

	_, runID := seedRunForUser(t, pool, devUserEmail, "failed", nil, nil)
	insertRunEvent(t, pool, runID, 1, "run_started", `{}`)
	insertRunEvent(t, pool, runID, 2, "llm_call", `{}`)

	h := newChatRouter(pool, &stubEnqueuer{})
	req := localRequest(http.MethodGet, "/api/runs/"+runID.String()+"/events", nil)
	rec := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if elapsed > time.Second {
		t.Errorf("terminal run stream took %v to close; want immediate drain", elapsed)
	}
	frames := readAllSSEFrames(t, rec.Body)
	if len(frames) != 2 || frames[0].id != "1" || frames[1].id != "2" {
		t.Errorf("frames = %+v, want the two stored events in order", frames)
	}
}

// Unknown and foreign runs 404 as plain JSON before any SSE headers (A5): the
// stream is the full transcript, so ownership gates it exactly like
// GET /api/runs/{id}. A malformed id is a 400, not a 500.
func TestRunEventsRejectsBadUnknownAndForeignRuns(t *testing.T) {
	pool := testPool(t)
	_, foreignRunID := seedRunForUser(t, pool, "someone-else@cortex.test", "running", nil, nil)
	h := newChatRouter(pool, &stubEnqueuer{})

	tests := []struct {
		name       string
		id         string
		wantStatus int
	}{
		{name: "malformed id", id: "not-a-uuid", wantStatus: http.StatusBadRequest},
		{name: "unknown run", id: uuid.NewString(), wantStatus: http.StatusNotFound},
		{name: "another user's run", id: foreignRunID.String(), wantStatus: http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := localRequest(http.MethodGet, "/api/runs/"+tt.id+"/events", nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			// The refusal happens before streaming starts: JSON error, not SSE.
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json (no SSE headers before the ownership check)", ct)
			}
			var body struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error == "" {
				t.Errorf("body %q is not a JSON error", rec.Body.String())
			}
		})
	}
}
