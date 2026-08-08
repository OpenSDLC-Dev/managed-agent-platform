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
		{name: "6to4 wrapping loopback", ip: "2002:7f00:1::1", refused: true},
		{name: "6to4 wrapping a public address", ip: "2002:5db8:d822::1"},
		{name: "Teredo wrapping loopback", ip: "2001::5:0:0:80ff:fffe", refused: true},
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
