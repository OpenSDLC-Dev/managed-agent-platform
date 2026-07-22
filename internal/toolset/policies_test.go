package toolset_test

import (
	"encoding/json"
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
