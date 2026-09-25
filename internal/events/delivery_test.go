package events_test

import (
	"context"
	"slices"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A delivered message that wakes its target is written in processing order
// (#793 item 3): the target's running event first, then the received row its
// woken turn consumes — the order all ten recorded report wakes list, and the
// one a spawn already writes. The helpers own the order so no emitter picks
// its own. A delivery that wakes nothing is the received row alone.
func TestDeliveryWritesTheWakeBeforeTheMessage(t *testing.T) {
	ctx := context.Background()
	// deliver runs one helper under the session lock and appends what it
	// returns, reporting the appended types and whether it woke.
	deliver := func(t *testing.T, pool *pgxpool.Pool, sid domain.ID,
		fn func(tx pgx.Tx) ([]events.NewEvent, *domain.SessionStatus, bool, error)) ([]domain.EventType, bool) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, sid.String()); err != nil {
			t.Fatal(err)
		}
		batch, moved, woke, err := fn(tx)
		if err != nil {
			t.Fatal(err)
		}
		appended, err := events.NewLog(pool).AppendInTx(ctx, tx, sid, batch, events.AppendOptions{SetStatus: moved})
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return types(appended), woke
	}
	setStatus := func(t *testing.T, pool *pgxpool.Pool, id domain.ID, status, stop string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE session_threads SET status = $2, stop_reason = $3::jsonb WHERE id = $1`,
			id.String(), status, stop); err != nil {
			t.Fatal(err)
		}
	}
	coordinator := events.ThreadPeer{AgentName: "named"}
	worker := func(id domain.ID) events.ThreadPeer { return events.ThreadPeer{ThreadID: id, AgentName: "worker"} }

	t.Run("a report wakes an idle coordinator", func(t *testing.T) {
		pool := pgtest.NewPool(t)
		sid := newThreadedSession(t, pool)
		child := pgtest.NewChildThread(t, pool, sid)
		setStatus(t, pool, child, "running", "null")
		if _, err := pool.Exec(ctx, `UPDATE sessions SET status = 'running' WHERE id = $1`, sid.String()); err != nil {
			t.Fatal(err)
		}
		_, received, err := events.ThreadMessage(sid, worker(child), coordinator, "done")
		if err != nil {
			t.Fatal(err)
		}
		got, woke := deliver(t, pool, sid, func(tx pgx.Tx) ([]events.NewEvent, *domain.SessionStatus, bool, error) {
			return events.DeliverAndWake(ctx, tx, sid, received)
		})
		// No session.status_running: the reporting child still runs, so the
		// fold already reads running.
		if !woke || !slices.Equal(got, []domain.EventType{domain.EventSessionThreadStatusRunning,
			domain.EventAgentThreadMessageReceived}) {
			t.Errorf("woke %v, appended %v, want the coordinator's running and then the report", woke, got)
		}
	})

	t.Run("a message wakes an idle child of an idle session", func(t *testing.T) {
		pool := pgtest.NewPool(t)
		sid := newThreadedSession(t, pool)
		child := pgtest.NewChildThread(t, pool, sid)
		_, received, err := events.ThreadMessage(sid, coordinator, worker(child), "one more thing")
		if err != nil {
			t.Fatal(err)
		}
		got, woke := deliver(t, pool, sid, func(tx pgx.Tx) ([]events.NewEvent, *domain.SessionStatus, bool, error) {
			return events.DeliverAndWake(ctx, tx, sid, received)
		})
		if !woke || !slices.Equal(got, []domain.EventType{domain.EventSessionStatusRunning,
			domain.EventSessionThreadStatusRunning, domain.EventAgentThreadMessageReceived}) {
			t.Errorf("woke %v, appended %v, want the running pair and then the message", woke, got)
		}
	})

	t.Run("a delivery to a running target wakes nothing", func(t *testing.T) {
		pool := pgtest.NewPool(t)
		sid := newThreadedSession(t, pool)
		child := pgtest.NewChildThread(t, pool, sid)
		setStatus(t, pool, child, "running", "null")
		_, received, err := events.ThreadMessage(sid, coordinator, worker(child), "more")
		if err != nil {
			t.Fatal(err)
		}
		got, woke := deliver(t, pool, sid, func(tx pgx.Tx) ([]events.NewEvent, *domain.SessionStatus, bool, error) {
			return events.DeliverAndWake(ctx, tx, sid, received)
		})
		if woke || !slices.Equal(got, []domain.EventType{domain.EventAgentThreadMessageReceived}) {
			t.Errorf("woke %v, appended %v, want the message alone", woke, got)
		}
	})

	t.Run("an ending notice wakes a parked coordinator", func(t *testing.T) {
		pool := pgtest.NewPool(t)
		sid := newThreadedSession(t, pool)
		setStatus(t, pool, domain.PrimaryThreadID(sid), "idle", `{"type":"end_turn"}`)
		child := pgtest.NewChildThread(t, pool, sid)
		setStatus(t, pool, child, "idle", `{"type":"requires_action","event_ids":[]}`)
		notice, err := events.ThreadEnded(sid, child, "worker", "[agent worker was archived]")
		if err != nil {
			t.Fatal(err)
		}
		got, woke := deliver(t, pool, sid, func(tx pgx.Tx) ([]events.NewEvent, *domain.SessionStatus, bool, error) {
			return events.DeliverThreadEnded(ctx, tx, sid, child, notice)
		})
		if !woke || !slices.Equal(got, []domain.EventType{domain.EventSessionStatusRunning,
			domain.EventSessionThreadStatusRunning, domain.EventAgentThreadMessageReceived}) {
			t.Errorf("woke %v, appended %v, want the running pair and then the notice", woke, got)
		}
	})

	t.Run("an ending with a sibling still working wakes nothing", func(t *testing.T) {
		pool := pgtest.NewPool(t)
		sid := newThreadedSession(t, pool)
		setStatus(t, pool, domain.PrimaryThreadID(sid), "idle", `{"type":"end_turn"}`)
		child := pgtest.NewChildThread(t, pool, sid)
		setStatus(t, pool, child, "running", "null")
		sibling := pgtest.NewChildThread(t, pool, sid)
		setStatus(t, pool, sibling, "running", "null")
		notice, err := events.ThreadEnded(sid, child, "worker", "[agent worker was interrupted]")
		if err != nil {
			t.Fatal(err)
		}
		got, woke := deliver(t, pool, sid, func(tx pgx.Tx) ([]events.NewEvent, *domain.SessionStatus, bool, error) {
			return events.DeliverThreadEnded(ctx, tx, sid, child, notice)
		})
		if woke || !slices.Equal(got, []domain.EventType{domain.EventAgentThreadMessageReceived}) {
			t.Errorf("woke %v, appended %v, want the notice alone", woke, got)
		}
	})
}
