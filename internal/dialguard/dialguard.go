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
	// The address itself, and then — because an IPv6 transition address
	// forwards to an embedded IPv4 target through a translator on the
	// deployment path — every IPv4 target a translator could reach through it,
	// so 64:ff9b::7f00:1 cannot smuggle 127.0.0.1 past the guard.
	//
	// Both, never one instead of the other. Replacing the address with its
	// decoded target would make the guard depend on the decoder being right
	// about every prefix: ::1 decodes to the IPv4 bytes 0.0.0.1, which is
	// loopback under no rule at all, so a decoder that reached it would turn
	// the most obvious refusal in the list into an admission. Checking each in
	// turn keeps a wrong decode able only to add a refusal.
	if refused(ip) {
		return fmt.Errorf("dial target %s is a disallowed address", ip)
	}
	for _, target := range embeddedIPv4(ip) {
		// A decode landing on the unspecified address is reading prefix or
		// suffix padding rather than a target — every NAT64 layout but the
		// right one does that on a shorter prefix — and refusing it would
		// refuse the legitimate address it was read from.
		if target.IsUnspecified() {
			continue
		}
		if refused(target) {
			return fmt.Errorf("dial target %s is a disallowed address", ip)
		}
	}
	return nil
}

// refused is the address-class list, applied to one address.
func refused(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast()
}

// embeddedIPv4 returns every IPv4 address an IPv6 transition form could be
// carrying — 6to4 (2002::/16, v4 in bytes 2–5), Teredo (2001:0::/32, client v4
// in the inverted low 32 bits), IPv4-compatible (::/96, v4 in the low 32 bits),
// and NAT64 (64:ff9b::/32, covering both the well-known 64:ff9b::/96 and the
// RFC 8215 local-use 64:ff9b:1::/48) — so the guard can re-check the target a
// translator would actually reach. Returns nil for a plain address.
//
// NAT64 returns six candidates because RFC 6052 §2.2 defines six prefix lengths
// (/32 /40 /48 /56 /64 /96), each embedding the four IPv4 octets at different
// offsets around the reserved u-byte, and an address on its own does not say
// which one its deployment uses. Reading only the low 32 bits — the /96 layout,
// and the obvious guess because the well-known prefix must be /96 — is wrong
// for the other five and wrong in the admitting direction: under a /48 prefix
// 64:ff9b:1:a9fe:a9:fe00:808:808 carries 169.254.169.254 in bytes 6,7,9,10 and
// nothing but padding and suffix in the low 32, so a low-32 reader sees 8.8.8.8
// and lets the metadata endpoint through. Trying all six costs a handful of
// byte loads and cannot admit anything, since the caller refuses on any
// candidate rather than on a chosen one.
//
// It does over-refuse, and not rarely: a public address under one layout is
// arbitrary bytes under the other five, and those bytes sometimes land in a
// blocked class. Measured over the whole IPv4 space, on the RFC 8215 local-use
// prefix, a legitimate target is refused for 12.8% of addresses under a /48
// mapping (whenever octet 2 or 3 is 127 or in 224-239) and 6.6% under /56 and
// /64. What makes that an acceptable price rather than a bug is where it
// falls: under a /96 mapping, and under the well-known 64:ff9b::/96 prefix —
// which is the only layout that prefix is allowed to use, and the case an
// operator gets without choosing anything — the rate is **zero**, because
// every speculative layout reads the zeroed prefix bytes and is skipped as
// padding. The cost and the risk therefore coincide exactly: a deployment
// where no layout ambiguity exists pays nothing, and a deployment where the
// bypass is real is the one that pays. An operator debugging an inexplicably
// refused NAT64 address on a local-use prefix is looking at this paragraph.
//
// IPv4-compatible is here because Go does not decode it for us and nothing else
// catches it. ::ffff:127.0.0.1 (IPv4-*mapped*) is safe without any of this —
// To4 returns 127.0.0.1 and IsLoopback says yes — which makes it easy to assume
// the deprecated ::127.0.0.1 (IPv4-*compatible*) behaves the same way. It does
// not: To4 returns nil and every net.IP class predicate returns false, so
// without this case the guard admits it. It is listed as hardening, not as a
// patched bypass: a connect to ::127.0.0.1 does not reach a listener on
// 127.0.0.1 on either platform (Linux answers ENETUNREACH, macOS drops the SYN),
// so the address only goes anywhere on a host configured for 6-over-4
// tunneling. Refusing it costs nothing and does not depend on that remaining
// true.
func embeddedIPv4(ip net.IP) []net.IP {
	b := ip.To16()
	if b == nil || ip.To4() != nil {
		return nil
	}
	switch {
	case b[0] == 0x20 && b[1] == 0x02:
		return []net.IP{v4(b[2], b[3], b[4], b[5])}
	case b[0] == 0x20 && b[1] == 0x01 && b[2] == 0x00 && b[3] == 0x00:
		return []net.IP{v4(^b[12], ^b[13], ^b[14], ^b[15])}
	case b[0] == 0x00 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b:
		// RFC 6052 §2.2, one row per prefix length. Byte 8 is the reserved
		// u-octet and is never part of the address, which is why the octets
		// skip it rather than running consecutively.
		return []net.IP{
			v4(b[4], b[5], b[6], b[7]),     // /32
			v4(b[5], b[6], b[7], b[9]),     // /40
			v4(b[6], b[7], b[9], b[10]),    // /48
			v4(b[7], b[9], b[10], b[11]),   // /56
			v4(b[9], b[10], b[11], b[12]),  // /64
			v4(b[12], b[13], b[14], b[15]), // /96
		}
	case isZero(b[:12]):
		return []net.IP{v4(b[12], b[13], b[14], b[15])}
	}
	return nil
}

func v4(a, b, c, d byte) net.IP { return net.IPv4(a, b, c, d).To4() }

func isZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
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
