// Package runner performs its owner's approved Claude Code delegations.
//
// It polls rather than working a River queue: any River client that works a
// queue can be elected leader, and a laptop holding leadership would stop the
// server's periodic jobs. Execution itself goes through the same
// ExecuteWriteWorker.Perform as every other write, so claiming, settling and
// resuming the run are shared, not copied.
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	defer func() {
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

	// In the background, because one delegation can block the poll loop for
	// its whole run.
	go r.heartbeatLoop(ctx)

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

// RecoverInterrupted settles delegations a previous runner left in
// 'executing'. Attempt 2 routes them through the worker's "interrupted, never
// re-run" branch. Only safe because cmd/runner holds a per-account advisory
// lock: no other live runner can be working on them.
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
