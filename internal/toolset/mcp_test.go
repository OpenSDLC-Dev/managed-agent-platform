package toolset_test

import (
	"encoding/json"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
)

// reported is a stand-in listing: three tools, as a server would report them.
func reported() []toolset.MCPTool {
	return []toolset.MCPTool{
		{Name: "search", Description: "Search the docs.", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "fetch", Description: "Fetch one document.", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "index", Description: "Rebuild the index.", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
}

func resolvedNames(rs []toolset.MCPResolved) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Name
	}
	return out
}

// The MCP toolset's enable resolution is the built-in toolset's, applied to a
// list the server supplies rather than a list this package knows.
func TestResolveMCPEnable(t *testing.T) {
	cases := []struct {
		name  string
		entry string
		want  []string
	}{
		{
			name:  "bare entry enables every reported tool",
			entry: `{"type":"mcp_toolset","mcp_server_name":"docs"}`,
			want:  []string{"search", "fetch", "index"},
		},
		{
			name:  "default_config off disables every tool",
			entry: `{"type":"mcp_toolset","mcp_server_name":"docs","default_config":{"enabled":false}}`,
			want:  nil,
		},
		{
			name: "a per-tool config overrides an off default",
			entry: `{"type":"mcp_toolset","mcp_server_name":"docs","default_config":{"enabled":false},
			         "configs":[{"name":"fetch","enabled":true}]}`,
			want: []string{"fetch"},
		},
		{
			name: "a per-tool config overrides an on default",
			entry: `{"type":"mcp_toolset","mcp_server_name":"docs",
			         "configs":[{"name":"index","enabled":false}]}`,
			want: []string{"search", "fetch"},
		},
		{
			name: "repeated entries for one tool merge, last value winning",
			entry: `{"type":"mcp_toolset","mcp_server_name":"docs",
			         "configs":[{"name":"index","enabled":false},{"name":"index","enabled":true}]}`,
			want: []string{"search", "fetch", "index"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := toolset.ResolveMCP(json.RawMessage(tc.entry), reported())
			if err != nil {
				t.Fatalf("ResolveMCP: %v", err)
			}
			if !equal(resolvedNames(got), tc.want) {
				t.Errorf("enabled = %v, want %v", resolvedNames(got), tc.want)
			}
		})
	}
}

