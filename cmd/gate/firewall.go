package main

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gaterun"
)

// iptablesFirewall replaces the gate's OUTPUT chain on both the IPv4 (iptables)
// and IPv6 (ip6tables) tables with the owner-match rules, and lists them back for
// verification. It runs once at startup, before the privilege drop, so the
// process still holds CAP_NET_ADMIN (granted to the gate container). The rule
// content and the verification of the listing live in internal/gaterun; this
// adapter only shells the two binaries.
type iptablesFirewall struct{}

func (iptablesFirewall) Apply(ctx context.Context, rules []gaterun.Rule) error {
	bins := []string{"iptables", "ip6tables"}
	// Lock BOTH families' OUTPUT policy to DROP *before* rebuilding either. The
	// flush→append sequence is not atomic: under a default-ACCEPT policy the
	// window between the flush and the catch-all DROP is fail-open, and rebuilding
	// IPv4 fully before touching IPv6 would leave IPv6 open throughout (and, on an
	// IPv4 error, indefinitely). Dropping both policies first means every instant
	// of the rebuild — a mid-flush crash, an append error, even the gate exiting —
	// denies egress on both families except what the rules below re-admit. This
	// assumes the gate owns its netns OUTPUT chain, so the chain holds no
	// pre-existing ACCEPT the policy would not dominate — true for the per-session
	// Docker sandbox netns; the K8s sidecar must reconcile with CNI/mesh rules
	// (STATE, 4d). -P/-F touch only the filter table's OUTPUT chain; NAT is untouched.
	for _, bin := range bins {
		if out, err := exec.CommandContext(ctx, bin, "-P", "OUTPUT", "DROP").CombinedOutput(); err != nil {
			return fmt.Errorf("%s -P OUTPUT DROP: %w: %s", bin, err, out)
		}
	}
	for _, bin := range bins {
		if out, err := exec.CommandContext(ctx, bin, "-F", "OUTPUT").CombinedOutput(); err != nil {
			return fmt.Errorf("%s -F OUTPUT: %w: %s", bin, err, out)
		}
		for _, r := range rules {
			args := append([]string{"-A", "OUTPUT"}, []string(r)...)
			if out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput(); err != nil {
				return fmt.Errorf("%s %v: %w: %s", bin, args, err, out)
			}
		}
	}
	return nil
}

func (iptablesFirewall) List(ctx context.Context) (v4, v6 string, err error) {
	v4b, err := exec.CommandContext(ctx, "iptables", "-S", "OUTPUT").Output()
	if err != nil {
		return "", "", fmt.Errorf("iptables -S OUTPUT: %w", err)
	}
	v6b, err := exec.CommandContext(ctx, "ip6tables", "-S", "OUTPUT").Output()
	if err != nil {
		return "", "", fmt.Errorf("ip6tables -S OUTPUT: %w", err)
	}
	return string(v4b), string(v6b), nil
}
