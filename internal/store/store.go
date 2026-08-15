// Package store owns the Postgres schema. Every table and index the platform's
// data lives in is defined by the SQL under migrations/, embedded into the
// binaries so a deployment needs no separate migration tool or step: Open
// connects and migrates, and controlplane, brain and executor each converge the
// database at startup.
// Query SQL is not owned here — it belongs to the packages that issue it
// (internal/api, internal/events, internal/queue and friends). The exception
// is SessionTombstoneInsertSQL below, which lives on the schema's owner
// precisely because two packages must agree on it exactly.
//
// Three properties of Migrate (migrate.go) are contract, not implementation
// detail, and are what a contributor breaks by accident.
//
// Pending migrations are applied in filename order inside one transaction, so
// a database either reaches the current schema or is left exactly as it was;
// there is no half-migrated state to repair by hand. The price of that
// guarantee is that a migration may not contain a statement Postgres forbids
// inside a transaction block — CREATE INDEX CONCURRENTLY above all. Needing
// one means extending the migrator, not slipping the statement into a file.
//
// A transaction-scoped advisory lock is taken before any work. On a fresh
// deploy several binaries Open the same database within the same second;
// without the lock they would race to apply the same CREATE TABLE and all but
// one would fail to start. With it, one migrates while the rest wait, and
// they then find every version already recorded.
//
// The filename is the version record: it is what is written to
// schema_migrations and what "already applied" is judged by. A merged
// migration is therefore immutable. Renaming one makes every existing
// database re-apply it under the new name; editing one changes nothing for
// databases that already ran it, so the file and the deployed schema silently
// disagree. A schema change is always a new, higher-numbered file.
//
// The schema also carries multi-tenancy it does not yet enforce: top-level
// resource tables (agents, environments, sessions, events, work_items,
// api_keys, skills, files, vaults, principals among them) reserve org_id,
// workspace_id and project_id as text NOT NULL DEFAULT 'default', while child
// tables inherit scope through their foreign key to a scoped parent. Rows
// written today mean the same thing once multi-tenancy lands, which is the
// whole point of reserving the columns rather than adding them later. Scoping
// is org/workspace/project and never an end-user: sessions carry no user_id
// by design, and created_by is audit only.
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SessionTombstoneInsertSQL writes a session's deleted_sessions tombstone —
// id and environment kind, read while the sessions row can still be joined,
// so it must run before the DELETE in the same transaction. One definition on
// the schema's owner: the API's deleteSession executes it in production and
// the reaper's tests use it to stage deleted sessions, so the shape the
// reaper consumes cannot drift from the shape the API writes (the tombstone
// is the reaper's deleted-tier evidence — plan 24).
const SessionTombstoneInsertSQL = `INSERT INTO deleted_sessions (id, environment_kind)
	 SELECT s.id, e.kind FROM sessions s JOIN environments e ON e.id = s.environment_id
	 WHERE s.id = $1
	 ON CONFLICT (id) DO NOTHING`

// Open connects to the database at dsn, verifies the connection, and applies
// any pending migrations. The returned pool is ready for use; the caller
// closes it at process exit.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: parse dsn: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}
