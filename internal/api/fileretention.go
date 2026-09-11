package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
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
const (
	// fileMetadataRetention is the reference's published window, measured from
	// expires_at rather than from created_at: a file uploaded with a 90-day
	// lifetime is readable for 120 days in total, which is what "remains
	// readable for up to 30 days" past the expiry says.
	fileMetadataRetention = 30 * 24 * time.Hour

	// filePurgeBatch bounds one statement, and with it one transaction and the
	// number of object deletes a tick can owe. memoryretention's sweep needs no
	// such bound because it does nothing per row; this one makes a network call
	// for every row it removes, so an unbounded first sweep over a long backlog
	// would hold a transaction open for as long as the object store takes.
	//
	// A backlog therefore drains over successive ticks rather than in one, and
	// that is not a cost worth engineering away: every row it walks is already
	// at least 30 days past an expiry nothing is waiting on. Raise it, or add a
	// drain loop, when a deployment measures a backlog it actually minds.
	filePurgeBatch = 1000

	// MetricExpiredFilesPurged counts rows the sweep removed. Exported so the
	// test can assert the exact name; no attributes, since the only candidates
	// would be file ids.
	MetricExpiredFilesPurged = "files.expired.purged"
)

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
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		n, err := purgeExpiredFiles(ctx, pool, blobs, fileMetadataRetention)
		switch {
		case err != nil && ctx.Err() == nil:
			slog.WarnContext(ctx, "expired file purge incomplete; the next interval retries", "error", err)
		case err == nil && n > 0:
			slog.InfoContext(ctx, "expired files purged", "count", n,
				"expired_before", fileMetadataRetention)
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
		               LIMIT $2)
		 RETURNING id`, retention.Seconds(), filePurgeBatch)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if blobs != nil {
		for _, id := range ids {
			if err := blobs.Delete(ctx, blob.FilesKey(id)); err != nil {
				slog.WarnContext(ctx, "expired file orphaned in object storage",
					"file_id", id, "err", err)
			}
		}
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
