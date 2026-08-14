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
	"errors"
	"fmt"
	"net"
	"syscall"
)

// ErrRefused is wrapped by every refusal this guard produces, so a caller can
// tell a destination that can never be dialled from a network that may recover:
// no retry makes a refused address reachable. The messages are unchanged — the
// sentinel is the phrase they already ended with.
var ErrRefused = errors.New("disallowed address")

// IPAllowed reports whether a resolved address may be dialed, returning an
// error naming the refusal. The text names the resolved address and says only
// that it is disallowed — never which class matched it, and never whether
// anything is listening there: the refusal is produced before any connect(2), so
// it reads identically either way and a caller that surfaces it cannot become an
// internal-host oracle.
func IPAllowed(ip net.IP) error {
	// An address this cannot read is refused rather than admitted. net.IP is a
	// byte slice, so a nil or wrong-length value is an ordinary value of the
	// type rather than a compile error, every predicate below answers false for
	// one, and net.ParseIP returns exactly that for anything it cannot parse —
	// so without this line the shape a caller naturally writes,
	// IPAllowed(net.ParseIP(host)), admits every host that is not an IP at all.
	if ip.To16() == nil {
		return fmt.Errorf("dial target is not a usable address: %w", ErrRefused)
	}
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
		return fmt.Errorf("dial target %s is a %w", ip, ErrRefused)
	}
	for _, target := range embeddedIPv4(ip) {
		// A decode landing on the unspecified address is reading padding
		// rather than a target, and must not refuse the address it was read
		// from. RFC 6052 §2.2 makes the suffix SHOULD-zero and, for every prefix
		// length from /32 to /56, that suffix covers all four of the low 32
		// bits — so a reader of those bits sees 0.0.0.0 rather than a target.
		// (It is the suffix that contains them, not the other way round: at /48
		// the suffix is bytes 11-15.) For the layouts a deployment can actually
		// use under this prefix that means /48 and /56: the guard this replaced
		// refused *every* conformant mapping under both, 64:ff9b:1:808:8:800::
		// (a /48 mapping of 8.8.8.8) as readily as it admitted the /48 mapping
		// of cloud metadata below. One misreading, pointing both ways.
		//
		// (The /32 and /40 rows are arithmetic rather than deployments. RFC 6052
		// fixes the well-known prefix at /96, and the shortest prefix inside
		// 64:ff9b::/32 an operator may use is RFC 8215's local-use
		// 64:ff9b:1::/48, from which a /48, /56, /64 or /96 translation prefix
		// may be carved — so nothing legitimately maps at /32 or /40 here, and
		// the decoder tries them because an address cannot prove that.)
		//
		// The skip is therefore what makes the six-candidate check usable, and
		// dropping it would cost those two layouts entirely. Its own price is a
		// rule rather than a list: an address every one of whose non-padding
		// readings is benign is admitted even when some other layout would read
		// 0.0.0.0 from it. Under this prefix that is 64:ff9b:: and 64:ff9b:1:: —
		// prefix base addresses where a translator has no target and no host is
		// listening — and equally 64:ff9b:1::808:808, whose /48 reading is
		// padding and whose /96 reading is 8.8.8.8. It is not confined to NAT64,
		// because the skip is not: 2002::/32 (a 6to4 wrapper carrying 0.0.0.0),
		// a Teredo address whose low 32 bits are all-ones, and an ISATAP
		// identifier carrying 0.0.0.0 are admitted for the same reason and are
		// wrappers around no target in the same way. Refusing them back is not
		// available separately; it is the same rule. The unspecified address is
		// itself in the refusal list because connect(0.0.0.0) reaches the local
		// host, and that is a property of a local dial rather than of a
		// destination a translator forwards a packet to.
		if target.IsUnspecified() {
			continue
		}
		if refused(target) {
			return fmt.Errorf("dial target %s is a %w", ip, ErrRefused)
		}
	}
	return nil
}

// refused is the address-class list, applied to one address.
func refused(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast()
}

