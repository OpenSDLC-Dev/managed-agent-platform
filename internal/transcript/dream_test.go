package transcript

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
)

// fakeLog is the only test double in this package: an in-memory Lister that
// honors the AfterSeq keyset and the Limit the renderer pages with, and
// records what each page was asked for. No pgtest — the renderer's contract
// with the log is the query it sends and the events it gets back, and a real
// Postgres would only slow that down.
type fakeLog struct {
	evs    []domain.Event
	err    error
	calls  int
	scopes []events.Scope
	limits []int
}

func (f *fakeLog) List(_ context.Context, _ domain.ID, q events.ListQuery) ([]domain.Event, error) {
	f.calls++
	f.scopes = append(f.scopes, q.Scope)
	f.limits = append(f.limits, q.Limit)
	if f.err != nil {
		return nil, f.err
	}
	var out []domain.Event
	for _, ev := range f.evs {
		if q.AfterSeq != nil && ev.Seq <= *q.AfterSeq {
			continue
		}
		out = append(out, ev)
		if q.Limit > 0 && len(out) == q.Limit {
			break
		}
	}
	return out, nil
}

// mustEvent builds one log row; seqs must ascend, because the keyset does.
func mustEvent(t *testing.T, seq int64, typ domain.EventType, body any) domain.Event {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return domain.Event{Seq: seq, Type: typ, Body: raw}
}

// content is an event body carrying one string of text.
func content(s string) any { return map[string]any{"content": s} }

func renderEvents(t *testing.T, evs ...domain.Event) *Dream {
	t.Helper()
	d, err := RenderDream(t.Context(), &fakeLog{evs: evs}, "sesn_test")
	if err != nil {
		t.Fatalf("RenderDream: %v", err)
	}
	return d
}

// The renderer pulls the session view in DreamPageSize keyset pages: three
// calls for 500 events, each asking for ScopeSession and the page limit.
func TestRenderDreamPages(t *testing.T) {
	var evs []domain.Event
	for i := range 500 {
		evs = append(evs, mustEvent(t, int64(i+1), domain.EventUserMessage, content(fmt.Sprintf("turn %d", i))))
	}
	log := &fakeLog{evs: evs}
	d, err := RenderDream(t.Context(), log, "sesn_paged")
	if err != nil {
		t.Fatalf("RenderDream: %v", err)
	}
	if log.calls != 3 {
		t.Errorf("List called %d times, want 3 pages of %d", log.calls, DreamPageSize)
	}
	for i, sc := range log.scopes {
		if sc != events.ScopeSession || log.limits[i] != DreamPageSize {
			t.Errorf("page %d asked for scope %v limit %d", i, sc, log.limits[i])
		}
	}
	if d.Turns != 500 {
		t.Errorf("Turns = %d, want 500", d.Turns)
	}
	if d.FirstUser != "turn 0" {
		t.Errorf("FirstUser = %q, want the first user message", d.FirstUser)
	}
}

// An empty log still renders its heading, and a failing log fails the render.
func TestRenderDreamEmptyAndError(t *testing.T) {
	d := renderEvents(t)
	if got := string(d.Text); got != "# session sesn_test\n\n" {
		t.Errorf("empty log rendered %q", got)
	}
	if d.Turns != 0 || d.FirstUser != "" || d.ElidedBytes != 0 {
		t.Errorf("empty log = %+v", d)
	}

	want := errors.New("log down")
	if _, err := RenderDream(t.Context(), &fakeLog{err: want}, "sesn_test"); !errors.Is(err, want) {
		t.Errorf("RenderDream error = %v, want %v", err, want)
	}
}

