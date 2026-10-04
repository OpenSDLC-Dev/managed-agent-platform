package modelgateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/catalog"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/upstream"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// The body bounds: a request as large as the Messages API takes, and an
// answer well past any model's output.
const (
	maxRequestBody  = 32 << 20
	maxResponseBody = 64 << 20
)

// SessionHeader carries the caller's session id, which keeps a session's
// requests on one upstream (catalog.Snapshot.Plan). It never goes upstream.
const SessionHeader = "X-MAP-Session-ID"

// defaultAnthropicVersion is sent when a caller sends none; every Anthropic
// SDK sends one.
const defaultAnthropicVersion = "2023-06-01"

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
	if len(attempts) > h.cfg.MaxAttempts {
		attempts = attempts[:h.cfg.MaxAttempts]
	}
	call := call{
		top:    top,
		alias:  model,
		path:   path,
		stream: path == "/v1/messages" && string(bytes.TrimSpace(top["stream"])) == "true",
		header: forwarded(r.Header),
	}
	var last *failure
	for i, at := range attempts {
		if i > 0 && !h.backoff(r.Context(), i) {
			return // the caller left before its answer began
		}
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
	for _, k := range []string{"Content-Type", "Retry-After"} {
		if v := f.header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.WriteHeader(f.status)
	_, _ = w.Write(f.body)
}

// retryable reports the statuses another attempt may cure: a rate limit, an
// overload, or a server error.
func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

// attempt makes one upstream call. It answers the caller and returns nil, or
// returns the failure and whether another attempt may cure it. Nothing is
// written to the caller before the upstream's first byte, so every failure it
// returns leaves the caller's response untouched.
//
// The call runs under a context the caller's cancellation does not reach,
// bounded by the provider's stall guard instead: a caller that disconnects
// does not end an upstream answer it has started paying for, which is read to
// its end.
func (h *handler) attempt(w http.ResponseWriter, r *http.Request, c call, at catalog.Attempt) (*failure, bool) {
	key, err := h.cfg.Cipher.Decrypt(r.Context(), at.Credential.Ciphertext, at.Credential.KeyID)
	if err != nil {
		slog.ErrorContext(r.Context(), "modelgateway: credential could not be opened", "credential", at.Credential.ID, "error", err)
		return &failure{status: http.StatusInternalServerError, typ: "api_error", err: errors.New("internal error")}, true
	}
	red := provider.NewRedactor(provider.Config{APIKey: string(key), Headers: at.Provider.Headers})
	body := upstreamBody(c.top, at.Deployment.UpstreamModel)
	ctx, guard := provider.NewStallGuard(context.WithoutCancel(r.Context()), at.Provider.StallTimeout)
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
		return noAnswer(guard, red, err), true
	}
	resp.Body = provider.ProgressBody(ctx, resp.Body)
	defer resp.Body.Close()
	switch s := resp.StatusCode; {
	case s >= 300 && s < 400:
		return &failure{status: http.StatusBadGateway, typ: "api_error",
			err: fmt.Errorf("upstream answered a redirect (%d), which the gateway does not follow", s)}, true
	case s >= 400:
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
		if err != nil {
			return noAnswer(guard, red, err), true
		}
		return &failure{status: s, header: resp.Header, body: []byte(red.String(string(b)))}, retryable(s)
	}
	if c.stream && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		br := bufio.NewReader(resp.Body)
		if _, err := br.Peek(1); err != nil {
			return noAnswer(guard, red, err), true
		}
		relayStream(w, br, c.alias, guard, red)
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	switch {
	case err != nil:
		return noAnswer(guard, red, err), true
	case len(b) > maxResponseBody:
		return &failure{status: http.StatusBadGateway, typ: "api_error", err: errors.New("upstream answer exceeds 64 MiB")}, false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(withModel(b, c.alias))
	return nil, false
}

// noAnswer is an attempt whose upstream gave no usable answer: a stall, or a
// transport error.
func noAnswer(guard *provider.StallGuard, red provider.Redactor, err error) *failure {
	if err = guard.Cause(err); errors.Is(err, provider.ErrStalled) {
		return &failure{status: http.StatusGatewayTimeout, typ: "timeout_error", err: fmt.Errorf("upstream sent nothing: %w", err)}
	}
	return &failure{status: http.StatusBadGateway, typ: "api_error", err: fmt.Errorf("upstream request failed: %w", red.Error(err))}
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

// relayStream passes an upstream's events to the caller as each arrives,
// rewriting only message_start's message.model. Comments and pings pass
// unchanged, and each event goes out whole. When the caller stops reading,
// the stream is still read to its end; when the upstream fails partway, the
// caller gets an error event, the only way left to say so.
func relayStream(w http.ResponseWriter, br *bufio.Reader, alias string, guard *provider.StallGuard, red provider.Redactor) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	gone := false
	send := func(b []byte) {
		if gone {
			return
		}
		if _, err := w.Write(b); err != nil {
			gone = true
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	events := upstream.NewReader(br)
	for {
		e, err := events.Next()
		if len(e.Raw) > 0 {
			if e.Name == "message_start" && e.Data != nil {
				send(e.WithData(messageStartWithModel(e.Data, alias)))
			} else {
				send(e.Raw)
			}
		}
		switch {
		case errors.Is(err, io.EOF):
			return
		case err != nil:
			err = guard.Cause(err)
			typ := "api_error"
			if errors.Is(err, provider.ErrStalled) {
				typ = "timeout_error"
			}
			msg, _ := json.Marshal(map[string]any{"type": "error",
				"error": map[string]string{"type": typ, "message": "upstream stream failed: " + red.Error(err).Error()}})
			send([]byte("event: error\ndata: " + string(msg) + "\n\n"))
			return
		}
	}
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
