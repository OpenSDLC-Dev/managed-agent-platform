package events_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
)

// Infrastructure failure paths: the log and broker must fail loudly (or heal)
// rather than corrupt the sequence or hang subscribers.

func TestLogFailurePaths(t *testing.T) {
	pool := newPool(t)
	log := events.NewLog(pool)
	sid := newSession(t, pool)
	ctx := context.Background()

	// An unmarshalable broadcast frame errors before touching the database.
	if err := log.PublishEventFrame(ctx, sid, map[string]any{"bad": func() {}}); err == nil {
		t.Error("unmarshalable frame should error")
	}

	// A vanished events table fails the append after the session lock.
	if _, err := pool.Exec(ctx, `ALTER TABLE events RENAME TO events_gone`); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(ctx, sid, []events.NewEvent{{Type: domain.EventUserMessage, Payload: text("x")}}); err == nil {
		t.Error("append without events table should error")
	}
	if _, err := log.List(ctx, sid, events.ListQuery{}); err == nil {
		t.Error("list without events table should error")
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE events_gone RENAME TO events`); err != nil {
		t.Fatal(err)
	}

	// span helpers surface append failures. End on an archived session must
	// return the error (the wire event could not land) while still closing
	// the OTel span with an error status rather than a clean one.
	_, mr, err := log.StartModelRequest(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET archived_at = now() WHERE id = $1`, sid.String()); err != nil {
		t.Fatal(err)
	}
	if err := mr.End(ctx, false, domain.ModelUsage{}); !errors.Is(err, events.ErrSessionArchived) {
		t.Errorf("End on archived session err = %v, want ErrSessionArchived", err)
	}

	// And a deleted session fails the start append.
	if _, err := pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, sid.String()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := log.StartModelRequest(ctx, sid); err == nil {
		t.Error("StartModelRequest on deleted session should error")
	}
}

func TestClosedPoolFailurePaths(t *testing.T) {
	pool := newPool(t)
	log := events.NewLog(pool)
	sid := newSession(t, pool)
	ctx := context.Background()

	_, mr, err := log.StartModelRequest(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := log.StartPreview(ctx, sid, domain.EventAgentMessage)
	if err != nil {
		t.Fatal(err)
	}

	pool.Close()

	if _, err := log.Append(ctx, sid, []events.NewEvent{{Type: domain.EventUserMessage, Payload: text("x")}}); err == nil {
		t.Error("append on closed pool should error")
	}
	if err := mr.End(ctx, false, domain.ModelUsage{}); err == nil {
		t.Error("span end on closed pool should error")
	}
	if err := preview.Delta(ctx, 0, "x"); err == nil {
		t.Error("delta on closed pool should error")
	}
	if _, err := log.StartPreview(ctx, sid, domain.EventAgentThinking); err == nil {
		t.Error("preview on closed pool should error")
	}
}

func TestBrokerSurvivesGarbageNotify(t *testing.T) {
	pool := newPool(t)
	log := events.NewLog(pool)
	broker := events.NewBroker(pool)
	sid := newSession(t, pool)
	ctx := context.Background()

	sub := subscribeReady(t, broker, sid)
	drainWakes(sub)

	// Hand-delivered garbage on both channels must not kill the listener.
	for _, ch := range []string{"map_session_events", "map_session_frames"} {
		if _, err := pool.Exec(ctx, `SELECT pg_notify($1, 'not json')`, ch); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := log.Append(ctx, sid, []events.NewEvent{{Type: domain.EventUserMessage, Payload: text("after garbage")}}); err != nil {
		t.Fatal(err)
	}
	waitWake(t, sub)
}

func TestBrokerReconnectsAfterConnectionLoss(t *testing.T) {
	pool := newPool(t)
	log := events.NewLog(pool)
	broker := events.NewBroker(pool)
	sid := newSession(t, pool)
	ctx := context.Background()

	sub := subscribeReady(t, broker, sid)
	drainWakes(sub)

	// Kill the LISTEN backend server-side; the broker must reconnect and
	// resume delivering wakes.
	if _, err := pool.Exec(ctx,
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		 WHERE query LIKE 'LISTEN %' AND pid <> pg_backend_pid()`); err != nil {
		t.Fatal(err)
	}
	readyCtx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()
	if err := broker.Ready(readyCtx); err != nil {
		t.Fatalf("broker did not re-listen: %v", err)
	}
	drainWakes(sub) // reconnect heal-wake may race the append's own notify
	if _, err := log.Append(ctx, sid, []events.NewEvent{{Type: domain.EventUserMessage, Payload: text("after kill")}}); err != nil {
		t.Fatal(err)
	}
	waitWake(t, sub)
}

func TestBrokerReadyHonorsContext(t *testing.T) {
	pool := newPool(t)
	broker := events.NewBroker(pool)

	// No subscribers → no listener → Ready can only end with the context.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := broker.Ready(ctx); err == nil {
		t.Error("Ready with no listener should time out")
	}
}
