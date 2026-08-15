package queue_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// currentLease reads the item's lease straight from the row, so a test can watch
// the keeper advance it without racing the keeper goroutine's in-place write to
// item.Lease.
func currentLease(t *testing.T, pool *pgxpool.Pool, id domain.ID) time.Time {
	t.Helper()
	var lease time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT lease_expires_at FROM work_items WHERE id = $1`, id).Scan(&lease); err != nil {
		t.Fatalf("read lease: %v", err)
	}
	return lease
}

// waitForLease polls until cond holds, giving a slow CI generous slack — the
// keeper renews at TTL/3, and these tests assert only that a renewal eventually
// happens, never that it lands within a fixed window.
func waitForLease(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("lease condition not met within 10s")
}

func TestKeepLeaseRenewsWhileHeld(t *testing.T) {
	// A holder that works past TTL/3 must not lose its lease: the keeper renews it
	// in the background, and the renewed value is the ownership proof a settling
	// commit later uses. This is the brain's long-time-to-first-token turn and the
	// executor's slow tool run, tested once at the shared keeper.
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sessionID, envID := pgtest.NewSession(t, pool, "cloud")
	q := queue.New(pool)

	if _, err := q.Enqueue(ctx, pool, envID, sessionID, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	// 1.5s, for the reason the two stall tests below carry the same value: a
	// renewal is bounded by TTL − TTL/3, and a round trip to a Dockerized
	// Postgres on a loaded machine can outrun a 400ms bound — which ends the
	// keeper on its first renewal and reddens this test for a reason that has
	// nothing to do with what it asserts.
	item, err := q.Claim(ctx, queue.ModelTurn, 1500*time.Millisecond)
	if err != nil || item == nil {
		t.Fatalf("Claim: item=%v err=%v", item, err)
	}

	lease0 := currentLease(t, pool, item.ID)
	kctx, keeper := q.KeepLease(ctx, item, 1500*time.Millisecond, 0)
	waitForLease(t, func() bool { return currentLease(t, pool, item.ID).After(lease0) })

	// Checked before Close, which cancels the context itself to release it: a
	// maintained lease must not have cancelled the work out from under the holder.
	if kctx.Err() != nil {
		t.Errorf("work context cancelled under a maintained lease: %v", kctx.Err())
	}
	if err := keeper.Close(); err != nil {
		t.Fatalf("Close after a healthy renewal: %v", err)
	}
}

func TestKeepLeaseEndsAnItemThatStopsMoving(t *testing.T) {
	// The wedge #383 is about: a sandbox call that never returns leaves the holder
	// blocked, the row untouched and the lease renewing forever, so the documented
	// recovery — the lease lapses, another claimant reclaims — never fires, because
	// nothing crashed. A holder that reports no progress for its stall budget has
	// its work cancelled and, crucially, its lease left to lapse: the keeper stops
	// renewing, so the item becomes reclaimable exactly as if the process had died.
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sessionID, envID := pgtest.NewSession(t, pool, "cloud")
	q := queue.New(pool)

	if _, err := q.Enqueue(ctx, pool, envID, sessionID, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	// A 1.5s TTL, not the few hundred milliseconds the budgets themselves need:
	// each renewal is bounded by the lease it races (TTL − TTL/3), and a round
	// trip to a Dockerized Postgres on a loaded machine can outrun a 400ms one —
	// which would cancel this work as a lost lease and test the wrong guard.
	item, err := q.Claim(ctx, queue.ToolExec, 1500*time.Millisecond)
	if err != nil || item == nil {
		t.Fatalf("Claim: item=%v err=%v", item, err)
	}

	kctx, keeper := q.KeepLease(ctx, item, 1500*time.Millisecond, 300*time.Millisecond)
	select {
	case <-kctx.Done(): // the wedged work is cancelled rather than protected
	case <-time.After(10 * time.Second):
		t.Fatal("work context not cancelled after the stall budget elapsed")
	}
	if err := keeper.Close(); !errors.Is(err, queue.ErrWorkStalled) {
		t.Errorf("Close error = %v, want ErrWorkStalled", err)
	}
	// And the lease is abandoned, not extended: a reclaim can now take the item.
	stalled := currentLease(t, pool, item.ID)
	time.Sleep(600 * time.Millisecond)
	if got := currentLease(t, pool, item.ID); !got.Equal(stalled) {
		t.Errorf("the lease moved after the stall (%v -> %v); a stalled item must be left to lapse", stalled, got)
	}
}

func TestKeepLeaseCarriesWorkThatKeepsMoving(t *testing.T) {
	// The other half of the guard, and the one that decides whether it can ship: a
	// long item that is still moving must never be killed for being long. Progress
	// is what the holder reports — a tool finished, a mount landed — not bytes on a
	// wire, so a legitimately slow `bash` inside a healthy run keeps its lease.
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sessionID, envID := pgtest.NewSession(t, pool, "cloud")
	q := queue.New(pool)

	if _, err := q.Enqueue(ctx, pool, envID, sessionID, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	// The TTL is 1.5s for the reason the stall test's is: a renewal is bounded by
	// TTL − TTL/3, and a slow round trip under a 400ms bound would cancel this
	// run as a lost lease — reporting "a moving run was killed" for a reason that
	// has nothing to do with the stall guard.
	item, err := q.Claim(ctx, queue.ToolExec, 1500*time.Millisecond)
	if err != nil || item == nil {
		t.Fatalf("Claim: item=%v err=%v", item, err)
	}

	kctx, keeper := q.KeepLease(ctx, item, 1500*time.Millisecond, time.Second)
	// Report progress well inside the budget for twice its length. Without the
	// reports this run would be cancelled; with them it must not be.
	for i := 0; i < 40; i++ {
		keeper.Progress()
		time.Sleep(50 * time.Millisecond)
		if kctx.Err() != nil {
			t.Fatalf("work cancelled while it was still reporting progress (after %d reports)", i+1)
		}
	}
	if err := keeper.Close(); err != nil {
		t.Errorf("Close after a progressing run: %v", err)
	}
}

func TestKeepLeaseLostCancelsAndReports(t *testing.T) {
	// If the lease is stolen (a second claimant reclaimed the item), the keeper's
	// next renewal matches no row: it cancels the work context so in-flight work
	// aborts, and Close surfaces ErrLeaseLost so the caller commits nothing.
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sessionID, envID := pgtest.NewSession(t, pool, "cloud")
	q := queue.New(pool)

	if _, err := q.Enqueue(ctx, pool, envID, sessionID, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	item, err := q.Claim(ctx, queue.ModelTurn, 600*time.Millisecond)
	if err != nil || item == nil {
		t.Fatalf("Claim: item=%v err=%v", item, err)
	}

	kctx, keeper := q.KeepLease(ctx, item, 600*time.Millisecond, 0)

	// Move the row's lease off the value the keeper holds, so its next Extend
	// finds no matching row — the shape of a reclaim by a second claimant.
	if _, err := pool.Exec(ctx,
		`UPDATE work_items SET lease_expires_at = lease_expires_at + interval '1 second' WHERE id = $1`,
		item.ID); err != nil {
		t.Fatal(err)
	}

	select {
	case <-kctx.Done(): // the keeper cancelled the work on the lost lease
	case <-time.After(10 * time.Second):
		t.Fatal("work context not cancelled after the lease was stolen")
	}
	if err := keeper.Close(); !errors.Is(err, queue.ErrLeaseLost) {
		t.Errorf("Close error = %v, want ErrLeaseLost", err)
	}
}

// TestAStallIsDetectedOnItsOwnBudgetNotTheLeasesThird pins two things the stall
// arm's placement rests on, neither of which any other test would notice
// changing.
//
// One: the stall check and the renewal share a ticker, so a lease far longer
// than the budget used to push detection out with it — at a three-hour TTL the
// tick is an hour, and a wedge the budget says costs half an hour would cost an
// hour and a half, during which a consumer that processes one item at a time
// runs nothing else. The tick is the shorter of the two now.
//
// Two: the check runs BEFORE the renewal on that tick. Move it after and the
// keeper buys the wedged item one more lease interval it cannot use — which
// nothing else here would fail on, since the other stall tests sample the lease
// only after Close, by which time it has stopped moving either way.
//
// Both are read off one run: a 30s lease with a 400ms budget must be cancelled
// in about the budget (a tick of a lease-third would be ten seconds), and the
// row's lease must still be exactly what Claim wrote — never once extended.
func TestAStallIsDetectedOnItsOwnBudgetNotTheLeasesThird(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sessionID, envID := pgtest.NewSession(t, pool, "cloud")
	q := queue.New(pool)

	if _, err := q.Enqueue(ctx, pool, envID, sessionID, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	item, err := q.Claim(ctx, queue.ToolExec, 30*time.Second)
	if err != nil || item == nil {
		t.Fatalf("Claim: item=%v err=%v", item, err)
	}
	claimed := currentLease(t, pool, item.ID)

	kctx, keeper := q.KeepLease(ctx, item, 30*time.Second, 400*time.Millisecond)
	select {
	case <-kctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the wedge was not detected on its own budget: a 400ms budget under a 30s lease waited past 5s, so detection is still riding the lease's third")
	}
	if err := keeper.Close(); !errors.Is(err, queue.ErrWorkStalled) {
		t.Fatalf("Close error = %v, want ErrWorkStalled", err)
	}
	if got := currentLease(t, pool, item.ID); !got.Equal(claimed) {
		t.Errorf("the lease moved to %v from the %v Claim wrote: the stall tick renewed before it checked, buying the wedge another interval", got, claimed)
	}
}
