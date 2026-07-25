package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gaterun"
)

// iptablesFirewall reconciles the gate's owner-match policy into both the IPv4
// (iptables) and IPv6 (ip6tables) tables, and lists it back for verification.
// It runs once at startup, before the privilege drop, so the process still
// holds CAP_NET_ADMIN (granted to the gate container). The rule content and
// the verification of the listings live in internal/gaterun; this adapter only
// shells the binaries.
//
// Reconcile, not flush-and-replace: the rules live in the gate's own chain
// (gaterun.ChainName), rebuilt atomically via `iptables-restore --noflush` —
// which flushes and repopulates exactly the chains its payload declares in one
// kernel commit, the kube-proxy coexistence pattern — and OUTPUT gets a single
// jump into that chain, ensured first. OUTPUT itself is never flushed and its
// policy never touched, so rules a CNI or service mesh installed in a shared
// K8s pod netns survive below the jump (unreachable behind the chain's
// terminal verdicts), and a restarted sidecar re-applying over its previous
// incarnation's rules is a no-op instead of a duplicate. Ordering carries the
// fail-closed guarantee the old policy-DROP backstop used to: the chain is
// complete before the jump steers any traffic into it, so no instant exposes a
// partial policy. Everything here touches only the filter table; NAT is
// untouched.
type iptablesFirewall struct{}

func (iptablesFirewall) Apply(ctx context.Context, rules []gaterun.Rule) error {
	for _, bin := range []string{"iptables", "ip6tables"} {
		restore := exec.CommandContext(ctx, bin+"-restore", "--noflush")
		restore.Stdin = strings.NewReader(restorePayload(rules))
		if out, err := restore.CombinedOutput(); err != nil {
			return fmt.Errorf("%s-restore --noflush: %w: %s", bin, err, out)
		}
		if err := ensureJumpFirst(ctx, bin); err != nil {
			return err
		}
	}
	return nil
}

// restorePayload renders the iptables-restore input that declares the gate's
// chain and its rules. Under --noflush the declaration line resets only this
// chain; every other chain in the filter table is left untouched.
func restorePayload(rules []gaterun.Rule) string {
	var b strings.Builder
	b.WriteString("*filter\n:" + gaterun.ChainName + " - [0:0]\n")
	for _, r := range rules {
		b.WriteString("-A " + gaterun.ChainName + " " + strings.Join(r, " ") + "\n")
	}
	b.WriteString("COMMIT\n")
	return b.String()
}

// ensureJumpFirst reconciles the single OUTPUT jump into the gate's chain:
// absent → insert at position 1; already first → no-op (the restarted-sidecar
// path, no traffic disturbed); present but no longer first → delete and
// re-insert at 1. Only that last, already-fail-open state (something inserted
// a rule above a previously verified jump) has a brief jump-absent window —
// remediating it fast beats refusing to serve while it persists.
func ensureJumpFirst(ctx context.Context, bin string) error {
	jump := []string{"-j", gaterun.ChainName}
	if out, err := exec.CommandContext(ctx, bin, append([]string{"-C", "OUTPUT"}, jump...)...).CombinedOutput(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 {
			return fmt.Errorf("%s -C OUTPUT -j %s: %w: %s", bin, gaterun.ChainName, err, out)
		}
		// Exit 1: the jump does not exist yet — fall through to the insert.
	} else {
		first, err := firstOutputRule(ctx, bin)
		if err != nil {
			return err
		}
		if slices.Equal(first, jump) {
			return nil
		}
		if out, err := exec.CommandContext(ctx, bin, append([]string{"-D", "OUTPUT"}, jump...)...).CombinedOutput(); err != nil {
			return fmt.Errorf("%s -D OUTPUT -j %s: %w: %s", bin, gaterun.ChainName, err, out)
		}
	}
	if out, err := exec.CommandContext(ctx, bin, append([]string{"-I", "OUTPUT", "1"}, jump...)...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s -I OUTPUT 1 -j %s: %w: %s", bin, gaterun.ChainName, err, out)
	}
	return nil
}

// firstOutputRule returns the token slice of the first appended OUTPUT rule
// (the arguments after "-A OUTPUT"), or nil when the chain has none.
func firstOutputRule(ctx context.Context, bin string) ([]string, error) {
	out, err := exec.CommandContext(ctx, bin, "-S", "OUTPUT").Output()
	if err != nil {
		return nil, fmt.Errorf("%s -S OUTPUT: %w", bin, err)
	}
	for _, ln := range strings.Split(string(out), "\n") {
		f := strings.Fields(ln)
		if len(f) >= 2 && f[0] == "-A" && f[1] == "OUTPUT" {
			return f[2:], nil
		}
	}
	return nil, nil
}

func (iptablesFirewall) List(ctx context.Context) (v4, v6 gaterun.Listing, err error) {
	list := func(bin string) (gaterun.Listing, error) {
		chain, err := exec.CommandContext(ctx, bin, "-S", gaterun.ChainName).Output()
		if err != nil {
			return gaterun.Listing{}, fmt.Errorf("%s -S %s: %w", bin, gaterun.ChainName, err)
		}
		output, err := exec.CommandContext(ctx, bin, "-S", "OUTPUT").Output()
		if err != nil {
			return gaterun.Listing{}, fmt.Errorf("%s -S OUTPUT: %w", bin, err)
		}
		return gaterun.Listing{Chain: string(chain), Output: string(output)}, nil
	}
	if v4, err = list("iptables"); err != nil {
		return gaterun.Listing{}, gaterun.Listing{}, err
	}
	if v6, err = list("ip6tables"); err != nil {
		return gaterun.Listing{}, gaterun.Listing{}, err
	}
	return v4, v6, nil
}
