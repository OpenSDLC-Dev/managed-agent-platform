package main

// The real-netns firewall contract test: the gate image runs under the real
// Docker daemon, so the owner-match adapter meets real iptables (the nft
// backend debian:stable-slim installs) inside a real network namespace — the
// one place the fake-backed gaterun tests cannot reach. Reaching "healthy"
// alone proves the production admission chain: Apply, the -S listing,
// CheckListing on both families, and the privilege drop, all against the real
// binaries; the exec probes then prove the rules move real packets the way
// Ruleset promises. A missing daemon is a hard failure, not a skip, per the
// repo's testing conventions.

import (
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/sandboxtest"
)

func TestGateImageOwnerMatchFirewall(t *testing.T) {
	image := sandboxtest.BuildGateImage(t)

	// A host listener the probes dial: reachable from the gate's netns by
	// construction, so a refused dial can only be the firewall's doing.
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	hostAddr := sandboxtest.DockerHostAddr(t)
	port := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)

	// The controlplane URL is unreachable on purpose: the gate stays healthy
	// serving deny-all (CP-outage tolerance), and this test only concerns the
	// firewall below the proxy.
	out, code := dockerCLI(t, "run", "-d", "--rm", "--cap-add", "NET_ADMIN",
		"-e", "CONTROLPLANE_URL=http://"+net.JoinHostPort(hostAddr, "1"),
		"-e", "GATE_TOKEN=gtk_firewalltest", image)
	if code != 0 {
		t.Fatalf("docker run: exit %d\n%s", code, out)
	}
	id := strings.TrimSpace(out)
	t.Cleanup(func() { _, _ = exec.Command("docker", "rm", "-f", id).CombinedOutput() })

	waitHealthy(t, id)

	assertFirewallShape(t, id)

	// Real packets: a root process (the sandbox's uid in production) cannot
	// leave the netns except over loopback; the gate's dropped-to uid can.
	dial := `exec 3<>"/dev/tcp/` + hostAddr + `/` + port + `"`
	if out, code := dockerCLI(t, "exec", id, "timeout", "3", "bash", "-c", dial); code == 0 {
		t.Errorf("root egress escaped the owner-match DROP:\n%s", out)
	}
	if out, code := dockerCLI(t, "exec", "-u", "65532", id, "timeout", "3", "bash", "-c", dial); code != 0 {
		t.Errorf("gate-uid egress refused (exit %d), want allowed:\n%s", code, out)
	}
	loop := `exec 3<>"/dev/tcp/127.0.0.1/15080"`
	if out, code := dockerCLI(t, "exec", id, "timeout", "3", "bash", "-c", loop); code != 0 {
		t.Errorf("root loopback dial to the proxy refused (exit %d), want allowed:\n%s", code, out)
	}
}

// assertFirewallShape pins the kernel's echo of the applied firewall on both
// families: the gate's own chain must be exactly Ruleset's tokens, in order,
// and the FIRST appended OUTPUT rule must be the jump into it — this pins that
// iptables-nft does not reorder or normalize what CheckListing must re-verify
// at startup. `wantChain` is a deliberate hard-coded copy of
// gaterun.Ruleset(65532) rehomed into the chain, not a render from it: an
// expectation rendered from Ruleset would follow an accidental edit, where
// this pin fails it.
func assertFirewallShape(t *testing.T, id string) {
	t.Helper()
	wantChain := []string{
		"-A MAP-GATE-EGRESS -o lo -j ACCEPT",
		"-A MAP-GATE-EGRESS -m owner --uid-owner 65532 -j ACCEPT",
		"-A MAP-GATE-EGRESS -j DROP",
	}
	for _, bin := range []string{"iptables", "ip6tables"} {
		listing, code := dockerCLI(t, "exec", id, bin, "-S", "MAP-GATE-EGRESS")
		if code != 0 {
			t.Fatalf("%s -S MAP-GATE-EGRESS: exit %d\n%s", bin, code, listing)
		}
		if got := appendedLines(listing, "-A MAP-GATE-EGRESS"); strings.Join(got, "\n") != strings.Join(wantChain, "\n") {
			t.Errorf("%s echoed chain:\n%s\nwant exactly:\n%s", bin, strings.Join(got, "\n"), strings.Join(wantChain, "\n"))
		}
		out, code := dockerCLI(t, "exec", id, bin, "-S", "OUTPUT")
		if code != 0 {
			t.Fatalf("%s -S OUTPUT: exit %d\n%s", bin, code, out)
		}
		jumps := appendedLines(out, "-A OUTPUT")
		if len(jumps) == 0 || jumps[0] != "-A OUTPUT -j MAP-GATE-EGRESS" {
			t.Errorf("%s OUTPUT does not start with the gate jump:\n%s", bin, out)
		}
	}
}

