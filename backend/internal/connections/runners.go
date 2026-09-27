package connections

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"cortex/internal/store"
)

// RunnerFreshness is how recent a runner's heartbeat must be for the delegate
// tool to be offered. The runner beats every 30s, so this tolerates three
// missed beats.
const RunnerFreshness = 2 * time.Minute

// OnlineRunnerRepos reports the repo names of the user's online Claude Code
// runner, or nil when there is none.
func OnlineRunnerRepos(q store.Querier) func(context.Context, uuid.UUID) ([]string, error) {
	return func(ctx context.Context, userID uuid.UUID) ([]string, error) {
		row, err := q.GetOnlineCodeRunnerForUser(ctx, store.GetOnlineCodeRunnerForUserParams{
			UserID:     userID,
			LastSeenAt: pgtype.Timestamptz{Time: time.Now().Add(-RunnerFreshness), Valid: true},
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return row.Repos, nil
	}
}
