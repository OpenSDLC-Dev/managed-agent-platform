package k8s_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
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
	kubeCtx := clusterContext(t)
	loadGateImage(t, kubeCtx, image)
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

// clusterContext resolves the kube context the tests run against —
// MAP_K8S_CONTEXT when set (local), otherwise the kubeconfig's current context
// (CI, where the kind-action sets it).
func clusterContext(t *testing.T) string {
	t.Helper()
	if ctx := os.Getenv("MAP_K8S_CONTEXT"); ctx != "" {
		return ctx
	}
	cfg, err := clientcmd.NewDefaultClientConfigLoadingRules().Load()
	if err != nil {
		t.Fatalf("load kubeconfig for the gate fixture: %v", err)
	}
	return cfg.CurrentContext
}

// loadGateImage sideloads the locally-built gate image into a kind cluster —
// kind's containerd cannot see the host daemon's images. `docker save
// --platform` keeps the archive single-platform (a multi-arch manifest breaks
// `kind load` on darwin; an older docker without the flag falls back to a
// plain save). Non-kind contexts (Docker Desktop) share the daemon's image
// store, so there is nothing to load.
func loadGateImage(t *testing.T, kubeCtx, image string) {
	t.Helper()
	cluster, ok := strings.CutPrefix(kubeCtx, "kind-")
	if !ok {
		// Not kind: assume the cluster shares the local daemon's image store
		// (docker-desktop does). Other clusters (minikube, k3d, remote) must have
		// the image loaded by hand before the run — MAP_K8S_HOST_ADDR fixes only
		// how pods address the stub controlplane, not image distribution.
		return
	}
	tar := filepath.Join(t.TempDir(), "gate.tar")
	if out, err := exec.Command("docker", "save", "--platform", "linux/"+runtime.GOARCH, "-o", tar, image).CombinedOutput(); err != nil {
		if out2, err2 := exec.Command("docker", "save", "-o", tar, image).CombinedOutput(); err2 != nil {
			t.Fatalf("docker save %s: %v\n%s\nfallback: %v\n%s", image, err, out, err2, out2)
		}
	}
	if out, err := exec.Command("kind", "load", "image-archive", tar, "--name", cluster).CombinedOutput(); err != nil {
		t.Fatalf("kind load image-archive: %v\n%s", err, out)
	}
}

// k8sHostAddr answers "an address of the test host reachable from a pod" for
// the cluster flavors the suite runs on: the kind docker network's IPv4
// gateway (a kind node routes to the host through it), or Docker Desktop's
// host.docker.internal. MAP_K8S_HOST_ADDR overrides both for anything else.
func k8sHostAddr(t *testing.T, kubeCtx string) string {
	t.Helper()
	if addr := os.Getenv("MAP_K8S_HOST_ADDR"); addr != "" {
		return addr
	}
	if !strings.HasPrefix(kubeCtx, "kind-") {
		return "host.docker.internal"
	}
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
// (~1s) later. The bound is generous so cluster exec latency does not flake it,
// while a regression to the old up-to-1s-late EOF still trips it.
func TestK8sTimedFastCommandReturnsPromptly(t *testing.T) {
	sb := liveSandbox(t)
	start := time.Now()
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Command: "echo hi", Timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "hi" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if elapsed := time.Since(start); elapsed > 900*time.Millisecond {
		t.Errorf("a quick timed command took %s — the watchdog is pinning the exec stream open again", elapsed)
	}
}
