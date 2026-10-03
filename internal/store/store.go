// Package store owns the Postgres schema. Every table and index the platform's
// data lives in is defined by the SQL under migrations/, embedded into the
// binaries so a deployment needs no separate migration tool or step: Open
// connects and migrates, and controlplane, brain and executor each converge the
// database at startup.
// Query SQL is not owned here — it belongs to the packages that issue it
// (internal/api, internal/events, internal/queue and friends). The exceptions
// below live on the schema's owner precisely because separate packages must
// agree on them exactly: SessionTombstoneInsertSQL, EnqueueObjectDeletes (its
// statement unexported, so no other package runs it bare), FileLiveSQL and
// FileObjectKeySQL, and the two that put a transaction where migration 0046's
// triggers require it, AllowFileCopyDeletes (its set_config) and
// BeginObjectDelete (READ COMMITTED).
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
// tables inherit scope through their foreign key to a scoped parent (since
// 0043, work_session_tokens through a session_id with no key, its rows deleted
// with the session by a trigger). Rows written today mean the same thing once
// multi-tenancy lands, which is the whole point of reserving the columns
// rather than adding them later. Scoping is org/workspace/project and never an
// end-user: sessions carry no user_id by design, and created_by is audit only.
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// FileLiveSQL is the predicate that says a files row still has content: it has
// no expiry, or its expiry has not arrived (#655, plan 49). Here for
// SessionTombstoneInsertSQL's reason, and more urgently.
//
// Four packages read this table to hand a file's bytes to something — api,
// brain, executor, events — and each was written when a row's existence was the
// whole question. When the expiry rule arrived, only api's own route was taught
// it; the other three were found serving expired bytes in review, one of them
// mounting into a sandbox what the HTTP route was already refusing. So the rule
// is written once, here, rather than in each reader's SQL: a reader that
// composes it has agreed to it, and a reader that does not has decided
// something instead of forgetting it.
//
// It reads now() from the database, never a replica's clock: expires_at was
// computed from the database's now() at upload. Unqualified, so it composes
// into a query that aliases the table as well as one that does not.
//
// now() is transaction start, not statement time, so a transaction that began
// just before an expiry and then waited on a row lock still sees the file as
// live. That is the wanted reading rather than a gap clock_timestamp() would
// close: a create mounting ten files judges all ten against one instant instead
// of letting the tenth expire between statements, expires_at is itself a now()
// value, and every other age predicate in this platform compares against now().
// The residue is a sub-transaction race that no clock function wins — a client
// one microsecond earlier would have been admitted anyway.
//
// One reader deliberately omits it, and only one: the grader's deliverables
// listing (internal/brain/grader.go) selects the session-scoped rows that own
// their object (`source_file_id IS NULL`), which only the outputs harvest
// writes, and the harvest sets no expiry. The upload route sets one, and a
// session's copy of an upload inherits it (#578), but the listing leaves copies
// out. Composing it there would guard a state no writer can reach.
const FileLiveSQL = `(expires_at IS NULL OR expires_at > now())`

// FileObjectKeySQL is the key a files row's bytes are at (#578, migration
// 0046): a session's copy of a file names its source's object in object_key,
// and every other row, which owns its object, leaves object_key NULL and keeps
// the key its id derives — blob.FilesKey's files/{id}, spelled again here
// because Postgres has to compute it too. Every reader of a file's bytes reads
// this rather than deriving a key from the id it was asked for, which would
// find nothing behind a copy. Unqualified, as FileLiveSQL is.
const FileObjectKeySQL = `coalesce(object_key, 'files/' || id)`

