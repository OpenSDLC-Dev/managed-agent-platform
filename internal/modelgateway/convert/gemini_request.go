package convert

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// GeminiSignaturePrefix opens the signature of every thinking block a Gemini
// answer becomes (GeminiAnswer): after it, the signature of the answer's first
// functionCall, or nothing (docs/plan/62_gemini-upstream-protocol.md,
// decision 6). Base64 holds no colon, so no Anthropic signature starts so,
// and an OpenAI one is empty: the prefix tells the gateway's thinking
// provenance which protocol produced a block.
const GeminiSignaturePrefix = "gemini:"

// skipSignature stands in for a function call's signature that no thinking
// block carries back. Gemini refuses a tool turn replayed without one
// ("Function call is missing a thought_signature"), and accepts this value in
// its place (measured 2026-10-10; bifrost sends it for history Gemini did not
// produce). A signature is a bytes field, which Gemini base64-decodes; this
// value decodes as URL-safe base64.
const skipSignature = "skip_thought_signature_validator"

// geminiContent is one turn of a generateContent request or answer. While a
// request is converted, signature is the Gemini signature the turn's thinking
// carries back, for its first call.
type geminiContent struct {
	Role      string       `json:"role,omitempty"`
	Parts     []geminiPart `json:"parts"`
	signature string
}

// geminiPart is one part of a turn: text, an inline image, a function call or
// a function's response, with the signature a model turn's parts carry, and
// the mark an answer's thought summaries carry.
type geminiPart struct {
	Text             string        `json:"text,omitempty"`
	Thought          bool          `json:"thought,omitempty"`
	InlineData       *geminiBlob   `json:"inlineData,omitempty"`
	FunctionCall     *geminiCall   `json:"functionCall,omitempty"`
	FunctionResponse *geminiResult `json:"functionResponse,omitempty"`
	ThoughtSignature string        `json:"thoughtSignature,omitempty"`
}

type geminiBlob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

// geminiResult is a functionResponse: the call it answers, by id and name,
// its text as {"output": …} or {"error": …}, and its images as parts.
type geminiResult struct {
	ID       string            `json:"id,omitempty"`
	Name     string            `json:"name"`
	Response map[string]string `json:"response"`
	Parts    []geminiPart      `json:"parts,omitempty"`
}

// GeminiRequest converts a Messages request body to a generateContent one;
// the model goes in the URL, and so does streaming. Every top-level field the
// Messages API takes at anthropic-sdk-go's pin has a disposition here, and a
// field it does not name is refused, as Request refuses one:
//
//   - mapped: system (systemInstruction, a part per text block), messages
//     (contents), tools (functionDeclarations, input_schema unchanged as
//     parametersJsonSchema), tool_choice (functionCallingConfig, sent only
//     beside a declaration: auto AUTO, any ANY, tool ANY naming it, none
//     NONE), and into generationConfig max_tokens, temperature, top_p, top_k
//     and stop_sequences, and thinking and output_config.effort as
//     thinkingConfig (geminiThinking);
//   - dropped: stream, which the URL carries; cache_control, as Gemini
//     caches a prompt's prefix on its own; metadata, service_tier,
//     inference_geo and container, as Request drops them;
//   - refused: output_config.format; tool_choice's disable_parallel_tool_use
//     true, as Gemini has no switch to hold an answer to one call; a server
//     tool, and a tool that is strict — Gemini bounds no call's input to its
//     schema — or defer_loading, or not callable directly; and a final
//     assistant turn, which Gemini would answer rather than continue.
//
// In the turns, a user turn is user and an assistant turn model; consecutive
// turns of one role are one turn, as the Messages API reads them. Text is
// text, an empty one sent as nothing; a base64 image is inlineData, and every
// other source and block is refused — a document, a search_result outside a
// tool_result, a server tool's. A tool_use is a functionCall under its id; a
// tool_result a functionResponse naming the tool its tool_use called, its
// text the response's output, or its error under is_error, its images the
// response's parts, and a search_result in it the text
// provider.SearchResultText renders. A thinking block carries back only the
// Gemini signature it may hold, onto the turn's first functionCall, where a
// call with none carries skipSignature; its text goes nowhere, nor does
// another protocol's thinking or redacted_thinking.
//
// The error names the field as the caller wrote it.
func GeminiRequest(top map[string]json.RawMessage) ([]byte, error) {
	if null(top["max_tokens"]) {
		return nil, fmt.Errorf("max_tokens: required")
	}
	out := map[string]any{}
	gen := map[string]json.RawMessage{}
	var choice map[string]any
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := top[k]
		if null(v) {
			continue
		}
		switch k {
		case "model", "messages", "system", "tools", "thinking", "output_config", "stream", "cache_control", "metadata", "service_tier", "inference_geo", "container":
		case "max_tokens":
			gen["maxOutputTokens"] = v
		case "temperature":
			gen["temperature"] = v
		case "top_p":
			gen["topP"] = v
		case "top_k":
			gen["topK"] = v
		case "stop_sequences":
			gen["stopSequences"] = v
		case "tool_choice":
			var err error
			if choice, err = geminiToolChoice(v); err != nil {
				return nil, fmt.Errorf("tool_choice%w", err)
			}
		default:
			return nil, fmt.Errorf("%s: has no Gemini counterpart", k)
		}
	}
	effort, err := geminiEffort(top["output_config"])
	if err != nil {
		return nil, fmt.Errorf("output_config%w", err)
	}
	thinking, err := geminiThinking(top["thinking"], effort)
	if err != nil {
		return nil, fmt.Errorf("thinking%w", err)
	}
	if thinking != nil {
		gen["thinkingConfig"] = encode(thinking)
	}
	out["generationConfig"] = gen
	system, err := geminiSystem(top["system"])
	if err != nil {
		return nil, fmt.Errorf("system%w", err)
	}
	if system != nil {
		out["systemInstruction"] = system
	}
	contents, err := geminiContents(top["messages"])
	if err != nil {
		return nil, err
	}
	out["contents"] = contents
	decls, err := geminiTools(top["tools"])
	if err != nil {
		return nil, err
	}
	if len(decls) > 0 {
		out["tools"] = []map[string]any{{"functionDeclarations": decls}}
		if choice != nil {
			out["toolConfig"] = map[string]any{"functionCallingConfig": choice}
		}
	}
	return encode(out), nil
}

