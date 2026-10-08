// Package openai adapts an OpenAI Chat Completions endpoint (OpenAI itself, a
// vLLM server, or an internal OpenAI-compatible gateway) to the provider
// interface. Unlike the anthropic adapter, which is near-verbatim, this is the
// platform's lossy seam: the internal Request is Anthropic-native, so every turn
// is translated to OpenAI wire on the way out and back on the way in. The way
// out is internal/modelgateway/convert's (Messages and Tools), which the model
// gateway's conversion path shares, and whose comments name what it drops and
// refuses — a signed thinking block and a tool_result's is_error dropped, a
// built-in tool's schema keywords stripped, an unsupported block failing
// loudly rather than vanishing. The way in is this package's, tested against a
// fake Chat Completions server: the deprecated single-function_call streaming
// format is rejected loudly (the endpoint must emit tool_calls) rather than
// silently losing the call, and reasoning_content is not read.
//
// base_url is the API root, the same convention as the anthropic provider: the
// adapter appends /v1/chat/completions. Set it to e.g. https://api.openai.com or
// https://vllm.internal, NOT .../v1.
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
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/convert"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// quotedBodyLimit bounds how much of a failing response body an error quotes.
const quotedBodyLimit = 4096

// drainTailLimit bounds the read-to-EOF a completed stream does on Close so the
// connection can be pooled. A conforming endpoint sends a newline or two after
// `data: [DONE]`; this leaves room for a stray keepalive and stops well short of
// a body that never ends.
const drainTailLimit = 4096

// New constructs the adapter from configuration alone.
func New(cfg provider.Config) (provider.Provider, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("openai provider requires a base_url")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("openai provider requires a model")
	}
	return &openaiProvider{
		endpoint:  strings.TrimRight(cfg.BaseURL, "/") + "/v1/chat/completions",
		apiKey:    cfg.APIKey,
		model:     cfg.Model,
		headers:   cfg.Headers,
		client:    http.DefaultClient,
		stall:     cfg.StallTimeout,
		maxTokens: cfg.MaxTokens,
		redact:    provider.NewRedactor(cfg),
	}, nil
}

type openaiProvider struct {
	endpoint string
	apiKey   string
	model    string
	headers  map[string]string
	client   *http.Client
	// stall is how long this endpoint may say nothing before the turn is
	// abandoned; zero takes provider.DefaultStallTimeout. It bounds the request
	// rather than the client because http.DefaultClient is shared with every
	// other provider instance — see provider.StallGuard and Registry.
	stall time.Duration
	// maxTokens is the route's default output cap for turns that set none;
	// zero keeps the field off the wire so the endpoint's default applies.
	maxTokens int64
	redact    provider.Redactor
}

