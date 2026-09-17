package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/api"
	"cortex/internal/connections"
	"cortex/internal/keys"
)

// Onboarding an account that has connected nothing.
//
// Two halves of the same contract:
//
//   - POST /api/chat with nothing to search is 409 {"error":"no_sources_connected"}
//     — the same shape as the llm_key_required conflict, so the frontend routes
//     it the same way — and NOTHING is created: no conversation, no run row, no job. A
//     run that exists only to fail is worse than no run: it puts a failure in the
//     user's history for an account that is merely new.
//   - GET /api/connections reports demo_available truthfully, because a
//     deployment with no demo workspace must not advertise a mode it cannot
//     enter.

// newSourcesRouter wires a router whose connections service knows exactly which
// demo sources this deployment offers. No demoSources means no demo workspace.
func newSourcesRouter(t *testing.T, db api.DB, enqueuer api.Enqueuer, demoSources ...string) http.Handler {
	t.Helper()
	cipher, err := keys.NewCipher(testKeyEncryptionSecret)
	if err != nil {
		t.Fatalf("build cipher: %v", err)
	}
	return api.NewRouter(withTestAuth(api.Deps{
		DB:          db,
		Enqueuer:    enqueuer,
		Model:       testModel,
		Logger:      discardLogger(),
		Keys:        newTestKeysService(t, db),
		Connections: connections.NewService(db, cipher, demoSources...),
	}))
}

// connectNotion stores a working-looking notion connection for the acting user,
// bypassing the paste endpoint (which would need a live provider fake).
func connectNotion(t *testing.T, pool *pgxpool.Pool, email string) {
	t.Helper()
	cipher, err := keys.NewCipher(testKeyEncryptionSecret)
	if err != nil {
		t.Fatalf("build cipher: %v", err)
	}
	svc := connections.NewService(pool, cipher)
	var userID uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM users WHERE email = $1`, email).Scan(&userID); err != nil {
		t.Fatalf("look up %s: %v", email, err)
	}
	if err := svc.Save(context.Background(), userID, connections.SourceNotion,
		connections.NotionCredentials{Token: "ntn-users-own-token"},
		map[string]string{"bot_name": "Cortex Bot"}); err != nil {
		t.Fatalf("save notion connection: %v", err)
	}
}

// forceUseDemo sets the demo toggle directly on the row, bypassing the API.
//
// The API refuses to switch the toggle on where there is no demo workspace, so
// this is the only way to reach a state that a real deployment reaches by a
// different route: the operator removed the demo credentials from a deployment
// whose users had already opted in, leaving the stored flag true.
func forceUseDemo(t *testing.T, pool *pgxpool.Pool, email string) {
	t.Helper()
	cipher, err := keys.NewCipher(testKeyEncryptionSecret)
	if err != nil {
		t.Fatalf("build cipher: %v", err)
	}
	var userID uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM users WHERE email = $1`, email).Scan(&userID); err != nil {
		t.Fatalf("look up %s: %v", email, err)
	}
	if err := connections.NewService(pool, cipher).SetUseDemo(context.Background(), userID, true); err != nil {
		t.Fatalf("force the demo toggle on: %v", err)
	}
}

