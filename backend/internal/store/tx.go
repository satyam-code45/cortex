package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Beginner is the subset of *pgxpool.Pool needed to open a transaction.
//
// Declared here rather than in each caller so the agent loop and the indexer
// depend on the same two-line contract, and so a test can substitute a pool that
// fails to begin.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// WithTx runs fn inside a transaction, committing on success and rolling back
// on any error.
//
// This lives in one place because both callers need the same non-obvious
// rollback detail. The deferred Rollback uses a context that CANNOT already be
// cancelled: pgx destroys the connection outright when it cannot send the
// ROLLBACK, which forces a fresh TLS handshake and authentication on the next
// use instead of returning the connection to the pool. On a cancelled request —
// the common case for a rollback — passing ctx straight through turns every
// abandoned transaction into a connection churn.
//
// Callers use short transactions rather than one spanning a whole operation: a
// transaction held across an LLM or embedding call pins a pool connection for
// the length of that network round trip.
func WithTx(ctx context.Context, db Beginner, fn func(q Querier) error) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op once committed

	if err := fn(New(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}
