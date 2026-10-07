package modelgateway

import (
	"bytes"
	"encoding/json"
	"slices"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/catalog"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// honoring keeps the attempts whose vendor does not ignore anything the
// request asks for (profile.Profile.Ignores), and names each field the ones
// it dropped ignore, once and in order, for the refusal when it keeps none.
func honoring(attempts []catalog.Attempt, req map[string]json.RawMessage) ([]catalog.Attempt, []string) {
	var ignored []string
	kept := attempts[:0:0]
	for _, at := range attempts {
		if p, _ := profile.Lookup(at.Provider.Profile); p.Ignores != nil {
			if field := p.Ignores(req); field != "" {
				if !slices.Contains(ignored, field) {
					ignored = append(ignored, field)
				}
				continue
			}
		}
		kept = append(kept, at)
	}
	return kept, ignored
}

// flattenSearchResults renders each search_result block in a tool_result's
// content as a text block, through provider.SearchResultText, as the brain's
// flatten_search_results does (internal/provider/anthropic). Keys are read
// exactly, as thinking provenance reads them; a block it cannot render goes
// as sent, for the upstream to judge, and so does everything else. The
// rendering is deterministic, so every request of a conversation sends the
// upstream the same prefix.
func flattenSearchResults(msgs json.RawMessage) json.RawMessage {
	if !bytes.Contains(msgs, []byte("search_result")) && !bytes.Contains(msgs, []byte(`\u`)) {
		return msgs
	}
	out, _ := eachObject(msgs, func(msg map[string]json.RawMessage) bool {
		return inPlace(msg, "content", func(content json.RawMessage) (json.RawMessage, bool) {
			return eachBlock(content, "tool_result", func(result map[string]json.RawMessage) bool {
				return inPlace(result, "content", func(inner json.RawMessage) (json.RawMessage, bool) {
					return eachBlock(inner, "search_result", flattenSearchResult)
				})
			})
		})
	})
	return out
}

// inPlace replaces obj[key] with what fn makes of it, when fn changed it.
func inPlace(obj map[string]json.RawMessage, key string, fn func(json.RawMessage) (json.RawMessage, bool)) bool {
	v, changed := fn(obj[key])
	if changed {
		obj[key] = v
	}
	return changed
}

// flattenSearchResult turns a search_result block into the text block
// SearchResultText renders, in place, keeping its cache_control, which a
// text block takes too; its citations config has no text-block form. The
// title, the source and the inner text blocks are read by their exact keys
// and handed to the renderer in one canonical form, so neither a key
// differing only in case nor the keys' order changes what is rendered. A
// block whose fields are not the strings and text blocks the wire has goes
// as sent.
func flattenSearchResult(block map[string]json.RawMessage) bool {
	var title, source string
	var inner []map[string]json.RawMessage
	if t, ok := block["title"]; ok && json.Unmarshal(t, &title) != nil {
		return false
	}
	if s, ok := block["source"]; ok && json.Unmarshal(s, &source) != nil {
		return false
	}
	if c, ok := block["content"]; ok && json.Unmarshal(c, &inner) != nil {
		return false
	}
	texts := make([]map[string]string, len(inner))
	for i, b := range inner {
		var typ, text string
		if json.Unmarshal(b["type"], &typ) != nil || typ != "text" || json.Unmarshal(b["text"], &text) != nil {
			return false
		}
		texts[i] = map[string]string{"type": "text", "text": text}
	}
	src, _ := json.Marshal(source)
	content, _ := json.Marshal(texts)
	rendered, err := provider.SearchResultText(title, src, content)
	if err != nil {
		return false
	}
	cache, cached := block["cache_control"]
	clear(block)
	block["type"] = json.RawMessage(`"text"`)
	block["text"], _ = json.Marshal(rendered)
	if cached {
		block["cache_control"] = cache
	}
	return true
}

// eachBlock calls fn on each block of type typ in raw, a content array.
func eachBlock(raw json.RawMessage, typ string, fn func(map[string]json.RawMessage) bool) (json.RawMessage, bool) {
	return eachObject(raw, func(block map[string]json.RawMessage) bool {
		var t string
		return json.Unmarshal(block["type"], &t) == nil && t == typ && fn(block)
	})
}

// eachObject calls fn on each object in raw, a JSON array, and re-encodes
// the ones fn changed. Anything that is not an array comes back as it came,
// as does an array fn left alone.
func eachObject(raw json.RawMessage, fn func(map[string]json.RawMessage) bool) (json.RawMessage, bool) {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return raw, false
	}
	changed := false
	for i, item := range items {
		var obj map[string]json.RawMessage
		if json.Unmarshal(item, &obj) != nil || !fn(obj) {
			continue
		}
		items[i], _ = json.Marshal(obj)
		changed = true
	}
	if !changed {
		return raw, false
	}
	out, _ := json.Marshal(items)
	return out, true
}
