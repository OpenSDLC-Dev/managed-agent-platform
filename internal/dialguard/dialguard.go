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
		// A decode landing on the unspecified address is reading padding
		// rather than a target, and must not refuse the address it was read
		// from. RFC 6052 §2.2 puts the suffix — SHOULD-zero — in the low 32
		// bits for every prefix length from /32 to /56, so a reader of those
		// bits sees 0.0.0.0 for *every* conformant mapping under four of the
		// six layouts. That is not a hypothetical: it is what the guard this
		// package replaced did, refusing 64:ff9b:1:808:8:800:: (a /48 mapping
		// of 8.8.8.8) as readily as it admitted the /48 mapping of cloud
		// metadata below. One misreading, pointing both ways.
		//
		// The skip is therefore what makes the six-candidate check usable, and
		// its own cost is small and stated: an address whose every non-padding
		// reading is benign is admitted, which includes 64:ff9b:: and
		// 64:ff9b:1:: themselves — prefix base addresses where a translator has
		// no target and no host is listening. The unspecified address is in the
		// refusal list because connect(0.0.0.0) reaches the local host, and
		// that is a property of a local dial, not of a destination a translator
		// forwards to.
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
// byte loads, and adding candidates only ever adds refusals: the caller refuses
// on any candidate rather than on a chosen one. What it is *not* is purely
// additive against the low-32 guard this replaced, because that guard read the
// zero suffix as a target — see the unspecified skip in IPAllowed for what that
// cost and what admitting it back costs in turn.
//
// Only 64:ff9b::/32 is recognized. RFC 6052 equally allows a Network-Specific
// Prefix from an operator's own space, and an address carries no mark saying it
// is one, so a deployment translating through (say) 2001:db8:122:344::/96 gets
// no NAT64 decoding here at all and its wrapped metadata address is admitted.
// That is the same missing knob as the over-refusal below — a guard told its
// deployment's prefix would fix both — and it is why this decoding is hardening
// on the one prefix an attacker can rely on being routed, not a general
// solution to NAT64.
//
// It does over-refuse, and how much depends on the deployment's prefix bytes
// rather than on its prefix length — which is worth stating precisely, because
// the tempting summary ("a /96 mapping costs nothing") is false.
//
// The well-known 64:ff9b::/96 costs exactly nothing, always: bytes 4-11 are
// zero by definition, so every speculative layout reads padding and is skipped.
// That is the case an operator gets without choosing anything, and RFC 6052
// fixes that prefix at /96, so there is no ambiguity there to pay for.
//
// On the RFC 8215 local-use 64:ff9b:1::/48 the cost is real and ranges from
// nothing to everything. With the remaining prefix bytes zero it is 12.8% of
// targets under a /48 mapping (whenever octet 2 or 3 of the target is 127 or in
// 224-239), 6.6% under /56 and /64, and 0% under /96 — measured over the whole
// IPv4 space. But an operator may carve an NSP whose own fixed bytes read as a
// blocked class under a shorter layout, and then the refusal does not depend on
// the target at all: under 64:ff9b:1:7f00::/96 the /48 reading of the prefix is
// 127.0.0.0, so **every** address that prefix can express is refused. Nothing
// here can tell that apart from a genuine /48 mapping of loopback.
//
// That is the honest shape of the trade: fail-closed, sometimes total, and
// unavoidable for a guard that is handed an address and not the layout that
// produced it. The escape hatch, if a deployment ever hits it, is to tell the
// guard its NAT64 prefix and collapse the six candidates to one — a knob
// nothing needs today, and one this design does not foreclose. An operator
// debugging an inexplicably refused NAT64 address is looking at this paragraph.
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
