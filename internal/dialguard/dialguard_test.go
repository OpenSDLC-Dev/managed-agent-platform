package dialguard_test

import (
	"net"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/dialguard"
)

func TestIPAllowed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		ip      string
		refused bool
	}{
		{name: "IPv4 loopback", ip: "127.0.0.1", refused: true},
		{name: "IPv4 loopback, not just .0.1", ip: "127.9.9.9", refused: true},
		// ::1 is also load-bearing for the shape of the check: its low 32 bits
		// decode to the IPv4 bytes 0.0.0.1, which no rule calls loopback, so a
		// guard that replaced the address with its decoded target instead of
		// checking both would admit it.
		{name: "IPv6 loopback", ip: "::1", refused: true},
		{name: "IPv4 link-local (cloud metadata)", ip: "169.254.169.254", refused: true},
		{name: "IPv6 link-local", ip: "fe80::1", refused: true},
		{name: "unspecified v4", ip: "0.0.0.0", refused: true},
		{name: "unspecified v6", ip: "::", refused: true},
		{name: "multicast v4", ip: "224.0.0.1", refused: true},
		{name: "multicast v6", ip: "ff02::1", refused: true},
		{name: "link-local multicast v4", ip: "224.0.0.251", refused: true},

		// RFC 1918 is deliberately allowed: the platform's premise is on-prem
		// operation, where an MCP server legitimately lives on the operator's
		// own private network. Refusing these would break the deployment model,
		// not protect it — so they are asserted allowed, not left untested.
		{name: "RFC 1918 ten", ip: "10.0.0.5"},
		{name: "RFC 1918 172.16", ip: "172.16.31.9"},
		{name: "RFC 1918 192.168", ip: "192.168.1.1"},
		{name: "IPv6 unique local", ip: "fd00::1"},
		{name: "public v4", ip: "93.184.216.34"},
		{name: "public v6", ip: "2606:2800:220:1:248:1893:25c8:1946"},

		// An IPv6 transition address forwards to an embedded IPv4 target
		// through a translator, so the guard has to check the target a
		// translator would actually reach rather than the v6 wrapper.
		{name: "NAT64 well-known prefix wrapping loopback", ip: "64:ff9b::7f00:1", refused: true},
		{name: "NAT64 local prefix wrapping link-local", ip: "64:ff9b:1::a9fe:a9fe", refused: true},
		{name: "NAT64 wrapping a public address", ip: "64:ff9b::5db8:d822"},
		// Trying six layouts can over-refuse, so the layouts a real deployment
		// uses to reach a real address are asserted allowed, not assumed to be.
		{name: "NAT64 local prefix, /96 layout, public address", ip: "64:ff9b:1::808:808"},
		{name: "NAT64 local prefix, /64 layout, public address", ip: "64:ff9b:1:2:8:808:800:0"},

		// RFC 6052 embeds the four IPv4 octets at a different offset for each
		// of its six prefix lengths, and an address does not say which its
		// deployment uses. This one is the /48 layout: bytes 6,7,9,10 carry
		// 169.254.169.254 while the low 32 bits — the only place a /96 reader
		// looks — carry 8.8.8.8. A guard that reads just the low 32 bits calls
		// it Google DNS and dials cloud metadata.
		{name: "NAT64 /48 layout wrapping cloud metadata", ip: "64:ff9b:1:a9fe:a9:fe00:808:808", refused: true},
		{name: "NAT64 /40 layout wrapping loopback", ip: "64:ff9b:7f:0:1::", refused: true},
		// The row above does not actually distinguish this guard from the low-32
		// one it replaced — its own low 32 bits are zero, so the old code
		// refused it too, as the unspecified address. This one does: the /40
		// reading is 127.0.0.1 while the low 32 read as the public 8.8.8.8, so
		// the old guard admitted it and only a real /40 decode refuses it.
		{name: "NAT64 /40 layout wrapping loopback, non-zero suffix", ip: "64:ff9b:7f:0:1:0:808:808", refused: true},

		// Two deliberate false refusals, pinned so they stay decisions rather
		// than surprises. The first depends on the target: under a /48 mapping
		// it carries the public 8.127.8.8, but read as /56 the same bytes are
		// 127.8.8.0 — loopback — and the guard cannot tell which layout the
		// deployment meant. That costs 12.8% of targets under a /48 mapping on
		// the local-use prefix.
		{name: "NAT64 /48 layout, public target, refused by the /56 reading", ip: "64:ff9b:1:87f:8:800::", refused: true},

		// The second depends on nothing but the prefix, which is the worse
		// case and the reason "a /96 mapping costs nothing" is false as a
		// general claim: 64:ff9b:1:7f00::/96 is a legal NSP whose own bytes
		// read as 127.0.0.0 under the /48 layout, so every address it can
		// express is refused however innocent the target. See the note on
		// embeddedIPv4 for why the guard cannot do better with an address
		// alone, and what a deployment that hits this would need.
		{name: "NAT64 /96 NSP whose prefix bytes read as loopback", ip: "64:ff9b:1:7f00::808:808", refused: true},

		// The other direction of the same trade, pinned because it is the one
		// that reads as a loosening. Skipping a candidate that decodes to the
		// unspecified address is what lets these through, and the old low-32
		// guard refused all of them: RFC 6052 puts the zero suffix in the low 32
		// bits for every layout from /32 to /56, so reading only those bits saw
		// 0.0.0.0 and refused every conformant /48 and /56 mapping — the two
		// layouts a deployment can use under this prefix — which is the same
		// misreading that admitted the metadata address above, pointing the
		// other way. The first row is the ordinary case that restores. The next
		// two are prefix base addresses, where the decode is padding all the way
		// down and no host exists to reach. The general shape of what the skip
		// admits is already above as the /96-layout row: 64:ff9b:1::808:808 has
		// a /48 reading that is padding and a /96 reading that is 8.8.8.8, and
		// nothing in it says which the deployment meant.
		{name: "NAT64 /48 layout, ordinary public target", ip: "64:ff9b:1:808:8:800::"},
		{name: "NAT64 well-known prefix base address", ip: "64:ff9b::"},
		{name: "NAT64 local-use prefix base address", ip: "64:ff9b:1::"},

		// RFC 6052 lets an operator assign a Network-Specific Prefix out of
		// their own space, and nothing in an address marks it as one. Only
		// 64:ff9b::/32 is decoded, so a deployment translating through its own
		// NSP gets no NAT64 decoding at all and this reaches cloud metadata.
		// Closing it needs the guard told its prefix — the same knob the
		// over-refusal above would want, and equally not built.
		{name: "NAT64 through an operator NSP is not decoded", ip: "2001:db8:122:344::a9fe:a9fe"},

		// ISATAP lives in the interface identifier, so the prefix is arbitrary
		// and a documentation-only prefix carries a real one here. The u bit is
		// set when the embedded address is globally unique, so both 0:5efe and
		// 200:5efe are the same form and both must decode.
		{name: "ISATAP wrapping loopback", ip: "2001:db8:1234:5678:0:5efe:7f00:1", refused: true},
		{name: "ISATAP wrapping cloud metadata, u bit set", ip: "2001:db8::200:5efe:a9fe:a9fe", refused: true},
		{name: "ISATAP wrapping a public address", ip: "2001:db8:1234:5678:0:5efe:5db8:d822"},
		// Not ISATAP: the OUI has to match, or every address whose bytes happen
		// to sit there would be read as a tunnel.
		{name: "ISATAP-shaped but wrong OUI", ip: "2001:db8:1234:5678:0:5eff:7f00:1"},

		{name: "6to4 wrapping loopback", ip: "2002:7f00:1::1", refused: true},
		{name: "6to4 wrapping a public address", ip: "2002:5db8:d822::1"},
		{name: "Teredo wrapping loopback", ip: "2001::5:0:0:80ff:fffe", refused: true},

		// IPv4-mapped (::ffff:a.b.c.d) is classified by net.IP itself, so these
		// rows guard against a refactor that stopped it doing so rather than
		// against the decoder.
		{name: "IPv4-mapped loopback", ip: "::ffff:127.0.0.1", refused: true},
		{name: "IPv4-mapped cloud metadata", ip: "::ffff:169.254.169.254", refused: true},
		{name: "IPv4-mapped public address", ip: "::ffff:93.184.216.34"},

		// IPv4-compatible (::a.b.c.d) is the one net.IP does not classify: every
		// class predicate returns false and To4 returns nil, so without an
		// explicit decode the guard admits it. Stock kernels will not route it
		// (see the note in dialguard.go), which is why these rows assert the
		// guard's own answer rather than reachability — the guard must not
		// depend on the kernel to be the thing that says no.
		{name: "IPv4-compatible loopback", ip: "::127.0.0.1", refused: true},
		{name: "IPv4-compatible cloud metadata", ip: "::169.254.169.254", refused: true},
		{name: "IPv4-compatible multicast", ip: "::224.0.0.1", refused: true},
		{name: "IPv4-compatible public address", ip: "::93.184.216.34"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("test fixture %q is not an IP", tc.ip)
			}
			err := dialguard.IPAllowed(ip)
			switch {
			case tc.refused && err == nil:
				t.Fatalf("IPAllowed(%s) = nil, want a refusal", tc.ip)
			case !tc.refused && err != nil:
				t.Fatalf("IPAllowed(%s) = %v, want nil", tc.ip, err)
			}
			// The refusal must not confirm anything about the target beyond
			// the address class: a caller surfacing it must not become an
			// internal-host oracle.
			if err != nil && !strings.Contains(err.Error(), "disallowed address") {
				t.Errorf("refusal %q does not read as an address-class refusal", err)
			}
		})
	}
}

