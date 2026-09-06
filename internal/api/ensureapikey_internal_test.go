package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

// setKeyAdoptionLockWait shortens the adoption lock's lock_timeout so the
// wedged-fleet branch resolves in test time, restoring it at test end. Test
// binary only; the hook is unexported, which is why this one test lives inside
// the package rather than beside the rest in ensureapikey_test.go.
func setKeyAdoptionLockWait(t *testing.T, d time.Duration) {
	t.Helper()
	prev := keyAdoptionLockWait
	keyAdoptionLockWait = d
	t.Cleanup(func() { keyAdoptionLockWait = prev })
}

// A fleet of replicas all hash the same configured key and all queue on the
// same advisory lock, and the boot context carries no deadline of its own. So
// an adopter that never gets the lock would wait forever, and the only symptom
// an operator sees is a liveness probe restarting a control plane that never
// says why. Bounded, it fails loudly instead — and the message names the lock,
// because "another replica is still holding it" and "the database is down" are
// different problems.
func TestEnsureAPIKeyBoundsTheAdoptionLockWait(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	setKeyAdoptionLockWait(t, 200*time.Millisecond)

	const key = "ak-wedged-fleet"
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	// The lock EnsureAPIKeyInWorkspace takes, keyed in SQL so this test derives
	// nothing on the Go side.
	if _, err := holder.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, hashKey(key)); err != nil {
		t.Fatalf("take the adoption lock: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- EnsureAPIKeyInWorkspace(ctx, pool, "default", "boot", key) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("EnsureAPIKeyInWorkspace succeeded while another transaction held the adoption lock")
		}
		if !strings.Contains(err.Error(), "adoption lock") {
			t.Errorf("error %q does not name the lock that was not available", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("EnsureAPIKeyInWorkspace never returned; the lock wait is unbounded")
	}
}
