package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// Expired-file retention (#655, plan 49 slice 2). The reference publishes both
// halves of what happens at expires_at: the content stops being retrievable
// immediately, while "Its metadata (GET /v1/files/{file_id}) remains readable
// for up to 30 days, with expires_at in the past". Slice 1 built the first
// half out of a predicate every reader composes; this is the second, and it is
// the only thing in this platform that removes a file nobody asked to remove.
//
// That reverses nothing. deleteOrphanedFile's "GC is a non-goal" note is about
// objects whose row never landed — accidents nobody can enumerate, where a
// sweep would have to guess what is live. An expired file is the opposite: the
// row names the object, and the deletion is the lifecycle the client bought at
// upload. The note stays true where it stands.
//
// It lives beside the file routes for memoryretention.go's reason — it is
// written from the same facts they are — and the controlplane hosts it because
// that binary already holds the pool and the blob store, and a deployment
// whose environments are all self_hosted runs no executor.
//
// Two things migration 0037's comment says are no longer true, and a merged
// migration is immutable — comments included — so the corrections live here,
// the same move memoryretention.go makes for 0029's. 0038's own two follow
// them.
//
// It calls this plan 48, the number the plan had while slice 1 was in review;
// an earlier-opened PR held that number and merged first, so the plan is 49.
//
// And it argues against an index on the ground that "a partial index over
// `expires_at IS NOT NULL` would cost every upload a write", which is backwards:
// a partial index does not index the rows its predicate excludes, so an upload
// with no lifetime pays nothing at all and only an expiring one pays. Migration
// 0038 adds that index, because the scan 0037 was willing to accept now runs
// once an hour on every deployment, forever.
//
// 0038 is immutable in its turn, and two things it says need the same
// treatment (#698). It describes the table as growing "a row per upload and a
// row per harvested deliverable, pruned by nothing except this sweep", and
// both halves are wrong about deliverables: the executor's harvest inserts
// them without an expires_at (internal/executor/harvest.go names id, filename,
// mime_type, size_bytes, downloadable, scope_type and scope_id), so this sweep
// never sees one — what removes them is a session delete, which takes every
// row scoped to the session, and the next replacing harvest, which drops the
// whole snapshot before writing the new one. The index still earns its place
// on the uploads, which do expire.
//
// And it omits the note 0031 and 0035 both carry: migrate.go applies every
// pending file inside one transaction, so CREATE INDEX cannot be CONCURRENTLY.
// This form takes SHARE on files for the build — 0035's own wording for the
// same situation — so reads go on and writes wait, and they wait not for the
// build but for the whole migration transaction, since migrate.go commits
// once. On the very table 0038 argues grows without bound, that is an upgrade
// stall on uploads, harvests and deletes that an operator gets no warning of.
const (
	// fileMetadataRetention is the reference's published window, measured from
	// expires_at rather than from created_at: a file uploaded with a 90-day
	// lifetime is readable for 120 days in total, which is what "remains
	// readable for up to 30 days" past the expiry says.
	fileMetadataRetention = 30 * 24 * time.Hour

	// filePurgeBatch bounds one transaction: the rows the DELETE locks and
	// rewrites, and the keys the enqueue writes beside them before either
	// commits. memoryretention's sweep needs no such bound because it does
	// nothing per row; this one writes a debt for every row it removes, and an
	// unbounded first sweep over a long backlog would hold all of them open at
	// once.
	//
	// A var rather than a const for filePurgeInterval's reason: at the real
	// size the order a batch is taken in is unobservable, so export_test.go
	// shrinks it to watch the oldest go first.
	//
	// The batch is taken oldest-first, so a backlog drains as a queue and not as
	// a lottery.
	//
	// A backlog therefore drains over the passes of one tick rather than over
	// successive hours, which is the loop below: the batch bounds a
	// transaction, not a tick. It bounded a tick until the sweep stopped
	// deleting objects (#696) — a thousand rows meant a thousand store round
	// trips, and an hourly ceiling of a thousand rows meant a 50k backlog took
	// 50 hours to clear while its metadata outlived the published window
	// (#698). A pass now costs one DELETE and one array insert, so there is
	// nothing left for the ceiling to protect.
	filePurgeBatchDefault = 1000

	// MetricExpiredFilesPurged counts rows the sweep removed. Exported so the
	// test can assert the exact name; no attributes, since the only candidates
	// would be file ids.
	MetricExpiredFilesPurged = "files.expired.purged"
)

