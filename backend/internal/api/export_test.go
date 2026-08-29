package api

import "time"

// SetSSETimingsForTest overrides the SSE loop timings and returns a restore
// func for t.Cleanup. The timings are package-level vars precisely so tests
// can shorten them (spec day-5, REQ-5.1 / TEST-5.1); this file is the only
// door into them from the external api_test package, and it compiles only
// under `go test`.
//
// Tests that call this must not run in parallel: the vars are shared state.
func SetSSETimingsForTest(poll, heartbeat, maxAge time.Duration) (restore func()) {
	prevPoll := ssePollInterval
	prevHeartbeat := sseHeartbeatInterval
	prevMaxAge := sseMaxStreamAge
	ssePollInterval, sseHeartbeatInterval, sseMaxStreamAge = poll, heartbeat, maxAge
	return func() {
		ssePollInterval, sseHeartbeatInterval, sseMaxStreamAge = prevPoll, prevHeartbeat, prevMaxAge
	}
}
