package sandbox_test

import (
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

func TestValidateEnv(t *testing.T) {
	// Empty and nil maps are trivially valid — the common case.
	for _, env := range []map[string]string{nil, {}} {
		if err := sandbox.ValidateEnv(env); err != nil {
			t.Errorf("ValidateEnv(%v) = %v, want nil", env, err)
		}
	}

	valid := []string{"A", "_", "_x", "A1", "PROXY_URL", "vltph_ABC", "a9_b0"}
	for _, k := range valid {
		if err := sandbox.ValidateEnv(map[string]string{k: "v"}); err != nil {
			t.Errorf("ValidateEnv key %q = %v, want nil", k, err)
		}
	}

	// Values are never constrained — only keys are.
	if err := sandbox.ValidateEnv(map[string]string{"K": "any=thing you\nlike"}); err != nil {
		t.Errorf("ValidateEnv must not constrain values: %v", err)
	}

	invalid := []string{"", "1A", "A=B", "a-b", "a.b", "a b", "MÜNCHEN"}
	for _, k := range invalid {
		err := sandbox.ValidateEnv(map[string]string{k: "v"})
		if err == nil {
			t.Errorf("ValidateEnv key %q = nil, want an error", k)
			continue
		}
		// The error names the offending key so a misconfiguration is diagnosable
		// (skip the empty key: every string contains "").
		if k != "" && !strings.Contains(err.Error(), k) {
			t.Errorf("ValidateEnv(%q) error %q does not name the key", k, err)
		}
	}
}