// Tool call inputs cut at 512 bytes and tool results at 2 KiB, both from the
// middle and both naming what went.
func TestRenderDreamPerEventTruncation(t *testing.T) {
	input := strings.Repeat("i", 4000)
	result := strings.Repeat("r", 8000)
	d := renderEvents(t,
		mustEvent(t, 1, domain.EventAgentToolUse, map[string]any{
			"name": "bash", "input": map[string]any{"command": input}}),
		mustEvent(t, 2, domain.EventAgentToolResult, content(result)),
	)
	got := string(d.Text)
	if strings.Contains(got, strings.Repeat("i", dreamToolInputBudget)) {
		t.Error("tool input was not truncated")
	}
	if strings.Contains(got, strings.Repeat("r", dreamToolResultBudget)) {
		t.Error("tool result was not truncated")
	}
	// Both ends of each item survive the middle elision.
	inputJSON := fmt.Sprintf(`{"command":%q}`, input)
	for _, want := range []string{
		"## tool call: bash", `{"command":"iii`, `iii"}`,
		"## tool result", "rrr",
		fmt.Sprintf("[… %d bytes elided …]", len(inputJSON)-dreamToolInputBudget),
		fmt.Sprintf("[… %d bytes elided …]", len(result)-dreamToolResultBudget),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// elide cuts on rune boundaries: a multi-byte rune at either cut is dropped
// whole rather than left as a broken sequence.
func TestElideRuneBoundaries(t *testing.T) {
	s := strings.Repeat("é", 100) // two bytes each, so every odd cut splits one
	out := elide(s, 51)
	if !strings.HasPrefix(out, "éé") || !strings.HasSuffix(out, "éé") {
		t.Errorf("elide left a partial rune: %q", out)
	}
	if strings.ContainsRune(out, '�') {
		t.Errorf("elide produced a replacement char: %q", out)
	}
	if got := elide("short", 100); got != "short" {
		t.Errorf("elide under the budget = %q", got)
	}
}

// dreamContentText's block rules: thinking is dropped, an image is named, a
// search_result flattens to its evidence, and the grader's fallbacks survive.
func TestDreamContentText(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"string", `{"content":"plain"}`, "plain"},
		{"thinking dropped", `{"content":[{"type":"thinking","thinking":"secret plan"},{"type":"text","text":"said"}]}`, "said"},
		{"redacted thinking dropped", `{"content":[{"type":"redacted_thinking","data":"opaque"},{"type":"text","text":"said"}]}`, "said"},
		{"image named", `{"content":[{"type":"image"},{"type":"text","text":"look"}]}`, "[image]\nlook"},
		{"search result", `{"content":[{"type":"search_result","title":"Go notes","source":"https://go.dev/doc","content":[{"type":"text","text":"highlights"}]}]}`,
			"Go notes\nhttps://go.dev/doc\nhighlights"},
		{"unknown shape", `{"content":{"k":1}}`, `{"k":1}`},
		{"no content", `{}`, ""},
		{"invalid json", `{`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := dreamContentText([]byte(c.body)); got != c.want {
				t.Errorf("dreamContentText(%s) = %q, want %q", c.body, got, c.want)
			}
		})
	}
}

// A tool_use event whose body is not an object renders nothing rather than a
// half-read call.
func TestRenderDreamUnreadableToolUse(t *testing.T) {
	d := renderEvents(t, domain.Event{Seq: 1, Type: domain.EventAgentToolUse, Body: []byte(`"not an object"`)})
	if strings.Contains(string(d.Text), "tool call") {
		t.Errorf("unreadable tool_use rendered:\n%s", d.Text)
	}
}

// A received message whose sender carries no name — the primary agent's half
// of the pair — still says who is speaking rather than trailing off.
func TestThreadMessageWithoutSenderName(t *testing.T) {
	d := renderEvents(t, mustEvent(t, 1, domain.EventAgentThreadMessageReceived,
		map[string]any{"content": "done", "from_agent_name": nil}))
	if want := "## message from another agent\n\ndone"; !strings.Contains(string(d.Text), want) {
		t.Errorf("missing %q in:\n%s", want, d.Text)
	}
}

// The four shapes, each fixture forcing the substitution, and one per source:
// the three roles' text, a tool input, a tool result, the INDEX.md preview,
// and a caller's instructions passed through Redact directly.
func TestRedactEverySourceAndShape(t *testing.T) {
	const (
		skKey   = "sk-live-abcdEFGHijklMNOP4242"
		akia    = "AKIAQWERTYUIOPASDFGH"
		bearer  = "Bearer eyJhbGciOiJub25lIn0.cGF5bG9hZA.c2ln"
		assign  = `api_key = hunter2hunter2please`
		assignJ = `{"token":"tok-9f3a7c2e5b1d8a4f"}`
	)
	d := renderEvents(t,
		mustEvent(t, 1, domain.EventUserMessage, content("deploy with "+skKey+" now")),
		mustEvent(t, 2, domain.EventAgentMessage, content("using "+akia+" for s3")),
		mustEvent(t, 3, domain.EventSystemMessage, content("header: "+bearer)),
		mustEvent(t, 4, domain.EventAgentToolUse, map[string]any{
			"name": "bash", "input": map[string]any{"command": "export " + assign}}),
		mustEvent(t, 5, domain.EventAgentToolResult, content("response "+assignJ)),
	)
	got := string(d.Text)
	for _, leaked := range []string{skKey, akia, "eyJhbGciOiJub25lIn0", "hunter2hunter2please", "tok-9f3a7c2e5b1d8a4f"} {
		if strings.Contains(got, leaked) {
			t.Errorf("secret %q survived redaction in:\n%s", leaked, got)
		}
	}
	// Every source produced a marker, and the shapes that identify a match
	// keep their left-hand side.
	if n := strings.Count(got, redactedSecret); n != 5 {
		t.Errorf("got %d redactions, want one per source:\n%s", n, got)
	}
	for _, want := range []string{"Bearer " + redactedSecret, "api_key = " + redactedSecret, `"token":"` + redactedSecret} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}

	// The INDEX.md preview is rendered from the same first user message.
	if !strings.Contains(d.FirstUser, redactedSecret) || strings.Contains(d.FirstUser, skKey) {
		t.Errorf("FirstUser = %q", d.FirstUser)
	}

	// The runner redacts a caller's instructions through the same function
	// before they are substituted into a stage message (§3.2).
	instructions := "look for regressions; the fixture password: swordfish-99-open"
	if out := Redact(instructions); strings.Contains(out, "swordfish-99-open") || !strings.Contains(out, "password: "+redactedSecret) {
		t.Errorf("Redact(instructions) = %q", out)
	}
}

