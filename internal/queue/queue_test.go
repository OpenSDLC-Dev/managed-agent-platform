package queue_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

func TestMain(m *testing.M) {
	os.Exit(pgtest.Main(m))
}

func TestEnqueueClaimComplete(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sessionID, envID := pgtest.NewSession(t, pool, "cloud")
	q := queue.New(pool)

	created, err := q.Enqueue(ctx, pool, envID, sessionID, queue.ModelTurn)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if !created {
		t.Fatal("first enqueue reported not created")
	}

	item, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if item == nil {
		t.Fatal("Claim returned nil with queued work")
	}
	if item.SessionID != sessionID || item.EnvironmentID != envID || item.Kind != queue.ModelTurn {
		t.Errorf("item = %+v", item)
	}
	if item.Reclaimed {
		t.Error("fresh claim reported as reclaim")
	}
	if !domain.ID(item.ID).HasPrefix("work") {
		t.Errorf("item id %q not work_-prefixed", item.ID)
	}

	if err := q.Complete(ctx, item); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	again, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil {
		t.Fatalf("Claim after complete: %v", err)
	}
	if again != nil {
		t.Errorf("claimed completed work: %+v", again)
	}
}

func TestEnqueueIdempotentWhileLive(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sessionID, envID := pgtest.NewSession(t, pool, "cloud")
	q := queue.New(pool)

	if _, err := q.Enqueue(ctx, pool, envID, sessionID, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	created, err := q.Enqueue(ctx, pool, envID, sessionID, queue.ModelTurn)
	if err != nil {
		t.Fatalf("second Enqueue: %v", err)
	}
	if created {
		t.Error("second enqueue created a duplicate live item")
	}

	// A different kind is independent.
	created, err = q.Enqueue(ctx, pool, envID, sessionID, queue.ToolExec)
	if err != nil {
		t.Fatalf("tool_exec Enqueue: %v", err)
	}
	if !created {
		t.Error("tool_exec enqueue suppressed by live model_turn")
	}

	// While the item is claimed (active) it still suppresses duplicates …
	item, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || item == nil {
		t.Fatalf("Claim: %v %v", item, err)
	}
	created, err = q.Enqueue(ctx, pool, envID, sessionID, queue.ModelTurn)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("enqueue while active created a duplicate")
	}

	// … and completion frees the slot.
	if err := q.Complete(ctx, item); err != nil {
		t.Fatal(err)
	}
	created, err = q.Enqueue(ctx, pool, envID, sessionID, queue.ModelTurn)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Error("enqueue after completion suppressed")
	}
}

func TestClaimIsolatesKindsAndOrdersOldestFirst(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)

	s1, e1 := pgtest.NewSession(t, pool, "cloud")
	s2, e2 := pgtest.NewSession(t, pool, "cloud")
	if _, err := q.Enqueue(ctx, pool, e1, s1, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Enqueue(ctx, pool, e1, s1, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Enqueue(ctx, pool, e2, s2, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}

	first, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || first == nil {
		t.Fatalf("first claim: %+v %v", first, err)
	}
	second, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || second == nil {
		t.Fatalf("second claim: %+v %v", second, err)
	}
	if first.SessionID != s1 || second.SessionID != s2 {
		t.Errorf("claim order: got %s then %s, want %s then %s", first.SessionID, second.SessionID, s1, s2)
	}
	third, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if third != nil {
		t.Errorf("model_turn claim returned tool_exec work: %+v", third)
	}
}

func TestParallelClaimsNeverShareAnItem(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)

	const n = 8
	for range n {
		s, e := pgtest.NewSession(t, pool, "cloud")
		if _, err := q.Enqueue(ctx, pool, e, s, queue.ModelTurn); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	seen := map[domain.ID]bool{}
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			item, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
			if err != nil || item == nil {
				t.Errorf("parallel claim: %+v %v", item, err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if seen[item.ID] {
				t.Errorf("item %s claimed twice", item.ID)
			}
			seen[item.ID] = true
		}()
	}
	wg.Wait()
	if len(seen) != n {
		t.Errorf("claimed %d distinct items, want %d", len(seen), n)
	}
}

func TestExpiredLeaseIsReclaimed(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sessionID, envID := pgtest.NewSession(t, pool, "cloud")
	q := queue.New(pool)

	if _, err := q.Enqueue(ctx, pool, envID, sessionID, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	item, err := q.Claim(ctx, queue.ModelTurn, 50*time.Millisecond)
	if err != nil || item == nil {
		t.Fatalf("claim: %+v %v", item, err)
	}

	// Not expired yet: nothing to claim.
	if got, err := q.Claim(ctx, queue.ModelTurn, time.Minute); err != nil || got != nil {
		t.Fatalf("claim before expiry: %+v %v", got, err)
	}

	time.Sleep(60 * time.Millisecond)
	re, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || re == nil {
		t.Fatalf("claim after expiry: %+v %v", re, err)
	}
	if re.ID != item.ID {
		t.Errorf("reclaimed a different item: %s vs %s", re.ID, item.ID)
	}
	if !re.Reclaimed {
		t.Error("expired-lease claim not flagged as reclaim")
	}

	// The first claimant lost the lease: its Complete must fail loudly.
	if err := q.Complete(ctx, item); err == nil {
		t.Error("Complete after losing the lease succeeded silently")
	} else {
		// The new claimant still owns it.
		if err := q.Complete(ctx, re); err != nil {
			t.Errorf("new claimant Complete: %v", err)
		}
	}
}

func TestExtendRenewsTheLease(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sessionID, envID := pgtest.NewSession(t, pool, "cloud")
	q := queue.New(pool)

	if _, err := q.Enqueue(ctx, pool, envID, sessionID, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	item, err := q.Claim(ctx, queue.ModelTurn, 50*time.Millisecond)
	if err != nil || item == nil {
		t.Fatalf("claim: %+v %v", item, err)
	}
	if err := q.Extend(ctx, item, time.Minute); err != nil {
		t.Fatalf("Extend: %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	if got, err := q.Claim(ctx, queue.ModelTurn, time.Minute); err != nil || got != nil {
		t.Errorf("extended lease was reclaimed: %+v %v", got, err)
	}

	if err := q.Complete(ctx, item); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// Extending a finished item is a lost lease, surfaced as an error.
	if err := q.Extend(ctx, item, time.Minute); err == nil {
		t.Error("Extend after completion succeeded silently")
	}
}

func TestEnqueueUnknownSessionFails(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)
	if _, err := q.Enqueue(ctx, pool, domain.NewID("env"), domain.NewID("sesn"), queue.ModelTurn); err == nil {
		t.Error("enqueue against missing session/environment succeeded")
	}
}
