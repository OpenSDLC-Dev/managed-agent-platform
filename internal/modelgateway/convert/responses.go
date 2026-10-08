package convert

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Responses is a Responses request (POST /v1/responses) converted to a
// Messages one (ResponsesRequest).
type Responses struct {
	// Messages is the Messages request. It carries no max_tokens when the
	// caller set no max_output_tokens, the gateway choosing one.
	Messages map[string]json.RawMessage
	// Echo is what a Response repeats of its request, defaults included:
	// github.com/openai/openai-go/v3 v3.73.0's Response requires
	// instructions, metadata, tools, tool_choice, parallel_tool_calls,
	// temperature and top_p.
	Echo map[string]json.RawMessage
}

// ResponsesRequest converts a Responses request to a Messages one, for a
// gateway that keeps no state: the conversation is the request's input, and
// what would need a stored response, conversation, prompt or file is
// refused, with the reason. Every top-level field
// github.com/openai/openai-go/v3 v3.73.0's ResponseNewParams names has a
// disposition, null being a field left out, and a field it does not name is
// refused:
//
//   - mapped: model; input (Messages' messages); instructions, then each
//     system and developer message in turn, a blank line apart (system);
//     max_output_tokens (max_tokens); temperature; top_p; stream; tools,
//     function tools alone; tool_choice and parallel_tool_calls false
//     (tool_choice's disable_parallel_tool_use), of a request offering
//     tools — one offering none drops both, but for a tool_choice that
//     requires a call, which its upstream refuses; reasoning.effort none
//     (thinking disabled), and any other (adaptive thinking, the effort its
//     output_config.effort, minimal as low) — asked for an effort, a
//     reasoning model reasons, and MiniMax-M3 thinks only when sent
//     adaptive (probed 2026-10-09; DeepSeek's and MiniMax's models all
//     take it), while a request with no effort leaves thinking to the
//     model — and where a Chat Completions upstream's profile names no
//     thinking toggle (openai-generic), none reaches it as nothing, as a
//     Messages request's disabled thinking does (Request), so its model
//     reasons as by default; and text.format json_schema
//     (output_config.format);
//   - echoed only: metadata, as Messages' metadata takes a user_id alone,
//     and truncation disabled, which is what the gateway does;
//   - dropped: store, as nothing is stored whatever it says; include, but
//     for logprobs, as a reasoning item always carries encrypted_content;
//     reasoning.summary and text.verbosity, ways of writing the answer a
//     Messages model has no knob for (responsesEffort has reasoning's other
//     fields); max_tool_calls, which counts built-in tool calls, and those
//     are refused; stream_options, service_tier, prompt_cache_key,
//     prompt_cache_retention, prompt_cache_options, safety_identifier and
//     user, an OpenAI account's knobs; and top_logprobs 0;
//   - refused: previous_response_id, conversation and prompt, which name
//     stored state, as does prompt_cache_options.comparison_response_id;
//     prompt_cache_options.prewarm true, which asks for no answer;
//     background true; truncation auto, which would drop input the gateway
//     cannot choose; text.format json_object, which Messages cannot ask for
//     without a schema; logprobs, which Messages does not return; and
//     context_management, moderation and access_programs, which ask of the
//     answer what a Messages model does not do.
func ResponsesRequest(top map[string]json.RawMessage) (Responses, error) {
	out := map[string]json.RawMessage{}
	echo := map[string]json.RawMessage{
		"instructions": json.RawMessage("null"), "metadata": json.RawMessage("{}"),
		"tools": json.RawMessage("[]"), "tool_choice": json.RawMessage(`"auto"`),
		"parallel_tool_calls": json.RawMessage("true"), "temperature": json.RawMessage("1"),
		"top_p": json.RawMessage("1"),
	}
	var instructions string
	var choice map[string]any
	parallel := true
	config := map[string]any{}
	for _, k := range slices.Sorted(maps.Keys(top)) {
		v := top[k]
		if null(v) {
			continue
		}
		switch k {
		case "model", "temperature", "top_p", "stream":
			out[k] = v
			if k == "temperature" || k == "top_p" {
				echo[k] = v
			}
		case "input":
		case "instructions":
			s, ok := text(top, k)
			if !ok {
				return Responses{}, fmt.Errorf("instructions: must be a string")
			}
			instructions, echo[k] = s, v
		case "max_output_tokens":
			var n int64
			if json.Unmarshal(v, &n) != nil || n < 1 {
				return Responses{}, fmt.Errorf("max_output_tokens: must be a positive integer")
			}
			out["max_tokens"], echo[k] = v, v
		case "tools":
			tools, err := responsesTools(v)
			if err != nil {
				return Responses{}, err
			}
			if len(tools) > 0 {
				out["tools"] = encode(tools)
			}
			echo[k] = v
		case "tool_choice":
			c, err := responsesToolChoice(v)
			if err != nil {
				return Responses{}, fmt.Errorf("tool_choice%w", err)
			}
			choice, echo[k] = c, v
		case "parallel_tool_calls":
			if json.Unmarshal(v, &parallel) != nil {
				return Responses{}, fmt.Errorf("parallel_tool_calls: must be a boolean")
			}
			echo[k] = v
		case "reasoning":
			effort, err := responsesEffort(v)
			if err != nil {
				return Responses{}, fmt.Errorf("reasoning%w", err)
			}
			switch effort {
			case "":
			case "none":
				out["thinking"] = encode(map[string]string{"type": "disabled"})
			default:
				out["thinking"] = encode(map[string]string{"type": "adaptive"})
				config["effort"] = effort
			}
			echo[k] = v
		case "text":
			format, err := responsesFormat(v)
			if err != nil {
				return Responses{}, fmt.Errorf("text%w", err)
			}
			if format != nil {
				config["format"] = format
			}
			echo[k] = v
		case "metadata", "truncation":
			if s, _ := text(top, k); k == "truncation" && s != "disabled" {
				return Responses{}, fmt.Errorf("truncation: only disabled; the gateway drops none of the input")
			}
			echo[k] = v
		case "include":
			var include []string
			if json.Unmarshal(v, &include) != nil {
				return Responses{}, fmt.Errorf("include: must be an array of strings")
			}
			if slices.Contains(include, "message.output_text.logprobs") {
				return Responses{}, fmt.Errorf("include: message.output_text.logprobs has no Messages counterpart")
			}
		case "top_logprobs":
			var n int64
			if json.Unmarshal(v, &n) != nil || n != 0 {
				return Responses{}, fmt.Errorf("top_logprobs: the Messages API returns no log probabilities")
			}
		case "background":
			var b bool
			if json.Unmarshal(v, &b) != nil || b {
				return Responses{}, fmt.Errorf("background: the gateway answers in the request, and runs nothing in the background")
			}
		case "previous_response_id", "conversation":
			return Responses{}, fmt.Errorf("%s: the gateway stores no response, so a request carries its whole conversation in input", k)
		case "prompt":
			return Responses{}, fmt.Errorf("prompt: the gateway stores no prompt template")
		case "prompt_cache_options":
			var opts map[string]json.RawMessage
			if json.Unmarshal(v, &opts) != nil {
				return Responses{}, fmt.Errorf("prompt_cache_options: must be an object")
			}
			var prewarm bool
			if json.Unmarshal(opts["prewarm"], &prewarm) == nil && prewarm {
				return Responses{}, fmt.Errorf("prompt_cache_options.prewarm: the gateway always generates an answer")
			}
			if !null(opts["comparison_response_id"]) {
				return Responses{}, fmt.Errorf("prompt_cache_options.comparison_response_id: the gateway stores no response")
			}
		case "store", "max_tool_calls", "stream_options", "service_tier", "prompt_cache_key",
			"prompt_cache_retention", "safety_identifier", "user":
		default:
			return Responses{}, fmt.Errorf("%s: not supported by the gateway's Responses API", k)
		}
	}
	if null(top["input"]) {
		return Responses{}, fmt.Errorf("input: Field required")
	}
	system, msgs, err := responsesInput(top["input"])
	if err != nil {
		return Responses{}, err
	}
	if instructions != "" {
		system = append([]string{instructions}, system...)
	}
	if len(system) > 0 {
		out["system"] = encode(strings.Join(system, "\n\n"))
	}
	out["messages"] = encode(msgs)
	// A request offering no tools asks nothing of none or auto, nor of
	// parallel calls; a call it requires stays, for the upstream to refuse.
	_, offered := out["tools"]
	if !offered && (choice["type"] == "none" || choice["type"] == "auto") {
		choice = nil
	}
	if !parallel && offered {
		if choice == nil {
			choice = map[string]any{"type": "auto"}
		}
		if choice["type"] != "none" {
			choice["disable_parallel_tool_use"] = true
		}
	}
	if choice != nil {
		out["tool_choice"] = encode(choice)
	}
	if len(config) > 0 {
		out["output_config"] = encode(config)
	}
	return Responses{Messages: out, Echo: echo}, nil
}

