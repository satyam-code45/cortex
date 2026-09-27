// Command runner performs its owner's approved Claude Code delegations on this
// machine. It needs the repos checked out here and a logged-in `claude`.
//
// ponytail: connects with the full DATABASE_URL, fine while the runner is the
// owner's own laptop; a per-runner token over HTTP if anyone else runs one.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/actions"
	"cortex/internal/jobs"
	"cortex/internal/runner"
	"cortex/internal/store"
	"cortex/internal/tools/claudecode"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("runner stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	writer, err := claudecode.NewWriter(cfg.writer)
	if err != nil {
		return err
	}

	pool, err := pgxpool.New(ctx, cfg.databaseURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()
	q := store.New(pool)
	// Connected; nothing this process starts needs the URL again.
	_ = os.Unsetenv("DATABASE_URL")

	user, err := q.GetUserByEmail(ctx, cfg.userEmail)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("no Cortex account for %s; sign in once in the web app first", cfg.userEmail)
	}
	if err != nil {
		return fmt.Errorf("look up %s: %w", cfg.userEmail, err)
	}

	// Held on its own connection for the life of the process.
	lockConn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire lock connection: %w", err)
	}
	defer lockConn.Release()
	locked, err := store.New(lockConn).TryLockCodeRunner(ctx, user.ID)
	if err != nil {
		return fmt.Errorf("take the runner lock: %w", err)
	}
	if !locked {
		return fmt.Errorf("another runner is already running for %s; stop it first", cfg.userEmail)
	}
	// The lock lives exactly as long as that connection. If it drops (sleep,
	// network change) Postgres frees the lock, so stop rather than run unguarded.
	ctx, lost := context.WithCancelCause(ctx)
	defer lost(nil)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := lockConn.Ping(ctx); err != nil && ctx.Err() == nil {
					lost(fmt.Errorf("lost the runner lock connection: %w", err))
					return
				}
			}
		}
	}()

	// Insert-only: never started, so it never joins River's leader election.
	// It exists to enqueue the resume once a delegation settles.
	queue, err := jobs.New(jobs.Config{Pool: pool, Logger: logger})
	if err != nil {
		return err
	}
	registry, err := actions.NewRegistry(writer)
	if err != nil {
		return err
	}
	worker, err := jobs.NewExecuteWriteWorker(pool,
		func(_ context.Context, owner uuid.UUID) (*actions.Registry, error) {
			if owner != user.ID {
				return nil, fmt.Errorf("runner acts for %s only", cfg.userEmail)
			}
			return registry, nil
		}, queue, logger)
	if err != nil {
		return err
	}

	r, err := runner.New(runner.Config{
		Queries:       q,
		Performer:     worker,
		UserID:        user.ID,
		Name:          cfg.name,
		Repos:         writer.RepoNames(),
		ClaudeVersion: claudeVersion(ctx, cfg.writer.Bin),
		PollInterval:  cfg.poll,
		Logger:        logger,
	})
	if err != nil {
		return err
	}
	logger.Info("runner online", "user", cfg.userEmail, "name", cfg.name, "repos", writer.RepoNames())
	if err := r.Run(ctx); err != nil {
		return err
	}
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return nil
}

type config struct {
	databaseURL string
	userEmail   string
	name        string
	poll        time.Duration
	writer      claudecode.WriterConfig
}

func loadConfig() (config, error) {
	var cfg config
	cfg.databaseURL = os.Getenv("DATABASE_URL")
	cfg.userEmail = strings.TrimSpace(os.Getenv("RUNNER_USER_EMAIL"))
	if cfg.databaseURL == "" || cfg.userEmail == "" {
		return cfg, errors.New("DATABASE_URL and RUNNER_USER_EMAIL are required")
	}
	cfg.name = os.Getenv("RUNNER_NAME")
	if cfg.name == "" {
		cfg.name, _ = os.Hostname()
	}

	rawRepos := os.Getenv("RUNNER_REPOS")
	if rawRepos == "" {
		return cfg, errors.New(`RUNNER_REPOS is required, e.g. {"cortex":{"path":"/home/me/cortex"}}`)
	}
	// Strict, so a misspelt "tools" is an error rather than the defaults.
	dec := json.NewDecoder(strings.NewReader(rawRepos))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg.writer.Repos); err != nil {
		return cfg, fmt.Errorf("RUNNER_REPOS: %w", err)
	}

	cfg.writer.WorktreeDir = os.Getenv("RUNNER_WORKTREE_DIR")
	if cfg.writer.WorktreeDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return cfg, err
		}
		cfg.writer.WorktreeDir = filepath.Join(home, ".cortex-worktrees")
	}
	cfg.writer.Bin = os.Getenv("RUNNER_CLAUDE_BIN")
	cfg.writer.Model = os.Getenv("RUNNER_MODEL")

	var err error
	if cfg.writer.MaxTurns, err = intEnv("RUNNER_MAX_TURNS"); err != nil {
		return cfg, err
	}
	if v := os.Getenv("RUNNER_MAX_BUDGET_USD"); v != "" {
		if cfg.writer.MaxBudgetUSD, err = strconv.ParseFloat(v, 64); err != nil {
			return cfg, fmt.Errorf("RUNNER_MAX_BUDGET_USD: %w", err)
		}
	}
	if cfg.writer.Timeout, err = durationEnv("RUNNER_TIMEOUT"); err != nil {
		return cfg, err
	}
	if cfg.poll, err = durationEnv("RUNNER_POLL_INTERVAL"); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func intEnv(key string) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func durationEnv(key string) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}

func claudeVersion(ctx context.Context, bin string) string {
	if bin == "" {
		bin = "claude"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
