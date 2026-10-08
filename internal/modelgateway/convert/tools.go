package convert

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// ChatTool is a Chat Completions function tool.
type ChatTool struct {
	Type     string `json:"type"`
	Function ToolFn `json:"function"`
}

// ToolFn is a function tool's definition. Strict is set by Request alone,
// from a Messages tool's own strict; Tools never sets it.
type ToolFn struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      json.RawMessage `json:"strict,omitempty"`
}

// Tools converts Anthropic tool definitions to function tools. input_schema
// becomes parameters as it came, except that a built-in's (builtin, the
// brain's provider.Request.BuiltinTools) loses the keywords
// stripSchemaKeywords removes. strict is never set: OpenAI's strict mode
// requires every object closed with additionalProperties: false and every
// field required (its structured-outputs guide), which the built-ins'
// schemas — optional fields, and additionalProperties stripped — do not meet.
func Tools(tools []json.RawMessage, builtin map[string]bool) ([]ChatTool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]ChatTool, 0, len(tools))
	for i, t := range tools {
		var def map[string]json.RawMessage
		if err := json.Unmarshal(t, &def); err != nil {
			return nil, fmt.Errorf("tools[%d]: %w", i, err)
		}
		name, _ := text(def, "name")
		if name == "" {
			return nil, fmt.Errorf("tools[%d]: missing name", i)
		}
		description, _ := text(def, "description")
		params := def["input_schema"]
		if builtin[name] {
			stripped, err := stripSchemaKeywords(params)
			if err != nil {
				return nil, fmt.Errorf("tools[%d].input_schema: %w", i, err)
			}
			params = stripped
		}
		out = append(out, ChatTool{Type: "function", Function: ToolFn{Name: name, Description: description, Parameters: params}})
	}
	return out, nil
}

// strippedKeywords leave the built-in tools' parameters on this route (#682, an
// owner decision). The built-in web tools carry all three, and since #822 the
// six sandbox tools carry additionalProperties and edit's old_string minLength,
// as the reference was recorded handing them to the model, and the anthropic
// adapter sends them on; but an OpenAI-compatible backend that accepts only
// part of JSON Schema — Gemini's compatibility endpoint and vLLM's guided
// decoding were the cases raised — can refuse the whole tool list over one of
// them, and the built-ins are on by default. What the model loses here is a
// hint, not the check: the executor validates the web tools' input, the edit
// tool refuses an empty old_string wherever it runs (executor or BYOC
// worker), and a sandbox tool refuses, naming it, a property a closed schema
// would have refused (#827). A custom or MCP tool's schema is never touched:
// it is a contract its author set, which no platform check stands behind, and
// whatever it carries it carried before #682.
// unevaluatedProperties goes with additionalProperties: it is the 2019-09
// keyword that closes an object the same way, and no built-in carries it yet.
var strippedKeywords = []string{"format", "minLength", "additionalProperties", "unevaluatedProperties"}

// stripSchemaKeywords removes strippedKeywords from a schema and from every
// subschema in it, at any depth. A schema with none of them is returned as it
// came; one that loses any is re-encoded, its numbers kept as written. Either
// way the request's encoding then compacts it, as it does every schema.
func stripSchemaKeywords(raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return raw, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var schema any
	if err := dec.Decode(&schema); err != nil {
		return nil, err
	}
	if !stripSchema(schema) {
		return raw, nil
	}
	return json.Marshal(schema)
}

// stripSchema strips one schema in place, reporting whether anything went. It
// descends through every keyword whose value is a subschema, a list of them or
// a map of them — drafts 07 to 2020-12 — and only through those, so a property
// merely named "format" survives, and so does instance data — enum, const,
// default, examples — whatever keys that data happens to hold.
func stripSchema(v any) bool {
	schema, ok := v.(map[string]any)
	if !ok {
		return false // a boolean schema, or not a schema at all
	}
	stripped := false
	for _, k := range strippedKeywords {
		if _, ok := schema[k]; ok {
			delete(schema, k)
			stripped = true
		}
	}
	for k, sub := range schema {
		switch k {
		case "properties", "patternProperties", "$defs", "definitions", "dependentSchemas", "dependencies":
			// A map from names to subschemas: the names are data. draft-07's
			// dependencies maps a name to a schema or to a list of names, and
			// a list is no schema, so stripSchema leaves it be.
			if m, ok := sub.(map[string]any); ok {
				for _, s := range m {
					stripped = stripSchema(s) || stripped
				}
			}
		case "items", "prefixItems", "additionalItems", "contains", "unevaluatedItems",
			"propertyNames", "contentSchema", "not", "if", "then", "else",
			"anyOf", "oneOf", "allOf":
			// One subschema, or a list of them (items' tuple form among them).
			if list, ok := sub.([]any); ok {
				for _, s := range list {
					stripped = stripSchema(s) || stripped
				}
				continue
			}
			stripped = stripSchema(sub) || stripped
		}
	}
	return stripped
}
