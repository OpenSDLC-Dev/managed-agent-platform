package modelgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/catalog"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/upstream"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// The body bounds: a request as large as the Messages API takes, and an
// answer well past any model's output.
const maxRequestBody = 32 << 20

var maxResponseBody = 64 << 20

// SessionHeader carries the caller's session id, which keeps a session's
// requests on one upstream (catalog.Snapshot.Plan). It never goes upstream.
const SessionHeader = "X-MAP-Session-ID"

// defaultAnthropicVersion is sent when a caller sends none; every Anthropic
// SDK sends one.
const defaultAnthropicVersion = "2023-06-01"

// writeStall bounds each write to the caller. A caller that stops reading but
// keeps its connection open would otherwise hold the handler for as long as it
// liked; past the bound the caller is treated as gone, and an upstream stream
// is still read to its end.
var writeStall = time.Minute

// maxHeld bounds the keep-alives held back before a stream's answer begins:
// past it the stream is committed to, so an upstream that pings and never
// answers costs the gateway no more than this.
const maxHeld = 64 << 10

// bounded sets the write bound for what follows. It is not lifted when the
// handler returns: the response's last bytes — all of a small one, which
// net/http buffers to give it a Content-Length — are written after that, and
// net/http clears the bound itself once they are, before the connection's next
// request (server.go's serve loop, after finishRequest).
func bounded(w http.ResponseWriter) *http.ResponseController {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(writeStall))
	return rc
}

// messages serves /v1/messages and its count_tokens twin.
func (h *handler) messages(w http.ResponseWriter, r *http.Request, c caller, path string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, r, &apiError{http.StatusRequestEntityTooLarge, "request_too_large", "Request exceeds the maximum allowed number of bytes."})
			return
		}
		writeError(w, r, invalid("Failed to read request body: %s", err))
		return
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		writeError(w, r, invalid("Failed to parse request body as JSON"))
		return
	}
	var model string
	if err := json.Unmarshal(top["model"], &model); err != nil || model == "" {
		writeError(w, r, invalid("model: Field required"))
		return
	}
	snap := h.cfg.Catalog.Snapshot()
	a, ok := snap.Alias(model)
	switch {
	case !ok:
		writeError(w, r, notFound("model: %s", model))
		return
	case !c.may(a.Name):
		writeError(w, r, forbidden("this API key may not use model %s", model))
		return
	case a.Kind != store.KindChat:
		writeError(w, r, invalid("model: %s serves %s, not chat", model, a.Kind))
		return
	}
	attempts := snap.Plan(a, profile.Anthropic, r.Header.Get(SessionHeader), h.draw)
	if len(attempts) == 0 {
		writeError(w, r, &apiError{http.StatusServiceUnavailable, "api_error",
			fmt.Sprintf("model %s has no enabled upstream on the Anthropic protocol", model)})
		return
	}
	call := call{
		top:    top,
		alias:  model,
		path:   path,
		stream: path == "/v1/messages" && string(bytes.TrimSpace(top["stream"])) == "true",
		header: forwarded(r.Header),
	}
	// MaxAttempts bounds the calls to each deployment rather than to the
	// alias, so a deployment with many credentials cannot spend the budget a
	// fallback deployment needed. A caller that leaves ends the retries, and
	// is still written the last failure: net/http also reads a caller that
	// only half-closed its connection as gone, and that caller is reading.
	var last *failure
	tried := map[string]int{}
	n := 0
	for _, at := range attempts {
		if tried[at.Deployment.ID] == h.cfg.MaxAttempts {
			continue
		}
		tried[at.Deployment.ID]++
		if n > 0 && !h.backoff(r.Context(), n) {
			break
		}
		n++
		f, retry := h.attempt(w, r, call, at)
		if f == nil {
			return
		}
		if !retry {
			f.write(w, r)
			return
		}
		slog.InfoContext(r.Context(), "modelgateway: attempt failed", "alias", a.Name,
			"deployment", at.Deployment.ID, "credential", at.Credential.ID, "status", f.status, "error", f.err)
		last = f
	}
	last.write(w, r)
}

