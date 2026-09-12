package api

import (
	"context"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
)

// The retrying half of object cleanup (plan 50, #645 and #320). A delete
// enqueues the keys it orphans in its own transaction and removes no bytes
// itself; this drains that queue, deleting each object and then the row that
// owed it.
//
// It lives in the control plane rather than in the executor's reaper, which is
// the near-miss worth naming: a reap pass visits only the sessions
// provider.Owned() currently returns, so a session whose sandbox the idle tier
// already destroyed is never revisited and the reaper is not a second remover
// for it at all. That is the hole #320 records, and no new tier could close it.

// objectDeleteBatch bounds one pass. A backlog drains over several passes
// rather than in one long transaction, which keeps a claim short and the tail
// from starving behind a store that has slowed down.
const objectDeleteBatch = 100

// objectDeleteBackoffMax caps the retry of a key the store refuses. Capped
// rather than unbounded because a row is never dropped: a permanently failing
// key retries forever, and forever at an hour is a line in a log a day, while
// forever at doubling would quietly become never.
//
// A var for the reason the base below is one, and for one of its own: a rung
// watching a refused key come back has to bound the doubling too, or the wait
// it saves on the first retry it spends on the ninth.
var objectDeleteBackoffMax = time.Hour

// objectDeleteBackoffBase is the first wait after a refusal, doubling from
// there to the cap above. A var so a rung can watch a refused key come back
// without spending the wait; never written in production.
var objectDeleteBackoffBase = 30 * time.Second

// objectDeleteCallBudget bounds one store call. It is a liveness bound and not
// a performance one: the point is that a delete cannot run forever, not that it
// must be quick. Without it a single call that never returns — a blackholed
// endpoint answers nothing and times out at no layer below, the HTTP client
// having no response deadline of its own — stops this sweeper for the life of
// the process, because the keys are worked through one at a time. With it that
// call is a failed attempt like any other: counted, carrying its cause, backed
// off, and followed by the next key. A var so a rung can reach the hang without
// waiting one out.
var objectDeleteCallBudget = 30 * time.Second

// objectDeleteInterval paces the sweep that runs without a wake. It is the
// backstop, not the usual path — an ending wakes its own replica — so it is
// sized for the two cases a wake cannot cover: another replica's enqueue, and a
// key whose backoff has just come due. A var so the one rung that must watch
// the interval itself fire need not spend it; never written in production.
var objectDeleteInterval = time.Minute

// objectDeleteClaimLease is how long a claimed batch is held before another
// replica may take it. It has to cover a batch and not a call: the claim is
// taken once for up to objectDeleteBatch keys, and the store calls then run
// sequentially, outside any transaction, never renewed. A lease sized for one
// round trip expires mid-batch against any store having a slow day and hands
// the tail to a second replica — the duplicated round trips the claim exists to
// avoid. What the larger value costs is a sweeper that dies mid-pass: its
// remaining keys wait this long instead of one interval. Neither way loses
// anything, a key deleted twice being nil and a row never dropped.
//
// Distinct from the backoff above, which is what a key gets after a refusal
// rather than while it is being tried. A const, unlike the two above: no rung
// needs it shortened, and a package var nothing writes is state pretending to
// be a knob.
const objectDeleteClaimLease = 5 * time.Minute

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
		// The first pass runs before the first wait, and this is the sweep with
		// the strongest claim to it: the queue holds precisely what some process
		// died before deleting, so a backlog at boot is the normal case rather
		// than the exception — and no wake can announce it, because the enqueue
		// happened in the process that is gone. A replica restarting more often
		// than the interval would otherwise never drain at all.
		//
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
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-q.waits():
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
//
// Each row goes as soon as its own object does, rather than all of them at the
// end of the pass. Batching that write would be one round trip instead of a
// hundred, and would keep every key of a slow batch claimable by a second
// replica for as long as the batch ran: the keys already deleted would be the
// ones sitting there longest, which is precisely the redundant work the claim
// is for. A row removed immediately cannot be claimed by anyone.
func drainPendingObjectDeletes(ctx context.Context, pool *pgxpool.Pool, blobs blob.Store) (done, failed int, err error) {
	claimed, err := claimPendingObjectDeletes(ctx, pool, objectDeleteBatch)
	if err != nil || len(claimed) == 0 {
		return 0, 0, err
	}
	// A pass does not outlive the claim it holds. Past the lease these keys are
	// another replica's to take, and a sweeper still grinding through them
	// would be doing exactly the duplicated work the claim exists to prevent —
	// which a store slow enough to need every one of its per-call budgets would
	// otherwise reach. The bookkeeping below stays on the outer context: a pass
	// that ran out of time must still be able to write down what it learned.
	passCtx, endPass := context.WithTimeout(ctx, objectDeleteClaimLease)
	defer endPass()

	var firstCause error
	for _, key := range claimed {
		if passCtx.Err() != nil {
			break
		}
		callCtx, endCall := context.WithTimeout(passCtx, objectDeleteCallBudget)
		derr := blobs.Delete(callCtx, key)
		endCall()
		if derr != nil {
			// A delete cut short because this process is going down, or because
			// the pass outlived its claim, is not a refusal and must not be
			// recorded as one: the key keeps its claim and comes back, rather
			// than carrying an attempt and a "context canceled" into the column
			// an operator reads to find out what a store actually refused. A
			// call that spent its own budget is a refusal — that is the whole
			// point of giving it one.
			if passCtx.Err() != nil {
				break
			}
			failed++
			if firstCause == nil {
				firstCause = derr
			}
			if ferr := deferPendingObjectDelete(ctx, pool, key, derr); ferr != nil && ctx.Err() == nil {
				slog.WarnContext(ctx, "object delete failure not recorded", "key", key, "error", ferr)
			}
			continue
		}
		if _, derr := pool.Exec(ctx,
			`DELETE FROM pending_object_deletes WHERE object_key = $1`, key); derr != nil {
			// The object is gone and its row is not. The next pass deletes a
			// key that is already missing, which every backend answers nil, so
			// what this costs is one redundant round trip. The count still says
			// the object went, because it did.
			if ctx.Err() == nil {
				slog.WarnContext(ctx, "deleted object still owed a row", "key", key, "error", derr)
			}
		}
		done++
	}
	// Said once for the pass rather than once per key, and said at all: a store
	// refusing everything is otherwise silent here, the only other line on this
	// path being a failure to record a failure. The cap on the backoff is what
	// keeps this to a line an hour for a key nothing can remove.
	if failed > 0 {
		slog.WarnContext(ctx, "objects still owed after a sweep", "count", failed, "error", firstCause)
	}
	return done, failed, nil
}

