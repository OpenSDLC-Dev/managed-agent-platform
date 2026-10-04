package k8s_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/dockertest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/hookedtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/k8s"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/sandboxtest"
)

// testImage satisfies the plan's image contract: /bin/bash at that exact path,
// plus a POSIX userland — the same image the docker backend's contract test uses.
const testImage = "debian:stable-slim"

// The Kubernetes backend against a real cluster. A missing cluster is a hard
// failure, not a skip: a skipped contract test would silently hollow out the
// gate. Locally point it at a kind cluster (MAP_K8S_CONTEXT=kind-...); in CI the
// kind-action sets the current context so the defaults suffice.
func TestK8sProviderContract(t *testing.T) {
	sandboxtest.Run(t, func(t *testing.T) sandboxtest.Harness {
		provider, err := k8s.New(k8s.Config{
			Context:   os.Getenv("MAP_K8S_CONTEXT"),
			Namespace: os.Getenv("MAP_K8S_NAMESPACE"),
		})
		if err != nil {
			t.Fatalf("contract tests require a Kubernetes cluster: %v", err)
		}
		// EnforcesPidsLimit stays false: the Pod API carries no per-pod pids
		// limit (it is the kubelet's `podPidsLimit` node setting), so the
		// suite's pids row is not registered here. docs/DIVERGENCES.md records
		// it; TestPodSpecIgnoresPidsLimit keeps it deliberate.
		return sandboxtest.Harness{Provider: provider, Image: testImage, Gate: k8sGateFixture}
	})
}

// liveSandbox provisions one throwaway pod for a backend-specific behaviour the
// shared contract does not pin (because the docker backend enforces it through a
// different mechanism). Same cluster gating as the contract test.
func liveSandbox(t *testing.T) sandbox.Sandbox {
	t.Helper()
	provider, err := k8s.New(k8s.Config{
		Context:   os.Getenv("MAP_K8S_CONTEXT"),
		Namespace: os.Getenv("MAP_K8S_NAMESPACE"),
	})
	if err != nil {
		t.Fatalf("these tests require a Kubernetes cluster: %v", err)
	}
	sb, err := provider.Provision(context.Background(), sandbox.Spec{
		SessionID:  domain.NewID("sesn"),
		Image:      testImage,
		Networking: domain.Networking{Type: domain.NetUnrestricted},
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Cleanup(func() { _ = sb.Destroy(context.Background()) })
	return sb
}

// podUID asks the cluster — not the provider — for the pod's immutable UID.
// Sandbox IDs here are derived names, identical across a delete-and-recreate,
// so only the UID can prove the refusal deleted nothing.
func podUID(t *testing.T, name string) string {
	t.Helper()
	args := []string{"get", "pod", name, "-o", "jsonpath={.metadata.uid}"}
	if ns := os.Getenv("MAP_K8S_NAMESPACE"); ns != "" {
		args = append(args, "-n", ns)
	}
	if kctx := os.Getenv("MAP_K8S_CONTEXT"); kctx != "" {
		args = append(args, "--context", kctx)
	}
	out, err := exec.Command("kubectl", args...).Output()
	if err != nil {
		t.Fatalf("kubectl get pod %s: %v", name, err)
	}
	return strings.TrimSpace(string(out))
}

// A session's pod created unrestricted must not be adopted by a limited
// request for the same session: the netsetup init container that enforces
// `limited` is fixed at pod create, so adopting would keep open egress. The
// refusal deletes nothing — proven by the pod's UID, since the derived name
// would survive a delete-and-recreate — and the original spec still owns its
// pod. The docker backend's adopt_test.go proves the same rule against a live
// daemon (#296).
func TestK8sAdoptionRefusesAMismatchedPod(t *testing.T) {
	provider, err := k8s.New(k8s.Config{
		Context:   os.Getenv("MAP_K8S_CONTEXT"),
		Namespace: os.Getenv("MAP_K8S_NAMESPACE"),
	})
	if err != nil {
		t.Fatalf("this test requires a Kubernetes cluster: %v", err)
	}
	ctx := context.Background()
	spec := sandbox.Spec{
		SessionID:  domain.NewID("sesn"),
		Image:      testImage,
		Networking: domain.Networking{Type: domain.NetUnrestricted},
	}
	first, err := provider.Provision(ctx, spec)
	if err != nil {
		t.Fatalf("provision (unrestricted): %v", err)
	}
	t.Cleanup(func() {
		if err := first.Destroy(context.Background()); err != nil {
			t.Errorf("destroy: %v", err)
		}
	})

	uid := podUID(t, first.ID())

	limited := spec
	limited.Networking = domain.Networking{Type: domain.NetLimited}
	if _, err := provider.Provision(ctx, limited); !errors.Is(err, sandbox.ErrSpecMismatch) {
		t.Fatalf("limited provision over an unrestricted pod: err = %v, want sandbox.ErrSpecMismatch", err)
	}
	if got := podUID(t, first.ID()); got != uid {
		t.Errorf("after the refusal, pod uid = %q, want %q (untouched)", got, uid)
	}

	if _, err := provider.Provision(ctx, spec); err != nil {
		t.Fatalf("re-provision (unrestricted) after the refusal: %v", err)
	}
	if got := podUID(t, first.ID()); got != uid {
		t.Errorf("re-provision replaced the pod: uid = %q, want the adopted %q", got, uid)
	}
}

// A symlink is not a regular file. Following it would let a short link past the
// size gate to a target of any size, so ReadFile rejects it — as the docker
// backend rejects a non-regular archive entry.
func TestK8sReadFileRejectsSymlink(t *testing.T) {
	sb := liveSandbox(t)
	ctx := context.Background()
	res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: "ln -s /etc/hostname " + sandbox.DefaultWorkdir + "/link"})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("create symlink: %+v %v", res, err)
	}
	if _, err := sb.ReadFile(ctx, sandbox.DefaultWorkdir+"/link"); !errors.Is(err, sandbox.ErrNotRegularFile) {
		t.Errorf("ReadFile(symlink) err = %v, want ErrNotRegularFile", err)
	}
}

