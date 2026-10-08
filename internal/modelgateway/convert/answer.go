package convert

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Usage is an answer's token counts in the Messages API's meaning: input
// excludes the prompt tokens read from the cache and written to it, which
// are counted apart.
type Usage struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheWrite int64 `json:"cache_creation_input_tokens"`
	CacheRead  int64 `json:"cache_read_input_tokens"`
}

// Answer converts a whole Chat Completions answer to a Messages one, for a
// caller that named alias as its model. Its content is the first choice's,
// in the order a Messages answer has it: the reasoning as a thinking block
// signed by signature (the gateway's provenance wrapper, which the caller
// returns), the text — the content's, then a refusal's — and each tool
// call as a tool_use block. usage is the upstream's, as the caller reads it;
// nil reports zeros. The id is the upstream's, or id when it names none.
//
// It fails on an answer it cannot carry: no choice, a tool call whose
// arguments are not a JSON object, the deprecated function_call, which
// names a call without the id a tool_result answers, or a content,
// refusal or reasoning_content that is not a string. A tool call the token
// limit cut short, its arguments no JSON object under finish_reason length,
// is left out: the answer stops for max_tokens, which tells the caller to
// ask again with more.
func Answer(b []byte, alias, id string, usage *Usage, signature func() string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(b, &obj) != nil || obj == nil {
		return nil, fmt.Errorf("the answer is not a JSON object")
	}
	var choices []map[string]json.RawMessage
	if json.Unmarshal(obj["choices"], &choices) != nil || len(choices) == 0 {
		return nil, fmt.Errorf("the answer has no choice")
	}
	var msg map[string]json.RawMessage
	if json.Unmarshal(choices[0]["message"], &msg) != nil || msg == nil {
		return nil, fmt.Errorf("the answer's choice has no message")
	}
	if !null(msg["function_call"]) {
		return nil, fmt.Errorf("the answer uses the deprecated function_call; the upstream must answer with tool_calls")
	}
	strs := map[string]string{}
	for _, key := range []string{"reasoning_content", "content", "refusal"} {
		v, ok := text(msg, key)
		if !ok && !null(msg[key]) {
			return nil, fmt.Errorf("the answer's %s is not a string", key)
		}
		strs[key] = v
	}
	finish, _ := text(choices[0], "finish_reason")
	var content []any
	if r := strs["reasoning_content"]; r != "" {
		content = append(content, map[string]string{"type": "thinking", "thinking": r, "signature": signature()})
	}
	if t := strs["content"] + strs["refusal"]; t != "" {
		content = append(content, map[string]string{"type": "text", "text": t})
	}
	var calls []map[string]json.RawMessage
	if !null(msg["tool_calls"]) && json.Unmarshal(msg["tool_calls"], &calls) != nil {
		return nil, fmt.Errorf("the answer's tool_calls is not an array of objects")
	}
	for i, c := range calls {
		var fn map[string]json.RawMessage
		_ = json.Unmarshal(c["function"], &fn)
		callID, _ := text(c, "id")
		name, _ := text(fn, "name")
		args, _, err := Arguments(fn["arguments"])
		if err != nil {
			return nil, fmt.Errorf("the answer's tool call %d: %w", i, err)
		}
		input, err := toolInput(args)
		if err != nil && finish == "length" {
			continue
		} else if err != nil {
			return nil, fmt.Errorf("the answer's tool call %d: %w", i, err)
		}
		content = append(content, map[string]any{"type": "tool_use", "id": callID, "name": name, "input": input})
	}
	if content == nil {
		content = []any{}
	}
	if upstreamID, ok := text(obj, "id"); ok && upstreamID != "" {
		id = upstreamID
	}
	if usage == nil {
		usage = &Usage{}
	}
	return encode(map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": alias, "content": content,
		"stop_reason": StopReason(finish, len(calls) > 0, strs["refusal"] != ""), "stop_sequence": nil, "usage": usage,
	}), nil
}

// Arguments is a tool call's arguments as text: a string as it is, a JSON
// object, which Z.ai's chat-completion reference types them as, as its JSON,
// and null or absent as none. object reports a JSON object, which is whole:
// no fragment may join it.
func Arguments(raw json.RawMessage) (text string, object bool, err error) {
	raw = bytes.TrimSpace(raw)
	switch {
	case null(raw):
		return "", false, nil
	case raw[0] == '"':
		var s string
		err := json.Unmarshal(raw, &s)
		return s, false, err
	case raw[0] == '{':
		return string(raw), true, nil
	}
	return "", false, fmt.Errorf("its arguments are neither a string nor an object")
}

// toolInput is a tool call's arguments as a tool_use block's input: a JSON
// object, and an empty one for no arguments at all.
func toolInput(args string) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace([]byte(args))
	if len(trimmed) == 0 {
		return json.RawMessage("{}"), nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(trimmed, &obj) != nil || obj == nil {
		return nil, fmt.Errorf("its arguments are not a JSON object")
	}
	return json.RawMessage(trimmed), nil
}

// StopReason is a finish_reason as a Messages stop_reason. length is the
// token limit, a tool call it cut short included, as Messages reports one;
// then content_filter is a refusal, as is an answer that refused, which
// OpenAI ends with stop and a refusal in place of content, its tool calls
// included: a refusal is terminal, and a caller that stops for one runs none
// of them, as the SDK's tool runner does (checked against anthropic-sdk-go
// v1.70.1 — betatoolrunner.go determineNextStepFromStopReason). Otherwise an
// answer that called a tool stopped for it, whatever finish_reason says, as
// some OpenAI-compatible servers end a tool turn with stop; every other
// reason is end_turn — stop,
// which also names a stop sequence that matched, where Chat Completions
// does not say which, so stop_sequence is never set; and DeepSeek's
// insufficient_system_resource and aborted, an answer the vendor cut short,
// which Messages has no reason for.
func StopReason(finish string, called, refused bool) string {
	switch {
	case finish == "length":
		return "max_tokens"
	case finish == "content_filter", refused:
		return "refusal"
	case called:
		return "tool_use"
	}
	return "end_turn"
}
