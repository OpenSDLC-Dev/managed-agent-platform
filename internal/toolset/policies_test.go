package toolset_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
)

// TestPolicies pins the permission-policy resolver: for each enabled built-in
// tool it resolves per-tool config > default_config > the plan's default
// (always_allow), and it mirrors Tools by omitting disabled tools.
func TestPolicies(t *testing.T) {
	allow := domain.PolicyAlwaysAllow
	ask := domain.PolicyAlwaysAsk

	cases := []struct {
		name  string
		entry string
		want  map[string]domain.PermissionPolicyType
	}{
		{
			// The plan's resolved default for the agent toolset is always_allow;
			// a bare entry enables every tool at that default.
			name:  "bare entry defaults every tool to always_allow",
			entry: `{"type":"agent_toolset_20260401"}`,
			want: map[string]domain.PermissionPolicyType{
				"bash": allow, "read": allow, "write": allow, "edit": allow,
				"glob": allow, "grep": allow, "web_fetch": allow, "web_search": allow,
			},
		},
		{
			name:  "default_config policy applies to every enabled tool",
			entry: `{"type":"agent_toolset_20260401","default_config":{"permission_policy":{"type":"always_ask"}}}`,
			want: map[string]domain.PermissionPolicyType{
				"bash": ask, "read": ask, "write": ask, "edit": ask,
				"glob": ask, "grep": ask, "web_fetch": ask, "web_search": ask,
			},
		},
		{
			name: "a per-tool config policy overrides the default_config policy",
			entry: `{"type":"agent_toolset_20260401",
			         "default_config":{"permission_policy":{"type":"always_ask"}},
			         "configs":[{"name":"read","permission_policy":{"type":"always_allow"}}]}`,
			want: map[string]domain.PermissionPolicyType{
				"bash": ask, "read": allow, "write": ask, "edit": ask,
				"glob": ask, "grep": ask, "web_fetch": ask, "web_search": ask,
			},
		},
		{
			name: "disabled tools are absent from the policy map",
			entry: `{"type":"agent_toolset_20260401","default_config":{"enabled":false},
			         "configs":[{"name":"bash","enabled":true,"permission_policy":{"type":"always_ask"}}]}`,
			want: map[string]domain.PermissionPolicyType{"bash": ask},
		},
		{
			// Enable resolution and policy resolution are independent: a config may
			// flip a tool on while leaving its policy at the default_config value.
			name: "a config enables a tool at the default_config policy",
			entry: `{"type":"agent_toolset_20260401","default_config":{"enabled":false,"permission_policy":{"type":"always_ask"}},
			         "configs":[{"name":"bash","enabled":true}]}`,
			want: map[string]domain.PermissionPolicyType{"bash": ask},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := toolset.Policies(json.RawMessage(tc.entry))
			if err != nil {
				t.Fatalf("Policies: %v", err)
			}
			if !samePolicies(got, tc.want) {
				t.Fatalf("policies = %v, want %v", got, tc.want)
			}
		})
	}
}

func samePolicies(a, b map[string]domain.PermissionPolicyType) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestPoliciesRejectsUnknownPolicy guards the enum: an unknown or malformed
// permission_policy on an enabled tool is a rejection, not a silent default —
// a policy this platform can't evaluate must never resolve to "run it anyway".
func TestPoliciesRejectsUnknownPolicy(t *testing.T) {
	for _, entry := range []string{
		`{"type":"agent_toolset_20260401","default_config":{"permission_policy":{"type":"sometimes"}}}`,
		`{"type":"agent_toolset_20260401","configs":[{"name":"bash","permission_policy":{"type":""}}]}`,
		`{"type":"agent_toolset_20260401","default_config":{"permission_policy":"always_ask"}}`,
	} {
		if _, err := toolset.Policies(json.RawMessage(entry)); err == nil {
			t.Fatalf("Policies(%s) = nil error, want a rejection", entry)
		}
	}
}

