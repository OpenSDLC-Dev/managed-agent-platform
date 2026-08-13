package queue_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// TestASlowRenewalDoesNotShortenTheNextOnesBudget is #392's real defect, found by
// a reviewer attacking the claim that it had none.
//
// The keeper renews on a `ttl/3` ticker and bounds each attempt so it cannot
// outlive the lease it is racing. That bound used to be the constant `ttl-ttl/3`,
// which is only the right number when the tick is punctual. A `time.Ticker`
// buffers one tick and drops the rest, so a renewal that takes longer than an
// interval is followed by a tick that is *already waiting*: the next attempt
// begins the moment the slow one returns, not an interval later. Its deadline
// then landed a third of a lease inside the lease that renewal had just bought,
// and a database slow enough to overrun it made the keeper report a lease it
// still held — abandoning a turn that had streamed a real answer, which is the
// false positive the issue was filed about and which the first version of this
// change wrongly argued was unreachable.
//
// Reproduced rather than reasoned about: a second connection holds the item's row
// with `SELECT … FOR UPDATE`, which is what makes `Extend`'s `UPDATE` wait, so the
// renewal is genuinely slow rather than merely told it is.
//
// The numbers, on a 900ms lease (300ms interval):
//
//   - t=0      the keeper starts; a lock is already held.
//   - t=300    the first renewal ticks and blocks on the lock.
//   - t=700    the lock releases; the renewal succeeds and buys a lease to 1600.
//     the t=600 tick is waiting, so the second renewal starts *now*.
//   - t=700    a second lock is taken, blocking that renewal.
//   - old      its budget was 900-300=600, so it gave up at 1300 — inside a lease
//     good until 1600, with nothing else able to claim the item.
//   - new      its budget is what the lease has left, 900, so it may run to 1600.
//   - t=1200   the lock releases and the renewal succeeds.
//
// The keeper must therefore still hold the lease at the end. Timings are slack in
// the direction that matters: every margin is ≥300ms, and the assertion is about
// the keeper surviving, so a slow host makes the *old* code fail sooner rather
// than making the new code flake.
func TestASlowRenewalDoesNotShortenTheNextOnesBudget(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sessionID, envID := pgtest.NewSession(t, pool, "cloud")
	q := queue.New(pool)

	if _, err := q.Enqueue(ctx, pool, envID, sessionID, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	const ttl = 900 * time.Millisecond
	item, err := q.Claim(ctx, queue.ModelTurn, ttl)
	if err != nil || item == nil {
		t.Fatalf("Claim: item=%v err=%v", item, err)
	}

	// holdRow takes the item's row after `after` and keeps it for `hold`. Both
	// locks are *queued* rather than raced: Postgres serves waiters in order, so
	// starting the second request while the first still holds the row guarantees it
	// is ahead of the renewal that will ask next. Racing them instead lets the
	// second renewal grab the row the instant the first lock lifts, which is how
	// the first version of this test passed against the unfixed keeper.
	holdRow := func(after, hold time.Duration) chan struct{} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			time.Sleep(after)
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Errorf("begin lock tx: %v", err)
				return
			}
			defer func() { _ = tx.Rollback(ctx) }()
			var id string
			if err := tx.QueryRow(ctx,
				`SELECT id FROM work_items WHERE id = $1 FOR UPDATE`, item.ID).Scan(&id); err != nil {
				t.Errorf("lock the row: %v", err)
				return
			}
			time.Sleep(hold)
		}()
		return done
	}

	// Lock A holds from t=0 to t=700, so the renewal that ticks at t=300 blocks on
	// it and lands at t=700 — slow, but successful, buying a lease to t=1600.
	first := holdRow(0, 700*time.Millisecond)
	// Lock B queues at t=350, behind that renewal and ahead of the next one, and
	// holds for 700ms once it gets the row (t=700 to t=1400). The t=600 tick is
	// already waiting, so the second renewal starts at t=700 and waits on B.
	//
	//	old budget: 900-300 = 600ms from t=700 → gives up at t=1300, a full 300ms
	//	            inside a lease nothing else could claim until t=1600.
	//	new budget: what the lease has left, ~900ms → may run to t=1600, and the
	//	            renewal lands at t=1400.
	second := holdRow(350*time.Millisecond, 700*time.Millisecond)

	kctx, keeper := q.KeepLease(ctx, item, ttl)
	<-first
	<-second
	time.Sleep(300 * time.Millisecond) // let the second renewal land

	if kctx.Err() != nil {
		t.Errorf("the keeper abandoned a lease it still held: %v", kctx.Err())
	}
	if err := keeper.Close(); err != nil {
		t.Errorf("Close reported %v; the lease was live throughout", err)
	}
}

// TestATickAfterAWholeLeaseNeverDialsTheDatabase pins the other end of the same
// budget: once subtracting the elapsed time can leave nothing, a tick that late
// must report the lease lost rather than issue an Extend that cannot land.
//
// A microsecond lease makes it deterministic on every platform without a fake
// clock — no timer anywhere delivers a 333ns interval inside a microsecond, so
// by the first tick the lease is long gone. The nil pool is the assertion: it is
// only safe because the branch returns before touching the database, so a keeper
// that tried the doomed Extend anyway would panic here instead of passing.
func TestATickAfterAWholeLeaseNeverDialsTheDatabase(t *testing.T) {
	q := queue.New(nil)
	item := &queue.Item{ID: domain.ID("work_starvedkeeper"), Lease: time.Now()}

	kctx, keeper := q.KeepLease(context.Background(), item, time.Microsecond)
	select {
	case <-kctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the keeper never gave up a lease that had fully elapsed")
	}
	if err := keeper.Close(); !errors.Is(err, queue.ErrLeaseLost) {
		t.Errorf("Close = %v, want %v", err, queue.ErrLeaseLost)
	}
}
