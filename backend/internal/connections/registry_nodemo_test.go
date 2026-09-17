package connections_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/agent"
	"cortex/internal/connections"
	"cortex/internal/tools"
)

// ForUser with a partial, absent, or un-asked-for demo workspace.
//
// Two defects closed here. A fresh account used to BE demo mode, which made the
// deployment owner's real Jira and mailbox the default identity of every new
// user; and NewRegistryBuilder used to reject a nil demo registry, which made a
// server with no demo workspace unbuildable.
//
// The contract:
//
//   - nothing connected and no demo, or nothing connected and the demo not
//     asked for → ErrNoSourcesConnected. A sentinel, NOT an empty registry that
//     runs successfully and answers out of thin air.
//   - nothing connected, the demo asked for, and a demo exists → the demo
//     registry.
//   - anything connected → that user's own sources, whether or not this
//     deployment has a demo workspace.

// newBuilderWithDemo wires a RegistryBuilder over an explicit demo registry,
// which may be nil — a deployment with no demo workspace.
func newBuilderWithDemo(t *testing.T, pool *pgxpool.Pool, demo *tools.Registry) (*connections.RegistryBuilder, *connections.Service) {
	t.Helper()
	svc := newTestService(t, pool)
	builder, err := connections.NewRegistryBuilder(connections.RegistryBuilderConfig{
		Service:            svc,
		Demo:               demo,
		GoogleClientID:     "web-client-id.apps.googleusercontent.com",
		GoogleClientSecret: "web-client-secret",
		Logger:             discardLogger(),
	})
	if err != nil {
		t.Fatalf("build registry builder (demo present: %t): %v", demo != nil, err)
	}
	return builder, svc
}

// A nil demo registry is legal. It makes demo mode unreachable; it does not
// make the server unbuildable.
func TestNewRegistryBuilderAcceptsANilDemoRegistry(t *testing.T) {
	// No database: the constructor only validates its configuration.
	builder, err := connections.NewRegistryBuilder(connections.RegistryBuilderConfig{
		Service: connections.NewService(nil, newTestCipher(t)),
		Demo:    nil,
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewRegistryBuilder with no demo registry: %v, want it to build", err)
	}
	if builder == nil {
		t.Fatal("NewRegistryBuilder returned a nil builder with a nil error")
	}

	// The Service is still required: without it there is nothing to read a
	// user's connections from.
	if _, err := connections.NewRegistryBuilder(connections.RegistryBuilderConfig{
		Logger: discardLogger(),
	}); err == nil {
		t.Error("NewRegistryBuilder without a Service returned no error")
	}
}

func TestForUserResolvesSourcesAcrossDemoConfigurations(t *testing.T) {
	// The full demo tool set, matching the stand-in newDemoRegistry builds.
	demoToolNames := []string{"gmail_search", "jira_search_issues", "notion_search", "search_knowledge_base"}

	tests := []struct {
		name string
		// demoConfigured is whether this deployment has a demo workspace at
		// all — the nil-vs-present demo registry.
		demoConfigured bool
		// connectJira is whether the user has connected a source of their own.
		connectJira bool
		// useDemo is whether the user explicitly asked for the demo workspace.
		useDemo bool

		wantErrNoSources bool
		wantMode         string
		wantConnected    []string
		wantTools        []string
	}{
		{
			// The new account on a bring-your-own-sources deployment. There is
			// nothing to search, so the run must never start: the sentinel is
			// what the chat handler turns into 409 no_sources_connected.
			name:             "nothing connected and no demo is the sentinel",
			wantErrNoSources: true,
		},
		{
			// The demo exists, but this user never asked for it. Inheriting
			// it is exactly the defect this closes.
			name:             "nothing connected and a demo not asked for is the same sentinel",
			demoConfigured:   true,
			wantErrNoSources: true,
		},
		{
			// Asking for a demo the deployment does not have cannot silently
			// succeed with an empty registry: that produces a confident answer
			// drawn from nothing.
			name:             "asking for a demo this deployment lacks is the sentinel",
			useDemo:          true,
			wantErrNoSources: true,
		},
		{
			name:           "nothing connected, demo asked for, demo exists",
			demoConfigured: true,
			useDemo:        true,
			wantMode:       agent.ModeDemo,
			wantTools:      demoToolNames,
		},
		{
			// The user's own sources answer, and the absence of a demo
			// workspace is irrelevant to them.
			name:          "a connected source answers on a deployment with no demo",
			connectJira:   true,
			wantMode:      agent.ModeUser,
			wantConnected: []string{"jira"},
			wantTools:     jiraToolNames,
		},
		{
			// ... and the presence of one is equally irrelevant: a user with
			// connections never gets demo tools mixed in.
			name:           "a connected source answers the same way when a demo exists",
			demoConfigured: true,
			connectJira:    true,
			wantMode:       agent.ModeUser,
			wantConnected:  []string{"jira"},
			wantTools:      jiraToolNames,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := testPool(t)
			var demo *tools.Registry
			if tt.demoConfigured {
				demo = newDemoRegistry(t)
			}
			builder, svc := newBuilderWithDemo(t, pool, demo)
			userID := insertUser(t, pool)

			if tt.connectJira {
				jiraSite := newFakeJiraSite(t)
				saveJira(t, svc, userID, jiraSite.server.URL, "owner@example.com", "their-token")
			}
			if tt.useDemo {
				if err := svc.SetUseDemo(context.Background(), userID, true); err != nil {
					t.Fatalf("SetUseDemo: %v", err)
				}
			}

			registry, sources, err := builder.ForUser(context.Background(), userID)

			if tt.wantErrNoSources {
				if !errors.Is(err, connections.ErrNoSourcesConnected) {
					t.Fatalf("ForUser error = %v, want ErrNoSourcesConnected", err)
				}
				if registry != nil {
					t.Errorf("ForUser returned a %d-tool registry alongside the sentinel (%v); "+
						"a run with nothing to search must not start", registry.Len(), registry.Names())
				}
				if sources.Mode != "" {
					t.Errorf("sources.Mode = %q alongside the sentinel, want the zero value: "+
						"the run never reaches run_started", sources.Mode)
				}
				return
			}

			if err != nil {
				t.Fatalf("ForUser error = %v, want nil", err)
			}
			if sources.Mode != tt.wantMode {
				t.Errorf("sources.Mode = %q, want %q", sources.Mode, tt.wantMode)
			}
			if !slices.Equal(sources.Connected, tt.wantConnected) {
				t.Errorf("sources.Connected = %v, want %v", sources.Connected, tt.wantConnected)
			}
			if got := registry.Names(); !slices.Equal(got, tt.wantTools) {
				t.Errorf("registry names = %v, want %v", got, tt.wantTools)
			}
		})
	}
}

