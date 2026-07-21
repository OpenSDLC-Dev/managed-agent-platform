package events_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

const waitTimeout = 10 * time.Second

// subscribeReady subscribes and blocks until LISTEN coverage is active, so
// everything the test publishes afterwards is observable.
func subscribeReady(t *testing.T, b *events.Broker, sid domain.ID) *events.Subscription {
	t.Helper()
	sub := b.Subscribe(sid)
	t.Cleanup(sub.Close)
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	if err := b.Ready(ctx); err != nil {
		t.Fatalf("broker never became ready: %v", err)
	}
	return sub
}

func waitWake(t *testing.T, sub *events.Subscription) {
	t.Helper()
	select {
	case <-sub.Wake():
	case <-time.After(waitTimeout):
		t.Fatal("no wake within timeout")
	}
}

func waitFrame(t *testing.T, sub *events.Subscription) map[string]any {
	t.Helper()
	select {
	case raw := <-sub.Frames():
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("frame is not JSON: %v", err)
		}
		return m
	case <-time.After(waitTimeout):
		t.Fatal("no frame within timeout")
		return nil
	}
}

func drainWakes(sub *events.Subscription) {
	for {
		select {
		case <-sub.Wake():
		default:
			return
		}
	}
}

func TestSubscribeWakesOnAppend(t *testing.T) {
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	broker := events.NewBroker(pool)
	sid := newSession(t, pool)

	sub := subscribeReady(t, broker, sid)
	drainWakes(sub) // the listener's initial heal-wake

	if _, err := log.Append(context.Background(), sid, []events.NewEvent{{Type: domain.EventUserMessage, Payload: text("hi")}}); err != nil {
		t.Fatal(err)
	}
	waitWake(t, sub)
}

func TestFrameFanoutIsPerSession(t *testing.T) {
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	broker := events.NewBroker(pool)
	sid := newSession(t, pool)
	other := newSession(t, pool)

	subA := subscribeReady(t, broker, sid)
	subB := subscribeReady(t, broker, sid)
	subOther := subscribeReady(t, broker, other)

	if err := log.PublishEventFrame(context.Background(), sid, map[string]any{
		"type": "session.deleted", "id": "sevt_x", "processed_at": "2026-07-10T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []*events.Subscription{subA, subB} {
		frame := waitFrame(t, sub)
		if frame["type"] != "session.deleted" {
			t.Errorf("frame type = %v", frame["type"])
		}
	}
	// Dispatch offers frames to every subscriber in one pass; after A and B
	// received, the other session verifiably got nothing.
	select {
	case raw := <-subOther.Frames():
		t.Errorf("other session received frame %s", raw)
	default:
	}
}

func TestPreviewStartAndDeltas(t *testing.T) {
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	broker := events.NewBroker(pool)
	sid := newSession(t, pool)
	ctx := context.Background()

	sub := subscribeReady(t, broker, sid)

	p, err := log.StartPreview(ctx, sid, domain.EventAgentMessage)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p.EventID().String(), "sevt_") {
		t.Errorf("preview pre-allocated id %q lacks sevt_ prefix", p.EventID())
	}

	start := waitFrame(t, sub)
	if start["type"] != "event_start" {
		t.Fatalf("first frame type = %v, want event_start", start["type"])
	}
	preview := start["event"].(map[string]any)
	if preview["id"] != p.EventID().String() || preview["type"] != "agent.message" {
		t.Errorf("event_start preview = %v", preview)
	}

	if err := p.Delta(ctx, 0, "hello "); err != nil {
		t.Fatal(err)
	}
	delta := waitFrame(t, sub)
	if delta["type"] != "event_delta" || delta["event_id"] != p.EventID().String() {
		t.Fatalf("event_delta frame = %v", delta)
	}
	d := delta["delta"].(map[string]any)
	if d["type"] != "content_delta" { // NOT the Messages API's content_block_delta
		t.Errorf("delta type = %v, want content_delta", d["type"])
	}
	if d["index"] != float64(0) {
		t.Errorf("delta index = %v, want 0", d["index"])
	}
	if d["content"].(map[string]any)["text"] != "hello " {
		t.Errorf("delta content = %v", d["content"])
	}

	// The buffered event appends under the preview's id, superseding deltas.
	got, err := log.Append(ctx, sid, []events.NewEvent{{
		ID: p.EventID(), Type: domain.EventAgentMessage, Payload: text("hello world"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].ID != p.EventID() {
		t.Errorf("buffered event id = %s, want preview id %s", got[0].ID, p.EventID())
	}

	// Previews are never persisted: the log holds only the buffered event.
	list, err := log.List(ctx, sid, events.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Type != domain.EventAgentMessage {
		t.Errorf("log contains %d events, want only the buffered agent.message", len(list))
	}
}

func TestPreviewRules(t *testing.T) {
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	sid := newSession(t, pool)
	ctx := context.Background()

	if _, err := log.StartPreview(ctx, sid, domain.EventUserMessage); err == nil {
		t.Error("user.message preview should be rejected")
	}
	thinking, err := log.StartPreview(ctx, sid, domain.EventAgentThinking)
	if err != nil {
		t.Fatalf("agent.thinking preview: %v", err)
	}
	if err := thinking.Delta(ctx, 0, "x"); err == nil {
		t.Error("agent.thinking is start-only; Delta should error")
	}
}

func TestDeltaChunksLongText(t *testing.T) {
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	broker := events.NewBroker(pool)
	sid := newSession(t, pool)
	ctx := context.Background()

	sub := subscribeReady(t, broker, sid)

	p, err := log.StartPreview(ctx, sid, domain.EventAgentMessage)
	if err != nil {
		t.Fatal(err)
	}
	waitFrame(t, sub) // event_start

	long := strings.Repeat("héllo wörld ", 1500) // ~19.5KB escaped, needs >3 NOTIFY frames
	if err := p.Delta(ctx, 2, long); err != nil {
		t.Fatal(err)
	}

	var rebuilt strings.Builder
	frames := 0
	for rebuilt.Len() < len(long) {
		frame := waitFrame(t, sub)
		if frame["type"] != "event_delta" {
			t.Fatalf("frame type = %v", frame["type"])
		}
		d := frame["delta"].(map[string]any)
		if d["index"] != float64(2) {
			t.Fatalf("chunk index = %v, want 2 on every chunk", d["index"])
		}
		rebuilt.WriteString(d["content"].(map[string]any)["text"].(string))
		frames++
	}
	if rebuilt.String() != long {
		t.Error("chunked deltas do not reassemble the original text")
	}
	if frames < 3 {
		t.Errorf("long delta produced %d frames, want several", frames)
	}
}

func TestBrokerCrossPoolAndResubscribe(t *testing.T) {
	poolA := pgtest.NewPool(t)
	broker := events.NewBroker(poolA)
	sid := newSession(t, poolA)

	// First subscription generation.
	sub := subscribeReady(t, broker, sid)
	sub.Close()

	// Second generation after the listener stopped: still works.
	sub2 := subscribeReady(t, broker, sid)
	drainWakes(sub2)

	// A different process (second pool over the same database) appends;
	// NOTIFY crosses connections.
	dsn := poolA.Config().ConnString()
	poolB := newPoolFromDSN(t, dsn)
	if _, err := events.NewLog(poolB).Append(context.Background(), sid,
		[]events.NewEvent{{Type: domain.EventUserMessage, Payload: text("cross")}}); err != nil {
		t.Fatal(err)
	}
	waitWake(t, sub2)
}
