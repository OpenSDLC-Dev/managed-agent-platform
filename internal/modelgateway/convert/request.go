package convert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Request converts a Messages request body to a Chat Completions one for
// model, an upstream model id. Every top-level field the Messages API takes
// at anthropic-sdk-go's pin has a disposition here, and a field it does not
// name is refused, since what it asks of the answer is unknown:
//
//   - mapped: max_tokens, messages, system, stop_sequences (stop),
//     temperature, top_p, tools, tool_choice, stream (asking for the
//     stream's usage, which the gateway counts), output_config.effort
//     (reasoning_effort), and thinking, by thinking, the vendor's
//     thinking.type for each Anthropic one (profile.Profile.ChatThinking);
//   - dropped, as what a vendor may ignore under the gateway's edit policy:
//     top_k, a sampling knob Chat Completions lacks; cache_control, as these
//     vendors cache a prompt's prefix on their own; metadata, whose user_id
//     shapes no answer; service_tier and inference_geo, an Anthropic
//     account's capacity and residency choices; container, which only a
//     server tool uses, and those are refused; and thinking, where the
//     vendor names no thinking.type;
//   - refused: output_config.format, structured output a Chat Completions
//     vendor may not honor; a server tool; thinking.display omitted, since
//     the vendor's reasoning comes back as text; and a final assistant turn,
//     which Messages continues and Chat Completions would answer.
//
// A tool's name, description, input_schema and strict are mapped; its
// cache_control, eager_input_streaming (a streaming granularity) and
// input_examples (a hint) dropped; defer_loading true, which needs tool
// search, and allowed_callers without "direct", which a model calling the
// tool directly would overstep, refused, as is a field ToolParam does not
// name.
//
// The error names the field as the caller wrote it.
func Request(top map[string]json.RawMessage, model string, thinking map[string]string) ([]byte, error) {
	if null(top["max_tokens"]) { // which Chat Completions leaves to the upstream
		return nil, fmt.Errorf("max_tokens: required")
	}
	out := map[string]json.RawMessage{"model": encode(model)}
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic, so the first refusal named is too
	for _, k := range keys {
		v := top[k]
		if null(v) {
			continue
		}
		switch k {
		case "model", "messages", "system", "tools", "top_k", "cache_control", "metadata", "service_tier", "inference_geo", "container":
		case "max_tokens", "temperature", "top_p":
			out[k] = v
		case "stop_sequences":
			out["stop"] = v
		case "stream":
			out[k] = v
			if bytes.Equal(bytes.TrimSpace(v), []byte("true")) {
				out["stream_options"] = json.RawMessage(`{"include_usage":true}`)
			}
		case "tool_choice":
			choice, parallel, err := toolChoice(v)
			if err != nil {
				return nil, fmt.Errorf("tool_choice%w", err)
			}
			out["tool_choice"] = choice
			if parallel != nil {
				out["parallel_tool_calls"] = parallel
			}
		case "thinking":
			if _, err := thinkingType(v, nil); err != nil {
				return nil, fmt.Errorf("thinking%w", err)
			}
			if t := Thinking(v, thinking); t != nil {
				out["thinking"] = t
			}
		case "output_config":
			effort, err := outputConfig(v)
			if err != nil {
				return nil, fmt.Errorf("output_config%w", err)
			}
			if effort != nil {
				out["reasoning_effort"] = effort
			}
		default:
			return nil, fmt.Errorf("%s: has no Chat Completions counterpart", k)
		}
	}
	system, err := systemText(top["system"])
	if err != nil {
		return nil, fmt.Errorf("system%w", err)
	}
	var raws []map[string]json.RawMessage
	if null(top["messages"]) || json.Unmarshal(top["messages"], &raws) != nil {
		return nil, fmt.Errorf("messages: must be an array of objects")
	}
	turns := make([]Message, len(raws))
	for i, m := range raws {
		role, _ := text(m, "role")
		turns[i] = Message{Role: role, Content: m["content"]}
	}
	if n := len(turns); n > 0 && turns[n-1].Role == "assistant" {
		return nil, fmt.Errorf("messages[%d]: a final assistant turn, which Messages continues, has no Chat Completions counterpart", n-1)
	}
	msgs, err := Messages(system, turns)
	if err != nil {
		return nil, err
	}
	out["messages"] = encode(msgs)
	if tools, err := requestTools(top["tools"]); err != nil {
		return nil, err
	} else if tools != nil {
		out["tools"] = tools
	}
	return encode(out), nil
}

// null reports whether v is absent or JSON null, which the Messages API
// reads as a field left out.
func null(v json.RawMessage) bool {
	v = bytes.TrimSpace(v)
	return len(v) == 0 || bytes.Equal(v, []byte("null"))
}

// systemText is the system prompt as one string: a string as it is, or its
// text blocks' texts, a line apart, as Anthropic's OpenAI compatibility joins
// the system messages it hoists the other way.
func systemText(raw json.RawMessage) (string, error) {
	if null(raw) {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return "", fmt.Errorf(": must be a string or an array of text blocks")
	}
	texts := make([]string, len(blocks))
	for i, b := range blocks {
		typ, _ := text(b, "type")
		t, ok := text(b, "text")
		if typ != "text" || !ok {
			return "", fmt.Errorf("[%d]: only text blocks have a Chat Completions counterpart", i)
		}
		texts[i] = t
	}
	return strings.Join(texts, "\n"), nil
}