// On a demo-less deployment the demo toggle is inert in both positions: a user
// with their own source connected answers from it whether the stale flag is on
// or off, and the run is labelled user mode either way.
func TestForUserDemoToggleOnADemolessDeployment(t *testing.T) {
	pool := testPool(t)
	builder, svc := newBuilderWithDemo(t, pool, nil)
	userID := insertUser(t, pool)
	jiraSite := newFakeJiraSite(t)
	saveJira(t, svc, userID, jiraSite.server.URL, "owner@example.com", "their-token")

	// A toggle with no demo behind it is inert, and their own Jira answers.
	//
	// The API refuses to switch the toggle ON where there is no demo, so this
	// state is reached one way: the operator removed the demo credentials from
	// a deployment whose users had already opted in, leaving the stored row
	// true. Treating it as demo mode would lock every one of those users out
	// of a source they can see connected on the Connections page — and the UI
	// hides the toggle when there is no demo, so they could not clear it.
	// Their own sources are the honest answer, reported honestly as user mode.
	if err := svc.SetUseDemo(context.Background(), userID, true); err != nil {
		t.Fatalf("SetUseDemo: %v", err)
	}
	staleRegistry, staleSources, err := builder.ForUser(context.Background(), userID)
	if err != nil {
		t.Fatalf("ForUser with a stale toggle and no demo workspace = %v, want their own sources", err)
	}
	if staleSources.Mode != agent.ModeUser {
		t.Errorf("sources.Mode = %q, want user — their own data must never be labelled demo", staleSources.Mode)
	}
	if got := staleRegistry.Names(); !slices.Equal(got, jiraToolNames) {
		t.Errorf("registry names = %v, want the jira set %v", got, jiraToolNames)
	}

	if err := svc.SetUseDemo(context.Background(), userID, false); err != nil {
		t.Fatalf("SetUseDemo(false): %v", err)
	}
	registry, sources, err := builder.ForUser(context.Background(), userID)
	if err != nil {
		t.Fatalf("ForUser after the toggle went off: %v", err)
	}
	if sources.Mode != agent.ModeUser || !slices.Equal(sources.Connected, []string{"jira"}) {
		t.Errorf("sources = %+v, want user mode with [jira]", sources)
	}
	if got := registry.Names(); !slices.Equal(got, jiraToolNames) {
		t.Errorf("registry names = %v, want the jira set %v", got, jiraToolNames)
	}
}

// HasSources answers the same question the chat handler asks before creating a
// run row, and must agree with ForUser in every configuration.
func TestHasSourcesMatchesForUser(t *testing.T) {
	tests := []struct {
		name        string
		demoSources []string
		connect     bool
		useDemo     bool
		want        bool
	}{
		{name: "fresh account, no demo workspace", want: false},
		{name: "fresh account, demo exists but not asked for", demoSources: []string{"jira"}, want: false},
		{name: "fresh account, demo asked for and it exists", demoSources: []string{"jira"}, useDemo: true, want: true},
		{name: "fresh account, demo asked for but there is none", useDemo: true, want: false},
		{name: "connected source, no demo workspace", connect: true, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := testPool(t)
			svc := connections.NewService(pool, newTestCipher(t), tt.demoSources...)
			userID := insertUser(t, pool)
			ctx := context.Background()

			if tt.connect {
				if err := svc.Save(ctx, userID, connections.SourceNotion,
					connections.NotionCredentials{Token: "ntn-token"},
					map[string]string{"bot_name": "Bot"}); err != nil {
					t.Fatalf("save notion: %v", err)
				}
			}
			if tt.useDemo {
				if err := svc.SetUseDemo(ctx, userID, true); err != nil {
					t.Fatalf("SetUseDemo: %v", err)
				}
			}

			got, err := svc.HasSources(ctx, userID)
			if err != nil {
				t.Fatalf("HasSources: %v", err)
			}
			if got != tt.want {
				t.Errorf("HasSources() = %t, want %t", got, tt.want)
			}
			if gotAvailable := svc.DemoAvailable(); gotAvailable != (len(tt.demoSources) > 0) {
				t.Errorf("DemoAvailable() = %t, want %t", gotAvailable, len(tt.demoSources) > 0)
			}
		})
	}
}
