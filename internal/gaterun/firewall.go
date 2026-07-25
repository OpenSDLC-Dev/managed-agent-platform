package gaterun

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Rule is one rule in the gate's own firewall chain (ChainName), expressed as
// the arguments that follow "-A MAP-GATE-EGRESS". The same rules apply to both
// the IPv4 (iptables) and IPv6 (ip6tables) tables — the adapter owns that
// duplication.
type Rule []string

// ChainName is the filter-table chain the gate owns outright. The owner-match
// rules live here — not inlined into OUTPUT — so the gate can coexist with a
// netns whose OUTPUT chain already carries foreign rules (a service mesh's
// redirects in a Kubernetes pod, a CNI's filters): OUTPUT gets exactly one
// jump into this chain, inserted first, and everything else in OUTPUT is left
// alone. Every verdict in the chain is terminal (ACCEPT or DROP, nothing
// RETURNs), so with the jump first no packet ever reaches a foreign OUTPUT
// rule — the policy semantics are identical to owning the whole chain.
const ChainName = "MAP-GATE-EGRESS"

// Ruleset is the gate's owner-match policy — the contents of ChainName — in
// evaluation order. iptables is first-match-wins, so the two ACCEPTs precede
// the catch-all DROP:
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
// The owner-match only isolates the sandbox if the sandbox cannot become the
// gate's uid: it must run as a distinct non-root identity and be unable to change
// uid (drop CAP_SETUID/CAP_SETGID, no-new-privileges), otherwise a tool could
// setuid to the gate uid and egress directly. It must also CapDrop NET_RAW (an
// AF_PACKET socket bypasses the netfilter OUTPUT hook, defeating owner-match).
// Both are the sandbox's provisioning concern, enforced in the Docker/K8s wiring
// (STATE, sub-PR 4), not here.
func Ruleset(gateUID int) []Rule {
	return []Rule{
		{"-o", "lo", "-j", "ACCEPT"},
		{"-m", "owner", "--uid-owner", strconv.Itoa(gateUID), "-j", "ACCEPT"},
		{"-j", "DROP"},
	}
}

// Listing is one family's firewall state as read back for verification: the
// gate's own chain and the OUTPUT chain, each in `iptables -S <chain>` form.
type Listing struct {
	Chain  string // `-S MAP-GATE-EGRESS`
	Output string // `-S OUTPUT`
}

// Firewall is the OS firewall the gate applies on startup. Apply RECONCILES on
// both the IPv4 and IPv6 tables: it (re)builds ChainName to exactly these rules
// atomically, then ensures a single jump into it sits first in OUTPUT — never
// flushing OUTPUT, never touching its policy, so foreign rules a CNI or service
// mesh installed in a shared pod netns survive below the jump (where the
// terminal chain verdicts make them unreachable). Ordering is the fail-closed
// guarantee: the chain is complete before any jump steers traffic into it, so
// there is no instant where a partial policy is live, and a re-apply over a
// previous incarnation's rules (a restarted sidecar in a live pod) is a no-op
// rather than a duplicate. List returns each family's chain and OUTPUT listings
// for the post-apply verification. The real adapter (iptables/ip6tables +
// iptables-restore via os/exec) lives in cmd/gate; tests supply a fake.
type Firewall interface {
	Apply(ctx context.Context, rules []Rule) error
	List(ctx context.Context) (v4, v6 Listing, err error)
}

// PrivDropper drops the process to the gate's unprivileged UID/GID after the
// firewall is applied — so the gate can no longer alter the rules, and its own
// sockets carry the owner-match UID. The real adapter (setgroups/setgid/setuid)
// lives in cmd/gate; tests supply a fake.
type PrivDropper interface {
	Drop() error
}

