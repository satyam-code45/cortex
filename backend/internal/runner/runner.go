// Package runner performs its owner's approved Claude Code delegations. It
// polls instead of joining River, whose leader election would let a laptop
// stall the server's periodic jobs; execution goes through the shared Perform.
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"cortex/internal/actions"
	"cortex/internal/store"
	"cortex/internal/tools/claudecode"
)

// Performer is ExecuteWriteWorker.Perform.
type Performer interface {
	Perform(ctx context.Context, actionID uuid.UUID, attempt int) error
}

// Config configures a Runner.
type Config struct {
	Queries       store.Querier
	Performer     Performer
	UserID        uuid.UUID
	Name          string
	Repos         []string
	ClaudeVersion string
	// PollInterval defaults to 5s, HeartbeatInterval to 30s.
	PollInterval      time.Duration
	HeartbeatInterval time.Duration
	Logger            *slog.Logger
}

// Runner is one machine's delegation loop.
type Runner struct{ cfg Config }

// New validates cfg.
func New(cfg Config) (*Runner, error) {
	switch {
	case cfg.Queries == nil:
		return nil, errors.New("runner: Queries is required")
	case cfg.Performer == nil:
		return nil, errors.New("runner: Performer is required")
	case cfg.UserID == uuid.Nil:
		return nil, errors.New("runner: UserID is required")
	case cfg.Name == "":
		return nil, errors.New("runner: Name is required")
	case len(cfg.Repos) == 0:
		return nil, errors.New("runner: at least one repo is required")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = 30 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Runner{cfg: cfg}, nil
}

// Run heartbeats and performs delegations until ctx ends, then takes the
// runner offline so the tool stops being offered.
func (r *Runner) Run(ctx context.Context) error {
	if err := r.Heartbeat(ctx); err != nil {
		return err
	}
	// Deferred first so it runs last: the heartbeat goroutine is gone before
	// the row is deleted, so a late beat cannot bring the runner back online.
	var beats sync.WaitGroup
	defer func() {
		beats.Wait()
		offline, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := r.cfg.Queries.DeleteCodeRunner(offline, store.DeleteCodeRunnerParams{
			UserID: r.cfg.UserID, Name: r.cfg.Name,
		}); err != nil {
			r.cfg.Logger.Warn("could not take the runner offline", "error", err)
		}
	}()
	if err := r.RecoverInterrupted(ctx); err != nil {
		return err
	}

	// In the background: one delegation can block the poll loop for its run.
	beatCtx, stopBeats := context.WithCancel(ctx)
	defer stopBeats()
	beats.Add(1)
	go func() {
		defer beats.Done()
		r.heartbeatLoop(beatCtx)
	}()

	poll := time.NewTicker(r.cfg.PollInterval)
	defer poll.Stop()
	for {
		if err := r.PollOnce(ctx); err != nil && ctx.Err() == nil {
			r.cfg.Logger.Error("poll failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-poll.C:
		}
	}
}

func (r *Runner) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(r.cfg.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.Heartbeat(ctx); err != nil && ctx.Err() == nil {
				r.cfg.Logger.Error("heartbeat failed", "error", err)
			}
		}
	}
}

// Heartbeat marks the runner online with its current repos.
func (r *Runner) Heartbeat(ctx context.Context) error {
	var version *string
	if r.cfg.ClaudeVersion != "" {
		version = &r.cfg.ClaudeVersion
	}
	_, err := r.cfg.Queries.UpsertCodeRunnerHeartbeat(ctx, store.UpsertCodeRunnerHeartbeatParams{
		UserID:        r.cfg.UserID,
		Name:          r.cfg.Name,
		Repos:         r.cfg.Repos,
		ClaudeVersion: version,
	})
	if err != nil {
		return fmt.Errorf("runner: heartbeat: %w", err)
	}
	return nil
}

// RecoverInterrupted fails, never re-runs, delegations a dead runner left in
// 'executing'. Safe only under cmd/runner's per-account lock.
func (r *Runner) RecoverInterrupted(ctx context.Context) error {
	return r.performAll(ctx, actions.StatusExecuting, 2)
}

// PollOnce performs every approved delegation, oldest first.
func (r *Runner) PollOnce(ctx context.Context) error {
	return r.performAll(ctx, actions.StatusApproved, 1)
}

func (r *Runner) performAll(ctx context.Context, status string, attempt int) error {
	ids, err := r.cfg.Queries.ListAgentActionIDsForUserByActionAndStatus(ctx,
		store.ListAgentActionIDsForUserByActionAndStatusParams{
			UserID: r.cfg.UserID,
			Action: claudecode.ActionDelegate,
			Status: status,
		})
	if err != nil {
		return fmt.Errorf("runner: list %s delegations: %w", status, err)
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return nil
		}
		r.cfg.Logger.Info("performing delegation", "action_id", id, "attempt", attempt)
		if err := r.cfg.Performer.Perform(ctx, id, attempt); err != nil {
			// Perform errors only when an outcome could not be recorded. The
			// row stays where it was and the next poll or restart deals with it.
			r.cfg.Logger.Error("delegation could not be recorded", "action_id", id, "error", err)
		}
	}
	return nil
}
