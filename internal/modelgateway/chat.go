package modelgateway

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
)

// The Chat Completions passthrough (docs/plan/59_model-gateway.md, "Two
// request paths"): the body goes upstream as the caller sent it but for
// model and, on a stream, stream_options; the answer comes back as the
// upstream sent it but for model. Fields are read and written by their exact
// keys, as the thinking edits are. Its typed reference is openai-go
// (docs/REFERENCE_PROJECTS.md). An embeddings or rerank body passes through
// the same way, and never streams, so model is all it changes; its answer is
// relayed as it arrives (vectors.go). A body goes out with its keys sorted, so a key differing from model only
// in case comes before model, and a decoder that matches keys regardless of
// case, keeping the last, reads the deployment's upstream id.

// chatBody is the caller's body for one deployment: model its upstream id,
// and a stream asking for its usage, which the ledger and the key's TPM
// limit count — MiniMax reports a stream's usage only when asked, DeepSeek
// always (probed 2026-10-08). Everything else goes as sent.
func chatBody(c call, d store.Deployment) []byte {
	out := make(map[string]json.RawMessage, len(c.top)+1)
	for k, v := range c.top {
		out[k] = v
	}
	out["model"] = encodeJSON(d.UpstreamModel)
	if c.stream {
		out["stream_options"] = withUsage(c.top["stream_options"])
	}
	return encodeJSON(out)
}

// withUsage is stream_options, an object or null (chatStreamOptions), with
// include_usage true and its other options kept.
func withUsage(raw json.RawMessage) json.RawMessage {
	opts := map[string]json.RawMessage{}
	_ = json.Unmarshal(raw, &opts)
	if opts == nil {
		opts = map[string]json.RawMessage{}
	}
	opts["include_usage"] = json.RawMessage("true")
	return encodeJSON(opts)
}

// chatStreamOptions refuses stream_options the gateway could not ask for a
// stream's usage through, for streamFlag's reason: a value that is not an
// object or null, or a key that differs from stream_options or
// include_usage only in case, which a case-insensitive decoder may read in
// place of the gateway's own — Go's takes the later of two, and a key
// spelled with ſ sorts after it — leaving a stream's usage unreported.
func chatStreamOptions(top map[string]json.RawMessage) *apiError {
	for k := range top {
		if k != "stream_options" && strings.EqualFold(k, "stream_options") {
			return invalid("%s: the field is stream_options, spelled in lower case", k)
		}
	}
	raw := bytes.TrimSpace(top["stream_options"])
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	var opts map[string]json.RawMessage
	if json.Unmarshal(raw, &opts) != nil {
		return invalid("stream_options: must be an object")
	}
	for k := range opts {
		if k != "include_usage" && strings.EqualFold(k, "include_usage") {
			return invalid("stream_options.%s: the field is include_usage, spelled in lower case", k)
		}
	}
	return nil
}

// asksForUsage reports whether a Chat Completions request asks for its
// stream's usage chunk.
func asksForUsage(top map[string]json.RawMessage) bool {
	var asked bool
	return json.Unmarshal(member(top["stream_options"], "include_usage"), &asked) == nil && asked
}

// chatAnswerJSON is a whole answer on an OpenAI route as the caller gets it,
// its model the name the caller sent; anything that is not an object with a
// model passes unchanged. Its usage object comes back beside it.
func chatAnswerJSON(b []byte, alias string) ([]byte, json.RawMessage) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(b, &obj) != nil || obj == nil {
		return b, nil
	}
	usage := obj["usage"]
	if _, ok := obj["model"]; !ok {
		return b, usage
	}
	obj["model"] = encodeJSON(alias)
	return encodeJSON(obj), usage
}
