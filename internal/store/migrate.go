package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Migration filenames are the applied-version record in schema_migrations:
// never rename a migration once it has merged, or every existing database
// re-applies it under the new name.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrateLockID serializes concurrent migrators (several binaries Open the
// same database at startup) on a Postgres advisory lock. Explicitly int64:
// untyped it would overflow int on 32-bit builds (the BYOC worker is meant
// to cross-compile).
const migrateLockID int64 = 7355608041991001

// Migrate applies any migrations not yet recorded in schema_migrations, in
// filename order, all inside one transaction: either the database reaches
// the current schema or it is left untouched. (Consequence: a migration can
// never use statements Postgres forbids inside a transaction block, e.g.
// CREATE INDEX CONCURRENTLY — extend the migrator if that day comes.) A
// transaction that meets a lock conflict is retried; see migrateAttempts.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return migrate(ctx, pool, "")
}

// migrateAttempts and migrateBackoff bound the retry of a migration
// transaction that met a lock conflict: a deadlock (40P01), or a migration's
// own lock_timeout running out (55P03). 0043 sets one to bound how long live
// traffic queues behind its lock requests. The transaction has rolled back
// whole, so a retry starts from the same schema. The wait before each retry
// doubles from migrateBackoff, 1+2+4+8 = 15 seconds across five attempts, and
// each attempt can itself wait up to 0043's 2s lock_timeout for each of the
// two tables it locks, so a conflict that outlasts all five fails the start
// after up to about 35 seconds, like any other migration error. Variables so
// a test can run the schedule to exhaustion quickly (export_test.go).
var (
	migrateAttempts = 5
	migrateBackoff  = time.Second
)

// migrate is Migrate stopping after the named migration file when through is
// set — the test seam for exercising a data backfill against rows written
// under the schema before it.
func migrate(ctx context.Context, pool *pgxpool.Pool, through string) error {
	wait := migrateBackoff
	for attempt := 1; ; attempt++ {
		err := migrateOnce(ctx, pool, through)
		var pgErr *pgconn.PgError
		if err == nil || attempt == migrateAttempts ||
			!errors.As(err, &pgErr) || (pgErr.Code != "40P01" && pgErr.Code != "55P03") {
			return err
		}
		slog.WarnContext(ctx, "store: migration met a lock conflict, retrying",
			"attempt", attempt, "wait", wait, "error", err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
		wait *= 2
	}
}

// migrateOnce is one attempt: every pending migration, in one transaction.
func migrateOnce(ctx context.Context, pool *pgxpool.Pool, through string) error {
	names, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("store: list migrations: %w", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin migration: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrateLockID); err != nil {
		return fmt.Errorf("store: acquire migration lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("store: ensure schema_migrations: %w", err)
	}

	announced := false
	for _, name := range names { // fs.Glob returns sorted names
		version := path.Base(name)
		var applied bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`,
			version).Scan(&applied); err != nil {
			return fmt.Errorf("store: check %s: %w", version, err)
		}
		if !applied {
			// Every binary migrates whatever database it connects to, so one
			// pointed at the wrong Postgres upgrades it without a word — a
			// second compose stack, resolving `postgres` to a running stack's
			// container, is how that happened (#438). Say which database once,
			// before the first statement runs rather than after the commit, so
			// a run that fails halfway still names what it was touching.
			//
			// Asked of the connection rather than read off the config, because
			// the config is what could not tell the two apart: both stacks
			// configure `postgres:5432/managed_agent_platform`, so only the
			// address that actually answered distinguishes them. Both are
			// logged — the surprise is precisely that they disagree. Neither
			// carries the password the DSN holds.
			if !announced {
				var db, serverAddr string
				var serverPort uint16
				if err := tx.QueryRow(ctx, `SELECT current_database(),
					coalesce(host(inet_server_addr()), 'unix socket'),
					coalesce(inet_server_port(), 0)`).Scan(&db, &serverAddr, &serverPort); err != nil {
					return fmt.Errorf("store: identify database: %w", err)
				}
				slog.InfoContext(ctx, "store: migrating database", "database", db,
					"server_addr", serverAddr, "server_port", serverPort,
					"configured_host", pool.Config().ConnConfig.Host)
				announced = true
			}
			slog.InfoContext(ctx, "store: applying migration", "version", version)
			sql, err := migrationsFS.ReadFile(name)
			if err != nil {
				return fmt.Errorf("store: read %s: %w", version, err)
			}
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return fmt.Errorf("store: apply %s: %w", version, err)
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
				return fmt.Errorf("store: record %s: %w", version, err)
			}
		}
		if version == through {
			break // applied already or just now: the schema stops here either way
		}
	}
	return tx.Commit(ctx)
}
