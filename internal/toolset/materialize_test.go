package toolset_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
)

// TestMaterializeResolvesBothToolsetKinds pins the resolved-config echo the
// reference's response types require: configs and default_config are always
// present, and every one of their enabled / permission_policy fields carries a
// concrete value. The two toolset kinds share the shape and differ only in the
// policy an omitted permission_policy resolves to — always_allow for the agent
// toolset, always_ask for MCP.
func TestMaterializeResolvesBothToolsetKinds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{{
		name: "bare agent toolset resolves to the toolset defaults",
		in:   `{"type":"agent_toolset_20260401"}`,
		want: `{"configs":[],"default_config":{"enabled":true,` +
			`"permission_policy":{"type":"always_allow"}},"type":"agent_toolset_20260401"}`,
	}, {
		name: "bare mcp toolset resolves to always_ask",
		in:   `{"type":"mcp_toolset","mcp_server_name":"github"}`,
		want: `{"configs":[],"default_config":{"enabled":true,` +
			`"permission_policy":{"type":"always_ask"}},"mcp_server_name":"github","type":"mcp_toolset"}`,
	}, {
		name: "supplied entries inherit the resolved default_config",
		in: `{"type":"mcp_toolset","mcp_server_name":"github",` +
			`"default_config":{"enabled":false},"configs":[{"name":"get_issue","enabled":true}]}`,
		want: `{"configs":[{"enabled":true,"name":"get_issue",` +
			`"permission_policy":{"type":"always_ask"}}],"default_config":{"enabled":false,` +
			`"permission_policy":{"type":"always_ask"}},"mcp_server_name":"github","type":"mcp_toolset"}`,
	}, {
		name: "a supplied per-tool policy overrides the default and is echoed verbatim",
		in: `{"type":"agent_toolset_20260401","default_config":{"permission_policy":{"type":"always_ask"}},` +
			`"configs":[{"name":"bash","permission_policy":{"type":"always_allow"}},{"name":"read"}]}`,
		want: `{"configs":[{"enabled":true,"name":"bash","permission_policy":{"type":"always_allow"}},` +
			`{"enabled":true,"name":"read","permission_policy":{"type":"always_ask"}}],` +
			`"default_config":{"enabled":true,"permission_policy":{"type":"always_ask"}},` +
			`"type":"agent_toolset_20260401"}`,
	}, {
		// Duplicate names merge per field the way resolveToolset's override map
		// does, and the merged result is echoed once, in first-appearance order:
		// the echo describes the effective configuration, not the input's shape.
		name: "duplicate config names merge per field into one entry",
		in: `{"type":"mcp_toolset","mcp_server_name":"s","configs":[` +
			`{"name":"t","enabled":false},{"name":"t","permission_policy":{"type":"always_allow"}}]}`,
		want: `{"configs":[{"enabled":false,"name":"t","permission_policy":{"type":"always_allow"}}],` +
			`"default_config":{"enabled":true,"permission_policy":{"type":"always_ask"}},` +
			`"mcp_server_name":"s","type":"mcp_toolset"}`,
	}, {
		// Materialization fills omitted fields; it never validates supplied ones.
		// A malformed leaf is echoed as it was stored — the API boundary rejects
		// it at write time, and a read must not fail on an older row.
		name: "a malformed supplied value is echoed unchanged",
		in:   `{"type":"mcp_toolset","mcp_server_name":"s","configs":[{"name":"t","enabled":"yes"}]}`,
		want: `{"configs":[{"enabled":"yes","name":"t","permission_policy":{"type":"always_ask"}}],` +
			`"default_config":{"enabled":true,"permission_policy":{"type":"always_ask"}},` +
			`"mcp_server_name":"s","type":"mcp_toolset"}`,
	}, {
		name: "a custom tool is not a toolset and passes through untouched",
		in:   `{"type":"custom","name":"x","description":"d","input_schema":{"type":"object"}}`,
		want: `{"type":"custom","name":"x","description":"d","input_schema":{"type":"object"}}`,
	}, {
		name: "an unknown tool type passes through untouched",
		in:   `{"type":"future_toolset","whatever":1}`,
		want: `{"type":"future_toolset","whatever":1}`,
	}, {
		name: "a non-object entry passes through untouched",
		in:   `"not an object"`,
		want: `"not an object"`,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := string(toolset.Materialize(json.RawMessage(tc.in)))
			if got != tc.want {
				t.Fatalf("Materialize:\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

// TestMaterializeToolsIsNonMutating guards the property the render funnels rely
// on: the echo is computed from the stored entries and never writes back
// through them, so materialization cannot leak into what is persisted.
func TestMaterializeToolsIsNonMutating(t *testing.T) {
	t.Parallel()
	stored := []json.RawMessage{
		json.RawMessage(`{"type":"agent_toolset_20260401"}`),
		json.RawMessage(`{"type":"custom","name":"x"}`),
	}
	before := make([]string, len(stored))
	for i, raw := range stored {
		before[i] = string(raw)
	}

	out := toolset.MaterializeTools(stored)

	for i, raw := range stored {
		if string(raw) != before[i] {
			t.Fatalf("MaterializeTools mutated entry %d: got %s, want %s", i, raw, before[i])
		}
	}
	if len(out) != len(stored) {
		t.Fatalf("MaterializeTools returned %d entries, want %d", len(out), len(stored))
	}
	if string(out[0]) == before[0] {
		t.Fatalf("MaterializeTools did not resolve the toolset entry: %s", out[0])
	}
}

// TestMaterializeToolsKeepsNilAndEmpty pins that the helper is safe on the two
// shapes a stored spec can carry before Normalize runs.
func TestMaterializeToolsKeepsNilAndEmpty(t *testing.T) {
	t.Parallel()
	if got := toolset.MaterializeTools(nil); got != nil {
		t.Fatalf("MaterializeTools(nil) = %v, want nil", got)
	}
	if got := toolset.MaterializeTools([]json.RawMessage{}); len(got) != 0 {
		t.Fatalf("MaterializeTools(empty) = %v, want empty", got)
	}
}

// TestValidateMCPToolset gives the MCP arm the create-time check the agent
// toolset already has (Validate): a misspelled key or an unevaluable policy is
// a 400 at write rather than a silent fail-open at the confirmation boundary
// (the #26 class). Unlike the agent toolset's, an MCP entry's tool names are
// the server's, so nothing here validates a name against a known set.
func TestValidateMCPToolset(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		in      string
		wantErr string
	}{{
		name: "a bare entry is valid",
		in:   `{"type":"mcp_toolset","mcp_server_name":"github"}`,
	}, {
		name: "a fully configured entry is valid",
		in: `{"type":"mcp_toolset","mcp_server_name":"github","default_config":{"enabled":false,` +
			`"permission_policy":{"type":"always_ask"}},"configs":[{"name":"get_issue","enabled":true,` +
			`"permission_policy":{"type":"always_allow"}}]}`,
	}, {
		name:    "a misspelled permission_policy key is rejected",
		in:      `{"type":"mcp_toolset","mcp_server_name":"g","configs":[{"name":"t","permission_polciy":{"type":"always_allow"}}]}`,
		wantErr: `unknown field "permission_polciy" in configs[0]`,
	}, {
		name:    "an unknown key on the toolset object is rejected",
		in:      `{"type":"mcp_toolset","mcp_server_name":"g","allowed_tools":["t"]}`,
		wantErr: `unknown field "allowed_tools"`,
	}, {
		name:    "an unknown key inside default_config is rejected",
		in:      `{"type":"mcp_toolset","mcp_server_name":"g","default_config":{"defer_loading":true}}`,
		wantErr: `unknown field "defer_loading" in default_config`,
	}, {
		name:    "an unknown key inside permission_policy is rejected",
		in:      `{"type":"mcp_toolset","mcp_server_name":"g","default_config":{"permission_policy":{"type":"always_ask","extra":1}}}`,
		wantErr: `unknown field "extra" in default_config.permission_policy`,
	}, {
		name:    "an unevaluable policy type is rejected",
		in:      `{"type":"mcp_toolset","mcp_server_name":"g","default_config":{"permission_policy":{"type":"always_deny"}}}`,
		wantErr: `unknown permission_policy type "always_deny"`,
	}, {
		name:    "an unevaluable per-tool policy type is rejected",
		in:      `{"type":"mcp_toolset","mcp_server_name":"g","configs":[{"name":"t","permission_policy":{"type":""}}]}`,
		wantErr: `unknown permission_policy type ""`,
	}, {
		name:    "a non-object configs entry is rejected",
		in:      `{"type":"mcp_toolset","mcp_server_name":"g","configs":["t"]}`,
		wantErr: "mcp_toolset",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := toolset.ValidateMCPToolset(json.RawMessage(tc.in))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("ValidateMCPToolset(%s) = %v, want nil", tc.in, err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("ValidateMCPToolset(%s) = nil, want error containing %q", tc.in, tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("ValidateMCPToolset(%s) = %v, want error containing %q", tc.in, err, tc.wantErr)
			}
			if tc.wantErr != "" && err != nil && !strings.Contains(err.Error(), "mcp_toolset") {
				t.Fatalf("error should name the toolset kind, got %v", err)
			}
		})
	}
}