// responsesTools is tools as Messages tools: each a function tool, flat as
// the Responses API has it, its parameters the input_schema (an object
// schema with no properties when it has none, and typed object when it
// states no type) and strict carried when true.
// A built-in tool, and a field FunctionToolParam names but Messages has no
// use for, are refused.
func responsesTools(raw json.RawMessage) ([]map[string]any, error) {
	var tools []map[string]json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return nil, fmt.Errorf("tools: must be an array of objects")
	}
	out := make([]map[string]any, 0, len(tools))
	for i, t := range tools {
		if typ, _ := text(t, "type"); typ != "function" {
			return nil, fmt.Errorf("tools[%d]: a %q tool is not supported by the gateway's Responses API; only function tools are", i, typ)
		}
		name, ok := text(t, "name")
		if !ok || name == "" {
			return nil, fmt.Errorf("tools[%d].name: required", i)
		}
		tool := map[string]any{"name": name, "input_schema": json.RawMessage(`{"type":"object","properties":{}}`)}
		for _, k := range slices.Sorted(maps.Keys(t)) {
			v := t[k]
			switch {
			case k == "type", k == "name", null(v):
			case k == "description":
				d, ok := text(t, k)
				if !ok {
					return nil, fmt.Errorf("tools[%d].description: must be a string", i)
				}
				tool[k] = d
			case k == "parameters":
				// Messages requires an object schema to say so; OpenAI does not.
				var schema map[string]json.RawMessage
				if json.Unmarshal(v, &schema) == nil && schema != nil && schema["type"] == nil {
					schema["type"] = json.RawMessage(`"object"`)
					v = encode(schema)
				}
				tool["input_schema"] = v
			case k == "strict":
				var strict bool
				if json.Unmarshal(v, &strict) != nil {
					return nil, fmt.Errorf("tools[%d].strict: must be a boolean", i)
				}
				if strict {
					tool[k] = true
				}
			default:
				return nil, fmt.Errorf("tools[%d].%s: not supported by the gateway's Responses API", i, k)
			}
		}
		out = append(out, tool)
	}
	return out, nil
}

