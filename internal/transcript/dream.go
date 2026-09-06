package transcript

// The dream renderer (plan 41 §3.2). A dream reads whole sessions it did not
// run, so this renderer answers two questions the grader's never had to: what
// a session may cost in memory while it is rendered, and what may leave the
// controlplane inside the text. The answers are the same two mechanisms
// throughout — the log is streamed in pages and only a head and a rolling tail
// are kept, so peak memory is the cap whatever the log's length; and every
// rendered byte passes Redact before any truncation, so an elision can never
// split a match and leave a fragment behind.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
)

const (
	// DreamPageSize is the keyset page the renderer pulls. Large enough that a
	// long session is not a thousand round trips, small enough that one page
	// of tool results is not itself the memory bound the cap exists to hold.
	DreamPageSize = 200
	// dreamToolInputBudget caps one rendered tool call's input: the model
	// reading a dream needs what the agent asked for, not a whole file body
	// that a write call carried.
	dreamToolInputBudget = 512
	// dreamToolResultBudget caps one rendered tool result, of every kind.
	dreamToolResultBudget = 2 << 10
	// dreamPreviewRunes bounds INDEX.md's first-user-message column (§3.1).
	dreamPreviewRunes = 200
	// redactedSecret stands in for a matched credential shape.
	redactedSecret = "[REDACTED_SECRET]"
)

// DreamTranscriptCap bounds one rendered transcript, so a hundred of them are
// at most 2.4 MiB — the per-dream bound (§3.2). The head kept and the rolling
// tail are each half of it, which is what makes it a memory bound and not just
// an output length: a test that lowers the cap sees both buffers shrink with
// it. A var, not a const, for that test (memoryretention.go:48-52's idiom);
// nothing in production writes it.
var DreamTranscriptCap = 24 << 10

// Lister is the one method of *events.Log the dream renderer needs.
type Lister interface {
	List(ctx context.Context, sessionID domain.ID, q events.ListQuery) ([]domain.Event, error)
}

// Dream is one input session rendered for a dream.
type Dream struct {
	SessionID   string
	Text        []byte // the rendered markdown: redacted, per-event truncated, capped
	Turns       int    // user.message events kept (the INDEX.md "turns" column)
	ElidedBytes int    // bytes the per-transcript cap removed; 0 when the cap was not hit
	FirstUser   string // the first user message's text, redacted, one line, ≤ 200 runes
}

// RenderDream streams the session's log in pages of DreamPageSize through
// log.List with Scope events.ScopeSession and the AfterSeq keyset, renders and
// redacts each event as it arrives, and keeps only a head and a rolling tail
// of half DreamTranscriptCap each — peak memory is the cap whatever the log's
// length.
//
// The scope is the coordinator's view: its primary thread's rows plus the ones
// a child cross-posted. Per-thread logs are not expanded, so a delegated task
// reads as the report that came back (§3.2).
func RenderDream(ctx context.Context, log Lister, sessionID string) (*Dream, error) {
	w := newCapWriter(DreamTranscriptCap)
	d := &Dream{SessionID: sessionID}
	w.write("# session " + sessionID + "\n\n")

	q := events.ListQuery{Scope: events.ScopeSession, Limit: DreamPageSize}
	for {
		page, err := log.List(ctx, domain.ID(sessionID), q)
		if err != nil {
			return nil, fmt.Errorf("render transcript %s: %w", sessionID, err)
		}
		if len(page) == 0 {
			break
		}
		for _, ev := range page {
			if ev.Type == domain.EventUserMessage {
				d.Turns++
				if d.Turns == 1 {
					d.FirstUser = preview(dreamContentText(ev.Body))
				}
			}
			w.write(renderDreamEvent(ev))
		}
		if len(page) < DreamPageSize {
			break
		}
		last := page[len(page)-1].Seq
		q.AfterSeq = &last
	}

	d.Text, d.ElidedBytes = w.result()
	return d, nil
}

