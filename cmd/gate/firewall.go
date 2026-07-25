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
		// Set the chain policy to DROP *before* flushing, so egress is fail-closed
		// for the entire rebuild. The flush→append sequence is not atomic: with a
		// default-ACCEPT policy the window between the flush and the catch-all DROP
		// would be fail-open (an issue on a gate restart while the sandbox is live),
		// and a partial append failure would leave a permissive chain. With policy
		// DROP, every instant from here on — a mid-rebuild crash, an append error,
		// even the gate exiting — denies all egress except what the rules below
		// re-admit. Then flush and append the exact rule set. This assumes the gate
		// owns its netns OUTPUT chain — true for the per-session Docker sandbox
		// netns; the K8s sidecar must reconcile with CNI/mesh rules (STATE, 4d).
		// -F/-P touch only the filter table's OUTPUT chain; NAT is untouched.
		if out, err := exec.CommandContext(ctx, bin, "-P", "OUTPUT", "DROP").CombinedOutput(); err != nil {
			return fmt.Errorf("%s -P OUTPUT DROP: %w: %s", bin, err, out)
		}
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
