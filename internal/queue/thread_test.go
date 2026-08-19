package queue_test

import (
	"context"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// Turns are (session, thread) under one live-dedup key with NULLS NOT DISTINCT
// (plan 35 decision 5): two primary turns dedupe, two sibling threads' turns do
// not, and the session-keyed exec kinds dedupe under the NULL thread as before.
func TestEnqueueThreadKeysTurnsAndDedupesExecItems(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sid, envID := pgtest.NewSession(t, pool, "cloud")
	q := queue.New(pool)
	child := pgtest.NewChildThread(t, pool, sid)

	for i, want := range []bool{true, false} {
		created, err := q.Enqueue(ctx, pool, envID, sid, queue.ModelTurn)
		if err != nil || created != want {
			t.Fatalf("primary enqueue %d: created=%v err=%v, want %v", i, created, err, want)
		}
	}
	created, err := q.EnqueueThread(ctx, pool, envID, sid, child, queue.ModelTurn)
	if err != nil || !created {
		t.Fatalf("child enqueue beside a live primary turn: created=%v err=%v, want true", created, err)
	}
	created, err = q.EnqueueThread(ctx, pool, envID, sid, child, queue.ModelTurn)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("a second child turn was not deduped")
	}
	for i, want := range []bool{true, false} {
		created, err := q.Enqueue(ctx, pool, envID, sid, queue.ToolExec)
		if err != nil || created != want {
			t.Fatalf("tool_exec enqueue %d: created=%v err=%v, want %v", i, created, err, want)
		}
	}

	// Claims carry the thread; the two turns come out oldest first.
	first, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || first == nil || first.ThreadID != "" {
		t.Fatalf("first claim = %+v err=%v, want the primary's turn", first, err)
	}
	second, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || second == nil || second.ThreadID != child {
		t.Fatalf("second claim = %+v err=%v, want the child's turn", second, err)
	}
	third, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if third != nil {
		t.Errorf("third claim = %+v, want nothing", third)
	}
}

// CancelThread stops one thread's turn and leaves the session's other items —
// the sibling's turn and the shared exec item — live.
func TestCancelThreadLeavesSiblingsAndExecItems(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sid, envID := pgtest.NewSession(t, pool, "cloud")
	q := queue.New(pool)
	a := pgtest.NewChildThread(t, pool, sid)
	b := pgtest.NewChildThread(t, pool, sid)
	for _, th := range []domain.ID{"", a, b} {
		if _, err := q.EnqueueThread(ctx, pool, envID, sid, th, queue.ModelTurn); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.Enqueue(ctx, pool, envID, sid, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	if err := q.CancelThread(ctx, pool, sid, a); err != nil {
		t.Fatal(err)
	}
	var live []string
	rows, err := pool.Query(ctx, `SELECT COALESCE(thread_id, '') || '/' || kind FROM work_items
	  WHERE session_id = $1 AND state <> 'stopped' ORDER BY 1`, sid.String())
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		live = append(live, s)
	}
	rows.Close()
	want := []string{"/model_turn", "/tool_exec", b.String() + "/model_turn"}
	if len(live) != 3 || live[0] != want[0] || live[1] != want[1] || live[2] != want[2] {
		t.Errorf("live after CancelThread(a) = %v, want %v", live, want)
	}
	// The primary's turn by the empty id.
	if err := q.CancelThread(ctx, pool, sid, ""); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM work_items WHERE session_id = $1 AND state <> 'stopped' AND kind = 'model_turn'`, sid.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("live turns after cancelling the primary = %d, want b's alone", n)
	}
}
