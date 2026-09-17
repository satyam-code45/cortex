package agent_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"cortex/internal/agent"
	"cortex/internal/llm"
	"cortex/internal/tools"
)

// New's guard is per concern, not per field.
//
// Provider and Registry are only the FALLBACKS used when the matching per-user
// factory is absent, so what the constructor must insist on is one of each
// pair, not both members of it. Requiring the fallback unconditionally made two
// supported deployments unconstructable:
//
//   - a BYOK server has no server-wide OpenAI key to build a Provider from
//     (every run spends its owner's key), and
//   - a server with no demo workspace has no server-wide Registry to build
//     (every run's tools come from its owner's connections).
//
// What must still be rejected is the case where a concern has neither a
// fallback nor a factory: that orchestrator would panic on its first run.

// constructorDB is an agent.DB that is never used: these tests only construct
// an Orchestrator, so reaching the database at all is the failure.
type constructorDB struct{ t *testing.T }

func (d constructorDB) Begin(context.Context) (pgx.Tx, error) {
	d.t.Error("agent.New reached the database; construction must not run a transaction")
	return nil, errors.New("constructorDB: Begin is not supported")
}

// emptyRegistry is a registry with no tools — enough to be non-nil, which is
// all the constructor looks at.
func emptyRegistry(t *testing.T) *tools.Registry {
	t.Helper()
	r, err := tools.NewRegistry()
	if err != nil {
		t.Fatalf("build empty registry: %v", err)
	}
	return r
}

func TestNewAcceptsAFactoryInPlaceOfEachFallback(t *testing.T) {
	registry := emptyRegistry(t)
	provider := newFakeProvider(t)

	providerFactory := func(context.Context, uuid.UUID) (llm.Provider, error) {
		return provider, nil
	}
	registryFactory := func(context.Context, uuid.UUID) (*tools.Registry, agent.Sources, error) {
		return registry, agent.Sources{Mode: agent.ModeUser}, nil
	}

	tests := []struct {
		name            string
		provider        llm.Provider
		registry        *tools.Registry
		providerForUser func(context.Context, uuid.UUID) (llm.Provider, error)
		registryForUser func(context.Context, uuid.UUID) (*tools.Registry, agent.Sources, error)

		wantErrContains string
	}{
		{
			// cmd/eval: one server key, one demo registry, no per-user
			// anything. Unchanged.
			name:     "both fallbacks and no factories is still accepted",
			provider: provider,
			registry: registry,
		},
		{
			// A BYOK server: every run's provider is built from its owner's
			// stored key, so there is no server-wide provider to hand over.
			name:            "a nil Provider is accepted when ProviderForUser is set",
			registry:        registry,
			providerForUser: providerFactory,
		},
		{
			// A deployment with no demo workspace: there is no server-wide
			// registry to build, because every run's tools come from its
			// owner's connections.
			name:            "a nil Registry is accepted when RegistryForUser is set",
			provider:        provider,
			registryForUser: registryFactory,
		},
		{
			// The day-11 deployment: BYOK *and* no demo workspace. This is the
			// combination the old guard made impossible to construct.
			name:            "both nil fallbacks are accepted when both factories are set",
			providerForUser: providerFactory,
			registryForUser: registryFactory,
		},
		{
			name:            "neither Provider nor ProviderForUser is rejected",
			registry:        registry,
			wantErrContains: "Provider",
		},
		{
			name:            "neither Registry nor RegistryForUser is rejected",
			provider:        provider,
			wantErrContains: "Registry",
		},
		{
			name:            "an empty config is rejected",
			wantErrContains: "Provider",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o, err := agent.New(agent.Config{
				DB:              constructorDB{t: t},
				Provider:        tt.provider,
				Registry:        tt.registry,
				ProviderForUser: tt.providerForUser,
				RegistryForUser: tt.registryForUser,
				Model:           testModel,
				Logger:          discardLogger(),
			})

			if tt.wantErrContains != "" {
				if err == nil {
					t.Fatalf("New() = %v, want an error mentioning %q", o, tt.wantErrContains)
				}
				if !strings.Contains(err.Error(), tt.wantErrContains) {
					t.Errorf("New() error = %q, want it to name %q", err, tt.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("New() error = %v, want nil", err)
			}
			if o == nil {
				t.Fatal("New() returned a nil orchestrator with a nil error")
			}
		})
	}
}

// DB and Model stay unconditionally required: neither has a per-user variant to
// stand in for it.
func TestNewStillRequiresDBAndModel(t *testing.T) {
	registry := emptyRegistry(t)
	provider := newFakeProvider(t)

	if _, err := agent.New(agent.Config{
		Provider: provider,
		Registry: registry,
		Model:    testModel,
		Logger:   discardLogger(),
	}); err == nil || !strings.Contains(err.Error(), "DB") {
		t.Errorf("New() without DB error = %v, want it to name DB", err)
	}

	if _, err := agent.New(agent.Config{
		DB:       constructorDB{t: t},
		Provider: provider,
		Registry: registry,
		Logger:   discardLogger(),
	}); err == nil || !strings.Contains(err.Error(), "Model") {
		t.Errorf("New() without Model error = %v, want it to name Model", err)
	}
}
