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
	"sync/atomic"
	"time"
	"unicode/utf8"

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

// maxSessionID bounds a session id: the platform's are 31 bytes, and an
// application's own is an id, not a document.
const maxSessionID = 256

// defaultAnthropicVersion is sent when a caller sends none; every Anthropic
// SDK sends one.
const defaultAnthropicVersion = "2023-06-01"

// writeStall bounds each write to the caller. A caller that stops reading but
// keeps its connection open would otherwise hold the handler for as long as it
// liked; past the bound the caller is treated as gone, and an upstream stream
// is still read to its end. It
// is read atomically: a test shortens it while a handler an earlier test
// started may still be finishing.
var writeStall = func() *atomic.Int64 {
	var d atomic.Int64
	d.Store(int64(time.Minute))
	return &d
}()

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
	_ = rc.SetWriteDeadline(time.Now().Add(time.Duration(writeStall.Load())))
	return rc
}

// messages serves /v1/messages and its count_tokens twin.
func (h *handler) messages(w http.ResponseWriter, r *http.Request, c caller, path string) {
	start := time.Now()
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
	// A deployment whose vendor would ignore what the request asks for is
	// skipped; a count is made all the same where every one would, since
	// what a vendor ignores leaves the count unchanged.
	kept, ignored := honoring(attempts, top)
	switch {
	case len(kept) > 0:
		if len(kept) < len(attempts) {
			slog.InfoContext(r.Context(), "modelgateway: attempts skipped for what their vendor ignores",
				"alias", a.Name, "fields", strings.Join(ignored, ","), "attempts", len(attempts)-len(kept))
		}
		attempts = kept
	case path == "/v1/messages/count_tokens":
	case len(ignored) == 1:
		writeError(w, r, invalid("%s: every upstream of model %s ignores it", ignored[0], model))
		return
	default:
		writeError(w, r, invalid("every upstream of model %s ignores one of %s", model, strings.Join(ignored, ", ")))
		return
	}
	// A session id the ledger cannot hold — longer than its index takes, or
	// not UTF-8, which net/http admits in a header and Postgres refuses in
	// text — is refused before it is counted, rather than leave the request
	// unrecorded.
	if s := r.Header.Get(SessionHeader); len(s) > maxSessionID || !utf8.ValidString(s) {
		writeError(w, r, invalid("%s: at most %d bytes of UTF-8", SessionHeader, maxSessionID))
		return
	}
	// Only a request the key's limits admit counts, and is recorded: the
	// refusals above are the gateway's own, made before any upstream was
	// asked.
	if !h.admit(w, r, c) {
		return
	}
	out := &outcome{}
	defer h.record(w, r, c, a.Name, model, path, start, out)
	call := call{
		top:    top,
		hist:   parseHistory(top["messages"]),
		alias:  model,
		path:   path,
		stream: path == "/v1/messages" && string(bytes.TrimSpace(top["stream"])) == "true",
		header: forwarded(r.Header),
		start:  start,
		out:    out,
	}
	if dep := call.hist.producer(attempts); dep != "" {
		attempts = preferring(attempts, dep)
	}
	// MaxAttempts bounds the calls to each deployment rather than to the
	// alias, so a deployment with many credentials cannot spend the budget a
	// fallback deployment needed. A caller that leaves ends the retries, and
	// is still written the last failure: net/http also reads a caller that
	// only half-closed its connection as gone, and that caller is reading.
	//
	// An upstream refusing the thinking its request carried puts the request
	// in strip mode — once, for the attempt it refused and every attempt
	// after, so nothing stripped is sent again — and that attempt is made
	// again, as a retry like any other: at the same deployment while its
	// budget lasts, and at the next one once it is spent.
	var last *failure
	tried := map[string]int{}
	n := 0
	strip := false
	for i := 0; i < len(attempts); i++ {
		at := attempts[i]
		if tried[at.Deployment.ID] == h.cfg.MaxAttempts {
			continue
		}
		tried[at.Deployment.ID]++
		if n > 0 && !h.backoff(r.Context(), n) {
			break
		}
		n++
		f, retry := h.tracedAttempt(w, r, call, at, strip)
		if f == nil {
			return
		}
		last = f
		if !strip && f.refusesThinking() && call.hist.carries(at.Deployment.ID) {
			slog.InfoContext(r.Context(), "modelgateway: upstream refused the request's thinking; retrying without it",
				"alias", a.Name, "deployment", at.Deployment.ID, "credential", at.Credential.ID)
			strip = true
			i--
			continue
		}
		if !retry {
			f.write(w, r, out)
			return
		}
		slog.InfoContext(r.Context(), "modelgateway: attempt failed", "alias", a.Name,
			"deployment", at.Deployment.ID, "credential", at.Credential.ID, "status", f.status, "error", f.err)
	}
	last.write(w, r, out)
}