func TestChatWithNoSourcesIs409NoSourcesConnected(t *testing.T) {
	tests := []struct {
		name string
		// demoSources is what the deployment offers; empty is no demo
		// workspace.
		demoSources []string
		// useDemo is whether the user asked for the demo workspace, through
		// the API as a real user would.
		useDemo bool
		// staleDemoToggle sets the flag directly, for the one state the API
		// will not produce: opted in, then the demo went away.
		staleDemoToggle bool
		// connect is whether the user has a source of their own.
		connect bool

		want409 bool
	}{
		{
			// The acceptance case: a fresh Google account on a
			// bring-your-own-sources deployment.
			name:    "fresh account on a deployment with no demo workspace",
			want409: true,
		},
		{
			// The demo exists but was never asked for. This used to run
			// happily against the deployment owner's real Jira and mailbox.
			name:        "fresh account on a deployment that has a demo it did not ask for",
			demoSources: []string{"jira", "notion", "gmail"},
			want409:     true,
		},
		{
			// A stale toggle with nothing behind it and nothing of their own.
			// The flag is inert, so this is the fresh-account case again.
			name:            "a stale demo toggle on a deployment that has none",
			staleDemoToggle: true,
			want409:         true,
		},
		{
			// The same stale flag, but they have their own source. It must not
			// lock them out of a connection they can see on the Connections
			// page — especially as the UI hides the toggle where there is no
			// demo, so they could not clear it themselves.
			name:            "a stale demo toggle does not strand a user who has their own source",
			staleDemoToggle: true,
			connect:         true,
		},
		{
			name:        "demo asked for where one exists",
			demoSources: []string{"jira"},
			useDemo:     true,
		},
		{
			name:    "a connected source of their own, no demo workspace",
			connect: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := testPool(t)
			enqueuer := &stubEnqueuer{}
			h := newSourcesRouter(t, pool, enqueuer, tt.demoSources...)
			giveLLMKey(t, pool, devUserEmail)

			if tt.connect {
				connectNotion(t, pool, devUserEmail)
			}
			if tt.useDemo {
				rec := putJSON(t, h, "/api/connections/mode", `{"use_demo_workspace":true}`)
				if rec.Code != http.StatusOK {
					t.Fatalf("PUT mode = %d (body %q)", rec.Code, rec.Body.String())
				}
			}
			if tt.staleDemoToggle {
				forceUseDemo(t, pool, devUserEmail)
			}

			rec := postChat(t, h, `{"message":"who blocked ATLAS-1?"}`)

			if !tt.want409 {
				if rec.Code != http.StatusAccepted {
					t.Fatalf("POST /api/chat = %d, want 202 (body %q)", rec.Code, rec.Body.String())
				}
				return
			}

			if rec.Code != http.StatusConflict {
				t.Fatalf("POST /api/chat = %d, want 409 (body %q)", rec.Code, rec.Body.String())
			}
			var body struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode 409 body %q: %v", rec.Body.String(), err)
			}
			if body.Error != "no_sources_connected" {
				t.Errorf(`error = %q, want "no_sources_connected" (the frontend routes on this exact string)`, body.Error)
			}

			// Nothing was created. A 409 is an onboarding state, not a failed
			// run.
			if n := queryInt(t, pool, `SELECT count(*) FROM agent_runs`); n != 0 {
				t.Errorf("agent_runs = %d after the 409, want 0", n)
			}
			if n := queryInt(t, pool, `SELECT count(*) FROM conversations`); n != 0 {
				t.Errorf("conversations = %d after the 409, want 0", n)
			}
			if n := queryInt(t, pool, `SELECT count(*) FROM messages`); n != 0 {
				t.Errorf("messages = %d after the 409, want 0", n)
			}
			if enqueuer.callCount() != 0 {
				t.Errorf("runs enqueued = %d after the 409, want 0", enqueuer.callCount())
			}
		})
	}
}

