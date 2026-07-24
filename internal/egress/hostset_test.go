package egress_test

import (
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
)

func TestHostSetMatch(t *testing.T) {
	set := egress.NewHostSet([]string{
		"example.com",   // exact hostname
		"10.1.2.3",      // exact IPv4
		"*.api.test",    // wildcard: any subdomain of api.test, not the apex
		"MixedCase.org", // stored uppercase; matching is case-insensitive
	})

	cases := []struct {
		host string
		want bool
	}{
		// Exact hostname.
		{"example.com", true},
		{"EXAMPLE.COM", true},    // request host case-insensitive
		{"example.com.", true},   // trailing FQDN dot tolerated
		{"a.example.com", false}, // exact entry is not a wildcard
		{"example.org", false},

		// Exact IPv4.
		{"10.1.2.3", true},
		{"10.1.2.4", false},

		// Wildcard *.api.test — subdomains at any depth, never the apex.
		{"v1.api.test", true},
		{"a.b.api.test", true},
		{"api.test", false},  // apex is excluded
		{"xapi.test", false}, // must be a label boundary, not a suffix
		{"api.test.evil.com", false},

		// Case-insensitive on both stored entry and query.
		{"mixedcase.org", true},
		{"MIXEDCASE.ORG", true},

		// A host with an empty label never matches — not a wildcard (the
		// boundary a naive HasSuffix would leak) and not an exact entry.
		{".api.test", false},
		{"a..api.test", false},
		{"..api.test", false},
		{".example.com", false},

		// Unknown host.
		{"other.net", false},
		{"", false},
	}
	for _, c := range cases {
		if got := set.Match(c.host); got != c.want {
			t.Errorf("Match(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

func TestHostSetEmpty(t *testing.T) {
	// A nil/empty set matches nothing (a credential with no allowed_hosts, or an
	// environment allow-list that admits nothing, never admits a request).
	if egress.NewHostSet(nil).Match("example.com") {
		t.Error("empty host set must match nothing")
	}
	// A nil *HostSet (a credential resolved without a host list) must not panic.
	var nilSet *egress.HostSet
	if nilSet.Match("example.com") {
		t.Error("nil host set must match nothing")
	}
}
