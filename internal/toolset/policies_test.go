package toolset_test

import (
	"encoding/json"
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
				"bash": allow, "read": allow, "write": allow,
				"edit": allow, "glob": allow, "grep": allow,
			},
		},
		{
			name:  "default_config policy applies to every enabled tool",
			entry: `{"type":"agent_toolset_20260401","default_config":{"permission_policy":{"type":"always_ask"}}}`,
			want: map[string]domain.PermissionPolicyType{
				"bash": ask, "read": ask, "write": ask,
				"edit": ask, "glob": ask, "grep": ask,
			},
		},
		{
			name: "a per-tool config policy overrides the default_config policy",
			entry: `{"type":"agent_toolset_20260401",
			         "default_config":{"permission_policy":{"type":"always_ask"}},
			         "configs":[{"name":"read","permission_policy":{"type":"always_allow"}}]}`,
			want: map[string]domain.PermissionPolicyType{
				"bash": ask, "read": allow, "write": ask,
				"edit": ask, "glob": ask, "grep": ask,
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
