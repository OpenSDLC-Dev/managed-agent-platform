package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func FuzzIDValidation(f *testing.F) {
	for _, seed := range []string{
		"",
		"agent_0123456789abcdefghjkmnpq",
		"sesn_0123456789abcdefghjkmnpq",
		"session_0123456789abcdefghjkmnpq",
		"skver_0123456789abcdefghjkmnpq",
		"skillver_0123456789abcdefghjkmnpq",
		"unknown_0123456789",
		"agent_",
		"agent_INVALID_I_O_l",
		"sesn_with\x00nul",
		"sesn_unicode_测试",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		id := ID(raw)
		valid := id.Valid()
		p := id.Prefix()
		if valid {
			if strings.ContainsRune(raw, 0) {
				t.Fatalf("Valid() accepted NUL byte in %q", raw)
			}
			for i := 0; i < len(raw); i++ {
				if raw[i] >= 0x80 {
					t.Fatalf("Valid() accepted non-ASCII byte in %q", raw)
				}
			}
			if p == "" || !knownPrefixes[p] {
				t.Fatalf("Valid() accepted unknown prefix %q in %q", p, raw)
			}
			if !WellFormedID(raw, p) {
				t.Fatalf("Valid() ID %q rejected by WellFormedID(_, %q)", raw, p)
			}
			if !id.HasPrefix(p) {
				t.Fatalf("Valid() ID %q failed HasPrefix(%q)", raw, p)
			}
		}
		// PrimaryThreadID must never panic and must always carry the sthr_ prefix.
		pt := PrimaryThreadID(id)
		if !pt.HasPrefix(PrefixSessionThread) {
			t.Fatalf("PrimaryThreadID(%q) = %q missing %q prefix", raw, pt, PrefixSessionThread)
		}
	})
}

func TestIDAndEventRoundTripProperties(t *testing.T) {
	canonical := []string{
		PrefixAgent, PrefixEnvironment, PrefixSession, PrefixEvent, PrefixWork,
		PrefixVault, PrefixCredential, PrefixResource, PrefixDeployment,
		PrefixDeploymentRun, PrefixFile, PrefixSkill, PrefixSkillVersion,
		PrefixOutcome, PrefixSessionThread, PrefixMemoryStore, PrefixMemory,
		PrefixMemoryVersion, PrefixDream,
	}
	seen := make(map[ID]bool, len(canonical)*25)
	for _, prefix := range canonical {
		for range 25 {
			id := NewID(prefix)
			if seen[id] {
				t.Fatalf("duplicate NewID(%q): %q", prefix, id)
			}
			seen[id] = true
			if !id.Valid() {
				t.Fatalf("NewID(%q) = %q is not Valid()", prefix, id)
			}
			if !id.HasPrefix(prefix) {
				t.Fatalf("NewID(%q) = %q failed HasPrefix", prefix, id)
			}
			if !WellFormedID(string(id), prefix) {
				t.Fatalf("NewID(%q) = %q failed WellFormedID", prefix, id)
			}
		}
	}

	// PrimaryThreadID equivalence across sesn_ and legacy session_ wire alias.
	for range 50 {
		sesn := NewID(PrefixSession)
		alias := ID(altSessionPrefix + "_" + strings.TrimPrefix(string(sesn), PrefixSession+"_"))
		if !alias.Valid() || !alias.HasPrefix(PrefixSession) {
			t.Fatalf("session_ alias %q did not validate", alias)
		}
		pt1, pt2 := PrimaryThreadID(sesn), PrimaryThreadID(alias)
		if pt1 != pt2 || !pt1.Valid() {
			t.Fatalf("PrimaryThreadID mismatch: %q vs %q (valid=%v)", pt1, pt2, pt1.Valid())
		}
	}

	// Wire-tagged domain structs round-trip cleanly through encoding/json.
	speed := "standard"
	usage := ModelUsage{
		CacheCreationInputTokens: 10,
		CacheReadInputTokens:     20,
		InputTokens:              300,
		OutputTokens:             45,
		Speed:                    &speed,
	}
	b, err := json.Marshal(usage)
	if err != nil {
		t.Fatalf("marshal ModelUsage: %v", err)
	}
	var decUsage ModelUsage
	if err := json.Unmarshal(b, &decUsage); err != nil {
		t.Fatalf("unmarshal ModelUsage: %v", err)
	}
	if decUsage.InputTokens != usage.InputTokens || decUsage.OutputTokens != usage.OutputTokens ||
		decUsage.CacheCreationInputTokens != usage.CacheCreationInputTokens ||
		decUsage.CacheReadInputTokens != usage.CacheReadInputTokens ||
		decUsage.Speed == nil || *decUsage.Speed != speed {
		t.Fatalf("ModelUsage round-trip mismatch: got %+v, want %+v", decUsage, usage)
	}
	sr := StopReason{Type: StopRequiresAction, EventIDs: []ID{NewID(PrefixEvent), NewID(PrefixEvent)}}
	sb, err := json.Marshal(sr)
	if err != nil {
		t.Fatalf("marshal StopReason: %v", err)
	}
	var decSR StopReason
	if err := json.Unmarshal(sb, &decSR); err != nil {
		t.Fatalf("unmarshal StopReason: %v", err)
	}
	if decSR.Type != sr.Type || len(decSR.EventIDs) != 2 || decSR.EventIDs[0] != sr.EventIDs[0] || decSR.EventIDs[1] != sr.EventIDs[1] {
		t.Fatalf("StopReason round-trip mismatch: got %+v, want %+v", decSR, sr)
	}
}