// renderDreamEvent renders one event as its markdown section, or "" for the
// events §3.2 drops — every session.*, span.*, user.interrupt and
// confirmation event, agent.thinking, and the agent.thread_message_sent half
// of a delegation. Redaction runs over each rendered string before the
// truncations, per this file's header.
func renderDreamEvent(ev domain.Event) string {
	switch ev.Type {
	case domain.EventUserMessage:
		return section("user", Redact(dreamContentText(ev.Body)))
	case domain.EventAgentMessage:
		return section("agent", Redact(dreamContentText(ev.Body)))
	case domain.EventSystemMessage:
		return section("system", Redact(dreamContentText(ev.Body)))
	case domain.EventAgentToolUse, domain.EventAgentMCPToolUse, domain.EventAgentCustomToolUse:
		var p struct {
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if json.Unmarshal(ev.Body, &p) != nil {
			return ""
		}
		return section("tool call: "+Redact(p.Name), elide(Redact(string(p.Input)), dreamToolInputBudget))
	case domain.EventUserToolResult, domain.EventUserCustomToolRes,
		domain.EventAgentToolResult, domain.EventAgentMCPToolResult:
		return section("tool result", elide(Redact(dreamContentText(ev.Body)), dreamToolResultBudget))
	case domain.EventAgentThreadMessageReceived:
		// The kept half of the delegation pair: in the session view this is the
		// only carrier of a child's result, because the child's own
		// submit_result stays on the child's log (§3.2).
		var p struct {
			FromAgentName string `json:"from_agent_name"`
		}
		from := "another agent"
		if json.Unmarshal(ev.Body, &p) == nil && p.FromAgentName != "" {
			from = p.FromAgentName
		}
		return section("message from "+Redact(from), Redact(dreamContentText(ev.Body)))
	}
	return ""
}

// section is one rendered event: a role or tool heading and its paragraph. The
// runner never parses this text — the model reads it.
func section(label, text string) string {
	return "## " + label + "\n\n" + text + "\n\n"
}

// dreamContentText flattens an event body's content the way ContentText does,
// under §3.2's block rules instead of the grader's: a thinking or
// redacted_thinking block is dropped, an image becomes "[image]" rather than
// vanishing, and a search_result still flattens to its evidence. It is not
// FlattenBlocks with a flag, because the grader's rendering is pinned
// byte-for-byte by its own tests and a shared knob would put this file's rules
// one boolean away from them.
func dreamContentText(body []byte) string {
	var p struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(body, &p) != nil || len(p.Content) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(p.Content, &s) == nil {
		return s
	}
	if text, ok := dreamBlocks(p.Content); ok {
		return text
	}
	return string(p.Content)
}

// dreamBlocks renders a content-block array under the rules above.
func dreamBlocks(raw json.RawMessage) (string, bool) {
	var blocks []struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Title   string          `json:"title"`
		Source  string          `json:"source"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return "", false
	}
	var sb strings.Builder
	add := func(text string) {
		if text == "" {
			return
		}
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(text)
	}
	for _, b := range blocks {
		switch b.Type {
		case "thinking", "redacted_thinking":
			continue
		case "image":
			add("[image]")
		case "search_result":
			var parts []string
			if b.Title != "" {
				parts = append(parts, b.Title)
			}
			if b.Source != "" {
				parts = append(parts, b.Source)
			}
			if nested, ok := dreamBlocks(b.Content); ok && nested != "" {
				parts = append(parts, nested)
			}
			add(strings.Join(parts, "\n"))
		default:
			add(b.Text)
		}
	}
	return sb.String(), true
}

// preview renders INDEX.md's last column: the first user message, redacted,
// collapsed to one line so a row stays a row, and cut to dreamPreviewRunes.
func preview(s string) string {
	s = strings.Join(strings.Fields(Redact(s)), " ")
	if utf8.RuneCountInString(s) <= dreamPreviewRunes {
		return s
	}
	n := 0
	for i := range s {
		if n == dreamPreviewRunes-1 {
			return s[:i] + "…"
		}
		n++
	}
	return s
}

// IndexLine is one INDEX.md row (§3.1): seq · session_id · created_at · turns
// · rendered_bytes · the first user message. What the cap elided is named in
// the bytes column rather than appended, so the one free-text field stays last
// and a "·" inside a user's message cannot look like another column.
func IndexLine(seq int, d *Dream, createdAt time.Time) string {
	size := fmt.Sprintf("%d bytes", len(d.Text))
	if d.ElidedBytes > 0 {
		size += fmt.Sprintf(" (%d elided)", d.ElidedBytes)
	}
	return fmt.Sprintf("%d · %s · %s · %d turns · %s · %s",
		seq, d.SessionID, createdAt.UTC().Format(time.RFC3339), d.Turns, size, d.FirstUser)
}

// dreamSecrets are the four shapes §3.2 names. Each replacement keeps what
// identifies the match — the "Bearer " scheme, the assignment's left-hand side
// — and replaces only the value, so a redacted transcript still shows the
// model what kind of thing was there.
var dreamSecrets = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`sk-[A-Za-z0-9._-]{8,}`), redactedSecret},
	{regexp.MustCompile(`AKIA[0-9A-Z]{12,}`), redactedSecret},
	{regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{8,}`), "${1}" + redactedSecret},
	{regexp.MustCompile(`(?i)((?:api[_-]?key|token|secret|password)"?\s*[:=]\s*"?)[^\s"',;}]+`), "${1}" + redactedSecret},
}