// Redaction runs before truncation, so an elision can never split a match and
// leave a fragment: this secret straddles the tool result's head cut.
func TestRedactBeforeTruncation(t *testing.T) {
	const secret = "sk-live-STRADDLEcut0123abcd"
	head := dreamToolResultBudget / 2
	// The suffix starts with a space: a shape pattern is greedy, and an
	// unbroken run of key-alphabet bytes after the secret would be swallowed
	// into the same match and leave nothing long enough to elide.
	body := strings.Repeat("a", head-12) + secret + " " + strings.Repeat("b", 3000)
	d := renderEvents(t, mustEvent(t, 1, domain.EventAgentToolResult, content(body)))
	got := string(d.Text)
	for _, fragment := range []string{"sk-live", "STRADDLE"} {
		if strings.Contains(got, fragment) {
			t.Errorf("fragment %q of a straddling secret survived:\n%s", fragment, got)
		}
	}
	if !strings.Contains(got, "bytes elided") {
		t.Error("the result was not elided at all, so nothing straddled")
	}
}

// The per-transcript cap: the head and tail are half of it each, the middle is
// named, ElidedBytes records it, and IndexLine carries the note. Lowering the
// cap shrinks both buffers with it.
func TestRenderDreamTranscriptCap(t *testing.T) {
	restore := DreamTranscriptCap
	DreamTranscriptCap = 2048
	t.Cleanup(func() { DreamTranscriptCap = restore })

	var evs []domain.Event
	for i := range 40 {
		evs = append(evs, mustEvent(t, int64(i+1), domain.EventUserMessage, content(strings.Repeat("x", 200))))
	}
	d, err := RenderDream(t.Context(), &fakeLog{evs: evs}, "sesn_capped")
	if err != nil {
		t.Fatalf("RenderDream: %v", err)
	}
	note := elisionNote(d.ElidedBytes)
	if len(d.Text) != DreamTranscriptCap+len(note) {
		t.Errorf("rendered %d bytes, want the cap %d plus the note", len(d.Text), DreamTranscriptCap)
	}
	if d.ElidedBytes <= 0 || !strings.Contains(string(d.Text), note) {
		t.Errorf("ElidedBytes = %d, text:\n%s", d.ElidedBytes, d.Text)
	}
	if !strings.HasPrefix(string(d.Text), "# session sesn_capped") {
		t.Error("the head buffer lost the start of the transcript")
	}

	line := IndexLine(3, d, time.Date(2026, 9, 6, 12, 30, 0, 0, time.UTC))
	want := fmt.Sprintf("3 · sesn_capped · 2026-09-06T12:30:00Z · 40 turns · %d bytes (%d elided) · %s",
		len(d.Text), d.ElidedBytes, strings.Repeat("x", 200))
	if line != want {
		t.Errorf("IndexLine =\n%s\nwant\n%s", line, want)
	}
}

// One event bigger than the whole cap: a message carries no per-event budget
// of its own, so the transcript cap is all that holds it, and the tail keeps
// the message's end rather than the buffer it overran.
func TestRenderDreamSingleOversizeEvent(t *testing.T) {
	restore := DreamTranscriptCap
	DreamTranscriptCap = 2048
	t.Cleanup(func() { DreamTranscriptCap = restore })

	body := strings.Repeat("m", 100_000) + "END"
	d, err := RenderDream(t.Context(), &fakeLog{evs: []domain.Event{
		mustEvent(t, 1, domain.EventUserMessage, content(body))}}, "sesn_big")
	if err != nil {
		t.Fatalf("RenderDream: %v", err)
	}
	got := string(d.Text)
	if len(d.Text) != DreamTranscriptCap+len(elisionNote(d.ElidedBytes)) {
		t.Errorf("rendered %d bytes for a %d-byte message", len(d.Text), len(body))
	}
	if !strings.HasPrefix(got, "# session sesn_big") || !strings.HasSuffix(got, "END\n\n") {
		t.Errorf("the cap kept the wrong ends:\n%s", got)
	}
}

