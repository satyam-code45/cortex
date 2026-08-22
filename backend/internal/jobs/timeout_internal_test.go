package jobs

import (
	"testing"
	"time"
)

// TestJobTimeoutIsNotRiverDefault guards the fix for the 60-second cliff.
//
// River cancels a job's context at JobTimeout and its default is one minute —
// shorter than a legitimate multi-hop run. That made deep investigations fail
// with "llm request timed out" while shallow ones passed, so the bug read as an
// OpenAI problem rather than a queue setting. If the explicit value is ever
// dropped, River silently reinstates its own default and the failure returns.
func TestJobTimeoutIsNotRiverDefault(t *testing.T) {
	const riverDefault = time.Minute
	if jobTimeout <= riverDefault {
		t.Fatalf("jobTimeout = %s, must exceed River's %s default", jobTimeout, riverDefault)
	}
	// One generation is allowed 90s and a run may make a dozen of them, so the
	// clock must not be what stops a legitimate investigation.
	if want := 10 * time.Minute; jobTimeout < want {
		t.Errorf("jobTimeout = %s, want at least %s to fit a full-length run", jobTimeout, want)
	}
}
