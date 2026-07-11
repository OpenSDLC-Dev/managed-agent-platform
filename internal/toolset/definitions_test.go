package toolset_test

import (
	"encoding/json"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
)

// names pulls the tool names out of the model-facing definitions, in order.
func names(t *testing.T, defs []json.RawMessage) []string {
	t.Helper()
	out := make([]string, len(defs))
	for i, raw := range defs {
		var d struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatalf("definition %d: %v", i, err)
		}
		if d.Name == "" || d.Description == "" || len(d.InputSchema) == 0 {
			t.Fatalf("definition %d is incomplete: %s", i, raw)
		}
		out[i] = d.Name
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestTools(t *testing.T) {
	all := []string{"bash", "read", "write", "edit", "glob", "grep"}

	cases := []struct {
		name  string
		entry string
		want  []string
	}{
		{
			// The reference resolves default_config/configs onto every stored
			// agent; a client may still send the bare entry, and the toolset's
			// defaults (everything on) must match the resolved default.
			name:  "bare entry enables every tool",
			entry: `{"type":"agent_toolset_20260401"}`,
			want:  all,
		},
		{
			name:  "explicit default_config enabled",
			entry: `{"type":"agent_toolset_20260401","default_config":{"enabled":true}}`,
			want:  all,
		},
		{
			name:  "default off disables everything",
			entry: `{"type":"agent_toolset_20260401","default_config":{"enabled":false}}`,
			want:  nil,
		},
		{
			name: "a config overrides the default off",
			entry: `{"type":"agent_toolset_20260401","default_config":{"enabled":false},
			         "configs":[{"name":"bash","enabled":true},{"name":"read","enabled":true}]}`,
			want: []string{"bash", "read"},
		},
		{
			name: "a config overrides the default on",
			entry: `{"type":"agent_toolset_20260401","default_config":{"enabled":true},
			         "configs":[{"name":"bash","enabled":false}]}`,
			want: []string{"read", "write", "edit", "glob", "grep"},
		},
		{
			// web_fetch/web_search are named in the wire's tool-config enum but
			// carry no input schema there and execute executor-side; they are
			// deferred, so enabling one offers the model nothing.
			name:  "web tools are not offered",
			entry: `{"type":"agent_toolset_20260401","default_config":{"enabled":false},"configs":[{"name":"web_search","enabled":true}]}`,
			want:  nil,
		},
		{
			// permission_policy is slice 7's; it must not change what is offered.
			name: "permission policy does not gate the definition",
			entry: `{"type":"agent_toolset_20260401","default_config":{"enabled":true,"permission_policy":{"type":"always_ask"}},
			         "configs":[{"name":"bash","enabled":true,"permission_policy":{"type":"always_ask"}}]}`,
			want: all,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defs, err := toolset.Tools(json.RawMessage(tc.entry))
			if err != nil {
				t.Fatalf("Tools: %v", err)
			}
			if got := names(t, defs); !equal(got, tc.want) {
				t.Fatalf("tools = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestToolsRejectsMalformedEntry(t *testing.T) {
	for _, entry := range []string{
		`not json`,
		`{"type":"agent_toolset_20260401","default_config":{"enabled":"yes"}}`,
		`{"type":"agent_toolset_20260401","configs":[{"name":"bash","enabled":"yes"}]}`,
		`{"type":"agent_toolset_20260401","configs":"all"}`,
	} {
		if _, err := toolset.Tools(json.RawMessage(entry)); err == nil {
			t.Fatalf("Tools(%s) = nil error, want a rejection", entry)
		}
	}
}

// The schema the model is handed is the one the wire documents, field for
// field (anthropic-sdk-go betaagent.go, BetaManagedAgents…{Bash,Read,…}Input).
func TestToolSchemasMatchTheWire(t *testing.T) {
	want := map[string]struct {
		props    []string
		required []string
	}{
		"bash":  {props: []string{"command", "restart", "timeout_ms"}},
		"read":  {props: []string{"file_path", "view_range"}, required: []string{"file_path"}},
		"write": {props: []string{"content", "file_path"}, required: []string{"content", "file_path"}},
		"edit": {props: []string{"file_path", "new_string", "old_string", "replace_all"},
			required: []string{"file_path", "new_string", "old_string"}},
		"glob": {props: []string{"path", "pattern"}, required: []string{"pattern"}},
		"grep": {props: []string{"path", "pattern"}, required: []string{"pattern"}},
	}

	defs, err := toolset.Tools(json.RawMessage(`{"type":"agent_toolset_20260401"}`))
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	for _, raw := range defs {
		var d struct {
			Name        string `json:"name"`
			InputSchema struct {
				Type       string                     `json:"type"`
				Properties map[string]json.RawMessage `json:"properties"`
				Required   []string                   `json:"required"`
			} `json:"input_schema"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatalf("definition: %v", err)
		}
		w, ok := want[d.Name]
		if !ok {
			t.Fatalf("unexpected tool %q", d.Name)
		}
		if d.InputSchema.Type != "object" {
			t.Errorf("%s: input_schema.type = %q, want object", d.Name, d.InputSchema.Type)
		}
		var props []string
		for p := range d.InputSchema.Properties {
			props = append(props, p)
		}
		sortStrings(props)
		if !equal(props, w.props) {
			t.Errorf("%s: properties = %v, want %v", d.Name, props, w.props)
		}
		req := append([]string(nil), d.InputSchema.Required...)
		sortStrings(req)
		if !equal(req, w.required) {
			t.Errorf("%s: required = %v, want %v", d.Name, req, w.required)
		}
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
