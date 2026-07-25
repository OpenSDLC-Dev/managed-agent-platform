package gaterun

import (
	"context"
	"fmt"
	"slices"
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

// Firewall is the OS firewall the gate applies on startup. Apply replaces the
// OUTPUT chain of both the IPv4 and IPv6 tables with exactly these rules — it
// flushes the chain, then appends them in order — so the post-apply listing is
// deterministic no matter what the chain started with (a fresh netns is empty,
// but the gate must not depend on that for a fail-closed guarantee). List
// returns each table's current OUTPUT rules in `iptables -S OUTPUT` form for the
// post-apply verification. The real adapter (iptables/ip6tables via os/exec)
// lives in cmd/gate; tests supply a fake.
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

// CheckListing verifies one table's `-S OUTPUT` listing is EXACTLY the gate's
// owner-match policy and nothing else: the ordered `-A OUTPUT` rules must equal
// Ruleset(gateUID) token-for-token. Exactness is the security property — a
// relative-order or substring check is not enough. A foreign ACCEPT before the
// DROP would leave egress fail-open (first-match-wins) yet still contain our
// three rules in order; a rule that only resembles ours changes the firewall's
// meaning while passing a loose match — `--uid-owner 1000` when the gate is uid
// 100, `-o lo0` or `! -o lo` for the loopback rule, or an extra match clause.
// Apply establishes the exact state by flushing the chain before it appends, so
// on a real (empty) startup the listing is precisely these rules; this reads it
// back and refuses to serve on anything else. It is the post-apply gate that
// aborts startup when the firewall did not take — the analogue of the K8s
// route-flush's survivor re-count.
func CheckListing(listing string, gateUID int) error {
	want := Ruleset(gateUID)
	var got [][]string
	for _, ln := range strings.Split(listing, "\n") {
		f := strings.Fields(ln)
		if len(f) >= 2 && f[0] == "-A" && f[1] == "OUTPUT" {
			got = append(got, f[2:]) // the rule tokens after "-A OUTPUT"
		}
	}
	if len(got) != len(want) {
		return fmt.Errorf("OUTPUT chain has %d appended rules, want exactly the %d-rule owner-match set", len(got), len(want))
	}
	for i := range want {
		if !slices.Equal(got[i], []string(want[i])) {
			return fmt.Errorf("OUTPUT rule %d is %v, want %v", i, got[i], []string(want[i]))
		}
	}
	return nil
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
