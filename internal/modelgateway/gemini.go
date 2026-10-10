package modelgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/catalog"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/convert"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// The Gemini conversion (docs/plan/62_gemini-upstream-protocol.md): a
// Messages request whose attempt reaches its provider on the Gemini protocol
// (catalog.Plan) goes to the deployment's generateContent as the request
// convert.GeminiRequest makes of it — its messages filtered by thinking
// provenance first, so a Gemini signature goes back only to the deployment
// that produced it — with the provider's key as x-goog-api-key, and the
// answer comes back as a Messages one (convert.GeminiAnswer), or a streamed
// request's stream as a Messages stream (convert.GeminiStream, relayed by
// convStream). Everything else
// is the Chat Completions conversion's (converted.go): routing, retries,
// limits, telemetry, errors — but the 400 Gemini refuses a key with
// (geminiKeyRefused) — and the ledger.

// geminiBody is the generateContent request one deployment is sent: the
// caller's body with its messages as provenance filters them for the
// deployment, converted. serve converted the request once already, so this
// cannot fail but on a request it refused there.
func geminiBody(c call, d store.Deployment, strip bool) ([]byte, error) {
	top := c.top
	if c.hist != nil {
		top = maps.Clone(c.top)
		top["messages"] = c.hist.messagesFor(d.ID, strip, profile.Gemini)
	}
	return convert.GeminiRequest(top)
}

// geminiPath is the path under a Gemini base URL that answers model, which
// goes in the path rather than in the body: under models/, or under the
// collection a name opening models/ or tunedModels/ names, as genai's tModel
// reads a name (google.golang.org/genai 1.73.0, transformer.go), the rest
// escaped, so it can add no segment or query. A streamed request goes to
// streamGenerateContent, as server-sent events (alt=sse).
func geminiPath(model string, stream bool) string {
	collection := "models"
	for _, c := range []string{"models", "tunedModels"} {
		if rest, ok := strings.CutPrefix(model, c+"/"); ok {
			collection, model = c, rest
			break
		}
	}
	method := ":generateContent"
	if stream {
		method = ":streamGenerateContent?alt=sse"
	}
	return "/" + collection + "/" + url.PathEscape(model) + method
}

// geminiStatus is the HTTP status a Gemini error object states as its code,
// a 4xx or 5xx, or 500 for one it does not state: the status of a refusal
// that reaches the gateway inside a 200 stream.
func geminiStatus(data []byte) int {
	var code int
	if json.Unmarshal(member(data, "error", "code"), &code) == nil && code >= 400 && code < 600 {
		return code
	}
	return http.StatusInternalServerError
}

// geminiStreamError is a Gemini stream whose first event is an error object:
// the upstream refused before it began an answer, so this is the error
// response it would have sent, its status the error's code, retried and
// relayed like one, a refused key included.
func geminiStreamError(ctx context.Context, c call, at catalog.Attempt, data []byte, header http.Header, rid string, red provider.Redactor) (*failure, bool) {
	status := geminiStatus(data)
	switch {
	case geminiKeyRefused(data):
		return refusedCredential(ctx, at, "API_KEY_INVALID", data, red)
	case status == http.StatusUnauthorized, status == http.StatusPaymentRequired, status == http.StatusForbidden:
		return refusedCredential(ctx, at, fmt.Sprintf("HTTP %d", status), data, red)
	}
	h := header.Clone()
	h.Set("Content-Type", "application/json")
	return &failure{status: status, header: h, body: c.upstreamError(ctx, red, data, status, rid)}, retryable(status, header)
}

// geminiKeyRefused reports whether a Gemini error refuses the key itself,
// which Google answers 400 INVALID_ARGUMENT with the google.rpc.ErrorInfo
// reason API_KEY_INVALID (measured 2026-10-10) rather than a 401, so its
// status alone would hand the caller the gateway's credential failure as a
// fault in its own request.
func geminiKeyRefused(b []byte) bool {
	var e struct {
		Error struct {
			Details []struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(b, &e)
	for _, d := range e.Error.Details {
		if d.Reason == "API_KEY_INVALID" {
			return true
		}
	}
	return false
}

// geminiUsageOf is the ledger's reading of a generateContent answer's
// usageMetadata, nil when it reports neither prompt nor candidate tokens.
// Input is the prompt and any tool-use prompt, which Gemini counts apart,
// but their cached part, which is the cache read; output is the candidates
// and the thoughts, which Gemini also counts apart and bills as output.
// Gemini reports no cache write.
func geminiUsageOf(raw json.RawMessage) *store.Tokens {
	var u map[string]json.RawMessage
	if json.Unmarshal(raw, &u) != nil || u == nil {
		return nil
	}
	prompt, okPrompt := countOf(u["promptTokenCount"])
	candidates, okCandidates := countOf(u["candidatesTokenCount"])
	if !okPrompt && !okCandidates {
		return nil
	}
	tool, _ := countOf(u["toolUsePromptTokenCount"])
	thoughts, _ := countOf(u["thoughtsTokenCount"])
	cached, _ := countOf(u["cachedContentTokenCount"])
	in := prompt + tool
	cached = min(cached, in)
	return &store.Tokens{Input: in - cached, Output: candidates + thoughts, CacheRead: cached}
}
