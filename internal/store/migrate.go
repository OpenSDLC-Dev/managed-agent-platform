package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"

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
// CREATE INDEX CONCURRENTLY — extend the migrator if that day comes.)
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
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

	for _, name := range names { // fs.Glob returns sorted names
		version := path.Base(name)
		var applied bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`,
			version).Scan(&applied); err != nil {
			return fmt.Errorf("store: check %s: %w", version, err)
		}
		if applied {
			continue
		}
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
	return tx.Commit(ctx)
}
