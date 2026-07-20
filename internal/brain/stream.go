package brain

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
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
}

// streamTurn drives one provider stream, broadcasting message previews as
// deltas arrive and appending each agent.thinking as its block closes. The
// lease keeper runs alongside; this function only distinguishes the two
// failure worlds — provider errors surface bare (they become the turn's
// session.error), brain-side database failures wrap as infra (the turn is
// abandoned to lease expiry, not reported as a model failure).
func (b *Brain) streamTurn(ctx context.Context, sid domain.ID, p provider.Provider, req provider.Request) (*turnResult, error) {
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
		// match is what concludes the start-only preview client-side.
		_, err := b.log.Append(ctx, sid, []events.NewEvent{
			{ID: thinkingPreview.EventID(), Type: domain.EventAgentThinking},
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
				thinkingPreview, err = b.log.StartPreview(ctx, sid, domain.EventAgentThinking)
				if err != nil {
					return nil, infra("thinking preview: %w", err)
				}
			}

		case provider.KindTextDelta:
			if err := closeThinking(); err != nil {
				return nil, err
			}
			// An empty delta adds no text. Skipping it before anything is
			// allocated keeps the content array dense: a block that never
			// produces text gets no entry, so the preview's delta indices
			// and the buffered event's content indices always agree, and
			// no preview is opened for an event that will never land.
			if c.Text == "" {
				continue
			}
			if msgPreview == nil {
				msgPreview, err = b.log.StartPreview(ctx, sid, domain.EventAgentMessage)
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

// normalizeToolInput accepts an absent or null input as the empty object and
// rejects anything that is not a JSON object. The null case is decided by the
// decode, not by comparing bytes: unmarshalling any JSON null — padded with
// whitespace or not — into a map leaves it nil and reports no error, so a
// byte comparison would wave ` null ` through as a valid object.
func normalizeToolInput(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage("{}"), nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("input must be a JSON object: %w", err)
	}
	if obj == nil {
		return json.RawMessage("{}"), nil
	}
	return raw, nil
}
