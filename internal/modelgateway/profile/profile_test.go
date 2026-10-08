package profile_test

import (
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
)

// Every profile is reachable by its name, and each host is an https URL with
// no path suffix a client would double, on a protocol the profile speaks.
func TestProfilesAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range profile.All() {
		if seen[p.Name] {
			t.Errorf("profile %q listed twice", p.Name)
		}
		seen[p.Name] = true
		if got, ok := profile.Lookup(p.Name); !ok || got.Name != p.Name {
			t.Errorf("Lookup(%q) = %v, %t", p.Name, got.Name, ok)
		}
		if p.DisplayName == "" || len(p.Protocols) == 0 {
			t.Errorf("profile %q lacks a display name or protocols", p.Name)
		}
		for _, h := range p.Hosts {
			if !p.Supports(h.Protocol) {
				t.Errorf("%s: host %s on a protocol the profile does not speak", p.Name, h.BaseURL)
			}
			u, err := url.Parse(h.BaseURL)
			if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || strings.HasSuffix(h.BaseURL, "/") {
				t.Errorf("%s: host %q is not a bare https base URL", p.Name, h.BaseURL)
			}
		}
	}
	if _, ok := profile.Lookup("nope"); ok {
		t.Error("Lookup found a profile that does not exist")
	}
}

// The four chat vendors publish both protocols, and the generic profiles carry
// no host: a provider on one names its own endpoint.
func TestVendorAndGenericProfiles(t *testing.T) {
	for _, name := range []string{"deepseek", "minimax", "zhipu", "moonshot"} {
		p, ok := profile.Lookup(name)
		if !ok {
			t.Fatalf("no %s profile", name)
		}
		for _, proto := range []profile.Protocol{profile.Anthropic, profile.OpenAI} {
			if !slices.ContainsFunc(p.Hosts, func(h profile.Host) bool { return h.Protocol == proto }) {
				t.Errorf("%s has no %s host", name, proto)
			}
		}
	}
	for name, proto := range map[string]profile.Protocol{"anthropic-generic": profile.Anthropic, "openai-generic": profile.OpenAI} {
		p, ok := profile.Lookup(name)
		if !ok {
			t.Fatalf("no %s profile", name)
		}
		if len(p.Hosts) != 0 || !slices.Equal(p.Protocols, []profile.Protocol{proto}) {
			t.Errorf("%s = %+v, want only %s and no hosts", name, p, proto)
		}
	}
}

// Gitee AI serves embeddings and rerank on its OpenAI host alone, and is the
// one profile whose connections are not reused.
func TestGiteeProfile(t *testing.T) {
	p, ok := profile.Lookup("gitee")
	if !ok || !slices.Equal(p.Protocols, []profile.Protocol{profile.OpenAI}) ||
		!slices.Equal(p.Hosts, []profile.Host{{Protocol: profile.OpenAI, BaseURL: "https://ai.gitee.com/v1"}}) {
		t.Fatalf("gitee = %+v", p)
	}
	for _, q := range profile.All() {
		if q.CloseConnections != (q.Name == "gitee") {
			t.Errorf("%s closes its connections: %t", q.Name, q.CloseConnections)
		}
	}
}

// All returns a copy: a caller that edits what it got cannot change the
// profiles every other caller reads.
func TestAllReturnsACopy(t *testing.T) {
	ps := profile.All()
	ps[0].Hosts[0].BaseURL = "https://evil.example"
	ps[0].Name = "changed"
	ps[0].ChatThinking["enabled"] = "changed"
	if got := profile.All(); got[0].Name == "changed" || got[0].Hosts[0].BaseURL == "https://evil.example" || got[0].ChatThinking["enabled"] == "changed" {
		t.Error("All shares its backing data with callers")
	}
}