// A limited sandbox fails closed: if the route flush silently no-ops — staged
// here with a netSetup image that carries no `ip` — Provision must refuse rather
// than start a sandbox that kept its egress route.
func TestK8sLimitedNetworkingFailsClosedWhenFlushNoOps(t *testing.T) {
	provider, err := k8s.New(k8s.Config{
		Context:   os.Getenv("MAP_K8S_CONTEXT"),
		Namespace: os.Getenv("MAP_K8S_NAMESPACE"),
		// debian:stable-slim has no iproute2, so the flush cannot run and the
		// init container's fail-closed check must fail the pod.
		NetSetupImage: testImage,
	})
	if err != nil {
		t.Fatalf("these tests require a Kubernetes cluster: %v", err)
	}
	sb, err := provider.Provision(context.Background(), sandbox.Spec{
		SessionID:  domain.NewID("sesn"),
		Image:      testImage,
		Networking: domain.Networking{Type: domain.NetLimited},
	})
	if err == nil {
		_ = sb.Destroy(context.Background())
		t.Fatal("limited provision with an ip-less netSetup image succeeded, want a fail-closed error")
	}
}

// k8sGateFixture builds the real gate image, makes it visible to the cluster
// (sideloaded into kind; Docker Desktop's cluster shares the daemon's image
// store), and stands in for the controlplane + egress origin on one host
// listener the gate sidecar can reach from a pod.
func k8sGateFixture(t *testing.T) sandboxtest.GateFixture {
	image := sandboxtest.BuildGateImage(t)
	kubeCtx := sandboxtest.KubeContext(t)
	// On kind, the pods run this run's own image of the gate: the suite's
	// shared tag with a nonce label on top, its layers shared through the
	// build cache, its ID — and so the digest the import record `kind load`
	// leaves is named for — this run's alone. Removing it when the test is
	// done takes its tag and its record and nothing another run or package
	// is running pods from; loading the shared tag instead left a dated
	// record on the nodes that no later run could tell from one a concurrent
	// run still needed, so none removed it. A context that is not kind's
	// must share the daemon's image store or have the image loaded by hand —
	// MAP_K8S_HOST_ADDR fixes only how pods address the stub controlplane,
	// not image distribution.
	if strings.HasPrefix(kubeCtx, "kind-") {
		var load [8]byte
		_, _ = rand.Read(load[:])
		image = dockertest.ImageFrom(t, "gate", "FROM "+image+"\nLABEL map.gate.load="+hex.EncodeToString(load[:])+"\n")
		if l := sandboxtest.LoadIntoKind(t, kubeCtx, image); l != nil {
			t.Cleanup(func() { l.Remove(t) })
		}
	}
	stub := sandboxtest.StartGateStubAt(t, k8sHostAddr(t, kubeCtx))
	return sandboxtest.GateFixture{
		Spec: &sandbox.GateSpec{
			Image:           image,
			ControlplaneURL: "http://" + stub.Addr,
			TokenMinter:     stub.Minter(),
		},
		AllowedAddr: stub.Addr,
		DeniedHost:  "denied.invalid",
		Placeholder: stub.Placeholder,
		Secret:      stub.Secret,
	}
}

