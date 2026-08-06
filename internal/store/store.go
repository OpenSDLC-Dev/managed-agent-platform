// Package store owns the Postgres schema. It embeds the SQL migrations and
// applies them on Open, so every binary converges the database to the
// current schema at startup.
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
