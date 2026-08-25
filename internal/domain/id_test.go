package domain

import (
	"strings"
	"testing"
)

func TestNewIDCarriesPrefix(t *testing.T) {
	cases := []string{
		PrefixAgent, PrefixEnvironment, PrefixSession, PrefixEvent, PrefixWork,
		PrefixVault, PrefixCredential, PrefixResource, PrefixDeployment,
		PrefixDeploymentRun, PrefixFile, PrefixSkill, PrefixSkillVersion,
		PrefixMemoryStore, PrefixMemory, PrefixMemoryVersion,
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

func TestIDIsZero(t *testing.T) {
	if !ID("").IsZero() {
		t.Errorf("empty ID should be zero")
	}
	if NewID(PrefixAgent).IsZero() {
		t.Errorf("generated ID should not be zero")
	}
}

func TestIDString(t *testing.T) {
	if got := ID("agent_abc").String(); got != "agent_abc" {
		t.Errorf("String() = %q, want %q", got, "agent_abc")
	}
}

func TestIDValid(t *testing.T) {
	// Every prefix NewID emits is Valid, and the session_ alt spelling too.
	for _, prefix := range []string{
		PrefixAgent, PrefixEnvironment, PrefixSession, PrefixEvent, PrefixWork,
		PrefixVault, PrefixResource, PrefixDeployment, PrefixDeploymentRun,
		PrefixFile, PrefixSkill, PrefixSkillVersion, PrefixMemoryStore,
		PrefixMemory, PrefixMemoryVersion, altSessionPrefix,
	} {
		id := ID(prefix + "_" + idEncoding.EncodeToString(make([]byte, idRandomBytes)))
		if !id.Valid() {
			t.Errorf("%q should be Valid", id)
		}
	}
	if !NewID(PrefixAgent).Valid() {
		t.Errorf("NewID output must be Valid")
	}

	// Invalid: bad structure, unknown prefix, out-of-alphabet, and the unstorable
	// bytes (U+0000, invalid UTF-8) that would otherwise reach a bind parameter.
	invalid := map[string]ID{
		"empty":             "",
		"no underscore":     "agentabc",
		"empty token":       "agent_",
		"unknown prefix":    "foo_23456",
		"out-of-alphabet i": "agent_missing",
		"out-of-alphabet o": "work_nope",
		"uppercase":         "agent_ABCDE",
		"hyphen":            "agent_ab-cd",
		"underscore token":  "agent_ab_cd",
		"nul byte":          ID("agent_\x00"),
		"nul mid-token":     ID("agent_ab\x00cd"),
		"invalid utf-8":     ID("agent_\x80"),
	}
	for name, id := range invalid {
		if id.Valid() {
			t.Errorf("%s: %q should be invalid", name, id)
		}
	}
}

// TestValidWithPrefix pins the off-wire spelling directly. It exists because
// PrefixEnvironmentKey is deliberately absent from knownPrefixes — so Valid can
// never answer for an envkey_ id, and the only thing standing between a
// malformed one and a bind parameter is this function.
func TestValidWithPrefix(t *testing.T) {
	if id := NewID(PrefixEnvironmentKey).String(); !ValidWithPrefix(id, PrefixEnvironmentKey) {
		t.Errorf("NewID(%q) = %q must satisfy ValidWithPrefix", PrefixEnvironmentKey, id)
	}
	// The whole point of keeping envkey_ out of knownPrefixes: the wire's own
	// validator must keep rejecting it, so admitting it here widens nothing.
	if ID(NewID(PrefixEnvironmentKey)).Valid() {
		t.Error("an envkey_ id must not be Valid on the wire's prefix set")
	}

	good := "envkey_0123456789abcdefghjkmnp"
	invalid := map[string]string{
		"empty":            "",
		"prefix only":      "envkey",
		"empty token":      "envkey_",
		"just underscore":  "_",
		"wrong prefix":     "env_0123456789abcdefghjkmnp",
		"prefix is a pre":  "envkeys_0123456789",
		"out-of-alphabet":  "envkey_illo",
		"uppercase":        "envkey_ABCDE",
		"underscore token": "envkey_ab_cd",
		"nul byte":         "envkey_\x00",
		"nul mid-token":    "envkey_ab\x00cd",
		"invalid utf-8":    "envkey_\x80",
		"leading space":    " envkey_abcde",
		"trailing space":   "envkey_abcde ",
	}
	for name, id := range invalid {
		if ValidWithPrefix(id, PrefixEnvironmentKey) {
			t.Errorf("%s: %q should be invalid under %q", name, id, PrefixEnvironmentKey)
		}
	}
	// The prefix is the caller's to name, and naming a different one rejects an
	// otherwise well-formed id — which is what makes this safe to reuse.
	if ValidWithPrefix(good, PrefixAgent) {
		t.Errorf("%q must not validate under the agent prefix", good)
	}
}