// k8sHostAddr answers "an address of the test host reachable from a pod" for
// the cluster flavors the suite runs on: whatever the Docker fixture resolves
// when the daemon is Docker Desktop (its VM holds the cluster either way), the
// kind docker network's IPv4 gateway for a local daemon (a kind node routes to
// the host through it), else host.docker.internal. MAP_K8S_HOST_ADDR overrides
// all three for anything else; on Desktop the delegated fixture honours its own
// MAP_DOCKER_HOST_ADDR too, MAP_K8S_HOST_ADDR winning when both are set.
func k8sHostAddr(t *testing.T, kubeCtx string) string {
	t.Helper()
	if addr := os.Getenv("MAP_K8S_HOST_ADDR"); addr != "" {
		return addr
	}
	// Docker Desktop runs the daemon — and so any cluster on it, kind or its own
	// built-in one — inside a VM of its own, which makes the kind network's
	// gateway an address in that VM rather than one this process answers on.
	// Ask the Docker fixture, which already works out what a container there can
	// reach this host by, per platform, before the cluster flavour is consulted
	// at all: the flavour is not what decides the answer.
	if sandboxtest.DockerDesktop(t) {
		return sandboxtest.DockerHostAddr(t)
	}
	if !strings.HasPrefix(kubeCtx, "kind-") {
		return "host.docker.internal"
	}
	// A kind node routes to the host through the kind network's gateway, the
	// daemon holding that network being local by the check above.
	out, err := exec.Command("docker", "network", "inspect", "kind",
		"-f", `{{range .IPAM.Config}}{{.Gateway}}
{{end}}`).CombinedOutput()
	if err != nil {
		t.Fatalf("docker network inspect kind: %v\n%s", err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if gw := strings.TrimSpace(line); strings.Count(gw, ".") == 3 {
			return gw
		}
	}
	t.Fatalf("no IPv4 gateway on the kind docker network:\n%s", out)
	return ""
}

// Untrusted tool commands must not receive the namespace ServiceAccount's token:
// the sandbox never calls the Kubernetes API, and a mounted token would hand the
// agent whatever RBAC that account carries.
func TestK8sNoServiceAccountToken(t *testing.T) {
	sb := liveSandbox(t)
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Command: "test -e /var/run/secrets/kubernetes.io/serviceaccount && echo present || echo absent",
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "absent" {
		t.Errorf("serviceaccount token dir = %q, want absent", got)
	}
}

// The deadline watchdog must not pin the exec's stderr open: a quick command
// under a timeout returns as soon as it finishes, not a watchdog poll interval
// (~1s) later. The script's half of that — the watchdog closing its inherited
// copy of the stream — is pinned without a cluster by
// TestExecWrapperReleasesTheStreamWhenTheCommandExits; what only a cluster can
// answer is whether the whole path still returns on the command's exit, kubelet
// and provider included.
//
// Measured as a difference rather than a wall clock (#318). The same command
// makes the same two round trips with and without a deadline — Exec's second
// exec collects the state either way — so the deadline adds the watchdog and
// nothing else, and runner load, which scales both, cancels. An absolute bound
// measured the cluster's latency as much as the property and reddened CI on a
// loaded runner: 900ms held at 0.85s on an idle machine and failed at 2.27s on a
// busy one, while the regression it exists for is a fixed ~1s.
//
// The pairs are interleaved and the **median** of their differences decides.
// Not the best pair: `min` passes as soon as one pair's noise runs 500ms the
// favourable way, so it would go green against a regression that delayed every
// timed exec by a second — the most permissive statistic is the one a guard can
// least afford. Not the worst either, which flakes on a single adverse outlier.
// The median has to be bought three times out of five in whichever direction is
// wrong, so both failure modes need a majority of the samples rather than one.
// The bound is half the watchdog's poll interval — far above the difference
// between two adjacent execs (single-digit ms idle, tens of ms on a host under
// 5x load), far below the interval a regression adds.
func TestK8sTimedExecDoesNotWaitForItsWatchdog(t *testing.T) {
	sb := liveSandbox(t)
	// Half these execs carry no deadline of their own — that is what makes them
	// the baseline — and an exec with neither is bounded by nothing at all, which
	// in this package means a wedged stream costs the whole binary rather than
	// this row (#318). A budget the ten fast execs cannot approach turns that back
	// into one row's failure, as the suite's own long rows already do.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	elapsed := func(timeout time.Duration) time.Duration {
		start := time.Now()
		res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: "echo hi", Timeout: timeout})
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "hi" {
			t.Fatalf("unexpected result: %+v", res)
		}
		return time.Since(start)
	}

	const pairs = 5
	costs := make([]time.Duration, 0, pairs)
	for i := range pairs {
		// Alternating which half goes first: a fixed order holds a fixed phase
		// against anything periodic on the host, and load that lands on every
		// timed half and no untimed one would then read as a cost rather than as
		// the noise it is. Alternating gives that pattern no phase to hold.
		var untimed, timed time.Duration
		if i%2 == 0 {
			untimed, timed = elapsed(0), elapsed(time.Minute)
		} else {
			timed, untimed = elapsed(time.Minute), elapsed(0)
		}
		costs = append(costs, timed-untimed)
	}
	slices.Sort(costs)
	if median := costs[len(costs)/2]; median > 500*time.Millisecond {
		t.Errorf("across %d pairs a deadline cost a quick command a median %s (%v) — the watchdog is pinning the exec stream open again",
			pairs, median, costs)
	}
}

