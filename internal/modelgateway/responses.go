package modelgateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/catalog"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/convert"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/upstream"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// responses serves POST /v1/responses, OpenAI's Responses API without its
// state: the request converts to a Messages one (convert.ResponsesRequest),
// which is served as /v1/messages is — routing, retries, thinking
// provenance, a conversion to Chat Completions for an OpenAI-only
// credential, usage and limits alike — and the answer converts back
// (convert.ResponsesAnswer, convert.ResponsesStream), errors in OpenAI's
// envelope. The ledger records it under its own endpoint, on the OpenAI
// protocol. Nothing is stored, so a stored response's routes are refused
// (ServeHTTP), as is a request naming one.
func (h *handler) responses(w http.ResponseWriter, r *http.Request, c caller) {
	start := time.Now()
	top, ok := readRequest(w, r)
	if !ok {
		return
	}
	conv, err := convert.ResponsesRequest(top)
	if err != nil {
		writeError(w, r, invalid("%s", err))
		return
	}
	meta := &convert.ResponseMeta{ID: "resp_" + strings.TrimPrefix(requestID(r), "req_"), CreatedAt: start.Unix(), Echo: conv.Echo}
	h.serve(w, r, c, "/v1/messages", "/v1/responses", conv.Messages, meta, start)
}

// defaultMaxTokens is the max_tokens a Responses request that sets no
// max_output_tokens is sent with, as Messages requires one: the alias's
// limit as /v1/models answers it (describe), or, where a target records
// none, the 8,192 the brain's anthropic adapter sends
// (internal/provider/anthropic).
func defaultMaxTokens(snap *catalog.Snapshot, a store.Alias) int64 {
	if n := describe(snap, a).MaxTokens; n > 0 {
		return n
	}
	return 8192
}

// responsesFields are the Messages fields a refusal of a converted request
// may name, and the Responses fields they came from, the longest first.
var responsesFields = []struct{ from, to string }{
	{"tool_choice.disable_parallel_tool_use", "parallel_tool_calls"},
	{"tool_choice.type", "tool_choice"},
	{"output_config.format", "text.format"},
	{"output_config.effort", "reasoning.effort"},
	{"output_config", "reasoning"},
	{"thinking.type", "reasoning.effort"},
	{"thinking", "reasoning"},
	{"max_tokens", "max_output_tokens"},
	{"system", "instructions"},
}

// responsesField is a refusal of the Messages request a Responses request
// became, by the field it starts with, renamed to the field the caller
// wrote: a message's path is input, whose items do not map one to one, and
// a tool's input_schema its parameters.
func responsesField(s string) string {
	field, rest, found := strings.Cut(s, ":")
	switch {
	case strings.HasPrefix(field, "messages"):
		field = "input"
	case strings.HasPrefix(field, "tools["):
		field = strings.Replace(field, ".input_schema", ".parameters", 1)
	default:
		for _, m := range responsesFields {
			if field == m.from || strings.HasPrefix(field, m.from+".") {
				field = m.to
				break
			}
		}
	}
	if !found {
		return field
	}
	return field + ":" + rest
}