func (p *openaiProvider) Generate(ctx context.Context, req provider.Request) (provider.Stream, error) {
	turns := make([]convert.Message, len(req.Messages))
	for i, m := range req.Messages {
		turns[i] = convert.Message{Role: m.Role, Content: m.Content}
	}
	messages, err := convert.Messages(req.System, turns)
	if err != nil {
		return nil, err
	}
	tools, err := convert.Tools(req.Tools, req.BuiltinTools)
	if err != nil {
		return nil, err
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = p.maxTokens
	}
	body := chatRequest{
		ReasoningEffort: string(req.Effort),
		Model:           p.model,
		Messages:        messages,
		MaxTokens:       maxTokens,
		Stream:          true,
		StreamOptions:   &streamOptions{IncludeUsage: true},
		Tools:           tools,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	// The guard bounds the whole call: the wait for response headers, which
	// nothing else covers, and the body reads below, which no header timeout
	// would reach.
	gctx, guard := provider.NewStallGuard(ctx, p.stall)
	httpReq, err := http.NewRequestWithContext(gctx, http.MethodPost, p.endpoint, bytes.NewReader(raw))
	if err != nil {
		guard.Stop()
		// A URL parse failure quotes the endpoint back verbatim, and base_url
		// may carry a credential; net/http strips one only from an error it
		// builds itself. The redactor finds an unparsable base_url's password
		// textually for exactly this case.
		return nil, p.redact.Error(err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	for k, v := range p.headers {
		httpReq.Header.Set(k, v)
	}
	for k, v := range provider.CallHeaders(ctx, req) {
		httpReq.Header.Set(k, v)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		err = guard.Cause(p.redact.Error(err))
		guard.Stop()
		return nil, err
	}
	// Every byte the endpoint delivers, keepalive comments included, is a sign
	// of life for the guard — the liveness a frame-level signal would miss,
	// since a comment is not an event. Wrapped before the status is examined so
	// an error body gets the same budget a stream body does: the response's own
	// arrival counts as progress, and each byte read buys another budget. The
	// anthropic adapter's middleware wraps every response the same way; only
	// this hand-rolled path could tell a 200 from a 500 and treat them
	// differently.
	respBody := provider.ProgressBody(gctx, resp.Body)
	if resp.StatusCode != http.StatusOK {
		defer guard.Stop()
		defer respBody.Close()
		// The body is quoted because the status alone rarely explains a gateway
		// misconfiguration — but an endpoint that echoes the request's
		// Authorization header into it must not get the credential into an
		// error the platform persists as a session.error event.
		//
		// The read overshoots the quote budget by the longest secret so that a
		// credential straddling it is still matched whole; the redacted text is
		// then cut back. Reading exactly the budget would leave the head of a
		// key in the message, matching nothing.
		msg, readErr := io.ReadAll(io.LimitReader(respBody, quotedBodyLimit+int64(p.redact.Longest())))
		// A body that simply exceeds the budget is not an error — LimitReader
		// ends it with EOF, and the overshoot above is what keeps a straddling
		// credential matchable. A real read failure is different: the bytes stop
		// wherever the failure fell, which may be the middle of an echoed
		// credential, and redaction matches whole secrets only — so a key cut in
		// half survives it, which this package's own tests define as a leak. An
		// endpoint that stalls mid-error-body reaches here now that the guard
		// cancels the read instead of letting it hang, so the truncated text is
		// dropped rather than quoted.
		if readErr != nil {
			return nil, fmt.Errorf("openai endpoint returned %s, and its error body could not be read: %w",
				p.redact.String(resp.Status), p.redact.Error(readErr))
		}
		quoted := p.redact.String(strings.TrimSpace(string(msg)))
		if len(quoted) > quotedBodyLimit {
			quoted = quoted[:quotedBodyLimit]
		}
		// The status line is endpoint-controlled too: HTTP/1 lets a server put
		// any text after the code.
		//
		// Not passed through guard.Cause, deliberately: an endpoint that answers
		// a status and then stalls is still bounded — the guard's cancellation is
		// what ends the read above — but what it said is the better diagnostic,
		// and naming the stall instead would throw it away.
		return nil, fmt.Errorf("openai endpoint returned %s: %s", p.redact.String(resp.Status), quoted)
	}
	return &stream{body: respBody, r: bufio.NewReader(respBody), redact: p.redact, guard: guard}, nil
}

// --- outgoing request shapes (Anthropic-native -> OpenAI Chat Completions) ---

type chatRequest struct {
	// Preserve the level; the endpoint validates model-specific support.
	// https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create
	ReasoningEffort string                `json:"reasoning_effort,omitempty"`
	Model           string                `json:"model"`
	Messages        []convert.ChatMessage `json:"messages"`
	// max_tokens is the field vLLM and the OpenAI-compatible gateways this
	// adapter targets accept; only api.openai.com's newest reasoning models
	// have switched to max_completion_tokens. Omitted when zero so the endpoint
	// applies its own default.
	MaxTokens     int64              `json:"max_tokens,omitempty"`
	Stream        bool               `json:"stream"`
	StreamOptions *streamOptions     `json:"stream_options,omitempty"`
	Tools         []convert.ChatTool `json:"tools,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// --- incoming stream translation (OpenAI SSE -> provider chunks) ---

type stream struct {
	body   io.ReadCloser
	r      *bufio.Reader
	redact provider.Redactor
	guard  *provider.StallGuard

	pending []provider.Chunk
	cur     provider.Chunk
	err     error
	done    bool

	tools     map[int]*toolAccum
	toolOrder []int

	stopReason string
	usage      domain.ModelUsage
	sawUsage   bool // a usage object arrived; zeroes without one are not a reading
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
	// Only when the turn completed normally is there a tail to drain (the few
	// bytes after `data: [DONE]`) — draining it lets net/http pool the
	// keep-alive connection across a session's many turns. On an early or
	// errored close the body may still be open, and an unbounded drain would
	// block until the upstream EOFs; since the brain closes the stream in a
	// defer before releasing the turn's lease, that would wedge the session on a
	// hung endpoint. So skip the drain and close immediately in that case.
	//
	// The drain is bounded even so. "The few bytes after [DONE]" is the
	// endpoint's good behavior, not a guarantee: one that keeps writing
	// keepalives past its own terminator would hold this Copy forever, and the
	// stall guard cannot end it — the drain's own reads are what feed the guard
	// (#121). A limit is the bound that does not depend on the reader being
	// idle. Overrunning it costs one pooled connection, which is what an
	// endpoint behaving this way deserves.
	if s.completed {
		_, _ = io.Copy(io.Discard, io.LimitReader(s.r, drainTailLimit))
	}
	err := s.body.Close()
	// After the body, so a drain that still reads is not aborted by the very
	// cancellation that releases the guard.
	s.guard.Stop()
	return err
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
			// Endpoint-controlled: an HTTP/2 GOAWAY carries server debug data
			// into the body-read error. An endpoint that simply stopped talking
			// is named as that instead — the read reports a bare cancellation.
			s.err = s.guard.Cause(s.redact.Error(err))
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
				s.err = s.guard.Cause(fmt.Errorf("openai stream ended before completion (no finish_reason or [DONE])"))
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
	var usage *domain.ModelUsage
	if s.sawUsage {
		u := s.usage
		usage = &u
	}
	s.pending = append(s.pending, provider.Chunk{Kind: provider.KindDone, StopReason: stop, Usage: usage})
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
		// Endpoint-supplied text under HTTP 200: the same credential echo as
		// the non-200 path, on the route an operator is least likely to test.
		return fmt.Errorf("openai stream error: %s", s.redact.String(fr.Error.Message))
	}
	if fr.Usage != nil {
		// The endpoint answered. A server that ignores include_usage never
		// gets here, and its done chunk carries no usage at all (#90).
		s.sawUsage = true
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
