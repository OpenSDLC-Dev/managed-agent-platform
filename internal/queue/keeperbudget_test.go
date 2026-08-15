package queue_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

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
// Every ordering the reproduction depends on is *observed* before the next step
// runs — a lock is held only once its goroutine says so, and a renewal is queued
// behind it only once Postgres reports a backend waiting on a lock. Two earlier
// versions of this test are why. The first raced the locks and passed 6/6 against
// the unfixed keeper; the second replaced the race with sleeps, and a reviewer
// pointed out that a slow enough host would simply not form the intended queue,
// leaving a regression test that silently stops testing. Sleeps here only ever
// set how *long* a lock is held, never who gets the row first.
//
// The numbers, on a 1500ms lease (500ms interval), with t=0 the keeper's start:
//
//   - t=0     lock A already holds the row; the keeper starts.
//   - t=500   the first renewal ticks and blocks on A. Observed, not assumed.
//   - then    lock B queues — behind that renewal, and observed to be waiting
//     before A is released, so the order is A → renewal 1 → B.
//   - t=1100  A releases. Renewal 1 has been blocked 600ms, longer than one
//     interval, so the t=1000 tick is buffered behind it. It commits, buying a
//     lease to ≈2600, and B takes the row next.
//   - t=1100  the buffered tick fires immediately, so renewal 2 starts now and
//     blocks on B.
//     old: budget 1500-500=1000 → gives up at ≈2100, inside a lease good
//     until ≈2600 that nothing else could claim.
//     new: budget is what the lease has left, ≈1500 → may run to ≈2600.
//   - t=2350  B releases and renewal 2 succeeds. That is 250ms past the old
//     deadline and 250ms short of the new one — the widest symmetric margin the
//     `ttl/3` gap between the two budgets allows.
//
// So the keeper must still hold the lease at the end.
func TestASlowRenewalDoesNotShortenTheNextOnesBudget(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sessionID, envID := pgtest.NewSession(t, pool, "cloud")
	q := queue.New(pool)

	if _, err := q.Enqueue(ctx, pool, envID, sessionID, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	const (
		ttl      = 1500 * time.Millisecond
		interval = ttl / 3
	)
	item, err := q.Claim(ctx, queue.ModelTurn, ttl)
	if err != nil || item == nil {
		t.Fatalf("Claim: item=%v err=%v", item, err)
	}

	// Reserved before anything else takes one: the two lock holders and the
	// keeper's own Extend occupy three connections, and the default pool is four,
	// so acquiring this later could block behind the very waits it is watching.
	mon, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire the monitor connection: %v", err)
	}
	defer mon.Release()

	// waitBlocked returns once exactly n backends are waiting on a lock. The
	// database is freshly created for this test, so nothing else can be waiting.
	waitBlocked := func(n int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			var got int
			if err := mon.QueryRow(ctx,
				`SELECT count(*) FROM pg_stat_activity
				 WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&got); err != nil {
				t.Fatalf("count blocked backends: %v", err)
			}
			if got == n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("waited for %d blocked backend(s), saw %d", n, got)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	// Registered before any lock exists, so — cleanups being LIFO — it runs after
	// every release below. Waiting inside each lock's own cleanup deadlocks
	// instead: B's would wait on a goroutine still queued behind A, whose release
	// sits in a cleanup that has not run yet, and the test hangs to its timeout
	// rather than reporting whatever failed first.
	var finished []chan struct{}
	t.Cleanup(func() {
		for _, done := range finished {
			<-done
		}
	})

	// holdRow takes the item's row and holds it until release is called. granted
	// closes once the lock really is held, so callers never guess.
	holdRow := func() (granted <-chan struct{}, release func()) {
		h, rel, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var once sync.Once
		release = func() { once.Do(func() { close(rel) }) }
		go func() {
			defer close(done)
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Errorf("begin lock tx: %v", err)
				close(h)
				return
			}
			defer func() { _ = tx.Rollback(ctx) }()
			var id string
			if err := tx.QueryRow(ctx,
				`SELECT id FROM work_items WHERE id = $1 FOR UPDATE`, item.ID).Scan(&id); err != nil {
				t.Errorf("lock the row: %v", err)
				close(h)
				return
			}
			close(h)
			<-rel
		}()
		finished = append(finished, done)
		// Release only, never wait: a t.Fatal anywhere below would otherwise
		// leave this goroutine parked on rel forever. The waiting is the
		// aggregate cleanup's job, once every holder has let go.
		t.Cleanup(release)
		return h, release
	}

	heldA, releaseA := holdRow()
	<-heldA // the row is locked before the keeper can renew anything

	// No stall budget: this test measures the renewal bound alone, and a holder
	// that reports no progress is exactly what it holds still to do.
	kctx, keeper := q.KeepLease(ctx, item, ttl, 0)

	waitBlocked(1) // the first renewal has issued its UPDATE and is queued behind A
	tick1 := time.Now()

	heldB, releaseB := holdRow()
	waitBlocked(2) // ...and B is queued behind that renewal, before A lets go

	// Hold A past a full interval so the next tick buffers behind renewal 1.
	time.Sleep(time.Until(tick1.Add(interval + interval/5)))
	releaseA()
	<-heldB // renewal 1 has committed (B could not have the row otherwise)

	// Renewal 2 is now running on the buffered tick and blocked on B. Release B
	// after the old budget would have expired but before the new one does: the
	// two deadlines are ttl/3 apart, so half past the earlier one is the widest
	// margin available on both sides.
	time.Sleep(interval * 5 / 2)
	releaseB()
	time.Sleep(300 * time.Millisecond) // let renewal 2 land

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
// A one-nanosecond lease makes that the first tick's situation, and the margin is
// measured rather than assumed. The keeper stamps its anchor before starting the
// goroutine, so what this branch races is anchor → goroutine start → ticker arm →
// first receive; timing exactly that path over 2000 samples on a dev host gives a
// floor of 414ns and a median of 480ns. A nanosecond lease clears the floor by
// ~400x.
//
// The microsecond lease this test used first did not clear it at all: 1µs sits
// *above* a 414ns floor, so the branch this test exists to pin could be skipped
// and the assertion would fail on the pool error instead. A reviewer called that
// thin and the measurement agreed — worth recording, because 1µs reads like the
// more conservative number and is the one to reach for again.
//
// The nanosecond TTL also runs through the sub-3ns fallback, which ticks at the
// TTL itself rather than panicking time.NewTicker.
//
// Genuinely deterministic would mean injecting a clock into LeaseKeeper for one
// defensive branch, which is the sort of single-use abstraction this repo says
// not to add; the residual is stated rather than papered over.
//
// The pool is the assertion: it is closed before the keeper ever sees it, so an
// Extend issued here could not reach a database and fails with an error that is
// not ErrLeaseLost. That keeps the two outcomes distinguishable, and keeps a
// failure a failure — a nil pool asserts the same thing by panicking, which would
// take the whole test binary down with it.
func TestATickAfterAWholeLeaseNeverDialsTheDatabase(t *testing.T) {
	unusable, err := pgxpool.New(context.Background(), "postgres://nobody@127.0.0.1:1/none")
	if err != nil {
		t.Fatalf("build the unusable pool: %v", err)
	}
	unusable.Close()

	q := queue.New(unusable)
	item := &queue.Item{ID: domain.ID("work_starvedkeeper"), Lease: time.Now()}

	kctx, keeper := q.KeepLease(context.Background(), item, time.Nanosecond, 0)
	select {
	case <-kctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the keeper never gave up a lease that had fully elapsed")
	}
	if err := keeper.Close(); !errors.Is(err, queue.ErrLeaseLost) {
		t.Errorf("Close = %v, want %v", err, queue.ErrLeaseLost)
	}
}
