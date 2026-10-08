package convert

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ResponseMeta is what a Response carries beside its output.
type ResponseMeta struct {
	ID        string // resp_…, which the items' ids extend
	Model     string // the name the caller sent
	CreatedAt int64  // Unix seconds
	Echo      map[string]json.RawMessage
}

// itemID is the id of the output item at index n: the response's own,
// under the item type's prefix, so an id names its response and place.
func (m ResponseMeta) itemID(prefix string, n int) string {
	return fmt.Sprintf("%s_%s_%d", prefix, strings.TrimPrefix(m.ID, "resp_"), n)
}

// response is the Response object: m's fields, the echoed request, and the
// status, output and usage given. A Response that failed carries its error.
func (m ResponseMeta) response(status string, output []any, usage any, incomplete, failed map[string]string) map[string]any {
	r := map[string]any{
		"id": m.ID, "object": "response", "created_at": m.CreatedAt, "status": status,
		"model": m.Model, "output": output, "usage": usage, "error": nil, "incomplete_details": nil,
		"access_programs": nil, "background": false, "truncation": "disabled",
		"text": map[string]any{"format": map[string]string{"type": "text"}}, "max_output_tokens": nil,
	}
	for k, v := range m.Echo {
		r[k] = v
	}
	if incomplete != nil {
		r["incomplete_details"] = incomplete
	}
	if failed != nil {
		r["error"] = failed
	}
	return r
}

// responseStatus is a Messages stop reason as a Response's status, and what
// left it incomplete: the token limit, or a refusal, a filter's doing.
func responseStatus(stop string) (string, map[string]string) {
	switch stop {
	case "max_tokens", "model_context_window_exceeded":
		return "incomplete", map[string]string{"reason": "max_output_tokens"}
	case "refusal":
		return "incomplete", map[string]string{"reason": "content_filter"}
	}
	return "completed", nil
}

// responseUsage is a Messages usage as a Response's: input counts the prompt
// tokens read from the cache and written to it, which Messages counts apart,
// and the reasoning tokens are the thinking tokens the upstream reported in
// output_tokens_details, or 0 where it reported none.
func responseUsage(u map[string]int64) map[string]any {
	in := u["input_tokens"] + u["cache_read_input_tokens"] + u["cache_creation_input_tokens"]
	return map[string]any{
		"input_tokens": in,
		"input_tokens_details": map[string]int64{
			"cached_tokens": u["cache_read_input_tokens"], "cache_write_tokens": u["cache_creation_input_tokens"],
		},
		"output_tokens":         u["output_tokens"],
		"output_tokens_details": map[string]int64{"reasoning_tokens": u["thinking_tokens"]},
		"total_tokens":          in + u["output_tokens"],
	}
}

// usageCounts reads a Messages usage object's counts, its thinking tokens
// as "thinking_tokens", overwriting those in into: a stream's message_delta
// restates what it reports.
func usageCounts(into map[string]int64, raw json.RawMessage) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return
	}
	for _, k := range []string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
		var n int64
		if json.Unmarshal(obj[k], &n) == nil && !null(obj[k]) {
			into[k] = n
		}
	}
	var details map[string]json.RawMessage
	var n int64
	if json.Unmarshal(obj["output_tokens_details"], &details) == nil &&
		json.Unmarshal(details["thinking_tokens"], &n) == nil && !null(details["thinking_tokens"]) {
		into["thinking_tokens"] = n
	}
}

// ResponsesAnswer converts a whole Messages answer to a Response. Each
// content block is an output item, in order, its id the response's: text a
// message holding one output_text, a thinking block a reasoning item whose
// one summary_text is the thinking and whose encrypted_content is the
// signature — null when there is none — a redacted_thinking block a
// reasoning item with no summary whose encrypted_content is its data, and a
// tool_use block a function_call whose call_id is the block's id. The stop
// reason is the status (responseStatus). It fails on a block with no
// Responses counterpart, which only a server tool, never asked for, makes.
func ResponsesAnswer(b []byte, m ResponseMeta) ([]byte, error) {
	var msg map[string]json.RawMessage
	if json.Unmarshal(b, &msg) != nil || msg == nil {
		return nil, fmt.Errorf("the answer is not a JSON object")
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(msg["content"], &blocks) != nil {
		return nil, fmt.Errorf("the answer's content is not an array of objects")
	}
	output := make([]any, 0, len(blocks))
	for i, blk := range blocks {
		item, err := responseItem(m, i, blk)
		if err != nil {
			return nil, err
		}
		output = append(output, item)
	}
	stop, _ := text(msg, "stop_reason")
	usage := map[string]int64{}
	usageCounts(usage, msg["usage"])
	status, incomplete := responseStatus(stop)
	return encode(m.response(status, output, responseUsage(usage), incomplete, nil)), nil
}

// responseItem is a content block as a finished output item at index n.
func responseItem(m ResponseMeta, n int, blk map[string]json.RawMessage) (map[string]any, error) {
	switch typ, _ := text(blk, "type"); typ {
	case "text":
		t, _ := text(blk, "text")
		return messageItem(m.itemID("msg", n), "completed", []any{outputText(t)}), nil
	case "thinking":
		t, _ := text(blk, "thinking")
		sig, _ := text(blk, "signature")
		return reasoningItem(m.itemID("rs", n), "completed", []any{summaryText(t)}, sig), nil
	case "redacted_thinking":
		data, _ := text(blk, "data")
		return reasoningItem(m.itemID("rs", n), "completed", []any{}, data), nil
	case "tool_use":
		id, _ := text(blk, "id")
		name, _ := text(blk, "name")
		args := "{}"
		if !null(blk["input"]) {
			args = string(compact(blk["input"]))
		}
		return callItem(m.itemID("fc", n), "completed", id, name, args), nil
	default:
		return nil, fmt.Errorf("content[%d]: a %q block has no Responses counterpart", n, typ)
	}
}

func messageItem(id, status string, content []any) map[string]any {
	return map[string]any{"id": id, "type": "message", "role": "assistant", "status": status, "content": content}
}

func outputText(t string) map[string]any {
	return map[string]any{"type": "output_text", "text": t, "annotations": []any{}, "logprobs": []any{}}
}

func summaryText(t string) map[string]any { return map[string]any{"type": "summary_text", "text": t} }

// reasoningItem is a reasoning item; an empty encrypted_content is null.
func reasoningItem(id, status string, summary []any, enc string) map[string]any {
	item := map[string]any{"id": id, "type": "reasoning", "status": status, "summary": summary, "encrypted_content": nil}
	if enc != "" {
		item["encrypted_content"] = enc
	}
	return item
}

func callItem(id, status, callID, name, args string) map[string]any {
	return map[string]any{"id": id, "type": "function_call", "status": status, "call_id": callID, "name": name, "arguments": args}
}