// openAIError is an upstream's error body, which the gateway relays in
// Anthropic's envelope on the Messages path (errorJSON, convertedError), in
// OpenAI's: its type and message where it is in either envelope, with the
// param and code OpenAI's carries, and the whole body as the message, of
// type api_error, where it is in neither. The body was redacted before it
// came here.
func openAIError(b []byte) []byte {
	typ, msg := "api_error", string(b)
	var param, code any
	var env struct {
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Param   any    `json:"param"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &env) == nil && env.Error != nil {
		msg, param, code = env.Error.Message, env.Error.Param, env.Error.Code
		if env.Error.Type != "" {
			typ = env.Error.Type
		}
	}
	return encodeJSON(map[string]any{"error": map[string]any{"message": msg, "type": typ, "param": param, "code": code}})
}

// convertedOpenAIError is an OpenAI-protocol upstream's error on the
// conversion path as a Responses caller is given it, where a Messages
// caller is given convertedError's: in OpenAI's envelope, the upstream's own
// type, message, param and code, each redacted — the type too, which is any
// string the upstream sent, where convertedError's is one of a closed set —
// and a type it states none of being the one convertedError gives the
// status. A body in Anthropic's envelope, as MiniMax answers on its OpenAI
// endpoint, reads the same way: its error member holds the type and
// message, and no param or code.
func convertedOpenAIError(red provider.Redactor, b []byte, status int) []byte {
	var e struct {
		Type  string `json:"type"`
		Param any    `json:"param"`
		Code  any    `json:"code"`
	}
	_ = json.Unmarshal(member(b, "error"), &e)
	if e.Type == "" {
		e.Type = messagesErrorType("", status)
	}
	return encodeJSON(map[string]any{"error": map[string]any{"message": red.String(errorMessage(b)), "type": red.String(e.Type),
		"param": redactValue(red, e.Param), "code": redactValue(red, e.Code)}})
}

// responsesStream relays a stream to a Responses caller: inner, the
// protocol of the Messages stream the request was served as — passed
// through or converted from Chat Completions — writes Messages events, and
// each becomes the Responses events it converts to. Keep-alives are SSE
// comments, which every SSE reader skips: a Messages ping has no Responses
// counterpart. A Messages event the conversion cannot carry ends the stream
// as response.failed, the upstream read on for the usage it reports, as a
// Chat Completions chunk convStream cannot carry does.
type responsesStream struct {
	inner  streamProto
	s      *convert.ResponsesStream
	c      call
	red    provider.Redactor // for a conversion failure's message, which names what the upstream sent
	failed bool
}

// event is inner's events converted. Once the conversion has failed, inner
// reads on for the usage, but the error the ledger records stays the one the
// caller was given, whatever inner makes of an error after it.
func (p *responsesStream) event(e upstream.Event) ([]byte, bool) {
	if p.failed {
		typ := p.c.out.errType
		_, last := p.inner.event(e)
		p.c.out.errType = typ
		return nil, last
	}
	out, last := p.inner.event(e)
	return p.convert(out), last
}

// convert is b, the inner protocol's events, as Responses events. An
// event it cannot read — one the inner protocol's rewriting grew past the
// bound on one event — fails the stream, as one it cannot convert does.
func (p *responsesStream) convert(b []byte) []byte {
	var out bytes.Buffer
	events := upstream.NewReader(bytes.NewReader(b))
	for !p.failed {
		e, err := events.Next()
		if err != nil && !errors.Is(err, io.EOF) {
			p.failed, p.c.out.errType = true, "api_error"
			out.Write(p.s.Failure("api_error", fmt.Sprintf("upstream stream could not be converted: %s", err)))
			break
		}
		if e.Name != "" {
			b, cerr := p.s.Event(e.Name, e.Data)
			if cerr != nil {
				p.failed, p.c.out.errType = true, "api_error"
				b = p.s.Failure("api_error", fmt.Sprintf("upstream stream could not be converted: %s", p.red.Error(cerr)))
			}
			out.Write(b)
		}
		if err != nil {
			break
		}
	}
	if out.Len() == 0 {
		return nil
	}
	return out.Bytes()
}

func (p *responsesStream) keepAlive(e upstream.Event) bool { return p.inner.keepAlive(e) }

func (p *responsesStream) complete(data []byte) bool { return p.inner.complete(data) }

// ended is "" once the conversion failed: the caller was given that error.
func (p *responsesStream) ended() string {
	if p.failed {
		return ""
	}
	return p.inner.ended()
}

func (p *responsesStream) closing() []byte { return p.convert(p.inner.closing()) }

func (p *responsesStream) idle() []byte { return []byte(": ping\n\n") }

func (p *responsesStream) draining() bool { return p.failed || p.inner.draining() }

func (p *responsesStream) failure(typ, msg string) []byte { return p.s.Failure(typ, msg) }
