package gaterun_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gaterun"
)

func TestRuleset(t *testing.T) {
	rules := gaterun.Ruleset(100)
	want := []gaterun.Rule{
		{"-o", "lo", "-j", "ACCEPT"},
		{"-m", "owner", "--uid-owner", "100", "-j", "ACCEPT"},
		{"-j", "DROP"},
	}
	if len(rules) != len(want) {
		t.Fatalf("Ruleset returned %d rules, want %d", len(rules), len(want))
	}
	for i := range want {
		if !slices.Equal([]string(rules[i]), []string(want[i])) {
			t.Errorf("rule %d = %v, want %v", i, rules[i], want[i])
		}
	}
	// The DROP is last — first-match-wins means an earlier DROP would deny the
	// ACCEPTs above it.
	if got := rules[len(rules)-1]; !slices.Equal([]string(got), []string{"-j", "DROP"}) {
		t.Errorf("last rule = %v, want the catch-all DROP", got)
	}
}

const goodListing = `-P OUTPUT ACCEPT
-A OUTPUT -o lo -j ACCEPT
-A OUTPUT -m owner --uid-owner 100 -j ACCEPT
-A OUTPUT -j DROP`

func TestCheckListing(t *testing.T) {
	if err := gaterun.CheckListing(goodListing, 100); err != nil {
		t.Errorf("valid listing rejected: %v", err)
	}

	cases := map[string]struct {
		listing string
		uid     int
	}{
		"missing DROP":        {"-A OUTPUT -o lo -j ACCEPT\n-A OUTPUT -m owner --uid-owner 100 -j ACCEPT", 100},
		"missing gate ACCEPT": {"-A OUTPUT -o lo -j ACCEPT\n-A OUTPUT -j DROP", 100},
		"missing loopback":    {"-A OUTPUT -m owner --uid-owner 100 -j ACCEPT\n-A OUTPUT -j DROP", 100},
		"wrong uid":           {goodListing, 999},
		"DROP before ACCEPT":  {"-A OUTPUT -j DROP\n-A OUTPUT -o lo -j ACCEPT\n-A OUTPUT -m owner --uid-owner 100 -j ACCEPT", 100},
		"empty":               {"", 100},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if err := gaterun.CheckListing(tc.listing, tc.uid); err == nil {
				t.Errorf("CheckListing accepted a bad listing (%s)", name)
			}
		})
	}
}

type fakeFirewall struct {
	applied    []gaterun.Rule
	v4, v6     string
	applyErr   error
	listErr    error
	listCalled bool
}

func (f *fakeFirewall) Apply(_ context.Context, rules []gaterun.Rule) error {
	f.applied = rules
	return f.applyErr
}

func (f *fakeFirewall) List(context.Context) (string, string, error) {
	f.listCalled = true
	return f.v4, f.v6, f.listErr
}

type fakePriv struct {
	dropped bool
	err     error
}

func (p *fakePriv) Drop() error { p.dropped = true; return p.err }

func TestSetupHappyPath(t *testing.T) {
	fw := &fakeFirewall{v4: goodListing, v6: goodListing}
	pd := &fakePriv{}
	if err := gaterun.Setup(context.Background(), fw, pd, 100); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if len(fw.applied) != 3 {
		t.Errorf("applied %d rules, want the 3-rule owner-match set", len(fw.applied))
	}
	if !pd.dropped {
		t.Error("privileges were not dropped after a clean firewall apply")
	}
}

func TestSetupDoesNotDropPrivilegesWhenVerificationFails(t *testing.T) {
	// The IPv6 table did not take (its DROP is missing). Setup must abort before
	// dropping privileges — the process stays root so an operator/entrypoint sees
	// the failure and the gate never serves on a half-applied firewall.
	fw := &fakeFirewall{v4: goodListing, v6: "-A OUTPUT -o lo -j ACCEPT"}
	pd := &fakePriv{}
	if err := gaterun.Setup(context.Background(), fw, pd, 100); err == nil {
		t.Fatal("Setup accepted a firewall whose IPv6 table did not take")
	}
	if pd.dropped {
		t.Error("privileges were dropped despite a failed firewall verification")
	}
}

func TestSetupApplyErrorStopsBeforeListing(t *testing.T) {
	fw := &fakeFirewall{applyErr: errors.New("iptables: permission denied")}
	pd := &fakePriv{}
	if err := gaterun.Setup(context.Background(), fw, pd, 100); err == nil {
		t.Fatal("Setup ignored an apply error")
	}
	if fw.listCalled {
		t.Error("Setup listed rules after Apply failed")
	}
	if pd.dropped {
		t.Error("Setup dropped privileges after Apply failed")
	}
}

func TestSetupDropErrorSurfaces(t *testing.T) {
	fw := &fakeFirewall{v4: goodListing, v6: goodListing}
	pd := &fakePriv{err: errors.New("setuid: operation not permitted")}
	err := gaterun.Setup(context.Background(), fw, pd, 100)
	if err == nil || !strings.Contains(err.Error(), "drop privileges") {
		t.Fatalf("Setup err = %v, want a drop-privileges error", err)
	}
}