// AllowFileCopyDeletes lets the caller's transaction delete a session's file
// copies. Without it a DELETE skips every copy it matches, as if its WHERE had
// not (migration 0046's guard): that is what keeps a previous build, which
// deletes every session-scoped row, from taking copies during a rolling
// upgrade. Transaction-local, so it ends with the commit and never reaches the
// next transaction on the pooled connection. store's
// TestEveryFilesDeleteDecidesAboutCopies holds the list of the DELETEs that
// call it and those that deliberately do not.
func AllowFileCopyDeletes(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT set_config('map.copy_delete', 'on', true)`)
	return err
}

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

// pendingObjectDeleteInsertSQL enqueues object keys the caller's transaction
// has just orphaned, for the sweeper that deletes them (plan 50). One
// definition on the schema's owner for the same reason as the tombstone above:
// the producer and the consumer are in different packages and must agree on
// the shape exactly. Unexported, so a producer outside this package runs it
// only through EnqueueObjectDeletes, and so only on an ObjectDeleteTx.
//
// One statement for the whole set, because a session's deliverables are up to
// two hundred keys and a delete should not become two hundred round trips
// inside a transaction that holds the session row. Conflicts are ignored: an
// object already owed is owed once, and a key enqueued twice would otherwise
// fail a delete that has nothing wrong with it.
//
// Each key goes in once, as the tombstone trigger enqueues its keys: two of a
// session's copies of one upload name one object, and the reference count is
// asked about it once. The keys go in in hashtext order, which is the id of
// the advisory lock migration 0046's count can take per key: two removers
// sharing a pair of keys take the pair in one order, and two keys whose hashes
// collide are one lock, so no order between them is owed.
const pendingObjectDeleteInsertSQL = `INSERT INTO pending_object_deletes (object_key)
	 SELECT k FROM (SELECT DISTINCT unnest($1::text[]) AS k) AS d ORDER BY hashtext(k)
	 ON CONFLICT (object_key) DO NOTHING`

// EnqueueObjectDeletes runs that statement on the caller's transaction, and is
// how every producer should reach it: eight removers write this queue across
// two packages (#703), and open-coding the Exec at each one is how they came to
// disagree about the empty set — some guarding, some sending Postgres a
// zero-length array. The rule lives here once instead. An empty set is not an
// error and not a statement: a remover that found nothing to remove owes
// nothing.
//
// The transaction is the point, so the parameter is one: a caller with only a
// pool has nothing to ride and is writing a debt no commit stands behind.
//
// A key some files row still names is dropped by the database, not owed:
// migration 0046's trigger counts the references, because a session's file
// copy shares its upload's object (#578). That count can take an advisory lock
// per key and hold it to the commit, so a remover enqueues once it has deleted
// every row it is going to: a row lock asked for after one of these locks can
// close a cycle with a remover holding that row and waiting on this key. The
// count refuses a files/ key under REPEATABLE READ and SERIALIZABLE, so the
// transaction is an ObjectDeleteTx, which BeginObjectDelete makes.
func EnqueueObjectDeletes(ctx context.Context, tx ObjectDeleteTx, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, pendingObjectDeleteInsertSQL, keys)
	return err
}

// ObjectDeleteTx is a transaction BeginObjectDelete began, and the only kind
// EnqueueObjectDeletes takes: a remover that hands it a plain pgx.Tx fails to
// compile rather than failing on a database whose default isolation is
// stricter than READ COMMITTED. The unexported method keeps another package
// from declaring a type that satisfies it outright; it is a pgx.Tx to
// everything else.
//
// That is all it guards. It does not see a transaction that enqueues without
// calling EnqueueObjectDeletes: one that writes a session's tombstone without
// setting map.copy_delete first, whose trigger then enqueues the session's
// files/ keys itself, or one that runs an INSERT INTO pending_object_deletes
// of its own (this package's statement is unexported, so only the package
// itself can Exec that one). Nor does it stop another package from embedding
// an ObjectDeleteTx in a struct of its own, whose method set then includes the
// unexported method, and wrapping any transaction in it. Each remover's
// isolation level is pinned by a test instead: internal/api's
// TestEveryObjectDeleteBeginsReadCommitted and internal/executor's
// TestAHarvestBeginsReadCommitted.
//
// A type rather than a test that scans for EnqueueObjectDeletes' callers, as
// TestEveryFilesDeleteDecidesAboutCopies does for its DELETEs: a remover's
// transaction reaches the enqueue through helpers (the dream runner's passes
// through as many as six functions before its close enqueues), which a scan
// would have to follow through the call graph, while a parameter type follows
// it for free.
type ObjectDeleteTx interface {
	pgx.Tx
	beganByBeginObjectDelete()
}

type objectDeleteTx struct{ pgx.Tx }

func (objectDeleteTx) beganByBeginObjectDelete() {}

// BeginObjectDelete begins a transaction that may enqueue an object delete:
// READ COMMITTED, named rather than inherited. Migration 0046's reference
// count looks again for a row naming a files/ key after waiting out the
// remover that held it, and only a new snapshot per statement sees that
// remover's commit, so the trigger refuses such a key under REPEATABLE READ
// and SERIALIZABLE. A plain Begin takes the database's
// default_transaction_isolation, which an operator may have set stricter, and
// every transaction that enqueues a files/ key would then fail, even one the
// count would drop. Every remover begins here: a file, session, skill or
// skill-version delete, the expiry sweep, a dream runner's tick (its close
// deletes the transcripts) and the executor's harvest settle.
func BeginObjectDelete(ctx context.Context, pool *pgxpool.Pool) (ObjectDeleteTx, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	return objectDeleteTx{tx}, nil
}

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