// geminiSystem is the system prompt as a systemInstruction: a string as one
// part, an array of text blocks as a part each; nil for none.
func geminiSystem(raw json.RawMessage) (*geminiContent, error) {
	if null(raw) {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" { // a part needs a field set, and an empty text sets none
			return nil, nil
		}
		return &geminiContent{Parts: []geminiPart{{Text: s}}}, nil
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return nil, fmt.Errorf(": must be a string or an array of text blocks")
	}
	sys := &geminiContent{}
	for i, b := range blocks {
		typ, _ := text(b, "type")
		t, ok := text(b, "text")
		if typ != "text" || !ok {
			return nil, fmt.Errorf("[%d]: only text blocks have a Gemini counterpart", i)
		}
		if t != "" {
			sys.Parts = append(sys.Parts, geminiPart{Text: t})
		}
	}
	if len(sys.Parts) == 0 {
		return nil, nil
	}
	return sys, nil
}

// geminiContents converts the conversation, joining consecutive turns of one
// role, and those a turn contributing no part leaves adjacent. A joined
// turn's signature is the first its messages carry, set on its first call;
// Gemini signs a turn's first call alone, and checks that one, so a call that
// leads a turn with none carries skipSignature.
func geminiContents(raw json.RawMessage) ([]geminiContent, error) {
	var msgs []map[string]json.RawMessage
	if null(raw) || json.Unmarshal(raw, &msgs) != nil {
		return nil, fmt.Errorf("messages: must be an array of objects")
	}
	if n := len(msgs); n > 0 {
		if role, _ := text(msgs[n-1], "role"); role == "assistant" {
			return nil, fmt.Errorf("messages[%d]: a final assistant turn, which Messages continues, has no Gemini counterpart", n-1)
		}
	}
	called := map[string]string{} // each tool_use id so far, to the tool it called
	out := []geminiContent{}
	for i, m := range msgs {
		role, _ := text(m, "role")
		switch role {
		case "user":
		case "assistant":
			role = "model"
		default:
			return nil, fmt.Errorf("messages[%d].role: %q has no Gemini counterpart", i, role)
		}
		parts, signature, err := geminiParts(m["content"], called)
		if err != nil {
			return nil, fmt.Errorf("messages[%d].%w", i, err)
		}
		out = joined(out, geminiContent{Role: role, Parts: parts, signature: signature})
	}
	// A message contributing no part — thinking alone, or empty — has given
	// its signature to its turn; dropped now, it may leave two turns of one
	// role adjacent.
	kept := out[:0:0]
	for _, c := range out {
		if len(c.Parts) > 0 {
			kept = joined(kept, c)
		}
	}
	if n := len(kept); n > 0 && kept[n-1].Role == "model" {
		return nil, fmt.Errorf("messages: the user turns after the last assistant turn hold nothing Gemini is sent, which leaves a final assistant turn, and that has no Gemini counterpart")
	}
	for _, c := range kept {
		for k := range c.Parts {
			if c.Parts[k].FunctionCall != nil {
				c.Parts[k].ThoughtSignature = cmp.Or(c.signature, skipSignature)
				break
			}
		}
	}
	return kept, nil
}