// Under the cap nothing is elided and the row carries no note: the head and
// the tail together are the whole text, because the tail only ever sees the
// bytes the head refused.
func TestRenderDreamUnderTheCap(t *testing.T) {
	d := renderEvents(t,
		mustEvent(t, 1, domain.EventUserMessage, content("hello")),
		mustEvent(t, 2, domain.EventAgentMessage, content("hi")),
	)
	want := "# session sesn_test\n\n## user\n\nhello\n\n## agent\n\nhi\n\n"
	if string(d.Text) != want {
		t.Errorf("rendered %q, want %q", d.Text, want)
	}
	if d.ElidedBytes != 0 {
		t.Errorf("ElidedBytes = %d under the cap", d.ElidedBytes)
	}
	line := IndexLine(1, d, time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC))
	if strings.Contains(line, "elided") {
		t.Errorf("IndexLine carried an elision note: %s", line)
	}
	if want := fmt.Sprintf("1 · sesn_test · 2026-09-06T00:00:00Z · 1 turns · %d bytes · hello", len(d.Text)); line != want {
		t.Errorf("IndexLine = %q, want %q", line, want)
	}
}

// The preview is one line and at most dreamPreviewRunes runes, cut with an
// ellipsis; multi-line and multi-byte messages both collapse.
func TestFirstUserPreview(t *testing.T) {
	d := renderEvents(t, mustEvent(t, 1, domain.EventUserMessage,
		content("  first line\n\tsecond line  ")))
	if d.FirstUser != "first line second line" {
		t.Errorf("FirstUser = %q", d.FirstUser)
	}

	long := strings.Repeat("é", 400)
	d = renderEvents(t, mustEvent(t, 1, domain.EventUserMessage, content(long)))
	if n := len([]rune(d.FirstUser)); n != dreamPreviewRunes {
		t.Errorf("FirstUser is %d runes, want %d", n, dreamPreviewRunes)
	}
	if !strings.HasSuffix(d.FirstUser, "…") {
		t.Errorf("a cut preview is not marked: %q", d.FirstUser)
	}
}

// The memory bound (§7): a 50,000-event log renders under the cap holding no
// more memory than a 500-event one, because the paging and the two fixed
// buffers are the only state. Retained heap is what is measured — the total
// allocated necessarily grows with the log, the live set must not.
func TestRenderDreamMemoryBound(t *testing.T) {
	measure := func(n int) (int64, *Dream) {
		t.Helper()
		log := &generatedLog{n: n}
		runtime.GC()
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		d, err := RenderDream(t.Context(), log, "sesn_bound")
		if err != nil {
			t.Fatalf("RenderDream: %v", err)
		}
		runtime.GC()
		runtime.GC()
		runtime.ReadMemStats(&after)
		return int64(after.HeapAlloc) - int64(before.HeapAlloc), d
	}
	smallHeap, small := measure(500)
	bigHeap, big := measure(50_000)

	for _, d := range []*Dream{small, big} {
		if len(d.Text) > DreamTranscriptCap+64 {
			t.Errorf("session of %d turns rendered %d bytes, over the cap", d.Turns, len(d.Text))
		}
	}
	if big.Turns != 50_000 || small.Turns != 500 {
		t.Fatalf("turns = %d and %d", small.Turns, big.Turns)
	}
	// One transcript is 24 KiB; a hundred times the log for a constant of
	// slack is the claim, so the slack is generous and still worlds from
	// linear (a materialized 50,000-event log is megabytes).
	const slack = 64 << 10
	t.Logf("retained heap: 500 events %d bytes, 50,000 events %d bytes", smallHeap, bigHeap)
	if bigHeap-smallHeap > slack {
		t.Errorf("retained heap grew by %d bytes between 500 and 50,000 events; the paging is not holding",
			bigHeap-smallHeap)
	}
	runtime.KeepAlive(small)
	runtime.KeepAlive(big)
}

// generatedLog serves a log of n synthetic events a page at a time without
// ever holding one: a fake that materialized 50,000 events would be measuring
// itself in the bound above.
type generatedLog struct{ n int }

func (g *generatedLog) List(_ context.Context, _ domain.ID, q events.ListQuery) ([]domain.Event, error) {
	start := int64(0)
	if q.AfterSeq != nil {
		start = *q.AfterSeq
	}
	out := make([]domain.Event, 0, q.Limit)
	for seq := start + 1; seq <= int64(g.n) && len(out) < q.Limit; seq++ {
		body := fmt.Sprintf(`{"content":"turn %d: %s"}`, seq, strings.Repeat("w", 120))
		out = append(out, domain.Event{Seq: seq, Type: domain.EventUserMessage, Body: []byte(body)})
	}
	return out, nil
}
