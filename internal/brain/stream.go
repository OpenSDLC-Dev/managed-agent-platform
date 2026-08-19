package brain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
)

// turnResult is one model response translated into event material.
type turnResult struct {
	// text holds the response's text blocks in arrival order; the wire
	// agent.message content is text-only.
	text []domain.ContentBlock
	// messageEventID is the preview-reserved id the buffered agent.message
	// must be appended under (zero when the turn produced no text).
	messageEventID domain.ID
	toolUses       []provider.ToolUse
	stopReason     string
	// usage is what the model reported, or nil when the endpoint reported
	// nothing — the two are different facts and only the first belongs in
	// the token metric (#90). A turn that never reached its done chunk also
	// leaves this nil.
	usage *domain.ModelUsage
	// firstTokenAt is when the first content delta — thinking or text —
	// arrived from the model, the stop side of the time-to-first-token
	// measurement. Zero for a turn that streamed no content (a straight-to-tool
	// call, an empty stream): there is no first token, so none is recorded.
	firstTokenAt time.Time
}

// streamTurn drives one provider stream, broadcasting message previews as
// deltas arrive and appending each agent.thinking as its block closes. The
// lease keeper runs alongside; this function only distinguishes the two
// failure worlds — provider errors surface bare (they become the turn's
// session.error), brain-side database failures wrap as infra (the turn is
// abandoned to lease expiry, not reported as a model failure).
func (b *Brain) streamTurn(ctx context.Context, sid, threadID domain.ID, p provider.Provider, req provider.Request) (*turnResult, error) {
	stream, err := p.Generate(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("model request: %w", err)
	}
	defer func() { _ = stream.Close() }()

	turn := &turnResult{}
	var msgPreview, thinkingPreview *events.Preview
	var thinkingIndex int64
	// entry maps a provider content-block index to its slot in turn.text:
	// the wire delta index addresses "which entry in the previewed event's
	// content array", not the provider's block numbering.
	entry := map[int64]int{}

	closeThinking := func() error {
		if thinkingPreview == nil {
			return nil
		}
		// The buffered event carries the preview's reserved id — that id
		// match is what concludes the start-only preview client-side. On
		// the turn's thread, where the preview went (plan 35 decision 2).
		_, err := b.log.Append(ctx, sid, []events.NewEvent{
			{ID: thinkingPreview.EventID(), Type: domain.EventAgentThinking, ThreadID: threadID},
		})
		thinkingPreview = nil
		if err != nil {
			return infra("close thinking: %w", err)
		}
		return nil
	}

	for stream.Next() {
		c := stream.Chunk()
		switch c.Kind {
		case provider.KindThinkingDelta:
			// Thinking is the first content a reasoning model streams, so it is
			// the first token whenever it comes before any text. Guarded on content
			// like the text path below: a content-free thinking delta — empty or
			// all NULs, the text lane's own rule — is not a token.
			if turn.firstTokenAt.IsZero() && toolset.SanitizeText(c.Text) != "" {
				turn.firstTokenAt = time.Now()
			}
			// The preview is start-only (agent.thinking carries no content);
			// one event per thinking block — a delta on a new provider block
			// index closes the previous block's event and opens the next.
			if thinkingPreview != nil && c.Index != thinkingIndex {
				if err := closeThinking(); err != nil {
					return nil, err
				}
			}
			if thinkingPreview == nil {
				thinkingIndex = c.Index
				thinkingPreview, err = b.log.StartPreviewOn(ctx, sid, threadID, domain.EventAgentThinking)
				if err != nil {
					return nil, infra("thinking preview: %w", err)
				}
			}

		case provider.KindTextDelta:
			if err := closeThinking(); err != nil {
				return nil, err
			}
			// The model is a NUL producer like any other (#228): Postgres
			// jsonb cannot store `\u0000`, so one NUL in a delta would fault
			// the buffered agent.message append and reclaim-loop the turn.
			// Stripped at arrival — ahead of the empty guard, so a delta of
			// nothing but NULs is an empty delta.
			c.Text = toolset.SanitizeText(c.Text)
			// An empty delta adds no text. Skipping it before anything is
			// allocated keeps the content array dense: a block that never
			// produces text gets no entry, so the preview's delta indices
			// and the buffered event's content indices always agree, and
			// no preview is opened for an event that will never land.
			if c.Text == "" {
				continue
			}
			// The first non-empty text delta is the first token when no thinking
			// preceded it. Stamped after the empty-delta guard so a block that
			// streams only empty deltas does not count as content.
			if turn.firstTokenAt.IsZero() {
				turn.firstTokenAt = time.Now()
			}
			if msgPreview == nil {
				msgPreview, err = b.log.StartPreviewOn(ctx, sid, threadID, domain.EventAgentMessage)
				if err != nil {
					return nil, infra("message preview: %w", err)
				}
				turn.messageEventID = msgPreview.EventID()
			}
			idx, ok := entry[c.Index]
			if !ok {
				idx = len(turn.text)
				entry[c.Index] = idx
				turn.text = append(turn.text, domain.ContentBlock{Type: "text"})
			}
			turn.text[idx].Text += c.Text
			if err := msgPreview.Delta(ctx, int64(idx), c.Text); err != nil {
				return nil, infra("message delta: %w", err)
			}

		case provider.KindToolUse:
			if err := closeThinking(); err != nil {
				return nil, err
			}
			// The event we are about to durably emit must carry a JSON
			// object: the log is append-only, and a tool_use block whose
			// input is `"oops"` or a truncated `{` would either abort every
			// settlement (a silent reclaim loop) or make the model reject
			// every future replay of this session.
			tu := *c.ToolUse
			tu.Name = toolset.SanitizeText(tu.Name)
			input, err := normalizeToolInput(tu.Input)
			if err != nil {
				return nil, fmt.Errorf("tool %q: %w", tu.Name, err)
			}
			tu.Input = input
			turn.toolUses = append(turn.toolUses, tu)

		case provider.KindDone:
			if err := closeThinking(); err != nil {
				return nil, err
			}
			turn.stopReason = c.StopReason
			if c.Usage != nil {
				// Copied, not aliased: the chunk belongs to the provider,
				// and this value outlives the stream it came from.
				u := *c.Usage
				turn.usage = &u
			}
		}
	}
	if err := stream.Err(); err != nil {
		return nil, fmt.Errorf("model stream: %w", err)
	}
	if turn.stopReason == "" {
		return nil, fmt.Errorf("model stream ended without a stop reason")
	}
	return turn, nil
}

