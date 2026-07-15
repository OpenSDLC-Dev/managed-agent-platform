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

func TestAckTransitionsQueuedToStarting(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)
	sessionID, env := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.Enqueue(ctx, pool, env, sessionID, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	w, err := q.Poll(ctx, env, time.Minute)
	if err != nil || w == nil {
		t.Fatalf("poll: %+v %v", w, err)
	}

	acked, err := q.Ack(ctx, env, w.ID)
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	if acked.State != "starting" {
		t.Errorf("state after ack = %q, want starting", acked.State)
	}
	if acked.AcknowledgedAt == nil {
		t.Error("acknowledged_at not set by ack")
	}
	// Idempotent: re-ack keeps starting and does not re-stamp acknowledged_at.
	first := *acked.AcknowledgedAt
	again, err := q.Ack(ctx, env, w.ID)
	if err != nil {
		t.Fatalf("re-ack: %v", err)
	}
	if again.State != "starting" || again.AcknowledgedAt == nil || !again.AcknowledgedAt.Equal(first) {
		t.Errorf("re-ack not idempotent: state=%q acked=%v (want starting, %v)", again.State, again.AcknowledgedAt, first)
	}

	// Unknown work id, and a real id under the wrong environment, are not-found.
	if _, err := q.Ack(ctx, env, domain.NewID("work")); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("ack unknown = %v, want ErrWorkNotFound", err)
	}
	_, otherEnv := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.Ack(ctx, otherEnv, w.ID); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("ack wrong env = %v, want ErrWorkNotFound", err)
	}
}

func TestHeartbeatClaimsAndExtendsWithOptimisticConcurrency(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)
	sessionID, env := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.Enqueue(ctx, pool, env, sessionID, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	w, _ := q.Poll(ctx, env, time.Minute)

	// A heartbeat before ack (still queued) cannot claim: mismatch.
	if _, err := q.Heartbeat(ctx, env, w.ID, queue.NoHeartbeat, 30); !errors.Is(err, queue.ErrHeartbeatMismatch) {
		t.Fatalf("heartbeat before ack = %v, want ErrHeartbeatMismatch", err)
	}
	if _, err := q.Ack(ctx, env, w.ID); err != nil {
		t.Fatal(err)
	}

	// First heartbeat claims the lease: starting → active.
	hb1, err := q.Heartbeat(ctx, env, w.ID, queue.NoHeartbeat, 30)
	if err != nil {
		t.Fatalf("first heartbeat: %v", err)
	}
	if hb1.State != "active" || !hb1.LeaseExtended || hb1.TTLSeconds != 30 {
		t.Errorf("first heartbeat = %+v, want active/extended/ttl 30", hb1)
	}
	got, _ := q.GetWork(ctx, env, w.ID)
	if got.StartedAt == nil {
		t.Error("started_at not set by first heartbeat")
	}

	// A second NO_HEARTBEAT is rejected — the lease is already claimed.
	if _, err := q.Heartbeat(ctx, env, w.ID, queue.NoHeartbeat, 30); !errors.Is(err, queue.ErrHeartbeatMismatch) {
		t.Errorf("re-claim = %v, want ErrHeartbeatMismatch", err)
	}
	// A stale/wrong expected value is rejected.
	if _, err := q.Heartbeat(ctx, env, w.ID, "2000-01-01T00:00:00Z", 30); !errors.Is(err, queue.ErrHeartbeatMismatch) {
		t.Errorf("wrong expected = %v, want ErrHeartbeatMismatch", err)
	}
	// Echoing the server's prior last_heartbeat extends the lease and rolls it.
	hb2, err := q.Heartbeat(ctx, env, w.ID, hb1.LastHeartbeat.Format(time.RFC3339Nano), 45)
	if err != nil {
		t.Fatalf("second heartbeat: %v", err)
	}
	if hb2.State != "active" || !hb2.LeaseExtended || !hb2.LastHeartbeat.After(hb1.LastHeartbeat) {
		t.Errorf("second heartbeat = %+v (prev %v), want active/extended/rolled", hb2, hb1.LastHeartbeat)
	}
	// The old value no longer matches (optimistic concurrency).
	if _, err := q.Heartbeat(ctx, env, w.ID, hb1.LastHeartbeat.Format(time.RFC3339Nano), 30); !errors.Is(err, queue.ErrHeartbeatMismatch) {
		t.Errorf("replay of superseded heartbeat = %v, want ErrHeartbeatMismatch", err)
	}
}