// appendedLines returns the listing's lines that start with prefix, trimmed.
func appendedLines(listing, prefix string) []string {
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(listing), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			got = append(got, line)
		}
	}
	return got
}

// TestGateImageFirewallReconcilesForeignRules is the reconcile proof: a netns
// whose OUTPUT chain already carries a foreign rule (the K8s pod situation —
// CNI or mesh rules present before the sidecar starts) must not be flushed.
// The gate becomes healthy, the foreign rule survives below the gate's jump —
// unreachable behind the chain's terminal verdicts, so root egress is still
// dropped. Re-apply over the LIVE netns (the restarted-sidecar case — note
// `docker restart` recreates the netns, so the only way to meet populated
// kernel state is re-running /gate inside the same container) is then proven
// on both divergence paths: a foreign rule pushed above the jump is remediated
// (delete + re-insert first), and a re-apply over an already-correct state is
// a no-op — no duplicate chain rules, no duplicate jump, either way.
func TestGateImageFirewallReconcilesForeignRules(t *testing.T) {
	image := sandboxtest.BuildGateImage(t)

	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	hostAddr := sandboxtest.DockerHostAddr(t)
	port := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)

	// Pre-populate the netns before the gate runs: a foreign ACCEPT in OUTPUT
	// (would fail-open if it ended up above the gate's jump) — then exec the
	// gate. No --rm: the restart below must not remove the container.
	foreign := "iptables -A OUTPUT -p tcp --dport 9 -j ACCEPT && ip6tables -A OUTPUT -p tcp --dport 9 -j ACCEPT && exec /gate"
	out, code := dockerCLI(t, "run", "-d", "--cap-add", "NET_ADMIN",
		"--entrypoint", "/bin/bash",
		"-e", "CONTROLPLANE_URL=http://"+net.JoinHostPort(hostAddr, "1"),
		"-e", "GATE_TOKEN=gtk_firewalltest", image, "-c", foreign)
	if code != 0 {
		t.Fatalf("docker run: exit %d\n%s", code, out)
	}
	id := strings.TrimSpace(out)
	t.Cleanup(func() { _, _ = exec.Command("docker", "rm", "-f", id).CombinedOutput() })

	waitHealthy(t, id)
	assertReconciledShape(t, id)

	// Root egress is still dropped: the foreign ACCEPT survives but sits below
	// the jump, where the chain's terminal verdicts make it unreachable.
	dial := `exec 3<>"/dev/tcp/` + hostAddr + `/` + port + `"`
	if out, code := dockerCLI(t, "exec", id, "timeout", "3", "bash", "-c", dial); code == 0 {
		t.Errorf("root egress escaped the owner-match DROP with a foreign rule present:\n%s", out)
	}

	// Remediation path: push a foreign ACCEPT ABOVE the jump (the already-
	// fail-open state a CNI/mesh agent could create at runtime), then re-run
	// /gate in the SAME netns. Setup re-applies — iptables-restore resets the
	// populated chain, ensureJumpFirst deletes and re-inserts the jump at 1 —
	// then the second process exits on the bind conflict with the live gate.
	for _, bin := range []string{"iptables", "ip6tables"} {
		if out, code := dockerCLI(t, "exec", "-u", "0", id, bin, "-I", "OUTPUT", "1", "-p", "tcp", "--dport", "19", "-j", "ACCEPT"); code != 0 {
			t.Fatalf("%s -I OUTPUT 1: exit %d\n%s", bin, code, out)
		}
	}
	reExecGate(t, id)
	assertReconciledShape(t, id)
	for _, bin := range []string{"iptables", "ip6tables"} {
		out, _ := dockerCLI(t, "exec", id, bin, "-S", "OUTPUT")
		if n := countEqual(appendedLines(out, "-A OUTPUT"), "-A OUTPUT -p tcp -m tcp --dport 19 -j ACCEPT"); n != 1 {
			t.Errorf("%s above-jump foreign rule not preserved below the jump (found %d):\n%s", bin, n, out)
		}
	}

	// No-op path: with everything already correct, a further re-apply must
	// change nothing — no duplicate rules, no duplicate jump.
	reExecGate(t, id)
	assertReconciledShape(t, id)
}