// CheckListing verifies one family took the owner-match policy, in two halves.
// (a) The gate's own chain is EXACTLY Ruleset(gateUID) and nothing else: the
// ordered `-A MAP-GATE-EGRESS` rules must equal it token-for-token. Exactness
// is the security property — a relative-order or substring check is not enough:
// a foreign ACCEPT before the DROP would leave egress fail-open
// (first-match-wins) yet still contain our three rules in order, and a rule
// that only resembles ours changes the firewall's meaning while passing a loose
// match — `--uid-owner 1000` when the gate is uid 100, `-o lo0` or `! -o lo`
// for the loopback rule, or an extra match clause. (b) The FIRST appended
// OUTPUT rule is exactly the jump into the chain: a foreign rule above the jump
// would decide traffic before the policy sees it (fail-open for an ACCEPT),
// while foreign rules BELOW the jump are tolerated — every chain verdict is
// terminal, so they are unreachable — which is what lets the gate coexist with
// CNI/mesh rules instead of flushing them. It is the post-apply gate that
// aborts startup when the firewall did not take. `-P` policy and `-N` chain
// declaration lines are ignored: the verified explicit catch-all DROP makes the
// steady state fail-closed by itself, whatever the policies say.
func CheckListing(l Listing, gateUID int) error {
	want := Ruleset(gateUID)
	got := appendedRules(l.Chain, ChainName)
	if len(got) != len(want) {
		return fmt.Errorf("%s chain has %d rules, want exactly the %d-rule owner-match set", ChainName, len(got), len(want))
	}
	for i := range want {
		if !slices.Equal(got[i], []string(want[i])) {
			return fmt.Errorf("%s rule %d is %v, want %v", ChainName, i, got[i], []string(want[i]))
		}
	}
	out := appendedRules(l.Output, "OUTPUT")
	if len(out) == 0 {
		return fmt.Errorf("OUTPUT chain has no rules — the -j %s jump is missing", ChainName)
	}
	if !slices.Equal(out[0], []string{"-j", ChainName}) {
		return fmt.Errorf("first OUTPUT rule is %v, want the -j %s jump first (a rule above it would decide traffic before the owner-match policy)", out[0], ChainName)
	}
	return nil
}

// appendedRules extracts the `-A <chain>` rules of one `iptables -S` listing,
// in order, as their token slices (the arguments after "-A <chain>").
func appendedRules(listing, chain string) [][]string {
	var got [][]string
	for _, ln := range strings.Split(listing, "\n") {
		f := strings.Fields(ln)
		if len(f) >= 2 && f[0] == "-A" && f[1] == chain {
			got = append(got, f[2:])
		}
	}
	return got
}

// maxGateID caps the gate's uid/gid. uid_t/gid_t are 32-bit; an int that does
// not fit truncates in the setuid/setgid syscall — e.g. 2^32 becomes 0 (root) —
// so the drop identity is required to sit well inside the range (a positive
// int32). No real deployment needs an id above this.
const maxGateID = 1<<31 - 1

// CheckGateID rejects a uid/gid that is not a positive non-root value fitting
// uid_t/gid_t. Zero is root (a Setuid/Setgid(0) drop is a silent no-op); a value
// past maxGateID would truncate in the syscall and could land on root. name
// labels the error for the caller (e.g. "GATE_UID").
func CheckGateID(name string, id int) error {
	if id <= 0 || id > maxGateID {
		return fmt.Errorf("%s must be a positive non-root id no larger than %d, got %d", name, maxGateID, id)
	}
	return nil
}

// Setup applies the owner-match firewall, verifies it took on both IP tables,
// then drops privileges — the startup order the gate's entrypoint runs before it
// begins serving (so the HEALTHCHECK that gates admission cannot pass until the
// firewall is in force). A verification failure aborts startup fail-closed: the
// gate never serves on a firewall that did not take.
//
// gateUID must be a positive non-root uid (see CheckGateID): dropping to uid 0
// is a silent no-op (the process stays root, keeps CAP_NET_ADMIN, and can still
// rewrite the chain), so an invalid gateUID is refused here rather than serving
// as un-dropped root.
func Setup(ctx context.Context, fw Firewall, pd PrivDropper, gateUID int) error {
	if err := CheckGateID("gate uid", gateUID); err != nil {
		return fmt.Errorf("gaterun: %w", err)
	}
	if err := fw.Apply(ctx, Ruleset(gateUID)); err != nil {
		return fmt.Errorf("gaterun: apply firewall: %w", err)
	}
	v4, v6, err := fw.List(ctx)
	if err != nil {
		return fmt.Errorf("gaterun: list firewall: %w", err)
	}
	for _, tbl := range []struct {
		name    string
		listing Listing
	}{{"iptables", v4}, {"ip6tables", v6}} {
		if err := CheckListing(tbl.listing, gateUID); err != nil {
			return fmt.Errorf("gaterun: %s verification: %w", tbl.name, err)
		}
	}
	if err := pd.Drop(); err != nil {
		return fmt.Errorf("gaterun: drop privileges: %w", err)
	}
	return nil
}
