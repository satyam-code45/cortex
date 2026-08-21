// Command rivermigrate applies River's own database schema.
//
// River's tables live outside goose on purpose. Its DDL contains a
// CREATE TYPE ... AS ENUM, CREATE OR REPLACE FUNCTION bodies quoted with $$,
// triggers, UNLOGGED tables, and /* TEMPLATE: schema */ placeholders that River
// resolves at apply time. Vendoring that into db/migrations/ would mean
// hand-wrapping every function body in -- +goose StatementBegin, feeding all of
// it to sqlc (whose schema: points at that directory), and freezing a copy that
// drifts from the River version in go.mod the first time it is upgraded.
//
// So goose owns application schema and River owns River's. Upgrading River then
// means bumping go.mod and re-running this — which is what `make migrate` does.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

// migrateTimeout bounds the whole migration.
const migrateTimeout = 60 * time.Second

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	down := flag.Bool("down", false, "roll River's schema back instead of applying it")
	databaseURL := flag.String("database-url", "",
		"Postgres connection string (defaults to $DATABASE_URL)")
	flag.Parse()

	if err := run(logger, *databaseURL, *down); err != nil {
		logger.Error("river migration failed", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger, databaseURL string, down bool) error {
	if databaseURL == "" {
		databaseURL = os.Getenv("DATABASE_URL")
	}
	if databaseURL == "" {
		return errors.New("DATABASE_URL is not set and --database-url was not given")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, migrateTimeout)
	defer cancel()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()

	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return fmt.Errorf("build migrator: %w", err)
	}

	direction := rivermigrate.DirectionUp
	if down {
		direction = rivermigrate.DirectionDown
	}

	result, err := migrator.Migrate(ctx, direction, nil)
	if err != nil {
		return fmt.Errorf("migrate river schema %s: %w", direction, err)
	}

	if len(result.Versions) == 0 {
		logger.Info("river schema already up to date", "direction", string(direction))
		return nil
	}
	for _, version := range result.Versions {
		logger.Info("applied river migration",
			"direction", string(direction), "version", version.Version, "name", version.Name)
	}
	return nil
}
