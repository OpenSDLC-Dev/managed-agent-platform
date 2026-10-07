package modelgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/catalog"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
)

// outcome is what one admitted request reports to the ledger, filled in as
// it is served.
type outcome struct {
	at      *catalog.Attempt // the attempt that answered, or the last one made
	status  int              // the HTTP status the caller was given
	errType string           // the error the caller was given, "" when none
	tokens  *store.Tokens    // as the upstream reported them; nil when it did not
	ttft    time.Duration    // until a stream's answer began; zero for a whole one
}

// recordTimeout bounds the ledger write a finished request makes.
const recordTimeout = 10 * time.Second

// record writes the request to the ledger once it is over, under a context
// its caller's leaving does not end, so a request whose caller left is
// recorded all the same. The write follows the answer's last byte but comes
// before net/http ends the response, so a caller waits on it the time one
// insert takes; in return a replica shutting down records every request it
// served before it stops. A failed write is the operator's to see in the
// log, never the caller's.
func (h *handler) record(r *http.Request, c caller, alias, model, path string, start time.Time, out *outcome) {
	u := store.Usage{
		RequestID: requestID(r), APIKeyID: c.keyID, Model: model, Alias: alias,
		SessionID: r.Header.Get(SessionHeader), Protocol: "anthropic", Endpoint: endpoints[path],
		Status: out.status, ErrorType: out.errType, Tokens: out.tokens, Latency: time.Since(start), TTFT: out.ttft,
	}
	if out.at != nil {
		u.DeploymentID, u.CredentialID = out.at.Deployment.ID, out.at.Credential.ID
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), recordTimeout)
	defer cancel()
	if err := h.cfg.Store.RecordUsage(ctx, u, c.policy.TPM != nil); err != nil {
		slog.ErrorContext(ctx, "modelgateway: usage could not be recorded", "request_id", u.RequestID,
			"api_key_id", u.APIKeyID, "deployment", u.DeploymentID, "error", err)
	}
}

// endpoints names each inbound route in the ledger.
var endpoints = map[string]string{"/v1/messages": "messages", "/v1/messages/count_tokens": "count_tokens"}

// admit counts the request against its key's limits, when the key has any,
// and answers a refusal itself: 429 rate_limit_error with retry-after in
// whole seconds, until the minute the refusal counted in ends.
func (h *handler) admit(w http.ResponseWriter, r *http.Request, c caller) bool {
	p := c.policy
	if p.RPM == nil && p.TPM == nil {
		return true
	}
	a, err := h.cfg.Store.Admit(r.Context(), c.keyID, p.RPM, p.TPM)
	if err != nil {
		writeError(w, r, internal(r, "rate limit check failed", err))
		return false
	}
	if a.Admitted {
		return true
	}
	limit := strconv.Itoa(int(derefInt32(p.RPM))) + " requests"
	if a.Tokens {
		limit = strconv.FormatInt(*p.TPM, 10) + " tokens"
	}
	w.Header().Set("Retry-After", strconv.Itoa(a.RetryAfter))
	writeError(w, r, &apiError{http.StatusTooManyRequests, "rate_limit_error",
		"this API key's limit of " + limit + " per minute is reached; retry after " + strconv.Itoa(a.RetryAfter) + " seconds"})
	return false
}

func derefInt32(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}

// usageCounts are the counts of an Anthropic usage object, by the store's
// fields.
var usageCounts = []struct {
	key string
	get func(*store.Tokens) *int64
}{
	{"input_tokens", func(t *store.Tokens) *int64 { return &t.Input }},
	{"output_tokens", func(t *store.Tokens) *int64 { return &t.Output }},
	{"cache_creation_input_tokens", func(t *store.Tokens) *int64 { return &t.CacheWrite }},
	{"cache_read_input_tokens", func(t *store.Tokens) *int64 { return &t.CacheRead }},
}

// usageOf lays the counts an Anthropic usage object names over prev, each
// read by its exact key: a stream's message_delta counts are whole-message
// totals that overwrite message_start's, and the ones it omits keep theirs,
// as the SDK accumulates them (anthropic-sdk-go v1.70.1 — messageutil.go
// Message.Accumulate). MiniMax's message_start reports zeros and its
// message_delta every count; DeepSeek's report all of them in both (probed
// 2026-10-07). A null, a count that is not a whole number, or a missing
// usage counts as nothing, so the result is prev when no count is read.
func usageOf(prev *store.Tokens, raw json.RawMessage) *store.Tokens {
	var u map[string]json.RawMessage
	if json.Unmarshal(raw, &u) != nil {
		return prev
	}
	var t store.Tokens
	if prev != nil {
		t = *prev
	}
	read := false
	for _, c := range usageCounts {
		var n int64
		if v, ok := u[c.key]; ok && !bytes.Equal(bytes.TrimSpace(v), []byte("null")) && json.Unmarshal(v, &n) == nil && n >= 0 {
			*c.get(&t) = n
			read = true
		}
	}
	if !read {
		return prev
	}
	return &t
}

// member is the value under the exact keys path in a JSON object, nil when
// any is missing.
func member(b []byte, path ...string) json.RawMessage {
	for _, k := range path {
		var obj map[string]json.RawMessage
		if json.Unmarshal(b, &obj) != nil {
			return nil
		}
		b = obj[k]
	}
	return b
}

// errorTypeOf is the type an Anthropic error envelope, a body or an event's
// data, names, or the type Anthropic gives status when it names none.
func errorTypeOf(b []byte, status int) string {
	var typ string
	if json.Unmarshal(member(b, "error", "type"), &typ) == nil && typ != "" {
		return typ
	}
	for t, s := range errorStatus {
		if s == status {
			return t
		}
	}
	return "api_error"
}