// joined appends c to turns, or joins it to the last turn when that is of
// c's role, which keeps the first signature either carries.
func joined(turns []geminiContent, c geminiContent) []geminiContent {
	n := len(turns)
	if n == 0 || turns[n-1].Role != c.Role {
		return append(turns, c)
	}
	turns[n-1].Parts = append(turns[n-1].Parts, c.Parts...)
	turns[n-1].signature = cmp.Or(turns[n-1].signature, c.signature)
	return turns
}

// geminiParts converts one message's content, and returns the Gemini
// signature its first thinking block to carry one holds, recording each
// tool_use it holds in called.
func geminiParts(raw json.RawMessage, called map[string]string) ([]geminiPart, string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, "", fmt.Errorf("content: %w", err)
		}
		if s == "" {
			return nil, "", nil
		}
		return []geminiPart{{Text: s}}, "", nil
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return nil, "", fmt.Errorf("content: must be a string or an array of blocks")
	}
	var (
		parts     []geminiPart
		signature string // the Gemini signature a thinking block carries back
	)
	for j, b := range blocks {
		typ, _ := text(b, "type")
		switch typ {
		case "text":
			t, ok := text(b, "text")
			if _, present := b["text"]; !ok && present {
				return nil, "", fmt.Errorf("content[%d].text: must be a string", j)
			}
			if t != "" {
				parts = append(parts, geminiPart{Text: t})
			}
		case "image":
			blob, err := geminiImage(b["source"])
			if err != nil {
				return nil, "", fmt.Errorf("content[%d].source: %w", j, err)
			}
			parts = append(parts, geminiPart{InlineData: blob})
		case "tool_use":
			id, okID := text(b, "id")
			name, okName := text(b, "name")
			switch {
			case !okID || id == "":
				return nil, "", fmt.Errorf("content[%d].id: must be a non-empty string", j)
			case !okName || name == "":
				return nil, "", fmt.Errorf("content[%d].name: must be a non-empty string", j)
			}
			args, err := geminiArgs(b["input"])
			if err != nil {
				return nil, "", fmt.Errorf("content[%d].input: %w", j, err)
			}
			called[id] = name
			parts = append(parts, geminiPart{FunctionCall: &geminiCall{ID: id, Name: name, Args: args}})
		case "tool_result":
			id, _ := text(b, "tool_use_id")
			name, ok := called[id]
			if !ok {
				return nil, "", fmt.Errorf("content[%d].tool_use_id: %q answers no earlier tool_use", j, id)
			}
			output, images, err := geminiResultContent(b["content"])
			if err != nil {
				return nil, "", fmt.Errorf("content[%d].content%w", j, err)
			}
			key := "output"
			if bytes.Equal(bytes.TrimSpace(b["is_error"]), []byte("true")) {
				key = "error"
			}
			parts = append(parts, geminiPart{FunctionResponse: &geminiResult{ID: id, Name: name,
				Response: map[string]string{key: output}, Parts: images}})
		case "thinking":
			if sig, _ := text(b, "signature"); signature == "" && strings.HasPrefix(sig, GeminiSignaturePrefix) {
				signature = strings.TrimPrefix(sig, GeminiSignaturePrefix)
			}
		case "redacted_thinking":
		default:
			return nil, "", fmt.Errorf("content[%d]: a %q block has no Gemini counterpart", j, typ)
		}
	}
	return parts, signature, nil
}