// responsesToolChoice is tool_choice as Messages' tool_choice: none, auto,
// required (any) or one named function (tool). Choosing among allowed tools,
// and a built-in tool, are refused.
func responsesToolChoice(raw json.RawMessage) (map[string]any, error) {
	var mode string
	if json.Unmarshal(raw, &mode) == nil {
		switch mode {
		case "none", "auto":
			return map[string]any{"type": mode}, nil
		case "required":
			return map[string]any{"type": "any"}, nil
		}
		return nil, fmt.Errorf(": %q is not none, auto or required", mode)
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil, fmt.Errorf(": must be a string or an object")
	}
	if typ, _ := text(obj, "type"); typ != "function" {
		return nil, fmt.Errorf(".type: %q is not supported by the gateway's Responses API; only function is", typ)
	}
	name, ok := text(obj, "name")
	if !ok || name == "" {
		return nil, fmt.Errorf(".name: required")
	}
	return map[string]any{"type": "tool", "name": name}, nil
}

// responsesEffort is reasoning.effort as Messages' output_config.effort,
// minimal as low, which is Messages' least; none stays none, which turns
// thinking off; "" is no effort asked. Of reasoning's other fields, summary
// and generate_summary are dropped, as a Messages model writes no summary;
// context auto and all_turns and mode standard are what the gateway does —
// every reasoning item the input carries goes back to the model, provenance
// permitting — and any other context or mode, which would ask for less, or
// for another way of reasoning, is refused.
func responsesEffort(raw json.RawMessage) (string, error) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return "", fmt.Errorf(": must be an object")
	}
	for _, k := range slices.Sorted(maps.Keys(obj)) {
		if null(obj[k]) {
			continue
		}
		v, _ := text(obj, k)
		switch k {
		case "effort", "summary", "generate_summary":
		case "context":
			if v != "auto" && v != "all_turns" {
				return "", fmt.Errorf(".context: %q is not supported: the gateway sends the model every reasoning item the input carries, so leave out those it should not see", v)
			}
		case "mode":
			if v != "standard" {
				return "", fmt.Errorf(".mode: %q is not supported by the gateway's Responses API; only standard is", v)
			}
		default:
			return "", fmt.Errorf(".%s: not supported by the gateway's Responses API", k)
		}
	}
	if null(obj["effort"]) {
		return "", nil
	}
	effort, _ := text(obj, "effort")
	switch effort {
	case "minimal":
		return "low", nil
	case "none", "low", "medium", "high", "xhigh", "max":
		return effort, nil
	}
	return "", fmt.Errorf(".effort: %q is not one of none, minimal, low, medium, high, xhigh and max", effort)
}

