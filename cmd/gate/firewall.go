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
	for _, bin := range []string{"iptables", "ip6tables"} {
		// Flush OUTPUT first so the chain is exactly these rules regardless of
		// what it started with — the verification that follows demands an exact
		// listing, and a fresh netns is not something a fail-closed gate may
		// assume. -F clears only the filter table's OUTPUT chain; NAT is untouched.
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
