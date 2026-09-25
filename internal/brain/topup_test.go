package brain

import (
	"context"
	"slices"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/transcript"
)

// The turn reads its history, then resolves its tools and skills, then
// writes its span start: an input appended in between has a seq below the
// start, so the request it opens consumes it (and, since #793, stamps it) —
// but the history read missed it. topUpHistory re-reads exactly that gap:
// the thread's own rows past the watermark and below the start, never a
// sibling's and never one that landed after the start.
func TestTopUpHistoryClosesTheSnapshotRace(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sid, _ := pgtest.NewSession(t, pool, "cloud")
	child := pgtest.NewChildThread(t, pool, sid)
	b := New(pool, nil, nil, Config{})
	msg := func(text string, thread domain.ID) {
		t.Helper()
		if _, err := b.log.Append(ctx, sid, []events.NewEvent{{
			Type: domain.EventUserMessage, Payload: []byte(`{"content":"` + text + `"}`), ThreadID: thread,
		}}); err != nil {
			t.Fatal(err)
		}
	}
	texts := func(evs []domain.Event) []string {
		var out []string
		for _, ev := range evs {
			out = append(out, transcript.ContentText(ev.Body))
		}
		return out
	}

	msg("one", "")
	history, err := b.log.List(ctx, sid, events.ListQuery{Scope: events.ScopeThread})
	if err != nil {
		t.Fatal(err)
	}
	watermark := history[len(history)-1].Seq
	msg("two", "")        // the race: below the start, missed by the read
	msg("sibling", child) // another thread's row in the same gap
	_, span, err := b.log.StartModelRequestOn(ctx, sid, "", events.Backend{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	msg("three", "") // mid-request: the next request's
	starts, err := b.log.List(ctx, sid, events.ListQuery{Types: []string{string(domain.EventSpanModelRequestStart)}})
	if err != nil || len(starts) != 1 || span.StartSeq() != starts[0].Seq {
		t.Fatalf("StartSeq = %d, want the start row's seq (%v, %v)", span.StartSeq(), starts, err)
	}

	got, topped, err := b.topUpHistory(ctx, sid, "", history, watermark, span.StartSeq())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"one", "two"}; !topped || !slices.Equal(texts(got), want) {
		t.Errorf("topped %v, history %v, want %v", topped, texts(got), want)
	}
	// Nothing left in the gap: the common case costs one empty read.
	again, topped, err := b.topUpHistory(ctx, sid, "", got, got[len(got)-1].Seq, span.StartSeq())
	if err != nil || topped || len(again) != 2 {
		t.Errorf("second top-up: topped %v, %d rows, %v; want nothing new", topped, len(again), err)
	}
}
