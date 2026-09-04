package brain_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/brain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// The model is the third NUL producer (#228), after tool output (#223) and
// API-inbound events (rejected with a 400). Postgres jsonb cannot store
// `\u0000`, so a single U+0000 in a model's output would fault the turn's
// append (SQLSTATE 22P05) and the work item would reclaim-loop — these tests
// run over the real store, so an unguarded lane reproduces the wedge as a
// failed RunOnce, not as a mocked assertion.

// singleEvent asserts the session's log holds exactly one event of typ and
// returns its payload.
func singleEvent(t *testing.T, h *harness, typ string) []byte {
	t.Helper()
	evs, err := h.log.List(context.Background(), h.sessionID, events.ListQuery{Types: []string{typ}})
	if err != nil || len(evs) != 1 {
		t.Fatalf("%s events = %d (%v), want 1", typ, len(evs), err)
	}
	return evs[0].Body
}

func TestModelTextNULIsStripped(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		textChunk(0, "he\x00l"), textChunk(0, "lo\x00"),
		done("end_turn", 3),
	}}, nil)
	h.wake(t, "hi")
	h.runOnce(t)

	if got := h.status(t); got != "idle" {
		t.Fatalf("status = %q, want idle", got)
	}
	body := singleEvent(t, h, "agent.message")
	var msg struct {
		Content []domain.ContentBlock `json:"content"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatal(err)
	}
	if len(msg.Content) != 1 || msg.Content[0].Text != "hello" {
		t.Errorf("agent.message content = %+v, want one block reading %q", msg.Content, "hello")
	}
}

// A delta that is nothing but NULs is an empty delta: no preview opens for
// it and no empty text block lands — the same dense-content rule the empty
// delta already follows.
func TestAllNULTextDeltaLandsNoMessage(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		textChunk(0, "\x00\x00"),
		done("end_turn", 1),
	}}, nil)
	h.wake(t, "hi")
	h.runOnce(t)

	for _, typ := range h.types(t) {
		if typ == "agent.message" {
			t.Fatal("an all-NUL delta landed an agent.message")
		}
	}
	if got := h.status(t); got != "idle" {
		t.Errorf("status = %q, want idle", got)
	}
}

// A NUL in a tool_use travels differently: the input is raw JSON, where
// U+0000 exists only as the six-byte `\u0000` escape — a byte-level strip
// cannot see it. The literal-text fixture (`\\u0000`) contains the same six
// bytes without encoding a NUL, and must come through untouched; the big
// integer pins that a rewrite preserves numeric fidelity.
func TestToolUseNULIsStripped(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		{Kind: provider.KindToolUse, ToolUse: &provider.ToolUse{
			ID:   "tu_1",
			Name: "make_\x00coffee",
			Input: json.RawMessage(
				`{"cmd":"a\u0000b","note":"\\u0000 stays","big":9007199254740993,"args":["x\u0000","y"]}`),
		}},
		done("tool_use", 2),
	}}, nil)
	// Declared under the NUL-stripped name: sanitization runs before the
	// class lookup (stream.go), so this is the name the call is matched
	// against, and it must stay client-executed rather than fall into
	// #567's unoffered path.
	h.customTool(t, "make_coffee")
	h.wake(t, "hi")
	h.runOnce(t)

	body := singleEvent(t, h, "agent.custom_tool_use")
	var p struct {
		Name  string                     `json:"name"`
		Input map[string]json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Name != "make_coffee" {
		t.Errorf("name = %q, want the NUL stripped", p.Name)
	}
	var cmd, note string
	var args []string
	if err := json.Unmarshal(p.Input["cmd"], &cmd); err != nil || cmd != "ab" {
		t.Errorf("input cmd = %q (%v), want the escape stripped to %q", cmd, err, "ab")
	}
	if err := json.Unmarshal(p.Input["note"], &note); err != nil || note != `\u0000 stays` {
		t.Errorf("input note = %q (%v), want the literal backslash-u text untouched", note, err)
	}
	if string(p.Input["big"]) != "9007199254740993" {
		t.Errorf("input big = %s, want the integer byte-exact", p.Input["big"])
	}
	if err := json.Unmarshal(p.Input["args"], &args); err != nil || len(args) != 2 || args[0] != "x" || args[1] != "y" {
		t.Errorf("input args = %v (%v), want the array's strings stripped", args, err)
	}
}

// A valid object followed by an unmatched closing delimiter is corrupted
// model output, not an object with garnish: executing its valid prefix would
// run a command the model never finished emitting. Decoder.More treats a
// trailing ] or } as the end of a collection and reports nothing left, so
// the trailing check must probe for io.EOF instead.
func TestToolInputTrailingDelimiterFailsTheTurn(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		{Kind: provider.KindToolUse, ToolUse: &provider.ToolUse{
			ID: "tu_1", Name: "custom_thing", Input: json.RawMessage(`{"command":"ls"}]`),
		}},
		done("tool_use", 2),
	}}, nil)
	h.wake(t, "hi")
	h.runOnce(t)

	for _, typ := range h.types(t) {
		if typ == "agent.tool_use" || typ == "agent.custom_tool_use" {
			t.Fatalf("corrupted input committed a %s intent", typ)
		}
	}
	body := singleEvent(t, h, "session.error")
	if !strings.Contains(string(body), "single JSON object") {
		t.Errorf("session.error = %s, want it to name the trailing content", body)
	}
}

// jsonb rejects more than U+0000: a lone surrogate escape is well-formed JSON
// to Go's validator but SQLSTATE 22P02 to Postgres. The input normalize
// launders it to U+FFFD by decoding and re-encoding, so the turn commits.
func TestToolInputLoneSurrogateIsLaundered(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		{Kind: provider.KindToolUse, ToolUse: &provider.ToolUse{
			ID: "tu_1", Name: "custom_thing", Input: json.RawMessage(`{"path":"\ud800"}`),
		}},
		done("tool_use", 2),
	}}, nil)
	h.customTool(t, "custom_thing")
	h.wake(t, "hi")
	h.runOnce(t)

	body := singleEvent(t, h, "agent.custom_tool_use")
	var p struct {
		Input map[string]string `json:"input"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Input["path"] != "�" {
		t.Errorf("input path = %q, want the lone surrogate laundered to U+FFFD", p.Input["path"])
	}
}