// Switching the demo toggle ON needs a demo to switch on to.
//
// The UI only renders the control where one exists, but the API is what
// decides: a stored true on a deployment with no demo is the stale row that
// used to leave a user unable to chat and unable to see the control that would
// have fixed it. Switching OFF is always allowed — withdrawing from something
// is never blocked by that thing having gone away.
func TestPutConnectionsModeRefusesADemoThatDoesNotExist(t *testing.T) {
	t.Run("on is refused where there is no demo workspace", func(t *testing.T) {
		pool := testPool(t)
		h := newSourcesRouter(t, pool, &stubEnqueuer{})

		rec := putJSON(t, h, "/api/connections/mode", `{"use_demo_workspace":true}`)
		if rec.Code != http.StatusConflict {
			t.Fatalf("PUT mode = %d, want 409 (body %q)", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "no demo workspace") {
			t.Errorf("body %q does not explain that there is no demo workspace", rec.Body.String())
		}
	})

	t.Run("on is accepted where there is one", func(t *testing.T) {
		pool := testPool(t)
		h := newSourcesRouter(t, pool, &stubEnqueuer{}, "jira")

		if rec := putJSON(t, h, "/api/connections/mode", `{"use_demo_workspace":true}`); rec.Code != http.StatusOK {
			t.Fatalf("PUT mode = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
	})

	t.Run("off is always accepted, even with no demo workspace", func(t *testing.T) {
		pool := testPool(t)
		h := newSourcesRouter(t, pool, &stubEnqueuer{})
		giveLLMKey(t, pool, devUserEmail)
		forceUseDemo(t, pool, devUserEmail)

		if rec := putJSON(t, h, "/api/connections/mode", `{"use_demo_workspace":false}`); rec.Code != http.StatusOK {
			t.Fatalf("PUT mode off = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
	})
}

// indexing_available is a different question from demo_available, and the
// frontend gates a different thing on each.
//
// Indexing needs BOTH a demo workspace to crawl AND the server's own OpenAI key
// to embed it with. A demo configured without that key therefore reports
// demo_available true and indexing_available false — and that is the
// configuration where gating the Sources nav on the wrong one of the two puts a
// tab in front of a user whose every refresh answers 503.
func TestGetConnectionsReportsIndexingSeparatelyFromTheDemo(t *testing.T) {
	newRouter := func(t *testing.T, pool *pgxpool.Pool, indexSources []string, demoSources ...string) http.Handler {
		t.Helper()
		cipher, err := keys.NewCipher(testKeyEncryptionSecret)
		if err != nil {
			t.Fatalf("build cipher: %v", err)
		}
		return api.NewRouter(withTestAuth(api.Deps{
			DB:           pool,
			Enqueuer:     &stubEnqueuer{},
			Model:        testModel,
			Logger:       discardLogger(),
			Keys:         newTestKeysService(t, pool),
			Connections:  connections.NewService(pool, cipher, demoSources...),
			IndexSources: indexSources,
		}))
	}

	read := func(t *testing.T, h http.Handler) (demo, indexing bool) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, localRequest(http.MethodGet, "/api/connections", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/connections = %d (body %q)", rec.Code, rec.Body.String())
		}
		var body struct {
			DemoAvailable     bool `json:"demo_available"`
			IndexingAvailable bool `json:"indexing_available"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %q: %v", rec.Body.String(), err)
		}
		return body.DemoAvailable, body.IndexingAvailable
	}

	t.Run("a demo with an index reports both", func(t *testing.T) {
		pool := testPool(t)
		demo, indexing := read(t, newRouter(t, pool, []string{"jira"}, "jira"))
		if !demo || !indexing {
			t.Errorf("demo_available = %v, indexing_available = %v, want both true", demo, indexing)
		}
	})

	t.Run("a demo with no server key has no index", func(t *testing.T) {
		pool := testPool(t)
		demo, indexing := read(t, newRouter(t, pool, nil, "jira"))
		if !demo {
			t.Error("demo_available = false, want true — the demo sources are configured")
		}
		if indexing {
			t.Error("indexing_available = true, want false — there is no corpus to browse")
		}
	})

	t.Run("no demo workspace has neither", func(t *testing.T) {
		pool := testPool(t)
		demo, indexing := read(t, newRouter(t, pool, nil))
		if demo || indexing {
			t.Errorf("demo_available = %v, indexing_available = %v, want both false", demo, indexing)
		}
	})
}

func TestGetConnectionsReportsDemoAvailabilityTruthfully(t *testing.T) {
	tests := []struct {
		name        string
		demoSources []string

		wantAvailable bool
		wantSources   []string
	}{
		{
			name:          "no demo workspace configured",
			wantAvailable: false,
		},
		{
			name:          "a partial demo workspace names only what it covers",
			demoSources:   []string{"jira", "notion"},
			wantAvailable: true,
			wantSources:   []string{"jira", "notion"},
		},
		{
			name:          "a full demo workspace",
			demoSources:   []string{"jira", "notion", "gmail"},
			wantAvailable: true,
			wantSources:   []string{"jira", "notion", "gmail"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := testPool(t)
			h := newSourcesRouter(t, pool, &stubEnqueuer{}, tt.demoSources...)

			_, overview := getConnections(t, h)

			if overview.DemoAvailable != tt.wantAvailable {
				t.Errorf("demo_available = %t, want %t", overview.DemoAvailable, tt.wantAvailable)
			}
			if !slices.Equal(overview.DemoSources, tt.wantSources) {
				t.Errorf("demo_sources = %v, want %v", overview.DemoSources, tt.wantSources)
			}
			// A fresh account is in neither demo nor user mode: it has
			// connected nothing and asked for nothing.
			if overview.Mode != "none" {
				t.Errorf("mode for a fresh account = %q, want none", overview.Mode)
			}
			if overview.UseDemoWorkspace {
				t.Error("use_demo_workspace is on for a fresh account; the demo is entered on purpose, never inherited")
			}
		})
	}
}