// geminiImage is a base64 image source as inline data; Gemini fetches no URL
// a caller names.
func geminiImage(raw json.RawMessage) (*geminiBlob, error) {
	var src map[string]json.RawMessage
	if json.Unmarshal(raw, &src) != nil {
		return nil, fmt.Errorf("must be an object")
	}
	typ, _ := text(src, "type")
	if typ != "base64" {
		return nil, fmt.Errorf("a %q source has no Gemini counterpart", typ)
	}
	media, _ := text(src, "media_type")
	data, ok := text(src, "data")
	if media == "" || !ok {
		return nil, fmt.Errorf("a base64 source needs media_type and data")
	}
	return &geminiBlob{MimeType: media, Data: data}, nil
}

// geminiArgs is a tool_use block's input as a functionCall's args, an object,
// which an input left out is.
func geminiArgs(raw json.RawMessage) (json.RawMessage, error) {
	if null(raw) {
		return json.RawMessage("{}"), nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil, fmt.Errorf("must be an object")
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// geminiResultContent is a tool_result's content as a functionResponse takes
// it: its text, joined as resultText joins it for a tool message, and its
// images as parts.
func geminiResultContent(raw json.RawMessage) (string, []geminiPart, error) {
	raw = bytes.TrimSpace(raw)
	if null(raw) {
		return "", nil, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", nil, fmt.Errorf(": %w", err)
		}
		return s, nil, nil
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return "", nil, fmt.Errorf(": must be a string or an array of blocks")
	}
	var (
		out    []string
		images []geminiPart
	)
	for j, b := range blocks {
		switch typ, _ := text(b, "type"); typ {
		case "text":
			t, ok := text(b, "text")
			if _, present := b["text"]; !ok && present {
				return "", nil, fmt.Errorf("[%d].text: must be a string", j)
			}
			out = append(out, t)
		case "search_result":
			title, _ := text(b, "title")
			flat, err := provider.SearchResultText(title, b["source"], b["content"])
			if err != nil {
				return "", nil, fmt.Errorf("[%d]: %w", j, err)
			}
			// As resultText: the flat form is line-shaped.
			if n := len(out); n > 0 && !strings.HasSuffix(out[n-1], "\n") {
				out = append(out, "\n")
			}
			out = append(out, flat)
		case "image":
			blob, err := geminiImage(b["source"])
			if err != nil {
				return "", nil, fmt.Errorf("[%d].source: %w", j, err)
			}
			images = append(images, geminiPart{InlineData: blob})
		default:
			return "", nil, fmt.Errorf("[%d]: a %q block has no Gemini counterpart", j, typ)
		}
	}
	return strings.Join(out, ""), images, nil
}

// geminiTools is the request's tools as function declarations, refusing what
// a function declaration cannot carry.
func geminiTools(raw json.RawMessage) ([]map[string]any, error) {
	if null(raw) {
		return nil, nil
	}
	var tools []map[string]json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return nil, fmt.Errorf("tools: must be an array of objects")
	}
	decls := make([]map[string]any, 0, len(tools))
	for i, def := range tools {
		if typ, ok := text(def, "type"); ok && typ != "custom" {
			return nil, fmt.Errorf("tools[%d]: a %q server tool has no Gemini counterpart", i, typ)
		} else if !ok && !null(def["type"]) {
			return nil, fmt.Errorf("tools[%d].type: must be a string", i)
		}
		keys := make([]string, 0, len(def))
		for k := range def {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := def[k]
			switch k {
			case "name", "description", "input_schema", "type", "cache_control", "eager_input_streaming", "input_examples":
				continue
			case "strict", "defer_loading":
				if null(v) || bytes.Equal(bytes.TrimSpace(v), []byte("false")) {
					continue
				}
			case "allowed_callers":
				var callers []string
				if null(v) || json.Unmarshal(v, &callers) == nil && slices.Contains(callers, "direct") {
					continue
				}
			}
			return nil, fmt.Errorf("tools[%d].%s: has no Gemini counterpart", i, k)
		}
		name, ok := text(def, "name")
		if !ok || name == "" {
			return nil, fmt.Errorf("tools[%d].name: must be a non-empty string", i)
		}
		decl := map[string]any{"name": name}
		if d, _ := text(def, "description"); d != "" {
			decl["description"] = d
		}
		if !null(def["input_schema"]) {
			decl["parametersJsonSchema"] = def["input_schema"]
		}
		decls = append(decls, decl)
	}
	return decls, nil
}

