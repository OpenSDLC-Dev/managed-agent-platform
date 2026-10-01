package toolset_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

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
	all := []string{"bash", "read", "write", "edit", "glob", "grep", "web_fetch", "web_search"}

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
			want: []string{"read", "write", "edit", "glob", "grep", "web_fetch", "web_search"},
		},
		{
			// web_fetch/web_search execute executor-side (plan 15), and since
			// slice 2 they resolve like any other built-in.
			name:  "a web tool is offered when enabled",
			entry: `{"type":"agent_toolset_20260401","default_config":{"enabled":false},"configs":[{"name":"web_search","enabled":true}]}`,
			want:  []string{"web_search"},
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
			defs, err := toolset.Tools(json.RawMessage(tc.entry), time.Now())
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
		if _, err := toolset.Tools(json.RawMessage(entry), time.Now()); err == nil {
			t.Fatalf("Tools(%s) = nil error, want a rejection", entry)
		}
	}
}

// The schema the model is handed is the one the wire documents, field for
// field (checked against anthropic-sdk-go v1.70.1 — betaagent.go
// BetaManagedAgentsAgentToolset20260401BashInput and
// BetaManagedAgentsAgentToolset20260401ReadInput and
// BetaManagedAgentsAgentToolset20260401WriteInput and
// BetaManagedAgentsAgentToolset20260401EditInput and
// BetaManagedAgentsAgentToolset20260401GlobInput and
// BetaManagedAgentsAgentToolset20260401GrepInput).
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
		// The wire carries no Input types for the web tools; their schemas are
		// the recorded reference's (TestWebToolSchemasMatchTheRecording).
		"web_fetch":  {props: []string{"url"}, required: []string{"url"}},
		"web_search": {props: []string{"query"}, required: []string{"query"}},
	}

	defs, err := toolset.Tools(json.RawMessage(`{"type":"agent_toolset_20260401"}`), time.Now())
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
		// No SDK type mirrors the web schemas, so their property types are a
		// contract this test owns.
		if d.Name == "web_fetch" || d.Name == "web_search" {
			for p, raw := range d.InputSchema.Properties {
				var ps struct {
					Type string `json:"type"`
				}
				if err := json.Unmarshal(raw, &ps); err != nil || ps.Type != "string" {
					t.Errorf("%s.%s: type = %q, want string", d.Name, p, ps.Type)
				}
			}
		}
	}
}