// TestValidateRejectsUnknownFields guards the permission boundary against a
// misspelled key: encoding/json drops an unknown field, so a typo'd
// permission_policy would otherwise be discarded and the tool would silently
// resolve to the always_allow default instead of the intended gate (issue #26).
// Every nesting level of the pinned agent_toolset_20260401 wire schema is checked,
// and the error must name the offending field's path so a client can find the typo.
func TestValidateRejectsUnknownFields(t *testing.T) {
	cases := []struct {
		name   string
		entry  string
		wantIn []string // substrings the rejection must name (field, and its path)
	}{
		{
			name:   "misspelled permission_policy in default_config",
			entry:  `{"type":"agent_toolset_20260401","default_config":{"permission_polciy":{"type":"always_ask"}}}`,
			wantIn: []string{"permission_polciy", "default_config"},
		},
		{
			name:   "misspelled permission_policy in a per-tool config",
			entry:  `{"type":"agent_toolset_20260401","configs":[{"name":"bash","permission_polciy":{"type":"always_ask"}}]}`,
			wantIn: []string{"permission_polciy", "configs[0]"},
		},
		{
			name:   "unknown key on the toolset object",
			entry:  `{"type":"agent_toolset_20260401","defualt_config":{"enabled":false}}`,
			wantIn: []string{"defualt_config"},
		},
		{
			name:   "unknown key inside a permission_policy object",
			entry:  `{"type":"agent_toolset_20260401","default_config":{"permission_policy":{"type":"always_ask","mode":"soft"}}}`,
			wantIn: []string{"mode", "default_config.permission_policy"},
		},
		{
			name:   "unknown key inside a per-tool config",
			entry:  `{"type":"agent_toolset_20260401","configs":[{"name":"bash","enabld":true}]}`,
			wantIn: []string{"enabld", "configs[0]"},
		},
		{
			// Eager: the malformed object is rejected even though the tool it sits on
			// is disabled. A typo'd policy on a disabled tool is a latent fail-open
			// that activates the moment the tool is enabled, not a harmless no-op —
			// unlike a bogus policy *value*, which TestPoliciesValidatesLazily leaves
			// to the live-tool check.
			name: "misspelled permission_policy on a disabled tool is still rejected",
			entry: `{"type":"agent_toolset_20260401","default_config":{"enabled":false},
			         "configs":[{"name":"bash","enabled":false,"permission_polciy":{"type":"always_ask"}}]}`,
			wantIn: []string{"permission_polciy", "configs[0]"},
		},
		{
			// The discriminator has to agree with the name it sits beside: each
			// of the eight union variants tags both keys with the same tool
			// constant (since anthropic-sdk-go v1.66.0 — betaagent.go
			// BetaManagedAgentsAgentToolConfigParamsUnion), so a type naming a
			// different tool selects two variants at once and there is no way
			// to tell which the client meant.
			name:   "a per-tool type that disagrees with name is rejected",
			entry:  `{"type":"agent_toolset_20260401","configs":[{"name":"bash","type":"read"}]}`,
			wantIn: []string{"configs[0].type", "bash", "read"},
		},
		{
			name:   "a non-string per-tool type is rejected",
			entry:  `{"type":"agent_toolset_20260401","configs":[{"name":"bash","type":1}]}`,
			wantIn: []string{"configs[0].type"},
		},
		{
			// encoding/json reads a JSON null into a plain string as "", so
			// two nulls would compare as two equal empty strings and pass —
			// a null type beside a real name is caught by the mismatch arm
			// either way, which is why this case nulls both.
			name:   "a null per-tool type beside a null name is rejected",
			entry:  `{"type":"agent_toolset_20260401","configs":[{"name":null,"type":null}]}`,
			wantIn: []string{"configs[0].type"},
		},
		{
			name:   "a per-tool type beside a null name is rejected",
			entry:  `{"type":"agent_toolset_20260401","configs":[{"name":null,"type":""}]}`,
			wantIn: []string{"configs[0].type"},
		},
		{
			// Only the eight per-tool variants gained the discriminator;
			// AgentToolsetDefaultConfigParams still carries enabled and
			// permission_policy and nothing else.
			name:   "type inside default_config is still an unknown field",
			entry:  `{"type":"agent_toolset_20260401","default_config":{"type":"bash"}}`,
			wantIn: []string{`unknown field "type"`, "default_config"},
		},
		{
			// The SDK also put the web tools' domain lists on the wire (since
			// anthropic-sdk-go v1.66.0 — betaagent.go
			// BetaManagedAgentsWebFetchToolConfigParams and
			// BetaManagedAgentsWebSearchToolConfigParams). This platform
			// configures them operator-side (WEBTOOL_ALLOWED_DOMAINS), so
			// accepting the field would promise a per-agent policy nothing here
			// enforces — the refusal is registered in docs/DIVERGENCES.md.
			name:   "web_fetch allowed_domains is refused",
			entry:  `{"type":"agent_toolset_20260401","configs":[{"name":"web_fetch","type":"web_fetch","allowed_domains":["docs.example.com"]}]}`,
			wantIn: []string{"allowed_domains", "configs[0]"},
		},
		{
			name:   "web_search user_location is refused",
			entry:  `{"type":"agent_toolset_20260401","configs":[{"name":"web_search","user_location":{"type":"approximate","country":"US"}}]}`,
			wantIn: []string{"user_location", "configs[0]"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := toolset.Validate(json.RawMessage(tc.entry))
			if err == nil {
				t.Fatalf("Validate(%s) = nil error, want a rejection", tc.entry)
			}
			for _, sub := range tc.wantIn {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("error %q does not name %q", err, sub)
				}
			}
		})
	}
}

