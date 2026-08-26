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
const (
	// memoryVersionRetention is the reference's published window.
	memoryVersionRetention = 30 * 24 * time.Hour
	// memoryVersionsKept is the "recent versions" count it leaves unstated.
	memoryVersionsKept = 5
	// memoryPruneInterval paces the sweep. The rows it removes are already
	// past their window by days, so nothing is gained by looking often.
	memoryPruneInterval = time.Hour

	// MetricMemoryVersionsPruned counts rows the sweep removed. Exported so
	// the test can assert the exact name; no attributes, because the only
	// candidates for one would be store or memory ids.
	MetricMemoryVersionsPruned = "memory.versions.pruned"
)

// StartMemoryRetention sweeps until ctx ends. The statement is idempotent and
// takes no locks a reader would wait on, so two controlplane replicas running
// it cost a duplicate query and never a wrong answer.
func StartMemoryRetention(ctx context.Context, pool *pgxpool.Pool) {
	t := time.NewTicker(memoryPruneInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		n, err := pruneMemoryVersions(ctx, pool, time.Now().Add(-memoryVersionRetention), memoryVersionsKept)
		switch {
		case err != nil && ctx.Err() == nil:
			slog.WarnContext(ctx, "memory version prune incomplete; the next interval retries", "error", err)
		case err == nil && n > 0:
			slog.InfoContext(ctx, "memory versions pruned", "count", n,
				"older_than", memoryVersionRetention, "kept_per_memory", memoryVersionsKept)
		}
	}
}

// pruneMemoryVersions removes every version older than the cutoff that is not
// among its memory's newest keep. Two rows are exempt whatever their age:
//
//   - A live memory's head. It is the newest of its lineage by construction,
//     so the keep window already covers it — the guard is here because a
//     dangling head is a wire 404 on memory_version_id, and this is the one
//     statement that could ever create one.
//   - Nothing else. A deleted memory's lineage prunes by the same rule, which
//     keeps it listable rather than immortal: its newest rows include the
//     `deleted` version that ends it. The reference's own wording — versions
//     "persist after the memory is deleted" — is about the delete not
//     cascading, not about exemption from retention, and its retention rule
//     names no exception.
//
// The cutoff and keep are parameters rather than the constants above so a test
// can drive a thirty-day rule without waiting thirty days.
func pruneMemoryVersions(ctx context.Context, pool *pgxpool.Pool, cutoff time.Time, keep int) (int64, error) {
	tag, err := pool.Exec(ctx, `
		DELETE FROM memory_versions v
		 WHERE v.created_at < $1
		   AND NOT EXISTS (SELECT 1 FROM memories m WHERE m.memory_version_id = v.id)
		   AND v.id NOT IN (
		        SELECT k.id FROM memory_versions k
		         WHERE k.memory_store_id = v.memory_store_id
		           AND k.memory_id = v.memory_id
		         ORDER BY k.created_at DESC, k.id DESC
		         LIMIT $2)`, cutoff, keep)
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