// filePurgeBatch is filePurgeBatchDefault, shrinkable by the test binary.
var filePurgeBatch = filePurgeBatchDefault

// filePurgeInterval paces the sweep. Expiry latency is not what this decides —
// the content route stops serving at expires_at, whatever the sweep has done —
// so looking more often than hourly buys nothing. A var, not a const, so the
// test binary can drive a tick without waiting an hour (memoryPruneInterval's
// reason; export_test.go holds the setter).
var filePurgeInterval = time.Hour

// StartFileRetention sweeps until ctx ends. Safe on every replica at once: the
// DELETE is itself the claim, so a row is returned to exactly one sweeper and
// only that sweeper owes its objects.
//
// It removes no bytes. It removes rows and writes, in the same transaction, one
// pending_object_deletes row per object they orphan — so a deployment has one
// remover of orphaned bytes, the drain, whether the keys came from a session
// delete or from here (plan 50). A deployment configured without object storage
// stops being a special case: the debt is recorded the same way, and the half
// that needs a store is the drain, which such a deployment does not run.
func StartFileRetention(ctx context.Context, pool *pgxpool.Pool, q *ObjectDeleteQueue) {
	t := time.NewTicker(filePurgeInterval)
	defer t.Stop()
	for {
		// The sweep runs before the first wait rather than after it: a ticker
		// does not fire on creation, so a control plane that restarts more
		// often than this interval would otherwise never sweep at all — and a
		// rolling deployment is exactly that control plane. Replicas all
		// sweeping at boot is not a collision either, since the DELETE is the
		// claim and their batches are disjoint.
		//
		// It then keeps going until a pass comes back short, which is the
		// object-delete drain's shape and is what keeps the batch a bound on
		// one transaction rather than on one hour: a backlog larger than a
		// batch used to wait a tick per thousand rows, so 50k expired rows
		// outlived the published window by two days (#698). Each pass is its
		// own transaction, so a tick that is interrupted keeps every pass that
		// committed.
		total := 0
		for {
			n, err := purgeExpiredFiles(ctx, pool, fileMetadataRetention)
			total += n
			if err != nil {
				if ctx.Err() == nil {
					slog.WarnContext(ctx, "expired file purge incomplete; the next interval retries", "error", err)
				}
				break
			}
			if n < filePurgeBatch {
				break
			}
		}
		if total > 0 {
			slog.InfoContext(ctx, "expired files purged", "count", total,
				"older_than", fileMetadataRetention)
			// The keys are visible the instant those commits returned, so the
			// drain is asked to look now rather than at its own next minute —
			// deleteSession's move, for a tick that can owe far more than a
			// session does. Non-blocking, and never load-bearing: it only
			// brings forward what the drain's own interval would do anyway.
			q.Wake()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// filePurgeBeforeCommitHook is a test-only seam fired after the sweep has
// enqueued the batch's object keys and before it commits; nil in production.
// It is deleteSessionBeforeCommitHook's twin and exists for the same window:
// the only place from which either half of "the row and the debt commit
// together" can be watched, and the only place an error can fail the sweep
// while both are still uncommitted.
var filePurgeBeforeCommitHook func() error

// filePurgeAfterCommitHook is the other half of that seam, fired between the
// commit and the sweep's return; nil in production. It is where "a sweep that
// commits cannot fail to owe" is actually checked: an enqueue moved to after
// the commit — which reopens the crash window this whole change closes, a
// process dying between the two writes — leaves the queue empty at this instant
// and full by the time the caller could look.
//
// Which is why nothing may be written between the commit and this call: a write
// placed there is the one version of that mistake the seam cannot see, and a
// probe confirmed it survives every rung.
var filePurgeAfterCommitHook func()

// purgeExpiredFiles removes one batch of files whose grace window has elapsed
// and, in the same transaction, records what those removals leave owed.
//
// One transaction rather than two statements, because an id is the only name an
// object has. A DELETE that committed without the enqueue would take the names
// with it and no tier could enumerate what was left in the store — #645's class,
// reached here three ways that have nothing to do with each other: a
// cancellation landing in the RETURNING drain (#696), a store refusing every key
// of a healthy sweep (#698), and a process that dies between the two statements.
// None of them can lose a batch now. What commits here the object-delete drain
// retries with backoff and never drops.
//
// So this sweep touches no object store, and nothing it does is best-effort.
// The old order it inherited — row first, object after, orphan accepted — is
// still the file, skill and dream delete paths' (#703), where one request
// orphans a handful at most: one object, or a skill's versions, or a dream's
// hundred transcripts and an index, rather than a batch of a thousand an hour.
//
// The window is a duration subtracted from the database's own clock, never a
// timestamp computed here: expires_at was itself computed from that clock at
// upload. retention is a parameter so a test can drive the rule in seconds
// rather than in days.
//
// A file an open dream owns is exempt, which is deleteFile's refusal made
// structural here rather than left resting on the fact that nothing sets
// expires_at on a transcript. That fact is true and is not an invariant: any
// future path that stamped an expiry on a transcript would walk through a door
// the manual route holds shut, and would take the file out from under a runner
// still writing to it. Closed dreams are not exempt — nobody is writing those —
// so the clause asks what deleteFile asks rather than excluding dream_id
// outright, which would quietly make a closed dream's transcripts immortal if
// one ever did get an expiry (#698).
func purgeExpiredFiles(ctx context.Context, pool *pgxpool.Pool, retention time.Duration) (int, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		DELETE FROM files
		 WHERE id IN (SELECT f.id FROM files f
		               WHERE f.expires_at < now() - make_interval(secs => $1)
		                 AND NOT EXISTS (SELECT 1 FROM dreams d
		                                  WHERE d.id = f.dream_id AND d.closed_at IS NULL)
		               ORDER BY f.expires_at, f.id
		               LIMIT $2
		               FOR UPDATE SKIP LOCKED)
		 RETURNING id`, retention.Seconds(), filePurgeBatch)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	if len(ids) > 0 {
		keys := make([]string, len(ids))
		for i, id := range ids {
			keys[i] = blob.FilesKey(id)
		}
		if _, err := tx.Exec(ctx, store.PendingObjectDeleteInsertSQL, keys); err != nil {
			return 0, err
		}
	}
	// Test seam: read the queue from another connection in exactly this window,
	// or fail the sweep here to watch the rollback. nil in production.
	if filePurgeBeforeCommitHook != nil {
		if err := filePurgeBeforeCommitHook(); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	// Test seam: read the queue in exactly this window. nil in production.
	if filePurgeAfterCommitHook != nil {
		filePurgeAfterCommitHook()
	}
	recordExpiredFilesPurged(ctx, len(ids))
	return len(ids), nil
}

// recordExpiredFilesPurged counts what one sweep removed; a sweep that removed
// nothing records nothing, so a quiet database leaves no series.
func recordExpiredFilesPurged(ctx context.Context, n int) {
	if n == 0 {
		return
	}
	c, err := otel.GetMeterProvider().Meter(apiMeterName).Int64Counter(
		MetricExpiredFilesPurged,
		metric.WithDescription("Expired files removed by the retention sweep."))
	if err != nil {
		return
	}
	c.Add(ctx, int64(n))
}
