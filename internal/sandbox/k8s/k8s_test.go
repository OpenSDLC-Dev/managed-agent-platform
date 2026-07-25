package k8s_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

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
		return sandboxtest.Harness{Provider: provider, Image: testImage}
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

// A write that fails inside the pod must surface as an error, not a silent
// success: the docker backend surfaces the daemon's error the same way.
func TestK8sWriteFileSurfacesFailure(t *testing.T) {
	sb := liveSandbox(t)
	// The workdir is a directory; writing a file over it cannot succeed.
	if err := sb.WriteFile(context.Background(), sandbox.DefaultWorkdir, []byte("x")); err == nil {
		t.Error("WriteFile onto a directory returned nil, want an error")
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

// recordingMinter counts mints so a test can prove the backend never touched
// the gate-token seam.
type recordingMinter struct{ generated int }

func (m *recordingMinter) Generate() string { m.generated++; return "gtk_ignored" }

func (m *recordingMinter) Persist(context.Context, domain.ID, string) error { return nil }

// Until slice 4d wires the K8s gate sidecar, a Spec carrying a Gate must be
// accepted and ignored — that is Spec.Gate's documented contract. Provision
// succeeds, the pre-gate fail-closed isolation stands (no route out), and the
// token seam is never touched.
func TestK8sIgnoresGateSpecUntilSidecarLands(t *testing.T) {
	provider, err := k8s.New(k8s.Config{
		Context:   os.Getenv("MAP_K8S_CONTEXT"),
		Namespace: os.Getenv("MAP_K8S_NAMESPACE"),
	})
	if err != nil {
		t.Fatalf("these tests require a Kubernetes cluster: %v", err)
	}
	m := &recordingMinter{}
	sb, err := provider.Provision(context.Background(), sandbox.Spec{
		SessionID:  domain.NewID("sesn"),
		Image:      testImage,
		Networking: domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{"example.com"}},
		Gate:       &sandbox.GateSpec{Image: "gate:ignored", ControlplaneURL: "http://cp.invalid", TokenMinter: m},
	})
	if err != nil {
		t.Fatalf("provision with Spec.Gate faulted, want it ignored: %v", err)
	}
	t.Cleanup(func() { _ = sb.Destroy(context.Background()) })
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{Command: "cat /proc/net/route"})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("read routes: %+v %v", res, err)
	}
	if routes := len(strings.Split(strings.TrimSpace(res.Stdout), "\n")) - 1; routes != 0 {
		t.Errorf("gate-ignoring limited pod has %d routes, want the pre-gate no-route isolation", routes)
	}
	if m.generated != 0 {
		t.Errorf("K8s backend minted %d gate tokens; Spec.Gate must be ignored until 4d", m.generated)
	}
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