// call is what every attempt of one request sends.
type call struct {
	top    map[string]json.RawMessage
	alias  string // the model name the caller sent, which the answer carries back
	path   string
	stream bool
	header http.Header // the caller's headers that go upstream
}

// forwarded keeps the caller's anthropic-* headers, verbatim and as an open
// list, and nothing else: its credential, the X-MAP-* headers and anything a
// client sets for its own transport stay at the gateway.
func forwarded(in http.Header) http.Header {
	out := http.Header{}
	for k, vs := range in {
		if strings.HasPrefix(strings.ToLower(k), "anthropic-") {
			out[k] = append([]string(nil), vs...)
		}
	}
	if out.Get("anthropic-version") == "" {
		out.Set("anthropic-version", defaultAnthropicVersion)
	}
	return out
}

// failure is an attempt that did not answer the caller: an upstream's error
// response, or no response at all.
type failure struct {
	status int
	header http.Header // an upstream's retry-after and content type
	body   []byte      // an upstream's body, its credential removed
	typ    string      // for a failure with no upstream body
	err    error
}

func (f *failure) write(w http.ResponseWriter, r *http.Request) {
	if f.body == nil {
		writeError(w, r, &apiError{f.status, f.typ, f.err.Error()})
		return
	}
	for _, k := range []string{"Content-Type", "Retry-After", "X-Should-Retry"} {
		if v := f.header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	bounded(w)
	w.WriteHeader(f.status)
	_, _ = w.Write(f.body)
}

// retryable reports whether another attempt may cure an upstream's error, by
// the rule Anthropic's SDK applies to its own retries (checked against
// anthropic-sdk-go v1.70.1 — internal/requestconfig/requestconfig.go
// shouldRetry): the upstream's x-should-retry when it sends one, and
// otherwise a timeout, a conflict, a rate limit, an overload or a server
// error.
func retryable(status int, h http.Header) bool {
	switch h.Get("X-Should-Retry") {
	case "true":
		return true
	case "false":
		return false
	}
	return status == http.StatusRequestTimeout || status == http.StatusConflict ||
		status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

// refusedCredential is a vendor refusing the gateway's own account: a revoked
// key, one without access to the model, or an empty balance (DeepSeek's 402).
// It is not the caller's to read as its own authentication failing, and
// another credential may serve, so it is a retryable 502 whose detail goes to
// the log.
// The refusal is named by its HTTP status or, in a stream, its error type.
func refusedCredential(ctx context.Context, at catalog.Attempt, refusal string, detail []byte, red provider.Redactor) (*failure, bool) {
	slog.WarnContext(ctx, "modelgateway: upstream refused a credential", "credential", at.Credential.ID,
		"deployment", at.Deployment.ID, "refusal", refusal, "detail", red.String(string(detail)))
	return &failure{status: http.StatusBadGateway, typ: "api_error",
		err: fmt.Errorf("upstream refused the gateway's credential (%s)", refusal)}, true
}

// errorStatus is the HTTP status an Anthropic error type answers with
// (platform.claude.com/docs/en/api/errors, read 2026-10-05), for a stream that
// opens with an error event (streamError); the account refusals are
// refusedCredential's, and an unknown type is a server error.
var errorStatus = map[string]int{
	"invalid_request_error": http.StatusBadRequest,
	"not_found_error":       http.StatusNotFound,
	"conflict_error":        http.StatusConflict,
	"request_too_large":     http.StatusRequestEntityTooLarge,
	"rate_limit_error":      http.StatusTooManyRequests,
	"api_error":             http.StatusInternalServerError,
	"timeout_error":         http.StatusGatewayTimeout,
	"overloaded_error":      529,
}

// streamError is a stream whose first event is an error: the upstream refused
// before it began an answer, so this is the error response it would have sent
// had its 200 not already gone, retried and relayed like one — its
// x-should-retry and Retry-After included.
func streamError(ctx context.Context, at catalog.Attempt, data []byte, header http.Header, rid string, red provider.Redactor) (*failure, bool) {
	var ev struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &ev)
	switch ev.Error.Type {
	case "authentication_error", "permission_error", "billing_error":
		return refusedCredential(ctx, at, ev.Error.Type, data, red)
	}
	status, ok := errorStatus[ev.Error.Type]
	if !ok {
		status = http.StatusInternalServerError
	}
	h := header.Clone()
	h.Set("Content-Type", "application/json")
	return &failure{status: status, header: h, body: errorJSON(ctx, red, data, rid)}, retryable(status, header)
}

// attempt makes one upstream call. It answers the caller and returns nil, or
// returns the failure and whether another attempt may cure it. Nothing is
// written to the caller until the upstream's answer has begun — a whole body,
// or a stream's first event that is not an error — so every failure it
// returns leaves the caller's response untouched.
//
// The call runs under a context the caller's cancellation does not reach,
// bounded by the provider's stall guard instead: a caller that disconnects
// does not end an upstream answer it has started paying for, which is read to
// its end.
func (h *handler) attempt(w http.ResponseWriter, r *http.Request, c call, at catalog.Attempt) (*failure, bool) {
	ctx := context.WithoutCancel(r.Context())
	key, err := h.open(ctx, at.Credential)
	if err != nil {
		slog.ErrorContext(ctx, "modelgateway: credential could not be opened", "credential", at.Credential.ID, "error", err)
		return &failure{status: http.StatusInternalServerError, typ: "api_error", err: errors.New("internal error")}, true
	}
	red := provider.NewRedactor(provider.Config{APIKey: string(key), Headers: at.Provider.Headers})
	body := upstreamBody(c.top, at.Deployment.UpstreamModel)
	ctx, guard := provider.NewStallGuard(ctx, at.Provider.StallTimeout)
	defer guard.Stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, at.Endpoint+c.path, bytes.NewReader(body))
	if err != nil {
		return &failure{status: http.StatusBadGateway, typ: "api_error", err: red.Error(err)}, true
	}
	for k, vs := range c.header {
		req.Header[k] = vs
	}
	for k, v := range at.Provider.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", string(key))

	resp, err := h.client.Do(req)
	if err != nil {
		return noAnswer(guard, red, err)
	}
	resp.Body = provider.ProgressBody(ctx, resp.Body)
	defer resp.Body.Close()
	switch s := resp.StatusCode; {
	case s >= 300 && s < 400:
		return &failure{status: http.StatusBadGateway, typ: "api_error",
			err: fmt.Errorf("upstream answered a redirect (%d), which the gateway does not follow", s)}, true
	case s == http.StatusUnauthorized, s == http.StatusPaymentRequired, s == http.StatusForbidden:
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return refusedCredential(ctx, at, fmt.Sprintf("HTTP %d", s), detail, red)
	case s >= 400:
		// Whether another attempt may cure it is the status's to say, whatever
		// becomes of the body: a refusal whose body broke off is a refusal.
		b, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxResponseBody)+1))
		switch {
		case err != nil:
			return &failure{status: s, typ: "api_error",
				err: fmt.Errorf("upstream answered %d and its body broke off: %w", s, red.Error(guard.Cause(err)))}, retryable(s, resp.Header)
		case len(b) > maxResponseBody:
			return &failure{status: s, typ: "api_error",
				err: fmt.Errorf("upstream answered %d with an error body over the gateway's bound of %d bytes", s, maxResponseBody)}, retryable(s, resp.Header)
		}
		return &failure{status: s, header: resp.Header, body: errorJSON(ctx, red, b, requestID(r))}, retryable(s, resp.Header)
	}
	if c.stream && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		// The answer begins with the first event that is not a keep-alive (a
		// comment or a ping); until then the caller has seen nothing and the
		// upstream may yet refuse, as an error event, which is answered like
		// any refusal. Keep-alives past maxHeld begin it anyway.
		events := upstream.NewReader(resp.Body)
		var held []byte
		for {
			e, err := events.Next()
			keepAlive := e.Name == "ping" || e.Name == "" && e.Data == nil
			switch {
			case err != nil:
				return noAnswer(guard, red, err)
			case keepAlive && len(held)+len(e.Raw) <= maxHeld:
				held = append(held, e.Raw...)
			case e.Name == "error":
				return streamError(ctx, at, e.Data, resp.Header, requestID(r), red)
			default:
				relayStream(ctx, w, events, held, e, c.alias, requestID(r), guard, red)
				return nil, false
			}
		}
	}
	// Once the answer's body has begun the upstream is generating — and
	// charging — for it, so a break past that point is the caller's 502, not
	// another paid attempt.
	b, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxResponseBody)+1))
	switch {
	case err != nil:
		f, retry := noAnswer(guard, red, err)
		return f, retry && len(b) == 0
	case len(b) > maxResponseBody:
		return &failure{status: http.StatusBadGateway, typ: "api_error",
			err: fmt.Errorf("upstream answer exceeds the gateway's bound of %d bytes", maxResponseBody)}, false
	}
	w.Header().Set("Content-Type", "application/json")
	bounded(w)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(withModel(b, c.alias))
	return nil, false
}

