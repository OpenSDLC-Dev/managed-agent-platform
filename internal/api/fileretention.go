package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
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
// the same move memoryretention.go makes for 0029's.
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
const (
	// fileMetadataRetention is the reference's published window, measured from
	// expires_at rather than from created_at: a file uploaded with a 90-day
	// lifetime is readable for 120 days in total, which is what "remains
	// readable for up to 30 days" past the expiry says.
	fileMetadataRetention = 30 * 24 * time.Hour

	// filePurgeBatch bounds two things: the DELETE itself — the rows one
	// statement locks and rewrites, and with them the length of that statement's
	// own transaction — and the number of object deletes the tick then owes.
	// memoryretention's sweep needs no such bound because it does nothing per
	// row; this one makes a network call for every row it removed, and an
	// unbounded first sweep over a long backlog would owe as many as there were.
	// (The object deletes are not inside that transaction: the DELETE commits
	// when the statement returns, and the loop below runs after it.)
	//
	// A var rather than a const for filePurgeInterval's reason: at the real
	// size the order a batch is taken in is unobservable, so export_test.go
	// shrinks it to watch the oldest go first.
	//
	// A backlog therefore drains over successive ticks rather than in one, and
	// that is not a cost worth engineering away: every row it walks is already
	// at least 30 days past an expiry nothing is waiting on. The batch is taken
	// oldest-first, so draining over ticks is a queue and not a lottery.
	//
	// It is a rate as well as a delay, though, and the rate is the part that
	// could bite: a thousand an hour is the ceiling, so a deployment expiring
	// more than that sustainedly would never drain and its metadata would
	// outlive the published window. Raise it, or add a drain loop, when a
	// deployment measures either problem.
	filePurgeBatchDefault = 1000

	// filePurgeCleanupBudget bounds the object deletes a tick will wait for
	// once its rows are gone. It is deleteSession's number for the same
	// operation, and the trade is different here in one way worth stating: this
	// half runs detached, so a controlplane shutdown waits up to this long for
	// the tick in flight. A store too slow to finish the batch inside it leaves
	// a tail — counted and logged as one line, rather than cancelled silently.
	//
	// That wait is why the chart sets terminationGracePeriodSeconds: an orderly
	// exit is up to 10s of HTTP drain plus up to this, and Kubernetes' own
	// default of 30s would SIGKILL the drain partway — losing both the tail and
	// the one line that would have counted it, which is the whole point of
	// running detached.
	//
	// A var rather than a const for filePurgeBatch's reason: nothing a test can
	// drive exhausts 30 seconds, so export_test.go shrinks it to watch a store
	// that has stopped answering cost the sweep the budget and not the process.
	filePurgeCleanupBudgetDefault = 30 * time.Second

	// MetricExpiredFilesPurged counts rows the sweep removed. Exported so the
	// test can assert the exact name; no attributes, since the only candidates
	// would be file ids.
	MetricExpiredFilesPurged = "files.expired.purged"
)

// filePurgeBatch is filePurgeBatchDefault, shrinkable by the test binary.
var filePurgeBatch = filePurgeBatchDefault

// filePurgeCleanupBudget is filePurgeCleanupBudgetDefault, likewise shrinkable.
var filePurgeCleanupBudget = filePurgeCleanupBudgetDefault

// filePurgeInterval paces the sweep. Expiry latency is not what this decides —
// the content route stops serving at expires_at, whatever the sweep has done —
// so looking more often than hourly buys nothing. A var, not a const, so the
// test binary can drive a tick without waiting an hour (memoryPruneInterval's
// reason; export_test.go holds the setter).
var filePurgeInterval = time.Hour

