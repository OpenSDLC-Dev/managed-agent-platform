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

func TestReservedEnvName(t *testing.T) {
	// Reserved: injecting an opaque value over one of these breaks the sandbox
	// (PATH — the bootstrap and every tool exec resolve binaries through it) or is
	// a process-injection hook (the loader/shell variables).
	reserved := []string{"PATH", "LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT", "BASH_ENV", "ENV", "IFS"}
	for _, k := range reserved {
		if !sandbox.ReservedEnvName(k) {
			t.Errorf("ReservedEnvName(%q) = false, want true", k)
		}
		// Reserved names are valid grammar — the reservation is a separate,
		// stronger rule, not something ValidateEnv catches.
		if !sandbox.ValidEnvName(k) {
			t.Errorf("reserved name %q should still be grammatically valid", k)
		}
	}
	// Ordinary secret names — including a lookalike — are not reserved.
	for _, k := range []string{"API_KEY", "DB_URL", "MY_PATH", "PATH_", "path"} {
		if sandbox.ReservedEnvName(k) {
			t.Errorf("ReservedEnvName(%q) = true, want false", k)
		}
	}
}