// noAnswer is an attempt whose upstream gave no usable answer: a stall, or a
// transport error. A transport error may be retried; a stall may not, since
// an upstream that has gone quiet may still be generating — and charging for
// — the answer it owes, as a whole answer is silent until it is done.
func noAnswer(guard *provider.StallGuard, red provider.Redactor, err error) (*failure, bool) {
	if err = guard.Cause(err); errors.Is(err, provider.ErrStalled) {
		return &failure{status: http.StatusGatewayTimeout, typ: "timeout_error", err: fmt.Errorf("upstream sent nothing: %w", err)}, false
	}
	return &failure{status: http.StatusBadGateway, typ: "api_error", err: fmt.Errorf("upstream request failed: %w", red.Error(err))}, true
}

// upstreamBody is the caller's body with model set to the deployment's
// upstream id; every other top-level value goes out as the caller sent it.
// The values were decoded from JSON, so encoding them again cannot fail.
func upstreamBody(top map[string]json.RawMessage, model string) []byte {
	out := make(map[string]json.RawMessage, len(top))
	for k, v := range top {
		out[k] = v
	}
	out["model"], _ = json.Marshal(model)
	b, _ := json.Marshal(out)
	return b
}

// redactJSON removes the call's credentials from an upstream's diagnostic. A
// JSON document is redacted value by value after decoding, so no escape its
// encoding chose (a quote, a slash written \/) can hide a secret from the
// match; anything else is redacted as text. A valid document decodes, and
// what it decoded to encodes again, so neither step can fail.
func redactJSON(red provider.Redactor, b []byte) []byte {
	if !json.Valid(b) {
		return []byte(red.String(string(b)))
	}
	return encodeJSON(redactValue(red, decodeJSON(b)))
}