func TestControlChecksTheResolvedAddress(t *testing.T) {
	t.Parallel()
	// The hook receives the dialer's "host:port", which is the resolved
	// address rather than the name in the URL — that is the whole point, since
	// a name that resolved innocently a moment ago can resolve to loopback on
	// the next lookup.
	var seen []net.IP
	control := dialguard.Control(func(ip net.IP) error {
		seen = append(seen, ip)
		return dialguard.IPAllowed(ip)
	})

	if err := control("tcp", "127.0.0.1:8443", nil); err == nil {
		t.Error("Control admitted loopback")
	}
	if err := control("tcp", "[fd00::1]:443", nil); err != nil {
		t.Errorf("Control refused a private address: %v", err)
	}
	if len(seen) != 2 || !seen[0].IsLoopback() {
		t.Fatalf("the hook saw %v, want the two resolved addresses", seen)
	}
}

func TestControlRejectsWhatItCannotParse(t *testing.T) {
	t.Parallel()
	// A hook that cannot tell what it is dialing must refuse rather than pass:
	// admitting an address it failed to parse would make the guard fail-open
	// on exactly the inputs it does not understand.
	called := false
	control := dialguard.Control(func(net.IP) error {
		called = true
		return nil
	})

	if err := control("tcp", "not-an-address", nil); err == nil {
		t.Error("Control admitted an address with no port")
	}
	if err := control("tcp", "example.com:443", nil); err == nil {
		t.Error("Control admitted an unresolved host name")
	}
	if called {
		t.Error("the allow predicate ran on an address the hook could not resolve")
	}
}