// responsesFormat is text.format as Messages' output_config.format, nil for
// plain text. A json_schema format is flat in the Responses API, its schema
// beside its name; json_object, which asks for JSON with no schema, has no
// Messages counterpart.
func responsesFormat(raw json.RawMessage) (map[string]any, error) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil, fmt.Errorf(": must be an object")
	}
	if null(obj["format"]) {
		return nil, nil
	}
	var format map[string]json.RawMessage
	if json.Unmarshal(obj["format"], &format) != nil {
		return nil, fmt.Errorf(".format: must be an object")
	}
	switch typ, _ := text(format, "type"); typ {
	case "text":
		return nil, nil
	case "json_schema":
		if null(format["schema"]) {
			return nil, fmt.Errorf(".format.schema: required")
		}
		return map[string]any{"type": "json_schema", "schema": format["schema"]}, nil
	case "json_object":
		return nil, fmt.Errorf(".format: json_object has no Messages counterpart; ask with json_schema")
	default:
		return nil, fmt.Errorf(".format.type: %q is not text or json_schema", typ)
	}
}

// turn is a Messages message as input builds it; results counts the
// tool_result blocks that lead a user turn's content, as Messages requires,
// and from is the Response an assistant turn's latest item names by its id
// (responseOf).
type turn struct {
	Role    string `json:"role"`
	Content []any  `json:"content"`
	results int
	from    string
}

// responseOf is the Response an output item's id names, as the gateway
// writes one (ResponseMeta.itemID): msg, rs or fc, the Response's id —
// the gateway's request id, 24 characters of the platform's id alphabet
// (newRequestID in internal/modelgateway) — and the item's index. An id of
// any other form, a caller's own among them, names none: "".
func responseOf(id string) string {
	parts := strings.Split(id, "_")
	if len(parts) != 3 || parts[0] != "msg" && parts[0] != "rs" && parts[0] != "fc" ||
		len(parts[1]) != 24 || strings.Trim(parts[1], "0123456789abcdefghjkmnpqrstvwxyz") != "" {
		return ""
	}
	if parts[2] == "" || strings.Trim(parts[2], "0123456789") != "" { // a decimal index, unsigned
		return ""
	}
	return parts[1]
}