// errorJSON is an upstream's error, a body or an event's data, as the gateway
// relays it: redacted, and, when it is Anthropic's error envelope, carrying
// the gateway's request_id — the id of the request-id header the caller
// reads, which Anthropic's errors page has the body's agree with. The
// upstream's own id goes to the log.
func errorJSON(ctx context.Context, red provider.Redactor, b []byte, rid string) []byte {
	m, ok := decodeJSON(b).(map[string]any)
	if !ok || m["type"] != "error" {
		return redactJSON(red, b)
	}
	if up, _ := m["request_id"].(string); up != "" {
		slog.InfoContext(ctx, "modelgateway: upstream error relayed under the gateway's request id",
			"request_id", rid, "upstream_request_id", red.String(up))
	}
	m = redactValue(red, m).(map[string]any)
	m["request_id"] = rid
	return encodeJSON(m)
}

// decodeJSON decodes JSON keeping numbers as written; invalid JSON is nil.
func decodeJSON(b []byte) any {
	var v any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	_ = dec.Decode(&v)
	return v
}

func encodeJSON(v any) []byte {
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimSuffix(out.Bytes(), []byte("\n"))
}

func redactValue(red provider.Redactor, v any) any {
	switch t := v.(type) {
	case string:
		return red.String(t)
	case []any:
		for i := range t {
			t[i] = redactValue(red, t[i])
		}
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[red.String(k)] = redactValue(red, x)
		}
		return out
	}
	return v
}

