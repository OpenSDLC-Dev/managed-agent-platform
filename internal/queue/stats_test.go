package queue_test

import (
	"context"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// TestWorkStats pins the work-queue statistics: depth/pending partition the
// queued state by whether a poll reservation is live, an acked item leaves the
// queue entirely, oldest_queued_at tracks the oldest queued item, workers_polling
// counts distinct in-window pollers (and a poll's reap physically removes rows
// aged past the window so the table stays bounded), and everything — including
// workers_polling — is scoped to the environment's own self_hosted tool_exec
// queue.
func TestWorkStats(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)
	s1, env := pgtest.NewSession(t, pool, "self_hosted")

	// An empty queue reports zeros and a null oldest_queued_at.
	st, err := q.Stats(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	if st.Depth != 0 || st.Pending != 0 || st.OldestQueuedAt != nil || st.WorkersPolling != 0 {
		t.Fatalf("empty stats = %+v, want zeros and nil oldest", st)
	}

	// Two queued items (two sessions — Enqueue dedupes per session). Both are
	// unreserved → depth 2, pending 0, oldest set.
	s2 := pgtest.NewSessionInEnv(t, pool, env)
	if _, err := q.Enqueue(ctx, pool, env, s1, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Enqueue(ctx, pool, env, s2, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	st, _ = q.Stats(ctx, env)
	if st.Depth != 2 || st.Pending != 0 {
		t.Errorf("after enqueue: depth=%d pending=%d, want 2/0", st.Depth, st.Pending)
	}
	if st.OldestQueuedAt == nil {
		t.Error("oldest_queued_at is nil, want the oldest queued item's created_at")
	}

	// Poll one → it stays queued with a live reservation: moves depth→pending.
	w, err := q.Poll(ctx, env, time.Minute)
	if err != nil || w == nil {
		t.Fatalf("poll: %v (item %v)", err, w)
	}
	st, _ = q.Stats(ctx, env)
	if st.Depth != 1 || st.Pending != 1 {
		t.Errorf("after poll: depth=%d pending=%d, want 1/1 (one reserved)", st.Depth, st.Pending)
	}

	// Ack it → starting: the acked item leaves the queue, counted by neither.
	if _, err := q.Ack(ctx, env, w.ID); err != nil {
		t.Fatal(err)
	}
	st, _ = q.Stats(ctx, env)
	if st.Depth != 1 || st.Pending != 0 || st.OldestQueuedAt == nil {
		t.Errorf("after ack: depth=%d pending=%d oldest=%v, want 1/0 with the remaining item's oldest", st.Depth, st.Pending, st.OldestQueuedAt)
	}

	// model_turn (the brain's queue) is never the worker's queue.
	mturn := pgtest.NewSessionInEnv(t, pool, env)
	if _, err := q.Enqueue(ctx, pool, env, mturn, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	st, _ = q.Stats(ctx, env)
	if st.Depth != 1 {
		t.Errorf("model_turn leaked into stats: depth=%d, want 1", st.Depth)
	}

	// workers_polling: distinct workers within the window; a re-poll dedupes.
	for _, wid := range []string{"worker-a", "worker-a", "worker-b"} {
		if err := q.RecordPoll(ctx, env, wid); err != nil {
			t.Fatal(err)
		}
	}
	st, _ = q.Stats(ctx, env)
	if st.WorkersPolling != 2 {
		t.Errorf("workers_polling = %d, want 2 (a re-poll dedupes, a and b distinct)", st.WorkersPolling)
	}

	// A worker whose last poll predates the window ages out of the count.
	if _, err := pool.Exec(ctx,
		`UPDATE worker_polls SET last_polled_at = now() - make_interval(secs => 60)
		 WHERE environment_id = $1 AND worker_id = 'worker-a'`, env); err != nil {
		t.Fatal(err)
	}
	st, _ = q.Stats(ctx, env)
	if st.WorkersPolling != 1 {
		t.Errorf("workers_polling after aging worker-a = %d, want 1 (only worker-b in window)", st.WorkersPolling)
	}

	// The reap bounds the table: any subsequent poll deletes the environment's
	// rows aged past the window, so worker-a's stale row is physically removed
	// rather than lingering forever (default worker ids are minted fresh per
	// process, so unreaped rows would leak one per restart).
	if err := q.RecordPoll(ctx, env, "worker-b"); err != nil {
		t.Fatal(err)
	}
	var stale int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM worker_polls WHERE environment_id = $1 AND worker_id = 'worker-a'`, env).
		Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale != 0 {
		t.Errorf("stale worker-a row not reaped: %d rows remain", stale)
	}

	// Scoping: another self_hosted environment's queue and pollers stay separate.
	os1, otherEnv := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.Enqueue(ctx, pool, otherEnv, os1, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	if err := q.RecordPoll(ctx, otherEnv, "worker-a"); err != nil {
		t.Fatal(err)
	}
	st, _ = q.Stats(ctx, env)
	if st.Depth != 1 || st.WorkersPolling != 1 {
		t.Errorf("cross-env leak: depth=%d workers=%d, want 1/1", st.Depth, st.WorkersPolling)
	}

	// A self_hosted environment with no queued work but a live poller: depth,
	// pending, and oldest are empty while workers_polling still reflects the
	// worker — the four fields are computed independently.
	_, idleEnv := pgtest.NewSession(t, pool, "self_hosted")
	if err := q.RecordPoll(ctx, idleEnv, "worker-idle"); err != nil {
		t.Fatal(err)
	}
	st, _ = q.Stats(ctx, idleEnv)
	if st.Depth != 0 || st.Pending != 0 || st.OldestQueuedAt != nil || st.WorkersPolling != 1 {
		t.Errorf("idle-with-poller stats = %+v, want depth 0/pending 0/oldest nil/workers 1", st)
	}

	// A cloud environment's tool_exec work is the platform executor's, not the
	// worker's queue: every field is scoped away to zero — including
	// workers_polling, whose subquery carries the same self_hosted gate, so a
	// stray poll recorded against a cloud env never inflates the count.
	cs, cloudEnv := pgtest.NewSession(t, pool, "cloud")
	if _, err := q.Enqueue(ctx, pool, cloudEnv, cs, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	if err := q.RecordPoll(ctx, cloudEnv, "worker-cloud"); err != nil {
		t.Fatal(err)
	}
	st, _ = q.Stats(ctx, cloudEnv)
	if st.Depth != 0 || st.Pending != 0 || st.OldestQueuedAt != nil || st.WorkersPolling != 0 {
		t.Errorf("cloud env stats = %+v, want all zero incl. workers_polling (not the worker's queue)", st)
	}
}
