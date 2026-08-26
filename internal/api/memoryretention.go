package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// Memory-version retention (#476, plan 36 scope decision 5). The reference
// publishes the window and withholds the count: "Versions are retained for 30
// days; however, the recent versions are always kept regardless of age, so
// memories that change infrequently might retain history beyond 30 days." How
// many "recent versions" survive is stated nowhere, and no recording against
// the platform can reveal a server-side background policy, so the count is
// ours — five, registered in docs/DIVERGENCES.md.
//
// It lives beside the memory routes rather than in a package of its own
// because it is the same three facts they are written from: the head pointer
// is memories.memory_version_id, a version's lineage is
// (memory_store_id, memory_id), and neither is a foreign key. The controlplane
// hosts it because that binary already holds the pool and serves every memory
// route — a deployment whose environments are all self_hosted runs no
// executor at all.
//
// The sweep leans on migration 0029's memory_versions_memory_idx
// (memory_store_id, memory_id, created_at DESC, id DESC), which makes the
// per-candidate "newest keep" lookup an index-only scan of exactly keep rows.
// That index's own comment predates this file and calls the history one
// "nothing ever prunes"; a merged migration is immutable, so the fact lives
// here instead. Narrowing or dropping it would turn a seconds-long sweep into
// one that walks a whole store's history per candidate row.
const (
	// memoryVersionRetention is the reference's published window.
	memoryVersionRetention = 30 * 24 * time.Hour
	// memoryVersionsKept is the "recent versions" count it leaves unstated.
	memoryVersionsKept = 5

	// MetricMemoryVersionsPruned counts rows the sweep removed. Exported so
	// the test can assert the exact name; no attributes, because the only
	// candidates for one would be store or memory ids.
	MetricMemoryVersionsPruned = "memory.versions.pruned"
)

// memoryPruneInterval paces the sweep. The rows it removes are already past
// their window by days, so nothing is gained by looking often. A var, not a
// const, so the test binary can drive a tick without waiting an hour —
// ssePingInterval's reason, and export_test.go holds the setter.
var memoryPruneInterval = time.Hour

// StartMemoryRetention sweeps until ctx ends. The statement is idempotent, so
// two controlplane replicas running it cost a duplicate query and never a
// wrong answer. It takes row locks only on the rows it deletes, which no plain
// reader waits on — a redaction's `SELECT … FOR UPDATE` on one of those rows
// would, for as long as the delete runs.
func StartMemoryRetention(ctx context.Context, pool *pgxpool.Pool) {
	t := time.NewTicker(memoryPruneInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		n, err := pruneMemoryVersions(ctx, pool, memoryVersionRetention, memoryVersionsKept)
		switch {
		case err != nil && ctx.Err() == nil:
			slog.WarnContext(ctx, "memory version prune incomplete; the next interval retries", "error", err)
		case err == nil && n > 0:
			slog.InfoContext(ctx, "memory versions pruned", "count", n,
				"older_than", memoryVersionRetention, "kept_per_memory", memoryVersionsKept)
		}
	}
}

// pruneMemoryVersions removes every version older than retention that is not
// among its memory's newest keep. One row is exempt whatever its age: a live
// memory's head. It is the newest of its lineage by construction, so the keep
// window already covers it — the guard is there because a dangling head is a
// wire 404 on memory_version_id, and this is the one statement that could ever
// create one.
//
// Nothing else is exempt. A deleted memory's lineage prunes by the same rule,
// which leaves it listable rather than immortal: keep rows of it always
// survive, so the lineage a client lists is never empty. Its `deleted` marker
// is normally among them, because no version is ever *inserted* for a memory
// id after that one — ids are random and never reused, and a re-create at the
// same path mints a new memory. That is insert order, not timestamp order:
// created_at defaults to now(), which is transaction start, and deleteMemory
// begins before it takes the store lock, so an update that begins later and
// wins the lock first commits a `modified` row stamped after the marker.
// Ordering the marker out of the window would take keep such interleavings
// inside one delete's begin-to-lock gap; the listable promise does not depend
// on it either way. The reference's own wording, that versions "persist after
// the memory is deleted", is about the delete not cascading rather than about
// exemption from retention, and its retention rule names no exception. A
// redacted version is no exception either, so the record of who erased what
// ages out with the row it annotates.
//
// The window is a duration the statement subtracts from the database's own
// clock, never a timestamp computed here: created_at is stamped by Postgres,
// and every other age predicate in this platform compares against now() for
// the same reason. retention and keep are parameters so a test can drive the
// rule in seconds rather than in days.
func pruneMemoryVersions(ctx context.Context, pool *pgxpool.Pool, retention time.Duration, keep int) (int64, error) {
	tag, err := pool.Exec(ctx, `
		DELETE FROM memory_versions v
		 WHERE v.created_at < now() - make_interval(secs => $1)
		   AND NOT EXISTS (SELECT 1 FROM memories m WHERE m.memory_version_id = v.id)
		   AND v.id NOT IN (
		        SELECT k.id FROM memory_versions k
		         WHERE k.memory_store_id = v.memory_store_id
		           AND k.memory_id = v.memory_id
		         ORDER BY k.created_at DESC, k.id DESC
		         LIMIT $2)`, retention.Seconds(), keep)
	if err != nil {
		return 0, err
	}
	n := tag.RowsAffected()
	recordMemoryVersionsPruned(ctx, n)
	return n, nil
}

// recordMemoryVersionsPruned counts what one sweep removed; a sweep that
// removed nothing records nothing, so a quiet database leaves no series.
func recordMemoryVersionsPruned(ctx context.Context, n int64) {
	if n == 0 {
		return
	}
	c, err := otel.GetMeterProvider().Meter(apiMeterName).Int64Counter(
		MetricMemoryVersionsPruned,
		metric.WithDescription("Memory versions removed by the retention sweep."))
	if err != nil {
		return
	}
	c.Add(ctx, n)
}