// call is what every attempt of one request sends.
type call struct {
	top    map[string]json.RawMessage
	hist   *history // its messages' thinking, nil when there is none
	alias  string   // the model name the caller sent, which the answer carries back
	path   string
	stream bool
	header http.Header // the caller's headers that go upstream
	start  time.Time   // when the request arrived
	out    *outcome    // what the ledger is told of it
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

// refusesThinking reports whether the failure is an upstream's 400 refusing
// the thinking in the request (thinkingRefusal).
func (f *failure) refusesThinking() bool {
	return f.status == http.StatusBadRequest && thinkingRefusal(f.body)
}

// errorType is the error the failure gives the caller.
func (f *failure) errorType() string {
	if f.body != nil {
		return errorTypeOf(f.body, f.status)
	}
	return f.typ
}

// write answers the caller with the failure, and tells out what the caller
// was given.
func (f *failure) write(w http.ResponseWriter, r *http.Request, out *outcome) {
	out.status, out.errType = f.status, f.errorType()
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
	typ := errorTypeOf(data, 0)
	switch typ {
	case "authentication_error", "permission_error", "billing_error":
		return refusedCredential(ctx, at, typ, data, red)
	}
	status, ok := errorStatus[typ]
	if !ok {
		status = http.StatusInternalServerError
	}
	h := header.Clone()
	h.Set("Content-Type", "application/json")
	return &failure{status: status, header: h, body: errorJSON(ctx, red, data, rid)}, retryable(status, header)
}

// attempt makes one upstream call, in strip mode or not. It answers the
// caller and returns nil, or
// returns the failure and whether another attempt may cure it. Nothing is
// written to the caller until the upstream's answer has begun — a whole body,
// or a stream's first event that is not an error — so every failure it
// returns leaves the caller's response untouched.
//
// The call runs under a context the caller's cancellation does not reach,
// bounded by the provider's stall guard instead: a caller that disconnects
// does not end an upstream answer it has started paying for, which is read to
// its end.
func (h *handler) attempt(w http.ResponseWriter, r *http.Request, c call, at catalog.Attempt, strip bool) (*failure, bool) {
	c.out.at = &at
	ctx := context.WithoutCancel(r.Context())
	key, err := h.open(ctx, at.Credential)
	if err != nil {
		slog.ErrorContext(ctx, "modelgateway: credential could not be opened", "credential", at.Credential.ID, "error", err)
		return &failure{status: http.StatusInternalServerError, typ: "api_error", err: errors.New("internal error")}, true
	}
	red := provider.NewRedactor(provider.Config{APIKey: string(key), Headers: at.Provider.Headers})
	prof, _ := profile.Lookup(at.Provider.Profile)
	body := upstreamBody(c, at.Deployment, prof, strip)
	wrap := newWrapping(at.Deployment.ID, strip)
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
	propagate(ctx, at.Provider, req.Header)
	req.Header.Set("Content-Type", "application/json")
	if prof.BearerAuth {
		req.Header.Set("Authorization", "Bearer "+string(key))
	} else {
		req.Header.Set("x-api-key", string(key))
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return noAnswer(guard, red, err)
	}
	answered(ctx, resp.StatusCode)
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
			switch {
			case err != nil:
				return noAnswer(guard, red, err)
			case keepAlive(e) && len(held)+len(e.Raw) <= maxHeld:
				held = append(held, e.Raw...)
			case e.Name == "error":
				return streamError(ctx, at, e.Data, resp.Header, requestID(r), red)
			default:
				relayStream(ctx, w, events, held, e, c, wrap, requestID(r), guard, red)
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
	body, usage := answerJSON(b, c.alias, wrap)
	_, _ = w.Write(body)
	c.out.status = resp.StatusCode
	if c.path == "/v1/messages" { // a count's answer is a count, not usage, whatever else it names
		c.out.tokens = usageOf(nil, usage)
	}
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

// upstreamBody is the caller's body for one deployment: its messages'
// thinking filtered by provenance (history.messagesFor), then the profile's
// edits, then model set to the deployment's upstream id; every other
// top-level value goes out as the caller sent it. The values were decoded
// from JSON, so encoding them again cannot fail.
func upstreamBody(c call, d store.Deployment, prof profile.Profile, strip bool) []byte {
	out := make(map[string]json.RawMessage, len(c.top))
	for k, v := range c.top {
		out[k] = v
	}
	if c.hist != nil {
		out["messages"] = c.hist.messagesFor(d.ID, strip)
	}
	if m, ok := out["messages"]; ok && prof.FlattenSearchResults {
		out["messages"] = flattenSearchResults(m)
	}
	out["model"] = encodeJSON(d.UpstreamModel)
	b := encodeJSON(out)
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

// answerJSON is a whole answer as the caller gets it: its model the name the
// caller sent, and its thinking blocks wrapped. Anything that is not an
// object with a model or thinking passes unchanged. The answer's usage
// object, read by its exact key, comes back beside it.
func answerJSON(b []byte, alias string, wrap *wrapping) ([]byte, json.RawMessage) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(b, &obj) != nil {
		return b, nil
	}
	usage := obj["usage"]
	_, model := obj["model"]
	content := wrap.content(obj["content"])
	if !model && content == nil {
		return b, usage
	}
	if model {
		obj["model"] = encodeJSON(alias)
	}
	if content != nil {
		obj["content"] = content
	}
	return encodeJSON(obj), usage
}

// keepAlive is an event that holds a stream open and says nothing: a
// comment or a ping.
func keepAlive(e upstream.Event) bool { return e.Name == "ping" || e.Name == "" && e.Data == nil }

// relayStream passes an upstream's events to the caller as each arrives —
// the keep-alives held before the first, then the first, then the rest —
// rewriting only message_start's message (answerJSON), an upstream error
// event (errorJSON) and a thinking block's wrapper, on its start or its first
// non-empty signature fragment. Comments and pings pass unchanged, and each
// event goes out
// whole: one the upstream cut off at the end of its stream is completed when
// its data parses, so the caller dispatches it and nothing written after it
// merges in, and dropped when its data does not, being unfinished. The
// usage message_start and message_delta report, the error the caller is
// given, and the time to the first event that is not a keep-alive — the
// relay may begin on keep-alives past maxHeld — go to c.out as they pass.
// When the caller stops reading — or reads too slowly to take an event within
// writeStall — the stream is still read. When the upstream fails partway, or
// ends before message_stop, the caller gets an error event, the only way left
// to say so, and an unfinished event before it is dropped rather than merged
// into it. Once message_stop or the upstream's own error event has passed, the
// stream has said all it will and the relay ends: an upstream that holds its
// connection open after that holds nothing of the gateway's. Ending there
// resets the stream on an HTTP/2 connection, which a TLS upstream negotiates,
// and gives up an HTTP/1.1 one rather than returning it to the pool.
func relayStream(ctx context.Context, w http.ResponseWriter, events *upstream.Reader, held []byte, first upstream.Event, c call, wrap *wrapping, rid string, guard *provider.StallGuard, red provider.Redactor) {
	c.out.status = http.StatusOK
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	rc := bounded(w)
	w.WriteHeader(http.StatusOK)
	gone, done := false, false
	send := func(b []byte) {
		if gone {
			return
		}
		_ = rc.SetWriteDeadline(time.Now().Add(time.Duration(writeStall.Load())))
		if _, err := w.Write(b); err != nil {
			gone = true
			return
		}
		if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
			gone = true
		}
	}
	relay := func(e upstream.Event) {
		if c.out.ttft == 0 && !keepAlive(e) {
			c.out.ttft = time.Since(c.start)
		}
		out := e.Raw
		switch {
		case e.Data == nil:
		case e.Name == "message_start":
			d, usage := messageStart(e.Data, c.alias, wrap)
			c.out.tokens = usageOf(nil, usage)
			out = e.WithData(d)
		case e.Name == "message_delta":
			c.out.tokens = usageOf(c.out.tokens, member(e.Data, "usage"))
		case e.Name == "error":
			c.out.errType = errorTypeOf(e.Data, 0)
			out = e.WithData(errorJSON(ctx, red, e.Data, rid))
		case e.Name == "content_block_start":
			if d := wrap.start(e.Data); d != nil {
				out = e.WithData(d)
			}
		case e.Name == "content_block_delta":
			if d := wrap.delta(e.Data); d != nil {
				out = e.WithData(d)
			}
		}
		send(out)
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
		c.out.errType = typ
		msg := encodeJSON(map[string]any{"type": "error", "request_id": rid,
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

// messageStart rewrites message_start's message as answerJSON does, and
// returns the message's usage beside it.
func messageStart(data []byte, alias string, wrap *wrapping) ([]byte, json.RawMessage) {
	var ev map[string]json.RawMessage
	if json.Unmarshal(data, &ev) != nil || ev["message"] == nil {
		return data, nil
	}
	var usage json.RawMessage
	ev["message"], usage = answerJSON(ev["message"], alias, wrap)
	return encodeJSON(ev), usage
}
