package domain

import (
	"strings"
	"testing"
)

func TestNewIDCarriesPrefix(t *testing.T) {
	cases := []string{
		PrefixAgent, PrefixEnvironment, PrefixSession, PrefixEvent, PrefixWork,
		PrefixVault, PrefixResource, PrefixDeployment, PrefixDeploymentRun,
		PrefixFile, PrefixSkill,
	}
	for _, prefix := range cases {
		id := NewID(prefix)
		if got := id.Prefix(); got != prefix {
			t.Errorf("NewID(%q).Prefix() = %q, want %q", prefix, got, prefix)
		}
		if !id.HasPrefix(prefix) {
			t.Errorf("NewID(%q).HasPrefix(%q) = false, want true", prefix, prefix)
		}
		if !strings.HasPrefix(string(id), prefix+"_") {
			t.Errorf("NewID(%q) = %q, want %q_ prefix", prefix, id, prefix)
		}
	}
}

func TestNewIDIsUnique(t *testing.T) {
	seen := make(map[ID]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id := NewID(PrefixSession)
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate ID generated: %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestIDTokenLength(t *testing.T) {
	// 15 random bytes -> 24 crockford-base32 chars, no padding.
	id := NewID(PrefixEvent)
	token := strings.TrimPrefix(string(id), PrefixEvent+"_")
	if len(token) != 24 {
		t.Errorf("token length = %d, want 24 (id=%q)", len(token), id)
	}
	if strings.ContainsAny(token, "iloILOU=") {
		t.Errorf("token %q contains non-crockford / padding chars", token)
	}
}

func TestSessionAltPrefixAccepted(t *testing.T) {
	alt := ID("session_abc123")
	if !alt.HasPrefix(PrefixSession) {
		t.Errorf("session_ alt form should satisfy HasPrefix(PrefixSession)")
	}
	// The alternate form must not leak into other resource checks.
	if alt.HasPrefix(PrefixAgent) {
		t.Errorf("session_ id must not report agent prefix")
	}
}

func TestPrefixEmptyWhenNoUnderscore(t *testing.T) {
	if got := ID("nounderscore").Prefix(); got != "" {
		t.Errorf("Prefix() = %q, want empty", got)
	}
}