// Redact replaces the four secret shapes with "[REDACTED_SECRET]": an sk- key,
// an AKIA access key id, a Bearer token, and an api_key/token/secret/password
// assignment with either separator. Fixed in code, not configurable — a knob
// here is a way to leak a credential into a memory store (§3.2).
//
// It is best-effort by construction: a shape pattern cannot know a private
// key, a cookie, a connection string or an opaque token from prose, which is
// why internal/provider/redact.go redacts by known value instead — that
// redactor holds the credential it is removing, and a dream's renderer holds
// none. What backs this is the clone by default and the completion scan, not
// the pattern list.
func Redact(s string) string {
	for _, p := range dreamSecrets {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}

// capWriter keeps the transcript's first head bytes and the last tail bytes of
// everything after them, and counts the rest. Both buffers are allocated once
// at their bound, so a session of any length costs the same.
type capWriter struct {
	head, tail       []byte
	headMax, tailMax int
	total            int
}

func newCapWriter(capBytes int) *capWriter {
	head := capBytes / 2
	return &capWriter{
		head:    make([]byte, 0, head),
		tail:    make([]byte, 0, capBytes-head),
		headMax: head,
		tailMax: capBytes - head,
	}
}

func (w *capWriter) write(s string) {
	w.total += len(s)
	if n := w.headMax - len(w.head); n > 0 {
		if n > len(s) {
			n = len(s)
		}
		w.head = append(w.head, s[:n]...)
		s = s[n:]
	}
	if len(s) == 0 {
		return
	}
	if len(s) >= w.tailMax {
		w.tail = append(w.tail[:0], s[len(s)-w.tailMax:]...)
		return
	}
	// Shift out just enough of the tail to make room; copy handles the
	// overlap, and the capacity never grows.
	if over := len(w.tail) + len(s) - w.tailMax; over > 0 {
		w.tail = append(w.tail[:0], w.tail[over:]...)
	}
	w.tail = append(w.tail, s...)
}

// result assembles the transcript and reports what the cap removed. Under the
// cap the two buffers are the whole text and never overlap — the tail only
// ever saw the bytes the head refused.
func (w *capWriter) result() ([]byte, int) {
	if w.total <= w.headMax+w.tailMax {
		return append(w.head, w.tail...), 0
	}
	head := trimPartialEnd(w.head)
	tail := trimPartialStart(w.tail)
	elided := w.total - len(head) - len(tail)
	out := append(head, elisionNote(elided)...)
	return append(out, tail...), elided
}

// elide cuts s to max bytes from the middle, so a truncated tool result still
// shows how it started and how it ended.
func elide(s string, max int) string {
	if len(s) <= max {
		return s
	}
	head := trimPartialEnd([]byte(s[:max/2]))
	tail := trimPartialStart([]byte(s[len(s)-(max-max/2):]))
	return string(head) + elisionNote(len(s)-len(head)-len(tail)) + string(tail)
}

// elisionNote is the one marker form both cuts emit.
func elisionNote(n int) string { return fmt.Sprintf("\n[… %d bytes elided …]\n", n) }

// trimPartialEnd and trimPartialStart drop the rune a byte-level cut split:
// the incomplete sequence a head ends on, the continuation bytes a tail starts
// with. A U+FFFD the text itself carried decodes as a 3-byte rune and stays.
func trimPartialEnd(b []byte) []byte {
	for range utf8.UTFMax {
		if r, size := utf8.DecodeLastRune(b); r != utf8.RuneError || size != 1 {
			break
		}
		b = b[:len(b)-1]
	}
	return b
}

func trimPartialStart(b []byte) []byte {
	for range utf8.UTFMax {
		if r, size := utf8.DecodeRune(b); r != utf8.RuneError || size != 1 {
			break
		}
		b = b[1:]
	}
	return b
}