func TestHeartbeatOnStoppingLearnsWithoutExtending(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)
	sessionID, env := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.Enqueue(ctx, pool, env, sessionID, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	w, _ := q.Poll(ctx, env, time.Minute)
	if _, err := q.Ack(ctx, env, w.ID); err != nil {
		t.Fatal(err)
	}
	hb, err := q.Heartbeat(ctx, env, w.ID, queue.NoHeartbeat, 30)
	if err != nil {
		t.Fatal(err)
	}
	// Control plane requests a graceful stop.
	if _, err := q.Stop(ctx, env, w.ID, false); err != nil {
		t.Fatalf("graceful stop: %v", err)
	}
	// The worker's next heartbeat (echoing the prior value) still matches, but
	// the item is stopping: it learns the state and the lease is not extended.
	after, err := q.Heartbeat(ctx, env, w.ID, hb.LastHeartbeat.Format(time.RFC3339Nano), 30)
	if err != nil {
		t.Fatalf("heartbeat on stopping: %v", err)
	}
	if after.State != "stopping" || after.LeaseExtended {
		t.Errorf("heartbeat on stopping = %+v, want stopping/not-extended", after)
	}
}

func TestStopForceAndGraceful(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)

	// Graceful stop of a live item → stopping; force then escalates → stopped.
	sessionID, env := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.Enqueue(ctx, pool, env, sessionID, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	w, _ := q.Poll(ctx, env, time.Minute)
	// Stop returns the updated item (the wire responds with the work object, not 204).
	stopped, err := q.Stop(ctx, env, w.ID, false)
	if err != nil {
		t.Fatalf("graceful stop: %v", err)
	}
	if stopped.State != "stopping" || stopped.StopRequestedAt == nil {
		t.Errorf("graceful stop returned %+v, want stopping with stop_requested_at", stopped)
	}
	// Re-graceful-stopping a stopping item is a conflict.
	if _, err := q.Stop(ctx, env, w.ID, false); !errors.Is(err, queue.ErrWorkConflict) {
		t.Errorf("re-graceful-stop = %v, want ErrWorkConflict", err)
	}
	// force escalates stopping → stopped.
	stopped, err = q.Stop(ctx, env, w.ID, true)
	if err != nil {
		t.Fatalf("force stop: %v", err)
	}
	if stopped.State != "stopped" || stopped.StoppedAt == nil {
		t.Errorf("force stop returned %+v, want stopped with stopped_at", stopped)
	}
	// Stopping an already-stopped item is a conflict.
	if _, err := q.Stop(ctx, env, w.ID, true); !errors.Is(err, queue.ErrWorkConflict) {
		t.Errorf("stop of stopped = %v, want ErrWorkConflict", err)
	}
	// A missing item is not-found.
	if _, err := q.Stop(ctx, env, domain.NewID("work"), true); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("stop unknown = %v, want ErrWorkNotFound", err)
	}
}

func TestGetWorkScopingAndFields(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)
	sessionID, env := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.Enqueue(ctx, pool, env, sessionID, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	w, _ := q.Poll(ctx, env, time.Minute)

	got, err := q.GetWork(ctx, env, w.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SessionID != sessionID || got.State != "queued" {
		t.Errorf("got = %+v", got)
	}
	// A queued item has reached no lifecycle timestamp.
	if got.AcknowledgedAt != nil || got.StartedAt != nil || got.StopRequestedAt != nil || got.StoppedAt != nil || got.LastHeartbeat != nil {
		t.Errorf("queued item has non-null lifecycle timestamps: %+v", got)
	}
	if _, err := q.GetWork(ctx, env, domain.NewID("work")); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("get unknown = %v, want ErrWorkNotFound", err)
	}
	_, otherEnv := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.GetWork(ctx, otherEnv, w.ID); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("get wrong env = %v, want ErrWorkNotFound", err)
	}
}

