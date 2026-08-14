package vaultresolve_test

import (
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/vaultresolve"
)

// The reference names four normalizations and three non-matches, and this table
// is one row per clause of that sentence: "Both URLs are normalized before
// matching (scheme and host lowercased, default ports and trailing slashes
// stripped), so differences in host casing, a default port, or a trailing slash
// don't prevent a match; a different path, subdomain, or non-default port does."
//
// The rows past those seven are the readings the sentence leaves open, each
// settled toward not matching — see normalizeMCPURL's comment for why that is
// the safe direction.
func TestMCPCredentialMatchingNormalizesWhatTheReferenceSaysAndNoMore(t *testing.T) {
	const server = "https://mcp.example.com/mcp"
	for _, row := range []struct {
		name string
		cred string
		want bool
	}{
		{"the same URL", "https://mcp.example.com/mcp", true},

		// The four normalizations.
		{"an upper-case scheme", "HTTPS://mcp.example.com/mcp", true},
		{"an upper-case host", "https://MCP.Example.COM/mcp", true},
		{"the default port spelled out", "https://mcp.example.com:443/mcp", true},
		{"a trailing slash", "https://mcp.example.com/mcp/", true},

		// The three the sentence says do prevent a match.
		{"a different path", "https://mcp.example.com/other", false},
		{"a subdomain", "https://api.mcp.example.com/mcp", false},
		{"a non-default port", "https://mcp.example.com:8443/mcp", false},

		// Only the scheme and the host are lowercased, so a path is not.
		{"a differently-cased path", "https://mcp.example.com/MCP", false},
		// A scheme is not a spelling of the same server.
		{"http where the agent declares https", "http://mcp.example.com/mcp", false},
		// Named by neither clause, and as distinguishing as a path.
		{"a query string the agent's URL does not carry", "https://mcp.example.com/mcp?v=2", false},
		{"userinfo the agent's URL does not carry", "https://alice@mcp.example.com/mcp", false},
		// One trailing slash is stripped, not a run of them.
		{"a doubled trailing slash", "https://mcp.example.com/mcp//", false},
		// The path is compared in its escaped form, so a URL that only spells
		// the same path differently is a different URL. Decoding first would
		// make this one match.
		{"a percent-encoded letter in the path", "https://mcp.example.com/%6dcp", false},
		// And an escaped separator is not the separator.
		{"an escaped slash in the path", "https://mcp.example.com/mc%2Fp", false},

		{"not a URL at all", "://", false},
		{"an ftp URL", "ftp://mcp.example.com/mcp", false},
		{"the empty string", "", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := vaultresolve.MatchesMCPServerForTest(row.cred, server); got != row.want {
				t.Errorf("matching %q against %q = %v, want %v", row.cred, server, got, row.want)
			}
		})
	}
}

// The default port is stripped for the scheme it is the default of, and is a
// distinguishing port for the other — which the match table cannot show, since
// a differing scheme already fails those pairs.
func TestMCPCredentialMatchingStripsOnlyTheSchemesOwnDefaultPort(t *testing.T) {
	for _, row := range []struct{ raw, want string }{
		{"http://mcp.example.com:80/mcp", "http://mcp.example.com/mcp"},
		{"https://mcp.example.com:443/mcp", "https://mcp.example.com/mcp"},
		{"http://mcp.example.com:443/mcp", "http://mcp.example.com:443/mcp"},
		{"https://mcp.example.com:80/mcp", "https://mcp.example.com:80/mcp"},
		// A bare host normalizes to itself; the trailing slash is the only path.
		{"https://mcp.example.com/", "https://mcp.example.com"},
		{"https://mcp.example.com", "https://mcp.example.com"},
		// The port is a number, and Go dials these as the numbers they spell.
		// Compared as text they would each be a server of their own.
		{"https://mcp.example.com:0443/mcp", "https://mcp.example.com/mcp"},
		{"http://mcp.example.com:080/mcp", "http://mcp.example.com/mcp"},
		{"https://mcp.example.com:/mcp", "https://mcp.example.com/mcp"},
		{"https://mcp.example.com:08443/mcp", "https://mcp.example.com:08443/mcp"},
	} {
		t.Run(row.raw, func(t *testing.T) {
			got, ok := vaultresolve.NormalizeMCPURLForTest(row.raw)
			if !ok || got != row.want {
				t.Errorf("normalize(%q) = %q, %v; want %q, true", row.raw, got, ok, row.want)
			}
		})
	}
}

// A string that is not an http(s) URL with a host is refused outright rather
// than reduced to some key of its own. Asserted on the normalization directly,
// because a match against a well-formed server URL fails for these whatever the
// refusal does — and the refusal is what keeps two *unnormalizable* strings from
// both reducing to "" and matching each other.
func TestMCPCredentialMatchingRefusesWhatItCannotNormalize(t *testing.T) {
	for _, raw := range []string{
		"ftp://mcp.example.com/mcp",
		"mailto:ops@example.com",
		"https:///mcp",
		"/mcp",
		"://",
		"",
	} {
		t.Run(raw, func(t *testing.T) {
			if got, ok := vaultresolve.NormalizeMCPURLForTest(raw); ok {
				t.Errorf("normalize(%q) = %q, true; want it refused", raw, got)
			}
		})
	}
	if vaultresolve.MatchesMCPServerForTest("://", "://") {
		t.Error("two strings neither side can normalize must not match each other")
	}
}

// An IPv6 zone identifier is locally significant and case-sensitive: folding it
// is the one way this could equate two hosts that are not the same one. The
// address in front of it is folded like any other host. (url.Parse decodes the
// zone's required "%25" into a bare "%", so that is the form the key carries —
// both sides of a comparison come through the same decode.)
func TestMCPCredentialMatchingKeepsAnIPv6ZoneCase(t *testing.T) {
	const withZone = "https://[FE80::1%25eth0]/mcp"
	got, ok := vaultresolve.NormalizeMCPURLForTest(withZone)
	if !ok {
		t.Fatalf("normalize(%q) reported not-a-URL", withZone)
	}
	if want := "https://[fe80::1%eth0]/mcp"; got != want {
		t.Errorf("normalize(%q) = %q, want the address folded and the zone kept: %q", withZone, got, want)
	}
	if vaultresolve.MatchesMCPServerForTest(withZone, "https://[fe80::1%25ETH0]/mcp") {
		t.Error("two link-local addresses differing only in zone case must not match")
	}
}

// Case folding a host name is an ASCII operation. Unicode's is not the same
// operation and maps several letters onto ASCII ones — strings.ToLower turns
// U+0130 (İ) into a plain "i" — so folding by Unicode would put an IDN and an
// unrelated ASCII domain into one key and send that credential's token to
// whichever of them an agent named. They are not one host: Go's HTTP stack
// resolves the IDN through IDNA to xn--i-9bb.example.
func TestMCPCredentialMatchingFoldsHostCaseAsASCIINotAsUnicode(t *testing.T) {
	const idn = "https://İ.example/mcp"
	if vaultresolve.MatchesMCPServerForTest(idn, "https://i.example/mcp") {
		t.Error("a credential for İ.example was matched to i.example, a different DNS name")
	}
	if vaultresolve.MatchesMCPServerForTest(idn, "https://I.example/mcp") {
		t.Error("a credential for İ.example was matched to I.example, a different DNS name")
	}
	// The ASCII fold itself still has to work, or the guard above is just a
	// comparison that never matches anything.
	if !vaultresolve.MatchesMCPServerForTest("https://MCP.Example/mcp", "https://mcp.example/mcp") {
		t.Error("an ASCII host's case is not significant and must still fold")
	}
}