// disarmTheWatchdog kills the command's watchdog — the wrapper's other child —
// waiting, briefly and boundedly, for it to exist first, and says "disarmed"
// on stdout when it did, so a row that needs it disarmed can tell.
const disarmTheWatchdog = `
  w=
  for i in $(seq 100); do
    for p in $(cat /proc/$PPID/task/$PPID/children 2>/dev/null); do [ "$p" != "$$" ] && w=$p; done
    [ -n "$w" ] && break
    sleep 0.01
  done
  [ -n "$w" ] && kill -9 "$w" 2>/dev/null && echo disarmed
`

// blindTheProbe is a command that disarms its watchdog, points the pid file the
// liveness probe reads at a process that has already exited, and then runs 5s:
// TestK8sOverrunThenExitIsATimeoutTheProbeCannotSee says why each step.
const blindTheProbe = blind + "  sleep 5\n"

// blind is blindTheProbe's staging alone, saying "blinded" on stdout when it
// worked.
const blind = `
  state=$(tr '\0' '\n' < /proc/$PPID/cmdline 2>/dev/null | tail -n 1)` + disarmTheWatchdog + `
  true & gone=$!
  wait "$gone"
  [ -n "$state" ] && [ -f "$state.pid" ] && echo "$gone" > "$state.pid" && echo blinded
`