// TestValidateConfigErrorsForTheRecordedCases pins the three configs[] refusals
// the reference was recorded answering: each is a *toolset.ConfigError carrying
// the reference's path and sentence, which the agent routes render (#540),
// while its Error() stays this package's own. Recording:
// managed-agents-wire-recordings 2026-09-02/batch2.json idx 131
// `agent.create.config-unknown-key`, idx 129 `agent.create.config-type-only`,
// idx 130 `agent.create.config-name-type-mismatch`. The unrecorded siblings —
// a name selecting no built-in, a null name, default_config, a
// permission_policy, the toolset object, the mcp_toolset kind — stay plain
// errors.
func TestValidateConfigErrorsForTheRecordedCases(t *testing.T) {
	for _, tc := range []struct {
		entry, path, reason, ours string
	}{
		{`{"type":"agent_toolset_20260401","configs":[{"name":"read"},{"name":"bash","bogus":1}]}`,
			"configs[1].bogus", `Extra inputs are not permitted for name "bash"`,
			`agent_toolset_20260401: unknown field "bogus" in configs[1]`},
		{`{"type":"agent_toolset_20260401","configs":[{"type":"bash"}]}`,
			"configs[0].name", `must be "bash" on a config of type "bash"`,
			`agent_toolset_20260401: configs[0].type needs a string name to equal`},
		{`{"type":"agent_toolset_20260401","configs":[{"name":"bash","type":"grep"}]}`,
			"configs[0].type", `"grep" does not match name "bash"`,
			`agent_toolset_20260401: configs[0].type is "grep" but must equal name "bash"`},
	} {
		err := toolset.Validate(json.RawMessage(tc.entry))
		var cfg *toolset.ConfigError
		if !errors.As(err, &cfg) {
			t.Errorf("Validate(%s) = %v, want a *toolset.ConfigError", tc.entry, err)
			continue
		}
		if cfg.Path != tc.path || cfg.Reason != tc.reason || cfg.Error() != tc.ours {
			t.Errorf("Validate(%s) = {Path %q, Reason %q, Error %q}, want {%q, %q, %q}",
				tc.entry, cfg.Path, cfg.Reason, cfg.Error(), tc.path, tc.reason, tc.ours)
		}
	}
	for _, entry := range []string{
		`{"type":"agent_toolset_20260401","configs":[{"name":"nope","bogus":1}]}`,
		`{"type":"agent_toolset_20260401","configs":[{"bogus":1}]}`,
		`{"type":"agent_toolset_20260401","configs":[{"type":"nope"}]}`,
		`{"type":"agent_toolset_20260401","configs":[{"name":null,"type":"bash"}]}`,
		`{"type":"agent_toolset_20260401","configs":[{"name":"nope","type":"bash"}]}`,
		`{"type":"agent_toolset_20260401","default_config":{"bogus":1}}`,
		`{"type":"agent_toolset_20260401","configs":[{"name":"bash","permission_policy":{"type":"always_ask","bogus":1}}]}`,
		`{"type":"agent_toolset_20260401","bogus":1}`,
	} {
		err := toolset.Validate(json.RawMessage(entry))
		var cfg *toolset.ConfigError
		if err == nil || errors.As(err, &cfg) {
			t.Errorf("Validate(%s) = %v, want a plain error", entry, err)
		}
	}
	err := toolset.ValidateMCPToolset(json.RawMessage(`{"type":"mcp_toolset","mcp_server_name":"s","configs":[{"name":"bash","bogus":1}]}`))
	var cfg *toolset.ConfigError
	if err == nil || errors.As(err, &cfg) {
		t.Errorf("ValidateMCPToolset with an unknown configs[] key = %v, want a plain error", err)
	}

	// A web tool's own fields are refused by this platform's choice; the
	// reference accepts them, so no recorded sentence applies and the refusal
	// stays a plain error in this package's words (#540, #481).
	for tool, fields := range map[string][]string{
		"web_fetch":  {"allowed_domains", "blocked_domains", "max_content_tokens"},
		"web_search": {"allowed_domains", "blocked_domains", "user_location"},
	} {
		for _, f := range fields {
			entry := fmt.Sprintf(`{"type":"agent_toolset_20260401","configs":[{"name":%q,%q:1}]}`, tool, f)
			err := toolset.Validate(json.RawMessage(entry))
			want := fmt.Sprintf(`agent_toolset_20260401: unknown field %q in configs[0]`, f)
			var cfg *toolset.ConfigError
			if err == nil || errors.As(err, &cfg) || err.Error() != want {
				t.Errorf("Validate(%s) = %v, want the plain %q", entry, err, want)
			}
		}
	}
	// A field belonging to the other web tool is unknown on both sides, and
	// takes the recorded sentence like any other unknown key.
	err = toolset.Validate(json.RawMessage(`{"type":"agent_toolset_20260401","configs":[{"name":"web_fetch","user_location":1}]}`))
	if !errors.As(err, &cfg) || cfg.Reason != `Extra inputs are not permitted for name "web_fetch"` {
		t.Errorf("web_fetch with user_location = %v, want the recorded sentence", err)
	}
}