// responsesInput is input as Messages' system texts and messages. A string
// is one user message. Items become blocks, each joining the message before
// it when the roles agree: a user message's text and images and a
// function_call_output's tool_result, which leads its message; an assistant
// message's text, a reasoning item's thinking block and a function_call's
// tool_use. A system or developer message is system text, as Messages takes
// none among its messages.
//
// A client may record each call beside its output, where a Response lists
// a turn's calls together: a function_call whose id names the Response the
// assistant turn before a user turn of nothing but tool results came from
// joins that turn, its output that user turn, so the turn keeps the
// thinking it began with, which Messages requires of a tool loop's last
// one. A call whose id names another Response, or that has no id the
// gateway wrote, is a turn of its own.
//
// A reasoning item is one the gateway answered, read back: its
// encrypted_content is the thinking block's signature when the item has a
// summary, the thinking its text, and a redacted_thinking block's data when
// it has none (responsesItem). One with no encrypted_content carries no
// signature to send, and is left out; the gateway's provenance check
// (internal/modelgateway) judges the rest.
func responsesInput(raw json.RawMessage) ([]string, []*turn, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return nil, []*turn{{Role: "user", Content: []any{textBlock(s)}}}, nil
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil, nil, fmt.Errorf("input: must be a string or an array of objects")
	}
	var system []string
	msgs := []*turn{}
	add := func(role string, block any, result bool, from string) {
		if len(msgs) == 0 || msgs[len(msgs)-1].Role != role {
			msgs = append(msgs, &turn{Role: role})
		}
		t := msgs[len(msgs)-1]
		t.from = from
		if result {
			t.Content = slices.Insert(t.Content, t.results, block)
			t.results++
			return
		}
		t.Content = append(t.Content, block)
	}
	for i, it := range items {
		typ, _ := text(it, "type")
		id, _ := text(it, "id")
		if _, ok := it["role"]; ok && typ == "" {
			typ = "message"
		}
		switch typ {
		case "message":
			role, _ := text(it, "role")
			blocks, err := messageBlocks(it, role)
			if err != nil {
				return nil, nil, fmt.Errorf("input[%d]%w", i, err)
			}
			for _, b := range blocks {
				if role == "system" || role == "developer" {
					system = append(system, b["text"].(string))
					continue
				}
				add(role, b, false, responseOf(id))
			}
		case "function_call":
			b, err := toolUse(it)
			if err != nil {
				return nil, nil, fmt.Errorf("input[%d]%w", i, err)
			}
			// The last turn nothing but tool results — a user turn — and the
			// one before it from the call's Response.
			if n := len(msgs); n >= 2 && responseOf(id) != "" && msgs[n-2].from == responseOf(id) &&
				msgs[n-1].results == len(msgs[n-1].Content) {
				msgs[n-2].Content = append(msgs[n-2].Content, b)
				continue
			}
			add("assistant", b, false, responseOf(id))
		case "function_call_output":
			b, err := toolResult(it)
			if err != nil {
				return nil, nil, fmt.Errorf("input[%d]%w", i, err)
			}
			add("user", b, true, "")
		case "reasoning":
			b, err := thinkingBlock(it)
			if err != nil {
				return nil, nil, fmt.Errorf("input[%d]%w", i, err)
			}
			if b != nil {
				add("assistant", b, false, responseOf(id))
			}
		case "item_reference":
			return nil, nil, fmt.Errorf("input[%d]: an item_reference names a stored item, and the gateway stores none", i)
		default:
			return nil, nil, fmt.Errorf("input[%d]: a %q item is not supported by the gateway's Responses API", i, typ)
		}
	}
	return system, msgs, nil
}