// TestUpdateMetadataPatches pins the work-item metadata patch: a string value
// upserts a key, an explicit delete removes it, absent keys are preserved, and
// the merge is atomic (a single UPDATE, no read-modify-write). It is scoped like
// the rest of the work API and leaves lifecycle state and timestamps untouched.
func TestUpdateMetadataPatches(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)
	sessionID, env := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.Enqueue(ctx, pool, env, sessionID, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	w, _ := q.Poll(ctx, env, time.Minute)

	// Upsert two keys onto the default-empty metadata.
	got, err := q.UpdateMetadata(ctx, env, w.ID, map[string]string{"a": "1", "b": "2"}, nil)
	if err != nil {
		t.Fatalf("update upsert: %v", err)
	}
	if len(got.Metadata) != 2 || got.Metadata["a"] != "1" || got.Metadata["b"] != "2" {
		t.Errorf("after upsert metadata = %v, want a=1 b=2", got.Metadata)
	}
	// The patch must not transition the item: still queued after a poll.
	if got.State != "queued" || got.AcknowledgedAt != nil || got.StartedAt != nil {
		t.Errorf("metadata update disturbed lifecycle: %+v", got)
	}

	// A mixed patch: upsert a, delete b, add c.
	got, err = q.UpdateMetadata(ctx, env, w.ID, map[string]string{"a": "9", "c": "3"}, []string{"b"})
	if err != nil {
		t.Fatalf("update mixed: %v", err)
	}
	if len(got.Metadata) != 2 || got.Metadata["a"] != "9" || got.Metadata["c"] != "3" {
		t.Errorf("after mixed patch metadata = %v, want a=9 c=3 (b deleted)", got.Metadata)
	}

	// An empty patch is a no-op that still returns the item.
	got, err = q.UpdateMetadata(ctx, env, w.ID, map[string]string{}, nil)
	if err != nil || len(got.Metadata) != 2 {
		t.Errorf("empty patch = %+v %v, want unchanged", got.Metadata, err)
	}

	// A nil patch (nil upserts AND nil deletes) is also a no-op, not corruption:
	// the guards turn nil upserts into {} (else `metadata || 'null'` coerces the
	// object into a JSON array) and nil deletes into an empty text[] (else
	// `metadata - NULL` nulls the NOT NULL column). This exercises those guards,
	// which no other caller reaches.
	got, err = q.UpdateMetadata(ctx, env, w.ID, nil, nil)
	if err != nil {
		t.Fatalf("nil patch: %v", err)
	}
	if len(got.Metadata) != 2 || got.Metadata["a"] != "9" || got.Metadata["c"] != "3" {
		t.Errorf("nil patch corrupted metadata = %v, want a=9 c=3 unchanged", got.Metadata)
	}

	// Scoping: an unknown id and a wrong-env id are both ErrWorkNotFound.
	if _, err := q.UpdateMetadata(ctx, env, domain.NewID("work"), map[string]string{"x": "1"}, nil); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("update unknown = %v, want ErrWorkNotFound", err)
	}
	_, otherEnv := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.UpdateMetadata(ctx, otherEnv, w.ID, map[string]string{"x": "1"}, nil); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("update wrong env = %v, want ErrWorkNotFound", err)
	}
}