// TestValidateNamesTheLeastUnknownKey pins that a configs[] entry, a
// default_config or a toolset carrying several unknown keys names the same
// one on every call — the byte-order-least, unknownkey.Least's pick —
// where a map's iteration order would pick any.
func TestValidateNamesTheLeastUnknownKey(t *testing.T) {
	for entry, want := range map[string]string{
		`{"type":"agent_toolset_20260401","configs":[{"name":"bash","zeta":1,"alpha":1,"mid":1}]}`: `agent_toolset_20260401: unknown field "alpha" in configs[0]`,
		`{"type":"agent_toolset_20260401","default_config":{"zeta":1,"alpha":1}}`:                  `agent_toolset_20260401: unknown field "alpha" in default_config`,
		`{"type":"agent_toolset_20260401","zeta":1,"alpha":1}`:                                     `agent_toolset_20260401: unknown field "alpha"`,
		// A web field and a truly unknown one: the least is the web field, so
		// the refusal is this platform's.
		`{"type":"agent_toolset_20260401","configs":[{"name":"web_fetch","zzz":1,"allowed_domains":1}]}`: `agent_toolset_20260401: unknown field "allowed_domains" in configs[0]`,
	} {
		for range 20 {
			if err := toolset.Validate(json.RawMessage(entry)); err == nil || err.Error() != want {
				t.Fatalf("Validate(%s) = %v, want %q", entry, err, want)
			}
		}
	}
	var cfg *toolset.ConfigError
	err := toolset.Validate(json.RawMessage(`{"type":"agent_toolset_20260401","configs":[{"name":"bash","zeta":1,"alpha":1}]}`))
	if !errors.As(err, &cfg) || cfg.Path != "configs[0].alpha" {
		t.Errorf("ConfigError = %v, want the path to name alpha", err)
	}
}

