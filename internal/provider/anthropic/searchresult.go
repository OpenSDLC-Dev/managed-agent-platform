package anthropic

import (
	"bytes"
	"encoding/json"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// searchResultMarker is the fast-path guard: a message whose raw content
// bytes never contain this substring cannot hold a search_result block
// anywhere (top-level or nested in a tool_result), so flattenSearchResults
// can skip parsing it at all. Without it, every message on a
// flatten_search_results route pays a full unmarshal/rewalk/remarshal even
// when it carries no search_result — measurably slower on a turn with many
// large, unrelated tool_results.
var searchResultMarker = []byte(`"search_result"`)

// flattenSearchResults rewrites search_result blocks inside a message's
// tool_result content to text, via the rendering shared with provider/openai
// (provider.SearchResultText — internal/provider/searchresult.go). It exists
// for the route opt-in Config.FlattenSearchResults — an endpoint that rejects
// search_result in replayed tool_result content (#565), not the adapter's
// default: every other content block, and the string form of a message's or
// a tool_result's content, passes through untouched. A block this cannot
// safely rewrite is left exactly as received rather than failing the whole
// request — validity is the endpoint's judgment, not this adapter's
// (Generate) — so flatten_search_results never turns a decode surprise into
// a failed turn.
func flattenSearchResults(raw json.RawMessage) json.RawMessage {
	if !bytes.Contains(raw, searchResultMarker) {
		return raw
	}
	rewritten, changed := rewriteBlocks(raw, "tool_result", flattenToolResultBlock)
	if !changed {
		return raw
	}
	return rewritten
}

// rewriteBlocks scans raw as an array of content blocks — a message's
// content, or a tool_result's — and replaces every block whose "type" field
// equals typ with fn's rendering, leaving every other block untouched. It
// reports (raw, false) unchanged when raw is not itself a block array (the
// string form of content), when it fails to decode, or when nothing of the
// given type needed rewriting; fn itself reports false to leave its one
// block untouched rather than aborting the scan. This is the one loop
// flattenSearchResults (typ "tool_result") and flattenToolResultBlock (typ
// "search_result") both drive.
func rewriteBlocks(raw json.RawMessage, typ string, fn func(json.RawMessage) (json.RawMessage, bool)) (json.RawMessage, bool) {
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
		return raw, false
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return raw, false
	}
	changed := false
	for i, b := range blocks {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(b, &probe); err != nil || probe.Type != typ {
			continue
		}
		if flat, ok := fn(b); ok {
			blocks[i] = flat
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	out, err := json.Marshal(blocks)
	if err != nil {
		return raw, false
	}
	return out, true
}

// flattenToolResultBlock rewrites one tool_result block's search_result
// content blocks to text, preserving every other field of the block
// (tool_use_id, is_error, cache_control, …) and every other content block
// untouched. The `content, ok := tr["content"]` check below matters: a
// tool_result with no content field must stay that way rather than gaining
// an unmarshaled `"content":null`.
func flattenToolResultBlock(block json.RawMessage) (json.RawMessage, bool) {
	var tr map[string]json.RawMessage
	if err := json.Unmarshal(block, &tr); err != nil {
		return block, false
	}
	content, ok := tr["content"]
	if !ok {
		return block, false
	}
	newContent, changed := rewriteBlocks(content, "search_result", flattenSearchResultBlock)
	if !changed {
		return block, false
	}
	tr["content"] = newContent
	out, err := json.Marshal(tr)
	if err != nil {
		return block, false
	}
	return out, true
}

// flattenSearchResultBlock renders one search_result block as a text block
// via provider.SearchResultText. Nothing else on the block survives: its
// `citations` field is a `{enabled}` config (CitationsConfigParam), while a
// text block's own `citations` field is a list of citation objects
// (TextCitationParamUnion) — a shape mismatch, not a missing field, so it is
// dropped along with the block's own boundaries, same as
// provider.SearchResultText already documents. (A search_result's only other
// wire fields are type and citations: the pinned managed-agents type,
// BetaManagedAgentsSearchResultBlockParam, has no cache_control unlike a text
// or tool_result block — and this platform's own inbound validation agrees,
// rejecting cache_control on an inbound search_result block — so there is
// nothing else a search_result block could carry over.) A block
// SearchResultText cannot render — a non-string source, a non-text inner
// block — is left untouched (reported via the bool) rather than failing the
// request.
func flattenSearchResultBlock(block json.RawMessage) (json.RawMessage, bool) {
	var sr map[string]json.RawMessage
	if err := json.Unmarshal(block, &sr); err != nil {
		return block, false
	}
	var title string
	if raw, ok := sr["title"]; ok {
		if err := json.Unmarshal(raw, &title); err != nil {
			return block, false
		}
	}
	text, err := provider.SearchResultText(title, sr["source"], sr["content"])
	if err != nil {
		return block, false
	}
	textJSON, err := json.Marshal(text)
	if err != nil {
		return block, false
	}
	out, err := json.Marshal(map[string]json.RawMessage{"type": json.RawMessage(`"text"`), "text": textJSON})
	if err != nil {
		return block, false
	}
	return out, true
}
