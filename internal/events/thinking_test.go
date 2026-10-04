package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

// appendThinkingEvent appends one content-free agent.thinking event, as the
// brain does while a turn streams, and returns its id.
func appendThinkingEvent(t *testing.T, log *events.Log, sid domain.ID) domain.ID {
	t.Helper()
	evs, err := log.Append(context.Background(), sid, []events.NewEvent{{Type: domain.EventAgentThinking}})
	if err != nil {
		t.Fatal(err)
	}
	return evs[0].ID
}

// A kept block comes back byte for byte: a signature covers the bytes the
// model sent, so the store may not normalize them. The fixture carries what
// jsonb would refuse (the \u0000 escape) and what it would rewrite (spacing
// and key order).
func TestThinkingBlocksRoundTripVerbatim(t *testing.T) {
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	sid := newSession(t, pool)
	id := appendThinkingEvent(t, log, sid)
	block := json.RawMessage(`{"type": "thinking",  "thinking":"a\u0000<b>","signature":"s1"}`)

	if _, err := log.AppendWith(context.Background(), sid,
		[]events.NewEvent{{Type: domain.EventAgentMessage, Payload: text("answer")}},
		events.AppendOptions{Thinking: []events.ThinkingBlock{
			{EventID: id, Model: "m", PrefixDigest: "d", Block: block},
		}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, err := log.ThinkingBlocks(context.Background(), sid, "")
	if err != nil {
		t.Fatal(err)
	}
	b, ok := got[id]
	if len(got) != 1 || !ok {
		t.Fatalf("blocks = %v, want one under %s", got, id)
	}
	if string(b.Block) != string(block) || b.Model != "m" || b.PrefixDigest != "d" || b.EventID != id {
		t.Errorf("block = %+v (%s), want the bytes written", b, b.Block)
	}
}

// Blocks are written by the settlement's own transaction: an append that rolls
// back keeps none.
func TestThinkingBlocksRollBackWithTheirAppend(t *testing.T) {
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	sid := newSession(t, pool)
	id := appendThinkingEvent(t, log, sid)

	_, err := log.AppendWith(context.Background(), sid,
		[]events.NewEvent{{Type: domain.EventAgentMessage, Payload: text("answer")}},
		events.AppendOptions{
			Thinking: []events.ThinkingBlock{{EventID: id, Model: "m", PrefixDigest: "d",
				Block: json.RawMessage(`{"type":"thinking","thinking":"x","signature":"s"}`)}},
			Then: func(context.Context, pgx.Tx) error { return errors.New("lease lost") },
		})
	if err == nil {
		t.Fatal("append succeeded, want the Then error")
	}
	got, err := log.ThinkingBlocks(context.Background(), sid, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("a rolled-back append kept %d blocks", len(got))
	}
}

// Blocks go with their event, and so with their session.
func TestThinkingBlocksGoWithTheirSession(t *testing.T) {
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	sid := newSession(t, pool)
	id := appendThinkingEvent(t, log, sid)
	if _, err := log.AppendWith(context.Background(), sid,
		[]events.NewEvent{{Type: domain.EventAgentMessage, Payload: text("answer")}},
		events.AppendOptions{Thinking: []events.ThinkingBlock{{EventID: id, Model: "m", PrefixDigest: "d",
			Block: json.RawMessage(`{"type":"redacted_thinking","data":"x"}`)}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM sessions WHERE id = $1`, sid.String()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM thinking_blocks WHERE event_id = $1`, id.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d blocks outlived their session", n)
	}
}

// A turn reads only its own thread's blocks: a child thread's reasoning is
// never replayed on the primary's turns, nor loaded for them.
func TestThinkingBlocksAreReadPerThread(t *testing.T) {
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	sid := newThreadedSession(t, pool)
	child := pgtest.NewChildThread(t, pool, sid)
	ctx := context.Background()
	evs, err := log.Append(ctx, sid, []events.NewEvent{
		{Type: domain.EventAgentThinking},
		{Type: domain.EventAgentThinking, ThreadID: child},
	})
	if err != nil {
		t.Fatal(err)
	}
	block := json.RawMessage(`{"type":"thinking","thinking":"x","signature":"s"}`)
	if _, err := log.AppendWith(ctx, sid,
		[]events.NewEvent{{Type: domain.EventAgentMessage, Payload: text("answer")}},
		events.AppendOptions{Thinking: []events.ThinkingBlock{
			{EventID: evs[0].ID, Model: "m", PrefixDigest: "d", Block: block},
			{EventID: evs[1].ID, Model: "m", PrefixDigest: "d", Block: block},
		}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		thread domain.ID
		want   domain.ID
	}{{"primary", "", evs[0].ID}, {"child", child, evs[1].ID}} {
		got, err := log.ThinkingBlocks(ctx, sid, tc.thread)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := got[tc.want]; len(got) != 1 || !ok {
			t.Errorf("%s: blocks = %v, want only %s", tc.name, got, tc.want)
		}
	}
}

// DropThinking forgets a session's kept blocks, and only that session's.
func TestDropThinkingForgetsOneSession(t *testing.T) {
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	ctx := context.Background()
	keep := func(sid domain.ID) {
		id := appendThinkingEvent(t, log, sid)
		if _, err := log.AppendWith(ctx, sid,
			[]events.NewEvent{{Type: domain.EventAgentMessage, Payload: text("answer")}},
			events.AppendOptions{Thinking: []events.ThinkingBlock{{EventID: id, Model: "m", PrefixDigest: "d",
				Block: json.RawMessage(`{"type":"redacted_thinking","data":"x"}`)}}}); err != nil {
			t.Fatal(err)
		}
	}
	dropped, other := newSession(t, pool), newSession(t, pool)
	keep(dropped)
	keep(other)
	if err := log.DropThinking(ctx, dropped); err != nil {
		t.Fatal(err)
	}
	for sid, want := range map[domain.ID]int{dropped: 0, other: 1} {
		got, err := log.ThinkingBlocks(ctx, sid, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != want {
			t.Errorf("session %s keeps %d blocks, want %d", sid, len(got), want)
		}
	}
}
