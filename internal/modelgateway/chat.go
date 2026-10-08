package modelgateway

import (
	"bytes"
	"encoding/json"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
)

// The Chat Completions passthrough (docs/plan/59_model-gateway.md, "Two
// request paths"): the body goes upstream as the caller sent it but for
// model and, on a stream, stream_options; the answer comes back as the
// upstream sent it but for model. Fields are read and written by their exact
// keys, as the thinking edits are. Its typed reference is openai-go
// (docs/REFERENCE_PROJECTS.md).

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

// withUsage is stream_options with include_usage true, its other options
// kept; a value that is not an object goes as sent, for the upstream to
// refuse.
func withUsage(raw json.RawMessage) json.RawMessage {
	opts := map[string]json.RawMessage{}
	if len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if json.Unmarshal(raw, &opts) != nil || opts == nil {
			return raw
		}
	}
	opts["include_usage"] = json.RawMessage("true")
	return encodeJSON(opts)
}

// asksForUsage reports whether a Chat Completions request asks for its
// stream's usage chunk.
func asksForUsage(top map[string]json.RawMessage) bool {
	var asked bool
	return json.Unmarshal(member(top["stream_options"], "include_usage"), &asked) == nil && asked
}

// chatAnswerJSON is a whole Chat Completions answer as the caller gets it,
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
