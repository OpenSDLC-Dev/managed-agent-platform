package events

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// Streaming previews: while the brain is producing an agent.message or
// agent.thinking event it can broadcast best-effort preview frames to opted-in
// stream subscribers — event_start once, then (agent.message only)
// event_delta fragments whose delta type is literally "content_delta" (NOT
// the Messages API's content_block_delta). Previews are never persisted; the
// buffered event, appended later under the pre-allocated preview id,
// supersedes whatever the deltas accumulated.

// PreviewableTypes are the only event types the wire allows previews for.
func Previewable(t domain.EventType) bool {
	return t == domain.EventAgentMessage || t == domain.EventAgentThinking
}

// Preview is one in-flight previewed event.
type Preview struct {
	log       *Log
	sessionID domain.ID
	eventID   domain.ID
	typ       domain.EventType
}

// StartPreview broadcasts the event_start frame and pre-allocates the id the
// buffered event must later be appended under.
func (l *Log) StartPreview(ctx context.Context, sessionID domain.ID, typ domain.EventType) (*Preview, error) {
	if !Previewable(typ) {
		return nil, fmt.Errorf("event type %q cannot be previewed", typ)
	}
	p := &Preview{log: l, sessionID: sessionID, eventID: domain.NewID("sevt"), typ: typ}
	frame := map[string]any{
		"type": "event_start",
		"event": map[string]any{
			"id":   p.eventID.String(),
			"type": string(typ),
		},
	}
	if err := l.publishFrame(ctx, sessionID, frame); err != nil {
		return nil, err
	}
	return p, nil
}

// EventID is the pre-allocated id: append the buffered event under it so
// subscribers can reconcile deltas.
func (p *Preview) EventID() domain.ID { return p.eventID }

// Delta broadcasts one content_delta fragment for the content-array entry at
// index. Only agent.message streams deltas; agent.thinking is start-only.
// Fragments longer than a NOTIFY payload allows are split into several
// frames at the same index (append semantics make that equivalent).
func (p *Preview) Delta(ctx context.Context, index int64, text string) error {
	if p.typ != domain.EventAgentMessage {
		return fmt.Errorf("event type %q is start-only and streams no deltas", p.typ)
	}
	for _, chunk := range chunkText(text, maxDeltaTextBytes) {
		frame := map[string]any{
			"type":     "event_delta",
			"event_id": p.eventID.String(),
			"delta": map[string]any{
				"type":  "content_delta",
				"index": index,
				"content": map[string]any{
					"type": "text",
					"text": chunk,
				},
			},
		}
		if err := p.log.publishFrame(ctx, p.sessionID, frame); err != nil {
			return err
		}
	}
	return nil
}

// Postgres caps NOTIFY payloads at 8000 bytes; the frame envelope around the
// text costs ~250. Budget the JSON-escaped text well under that.
const maxDeltaTextBytes = 6000

// chunkText splits s at rune boundaries so each chunk's JSON-escaped form
// stays within maxEscaped bytes.
func chunkText(s string, maxEscaped int) []string {
	if s == "" {
		return []string{""}
	}
	var chunks []string
	var b strings.Builder
	size := 0
	for _, r := range s {
		n := escapedLen(r)
		if size+n > maxEscaped && b.Len() > 0 {
			chunks = append(chunks, b.String())
			b.Reset()
			size = 0
		}
		b.WriteRune(r)
		size += n
	}
	if b.Len() > 0 {
		chunks = append(chunks, b.String())
	}
	return chunks
}

// escapedLen is the worst-case size of one rune inside a JSON string as
// encoding/json emits it (\uXXXX escapes for control/HTML-sensitive runes).
func escapedLen(r rune) int {
	switch r {
	case '"', '\\', '\n', '\r', '\t':
		return 2
	case '<', '>', '&', '\u2028', '\u2029':
		return 6
	}
	if r < 0x20 {
		return 6
	}
	return utf8.RuneLen(r)
}
