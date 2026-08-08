// Package dialguard blocks the addresses a platform-initiated outbound
// connection must never reach.
//
// Anything that dials a URL supplied by a customer — a vault credential's MCP
// server or token endpoint, an agent's `mcp_servers` entry — is an SSRF vector,
// and one that returns response bodies is a full-read vector. The guard refuses
// the address classes that are never a legitimate third-party endpoint but are
// prime exfiltration targets: loopback (the platform's own surfaces),
// link-local (cloud metadata, 169.254.169.254 / fe80::/10), the unspecified
// address, and multicast.
//
// Two properties matter more than the list. The check runs on the *resolved* IP
// at connect time (net.Dialer.Control), on every dial, so DNS rebinding cannot
// slip a blocked address past a name that resolved innocently a moment earlier.
// And RFC 1918 private ranges are deliberately allowed: this platform's premise
// is on-prem / in-VPC operation (CLAUDE.md), where MCP servers and token
// endpoints legitimately live on the operator's own private network — the
// address-based guard is therefore not the network policy, only the floor
// beneath it (an agent's egress policy is enforced separately, and refusing
// RFC 1918 here would break the deployment model rather than protect it).
//
// Redirects are a separate matter this package does not address: a caller that
// follows one replays its request body to a new target, which a per-hop IP
// check cannot see is wrong. Callers refuse to follow them
// (http.ErrUseLastResponse).
package dialguard

import (
	"fmt"
	"net"
	"syscall"
)

// IPAllowed reports whether a resolved address may be dialed, returning an
// error naming the refusal. The error text says only that the address is
// disallowed: a caller that surfaces it must not reveal whether an internal
// host exists.
func IPAllowed(ip net.IP) error {
	// An IPv6 transition address forwards to an embedded IPv4 target through a
	// translator on the deployment path; check that real target, not the v6
	// wrapper, so 64:ff9b::7f00:1 cannot smuggle 127.0.0.1 past the guard.
	target := ip
	if v4 := embeddedIPv4(ip); v4 != nil {
		target = v4
	}
	switch {
	case target.IsLoopback(), target.IsLinkLocalUnicast(), target.IsLinkLocalMulticast(),
		target.IsUnspecified(), target.IsMulticast():
		return fmt.Errorf("dial target %s is a disallowed address", ip)
	default:
		return nil
	}
}

// embeddedIPv4 returns the IPv4 address wrapped by an IPv6 transition form —
// NAT64 (the whole 64:ff9b::/32, covering both the 64:ff9b::/96 well-known and
// 64:ff9b:1::/48 local prefixes; v4 in the low 32 bits), 6to4 (2002::/16, v4 in
// bytes 2–5), and Teredo (2001:0::/32, client v4 in the inverted low 32 bits) —
// so the guard re-checks the target a translator would actually reach. The
// NAT64 match is deliberately broad and assumes /96-style low-32 embedding: a
// mis-decode can only add a refusal, never an admit, so it stays fail-safe.
// Returns nil for a plain address.
func embeddedIPv4(ip net.IP) net.IP {
	b := ip.To16()
	if b == nil || ip.To4() != nil {
		return nil
	}
	switch {
	case b[0] == 0x20 && b[1] == 0x02:
		return net.IPv4(b[2], b[3], b[4], b[5]).To4()
	case b[0] == 0x20 && b[1] == 0x01 && b[2] == 0x00 && b[3] == 0x00:
		return net.IPv4(^b[12], ^b[13], ^b[14], ^b[15]).To4()
	case b[0] == 0x00 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b:
		return net.IPv4(b[12], b[13], b[14], b[15]).To4()
	}
	return nil
}

// Control builds a net.Dialer Control hook that runs allow on the resolved
// address of every dial. allow is taken as a parameter rather than called
// directly so a caller can keep its own overridable seam (a test pointing at an
// httptest server on loopback needs one), and it is read per dial rather than
// captured once for the same reason.
func Control(allow func(net.IP) error) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return fmt.Errorf("dial address %q did not resolve to an IP", address)
		}
		return allow(ip)
	}
}