// An MCP tool defaults to always_ask, where a built-in defaults to
// always_allow: a third party's tool is not the platform's own.
func TestResolveMCPPolicy(t *testing.T) {
	cases := []struct {
		name  string
		entry string
		want  map[string]domain.PermissionPolicyType
	}{
		{
			name:  "the toolset default is always_ask",
			entry: `{"type":"mcp_toolset","mcp_server_name":"docs"}`,
			want: map[string]domain.PermissionPolicyType{
				"search": domain.PolicyAlwaysAsk, "fetch": domain.PolicyAlwaysAsk, "index": domain.PolicyAlwaysAsk,
			},
		},
		{
			name: "default_config sets every tool's policy",
			entry: `{"type":"mcp_toolset","mcp_server_name":"docs",
			         "default_config":{"permission_policy":{"type":"always_allow"}}}`,
			want: map[string]domain.PermissionPolicyType{
				"search": domain.PolicyAlwaysAllow, "fetch": domain.PolicyAlwaysAllow, "index": domain.PolicyAlwaysAllow,
			},
		},
		{
			name: "a per-tool policy overrides default_config",
			entry: `{"type":"mcp_toolset","mcp_server_name":"docs",
			         "default_config":{"permission_policy":{"type":"always_allow"}},
			         "configs":[{"name":"index","permission_policy":{"type":"always_ask"}}]}`,
			want: map[string]domain.PermissionPolicyType{
				"search": domain.PolicyAlwaysAllow, "fetch": domain.PolicyAlwaysAllow, "index": domain.PolicyAlwaysAsk,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := toolset.ResolveMCP(json.RawMessage(tc.entry), reported())
			if err != nil {
				t.Fatalf("ResolveMCP: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("resolved %d tools, want %d", len(got), len(tc.want))
			}
			for _, r := range got {
				if r.Policy != tc.want[r.Name] {
					t.Errorf("%s policy = %q, want %q", r.Name, r.Policy, tc.want[r.Name])
				}
			}
		})
	}
}

// A policy this platform cannot evaluate is the #26 fail-open class: defaulting
// it would run an unconfirmed tool. It is an error only where a live tool would
// carry it — the same laziness resolveToolset applies to the built-ins.
func TestResolveMCPUnevaluablePolicy(t *testing.T) {
	live := `{"type":"mcp_toolset","mcp_server_name":"docs",
	          "configs":[{"name":"index","permission_policy":{"type":"always_deny"}}]}`
	if _, _, err := toolset.ResolveMCP(json.RawMessage(live), reported()); err == nil {
		t.Error("ResolveMCP accepted an unevaluable policy on an enabled tool")
	}

	disabled := `{"type":"mcp_toolset","mcp_server_name":"docs",
	             "configs":[{"name":"index","enabled":false,"permission_policy":{"type":"always_deny"}}]}`
	got, _, err := toolset.ResolveMCP(json.RawMessage(disabled), reported())
	if err != nil {
		t.Fatalf("ResolveMCP rejected a policy no live tool carries: %v", err)
	}
	if !equal(resolvedNames(got), []string{"search", "fetch"}) {
		t.Errorf("enabled = %v, want search and fetch", resolvedNames(got))
	}
}

// An MCP server's tool list is dynamic and unknowable when the agent is
// written, so a configs[] entry naming a tool the server does not report is
// reported back as unknown rather than failing the turn — the docs make it a
// warning, not an error.
func TestResolveMCPUnknownConfigNames(t *testing.T) {
	entry := `{"type":"mcp_toolset","mcp_server_name":"docs",
	           "configs":[{"name":"search","enabled":true},{"name":"summarise","enabled":false},
	                      {"name":"translate","permission_policy":{"type":"always_deny"}}]}`
	got, unknown, err := toolset.ResolveMCP(json.RawMessage(entry), reported())
	if err != nil {
		t.Fatalf("ResolveMCP: %v", err)
	}
	if !equal(resolvedNames(got), []string{"search", "fetch", "index"}) {
		t.Errorf("enabled = %v, want every reported tool", resolvedNames(got))
	}
	if !equal(unknown, []string{"summarise", "translate"}) {
		t.Errorf("unknown = %v, want summarise and translate", unknown)
	}
}

// A tool with no name cannot be offered: the model-facing name it composes to
// is legal on its face, and the wire event a call on it would commit requires a
// tool name. Nothing writes such a row — internal/mcp refuses an empty name
// where the listing is read — which is why the drop is silent rather than a
// warning about a state that cannot arise.
func TestResolveMCPDropsANamelessTool(t *testing.T) {
	nameless := append(reported(), toolset.MCPTool{
		Description: "No name at all.", InputSchema: json.RawMessage(`{"type":"object"}`),
	})
	got, _, err := toolset.ResolveMCP(json.RawMessage(`{"type":"mcp_toolset","mcp_server_name":"docs"}`), nameless)
	if err != nil {
		t.Fatalf("ResolveMCP: %v", err)
	}
	if !equal(resolvedNames(got), []string{"search", "fetch", "index"}) {
		t.Errorf("enabled = %v, want the named tools alone", resolvedNames(got))
	}
}

// A server that reports the same tool twice gets one definition: a duplicate
// model-facing name is a request the endpoint rejects, and the two entries are
// indistinguishable to everything downstream — the result names a tool, not a
// position.
func TestResolveMCPDropsARepeatedToolName(t *testing.T) {
	twice := append(reported(), toolset.MCPTool{
		Name: "search", Description: "A second listing.", InputSchema: json.RawMessage(`{"type":"object"}`),
	})
	got, _, err := toolset.ResolveMCP(json.RawMessage(`{"type":"mcp_toolset","mcp_server_name":"docs"}`), twice)
	if err != nil {
		t.Fatalf("ResolveMCP: %v", err)
	}
	if !equal(resolvedNames(got), []string{"search", "fetch", "index"}) {
		t.Errorf("enabled = %v, want each name once", resolvedNames(got))
	}
	if got[0].Description != "Search the docs." {
		t.Errorf("kept description = %q, want the first listing's", got[0].Description)
	}
}
