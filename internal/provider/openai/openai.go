// Package openai adapts an OpenAI Chat Completions endpoint (OpenAI itself, a
// vLLM server, or an internal OpenAI-compatible gateway) to the provider
// interface. Unlike the anthropic adapter, which is near-verbatim, this is the
// platform's lossy seam: the internal Request is Anthropic-native, so every turn
// is translated to OpenAI wire on the way out and back on the way in. All of that
// conversion is confined here and tested against a fake Chat Completions server.
//
// base_url is the API root, the same convention as the anthropic provider: the
// adapter appends /v1/chat/completions. Set it to e.g. https://api.openai.com or
// https://vllm.internal, NOT .../v1.
//
// Known lossy gaps (documented, not silent): Anthropic thinking blocks have no
// Chat Completions equivalent and are dropped; image blocks are not yet mapped
// and an unsupported block (top-level or inside a tool_result) fails loudly
// rather than vanishing; a tool_result's is_error flag is dropped (OpenAI's
// tool message has no error field) — the error text the platform embeds in the
// result content is still forwarded, so the model sees the failure, only the
// boolean is lost. Incoming, the deprecated single-function_call streaming
// format is rejected loudly (the endpoint must emit tool_calls) rather than
// silently losing the call.
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// New constructs the adapter from configuration alone.
func New(cfg provider.Config) (provider.Provider, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("openai provider requires a base_url")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("openai provider requires a model")
	}
	return &openaiProvider{
		endpoint: strings.TrimRight(cfg.BaseURL, "/") + "/v1/chat/completions",
		apiKey:   cfg.APIKey,
		model:    cfg.Model,
		headers:  cfg.Headers,
		client:   http.DefaultClient,
	}, nil
}

type openaiProvider struct {
	endpoint string
	apiKey   string
	model    string
	headers  map[string]string
	client   *http.Client
}

func (p *openaiProvider) Generate(ctx context.Context, req provider.Request) (provider.Stream, error) {
	messages, err := convertMessages(req.System, req.Messages)
	if err != nil {
		return nil, err
	}
	tools, err := convertTools(req.Tools)
	if err != nil {
		return nil, err
	}
	body := chatRequest{
		Model:         p.model,
		Messages:      messages,
		MaxTokens:     req.MaxTokens,
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
		Tools:         tools,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	for k, v := range p.headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("openai endpoint returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return &stream{body: resp.Body, r: bufio.NewReader(resp.Body)}, nil
}

// --- outgoing request shapes (Anthropic-native -> OpenAI Chat Completions) ---

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	// max_tokens is the field vLLM and the OpenAI-compatible gateways this
	// adapter targets accept; only api.openai.com's newest reasoning models
	// have switched to max_completion_tokens. Omitted when zero so the endpoint
	// applies its own default.
	MaxTokens     int64          `json:"max_tokens,omitempty"`
	Stream        bool           `json:"stream"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
	Tools         []chatTool     `json:"tools,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatToolCall struct {
	ID       string     `json:"id"`
	Type     string     `json:"type"`
	Function chatCallFn `json:"function"`
}

type chatCallFn struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // OpenAI carries tool arguments as a JSON string
}

type chatTool struct {
	Type     string     `json:"type"`
	Function chatToolFn `json:"function"`
}

