package k8s_test

import (
	"context"
	"errors"
	"os"
	"testing"

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
