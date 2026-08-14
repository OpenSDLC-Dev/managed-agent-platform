package k8s_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/k8s"
)

// applyPod creates a pod straight through kubectl, which is how a test reaches a
// state Provision will not leave behind — a pod that never becomes ready, or one
// carrying the session's derived name without its ownership label.
func applyPod(t *testing.T, manifest string) {
	t.Helper()
	args := []string{"apply", "-f", "-"}
	if ns := os.Getenv("MAP_K8S_NAMESPACE"); ns != "" {
		args = append(args, "-n", ns)
	}
	if kctx := os.Getenv("MAP_K8S_CONTEXT"); kctx != "" {
		args = append(args, "--context", kctx)
	}
	cmd := exec.Command("kubectl", args...)
	cmd.Stdin = strings.NewReader(manifest)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("kubectl apply: %v: %s", err, out)
	}
}

func attachTestProvider(t *testing.T) *k8s.Provider {
	t.Helper()
	provider, err := k8s.New(k8s.Config{
		Context:   os.Getenv("MAP_K8S_CONTEXT"),
		Namespace: os.Getenv("MAP_K8S_NAMESPACE"),
	})
	if err != nil {
		t.Fatalf("this test requires a Kubernetes cluster: %v", err)
	}
	return provider
}

// podNameFor mirrors the backend's derivation, so a test can plant a pod at the
// name Attach will look for.
func podNameFor(sid domain.ID) string {
	return "map-" + strings.ReplaceAll(strings.ToLower(string(sid)), "_", "-")
}

// A pod that is not ready is a miss, not something to wait for — and the waiting
// is exactly what must not happen here. Provision's readiness wait ends in
// reclaimUnready for a gated pod, so an Attach that waited under a caller's
// shorter deadline would have a merely slow pod deleted out from under a running
// turn. The shared contract cannot reach this state: it has no way to make a pod
// unready without destroying it.
func TestK8sAttachRefusesAPodThatIsNotReady(t *testing.T) {
	provider := attachTestProvider(t)
	sid := domain.NewID("sesn")
	name := podNameFor(sid)
	applyPod(t, fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: %s
  labels:
    dev.opensdlc.managed-agent-platform.session-id: %s
spec:
  restartPolicy: Never
  containers:
    - name: sandbox
      image: %s
      workingDir: /workspace
      command: ["sleep", "3600"]
      readinessProbe:
        exec:
          command: ["false"]
        periodSeconds: 3600
        initialDelaySeconds: 3600
`, name, sid, testImage))
	t.Cleanup(func() {
		if err := provider.Reap(context.Background(), sid); err != nil {
			t.Errorf("reap: %v", err)
		}
	})

	got, err := provider.Attach(context.Background(), sid)
	if !errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("attach of a pod that is not ready: %v, want ErrNotFound", err)
	}
	if got != nil {
		t.Errorf("attach returned a handle %v for a pod that is not ready", got.ID())
	}
	// The refusal deletes nothing: a pod that is only slow must survive being
	// asked about, which is the whole reason this seam exists.
	if uid := podUID(t, name); uid == "" {
		t.Errorf("the refused attach removed pod %s", name)
	}
}

// A pod holding the session's derived name without this platform's ownership
// label is an error rather than a miss — the same refusal every adoption path
// makes. A handle is a write primitive, so answering "no sandbox" for someone
// else's pod would be right by accident today and wrong the moment a caller
// reads the miss as permission to create.
func TestK8sAttachRefusesAPodThatIsNotOurs(t *testing.T) {
	provider := attachTestProvider(t)
	sid := domain.NewID("sesn")
	name := podNameFor(sid)
	applyPod(t, fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: %s
spec:
  restartPolicy: Never
  containers:
    - name: sandbox
      image: %s
      workingDir: /workspace
      command: ["sleep", "3600"]
`, name, testImage))
	t.Cleanup(func() {
		args := []string{"delete", "pod", name, "--ignore-not-found", "--grace-period=0", "--force"}
		if ns := os.Getenv("MAP_K8S_NAMESPACE"); ns != "" {
			args = append(args, "-n", ns)
		}
		if kctx := os.Getenv("MAP_K8S_CONTEXT"); kctx != "" {
			args = append(args, "--context", kctx)
		}
		if out, err := exec.Command("kubectl", args...).CombinedOutput(); err != nil {
			t.Errorf("kubectl delete pod %s: %v: %s", name, err, out)
		}
	})

	got, err := provider.Attach(context.Background(), sid)
	if err == nil || errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("attach of a pod this platform does not own: %v, want a refusal", err)
	}
	if got != nil {
		t.Errorf("attach returned a handle %v for a pod this platform does not own", got.ID())
	}
}