// Invalid UTF-8 rides json.RawMessage past every Go string coercion — the
// SDK's decode hands the raw bytes through — and faults the append with
// SQLSTATE 22021. The same decode→re-encode launders it.
func TestToolInputInvalidUTF8IsLaundered(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		{Kind: provider.KindToolUse, ToolUse: &provider.ToolUse{
			ID: "tu_1", Name: "custom_thing", Input: json.RawMessage("{\"q\":\"a\xffb\"}"),
		}},
		done("tool_use", 2),
	}}, nil)
	h.customTool(t, "custom_thing")
	h.wake(t, "hi")
	h.runOnce(t)

	body := singleEvent(t, h, "agent.custom_tool_use")
	var p struct {
		Input map[string]string `json:"input"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Input["q"] != "a�b" {
		t.Errorf("input q = %q, want the invalid byte laundered to U+FFFD", p.Input["q"])
	}
}

// Two different keys that collide once stripped would leave Go map iteration
// to pick which value survives — a write tool targeting either of two files
// on a coin flip. The turn fails visibly instead.
func TestToolInputNULKeyCollisionFailsTheTurn(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		{Kind: provider.KindToolUse, ToolUse: &provider.ToolUse{
			ID: "tu_1", Name: "write",
			Input: json.RawMessage(`{"file_path":"safe.txt","file_\u0000path":"other.txt"}`),
		}},
		done("tool_use", 2),
	}}, nil)
	h.wake(t, "hi")
	h.runOnce(t)

	for _, typ := range h.types(t) {
		if typ == "agent.tool_use" || typ == "agent.custom_tool_use" {
			t.Fatalf("a colliding input committed a %s intent", typ)
		}
	}
	body := singleEvent(t, h, "session.error")
	if !strings.Contains(string(body), "collide") {
		t.Errorf("session.error = %s, want it to name the key collision", body)
	}
	if got := h.status(t); got != "idle" {
		t.Errorf("status = %q, want idle — the failed turn must settle, not reclaim-loop", got)
	}
	if n := h.liveWork(t); n != 0 {
		t.Errorf("%d work item(s) still live after the failed turn settled", n)
	}
}

// A thinking delta of nothing but NULs is no more a first token than an empty
// one — the same rule the text lane applies.
func TestNULOnlyThinkingDeltaRecordsNoFirstToken(t *testing.T) {
	collect := collectBrainMetrics(t)

	h := newHarness(t, [][]provider.Chunk{{
		{Kind: provider.KindThinkingDelta, Index: 0, Text: "\x00"},
		done("end_turn", 1),
	}}, nil)
	h.wake(t, "hi")
	h.runOnce(t)

	if pts := floatPoints(t, collect(), brain.MetricTimeToFirstToken); len(pts) != 0 {
		t.Errorf("recorded %d first-token point(s) for NUL-only thinking, want 0", len(pts))
	}
}

// The preview lane must strip too: sanitization sits ahead of the delta
// broadcast, so a live SSE subscriber and the stored event read the same
// text. The all-NUL control proves no preview opens at all. The drain is
// bounded by a sentinel frame, not a Wake: wakes are coalesced and the
// listener's coverage-healing wakeAll buffers one at LISTEN time, so a
// single Wake can be satisfied before any of the turn's notifications have
// been dispatched (#294). The sentinel's NOTIFY autocommits after the
// settle, and the one listening connection delivers in commit order, so
// every frame the turn broadcast is buffered before the sentinel arrives —
// including none, which the all-NUL control needs to be able to prove.
func TestPreviewFramesCarryOnlySanitizedText(t *testing.T) {
	drain := func(t *testing.T, h *harness) []map[string]any {
		t.Helper()
		broker := events.NewBroker(h.pool)
		sub := broker.Subscribe(h.sessionID)
		t.Cleanup(sub.Close)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := broker.Ready(ctx); err != nil {
			t.Fatalf("broker never became ready: %v", err)
		}
		h.runOnce(t)
		// A fresh deadline: the outer ctx has been paying for Ready and the
		// whole turn, and the sentinel publish must not inherit that debt.
		pubCtx, pubCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer pubCancel()
		if err := h.log.PublishEventFrame(pubCtx, h.sessionID,
			map[string]any{"type": "drain_sentinel"}); err != nil {
			t.Fatalf("publish drain sentinel: %v", err)
		}
		var frames []map[string]any
		for {
			select {
			case raw := <-sub.Frames():
				var f map[string]any
				if err := json.Unmarshal(raw, &f); err != nil {
					t.Fatalf("frame %s: %v", raw, err)
				}
				if f["type"] == "drain_sentinel" {
					return frames
				}
				frames = append(frames, f)
			case <-time.After(10 * time.Second):
				t.Fatal("the drain sentinel never arrived on the frames lane")
			}
		}
	}

	t.Run("delta frames carry the stripped text", func(t *testing.T) {
		h := newHarness(t, [][]provider.Chunk{{
			textChunk(0, "he\x00l"), textChunk(0, "lo"),
			done("end_turn", 3),
		}}, nil)
		h.wake(t, "hi")
		var streamed string
		for _, f := range drain(t, h) {
			if f["type"] != "event_delta" {
				continue
			}
			text := f["delta"].(map[string]any)["content"].(map[string]any)["text"].(string)
			if strings.IndexByte(text, 0) >= 0 {
				t.Errorf("a delta frame streamed a NUL: %q", text)
			}
			streamed += text
		}
		if streamed != "hello" {
			t.Errorf("streamed preview = %q, want %q — preview and stored event must agree", streamed, "hello")
		}
	})

	t.Run("an all-NUL turn opens no preview", func(t *testing.T) {
		h := newHarness(t, [][]provider.Chunk{{
			textChunk(0, "\x00\x00"),
			done("end_turn", 1),
		}}, nil)
		h.wake(t, "hi")
		if frames := drain(t, h); len(frames) != 0 {
			t.Errorf("an all-NUL delta broadcast %d frame(s): %v", len(frames), frames)
		}
	})
}

// A NUL inside a JSON object key must go the same way as one in a value.
func TestToolInputKeyNULIsStripped(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		{Kind: provider.KindToolUse, ToolUse: &provider.ToolUse{
			ID: "tu_1", Name: "custom_thing", Input: json.RawMessage(`{"k\u0000ey":"v"}`),
		}},
		done("tool_use", 2),
	}}, nil)
	h.customTool(t, "custom_thing")
	h.wake(t, "hi")
	h.runOnce(t)

	body := singleEvent(t, h, "agent.custom_tool_use")
	var p struct {
		Input map[string]string `json:"input"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	if v, ok := p.Input["key"]; !ok || v != "v" {
		t.Errorf("input = %v, want the key's NUL stripped", p.Input)
	}
}

// The failure lane is endpoint-produced text too: a model error message may
// quote the endpoint's bytes, and a NUL there would fault the session.error
// append — the failure path failing, the same wedge one level up.
func TestModelErrorNULIsStripped(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		textChunk(0, "partial"),
	}}, []error{errors.New("endpoint said: \x00boom")})
	h.wake(t, "hi")
	h.runOnce(t)

	body := singleEvent(t, h, "session.error")
	var p struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	if strings.IndexByte(p.Error.Message, 0) >= 0 {
		t.Errorf("session.error message carries a NUL: %q", p.Error.Message)
	}
	if !strings.Contains(p.Error.Message, "boom") {
		t.Errorf("session.error message = %q, want the endpoint's text kept", p.Error.Message)
	}
	if got := h.status(t); got != "idle" {
		t.Errorf("status = %q, want idle after the failed turn settled", got)
	}
}
