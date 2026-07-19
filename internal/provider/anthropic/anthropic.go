// Package anthropic adapts any endpoint speaking the Anthropic Messages
// protocol to the provider interface, via the official SDK with a
// configurable base URL — an enterprise gateway, a proxy, or a self-hosted
// model are all just configuration. Requests pass through near-verbatim:
// content blocks and tool definitions are the Anthropic wire shapes already.
package anthropic

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
)

// New constructs the adapter from configuration alone.
func New(cfg provider.Config) (provider.Provider, error) {
	if cfg.BaseURL == "" {
		// Deliberate: no silent fallback to api.anthropic.com (CLAUDE.md
		// principle 4) — pointing at an endpoint is a conscious choice.
		return nil, fmt.Errorf("anthropic provider requires a base_url")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("anthropic provider requires a model")
	}
	opts := []option.RequestOption{
		// Constructed purely from configuration: without this, the SDK
		// autoloads ambient ANTHROPIC_* credentials (auth-token env,
		// profile files) underneath our options and would leak the
		// operator's real Anthropic credential to a third-party base_url.
		option.WithoutEnvironmentDefaults(),
		option.WithBaseURL(cfg.BaseURL),
		option.WithAPIKey(cfg.APIKey),
	}
	for k, v := range cfg.Headers {
		opts = append(opts, option.WithHeader(k, v))
	}
	client := sdk.NewClient(opts...)
	return &anthropicProvider{client: client, model: cfg.Model, redact: provider.NewRedactor(cfg)}, nil
}

type anthropicProvider struct {
	client sdk.Client
	model  string
	// The SDK's API error quotes the whole response body and the request URL,
	// so both an echoed credential and one carried in base_url reach its text.
	redact provider.Redactor
}

// defaultMaxTokens applies when the request doesn't set one; the wire field
// is required.
const defaultMaxTokens = 8192

func (p *anthropicProvider) Generate(ctx context.Context, req provider.Request) (provider.Stream, error) {
	params := sdk.MessageNewParams{
		Model:     sdk.Model(p.model),
		MaxTokens: req.MaxTokens,
	}
	if params.MaxTokens == 0 {
		params.MaxTokens = defaultMaxTokens
	}
	if req.System != "" {
		params.System = []sdk.TextBlockParam{{Text: req.System}}
	}
	// param.SetJSON serializes the raw wire bytes verbatim. Round-tripping
	// through the SDK's typed variants instead would silently drop fields
	// and tool types the pinned SDK version doesn't model yet; validity is
	// the endpoint's judgment, not this adapter's.
	for i, m := range req.Messages {
		role, err := json.Marshal(m.Role)
		if err != nil {
			return nil, err
		}
		wire, err := json.Marshal(map[string]json.RawMessage{
			"role":    role,
			"content": m.Content,
		})
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		var mp sdk.MessageParam
		param.SetJSON(wire, &mp)
		params.Messages = append(params.Messages, mp)
	}
	for i, t := range req.Tools {
		if !json.Valid(t) {
			return nil, fmt.Errorf("tools[%d]: invalid JSON", i)
		}
		var tp sdk.ToolUnionParam
		param.SetJSON(t, &tp)
		params.Tools = append(params.Tools, tp)
	}

	events := p.client.Messages.NewStreaming(ctx, params)
	if err := events.Err(); err != nil {
		return nil, p.redact.Error(err)
	}
	// The stream carries the redactor too: an endpoint can report a failure
	// mid-stream under HTTP 200, and that error surfaces from Err() after
	// Next(), never from here.
	return &stream{events: events, redact: p.redact}, nil
}

// stream translates the Messages API event stream into provider chunks:
// text/thinking deltas pass through as they arrive; tool_use inputs
// accumulate and emit once complete; message_delta carries stop reason and
// output usage, emitted as the final done chunk.
type stream struct {
	events *ssestream.Stream[sdk.MessageStreamEventUnion]
	redact provider.Redactor
	cur    provider.Chunk
	err    error

	// tool_use accumulation for the currently open block
	toolIndex int64
	toolID    string
	toolName  string
	toolJSON  []byte
	toolSeed  json.RawMessage // complete input carried on content_block_start
	inTool    bool

	usage      domain.ModelUsage
	stopReason string
	done       bool
}

func (s *stream) Chunk() provider.Chunk { return s.cur }
func (s *stream) Err() error            { return s.err }
func (s *stream) Close() error          { return s.events.Close() }