// claimPendingObjectDeletes takes the due keys, oldest first, and pushes their
// next attempt out so a concurrent replica does not pick up what this pass is
// already working on. The push is the claim: the rows are not locked for the
// duration of the store calls, which happen after this transaction commits.
func claimPendingObjectDeletes(ctx context.Context, pool *pgxpool.Pool, limit int) ([]string, error) {
	// Seconds through make_interval rather than a duration string. Postgres
	// does parse Go's "5m0s" spelling, but a duration that reaches SQL as text
	// is one parser change away from meaning something else, and "m" is the
	// character that means minutes here and months two lines of documentation
	// away. internal/api/apikeylifecycle_test.go made the same choice.
	rows, err := pool.Query(ctx,
		`UPDATE pending_object_deletes SET next_attempt_at = now() + make_interval(secs => $2)
		  WHERE object_key IN (
		        SELECT object_key FROM pending_object_deletes
		         WHERE next_attempt_at <= now()
		         ORDER BY next_attempt_at, enqueued_at
		         LIMIT $1
		         FOR UPDATE SKIP LOCKED)
		  RETURNING object_key`,
		limit, objectDeleteClaimLease.Seconds())
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
//
// The exponent is clamped as well as the product, and the second of those is
// the one anybody writes by reflex. `interval * power(2, attempts)` leaves
// interval range once attempts passes 38, and LEAST cannot cap a product that
// errored on its way to being computed: the whole statement fails, so the
// attempt goes uncounted, the cause unrecorded, and the row keeps only its
// claim. That inverts the cap exactly where it is needed — the key comes back
// far sooner than the cap says, for as long as the outage lasts, with the
// column an operator reads frozen at the last value it could write. Seven
// doublings reach the cap and every attempt after is an hour apart, so one key
// a store refuses for a day and a half arrives there. The clamp sits far above
// the exponent any cap inside interval range can need; its only job is to keep
// the arithmetic inside what an interval can hold.
func deferPendingObjectDelete(ctx context.Context, pool *pgxpool.Pool, key string, cause error) error {
	_, err := pool.Exec(ctx,
		`UPDATE pending_object_deletes
		    SET attempts        = attempts + 1,
		        last_error      = $2,
		        next_attempt_at = now() + LEAST(
		            make_interval(secs => $3) * power(2, LEAST(attempts, 20)),
		            make_interval(secs => $4))
		  WHERE object_key = $1`,
		key, truncateError(cause), objectDeleteBackoffBase.Seconds(), objectDeleteBackoffMax.Seconds())
	return err
}

// truncateError makes a store's error safe to put in a text column, and bounds
// it: a backend is free to return a whole response body, and this column is
// read by an operator asking what went wrong, not by anything that needs the
// tail.
//
// Three ways the value can be bytes a UTF8 database refuses, and all three end
// the same way — the defer UPDATE fails, so the key loses its count and its
// backoff exactly as an arithmetic error would, and what caused that is the
// store message the row exists to carry. Invalid sequences go first, because
// unlike the strings internal/identity's own truncate handles, this one has not
// been through encoding/json and a backend may hand back a raw response body.
// NUL goes with them, and is the one that survives a validity check: U+0000 is
// perfectly good UTF-8 and perfectly unstorable, Postgres being a C program
// whose text values end at the first zero byte. Then the cut is walked back off
// a partial rune, which is the failure truncate documents at length.
func truncateError(err error) string {
	const max = 500
	s := strings.ToValidUTF8(err.Error(), "")
	s = strings.ReplaceAll(s, "\x00", "")
	if len(s) <= max {
		return s
	}
	out := s[:max]
	for len(out) > 0 {
		r, size := utf8.DecodeLastRuneInString(out)
		if r != utf8.RuneError || size > 1 {
			break
		}
		out = out[:len(out)-1]
	}
	return out + "…"
}