// embeddedIPv4 returns the IPv4 addresses an IPv6 transition form could be
// carrying, so the guard can re-check the target a translator would actually
// reach. Returns nil for a plain address.
//
// Six forms are decoded: 6to4 (2002::/16, v4 in bytes 2–5), Teredo
// (2001:0::/32, client v4 in the inverted low 32 bits), IPv4-compatible (::/96,
// v4 in the low 32 bits), IPv4-translated (RFC 2765 §2.1's ::ffff:0:0:0/96, v4
// likewise in the low 32 bits behind a different prefix), NAT64 (64:ff9b::/32,
// covering both the well-known 64:ff9b::/96 and the RFC 8215 local-use
// 64:ff9b:1::/48), and ISATAP (RFC 5214's 00-00-5E-FE interface identifier under
// any prefix). That is not "every transition form", and saying so would be the
// sort of claim this file exists to avoid. Two kinds are left out. 6rd carries
// an IPv4 address whose position depends on provider-assigned parameters an
// address does not contain, and a NAT64 deployment using its own
// Network-Specific Prefix is invisible for the same reason (see below); both
// need the guard told its deployment's parameters, which nothing does today.
//
// RFC 2529 6over4 is the other kind, and it is left out by decision rather than
// by blindness, so "every form a specification fixes" would be false. Its
// identifier *is* fixed — 32 zero bits then the IPv4 address — but that pattern
// is indistinguishable from the ordinary operator habit of writing a host part
// as `prefix::a.b.c.d`, and decoding it would refuse 6.642% of the addresses
// that habit produces (measured over the target space, the same order as the
// NAT64 /56 cost below) for a mechanism that requires an IPv4-multicast-capable
// link and is effectively extinct. RFC 2529 §5 also lets the actual link-layer
// IPv4 address, learned through Neighbor Discovery, differ from the one in the
// identifier — so the decode would not even be authoritative about where a
// packet goes. Refused here is a cost paid by real addresses for a target that
// may not be the real one.
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
// It does over-refuse, and its prefix length does not determine how much: the
// prefix bytes decide it too, and can decide it entirely. Both halves matter,
// because the tempting summary ("a /96 mapping costs nothing") is false.
//
// The well-known 64:ff9b::/96 costs exactly nothing, always — measured
// exhaustively over the whole IPv4 target space, zero refusals. Bytes 4-11 are
// zero by definition, so the /32 through /56 layouts read padding and are
// skipped. Saying *every* speculative layout is skipped would be the neat
// version and is wrong: the /64 layout reads bytes 9-12, and byte 12 is the
// target's own first octet, so for 255 of its 256 values it decodes to 0.0.0.x
// rather than to padding, and costs nothing because no blocked class matches
// 0.0.0.x — not because it goes unread. (For the 256th, a target whose first
// octet is zero, the reading *is* 0.0.0.0 and the skip does take it. Zero either
// way; two different reasons.) That is the case an operator gets without
// choosing anything, and RFC 6052 fixes that prefix at /96, so there is no
// ambiguity there to pay for.
//
// On the RFC 8215 local-use 64:ff9b:1::/48 the cost is real and ranges from
// nothing to everything. With the remaining prefix bytes zero it is 12.8% of
// targets under a /48 mapping — mostly, but not only, the targets whose octet 2
// or 3 is 127 or in 224-239, which alone accounts for 12.840% of the measured
// 12.843%; the remainder is 169.254 turning up in the /56 and /64 readings —
// then 6.6% under /56 and /64, and 0% under /96, each measured exhaustively over
// the octets the layout reads rather than sampled.
//
// RFC 8215 §5 is worth quoting against this decoding rather than only for it,
// since it reserves the prefix the cost is paid on: nodes "must not make any
// assumptions regarding the syntax or properties of those addresses (e.g., the
// existence and location of embedded IPv4 addresses)". Lowercase, in Deployment
// Considerations, so not RFC 2119 — but it is the sentence most directly
// against what happens here, and the answer is not that it does not apply. It is
// that a guard deciding whether to *refuse* a dial is not a node forwarding a
// packet: assuming an embedded address that is not there costs a refusal, while
// declining to assume one that is there costs the metadata endpoint.
//
// But an operator may carve an NSP whose own fixed bytes read as a
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
// The two zero-prefix forms are here because Go decodes neither and nothing
// else catches them. ::ffff:127.0.0.1 (IPv4-*mapped*) is safe without any of
// this — To4 returns 127.0.0.1 and IsLoopback says yes — which makes it easy to
// assume that the deprecated ::127.0.0.1 (IPv4-*compatible*) and
// ::ffff:0:127.0.0.1 (IPv4-*translated*) behave the same way. Neither does: To4
// returns nil for both, because it insists on the mapped prefix's 0xffff in
// bytes 10-11, and every predicate in the refusal list answers false. The
// standard library does not merely stay silent about them either —
// IsGlobalUnicast affirmatively answers true. So without these two cases the
// guard admits them.
//
// They are hardening rather than patched bypasses, and the reason is not that a
// kernel would refuse them. Both kernels route ::127.0.0.1 as an ordinary
// global IPv6 destination — macOS resolves it to the LAN's IPv6 default router
// and the connect blocks until it times out, Linux does the same where it has
// IPv6 connectivity and fails immediately with ENETUNREACH where it has none —
// so the SYN leaves the host and simply never arrives at 127.0.0.1. The
// mechanism that would have delivered it to the embedded address is RFC 2893 §5
// automatic tunneling, which RFC 4213 §8 removed. Its §3.6 points the same way
// without being as strong as it is tempting to make it: a decapsulator MUST
// silently discard a packet whose inner IPv6 source is invalid, and the list of
// invalid sources SHOULD include ::/96 — a MUST on the discard, a SHOULD on the
// membership, and about the inner source rather than the outer tunnel endpoint.
// Refusing these
// costs nothing and does not depend on any of that staying true: a guard that
// holds only because the kernel also says no is not a guard.
func embeddedIPv4(ip net.IP) []net.IP {
	b := ip.To16()
	if b == nil || ip.To4() != nil {
		return nil
	}
	var out []net.IP
	switch {
	case b[0] == 0x20 && b[1] == 0x02:
		out = append(out, v4(b[2], b[3], b[4], b[5]))
	case b[0] == 0x20 && b[1] == 0x01 && b[2] == 0x00 && b[3] == 0x00:
		out = append(out, v4(^b[12], ^b[13], ^b[14], ^b[15]))
	case b[0] == 0x00 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b:
		// RFC 6052 §2.2, one row per prefix length. Byte 8 is the reserved
		// u-octet and is never part of the address, which is why the octets
		// skip it rather than running consecutively.
		out = append(out,
			v4(b[4], b[5], b[6], b[7]),     // /32
			v4(b[5], b[6], b[7], b[9]),     // /40
			v4(b[6], b[7], b[9], b[10]),    // /48
			v4(b[7], b[9], b[10], b[11]),   // /56
			v4(b[9], b[10], b[11], b[12]),  // /64
			v4(b[12], b[13], b[14], b[15]), // /96
		)
	case isZero(b[:8]) && b[8] == 0xff && b[9] == 0xff && b[10] == 0x00 && b[11] == 0x00:
		// IPv4-translated, RFC 2765 §2.1. One byte-pair away from the mapped
		// form net.IP already decodes — 0xffff sits at bytes 8-9 here and at
		// 10-11 there — which is exactly why To4 answers nil for it.
		out = append(out, v4(b[12], b[13], b[14], b[15]))
	case isZero(b[:12]):
		out = append(out, v4(b[12], b[13], b[14], b[15]))
	}
	// ISATAP (RFC 5214 §6.1) is the one form here that lives in the interface
	// identifier rather than in a prefix, so it is checked in addition to the
	// cases above rather than as one of them: any /64 can carry one, including
	// theirs. The identifier is the IANA OUI 00-00-5E, then 0xFE, then the IPv4
	// address.
	//
	// Only the u bit of the first octet varies — it is set when the embedded
	// address is globally unique — so the mask leaves 0x02 free and requires
	// every other bit to be zero. Leaving the g bit free as well would be the
	// natural-looking reading of "u/g bits" and is wrong: g is the group bit,
	// and an interface identifier of a unicast address does not set it. The
	// difference is not academic, because it points the over-refusing way: with
	// g free, an ordinary global-unicast address whose identifier happens to
	// read 0100:5efe or 0300:5efe — no tunnel involved — decodes as one and is
	// refused.
	if b[8]&0xfd == 0 && b[9] == 0x00 && b[10] == 0x5e && b[11] == 0xfe {
		out = append(out, v4(b[12], b[13], b[14], b[15]))
	}
	return out
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
// address of every dial — the address the connection is about to be made to
// rather than the name it came from, which is what makes DNS rebinding
// ineffective. allow is a parameter rather than a hard-wired call so a caller
// can hand in a closure over its own overridable seam (a test pointing at an
// httptest server on loopback needs one); the closure itself is captured once
// and called per dial, so a seam it reads is consulted afresh on each one.
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
