package transcript

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
)

// fakeLog is the only test double in this package: an in-memory Lister that
// honors the AfterSeq keyset, the Desc direction and the Limit the renderer
// asks with, and records every query it was given. No pgtest — the renderer's
// contract with the log is the query it sends and the events it gets back, and
// a real Postgres would only slow that down.
type fakeLog struct {
	evs     []domain.Event
	err     error
	queries []events.ListQuery
}

func (f *fakeLog) List(_ context.Context, _ domain.ID, q events.ListQuery) ([]domain.Event, error) {
	f.queries = append(f.queries, q)
	if f.err != nil {
		return nil, f.err
	}
	return pageOf(f.evs, q), nil
}

// pageOf answers one ListQuery out of an ordered slice, the way the real log's
// keyset does: exclusive of AfterSeq in whichever direction the sort runs.
func pageOf(evs []domain.Event, q events.ListQuery) []domain.Event {
	var out []domain.Event
	take := func(ev domain.Event) bool {
		if q.AfterSeq != nil {
			if q.Desc && ev.Seq >= *q.AfterSeq {
				return true
			}
			if !q.Desc && ev.Seq <= *q.AfterSeq {
				return true
			}
		}
		out = append(out, ev)
		return q.Limit == 0 || len(out) < q.Limit
	}
	if q.Desc {
		for i := len(evs) - 1; i >= 0; i-- {
			if !take(evs[i]) {
				break
			}
		}
		return out
	}
	for _, ev := range evs {
		if !take(ev) {
			break
		}
	}
	return out
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

// The renderer reads the high-water mark in one descending row, then pulls the
// session view in DreamPageSize keyset pages: three of them for 500 events,
// each asking for ScopeSession and the page limit.
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
	if len(log.queries) != 4 {
		t.Fatalf("List called %d times, want the mark read and 3 pages of %d", len(log.queries), DreamPageSize)
	}
	if m := log.queries[0]; m.Scope != events.ScopeSession || !m.Desc || m.Limit != 1 {
		t.Errorf("the first query is %+v, want the descending one-row mark read", m)
	}
	for i, q := range log.queries[1:] {
		if q.Scope != events.ScopeSession || q.Limit != DreamPageSize || q.Desc {
			t.Errorf("page %d asked for %+v", i, q)
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

// A session that is still being written to while the dream reads it: this log
// answers every page in full and appends another page's worth each time it is
// asked, so a renderer that stopped only on a short page would never stop. The
// mark taken before the first page is what ends the render, and it renders what
// existed when it started and nothing that arrived after.
func TestRenderDreamStopsAtTheHighWaterMark(t *testing.T) {
	log := &appendingLog{}
	log.append(2 * DreamPageSize)

	done := make(chan *Dream, 1)
	go func() {
		d, err := RenderDream(context.Background(), log, "sesn_busy")
		if err != nil {
			t.Errorf("RenderDream: %v", err)
			close(done)
			return
		}
		done <- d
	}()
	select {
	case d := <-done:
		if d == nil {
			t.Fatal("RenderDream failed")
		}
		if d.Turns != 2*DreamPageSize {
			t.Errorf("rendered %d turns, want the %d that existed at the mark", d.Turns, 2*DreamPageSize)
		}
		if got := string(d.Text); strings.Contains(got, fmt.Sprintf("turn %d", 2*DreamPageSize+1)) {
			t.Error("the render carried an event appended after the mark")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RenderDream never returned: the appender kept every page full, so nothing but a high-water mark can end it")
	}
}

// appendingLog is a log a client keeps writing to: it answers the query it was
// given and then appends a full page more, mimicking an appender that outruns
// the reader.
type appendingLog struct {
	mu  sync.Mutex
	evs []domain.Event
}

func (a *appendingLog) append(n int) {
	for range n {
		seq := int64(len(a.evs) + 1)
		a.evs = append(a.evs, domain.Event{Seq: seq, Type: domain.EventUserMessage,
			Body: []byte(fmt.Sprintf(`{"content":"turn %d"}`, seq))})
	}
}

func (a *appendingLog) List(_ context.Context, _ domain.ID, q events.ListQuery) ([]domain.Event, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := pageOf(a.evs, q)
	a.append(DreamPageSize)
	return out, nil
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
	// Both ends of each item survive the middle elision, and each marker names
	// what its own budget could not keep — the marker's bytes come out of the
	// budget, so the count runs past the overflow by the marker's own width.
	inputJSON := fmt.Sprintf(`{"command":%q}`, input)
	for _, want := range []string{
		"## tool call: bash", `{"command":"iii`, `iii"}`,
		"## tool result", "rrr",
		elisionNote(len(inputJSON) - elisionKeep(len(inputJSON), dreamToolInputBudget)),
		elisionNote(len(result) - elisionKeep(len(result), dreamToolResultBudget)),
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

// Every budget is strict: the marker announcing the cut is paid for out of the
// kept bytes, so an elided string is never longer than the budget it was cut
// to. ASCII content, so no byte goes to a rune boundary and the result lands on
// the budget exactly.
func TestElideBudgetIsStrict(t *testing.T) {
	for _, max := range []int{64, 100, dreamToolInputBudget, dreamToolResultBudget} {
		for _, n := range []int{max + 1, max * 3, 100_000} {
			if got := len(elide(strings.Repeat("z", n), max)); got != max {
				t.Errorf("elide(%d bytes, budget %d) = %d bytes, want exactly the budget", n, max, got)
			}
		}
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

// The assignment shape quotes its value three ways, and a quoted value ends at
// its closing quote rather than swallowing it. A single-quoted value is the one
// that escaped both defences before this test existed: the pattern left it
// whole, so the runner's completion scan — which asks whether Redact changed
// the content — passed it too.
func TestRedactQuotedAssignments(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"double", `password="hunter2hunter2"`, `password="` + redactedSecret + `"`},
		{"single", `password='hunter2hunter2'`, `password='` + redactedSecret + `'`},
		{"backtick", "password=`hunter2hunter2`", "password=`" + redactedSecret + "`"},
		{"single around the key too", `'api-key': 'hunter2hunter2'`, `'api-key': '` + redactedSecret + `'`},
		{"backtick around the key too", "`token`: `hunter2hunter2`", "`token`: `" + redactedSecret + "`"},
		{"unquoted", `secret = hunter2hunter2`, `secret = ` + redactedSecret},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := Redact(c.in); got != c.want {
				t.Errorf("Redact(%s) = %s, want %s", c.in, got, c.want)
			}
		})
	}
}

// Redaction runs before truncation, so an elision can never split a match and
// leave a fragment: this secret straddles the tool result's head cut.
func TestRedactBeforeTruncation(t *testing.T) {
	const secret = "sk-live-STRADDLEcut0123abcd"
	const pad = 1000
	// The suffix starts with a space: a shape pattern is greedy, and an
	// unbroken run of key-alphabet bytes after the secret would be swallowed
	// into the same match and leave nothing long enough to elide.
	body := strings.Repeat("a", pad) + secret + " " + strings.Repeat("b", 3000)
	// The straddle has to be short enough that neither side is a match on its
	// own, or a redaction running after the cut would clean both up and the
	// test could not fail for the property it pins. Nine bytes of the secret
	// stay on the head side — "sk-live-S", six characters after "sk-" where the
	// pattern needs eight — and the rest goes into the elided middle. The cut
	// moves with the marker's width, so the fixture checks where it landed
	// rather than assuming.
	if got := elisionKeep(len(body), dreamToolResultBudget)/2 - pad; got != 9 {
		t.Fatalf("the fixture straddles the head cut by %d bytes, want 9", got)
	}
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
	// The cap is strict — the note is paid for out of it — and this fixture is
	// ASCII, so nothing is lost to a rune boundary and the transcript lands on
	// the cap exactly.
	if len(d.Text) != DreamTranscriptCap {
		t.Errorf("rendered %d bytes, want the cap %d, note included", len(d.Text), DreamTranscriptCap)
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
	if len(d.Text) != DreamTranscriptCap {
		t.Errorf("rendered %d bytes for a %d-byte message, want the cap %d",
			len(d.Text), len(body), DreamTranscriptCap)
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
// buffers are the only state.
//
// Two measures, because retained heap alone cannot fail for the property: a
// renderer that materialised all 50,000 events and dropped the slice before
// returning would leave nothing behind and pass. So the log itself measures the
// live heap at every page boundary — a GC and a ReadMemStats in the List call —
// and keeps the largest it sees. A materialising renderer holds its growing
// slice live across exactly those calls, so its peak climbs with the log while
// a paged renderer's does not. It is a heuristic on two counts: the sample is
// taken between pages rather than continuously, and HeapAlloc counts everything
// live, not only the renderer's share.
func TestRenderDreamMemoryBound(t *testing.T) {
	measure := func(n int) (int64, uint64, *Dream) {
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
		return int64(after.HeapAlloc) - int64(before.HeapAlloc), log.peak, d
	}
	smallHeap, smallPeak, small := measure(500)
	bigHeap, bigPeak, big := measure(50_000)

	for _, d := range []*Dream{small, big} {
		if len(d.Text) > DreamTranscriptCap {
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
	t.Logf("peak live heap at a page boundary: 500 events %d bytes, 50,000 events %d bytes", smallPeak, bigPeak)
	if bigHeap-smallHeap > slack {
		t.Errorf("retained heap grew by %d bytes between 500 and 50,000 events; the paging is not holding",
			bigHeap-smallHeap)
	}
	// 1 MiB, chosen from both footprints rather than guessed: the paged
	// renderer's peak grows by 33-41 KiB between the two runs (page buffers and
	// GC noise, measured over repeated runs), and a renderer that materialised
	// the 50,000 events grew its peak by 14.5 MiB. The bound sits an order of
	// magnitude clear of each.
	const peakSlack = 1 << 20
	if int64(bigPeak)-int64(smallPeak) > peakSlack {
		t.Errorf("peak live heap grew by %d bytes between 500 and 50,000 events; the renderer is holding the log, not paging it",
			int64(bigPeak)-int64(smallPeak))
	}
	runtime.KeepAlive(small)
	runtime.KeepAlive(big)
}

// generatedLog serves a log of n synthetic events a page at a time without
// ever holding one: a fake that materialized 50,000 events would be measuring
// itself in the bound above. It also takes that bound's peak sample — a GC and
// a ReadMemStats at each page boundary, keeping the largest live heap it sees.
type generatedLog struct {
	n    int
	peak uint64
}

func (g *generatedLog) List(_ context.Context, _ domain.ID, q events.ListQuery) ([]domain.Event, error) {
	if q.Desc {
		// The high-water mark: the newest row, which for a generated log of n
		// events is the nth.
		return []domain.Event{g.event(int64(g.n))}, nil
	}
	start := int64(0)
	if q.AfterSeq != nil {
		start = *q.AfterSeq
	}
	out := make([]domain.Event, 0, q.Limit)
	for seq := start + 1; seq <= int64(g.n) && len(out) < q.Limit; seq++ {
		out = append(out, g.event(seq))
	}
	g.sample()
	return out, nil
}

// sample records the live heap between two pages, which is where a renderer
// that accumulates has its accumulation.
func (g *generatedLog) sample() {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	if m.HeapAlloc > g.peak {
		g.peak = m.HeapAlloc
	}
}

func (g *generatedLog) event(seq int64) domain.Event {
	body := fmt.Sprintf(`{"content":"turn %d: %s"}`, seq, strings.Repeat("w", 120))
	return domain.Event{Seq: seq, Type: domain.EventUserMessage, Body: []byte(body)}
}
