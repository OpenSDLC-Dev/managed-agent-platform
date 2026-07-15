package queue

import (
	"context"
	"fmt"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// workersPollingWindowSeconds is the wire's window for workers_polling: a worker
// counts toward the stat if its most recent poll landed within the last 30
// seconds (BetaSelfHostedWorkQueueStats.workers_polling). It doubles as the
// reap horizon in RecordPoll — a row aged past it can never be counted, so
// deleting it keeps worker_polls bounded without affecting any result.
const workersPollingWindowSeconds = 30

// WorkStats is the work-queue statistics projection for the wire stats endpoint
// (BetaSelfHostedWorkQueueStats). depth and pending partition the queued state by
// whether a poll reservation is still live; OldestQueuedAt is the oldest queued
// item's timestamp (nil when the queue holds none); WorkersPolling counts the
// distinct recently-polling workers.
type WorkStats struct {
	Depth          int64
	Pending        int64
	OldestQueuedAt *time.Time
	WorkersPolling int64
}

// Stats computes the work-queue statistics for a self_hosted environment, scoped
// like the rest of the work API (see workAPIScope). Both depth and pending count
// only queued items — the wire's "acknowledged" is our Ack (queued→starting), so
// an acked item has left the queue and counts toward neither:
//
//   - depth — queued items available to be picked up: no reservation, or a poll
//     reservation whose lease has lapsed (the same lease_expires_at < now()
//     boundary Poll uses to re-offer an item).
//   - pending — queued items polled but not yet acked: a live poll reservation.
//   - oldest_queued_at — the oldest queued item's created_at (depth + pending),
//     null when no item is queued.
//   - workers_polling — distinct workers whose last poll landed within the
//     window (recorded by RecordPoll off the poll path). Its subquery carries
//     the same self_hosted gate as workAPIScope, so all four fields report on
//     the same queue — a non-self_hosted environment is zero across the board.
//
// It is one snapshot: an aggregate-only SELECT always returns exactly one row, so
// an empty queue reports zeros with a null oldest_queued_at.
func (q *Queue) Stats(ctx context.Context, envID domain.ID) (*WorkStats, error) {
	var s WorkStats
	err := q.pool.QueryRow(ctx,
		`SELECT
		   count(*) FILTER (WHERE lease_expires_at IS NULL OR lease_expires_at < now()),
		   count(*) FILTER (WHERE lease_expires_at >= now()),
		   min(created_at),
		   (SELECT count(DISTINCT worker_id) FROM worker_polls
		     WHERE environment_id = $1
		       AND last_polled_at > now() - make_interval(secs => ($2)::double precision)
		       AND EXISTS (SELECT 1 FROM environments e WHERE e.id = $1 AND e.kind = 'self_hosted'))
		 FROM work_items
		 WHERE environment_id = $1 AND state = 'queued'`+workAPIScope,
		envID, workersPollingWindowSeconds).
		Scan(&s.Depth, &s.Pending, &s.OldestQueuedAt, &s.WorkersPolling)
	if err != nil {
		return nil, fmt.Errorf("queue: stats %s: %w", envID, err)
	}
	return &s, nil
}

// RecordPoll upserts a BYOC worker's most recent poll time for the environment,
// feeding the workers_polling stat. It is best-effort telemetry off the poll
// path: a worker identifies itself with the Anthropic-Worker-ID header, and a
// poll without an id is simply not recorded (the wire documents workers_polling
// as requiring worker_id).
//
// The same statement reaps the environment's rows that have aged past the
// workers_polling window (excluding this worker, which it is refreshing) so the
// table stays bounded by recently-active workers — without the reap, default
// worker ids being minted fresh per process (worker.defaultWorkerID) would leak
// one permanent row per process start. Reaping only rows already outside the
// window can never drop one that workers_polling would count.
func (q *Queue) RecordPoll(ctx context.Context, envID domain.ID, workerID string) error {
	_, err := q.pool.Exec(ctx,
		`WITH reap AS (
		    DELETE FROM worker_polls
		    WHERE environment_id = $1 AND worker_id <> $2
		      AND last_polled_at < now() - make_interval(secs => ($3)::double precision)
		 )
		 INSERT INTO worker_polls (environment_id, worker_id, last_polled_at)
		 VALUES ($1, $2, now())
		 ON CONFLICT (environment_id, worker_id) DO UPDATE SET last_polled_at = now()`,
		envID, workerID, workersPollingWindowSeconds)
	if err != nil {
		return fmt.Errorf("queue: record poll %s/%s: %w", envID, workerID, err)
	}
	return nil
}
