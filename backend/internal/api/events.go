package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"cortex/internal/agent"
	"cortex/internal/auth"
	"cortex/internal/store"
)

// The SSE stream: GET /api/runs/{id}/events.
//
// The worker is already writing every step of a run to run_events; this handler
// replays that table to the browser as it grows. Polling the table — rather
// than LISTEN/NOTIFY or an in-process channel — is a deliberate trade: the
// events are durable rows either way, a 400ms poll is invisible next to
// multi-second LLM calls, and the handler works identically for a run executing
// on another process's worker.
//
// Variables rather than constants so tests can shorten them; the timings
// themselves are not configuration anyone tunes in production.
var (
	// ssePollInterval is how often the handler checks for new events.
	ssePollInterval = 400 * time.Millisecond
	// sseHeartbeatInterval is how often a comment line keeps the connection
	// from looking idle to proxies and to EventSource's reconnect logic.
	sseHeartbeatInterval = 15 * time.Second
	// sseMaxStreamAge caps a stream that never reaches a terminal event — an
	// abandoned run must not hold a connection and a poll loop forever.
	sseMaxStreamAge = 5 * time.Minute
)

// handleRunEvents streams a run's events as Server-Sent Events.
//
// GET /api/runs/{id}/events → text/event-stream of
//
//	id: <seq>
//	event: <type>
//	data: <payload JSON>
//
// The id field carries the run_events seq, which is what makes reconnects
// cheap: EventSource re-sends the last id it saw as the Last-Event-ID header,
// and the stream resumes from seq > that instead of replaying the transcript.
//
// The stream closes after run_finished or run_failed — the transcript is
// complete then and the browser switches to GET /api/runs/{id}/trace.
func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	logger := s.deps.Logger

	runID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, logger, http.StatusBadRequest, "run id must be a UUID")
		return
	}

	// Ownership before any SSE headers, exactly as in handleGetRun: the stream
	// exposes the full transcript, so an unknown or foreign run must 404 as a
	// plain JSON error while that is still possible.
	q := store.New(s.deps.DB)
	user, ok := auth.UserFrom(r.Context())
	if !ok {
		writeError(w, logger, http.StatusInternalServerError, "failed to open event stream")
		return
	}
	run, err := q.GetAgentRunForUser(r.Context(), store.GetAgentRunForUserParams{
		ID:     runID,
		UserID: user.ID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, logger, http.StatusNotFound, "run not found")
			return
		}
		logger.Error("events: failed to load run", "run_id", runID, "error", err)
		writeError(w, logger, http.StatusInternalServerError, "failed to open event stream")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		// Streaming needs per-event flushes; without them every event would sit
		// in the buffer until the run ended, which is polling with extra steps.
		logger.Error("events: response writer does not support flushing")
		writeError(w, logger, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	lastSeq := lastEventID(r)

	// A run that is already terminal can gain no more events: the terminal
	// event and the status commit in one transaction, and StartAgentRun refuses
	// to restart a terminal run. So this stream drains what exists and closes —
	// without this, a reconnect that had already seen the terminal event
	// (lastSeq past it) would idle against a finished run until the 5-min cap.
	runTerminal := run.Status == "completed" || run.Status == "failed"

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	// Tells nginx-style proxies not to buffer the stream; harmless elsewhere.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// The cap is on the stream, not the run: hitting it closes this connection,
	// and a client that still cares reconnects with Last-Event-ID.
	ctx, cancel := context.WithTimeout(r.Context(), sseMaxStreamAge)
	defer cancel()

	poll := time.NewTicker(ssePollInterval)
	defer poll.Stop()
	heartbeat := time.NewTicker(sseHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		events, err := q.ListRunEventsByRunAfterSeq(ctx, store.ListRunEventsByRunAfterSeqParams{
			AgentRunID: runID,
			Seq:        lastSeq,
		})
		if err != nil {
			// The status line is long gone, so an error can only end the
			// stream; the client's reconnect will surface a real HTTP error if
			// the problem persists.
			if ctx.Err() == nil {
				logger.Error("events: failed to list run events", "run_id", runID, "error", err)
			}
			return
		}
		for _, event := range events {
			if err := writeSSEEvent(w, event); err != nil {
				logger.Warn("events: client write failed", "run_id", runID, "error", err)
				return
			}
			lastSeq = event.Seq
			if event.Type == agent.EventRunFinished || event.Type == agent.EventRunFailed {
				flusher.Flush()
				return
			}
		}
		if len(events) > 0 {
			flusher.Flush()
		}
		if runTerminal {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-poll.C:
		case <-heartbeat.C:
			// A comment line: ignored by EventSource, but traffic on the wire.
			if _, err := fmt.Fprint(w, ": hb\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// lastEventID reads the seq the client saw last, from the Last-Event-ID header
// EventSource sends on reconnect. Zero — start from the beginning — for a first
// connect or an unparseable value.
func lastEventID(r *http.Request) int32 {
	raw := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if raw == "" {
		return 0
	}
	seq, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || seq < 0 {
		return 0
	}
	return int32(seq)
}

// writeSSEEvent renders one run_events row in the SSE wire format.
//
// Each data line is prefixed separately: the payload is jsonb and jsonb never
// contains a raw newline, but the wire format must stay correct even if that
// assumption breaks, because a stray newline inside a data field desynchronizes
// every event after it.
func writeSSEEvent(w http.ResponseWriter, event store.RunEvent) error {
	var b strings.Builder
	fmt.Fprintf(&b, "id: %d\n", event.Seq)
	fmt.Fprintf(&b, "event: %s\n", event.Type)
	for _, line := range strings.Split(string(event.Payload), "\n") {
		fmt.Fprintf(&b, "data: %s\n", line)
	}
	b.WriteString("\n")
	_, err := fmt.Fprint(w, b.String())
	return err
}
