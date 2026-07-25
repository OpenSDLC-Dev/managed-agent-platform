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

	// The kernel's echo of the applied rules must be exactly Ruleset's tokens,
	// in order, on both families — this pins that iptables-nft does not reorder
	// or normalize what CheckListing must re-verify at startup. `want` is a
	// deliberate hard-coded copy of gaterun.Ruleset(65532), not a call to it:
	// an expectation rendered from Ruleset would follow an accidental edit,
	// where this pin fails it.
	want := []string{
		"-A OUTPUT -o lo -j ACCEPT",
		"-A OUTPUT -m owner --uid-owner 65532 -j ACCEPT",
		"-A OUTPUT -j DROP",
	}
	for _, bin := range []string{"iptables", "ip6tables"} {
		listing, code := dockerCLI(t, "exec", id, bin, "-S", "OUTPUT")
		if code != 0 {
			t.Fatalf("%s -S OUTPUT: exit %d\n%s", bin, code, listing)
		}
		var appended []string
		for _, line := range strings.Split(strings.TrimSpace(listing), "\n") {
			if strings.HasPrefix(line, "-A OUTPUT") {
				appended = append(appended, strings.TrimSpace(line))
			}
		}
		if strings.Join(appended, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s echoed rules:\n%s\nwant exactly:\n%s", bin, strings.Join(appended, "\n"), strings.Join(want, "\n"))
		}
	}

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
