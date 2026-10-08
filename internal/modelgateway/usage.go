package modelgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/catalog"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
)

// outcome is what one admitted request reports to the ledger, filled in as
// it is served.
type outcome struct {
	at      *catalog.Attempt // the attempt that answered, or the last one made
	status  int              // the HTTP status the caller was given
	errType string           // the error the caller was given, "" when none
	tokens  *store.Tokens    // as the upstream reported them; nil when it did not
	ttft    time.Duration    // until a stream's first event that is not a keep-alive; zero for a whole answer
}

// recordTimeout bounds the ledger write a finished request makes.
const recordTimeout = 10 * time.Second

// record writes the request to the ledger once it is over, under a context
// its caller's leaving does not end, so a request whose caller left is
// recorded all the same. The write follows the answer's last byte but comes
// before net/http ends the response, so a caller waits on it the time one
// insert takes; in return a replica shutting down records every request that
// ends within its shutdown's grace (cmd/modelgateway) — one still running
// when the grace runs out is cut off unrecorded. The write bound is lifted while the ledger is
// written — nothing goes to the caller then, and an HTTP/2 stream whose
// bound fires is reset and takes no new one — and what net/http writes
// after the handler returns, a small answer it buffered or the end of a
// chunked one, gets a bound of its own after it, so a slow write here
// cannot spend the bound the answer was given. A failed write is the
// operator's to see in the log, never the caller's.
func (h *handler) record(w http.ResponseWriter, r *http.Request, c caller, alias, model, path string, proto profile.Protocol, start time.Time, out *outcome) {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	defer func() { _ = rc.SetWriteDeadline(time.Now().Add(time.Duration(writeStall.Load()))) }()
	u := store.Usage{
		RequestID: requestID(r), APIKeyID: c.keyID, Model: ledgerText(model), Alias: alias,
		SessionID: r.Header.Get(SessionHeader), Protocol: string(proto), Endpoint: endpoints[path],
		Status: out.status, ErrorType: ledgerText(out.errType), Tokens: out.tokens, Latency: time.Since(start), TTFT: out.ttft,
	}
	if out.at != nil {
		u.DeploymentID, u.CredentialID = out.at.Deployment.ID, out.at.Credential.ID
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), recordTimeout)
	defer cancel()
	cost, err := h.cfg.Store.RecordUsage(ctx, u, c.policy.TPM != nil)
	if err != nil {
		slog.ErrorContext(ctx, "modelgateway: usage could not be recorded", "request_id", u.RequestID,
			"api_key_id", u.APIKeyID, "deployment", u.DeploymentID, "error", err)
	}
	observe(context.WithoutCancel(r.Context()), u, out.at, cost)
}

// maxLedgerText bounds the text a ledger row takes from a caller or an
// upstream: the model name sent, which the wildcard alias admits whatever it
// is, and the error type an upstream names.
const maxLedgerText = 256

// ledgerText is s, a string decoded from JSON and so valid UTF-8, as
// Postgres text holds it: without the NUL a JSON string may carry, which
// would fail the row, and cut to maxLedgerText bytes at a character's start.
func ledgerText(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	if len(s) <= maxLedgerText {
		return s
	}
	i := maxLedgerText
	for !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}

// endpoints names each inbound route in the ledger.
var endpoints = map[string]string{"/v1/messages": "messages", "/v1/messages/count_tokens": "count_tokens",
	"/v1/chat/completions": "chat_completions", "/v1/embeddings": "embeddings", "/v1/rerank": "rerank"}

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

// maxCount bounds a count the ledger reads: 2^32 tokens is some four hundred
// times the largest context window offered (10M tokens), so a count above
// it is a broken upstream's, read as nothing. Under it, a day's rollup reaches bigint's bound only past two
// billion such answers in one day for one key, alias and deployment, where
// one overflow would fail every later write to that day.
const maxCount = 1 << 32

// usageOf lays the counts an Anthropic usage object names over prev, each
// read by its exact key: a stream's message_delta counts are whole-message
// totals that overwrite message_start's, and the ones it omits keep theirs,
// as the SDK accumulates them (checked against anthropic-sdk-go v1.70.1 —
// messageutil.go Message.Accumulate) — but for output_tokens, which the SDK
// takes as zero when a delta leaves it out or null, where the ledger keeps
// the count it had rather than record a count no upstream reported.
// MiniMax's message_start reports zeros and its message_delta every count;
// DeepSeek's report all of them in both (probed 2026-10-07). A null, a count
// that is not a whole number or is past maxCount, or a missing usage counts
// as nothing, so the result is prev when no count is read.
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
		if n, ok := countOf(u[c.key]); ok {
			*c.get(&t) = n
			read = true
		}
	}
	if !read {
		return prev
	}
	return &t
}

// chatUsageOf reads an OpenAI usage object, by its exact keys, in the
// ledger's meaning, the Messages API's: input is the prompt tokens not read
// from the cache, and cache reads are counted apart. Both vendors count the
// cache reads inside prompt_tokens and report them as
// prompt_tokens_details.cached_tokens; DeepSeek also as
// prompt_cache_hit_tokens, read when the details are absent (probed
// 2026-10-08). OpenAI reports no cache writes. An embeddings answer reports
// prompt_tokens alone; Gitee's rerank answers report zeros under camelCase
// keys, which are not read (probed 2026-10-08). A usage without either
// prompt_tokens or completion_tokens, null, or a count usageOf would not
// read, is nil; a cache count past the prompt's is read as the prompt.
func chatUsageOf(raw json.RawMessage) *store.Tokens {
	var u map[string]json.RawMessage
	if json.Unmarshal(raw, &u) != nil || u == nil {
		return nil
	}
	prompt, okPrompt := countOf(u["prompt_tokens"])
	completion, okCompletion := countOf(u["completion_tokens"])
	if !okPrompt && !okCompletion {
		return nil
	}
	cached, ok := countOf(member(u["prompt_tokens_details"], "cached_tokens"))
	if !ok {
		cached, _ = countOf(u["prompt_cache_hit_tokens"])
	}
	cached = min(cached, prompt)
	return &store.Tokens{Input: prompt - cached, Output: completion, CacheRead: cached}
}

// countOf reads a count: a whole number from zero to maxCount, not null.
func countOf(v json.RawMessage) (int64, bool) {
	var n int64
	if len(v) == 0 || bytes.Equal(bytes.TrimSpace(v), []byte("null")) || json.Unmarshal(v, &n) != nil || n < 0 || n > maxCount {
		return 0, false
	}
	return n, true
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
// data, names, or the type Anthropic gives status when it names none — or
// names one carrying a NUL, which no Anthropic type has (and a lone one
// would leave the ledger no error to record).
func errorTypeOf(b []byte, status int) string {
	var typ string
	if json.Unmarshal(member(b, "error", "type"), &typ) == nil && typ != "" && !strings.ContainsRune(typ, 0) {
		return typ
	}
	for t, s := range errorStatus {
		if s == status {
			return t
		}
	}
	return "api_error"
}