// reExecGate re-runs /gate as root inside the live container — the one way to
// exercise Apply against the SAME netns with populated kernel state. The
// second process inherits the container env, runs the full Setup (apply,
// verify, privdrop), then exits on the bind conflict with the live gate; that
// bind error is the positive signal Setup completed, and a "gaterun:" error
// would mean the firewall re-apply itself failed.
func reExecGate(t *testing.T, id string) {
	t.Helper()
	out, code := dockerCLI(t, "exec", "-u", "0", id, "timeout", "20", "/gate")
	if code == 0 {
		t.Fatalf("re-exec'd gate exited 0, want a bind-conflict exit:\n%s", out)
	}
	if strings.Contains(out, "gaterun:") {
		t.Fatalf("re-exec'd gate failed in firewall setup, not at bind:\n%s", out)
	}
	if !strings.Contains(out, "address already in use") {
		t.Fatalf("re-exec'd gate did not reach the bind conflict (exit %d):\n%s", code, out)
	}
}

// assertReconciledShape asserts the gate shape AND the foreign rule's survival,
// with exactly one gate jump in OUTPUT (idempotency).
func assertReconciledShape(t *testing.T, id string) {
	t.Helper()
	assertFirewallShape(t, id)
	for _, bin := range []string{"iptables", "ip6tables"} {
		out, code := dockerCLI(t, "exec", id, bin, "-S", "OUTPUT")
		if code != 0 {
			t.Fatalf("%s -S OUTPUT: exit %d\n%s", bin, code, out)
		}
		rules := appendedLines(out, "-A OUTPUT")
		if n := countEqual(rules, "-A OUTPUT -j MAP-GATE-EGRESS"); n != 1 {
			t.Errorf("%s OUTPUT holds %d gate jumps, want exactly 1:\n%s", bin, n, out)
		}
		if n := countEqual(rules, "-A OUTPUT -p tcp -m tcp --dport 9 -j ACCEPT"); n != 1 {
			t.Errorf("%s foreign rule did not survive the reconcile (found %d):\n%s", bin, n, out)
		}
	}
}

func countEqual(lines []string, want string) int {
	n := 0
	for _, l := range lines {
		if l == want {
			n++
		}
	}
	return n
}

// waitHealthy polls the container's HEALTHCHECK; failing to get there means the
// production admission chain (firewall apply → verify → privdrop → listen)
// broke against real iptables, so the gate's own logs are the diagnosis.
func waitHealthy(t *testing.T, id string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		out, code := dockerCLI(t, "inspect", "--format", "{{.State.Health.Status}}", id)
		status := strings.TrimSpace(out)
		if code == 0 && status == "healthy" {
			return
		}
		if code == 0 && status == "unhealthy" || time.Now().After(deadline) {
			logs, _ := dockerCLI(t, "logs", id)
			t.Fatalf("gate container %s (status %q) never became healthy; logs:\n%s", id, status, logs)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// dockerCLI runs one docker command, returning its combined output and exit
// code; only a failure to launch docker at all is fatal.
func dockerCLI(t *testing.T, args ...string) (string, int) {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return string(out), ee.ExitCode()
		}
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out), 0
}