// A command that disarms its watchdog, overruns its deadline and then exits clean
// is a timeout even where the overrun probe cannot see it: the #832 flake, where
// the probe answered too late, and classifyTimeout argues what still sees it.
//
// No cluster can be told to answer a probe late, so this row blinds the probe
// instead, which is the same thing as far as the probe can tell: with its
// watchdog gone, the command points the pid file — all the probe reads — at a
// process that has already exited. It waits, briefly and boundedly, for the
// watchdog to exist before killing it, and says on stdout what it managed, so a
// cluster where the staging cannot work (no /proc/<pid>/task/<pid>/children)
// fails this row by name rather than passing it vacuously or spinning. Exec gets a
// kill grace no cluster's latency approaches, so the command exits before Exec
// gives up on it and the row stays on the path it pins. The deadline is 3s, not
// 1s, so the staging finishes well before the watchdog would fire on a loaded
// node: at 1s it lost that race twice in 24 runs under heavy load.
func TestK8sOverrunThenExitIsATimeoutTheProbeCannotSee(t *testing.T) {
	sb := liveSandbox(t)
	k8s.SetKillGraceForTest(sb, 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: blindTheProbe, Timeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(res.Stdout, "disarmed") {
		t.Fatalf("the command never found its watchdog to kill, so the watchdog was left to fire and this row proves nothing; "+
			"it needs /proc/<pid>/task/<pid>/children, which this cluster's runtime may not provide: %+v", res)
	}
	if !strings.Contains(res.Stdout, "blinded") {
		t.Fatalf("the command could not point its pid file at a dead process, so the probe could still see it and this row proves nothing: %+v", res)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d, want the command's own 0: it disarmed its watchdog, yet something killed it or Exec gave up on it despite a 30s kill grace, so the row never reached the path it pins: %+v",
			res.ExitCode, res)
	}
	if !res.TimedOut {
		t.Errorf("a command that ran 5s against a 3s deadline and exited while the probe was blind was not a timeout: %+v", res)
	}
}

// A SIGKILL the watchdog did not deliver is the deadline's when it lands past
// the deadline, even where the pre-deadline probe cannot see the command alive
// (#838): #832's late probe, at the probe's other instant. On a loaded cluster
// that probe answers a round trip after it asks, past a kill it was sent to
// see. Nothing marks a kill the watchdog did not make: the tenant killed the
// watchdog, and the node's OOM killer, say, killed the command.
//
// So the row blinds the probe, as TestK8sOverrunThenExitIsATimeoutTheProbeCannotSee
// does, and the command SIGKILLs itself, standing in for the node, 0.25s past
// its 3s deadline. That is inside the 0.5s the overrun rule waits out, so
// only the wrapper's record from the watchdog's launch can call it a timeout.
// Its staging runs after the watchdog's launch, since disarming waits for the
// watchdog, so the record comes to the 3.25s less only the wrapper's step from
// that launch to its reading.
//
// The second row is the line's other side. A SIGKILL a second before the
// deadline is the command's own, as the shared contract says
// (ExecSelfInflictedKillIsNotATimeout), with the probe blind and the record
// still there to read.
func TestK8sSigkillTheWatchdogDidNotDeliver(t *testing.T) {
	sb := liveSandbox(t)
	k8s.SetKillGraceForTest(sb, 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, tc := range []struct {
		name, after string
		timedOut    bool
	}{
		{"past the deadline", "3.25", true},
		{"a second before the deadline", "2", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := sb.Exec(ctx, sandbox.ExecRequest{
				Command: blind + "  sleep " + tc.after + "\n  kill -9 $$\n", Timeout: 3 * time.Second,
			})
			if err != nil {
				t.Fatalf("exec: %v", err)
			}
			if !strings.Contains(res.Stdout, "disarmed") || !strings.Contains(res.Stdout, "blinded") {
				t.Fatalf("the command could not disarm its watchdog and blind the probe, so this row proves nothing: %+v", res)
			}
			if res.ExitCode != 137 || res.TimedOut != tc.timedOut {
				t.Errorf("a SIGKILL %ss into a 3s deadline: exit %d, timed out %v; want 137, %v: %+v",
					tc.after, res.ExitCode, res.TimedOut, tc.timedOut, res)
			}
		})
	}
}

