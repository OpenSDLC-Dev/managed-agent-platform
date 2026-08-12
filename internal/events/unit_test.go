package events

import (
	"encoding/json"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"strings"
	"testing"
	"unicode/utf8"
)

// White-box: chunkText must keep every chunk's JSON-escaped form within the
// budget, across the escape classes encoding/json actually emits.
func TestChunkTextEscapeBudget(t *testing.T) {
	// A pathological mix: quotes, backslashes, newlines, control chars,
	// HTML-escaped runes, U+2028/U+2029, and multibyte text.
	unit := "a\"b\\c\nd\x01e<f&g h 界"
	long := strings.Repeat(unit, 400)

	const budget = 100
	chunks := chunkText(long, budget)
	if strings.Join(chunks, "") != long {
		t.Fatal("chunks do not reassemble the input")
	}
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		encoded, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		// len minus the surrounding quotes must fit the budget.
		if got := len(encoded) - 2; got > budget {
			t.Errorf("chunk %d escapes to %d bytes, budget %d", i, got, budget)
		}
		if !utf8.ValidString(c) {
			t.Errorf("chunk %d split a rune", i)
		}
	}

	if got := chunkText("", 10); len(got) != 1 || got[0] != "" {
		t.Errorf("empty text chunks = %q", got)
	}
}

// White-box: the two tables that decide gating have to agree, and the way they
// can drift is silent. confirmableToolUseTypes says which calls a human may be
// asked about; toolUseAnswer says what answers each family when the human says
// no (or a user.interrupt abandons it). A confirmable family missing from the
// answer table has no denial shape at all, so its denial would fail at the one
// moment a session depends on it. An answer that is an inbound type would land
// with a NULL processed_at unless the synthesis stamped it, since the store
// stamps only what it emits itself — and a result carrying a session_thread_id
// would invent a field neither agent.* result declares on the wire.
//
// This is a test rather than a runtime check because the drift is a compile-time
// fact about two package-level variables: unreachable code cannot be exercised,
// and an assertion that cannot fail is not a guard.
func TestEveryConfirmableFamilyHasAnOutboundAnswer(t *testing.T) {
	for _, typ := range confirmableToolUseTypes {
		answer, ok := toolUseAnswer[domain.EventType(typ)]
		if !ok {
			t.Errorf("%s is confirmable but no result event answers it", typ)
			continue
		}
		if answer.result.Inbound() {
			t.Errorf("%s is answered by %s, an inbound type the store does not stamp", typ, answer.result)
		}
		if answer.thread {
			t.Errorf("%s is answered by %s, which the table marks as carrying a session_thread_id", typ, answer.result)
		}
	}
}

// White-box: a dropped event_delta must poison the remainder of its preview
// (prefix, never an interior hole), and the next event_start resets it.
func TestDispatchDropsDeltasAsPrefix(t *testing.T) {
	b := &Broker{
		subs:  make(map[domain.ID]map[*Subscription]struct{}),
		ready: make(chan struct{}),
	}
	sub := &Subscription{
		broker:    b,
		sessionID: "sesn_x",
		wake:      make(chan struct{}, 1),
		frames:    make(chan json.RawMessage, 2),
	}
	b.subs["sesn_x"] = map[*Subscription]struct{}{sub: {}}

	send := func(frame string) {
		b.dispatch(channelFrames, `{"session_id":"sesn_x","frame":`+frame+`}`)
	}
	delta := func(text string) string {
		return `{"type":"event_delta","event_id":"sevt_1","delta":{"type":"content_delta","index":0,"content":{"type":"text","text":"` + text + `"}}}`
	}

	send(delta("one"))
	send(delta("two"))
	send(delta("three")) // buffer full → dropped, preview poisoned
	<-sub.frames
	<-sub.frames
	send(delta("four")) // space again, but the poisoned preview stays dry
	select {
	case f := <-sub.frames:
		t.Fatalf("delta after a drop must be suppressed, got %s", f)
	default:
	}

	// A new preview generation resets the subscriber.
	send(`{"type":"event_start","event":{"id":"sevt_2","type":"agent.message"}}`)
	send(delta("fresh"))
	if got := len(sub.frames); got != 2 {
		t.Fatalf("expected event_start + fresh delta, %d frames buffered", got)
	}
}
