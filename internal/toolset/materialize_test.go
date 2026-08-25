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
		want: `{"configs":[{"enabled":true,"name":"bash","permission_policy":{"type":"always_allow"},"type":"bash"},` +
			`{"enabled":true,"name":"read","permission_policy":{"type":"always_ask"},"type":"read"}],` +
			`"default_config":{"enabled":true,"permission_policy":{"type":"always_ask"}},` +
			`"type":"agent_toolset_20260401"}`,
	}, {
		// anthropic-sdk-go v1.66.0 split the built-in per-tool config into a
		// union whose eight variants mark `type` required on the response, so
		// the echo renders it whether or not the request carried one — here the
		// entry sent nothing but its name and inherits the rest.
		name: "a built-in entry gains the type discriminator it was not sent with",
		in: `{"type":"agent_toolset_20260401","default_config":{"enabled":false},` +
			`"configs":[{"name":"grep"}]}`,
		want: `{"configs":[{"enabled":false,"name":"grep","permission_policy":{"type":"always_allow"},"type":"grep"}],` +
			`"default_config":{"enabled":false,"permission_policy":{"type":"always_allow"}},` +
			`"type":"agent_toolset_20260401"}`,
	}, {
		// A supplied discriminator is rebuilt from the name rather than carried
		// through, so it cannot be echoed twice or echoed disagreeing with the
		// name — Validate has already refused a request where the two differ.
		name: "a supplied type is echoed once",
		in:   `{"type":"agent_toolset_20260401","configs":[{"name":"bash","type":"bash"}]}`,
		want: `{"configs":[{"enabled":true,"name":"bash","permission_policy":{"type":"always_allow"},"type":"bash"}],` +
			`"default_config":{"enabled":true,"permission_policy":{"type":"always_allow"}},` +
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
		// Only a row written before ValidateMCPToolset existed can carry an
		// unknown key inside a config object; the echo rebuilds those objects
		// from the schema's three fields, so it drops — toward the safe
		// reading, since the tool really does resolve to the default policy the
		// echo now shows. A stray key at the toolset object's own level rides
		// along instead, because that object is echoed rather than rebuilt.
		name: "an unknown nested key is dropped, an unknown top-level key is kept",
		in: `{"type":"mcp_toolset","mcp_server_name":"s","stray":1,` +
			`"configs":[{"name":"t","permission_polciy":{"type":"always_allow"}}]}`,
		want: `{"configs":[{"enabled":true,"name":"t","permission_policy":{"type":"always_ask"}}],` +
			`"default_config":{"enabled":true,"permission_policy":{"type":"always_ask"}},` +
			`"mcp_server_name":"s","stray":1,"type":"mcp_toolset"}`,
	}, {
		// The built-in toolset configures exactly its own definitions, so an
		// entry naming anything else overrides nothing — resolveToolset never
		// looks it up. Filling it in would present it as effective
		// configuration, so it is echoed as supplied instead.
		name: "an entry naming no built-in tool is echoed as supplied",
		in:   `{"type":"agent_toolset_20260401","configs":[{"name":"nope"},{"name":"bash"}]}`,
		want: `{"configs":[{"name":"nope"},{"enabled":true,"name":"bash",` +
			`"permission_policy":{"type":"always_allow"},"type":"bash"}],"default_config":{"enabled":true,` +
			`"permission_policy":{"type":"always_allow"}},"type":"agent_toolset_20260401"}`,
	}, {
		// name is optional on this arm (it has been since #26), so a stored
		// entry can omit it. Grouping every such entry under one "" key would
		// merge unrelated entries into a single one neither of them wrote.
		name: "nameless entries stay separate rather than merging under one key",
		in: `{"type":"agent_toolset_20260401","configs":[{"enabled":false},` +
			`{"permission_policy":{"type":"always_ask"}}]}`,
		want: `{"configs":[{"enabled":false},{"permission_policy":{"type":"always_ask"}}],` +
			`"default_config":{"enabled":true,"permission_policy":{"type":"always_allow"}},` +
			`"type":"agent_toolset_20260401"}`,
	}, {
		// A client that GETs an agent and re-POSTs the tools it was handed must
		// not lose entries the API accepted, so every element is accounted for
		// even when it is not an object.
		name: "a non-object configs element keeps its place",
		in:   `{"type":"agent_toolset_20260401","configs":[null,{"name":"bash","enabled":false}]}`,
		want: `{"configs":[null,{"enabled":false,"name":"bash",` +
			`"permission_policy":{"type":"always_allow"},"type":"bash"}],"default_config":{"enabled":true,` +
			`"permission_policy":{"type":"always_allow"}},"type":"agent_toolset_20260401"}`,
	}, {
		// An MCP server names its own tools, so any non-empty name can be real
		// and resolves; only a nameless entry configures nothing. Validation
		// rejects one at write, so only a row older than it can carry one.
		name: "an mcp entry with no name is echoed as supplied",
		in:   `{"type":"mcp_toolset","mcp_server_name":"s","configs":[{"enabled":false}]}`,
		want: `{"configs":[{"enabled":false}],"default_config":{"enabled":true,` +
			`"permission_policy":{"type":"always_ask"}},"mcp_server_name":"s","type":"mcp_toolset"}`,
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
		// v1.66.0's per-tool `type` discriminator landed on the eight built-in
		// variants only: BetaManagedAgentsMCPToolConfig(Params) still carries
		// name / enabled / permission_policy, so this arm keeps refusing it —
		// and its echo keeps rendering entries without one.
		name:    "a per-tool type is rejected on the mcp arm",
		in:      `{"type":"mcp_toolset","mcp_server_name":"g","configs":[{"name":"t","type":"t"}]}`,
		wantErr: `unknown field "type" in configs[0]`,
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
	}, {
		// An explicit null decodes into the same nil pointer an absent key
		// does, so accepting it would let a per-tool gate be erased silently:
		// under `default_config.permission_policy: always_allow`, a config
		// entry whose policy is null inherits the permissive default. The
		// wire's policy union has no null arm, so refusing it is also the
		// reading that matches the reference's schema.
		name: "an explicit null permission_policy is rejected",
		in: `{"type":"mcp_toolset","mcp_server_name":"g",` +
			`"default_config":{"permission_policy":{"type":"always_allow"}},` +
			`"configs":[{"name":"danger","permission_policy":null}]}`,
		wantErr: "configs[0].permission_policy must not be null",
	}, {
		name:    "an explicit null enabled is rejected",
		in:      `{"type":"mcp_toolset","mcp_server_name":"g","default_config":{"enabled":null}}`,
		wantErr: "default_config.enabled must not be null",
	}, {
		// name identifies the tool an entry configures and is required on the
		// response type: an entry without one configures nothing and would
		// render an echo the wire schema rejects.
		name:    "a configs entry without a name is rejected",
		in:      `{"type":"mcp_toolset","mcp_server_name":"g","configs":[{"enabled":true}]}`,
		wantErr: "configs[0] requires a non-empty name",
	}, {
		name:    "a null configs entry is rejected",
		in:      `{"type":"mcp_toolset","mcp_server_name":"g","configs":[null]}`,
		wantErr: "configs[0] must be an object",
	}, {
		name:    "an empty name is rejected",
		in:      `{"type":"mcp_toolset","mcp_server_name":"g","configs":[{"name":""}]}`,
		wantErr: "configs[0] requires a non-empty name",
	}, {
		name:    "a null name is rejected",
		in:      `{"type":"mcp_toolset","mcp_server_name":"g","configs":[{"name":null}]}`,
		wantErr: "configs[0] requires a non-empty name",
	}, {
		// The leaf type checks below used to be a typed json.Unmarshal, whose
		// error text is a dump of the receiving struct — unexported field names,
		// Go type syntax and all. That text is the message of a 400, so each of
		// these names the field's path instead; the loop asserts it of every
		// error this table produces.
		name:    "configs must be an array",
		in:      `{"type":"mcp_toolset","mcp_server_name":"g","configs":{"a":1}}`,
		wantErr: "configs must be an array",
	}, {
		name:    "default_config must be an object",
		in:      `{"type":"mcp_toolset","mcp_server_name":"g","default_config":"on"}`,
		wantErr: "default_config must be an object",
	}, {
		name:    "a non-boolean enabled is rejected",
		in:      `{"type":"mcp_toolset","mcp_server_name":"g","default_config":{"enabled":"yes"}}`,
		wantErr: "default_config.enabled must be a boolean",
	}, {
		name:    "a non-object permission_policy is rejected",
		in:      `{"type":"mcp_toolset","mcp_server_name":"g","configs":[{"name":"t","permission_policy":"always_ask"}]}`,
		wantErr: "configs[0].permission_policy must be an object",
	}, {
		// Decoding an object into a map keeps only the last value of a repeated
		// member, so the walk above sees a well-formed entry here. encoding/json
		// applies every occurrence, which is why the typed backstop stays: a
		// client whose first `enabled` is a string is writing something this
		// arm refused before the walk replaced the decode, and the built-in
		// arm's resolveToolset still refuses it.
		name: "a repeated field whose earlier value has the wrong type is rejected",
		in: `{"type":"mcp_toolset","mcp_server_name":"g",` +
			`"default_config":{"enabled":"yes","enabled":true}}`,
		wantErr: "a repeated field carries conflicting values",
	}, {
		// The conflict can also sit under one occurrence rather than on the
		// repeated key itself, which is why the message names no path.
		name: "a repeated field with a conflict nested under it is rejected",
		in: `{"type":"mcp_toolset","mcp_server_name":"g",` +
			`"configs":[{"name":"t","enabled":"yes"}],"configs":[{"name":"t","enabled":true}]}`,
		wantErr: "a repeated field carries conflicting values",
	}, {
		name: "a repeated field of one type is accepted, last value winning",
		in: `{"type":"mcp_toolset","mcp_server_name":"g",` +
			`"default_config":{"enabled":false,"enabled":true}}`,
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
			// Every message here becomes the `message` of a 400, so none of
			// them may leak Go internals the way a typed decode's error does.
			for _, leak := range []string{"Go struct", "json:\"", "toolset."} {
				if err != nil && strings.Contains(err.Error(), leak) {
					t.Fatalf("error leaks %q to the client: %v", leak, err)
				}
			}
		})
	}
}
