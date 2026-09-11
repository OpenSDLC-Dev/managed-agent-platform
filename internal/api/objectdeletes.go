package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
)

// The retrying half of object cleanup (plan 49, #645 and #320). A delete
// enqueues the keys it orphans in its own transaction and removes no bytes
// itself; this drains that queue, deleting each object and then the row that
// owed it.
//
// It lives in the control plane rather than in the executor's reaper, which is
// the near-miss worth naming: a reap pass visits only the sessions
// provider.Owned() currently returns, so a session whose sandbox the idle tier
// already destroyed is never revisited and the reaper is not a second remover
// for it at all. That is the hole #320 records, and no new tier could close it.

const (
	// objectDeleteBatch bounds one pass. A backlog drains over several passes
	// rather than in one long transaction, which keeps a claim short and the
	// tail from starving behind a store that has slowed down.
	objectDeleteBatch = 100

	// objectDeleteBackoffMax caps the retry of a key the store refuses. Capped
	// rather than unbounded because a row is never dropped: a permanently
	// failing key retries forever, and forever at an hour is a line in a log a
	// day, while forever at doubling would quietly become never.
	objectDeleteBackoffMax = time.Hour
)

// objectDeleteBackoffBase is the first wait after a refusal, doubling from
// there to the cap above. A var so a rung can watch a refused key come back
// without spending the wait; never written in production.
var objectDeleteBackoffBase = 30 * time.Second

// objectDeleteInterval paces the sweep that runs without a wake. It is the
// backstop, not the usual path — an ending wakes its own replica — so it is
// sized for the two cases a wake cannot cover: another replica's enqueue, and a
// key whose backoff has just come due. A var so the one rung that must watch
// the interval itself fire need not spend it; never written in production.
var objectDeleteInterval = time.Minute

// objectDeleteClaimLease is how long a claimed key is held before another
// replica may take it. Long enough that an ordinary store call finishes inside
// it, short enough that a sweeper which dies mid-pass leaves its claim stuck
// for about one sweep rather than for a backoff. Distinct from the backoff
// above, which is what a key gets after a refusal rather than while it is being
// tried. A var for the same reason as the interval.
var objectDeleteClaimLease = time.Minute

// ObjectDeleteQueue is the wake shared between the handlers that enqueue and
// the sweeper that drains, within one process. A capacity-one channel: the
// request is "look now", so several endings collapse into the one sweep that
// sees all of them.
type ObjectDeleteQueue struct {
	wake chan struct{}
}

// NewObjectDeleteQueue returns the wake. A nil queue is legal everywhere and
// means no wake — the sweeper's interval still drains, which is what every test
// that does not care about latency gets.
func NewObjectDeleteQueue() *ObjectDeleteQueue {
	return &ObjectDeleteQueue{wake: make(chan struct{}, 1)}
}

// Wake asks this process's sweeper to drain now. Non-blocking and infallible by
// construction: it is called after a commit, where nothing may fail the request
// that has already succeeded, and everything it does the interval also does.
func (q *ObjectDeleteQueue) Wake() {
	if q == nil {
		return
	}
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// waits returns the channel to select on, or nil for a queue that was never
// built — a nil channel blocks forever in a select, which is exactly "this
// process has no wake" without a second arm to write.
func (q *ObjectDeleteQueue) waits() <-chan struct{} {
	if q == nil {
		return nil
	}
	return q.wake
}

// StartPendingObjectDeletes drains the queue until ctx ends: once per interval,
// and again whenever a delete in this process wakes it. It never returns an
// error — a store that is refusing today is a row that is still owed tomorrow,
// which is the whole point of the table.
func StartPendingObjectDeletes(ctx context.Context, pool *pgxpool.Pool, blobs blob.Store, q *ObjectDeleteQueue) {
	if blobs == nil {
		// No store configured, so nothing can be deleted and nothing should be
		// claimed: rows stay owed for a deployment that does have one.
		return
	}
	t := time.NewTicker(objectDeleteInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-q.waits():
		}
		// Until the pass claims nothing: a wake says a delete just enqueued a
		// set, and one batch may not be all of it.
		for {
			done, failed, err := drainPendingObjectDeletes(ctx, pool, blobs)
			if err != nil {
				if ctx.Err() == nil {
					slog.WarnContext(ctx, "object delete sweep incomplete; the next pass retries", "error", err)
				}
				break
			}
			if done > 0 {
				slog.InfoContext(ctx, "orphaned objects deleted", "count", done)
			}
			if done+failed < objectDeleteBatch {
				break
			}
		}
	}
}