// requestTools converts the request's tools, refusing a server tool — one
// with a type other than custom — which no Chat Completions vendor runs. A
// tool's strict carries over: it bounds the call's input to the schema.
func requestTools(raw json.RawMessage) (json.RawMessage, error) {
	if null(raw) {
		return nil, nil
	}
	var tools []json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return nil, fmt.Errorf("tools: must be an array")
	}
	if len(tools) == 0 { // as absent, since a Chat Completions server may refuse an empty or null one
		return nil, nil
	}
	defs := make([]map[string]json.RawMessage, len(tools))
	for i, t := range tools {
		if json.Unmarshal(t, &defs[i]) != nil || defs[i] == nil {
			return nil, fmt.Errorf("tools[%d]: must be an object", i)
		}
		if typ, ok := text(defs[i], "type"); ok && typ != "custom" {
			return nil, fmt.Errorf("tools[%d]: a %q server tool has no Chat Completions counterpart", i, typ)
		} else if !ok && !null(defs[i]["type"]) {
			return nil, fmt.Errorf("tools[%d].type: must be a string", i)
		}
		keys := make([]string, 0, len(defs[i]))
		for k := range defs[i] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := defs[i][k]
			switch k {
			case "name", "description", "input_schema", "strict", "type", "cache_control", "eager_input_streaming", "input_examples":
				continue
			case "defer_loading":
				if null(v) || bytes.Equal(bytes.TrimSpace(v), []byte("false")) {
					continue
				}
			case "allowed_callers":
				var callers []string
				if null(v) || json.Unmarshal(v, &callers) == nil && slices.Contains(callers, "direct") {
					continue
				}
			}
			return nil, fmt.Errorf("tools[%d].%s: has no Chat Completions counterpart", i, k)
		}
	}
	converted, err := Tools(tools, nil)
	if err != nil {
		return nil, err
	}
	for i := range converted {
		if !null(defs[i]["strict"]) {
			converted[i].Function.Strict = defs[i]["strict"]
		}
	}
	return encode(converted), nil
}

// toolChoice is tool_choice in Chat Completions' terms — auto, required for
// any, the named function for tool, none — and parallel_tool_calls false
// for disable_parallel_tool_use true, else nil.
func toolChoice(raw json.RawMessage) (choice, parallel json.RawMessage, err error) {
	var c map[string]json.RawMessage
	if json.Unmarshal(raw, &c) != nil {
		return nil, nil, fmt.Errorf(": must be an object")
	}
	typ, _ := text(c, "type")
	switch typ {
	case "auto":
		choice = encode("auto")
	case "any":
		choice = encode("required")
	case "none":
		choice = encode("none")
	case "tool":
		name, ok := text(c, "name")
		if !ok {
			return nil, nil, fmt.Errorf(".name: must be a string")
		}
		choice = encode(map[string]any{"type": "function", "function": map[string]string{"name": name}})
	default:
		return nil, nil, fmt.Errorf(".type: %q has no Chat Completions counterpart", typ)
	}
	switch v := bytes.TrimSpace(c["disable_parallel_tool_use"]); {
	case bytes.Equal(v, []byte("true")):
		parallel = json.RawMessage("false")
	case len(v) == 0, bytes.Equal(v, []byte("false")), bytes.Equal(v, []byte("null")):
	default:
		return nil, nil, fmt.Errorf(".disable_parallel_tool_use: must be a boolean")
	}
	return choice, parallel, nil
}

// Thinking is a Messages request's thinking as a vendor's OpenAI endpoint
// takes it, {"type": the vendor's word for its type}, or nil where the
// vendor names none, or the request's thinking is absent or one Request
// refuses.
func Thinking(raw json.RawMessage, vendor map[string]string) json.RawMessage {
	if null(raw) {
		return nil
	}
	t, err := thinkingType(raw, vendor)
	if err != nil || t == "" {
		return nil
	}
	return encode(map[string]string{"type": t})
}

// thinkingType is the vendor's thinking.type for the request's thinking,
// "" where the vendor names none; budget_tokens has no counterpart on these
// endpoints and goes with the rest. A field is refused where its type does
// not take it (checked against anthropic-sdk-go v1.70.1 — message.go
// ThinkingConfigEnabledParam, ThinkingConfigAdaptiveParam and
// ThinkingConfigDisabledParam): budget_tokens beside enabled alone, display
// beside enabled and adaptive.
func thinkingType(raw json.RawMessage, vendor map[string]string) (string, error) {
	var t map[string]json.RawMessage
	if json.Unmarshal(raw, &t) != nil {
		return "", fmt.Errorf(": must be an object")
	}
	typ, _ := text(t, "type")
	switch typ {
	case "enabled", "adaptive", "disabled":
	default:
		return "", fmt.Errorf(".type: %q has no Chat Completions counterpart", typ)
	}
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	takes := map[string]bool{"type": true, "budget_tokens": typ == "enabled", "display": typ != "disabled"}
	for _, k := range keys {
		switch display, _ := text(t, k); {
		case null(t[k]), takes[k] && (k != "display" || display == "summarized"):
		default:
			return "", fmt.Errorf(".%s: has no Chat Completions counterpart", k)
		}
	}
	return vendor[typ], nil
}

// outputConfig is output_config's effort, as reasoning_effort, or nil;
// format is refused.
func outputConfig(raw json.RawMessage) (json.RawMessage, error) {
	var c map[string]json.RawMessage
	if json.Unmarshal(raw, &c) != nil {
		return nil, fmt.Errorf(": must be an object")
	}
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var effort json.RawMessage
	for _, k := range keys {
		v := c[k]
		switch {
		case null(v):
		case k == "effort":
			if _, ok := text(c, k); !ok {
				return nil, fmt.Errorf(".effort: must be a string")
			}
			effort = v
		default:
			return nil, fmt.Errorf(".%s: has no Chat Completions counterpart", k)
		}
	}
	return effort, nil
}
