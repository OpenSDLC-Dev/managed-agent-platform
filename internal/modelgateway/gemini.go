package modelgateway

import (
	"encoding/json"
	"errors"
	"maps"
	"net/url"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/convert"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
)

// The Gemini conversion (docs/plan/62_gemini-upstream-protocol.md): a
// Messages request whose attempt reaches its provider on the Gemini protocol
// (catalog.Plan) goes to the deployment's generateContent as the request
// convert.GeminiRequest makes of it — its messages filtered by thinking
// provenance first, so a Gemini signature goes back only to the deployment
// that produced it — with the provider's key as x-goog-api-key, and the
// answer comes back as a Messages one (convert.GeminiAnswer). Everything else
// is the Chat Completions conversion's (converted.go): routing, retries,
// limits, telemetry, errors and the ledger.

// errGeminiStream is the refusal of a streamed request by a Gemini attempt,
// which answers whole answers alone until the stream conversion lands.
var errGeminiStream = errors.New("stream: a Gemini upstream answers only a request that does not stream")

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
// goes in the path, escaped, rather than in the body.
func geminiPath(model string) string {
	return "/models/" + url.PathEscape(model) + ":generateContent"
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