// StartFileRetention sweeps until ctx ends. Safe on every replica at once: the
// DELETE is itself the claim, so a row is returned to exactly one sweeper and
// only that sweeper deletes its object.
//
// blobs may be nil — a deployment configured without object storage cannot
// accept an upload at all, so it can only hold rows from a configuration that
// had one. The rows are still removed; the objects are then beyond this
// process's reach and are left to the operator who took the store away.
func StartFileRetention(ctx context.Context, pool *pgxpool.Pool, blobs blob.Store) {
	t := time.NewTicker(filePurgeInterval)
	defer t.Stop()
	for {
		// The sweep runs before the first wait rather than after it: a ticker
		// does not fire on creation, so a control plane that restarts more
		// often than this interval would otherwise never sweep at all — and a
		// rolling deployment is exactly that control plane. Replicas all
		// sweeping at boot is not a collision either, since the DELETE is the
		// claim and their batches are disjoint.
		n, err := purgeExpiredFiles(ctx, pool, blobs, fileMetadataRetention)
		switch {
		case err != nil && ctx.Err() == nil:
			slog.WarnContext(ctx, "expired file purge incomplete; the next interval retries", "error", err)
		case err == nil && n > 0:
			slog.InfoContext(ctx, "expired files purged", "count", n,
				"expired_before", fileMetadataRetention)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// purgeExpiredFiles removes one batch of files whose grace window has elapsed,
// then their objects.
//
// The row goes first and the object follows best-effort, which is deleteFile's
// order and deleteFile's reason: a failure here leaves an orphaned object,
// which this package accepts everywhere it touches the two stores, while the
// reverse order would leave a metadata row pointing at bytes that are gone —
// the one state insertFile is built to make impossible.
//
// The window is a duration subtracted from the database's own clock, never a
// timestamp computed here: expires_at was itself computed from that clock at
// upload. retention is a parameter so a test can drive the rule in seconds
// rather than in days.
//
// A dream's transcript files are out of range by construction rather than by a
// clause — nothing sets expires_at on them — so deleteFile's refusal to remove
// a file an open dream owns has no twin to grow here.
func purgeExpiredFiles(ctx context.Context, pool *pgxpool.Pool, blobs blob.Store, retention time.Duration) (int, error) {
	rows, err := pool.Query(ctx, `
		DELETE FROM files
		 WHERE id IN (SELECT id FROM files
		               WHERE expires_at < now() - make_interval(secs => $1)
		               ORDER BY expires_at, id
		               LIMIT $2
		               FOR UPDATE SKIP LOCKED)
		 RETURNING id`, retention.Seconds(), filePurgeBatch)
	if err != nil {
		return 0, err
	}
	// The detachment below starts here, not earlier, and that boundary leaves a
	// window of its own: a cancellation landing while this drains loses the ids
	// of a DELETE the server may already have committed, and then no tier knows
	// those keys. It is milliseconds wide against an hourly tick, and closing it
	// would mean running the statement itself past the caller's cancellation —
	// which cannot distinguish a commit from an abort either. #696 holds it.
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	recordExpiredFilesPurged(ctx, len(ids))
	if blobs == nil || len(ids) == 0 {
		return len(ids), nil
	}
	// The rows are committed by the time this runs, so their ids are gone and
	// nothing else knows these keys: an object skipped here is orphaned for
	// good (#645's class). It therefore runs on a context the sweep's own
	// cancellation cannot reach — deleteSession's shape for the same operation
	// — with a budget instead, so a shutdown mid-sweep costs at most that much
	// delay rather than a silent leak of the whole batch.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), filePurgeCleanupBudget)
	defer cancel()
	var failed, unattempted int
	var firstErr error
	for i, id := range ids {
		if dctx.Err() != nil {
			unattempted = len(ids) - i
			break
		}
		if err := blobs.Delete(dctx, blob.FilesKey(id)); err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	// One line for the set, not one per object: deleteSession's rule, and a
	// batch is a thousand times more able to break it — a store answering 403
	// to every key would otherwise say so a thousand times a tick and never say
	// how many objects were left behind.
	if failed > 0 || unattempted > 0 {
		slog.WarnContext(ctx, "expired files left in object storage",
			"failed", failed, "unattempted", unattempted, "total", len(ids), "error", firstErr)
	}
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