func stripValue(v any) (any, error) {
	switch x := v.(type) {
	case string:
		return toolset.SanitizeText(x), nil
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			k = toolset.SanitizeText(k)
			// Two different keys that collide once stripped would leave Go
			// map iteration to pick which value survives — a write targeting
			// either of two files on a coin flip. Fail the turn visibly.
			if _, dup := m[k]; dup {
				return nil, fmt.Errorf("input keys collide at %q after NUL strip", k)
			}
			var err error
			if m[k], err = stripValue(e); err != nil {
				return nil, err
			}
		}
		return m, nil
	case []any:
		for i, e := range x {
			var err error
			if x[i], err = stripValue(e); err != nil {
				return nil, err
			}
		}
		return x, nil
	}
	return v, nil
}

// normalizeToolInput accepts an absent or null input as the empty object,
// rejects anything that is not a JSON object, and re-encodes what it accepts
// through Go rather than passing the model's bytes along. The rewrite is what
// makes the payload storable: the input is the one model-produced value that
// reaches jsonb as raw bytes — every Go-string lane is coerced to valid UTF-8
// at marshal time, this one is not — so a NUL escape, a lone surrogate escape
// (well-formed JSON to Go, SQLSTATE 22P02 to Postgres), or raw invalid UTF-8
// (22021) would each fault the append and reclaim-loop the item (#228).
// Decoding launders the last two to U+FFFD, stripValue removes the NULs, and
// UseNumber carries every numeric lexeme through untouched. The null case is
// decided by the decode, not by comparing bytes: decoding any JSON null —
// padded with whitespace or not — into a map leaves it nil and reports no
// error, so a byte comparison would wave ` null ` through as a valid object.
func normalizeToolInput(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage("{}"), nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, fmt.Errorf("input must be a JSON object: %w", err)
	}
	// Probing with Token, not More: More treats a trailing ] or } as the end
	// of a collection and reports nothing left, which would silently discard
	// the malformed suffix and execute the valid prefix of corrupted output.
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("input must be a single JSON object")
	}
	if obj == nil {
		return json.RawMessage("{}"), nil
	}
	v, err := stripValue(obj)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}
