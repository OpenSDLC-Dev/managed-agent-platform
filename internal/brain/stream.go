package brain

import (
	"context"
	"fmt"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
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
	usage          domain.ModelUsage
}

// streamTurn drives one provider stream, broadcasting message previews as
// deltas arrive and appending each agent.thinking as its block closes. The
// work-item lease is re-extended mid-stream so a long generation cannot be
// reclaimed while healthy.
func (b *Brain) streamTurn(ctx context.Context, sid domain.ID, item *queue.Item, p provider.Provider, req provider.Request) (*turnResult, error) {
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
	extendAt := time.Now().Add(b.cfg.LeaseTTL / 2)

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
		return err
	}

	for stream.Next() {
		if time.Now().After(extendAt) {
			if err := b.queue.Extend(ctx, item, b.cfg.LeaseTTL); err != nil {
				return nil, fmt.Errorf("mid-stream: %w", err)
			}
			extendAt = time.Now().Add(b.cfg.LeaseTTL / 2)
		}

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
					return nil, err
				}
			}

		case provider.KindTextDelta:
			if err := closeThinking(); err != nil {
				return nil, err
			}
			if msgPreview == nil {
				msgPreview, err = b.log.StartPreview(ctx, sid, domain.EventAgentMessage)
				if err != nil {
					return nil, err
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
				return nil, err
			}

		case provider.KindToolUse:
			if err := closeThinking(); err != nil {
				return nil, err
			}
			turn.toolUses = append(turn.toolUses, *c.ToolUse)

		case provider.KindDone:
			if err := closeThinking(); err != nil {
				return nil, err
			}
			turn.stopReason = c.StopReason
			if c.Usage != nil {
				turn.usage = *c.Usage
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