// TestPollReclaimsExpiredLeases pins the reclaim scope: a still-queued (un-acked)
// reservation whose window lapsed is re-offered, AND an acked (`starting`) or
// heartbeating (`active`) item whose lease has lapsed (a dead worker) is
// reclaimed — reset to a fresh queued reservation so the next worker can re-poll,
// re-ack, and re-claim it (NO_HEARTBEAT needs last_heartbeat cleared). An item
// whose lease is still LIVE is never reclaimed: its worker still owns it.
func TestPollReclaimsExpiredLeases(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)
	sessionID, env := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.Enqueue(ctx, pool, env, sessionID, queue.ToolExec); err != nil {
		t.Fatal(err)
	}

	// A queued reservation whose window lapsed is re-offered on the next poll.
	first, _ := q.Poll(ctx, env, -time.Second) // reservation already expired
	if first == nil {
		t.Fatal("first poll returned nil")
	}
	again, err := q.Poll(ctx, env, time.Minute)
	if err != nil || again == nil || again.ID != first.ID {
		t.Fatalf("lapsed queued reservation not re-offered: %+v %v", again, err)
	}

	expireLease := func(id domain.ID) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`UPDATE work_items SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}

	// Ack it (queued→starting) and give it a live lease via a first heartbeat
	// (starting→active). While the lease is LIVE, poll must not reclaim it.
	if _, err := q.Ack(ctx, env, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Heartbeat(ctx, env, first.ID, queue.NoHeartbeat, 30); err != nil {
		t.Fatal(err)
	}
	if w, err := q.Poll(ctx, env, time.Minute); err != nil || w != nil {
		t.Errorf("poll reclaimed a live-leased active item = %+v %v, want nil (still owned)", w, err)
	}

	// The worker dies: its lease lapses. Poll now reclaims the active item,
	// resetting it so it re-enters the poll→ack→NO_HEARTBEAT-claim flow.
	expireLease(first.ID)
	reclaimed, err := q.Poll(ctx, env, time.Minute)
	if err != nil || reclaimed == nil || reclaimed.ID != first.ID {
		t.Fatalf("expired active item not reclaimed: %+v %v", reclaimed, err)
	}
	if _, err := q.Ack(ctx, env, reclaimed.ID); err != nil {
		t.Fatalf("reclaimed item cannot be re-acked: %v", err)
	}
	if _, err := q.Heartbeat(ctx, env, reclaimed.ID, queue.NoHeartbeat, 30); err != nil {
		t.Fatalf("reclaimed item cannot be re-claimed with NO_HEARTBEAT: %v", err)
	}

	// A freshly-acked `starting` item (a worker that has not sent its first
	// heartbeat yet) must NOT be reclaimed just because its un-acked poll
	// reservation lapsed: Ack installs a startup lease, so a slow-but-live worker
	// keeps its item. Poll it with an already-expired reservation, ack, and
	// confirm the next poll does not steal it.
	sid2, env2 := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.Enqueue(ctx, pool, env2, sid2, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	w2, _ := q.Poll(ctx, env2, -time.Second) // reservation already expired
	if _, err := q.Ack(ctx, env2, w2.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := q.Poll(ctx, env2, time.Minute); err != nil || got != nil {
		t.Errorf("poll reclaimed a freshly-acked starting item = %+v %v, want nil (startup lease protects it)", got, err)
	}

	// Once that startup lease lapses — a worker that died between ack and its
	// first heartbeat — the starting item is reclaimed.
	expireLease(w2.ID)
	if got, err := q.Poll(ctx, env2, time.Minute); err != nil || got == nil || got.ID != w2.ID {
		t.Fatalf("expired starting item not reclaimed: %+v %v", got, err)
	}
}

// TestLifecycleEndpointsRejectModelTurn pins that a worker's environment-key
// endpoints cannot see or mutate the brain's model_turn rows (which share the
// work_items table): acking one would wedge the brain's turn.
func TestLifecycleEndpointsRejectModelTurn(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)
	sessionID, env := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.Enqueue(ctx, pool, env, sessionID, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	var mtID domain.ID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM work_items WHERE session_id = $1 AND kind = 'model_turn'`, sessionID).Scan(&mtID); err != nil {
		t.Fatal(err)
	}

	if _, err := q.GetWork(ctx, env, mtID); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("GetWork(model_turn) = %v, want ErrWorkNotFound", err)
	}
	if _, err := q.Ack(ctx, env, mtID); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("Ack(model_turn) = %v, want ErrWorkNotFound", err)
	}
	if _, err := q.Heartbeat(ctx, env, mtID, queue.NoHeartbeat, 30); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("Heartbeat(model_turn) = %v, want ErrWorkNotFound", err)
	}
	if _, err := q.Stop(ctx, env, mtID, true); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("Stop(model_turn) = %v, want ErrWorkNotFound", err)
	}
	if _, err := q.UpdateMetadata(ctx, env, mtID, map[string]string{"x": "1"}, nil); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("UpdateMetadata(model_turn) = %v, want ErrWorkNotFound", err)
	}
	// The brain's item is untouched: still queued and claimable by the brain.
	it, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || it == nil || it.ID != mtID {
		t.Fatalf("model_turn item was disturbed by the work API: claim=%+v err=%v", it, err)
	}
}