func (s *stream) Next() bool {
	if s.err != nil || s.done {
		return false
	}
	for s.events.Next() {
		ev := s.events.Current()
		switch ev.Type {
		case "message_start":
			u := ev.Message.Usage
			s.usage.InputTokens = u.InputTokens
			s.usage.CacheCreationInputTokens = u.CacheCreationInputTokens
			s.usage.CacheReadInputTokens = u.CacheReadInputTokens

		case "content_block_start":
			if ev.ContentBlock.Type == "tool_use" {
				if s.inTool {
					// Losing the still-open tool call silently would make
					// the brain dispatch a different tool set than the
					// model produced; a malformed stream must fail loudly.
					s.err = fmt.Errorf("tool_use block %d started before block %d closed", ev.Index, s.toolIndex)
					return false
				}
				s.inTool = true
				s.toolIndex = ev.Index
				s.toolID = ev.ContentBlock.ID
				s.toolName = ev.ContentBlock.Name
				s.toolJSON = s.toolJSON[:0]
				s.toolSeed = nil
				// The official protocol starts tool blocks with input {}
				// and streams the JSON via deltas, but other
				// Anthropic-protocol endpoints may put the complete input
				// on the start block; dropping it would invoke the tool
				// with empty arguments. The raw bytes pass through
				// verbatim — re-encoding a decoded value would round large
				// integers through float64.
				if raw := ev.ContentBlock.JSON.Input.Raw(); raw != "" && raw != "{}" && raw != "null" {
					s.toolSeed = json.RawMessage(raw)
				}
			}

		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				s.cur = provider.Chunk{Kind: provider.KindTextDelta, Index: ev.Index, Text: ev.Delta.Text}
				return true
			case "thinking_delta":
				s.cur = provider.Chunk{Kind: provider.KindThinkingDelta, Index: ev.Index, Text: ev.Delta.Thinking}
				return true
			case "input_json_delta":
				if s.inTool && ev.Index == s.toolIndex {
					s.toolJSON = append(s.toolJSON, ev.Delta.PartialJSON...)
				}
			}

		case "content_block_stop":
			if s.inTool && ev.Index == s.toolIndex {
				s.inTool = false
				// Streamed deltas are authoritative; the start-block seed
				// covers endpoints that sent the input up front instead.
				// The emitted input must be a copy — s.toolJSON's backing
				// array is reused by the next tool block, and an aliased
				// slice would corrupt an already-delivered chunk.
				input := json.RawMessage(append([]byte(nil), s.toolJSON...))
				if len(input) == 0 {
					input = s.toolSeed
				}
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				s.cur = provider.Chunk{Kind: provider.KindToolUse, Index: ev.Index, ToolUse: &provider.ToolUse{
					ID: s.toolID, Name: s.toolName, Input: input,
				}}
				return true
			}

		case "message_delta":
			if r := string(ev.Delta.StopReason); r != "" {
				s.stopReason = r
			}
			// Cumulative counters override the message_start snapshot when
			// the endpoint reports them here; a frame without usage must
			// not zero what an earlier frame already reported.
			if ev.Usage.OutputTokens > 0 {
				s.usage.OutputTokens = ev.Usage.OutputTokens
			}
			if ev.Usage.InputTokens > 0 {
				s.usage.InputTokens = ev.Usage.InputTokens
			}
			if ev.Usage.CacheCreationInputTokens > 0 {
				s.usage.CacheCreationInputTokens = ev.Usage.CacheCreationInputTokens
			}
			if ev.Usage.CacheReadInputTokens > 0 {
				s.usage.CacheReadInputTokens = ev.Usage.CacheReadInputTokens
			}

		case "message_stop":
			if s.inTool {
				s.err = fmt.Errorf("message_stop with tool_use block %d still open", s.toolIndex)
				return false
			}
			if s.stopReason == "" {
				// Without message_delta there is no stop reason and no
				// output usage; a hollow done chunk would silently corrupt
				// session state downstream.
				s.err = fmt.Errorf("message_stop without a stop reason (missing message_delta)")
				return false
			}
			s.done = true
			usage := s.usage
			s.cur = provider.Chunk{Kind: provider.KindDone, StopReason: s.stopReason, Usage: &usage}
			return true
		}
	}
	s.err = s.redact.Error(s.events.Err())
	// A drained stream without message_stop is a truncated turn, not a
	// success: callers must never mistake it for a turn that merely
	// produced no done chunk.
	if s.err == nil {
		s.err = fmt.Errorf("model stream ended before message_stop")
	}
	return false
}