// An image whose startup file turns errexit on (and prints, as
// sandboxtest.BannerHook does) runs it in the exec wrapper's own shell, which
// it used to end at the first command that failed (#860): `wait` on a command
// that exits 7, and the wrapper died before recording it, so the 7 read as a
// SIGKILL nobody sent. On that image as on the plain one, a command's own exit
// stands; a timeout is still the watchdog's 137, and a SIGKILL the command
// sent itself is still 137 and no timeout; and the overrun rules hold — a
// command that disarms its watchdog and blinds the probe is a timeout by the
// wrapper's record of its run (#832), whatever it exits with, and one that
// disarms it and runs on is a timeout by the probes (#95, #110).
func TestK8sExecUnderAnErrexitStartup(t *testing.T) {
	provider, err := k8s.New(k8s.Config{
		Context:   os.Getenv("MAP_K8S_CONTEXT"),
		Namespace: os.Getenv("MAP_K8S_NAMESPACE"),
	})
	if err != nil {
		t.Fatalf("this test requires a Kubernetes cluster: %v", err)
	}
	for _, image := range []struct{ name, image string }{
		{"plain", testImage},
		{"errexit startup", hookedtest.Image(t, "set -e\n"+sandboxtest.BannerHook)},
	} {
		t.Run(image.name, func(t *testing.T) {
			sb, err := provider.Provision(context.Background(), sandbox.Spec{
				SessionID:  domain.NewID("sesn"),
				Image:      image.image,
				Networking: domain.Networking{Type: domain.NetUnrestricted},
			})
			if err != nil {
				t.Fatalf("provision: %v", err)
			}
			t.Cleanup(func() { _ = sb.Destroy(context.Background()) })
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			for _, tc := range []struct {
				name, command string
				timeout       time.Duration
				grace         time.Duration
				code          int
				timedOut      bool
			}{
				{"a failing command's own exit", "exit 7", 30 * time.Second, 0, 7, false},
				{"a command killed on its deadline", "sleep 300", time.Second, 0, 137, true},
				{"a SIGKILL the command sent itself", "kill -9 $$", 30 * time.Second, 0, 137, false},
				{"an overrun the probe cannot see, then a clean exit", blindTheProbe, 3 * time.Second, 30 * time.Second, 0, true},
				{"an overrun the probe cannot see, then a failing exit", blindTheProbe + "exit 3\n", 3 * time.Second, 30 * time.Second, 3, true},
				// 3s, as blindTheProbe's rows: the command must find and kill
				// its watchdog before the watchdog fires, which 1s does not
				// leave room for on a loaded node.
				{"a disarmed watchdog and a command that runs on", disarmTheWatchdog + "sleep 987321", 3 * time.Second, 2 * time.Second, 137, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if tc.grace > 0 {
						k8s.SetKillGraceForTest(sb, tc.grace)
					}
					res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: tc.command, Timeout: tc.timeout})
					if err != nil {
						t.Fatalf("exec: %v", err)
					}
					if res.ExitCode != tc.code || res.TimedOut != tc.timedOut {
						t.Errorf("exit %d, timed out %v; want %d, %v: %+v", res.ExitCode, res.TimedOut, tc.code, tc.timedOut, res)
					}
					if strings.Contains(tc.command, disarmTheWatchdog) && !strings.Contains(res.Stdout, "disarmed") {
						t.Errorf("the command never found its watchdog to kill, so this row proves nothing: %+v", res)
					}
					if strings.HasPrefix(tc.command, blindTheProbe) && !strings.Contains(res.Stdout, "blinded") {
						t.Errorf("the command could not blind the probe, so this row proves nothing: %+v", res)
					}
				})
			}
		})
	}
}

// An image whose startup prints more than the output cap — 1.2 MB of "y\n" in
// every shell — pushes each exec's exit record out of its output, and Exec
// answers with the startup's error, which says the command ran — not with the
// kill's 137 for every command that a lost record would read as
// (readExitRecord; docs/self-hosted-security.md, the 1 MiB room).
func TestK8sExecUnderAFloodingStartupIsAnError(t *testing.T) {
	provider, err := k8s.New(k8s.Config{
		Context:   os.Getenv("MAP_K8S_CONTEXT"),
		Namespace: os.Getenv("MAP_K8S_NAMESPACE"),
	})
	if err != nil {
		t.Fatalf("this test requires a Kubernetes cluster: %v", err)
	}
	sb, err := provider.Provision(context.Background(), sandbox.Spec{
		SessionID:  domain.NewID("sesn"),
		Image:      hookedtest.Image(t, "yes | head -c 1200000\n"),
		Networking: domain.Networking{Type: domain.NetUnrestricted},
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Cleanup(func() { _ = sb.Destroy(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, command := range []string{"exit 0", "exit 7"} {
		res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: command, Timeout: 30 * time.Second})
		var startup *sandbox.StartupOutputError
		if !errors.As(err, &startup) || !startup.Ran {
			t.Errorf("%s under a flooding startup = exit %d, timed out %v, %v; want a StartupOutputError of a command that ran", command, res.ExitCode, res.TimedOut, err)
		}
	}
}