// Two of the keywords a 2026-09-02 recording showed the reference closing its
// six sandbox tools with are adopted (#822, an owner decision):
// additionalProperties:false on all six, and minLength:1 on edit's old_string.
// The recording's ^\/ pattern on file_path is not: paths stay relative, as the
// skills block hands the model one (docs/DIVERGENCES.md).
func TestSandboxToolSchemasCarryTheAdoptedRecordedKeywords(t *testing.T) {
	defs, err := toolset.Tools(json.RawMessage(`{"type":"agent_toolset_20260401"}`), time.Now())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	seen := 0
	for _, raw := range defs {
		var d struct {
			Name        string `json:"name"`
			InputSchema struct {
				Properties           map[string]map[string]any `json:"properties"`
				AdditionalProperties *bool                     `json:"additionalProperties"`
			} `json:"input_schema"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatalf("definition: %v", err)
		}
		if toolset.IsWebTool(d.Name) {
			continue
		}
		seen++
		if ap := d.InputSchema.AdditionalProperties; ap == nil {
			t.Errorf("%s: additionalProperties absent, want false", d.Name)
		} else if *ap {
			t.Errorf("%s: additionalProperties = true, want false", d.Name)
		}
		if fp, ok := d.InputSchema.Properties["file_path"]; ok {
			if _, ok := fp["pattern"]; ok {
				t.Errorf("%s: file_path carries a pattern %v, want relative paths admitted", d.Name, fp["pattern"])
			}
		}
		for prop, schema := range d.InputSchema.Properties {
			minLen, has := schema["minLength"]
			if d.Name == "edit" && prop == "old_string" {
				if minLen != float64(1) {
					t.Errorf("edit.old_string: minLength = %v, want 1", minLen)
				}
			} else if has {
				t.Errorf("%s.%s: minLength = %v, want none", d.Name, prop, minLen)
			}
		}
	}
	if seen != 6 {
		t.Fatalf("checked %d sandbox tools, want 6", seen)
	}
}

// The web tools' input schemas are the reference's, keyword for keyword and
// property description for property description, as a 2026-09-02 recording
// captured them: the agent echoed its tool definitions (a model-mediated echo,
// not a captured provider request — docs/DIVERGENCES.md weighs it). The tool
// descriptions are TestWebToolDescriptionsMatchTheRecording's.
func TestWebToolSchemasMatchTheRecording(t *testing.T) {
	recorded := map[string]string{
		"web_fetch": `{"type":"object","properties":{"url":{"type":"string","format":"uri",` +
			`"description":"The URL to fetch content from"}},"required":["url"],"additionalProperties":false}`,
		"web_search": `{"type":"object","properties":{"query":{"type":"string","minLength":2,` +
			`"description":"The search query to use"}},"required":["query"],"additionalProperties":false}`,
	}

	defs, err := toolset.Tools(json.RawMessage(`{"type":"agent_toolset_20260401"}`), time.Now())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	seen := 0
	for _, raw := range defs {
		var d struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"input_schema"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatalf("definition: %v", err)
		}
		rec, ok := recorded[d.Name]
		if !ok {
			continue
		}
		seen++
		var want map[string]any
		if err := json.Unmarshal([]byte(rec), &want); err != nil {
			t.Fatalf("recorded %s: %v", d.Name, err)
		}
		if !reflect.DeepEqual(d.InputSchema, want) {
			got, _ := json.Marshal(d.InputSchema)
			t.Errorf("%s input_schema = %s, want the recorded %s", d.Name, got, rec)
		}
	}
	if seen != len(recorded) {
		t.Fatalf("saw %d of the %d web tools", seen, len(recorded))
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func TestIsWebTool(t *testing.T) {
	for name, want := range map[string]bool{
		"web_fetch": true, "web_search": true,
		"bash": false, "grep": false, "no_such_tool": false,
	} {
		if got := toolset.IsWebTool(name); got != want {
			t.Errorf("IsWebTool(%q) = %v, want %v", name, got, want)
		}
	}
}

// Resolve is Tools and Policies in one pass, for the brain, which needs both
// for every toolset entry on every turn: the same tools, in the same order,
// each definition byte-identical to Tools' and each policy Policies'.
func TestResolveIsToolsAndPoliciesInOnePass(t *testing.T) {
	entry := json.RawMessage(`{"type":"agent_toolset_20260401","default_config":{"permission_policy":{"type":"always_ask"}},` +
		`"configs":[{"name":"bash","permission_policy":{"type":"always_allow"}},{"name":"grep","enabled":false}]}`)
	now := time.Date(2026, 9, 2, 0, 2, 58, 0, time.UTC)
	defs, err := toolset.Tools(entry, now)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	policies, err := toolset.Policies(entry)
	if err != nil {
		t.Fatalf("Policies: %v", err)
	}
	got, err := toolset.Resolve(entry, now)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != len(defs) || len(got) != len(policies) {
		t.Fatalf("Resolve = %d tools, Tools %d, Policies %d", len(got), len(defs), len(policies))
	}
	for i, b := range got {
		if string(b.Definition) != string(defs[i]) {
			t.Errorf("tool %d (%s) definition differs from Tools':\n%s\n%s", i, b.Name, b.Definition, defs[i])
		}
		if b.Policy != policies[b.Name] {
			t.Errorf("%s policy = %q, want Policies' %q", b.Name, b.Policy, policies[b.Name])
		}
		var d struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(b.Definition, &d); err != nil || d.Name != b.Name {
			t.Errorf("tool %d named %q, its definition %q (%v)", i, b.Name, d.Name, err)
		}
	}
	if _, err := toolset.Resolve(json.RawMessage(`{"type":"agent_toolset_20260401","default_config":{"permission_policy":{"type":"nope"}}}`), now); err == nil {
		t.Error("Resolve accepted a policy Tools and Policies refuse")
	}
}
