package modelgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/catalog"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/convert"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/upstream"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// The conversion path (docs/plan/59_model-gateway.md, "Two request paths"):
// a Messages request whose attempt reaches its provider on the OpenAI
// protocol (catalog.Plan) goes to the vendor's /chat/completions as the
// Chat Completions request convert.Request makes of it — its messages
// filtered by thinking provenance first, as a passthrough attempt's are —
// and the answer comes back as a Messages one, whole (convert.Answer) or
// streamed (convStream). Routing, retries, limits, telemetry and the ledger
// are the passthrough's; the ledger row's protocol is the caller's.

// passingThrough is the attempts on proto itself, which convert nothing.
func passingThrough(attempts []catalog.Attempt, proto profile.Protocol) []catalog.Attempt {
	var out []catalog.Attempt
	for _, at := range attempts {
		if at.Protocol == proto {
			out = append(out, at)
		}
	}
	return out
}

// convertedBody is the Chat Completions request one deployment is sent: the
// caller's body with its messages as provenance filters them for the
// deployment, converted for its upstream model in the profile's words for
// thinking. inference converted the request once already, so this cannot
// fail but on a request it refused there.
func convertedBody(c call, d store.Deployment, prof profile.Profile, strip bool) ([]byte, error) {
	top := c.top
	if c.hist != nil {
		top = make(map[string]json.RawMessage, len(c.top))
		for k, v := range c.top {
			top[k] = v
		}
		top["messages"] = c.hist.messagesFor(d.ID, strip, true)
	}
	return convert.Request(top, d.UpstreamModel, prof.ChatThinking)
}

// signer signs each thinking block a converted answer carries: the
// deployment's provenance wrapper around an empty upstream signature, so the
// block goes back to the deployment that produced it, and no other, as the
// reasoning_content a Chat Completions vendor expects back (convert).
func signer(wrap *wrapping) func() string {
	return func() string { return wrap.wrap("") }
}

// callerUsage is the ledger's reading of an upstream's usage as a converted
// answer reports it, nil for none.
func callerUsage(t *store.Tokens) *convert.Usage {
	if t == nil {
		return nil
	}
	return &convert.Usage{Input: t.Input, Output: t.Output, CacheRead: t.CacheRead}
}

// convertedError is an upstream's error on the conversion path, a body or a
// stream chunk's data, as a Messages caller reads it: Anthropic's envelope,
// carrying the gateway's request_id, with the call's credentials removed. An
// error already in that envelope — MiniMax answers in it even on its OpenAI
// endpoint — is errorJSON's; any other keeps its message, its type where it
// is one of Anthropic's, and otherwise the type Anthropic gives status.
func convertedError(ctx context.Context, red provider.Redactor, b []byte, status int, rid string) []byte {
	if m, ok := decodeJSON(b).(map[string]any); ok && m["type"] == "error" {
		return errorJSON(ctx, red, b, rid)
	}
	var typ string
	_ = json.Unmarshal(member(b, "error", "type"), &typ)
	return encodeJSON(map[string]any{"type": "error", "request_id": rid, "error": map[string]string{
		"type": messagesErrorType(typ, status), "message": red.String(errorMessage(b)),
	}})
}

// messagesErrorType is typ when it is one of Anthropic's error types, and
// otherwise the one Anthropic gives status (platform.claude.com/docs/en/api/
// errors), any other 4xx — a 422 among them — being invalid_request_error.
func messagesErrorType(typ string, status int) string {
	switch typ {
	case "authentication_error", "permission_error", "billing_error":
		return typ
	}
	switch status { // which only an error after the answer began states: refusedCredential takes the others
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusPaymentRequired:
		return "billing_error"
	case http.StatusForbidden:
		return "permission_error"
	}
	if _, ok := errorStatus[typ]; ok {
		return typ
	}
	for t, s := range errorStatus {
		if s == status {
			return t
		}
	}
	if status >= 400 && status < 500 {
		return "invalid_request_error"
	}
	return "api_error"
}

// convStream relays a Chat Completions stream to a Messages caller, each
// chunk converted (convert.Stream), the usage a chunk reports going to c.out
// and to message_delta. It reads the stream as chatStream does: a chunk
// carrying error, or an event named error, ends it, as the Messages error
// event convertedError makes of it; data that is no JSON object ends it with
// the gateway's own; a chunk convert cannot carry ends it for the caller with
// the gateway's own too, and the upstream, which is still generating and
// charging, is read on to its end for the usage it reports after its finish
// (draining); [DONE] ends it with
// the message's last events, and so does the upstream closing it once the
// choice has finished, which MiniMax-M3 does without [DONE]. Its keep-alives
// are Chat Completions', and the caller is sent Messages pings in their
// place, and whenever the relay has written nothing for pingEvery.
type convStream struct {
	chatFraming
	c      call
	s      *convert.Stream
	ctx    context.Context
	red    provider.Redactor
	rid    string
	failed bool // a chunk could not be converted, and the upstream is read on for its usage alone
}

func (p *convStream) event(e upstream.Event) ([]byte, bool) {
	named := e.Name == "error"
	if p.failed {
		var obj map[string]json.RawMessage
		switch {
		case e.Data == nil && !named:
			return nil, false
		case named || isDone(e.Data) || json.Unmarshal(e.Data, &obj) != nil:
			return nil, true
		}
		if _, ok := obj["error"]; ok { // the upstream has ended its answer
			return nil, true
		}
		if t := chatUsageOf(obj["usage"]); t != nil {
			p.c.out.tokens = t
		}
		return nil, false
	}
	switch {
	case e.Data == nil && !named:
		return convert.Ping(), false
	case isDone(e.Data) && !named:
		return p.s.End(), true
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(e.Data, &obj) != nil || obj == nil {
		p.c.out.errType = "api_error"
		return p.failure("api_error", "upstream sent an event that is not a JSON object"), true
	}
	if _, ok := obj["error"]; ok || named {
		b := convertedError(p.ctx, p.red, e.Data, statedStatus(e.Data), p.rid)
		p.c.out.errType = errorTypeOf(b, 0)
		return []byte("event: error\ndata: " + string(b) + "\n\n"), true
	}
	if t := chatUsageOf(obj["usage"]); t != nil {
		p.c.out.tokens = t
		p.s.SetUsage(*callerUsage(t))
	}
	out, err := p.s.Chunk(e.Data)
	if err != nil {
		p.c.out.errType, p.failed = "api_error", true
		return p.failure("api_error", fmt.Sprintf("upstream stream could not be converted: %s", err)), false
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, false
}

// ended is "" once a chunk could not be converted: the caller was given that
// error, which a drain that then breaks off leaves the ledger's.
func (p *convStream) ended() string {
	if p.failed || p.s.Finished() {
		return ""
	}
	return "the stream ended before its finish"
}

func (p *convStream) failure(typ, msg string) []byte { return messagesError(p.rid, typ, msg) }

func (p *convStream) closing() []byte { return p.s.End() }

func (p *convStream) draining() bool { return p.failed }

func (p *convStream) idle() []byte { return convert.Ping() }
