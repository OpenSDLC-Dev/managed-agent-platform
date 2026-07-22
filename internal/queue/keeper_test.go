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
	item, err := q.Claim(ctx, queue.ModelTurn, 600*time.Millisecond)
	if err != nil || item == nil {
		t.Fatalf("Claim: item=%v err=%v", item, err)
	}

	lease0 := currentLease(t, pool, item.ID)
	kctx, keeper := q.KeepLease(ctx, item, 600*time.Millisecond)
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

	kctx, keeper := q.KeepLease(ctx, item, 600*time.Millisecond)

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