// TestLifecycleEndpointsRejectCloudToolExec pins that a cloud environment's
// tool_exec item — the platform executor's work, not a worker's — is invisible
// to the work API even though it is tool_exec, so an errant cloud env key cannot
// yank it from the executor mid-run.
func TestLifecycleEndpointsRejectCloudToolExec(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)
	sessionID, cloudEnv := pgtest.NewSession(t, pool, "cloud")
	if _, err := q.Enqueue(ctx, pool, cloudEnv, sessionID, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	var id domain.ID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM work_items WHERE session_id = $1 AND kind = 'tool_exec'`, sessionID).Scan(&id); err != nil {
		t.Fatal(err)
	}

	if _, err := q.GetWork(ctx, cloudEnv, id); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("GetWork(cloud tool_exec) = %v, want ErrWorkNotFound", err)
	}
	if _, err := q.Ack(ctx, cloudEnv, id); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("Ack(cloud tool_exec) = %v, want ErrWorkNotFound", err)
	}
	if _, err := q.Stop(ctx, cloudEnv, id, true); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("Stop(cloud tool_exec) = %v, want ErrWorkNotFound", err)
	}
	if _, err := q.UpdateMetadata(ctx, cloudEnv, id, map[string]string{"x": "1"}, nil); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("UpdateMetadata(cloud tool_exec) = %v, want ErrWorkNotFound", err)
	}
	// Poll serves only self_hosted, so a cloud environment yields nothing.
	if w, err := q.Poll(ctx, cloudEnv, time.Minute); err != nil || w != nil {
		t.Errorf("Poll(cloud env) = %+v %v, want nil (cloud is the executor's)", w, err)
	}
	// ListWork is scoped the same way: a cloud env's tool_exec never lists.
	if items, err := q.ListWork(ctx, cloudEnv, false, time.Time{}, "", 10); err != nil || len(items) != 0 {
		t.Errorf("ListWork(cloud env) = %d items %v, want 0 (cloud is the executor's)", len(items), err)
	}
	// The executor can still claim it — it was never disturbed.
	if it, err := q.Claim(ctx, queue.ToolExec, time.Minute); err != nil || it == nil || it.ID != id {
		t.Fatalf("cloud tool_exec disturbed by the work API: claim=%+v err=%v", it, err)
	}
}

// TestHeartbeatMissingAndMalformed pins two error mappings: a heartbeat on an
// absent item is not-found (404), distinct from a mismatch, and a malformed
// expected value is a mismatch (412), never a 500 from a failed SQL cast.
func TestHeartbeatMissingAndMalformed(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)
	sessionID, env := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.Enqueue(ctx, pool, env, sessionID, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	w, _ := q.Poll(ctx, env, time.Minute)
	if _, err := q.Ack(ctx, env, w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Heartbeat(ctx, env, w.ID, queue.NoHeartbeat, 30); err != nil {
		t.Fatal(err)
	}

	// An absent item is not-found (so a worker can tell "stale, retry" from "gone").
	if _, err := q.Heartbeat(ctx, env, domain.NewID("work"), "2026-01-01T00:00:00Z", 30); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("heartbeat on missing item = %v, want ErrWorkNotFound", err)
	}
	// A malformed expected value is a mismatch, never a cast-error 500.
	if _, err := q.Heartbeat(ctx, env, w.ID, "not-a-timestamp", 30); !errors.Is(err, queue.ErrHeartbeatMismatch) {
		t.Errorf("heartbeat with malformed expected = %v, want ErrHeartbeatMismatch", err)
	}
	// A valid-but-stale value on a live item is still a mismatch.
	if _, err := q.Heartbeat(ctx, env, w.ID, "2000-01-01T00:00:00Z", 30); !errors.Is(err, queue.ErrHeartbeatMismatch) {
		t.Errorf("heartbeat with stale expected = %v, want ErrHeartbeatMismatch", err)
	}
}
