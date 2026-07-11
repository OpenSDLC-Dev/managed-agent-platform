package toolset

import (
	"encoding/json"
	"fmt"
)

// definitions are the six built-in tools in the order the reference lists them,
// each already in the Messages-API tool shape the provider request carries
// (name / description / input_schema). The schemas are the wire's, field for
// field — anthropic-sdk-go's BetaManagedAgentsAgentToolset20260401*Input types
// are what the model's tool calls are validated against on the other side, so a
// property this platform invents is a property no reference client would send.
var definitions = []toolDef{
	{
		name:        "bash",
		description: "Run a bash command in a persistent shell. State (cwd, env vars) persists across calls.",
		props: map[string]any{
			"command":    prop("string", "The command to run."),
			"restart":    prop("boolean", "Restart the persistent shell before running."),
			"timeout_ms": prop("integer", "Per-call timeout in milliseconds."),
		},
	},
	{
		name:        "read",
		description: "Read a UTF-8 text file. Relative paths resolve against the workdir.",
		props: map[string]any{
			"file_path": prop("string", "Path of the file to read."),
			"view_range": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "integer"},
				"description": "[start_line, end_line] 1-indexed inclusive. end_line of 0 or less means to end of file.",
			},
		},
		required: []string{"file_path"},
	},
	{
		name:        "write",
		description: "Write a UTF-8 text file, creating parent directories as needed. Relative paths resolve against the workdir.",
		props: map[string]any{
			"file_path": prop("string", "Path of the file to write."),
			"content":   prop("string", "Full file contents to write."),
		},
		required: []string{"file_path", "content"},
	},
	{
		name:        "edit",
		description: "Replace a unique occurrence of old_string with new_string in a file (set replace_all to replace every occurrence).",
		props: map[string]any{
			"file_path":   prop("string", "Path of the file to edit."),
			"old_string":  prop("string", "Substring to find and replace."),
			"new_string":  prop("string", "Replacement text."),
			"replace_all": prop("boolean", "Replace every occurrence instead of requiring a unique match."),
		},
		required: []string{"file_path", "old_string", "new_string"},
	},
	{
		name:        "glob",
		description: "List paths matching a glob pattern (e.g. **/*.go), newest first.",
		props: map[string]any{
			"pattern": prop("string", "Glob pattern, e.g. **/*.go (** matches any depth)."),
			"path":    prop("string", "Directory to search in. Defaults to the workdir."),
		},
		required: []string{"pattern"},
	},
	{
		name:        "grep",
		description: "Search file contents for a regular expression, returning matching lines as path:line:text.",
		props: map[string]any{
			"pattern": prop("string", "Regular expression to search for."),
			"path":    prop("string", "Directory to search in. Defaults to the workdir."),
		},
		required: []string{"pattern"},
	},
}

type toolDef struct {
	name        string
	description string
	props       map[string]any
	required    []string
}

func prop(typ, description string) map[string]any {
	return map[string]any{"type": typ, "description": description}
}

// marshal renders the definition as the provider request's tool shape.
func (d toolDef) marshal() (json.RawMessage, error) {
	schema := map[string]any{"type": "object", "properties": d.props}
	if len(d.required) > 0 {
		schema["required"] = d.required
	}
	return json.Marshal(map[string]any{
		"name": d.name, "description": d.description, "input_schema": schema,
	})
}

// entry is an agent's agent_toolset_20260401 tool, as stored on the resolved
// agent. Both config fields are optional here though the reference renders them
// on every resolved agent: this platform stores the client's tools verbatim, so
// a bare entry is what a bare entry arrives as. Omitted means the reference's
// resolved default — the toolset is on. permission_policy is read by nobody
// yet; policies and the confirmation round-trip are slice 7.
type entry struct {
	DefaultConfig *struct {
		Enabled *bool `json:"enabled"`
	} `json:"default_config"`
	Configs []struct {
		Name    string `json:"name"`
		Enabled *bool  `json:"enabled"`
	} `json:"configs"`
}

// Tools returns the model-facing definitions of the built-in tools an
// agent_toolset_20260401 entry enables, in the wire's order.
//
// web_fetch and web_search are in the wire's tool-config enum but carry no
// input schema there and run executor-side against an egress policy this
// platform has not built; enabling one offers the model nothing, and calling it
// is an error result rather than a tool call that hangs.
func Tools(raw json.RawMessage) ([]json.RawMessage, error) {
	var e entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, fmt.Errorf("agent_toolset_20260401: %w", err)
	}

	enabled := true
	if e.DefaultConfig != nil && e.DefaultConfig.Enabled != nil {
		enabled = *e.DefaultConfig.Enabled
	}
	override := make(map[string]bool, len(e.Configs))
	for _, c := range e.Configs {
		if c.Enabled != nil {
			override[c.Name] = *c.Enabled
		}
	}

	var out []json.RawMessage
	for _, d := range definitions {
		on, ok := override[d.name]
		if !ok {
			on = enabled
		}
		if !on {
			continue
		}
		def, err := d.marshal()
		if err != nil {
			return nil, err
		}
		out = append(out, def)
	}
	return out, nil
}