type chatToolFn struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// contentBlock is one Anthropic content block, enough to route by type.
type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`          // tool_use
	Name      string          `json:"name"`        // tool_use
	Input     json.RawMessage `json:"input"`       // tool_use
	ToolUseID string          `json:"tool_use_id"` // tool_result
	Content   json.RawMessage `json:"content"`     // tool_result: string or block array
}

func strp(s string) *string { return &s }

// convertMessages turns the Anthropic-native turns into OpenAI messages. A user
// turn carrying tool_result blocks fans out into one `tool` role message per
// result (OpenAI's shape), and an assistant turn's tool_use blocks become
// tool_calls on the assistant message.
func convertMessages(system string, msgs []provider.Message) ([]chatMessage, error) {
	var out []chatMessage
	if system != "" {
		out = append(out, chatMessage{Role: "system", Content: strp(system)})
	}
	for i, m := range msgs {
		text, isString, err := decodeContent(m.Content)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		if isString {
			out = append(out, chatMessage{Role: m.Role, Content: strp(text)})
			continue
		}
		var blocks []contentBlock
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		var textParts []string
		var toolCalls []chatToolCall
		var toolResults []chatMessage
		for j, b := range blocks {
			switch b.Type {
			case "text":
				textParts = append(textParts, b.Text)
			case "tool_use":
				args, err := compactJSON(b.Input)
				if err != nil {
					return nil, fmt.Errorf("messages[%d].blocks[%d]: %w", i, j, err)
				}
				toolCalls = append(toolCalls, chatToolCall{
					ID: b.ID, Type: "function",
					Function: chatCallFn{Name: b.Name, Arguments: args},
				})
			case "tool_result":
				// b.IsError is intentionally dropped: OpenAI's tool message has
				// no error field. The error text is in b.Content, which is
				// forwarded, so the failure still reaches the model (see the
				// package doc's lossy-gaps note).
				content, err := toolResultText(b.Content)
				if err != nil {
					return nil, fmt.Errorf("messages[%d].blocks[%d]: %w", i, j, err)
				}
				toolResults = append(toolResults, chatMessage{
					Role: "tool", ToolCallID: b.ToolUseID, Content: strp(content),
				})
			case "thinking", "redacted_thinking":
				// No Chat Completions equivalent; dropped by design.
			default:
				return nil, fmt.Errorf("messages[%d].blocks[%d]: unsupported content block %q", i, j, b.Type)
			}
		}
		// tool_result blocks belong to a user turn and map to tool messages,
		// which must precede any user text so they answer the prior assistant.
		out = append(out, toolResults...)
		if len(textParts) > 0 || len(toolCalls) > 0 {
			msg := chatMessage{Role: m.Role, ToolCalls: toolCalls}
			if len(textParts) > 0 {
				msg.Content = strp(strings.Join(textParts, ""))
			}
			out = append(out, msg)
		}
	}
	return out, nil
}

// decodeContent reports whether the raw content is a JSON string (and its value)
// or something else (an array of blocks).
func decodeContent(raw json.RawMessage) (string, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", false, fmt.Errorf("empty content")
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return "", false, err
		}
		return s, true, nil
	}
	return "", false, nil
}

// toolResultText flattens an Anthropic tool_result content (a string, or an
// array of blocks) into the plain text OpenAI's tool message carries. A
// non-text block (e.g. an image) has no representation in an OpenAI tool
// message, so it fails loudly rather than silently vanishing — matching how an
// unsupported top-level content block is handled.
func toolResultText(raw json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", nil
	}
	if s, isString, err := decodeContent(raw); err != nil {
		return "", err
	} else if isString {
		return s, nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", err
	}
	var parts []string
	for _, b := range blocks {
		if b.Type != "text" {
			return "", fmt.Errorf("unsupported tool_result content block %q (OpenAI tool messages are text-only)", b.Type)
		}
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, ""), nil
}

func compactJSON(raw json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "{}", nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// anthropic tool def -> OpenAI function tool.
func convertTools(tools []json.RawMessage) ([]chatTool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]chatTool, 0, len(tools))
	for i, t := range tools {
		var def struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		}
		if err := json.Unmarshal(t, &def); err != nil {
			return nil, fmt.Errorf("tools[%d]: %w", i, err)
		}
		if def.Name == "" {
			return nil, fmt.Errorf("tools[%d]: missing name", i)
		}
		out = append(out, chatTool{
			Type: "function",
			Function: chatToolFn{
				Name: def.Name, Description: def.Description, Parameters: def.InputSchema,
			},
		})
	}
	return out, nil
}

// --- incoming stream translation (OpenAI SSE -> provider chunks) ---

type stream struct {
	body io.ReadCloser
	r    *bufio.Reader

	pending []provider.Chunk
	cur     provider.Chunk
	err     error
	done    bool

	tools     map[int]*toolAccum
	toolOrder []int

	stopReason string
	usage      domain.ModelUsage
	sawFinish  bool // a finish_reason arrived
	sawTools   bool // at least one tool call was accumulated this turn
	completed  bool // the done chunk has been queued
}

type toolAccum struct {
	id   string
	name string
	args []byte
}

func (s *stream) Chunk() provider.Chunk { return s.cur }
func (s *stream) Err() error            { return s.err }

func (s *stream) Close() error {
	// Only when the turn completed normally is there a bounded tail to drain
	// (the few bytes after `data: [DONE]`) — draining it lets net/http pool the
	// keep-alive connection across a session's many turns. On an early or
	// errored close the body may still be open, and an unbounded drain would
	// block until the upstream EOFs; since the brain closes the stream in a
	// defer before releasing the turn's lease, that would wedge the session on a
	// hung endpoint. So skip the drain and close immediately in that case.
	if s.completed {
		_, _ = io.Copy(io.Discard, s.r)
	}
	return s.body.Close()
}

func (s *stream) Next() bool {
	if s.err != nil || s.done {
		return false
	}
	for {
		if len(s.pending) > 0 {
			s.cur = s.pending[0]
			s.pending = s.pending[1:]
			if s.cur.Kind == provider.KindDone {
				s.done = true
			}
			return true
		}
		data, status, err := s.readData()
		if err != nil {
			s.err = err
			return false
		}
		switch status {
		case statusData:
			if err := s.process(data); err != nil {
				s.err = err
				return false
			}
		case statusDone:
			// The server sent `[DONE]` — it signalled a complete turn, even if
			// (some minimal implementations) it never populated finish_reason.
			s.complete()
		case statusEOF:
			// The body ended with no `[DONE]`. If a finish_reason arrived, the
			// turn is complete and the missing terminator is benign; otherwise
			// the stream was cut off mid-turn — a truncated turn, not a success.
			if !s.sawFinish {
				s.err = fmt.Errorf("openai stream ended before completion (no finish_reason or [DONE])")
				return false
			}
			s.complete()
		}
	}
}

// complete queues the terminal done chunk exactly once. stop_reason is tool_use
// whenever the stream carried any tool call — the single signal the brain acts
// on to run tools — regardless of the server's finish_reason, since some
// OpenAI-compatible servers end a tool turn with "stop"/"length". Otherwise it
// is the mapped finish_reason (or end_turn when none arrived).
func (s *stream) complete() {
	if s.completed {
		return
	}
	s.completed = true
	s.flushTools()
	stop := s.stopReason
	if s.sawTools {
		stop = "tool_use"
	} else if stop == "" {
		stop = "end_turn"
	}
	usage := s.usage
	s.pending = append(s.pending, provider.Chunk{Kind: provider.KindDone, StopReason: stop, Usage: &usage})
}

type readStatus int

const (
	statusData readStatus = iota // a `data:` JSON payload to process
	statusDone                   // the `[DONE]` terminator
	statusEOF                    // the body ended with no `[DONE]`
)

// readData returns the next SSE `data:` payload and how the read terminated.
func (s *stream) readData() (string, readStatus, error) {
	for {
		line, err := s.r.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\r\n")
			if rest, found := strings.CutPrefix(line, "data:"); found {
				data := strings.TrimSpace(rest)
				if data == "[DONE]" {
					return "", statusDone, nil
				}
				if data != "" {
					return data, statusData, nil
				}
			}
			// other lines (event:, comments, blanks) are ignored
		}
		if err != nil {
			if err == io.EOF {
				return "", statusEOF, nil
			}
			return "", statusEOF, err
		}
	}
}

func (s *stream) process(payload string) error {
	var fr struct {
		Choices []struct {
			Delta struct {
				Content   *string `json:"content"`
				Refusal   *string `json:"refusal"` // OpenAI safety refusal text
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
				// The deprecated single-function-call field. This adapter speaks
				// the tool_calls format; a server still emitting function_call is
				// rejected loudly rather than silently dropping the tool call.
				FunctionCall *json.RawMessage `json:"function_call"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens        int64 `json:"prompt_tokens"`
			CompletionTokens    int64 `json:"completion_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
		// Some gateways/vLLM report a failure mid-stream under HTTP 200 as an
		// error frame rather than an HTTP status; surface it instead of letting
		// the turn look truncated.
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(payload), &fr); err != nil {
		return fmt.Errorf("openai stream frame: %w", err)
	}
	if fr.Error != nil {
		return fmt.Errorf("openai stream error: %s", fr.Error.Message)
	}
	if fr.Usage != nil {
		// prompt_tokens counts cached tokens too; split the cached subset out so
		// InputTokens carries only fresh input, matching the Anthropic usage
		// shape the domain and the anthropic adapter use.
		cached := int64(0)
		if fr.Usage.PromptTokensDetails != nil {
			cached = fr.Usage.PromptTokensDetails.CachedTokens
		}
		if cached > fr.Usage.PromptTokens {
			// A malformed server reporting more cached than total would push
			// InputTokens negative into session usage; clamp instead.
			cached = fr.Usage.PromptTokens
		}
		s.usage.InputTokens = fr.Usage.PromptTokens - cached
		s.usage.CacheReadInputTokens = cached
		s.usage.OutputTokens = fr.Usage.CompletionTokens
	}
	for _, ch := range fr.Choices {
		if ch.Delta.FunctionCall != nil {
			return fmt.Errorf("openai stream used the deprecated function_call format; the endpoint must emit tool_calls")
		}
		if ch.Delta.Content != nil && *ch.Delta.Content != "" {
			s.pending = append(s.pending, provider.Chunk{Kind: provider.KindTextDelta, Index: 0, Text: *ch.Delta.Content})
		}
		// A refusal is the assistant's user-visible reply; without this it would
		// vanish and the turn would complete with no agent.message.
		if ch.Delta.Refusal != nil && *ch.Delta.Refusal != "" {
			s.pending = append(s.pending, provider.Chunk{Kind: provider.KindTextDelta, Index: 0, Text: *ch.Delta.Refusal})
		}
		for _, tc := range ch.Delta.ToolCalls {
			s.sawTools = true
			acc := s.toolAcc(tc.Index)
			if tc.ID != "" {
				acc.id = tc.ID
			}
			if tc.Function.Name != "" {
				acc.name = tc.Function.Name
			}
			acc.args = append(acc.args, tc.Function.Arguments...)
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			// Record the non-tool stop reason; complete() decides the final
			// value (tool_use wins whenever the stream carried tool calls).
			s.stopReason = mapFinishReason(*ch.FinishReason)
			s.sawFinish = true
		}
	}
	return nil
}

func (s *stream) toolAcc(index int) *toolAccum {
	if s.tools == nil {
		s.tools = map[int]*toolAccum{}
	}
	acc, ok := s.tools[index]
	if !ok {
		acc = &toolAccum{}
		s.tools[index] = acc
		s.toolOrder = append(s.toolOrder, index)
	}
	return acc
}

// flushTools emits the accumulated tool calls as tool_use chunks, in the order
// they first appeared. OpenAI has no per-tool-call stop event, so completion
// (a finish_reason or [DONE]) is the signal that the tool calls are whole.
func (s *stream) flushTools() {
	for _, idx := range s.toolOrder {
		acc := s.tools[idx]
		input := json.RawMessage(acc.args)
		if len(bytes.TrimSpace(input)) == 0 {
			input = json.RawMessage("{}")
		}
		s.pending = append(s.pending, provider.Chunk{
			Kind:    provider.KindToolUse,
			Index:   int64(idx),
			ToolUse: &provider.ToolUse{ID: acc.id, Name: acc.name, Input: input},
		})
	}
	s.toolOrder = nil
	s.tools = nil
}

// mapFinishReason maps an OpenAI finish_reason onto the Anthropic stop_reason
// vocabulary for a turn that carried NO tool calls (tool_use is decided in
// complete() from whether the stream carried tool calls, not from
// finish_reason). "length" is a genuine truncation; every other reason —
// "stop", "content_filter", a "tool_calls"/"function_call" that produced no
// tool call, and unknowns — is a completed turn. The brain treats all of these
// (max_tokens included) as a completed turn in v1, so the distinction is only
// preserved for telemetry.
func mapFinishReason(finish string) string {
	if finish == "length" {
		return "max_tokens"
	}
	return "end_turn"
}