// drainPendingObjectDeletes claims one batch of due keys, deletes each object,
// and removes the rows whose objects are gone. It reports how many it removed
// and how many it left owed.
//
// The claim is FOR UPDATE SKIP LOCKED — the work queue's pattern — so several
// replicas drain one table without duplicating the store round trips. Nothing
// depends on it for correctness: deleting a key twice is nil for every backend
// (blob.Store's contract, and blobtest's DeleteMissingIsNil rung), so a claim
// two replicas somehow shared would cost a redundant call and nothing else.
//
// The deletes run outside the transaction on purpose. A store call is a network
// round trip of unbounded duration, and a hundred of them inside an open
// transaction would hold a connection and a snapshot for as long as the slowest
// store cares to take. The cost of that choice is the crash window it opens —
// an object deleted, then the process dies before its row is — which costs a
// redundant delete on the next pass and nothing more.
func drainPendingObjectDeletes(ctx context.Context, pool *pgxpool.Pool, blobs blob.Store) (done, failed int, err error) {
	claimed, err := claimPendingObjectDeletes(ctx, pool, objectDeleteBatch)
	if err != nil || len(claimed) == 0 {
		return 0, 0, err
	}
	var removed []string
	for _, key := range claimed {
		if ctx.Err() != nil {
			break
		}
		if derr := blobs.Delete(ctx, key); derr != nil {
			failed++
			if ferr := deferPendingObjectDelete(ctx, pool, key, derr); ferr != nil && ctx.Err() == nil {
				slog.WarnContext(ctx, "object delete failure not recorded", "key", key, "error", ferr)
			}
			continue
		}
		removed = append(removed, key)
	}
	if len(removed) > 0 {
		if _, derr := pool.Exec(ctx,
			`DELETE FROM pending_object_deletes WHERE object_key = ANY($1::text[])`, removed); derr != nil {
			// The objects are gone; the rows are not. The next pass deletes
			// keys that are already missing, which every backend answers nil.
			return 0, failed, derr
		}
	}
	return len(removed), failed, nil
}

// claimPendingObjectDeletes takes the due keys, oldest first, and pushes their
// next attempt out so a concurrent replica does not pick up what this pass is
// already working on. The push is the claim: the rows are not locked for the
// duration of the store calls, which happen after this transaction commits.
func claimPendingObjectDeletes(ctx context.Context, pool *pgxpool.Pool, limit int) ([]string, error) {
	rows, err := pool.Query(ctx,
		`UPDATE pending_object_deletes SET next_attempt_at = now() + $2::interval
		  WHERE object_key IN (
		        SELECT object_key FROM pending_object_deletes
		         WHERE next_attempt_at <= now()
		         ORDER BY next_attempt_at, enqueued_at
		         LIMIT $1
		         FOR UPDATE SKIP LOCKED)
		  RETURNING object_key`,
		limit, objectDeleteClaimLease.String())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// deferPendingObjectDelete records a failed attempt and sets when to try again.
// The backoff doubles from the base and stops at the cap, because the row is
// never dropped: a key the store permanently refuses retries forever, and the
// cap is what keeps forever to a line an hour rather than a line a day and then
// a line a year.
func deferPendingObjectDelete(ctx context.Context, pool *pgxpool.Pool, key string, cause error) error {
	_, err := pool.Exec(ctx,
		`UPDATE pending_object_deletes
		    SET attempts        = attempts + 1,
		        last_error      = $2,
		        next_attempt_at = now() + LEAST($3::interval * power(2, attempts), $4::interval)
		  WHERE object_key = $1`,
		key, truncateError(cause), objectDeleteBackoffBase.String(), objectDeleteBackoffMax.String())
	return err
}

// truncateError bounds what a store's error can write into a row. A backend is
// free to return a whole response body, and this column is read by an operator
// asking what went wrong, not by anything that needs the tail.
func truncateError(err error) string {
	const max = 500
	s := err.Error()
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