// TestValidateAcceptsKnownFields pins that the unknown-key check does not overreach:
// a fully-populated entry using only pinned wire fields validates, and a correctly
// spelled permission_policy still resolves to its value rather than the default.
func TestValidateAcceptsKnownFields(t *testing.T) {
	entry := `{"type":"agent_toolset_20260401",
	          "default_config":{"enabled":true,"permission_policy":{"type":"always_ask"}},
	          "configs":[{"name":"bash","enabled":true,"permission_policy":{"type":"always_allow"}}]}`
	if err := toolset.Validate(json.RawMessage(entry)); err != nil {
		t.Fatalf("Validate(%s) = %v, want nil", entry, err)
	}
	pols, err := toolset.Policies(json.RawMessage(entry))
	if err != nil {
		t.Fatalf("Policies: %v", err)
	}
	if pols["bash"] != domain.PolicyAlwaysAllow || pols["read"] != domain.PolicyAlwaysAsk {
		t.Errorf("policies = %v, want bash=always_allow, read=always_ask", pols)
	}
}

// TestValidateAcceptsToolTypeDiscriminator pins the one wire change plan 36
// slice 0's SDK bump forces on this arm: the per-tool config became a union
// whose eight variants each carry a `type` beside `name` (since
// anthropic-sdk-go v1.66.0 — betaagent.go
// BetaManagedAgentsAgentToolConfigParamsUnion). It is optional on the request —
// the SDK's own generated example sends it, and the `ant` CLI passes `--tool`
// JSON through raw — so a client of that version must not meet a 400, and an
// entry that omits it must keep working. The discriminator configures nothing,
// so it changes no policy either way.
func TestValidateAcceptsToolTypeDiscriminator(t *testing.T) {
	entry := `{"type":"agent_toolset_20260401","configs":[` +
		`{"name":"bash","type":"bash","enabled":true,"permission_policy":{"type":"always_ask"}},` +
		`{"name":"read"}]}`
	if err := toolset.Validate(json.RawMessage(entry)); err != nil {
		t.Fatalf("Validate(%s) = %v, want nil", entry, err)
	}
	pols, err := toolset.Policies(json.RawMessage(entry))
	if err != nil {
		t.Fatalf("Policies: %v", err)
	}
	if pols["bash"] != domain.PolicyAlwaysAsk || pols["read"] != domain.PolicyAlwaysAllow {
		t.Errorf("policies = %v, want bash=always_ask, read=always_allow", pols)
	}
}

// TestPoliciesValidatesLazily pins that only a live tool's policy is validated:
// a malformed policy on a tool that does not resolve into the enabled set is
// ignored, not rejected, so enable and policy resolution stay consistent.
func TestPoliciesValidatesLazily(t *testing.T) {
	for _, entry := range []string{
		// default off → no tool carries the bogus default policy.
		`{"type":"agent_toolset_20260401","default_config":{"enabled":false,"permission_policy":{"type":"bogus"}}}`,
		// bash overrides the bogus default with a valid policy; nothing else is on.
		`{"type":"agent_toolset_20260401","default_config":{"enabled":false,"permission_policy":{"type":"bogus"}},
		  "configs":[{"name":"bash","enabled":true,"permission_policy":{"type":"always_ask"}}]}`,
		// a bogus policy on a disabled tool is never resolved.
		`{"type":"agent_toolset_20260401","configs":[{"name":"bash","enabled":false,"permission_policy":{"type":"bogus"}}]}`,
	} {
		if _, err := toolset.Policies(json.RawMessage(entry)); err != nil {
			t.Errorf("Policies(%s) = %v, want nil (a non-live tool's policy is not validated)", entry, err)
		}
	}
}