// geminiToolChoice is tool_choice as a functionCallingConfig. Its
// disable_parallel_tool_use may not be true: Gemini has no switch to hold an
// answer to one call, and an answer that calls several where the caller
// allowed one is what the gateway sends a request elsewhere rather than risk
// (profile.go, deepseekIgnores).
func geminiToolChoice(raw json.RawMessage) (map[string]any, error) {
	var c map[string]json.RawMessage
	if json.Unmarshal(raw, &c) != nil {
		return nil, fmt.Errorf(": must be an object")
	}
	switch v := bytes.TrimSpace(c["disable_parallel_tool_use"]); {
	case len(v) == 0, bytes.Equal(v, []byte("false")), bytes.Equal(v, []byte("null")):
	case bytes.Equal(v, []byte("true")):
		return nil, fmt.Errorf(".disable_parallel_tool_use: true has no Gemini counterpart, as Gemini may answer with several calls")
	default:
		return nil, fmt.Errorf(".disable_parallel_tool_use: must be a boolean")
	}
	typ, _ := text(c, "type")
	switch typ {
	case "auto":
		return map[string]any{"mode": "AUTO"}, nil
	case "any":
		return map[string]any{"mode": "ANY"}, nil
	case "none":
		return map[string]any{"mode": "NONE"}, nil
	case "tool":
		name, ok := text(c, "name")
		if !ok {
			return nil, fmt.Errorf(".name: must be a string")
		}
		return map[string]any{"mode": "ANY", "allowedFunctionNames": []string{name}}, nil
	}
	return nil, fmt.Errorf(".type: %q has no Gemini counterpart", typ)
}

// geminiLevels is the thinkingLevel each effort becomes, in
// google.golang.org/genai 1.73.0's ThinkingLevel values; Gemini's highest is
// HIGH.
var geminiLevels = map[string]string{"low": "LOW", "medium": "MEDIUM", "high": "HIGH", "xhigh": "HIGH", "max": "HIGH"}

// geminiEffort is output_config's effort as a thinkingLevel, or "" for none;
// format is refused.
func geminiEffort(raw json.RawMessage) (string, error) {
	if null(raw) {
		return "", nil
	}
	var c map[string]json.RawMessage
	if json.Unmarshal(raw, &c) != nil {
		return "", fmt.Errorf(": must be an object")
	}
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var level string
	for _, k := range keys {
		switch {
		case null(c[k]):
		case k == "effort":
			effort, _ := text(c, k)
			var ok bool
			if level, ok = geminiLevels[effort]; !ok {
				return "", fmt.Errorf(".effort: %s has no Gemini counterpart", c[k])
			}
		default:
			return "", fmt.Errorf(".%s: has no Gemini counterpart", k)
		}
	}
	return level, nil
}

// geminiThinking is the request's thinking, and the thinkingLevel its effort
// became, as a thinkingConfig, or nil when it sets nothing: budget_tokens
// beside enabled as thinkingBudget, else the level, which Gemini refuses
// together with a budget; includeThoughts for enabled and adaptive, unless
// display is omitted. Disabled sets nothing of its own, as Gemini 3 thinks
// regardless. A field is refused where its type does not take it, as
// thinkingType refuses one.
func geminiThinking(raw json.RawMessage, level string) (map[string]any, error) {
	cfg := map[string]any{}
	if !null(raw) {
		var t map[string]json.RawMessage
		if json.Unmarshal(raw, &t) != nil {
			return nil, fmt.Errorf(": must be an object")
		}
		typ, _ := text(t, "type")
		switch typ {
		case "enabled", "adaptive", "disabled":
		default:
			return nil, fmt.Errorf(".type: %q has no Gemini counterpart", typ)
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		takes := map[string]bool{"type": true, "budget_tokens": typ == "enabled", "display": typ != "disabled"}
		for _, k := range keys {
			switch display, _ := text(t, k); {
			case null(t[k]), takes[k] && (k != "display" || display == "summarized" || display == "omitted"):
			default:
				return nil, fmt.Errorf(".%s: has no Gemini counterpart", k)
			}
		}
		if display, _ := text(t, "display"); typ != "disabled" && display != "omitted" {
			cfg["includeThoughts"] = true
		}
		if typ == "enabled" && !null(t["budget_tokens"]) {
			cfg["thinkingBudget"] = t["budget_tokens"]
			level = ""
		}
	}
	if level != "" {
		cfg["thinkingLevel"] = level
	}
	if len(cfg) == 0 {
		return nil, nil
	}
	return cfg, nil
}