func textBlock(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

// messageBlocks is a message's content as Messages blocks, an empty text
// left out, as Messages refuses one: text for every role, and an image in a
// user message. A system or developer message takes text alone. A part's
// fields a block has no place for — an image's detail, an output_text's
// annotations and logprobs — are dropped.
func messageBlocks(it map[string]json.RawMessage, role string) ([]map[string]any, error) {
	switch role {
	case "user", "assistant", "system", "developer":
	default:
		return nil, fmt.Errorf(".role: %q is not user, assistant, system or developer", role)
	}
	if s, ok := text(it, "content"); ok {
		if s == "" {
			return nil, nil
		}
		return []map[string]any{textBlock(s)}, nil
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(it["content"], &parts) != nil {
		return nil, fmt.Errorf(".content: must be a string or an array of objects")
	}
	var out []map[string]any
	for j, p := range parts {
		typ, _ := text(p, "type")
		switch {
		case typ == "input_text" || typ == "output_text" || typ == "refusal" && role == "assistant":
			key := "text"
			if typ == "refusal" {
				key = "refusal"
			}
			s, ok := text(p, key)
			if !ok {
				return nil, fmt.Errorf(".content[%d].%s: must be a string", j, key)
			}
			if s != "" {
				out = append(out, textBlock(s))
			}
		case typ == "input_image" && role == "user":
			b, err := imageBlock(p)
			if err != nil {
				return nil, fmt.Errorf(".content[%d]%w", j, err)
			}
			out = append(out, b)
		default:
			return nil, fmt.Errorf(".content[%d]: an %q part in a %s message is not supported by the gateway's Responses API", j, typ, role)
		}
	}
	return out, nil
}

// imageBlock is an input_image as a Messages image: a data: URL as a base64
// source, an http(s) URL as a url source. A file_id names a stored file.
func imageBlock(p map[string]json.RawMessage) (map[string]any, error) {
	if !null(p["file_id"]) {
		return nil, fmt.Errorf(".file_id: the gateway stores no files; send image_url")
	}
	u, ok := text(p, "image_url")
	if !ok {
		return nil, fmt.Errorf(".image_url: required")
	}
	if rest, ok := strings.CutPrefix(u, "data:"); ok {
		media, data, ok := strings.Cut(rest, ";base64,")
		if !ok || media == "" {
			return nil, fmt.Errorf(".image_url: a data URL must be base64, with a media type")
		}
		return map[string]any{"type": "image", "source": map[string]string{"type": "base64", "media_type": media, "data": data}}, nil
	}
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return nil, fmt.Errorf(".image_url: must be an http(s) or a data URL")
	}
	return map[string]any{"type": "image", "source": map[string]string{"type": "url", "url": u}}, nil
}

// toolUse is a function_call as a tool_use block, its id the call_id a
// function_call_output answers and its input the arguments, which must be a
// JSON object; empty arguments are an empty object.
func toolUse(it map[string]json.RawMessage) (map[string]any, error) {
	id, ok := text(it, "call_id")
	if !ok || id == "" {
		return nil, fmt.Errorf(".call_id: required")
	}
	name, ok := text(it, "name")
	if !ok || name == "" {
		return nil, fmt.Errorf(".name: required")
	}
	args, _ := text(it, "arguments")
	input := json.RawMessage("{}")
	if strings.TrimSpace(args) != "" {
		var obj map[string]json.RawMessage
		if json.Unmarshal([]byte(args), &obj) != nil || obj == nil {
			return nil, fmt.Errorf(".arguments: must be a JSON object")
		}
		input = json.RawMessage(args)
	}
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}, nil
}

// toolResult is a function_call_output as a tool_result block: a string
// output as its content, and a list's text and images as content blocks.
func toolResult(it map[string]json.RawMessage) (map[string]any, error) {
	id, ok := text(it, "call_id")
	if !ok || id == "" {
		return nil, fmt.Errorf(".call_id: required")
	}
	out := map[string]any{"type": "tool_result", "tool_use_id": id}
	if s, ok := text(it, "output"); ok {
		out["content"] = s
		return out, nil
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(it["output"], &parts) != nil {
		return nil, fmt.Errorf(".output: must be a string or an array of objects")
	}
	content := []any{}
	for j, p := range parts {
		switch typ, _ := text(p, "type"); typ {
		case "input_text":
			s, ok := text(p, "text")
			if !ok {
				return nil, fmt.Errorf(".output[%d].text: must be a string", j)
			}
			if s != "" {
				content = append(content, textBlock(s))
			}
		case "input_image":
			b, err := imageBlock(p)
			if err != nil {
				return nil, fmt.Errorf(".output[%d]%w", j, err)
			}
			content = append(content, b)
		default:
			return nil, fmt.Errorf(".output[%d]: an %q part is not supported by the gateway's Responses API", j, typ)
		}
	}
	out["content"] = content
	return out, nil
}

// thinkingBlock is a reasoning item as the thinking block it was
// (responsesInput), nil for one with no encrypted_content.
func thinkingBlock(it map[string]json.RawMessage) (map[string]any, error) {
	enc, _ := text(it, "encrypted_content")
	if enc == "" {
		return nil, nil
	}
	var summary []map[string]json.RawMessage
	if !null(it["summary"]) && json.Unmarshal(it["summary"], &summary) != nil {
		return nil, fmt.Errorf(".summary: must be an array of objects")
	}
	if len(summary) == 0 {
		return map[string]any{"type": "redacted_thinking", "data": enc}, nil
	}
	var b strings.Builder
	for j, p := range summary {
		s, ok := text(p, "text")
		if !ok {
			return nil, fmt.Errorf(".summary[%d].text: must be a string", j)
		}
		b.WriteString(s)
	}
	return map[string]any{"type": "thinking", "thinking": b.String(), "signature": enc}, nil
}
