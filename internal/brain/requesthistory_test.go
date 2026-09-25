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

// The turn reads its history once, after its span start commits, and the
// request is exactly what that start consumed (#793): the thread's own rows
// below the start, however late before it they landed. A row that lands
// after the start — mid-request, and the read can see it — is the next
// request's, and a sibling's is never this thread's.
func TestRequestHistoryIsTheThreadBelowItsStart(t *testing.T) {
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

	msg("one", "")
	msg("sibling", child)
	msg("two", "") // just before the start
	_, span, err := b.log.StartModelRequestOn(ctx, sid, "", events.Backend{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	msg("three", "") // mid-request: committed before the read below, but past the start

	got, err := b.requestHistory(ctx, sid, "", span.StartSeq())
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, ev := range got {
		if ev.Seq >= span.StartSeq() {
			t.Errorf("%s at seq %d is not below the start at %d", ev.Type, ev.Seq, span.StartSeq())
		}
		texts = append(texts, transcript.ContentText(ev.Body))
	}
	if want := []string{"one", "two"}; !slices.Equal(texts, want) {
		t.Errorf("history = %v, want %v", texts, want)
	}
}
