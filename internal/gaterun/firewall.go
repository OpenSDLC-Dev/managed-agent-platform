package gaterun

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Rule is one OUTPUT-chain firewall rule, expressed as the arguments that follow
// "-A OUTPUT". The same rules apply to both the IPv4 (iptables) and IPv6
// (ip6tables) tables — the adapter owns that duplication.
type Rule []string

// Ruleset is the gate's OUTPUT-chain owner-match policy, in evaluation order.
// iptables is first-match-wins, so the two ACCEPTs precede the catch-all DROP:
//
//  1. all loopback traffic — the sandbox reaches the proxy, and curls its own
//     localhost dev servers, over lo (the operator-approved intra-netns loopback
//     width, docs/plan/12 slice 4c-2b decision);
//  2. the gate process's own egress, matched by its post-privdrop UID (the
//     owner-match — this is why the gate drops to a dedicated UID);
//  3. everything else — the sandbox's own UID reaching a non-loopback address —
//     is dropped, so the sandbox can leave the netns only through the proxy,
//     which then egresses as the gate UID.
//
// The sandbox must also CapDrop NET_RAW (an AF_PACKET socket bypasses the
// netfilter OUTPUT hook, defeating owner-match); that is the sandbox's
// provisioning concern, enforced in the Docker/K8s wiring, not here.
func Ruleset(gateUID int) []Rule {
	return []Rule{
		{"-o", "lo", "-j", "ACCEPT"},
		{"-m", "owner", "--uid-owner", strconv.Itoa(gateUID), "-j", "ACCEPT"},
		{"-j", "DROP"},
	}
}

// Firewall is the OS firewall the gate applies on startup. Apply installs rules
// on the OUTPUT chain of both the IPv4 and IPv6 tables; List returns each
// table's current OUTPUT rules in `iptables -S OUTPUT` form for the post-apply
// verification. The real adapter (iptables/ip6tables via os/exec) lives in
// cmd/gate; tests supply a fake.
type Firewall interface {
	Apply(ctx context.Context, rules []Rule) error
	List(ctx context.Context) (v4, v6 string, err error)
}

// PrivDropper drops the process to the gate's unprivileged UID/GID after the
// firewall is applied — so the gate can no longer alter the rules, and its own
// sockets carry the owner-match UID. The real adapter (setgroups/setgid/setuid)
// lives in cmd/gate; tests supply a fake.
type PrivDropper interface {
	Drop() error
}

// CheckListing verifies one table's `-S OUTPUT` listing enforces the fail-closed
// owner-match policy: a loopback ACCEPT and the gate-UID ACCEPT both present and
// both preceding a catch-all DROP. A missing DROP (egress not fail-closed), a
// missing gate ACCEPT (the gate itself blocked), or a DROP ordered before an
// ACCEPT (drops everything) each fail. This is the post-apply gate that refuses
// to serve on a firewall that did not take — the analogue of the K8s
// route-flush's survivor re-count.
func CheckListing(listing string, gateUID int) error {
	loopIdx, gateIdx, dropIdx := -1, -1, -1
	uidMatch := "--uid-owner " + strconv.Itoa(gateUID)
	for i, ln := range strings.Split(listing, "\n") {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "-A OUTPUT") {
			continue
		}
		accept := strings.HasSuffix(ln, "-j ACCEPT")
		switch {
		case accept && strings.Contains(ln, "-o lo"):
			loopIdx = i
		case accept && strings.Contains(ln, uidMatch):
			gateIdx = i
		case ln == "-A OUTPUT -j DROP":
			dropIdx = i
		}
	}
	switch {
	case loopIdx < 0:
		return errors.New("loopback ACCEPT rule missing")
	case gateIdx < 0:
		return fmt.Errorf("gate uid-owner ACCEPT rule (uid %d) missing", gateUID)
	case dropIdx < 0:
		return errors.New("catch-all DROP rule missing — egress is not fail-closed")
	case dropIdx < loopIdx || dropIdx < gateIdx:
		return errors.New("catch-all DROP precedes an ACCEPT — rules out of order")
	}
	return nil
}

// Setup applies the owner-match firewall, verifies it took on both IP tables,
// then drops privileges — the startup order the gate's entrypoint runs before it
// begins serving (so the HEALTHCHECK that gates admission cannot pass until the
// firewall is in force). A verification failure aborts startup fail-closed: the
// gate never serves on a firewall that did not take.
func Setup(ctx context.Context, fw Firewall, pd PrivDropper, gateUID int) error {
	if err := fw.Apply(ctx, Ruleset(gateUID)); err != nil {
		return fmt.Errorf("gaterun: apply firewall: %w", err)
	}
	v4, v6, err := fw.List(ctx)
	if err != nil {
		return fmt.Errorf("gaterun: list firewall: %w", err)
	}
	for _, tbl := range []struct{ name, listing string }{{"iptables", v4}, {"ip6tables", v6}} {
		if err := CheckListing(tbl.listing, gateUID); err != nil {
			return fmt.Errorf("gaterun: %s verification: %w", tbl.name, err)
		}
	}
	if err := pd.Drop(); err != nil {
		return fmt.Errorf("gaterun: drop privileges: %w", err)
	}
	return nil
}