// withModel sets a JSON object's model to the name the caller sent. Anything
// that is not an object with a model field passes unchanged.
func withModel(b []byte, alias string) []byte {
	var obj map[string]json.RawMessage
	if json.Unmarshal(b, &obj) != nil {
		return b
	}
	if _, ok := obj["model"]; !ok {
		return b
	}
	obj["model"], _ = json.Marshal(alias)
	out, _ := json.Marshal(obj)
	return out
}

// relayStream passes an upstream's events to the caller as each arrives —
// the keep-alives held before the first, then the first, then the rest —
// rewriting only message_start's message.model and an upstream error event
// (errorJSON). Comments and pings pass unchanged, and each event goes out
// whole: one the upstream cut off at the end of its stream is completed when
// its data parses, so the caller dispatches it and nothing written after it
// merges in, and dropped when its data does not, being unfinished. When the
// caller stops reading — or reads too slowly to take an event within
// writeStall — the stream is still read. When the upstream fails partway, or
// ends before message_stop, the caller gets an error event, the only way left
// to say so, and an unfinished event before it is dropped rather than merged
// into it. Once message_stop or the upstream's own error event has passed, the
// stream has said all it will and the relay ends: an upstream that holds its
// connection open after that holds nothing of the gateway's.
func relayStream(ctx context.Context, w http.ResponseWriter, events *upstream.Reader, held []byte, first upstream.Event, alias, rid string, guard *provider.StallGuard, red provider.Redactor) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	rc := bounded(w)
	w.WriteHeader(http.StatusOK)
	gone, done := false, false
	send := func(b []byte) {
		if gone {
			return
		}
		_ = rc.SetWriteDeadline(time.Now().Add(writeStall))
		if _, err := w.Write(b); err != nil {
			gone = true
			return
		}
		if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
			gone = true
		}
	}
	relay := func(e upstream.Event) {
		switch {
		case e.Name == "message_start" && e.Data != nil:
			send(e.WithData(messageStartWithModel(e.Data, alias)))
		case e.Name == "error" && e.Data != nil:
			send(e.WithData(errorJSON(ctx, red, e.Data, rid)))
		default:
			send(e.Raw)
		}
		done = done || e.Name == "message_stop" || e.Name == "error"
	}
	if len(held) > 0 {
		send(held)
	}
	relay(first)
	for !done {
		e, err := events.Next()
		torn := errors.Is(err, io.EOF) && e.Data != nil && !json.Valid(e.Data)
		if len(e.Raw) > 0 && (err == nil || errors.Is(err, io.EOF)) && !torn {
			if err != nil {
				e.Raw = terminated(e.Raw)
			}
			relay(e)
		}
		if err == nil || done {
			continue
		}
		if errors.Is(err, io.EOF) {
			err = errors.New("the stream ended before message_stop")
		} else {
			err = guard.Cause(err)
		}
		typ := "api_error"
		if errors.Is(err, provider.ErrStalled) {
			typ = "timeout_error"
		}
		msg, _ := json.Marshal(map[string]any{"type": "error", "request_id": rid,
			"error": map[string]string{"type": typ, "message": "upstream stream failed: " + red.Error(err).Error()}})
		send([]byte("event: error\ndata: " + string(msg) + "\n\n"))
		return
	}
}

// terminated completes an event the upstream cut off at the end of its
// stream with the blank line that ends an event.
func terminated(raw []byte) []byte {
	switch {
	case bytes.HasSuffix(raw, []byte("\n\n")), bytes.HasSuffix(raw, []byte("\r\n\r\n")):
		return raw
	case bytes.HasSuffix(raw, []byte("\n")):
		return append(raw, '\n')
	}
	return append(raw, '\n', '\n')
}

// messageStartWithModel rewrites the model inside message_start's message.
func messageStartWithModel(data []byte, alias string) []byte {
	var ev map[string]json.RawMessage
	if json.Unmarshal(data, &ev) != nil || ev["message"] == nil {
		return data
	}
	ev["message"] = withModel(ev["message"], alias)
	out, _ := json.Marshal(ev)
	return out
}
