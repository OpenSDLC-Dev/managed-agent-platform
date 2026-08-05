package docker_test

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/docker"
)

// effectiveNetworkMode asks the real daemon — not the provider — what
// networking a container was created with; the adoption spec check guards this
// exact value, so the test must read it from the daemon's own inspect.
func effectiveNetworkMode(t *testing.T, id string) string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "-f", "{{.HostConfig.NetworkMode}}", id).Output()
	if err != nil {
		t.Fatalf("docker inspect %s: %v", id, err)
	}
	return strings.TrimSpace(string(out))
}

// A session provisioned unrestricted holds a `bridge` container, and
// re-provisioning the same session as limited must refuse it
// (sandbox.ErrSpecMismatch) rather than hand a fail-closed request the old
// route out — and must leave the container as it was, since a mismatch has no
// authority to delete. A limited session's own re-provision still adopts, and
// what it adopts is effectively `none` on the daemon (#29).
func TestAdoptionValidatesTheEffectiveNetworkMode(t *testing.T) {
	provider, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("this test requires Docker: %v", err)
	}
	ctx := context.Background()

	unrestricted := sandbox.Spec{
		SessionID:  domain.NewID("sesn"),
		Image:      testImage,
		Networking: domain.Networking{Type: domain.NetUnrestricted},
	}
	first, err := provider.Provision(ctx, unrestricted)
	if err != nil {
		t.Fatalf("provision (unrestricted): %v", err)
	}
	t.Cleanup(func() { first.Destroy(context.Background()) })
	if mode := effectiveNetworkMode(t, first.ID()); mode != "bridge" {
		t.Fatalf("unrestricted container mode = %q, want bridge", mode)
	}

	limited := unrestricted
	limited.Networking = domain.Networking{Type: domain.NetLimited}
	if _, err := provider.Provision(ctx, limited); !errors.Is(err, sandbox.ErrSpecMismatch) {
		t.Fatalf("limited provision over a bridge container: err = %v, want sandbox.ErrSpecMismatch", err)
	}
	if mode := effectiveNetworkMode(t, first.ID()); mode != "bridge" {
		t.Errorf("after the refusal, container mode = %q, want bridge (untouched)", mode)
	}

	fresh := sandbox.Spec{
		SessionID:  domain.NewID("sesn"),
		Image:      testImage,
		Networking: domain.Networking{Type: domain.NetLimited},
	}
	l1, err := provider.Provision(ctx, fresh)
	if err != nil {
		t.Fatalf("provision (limited): %v", err)
	}
	t.Cleanup(func() { l1.Destroy(context.Background()) })
	l2, err := provider.Provision(ctx, fresh)
	if err != nil {
		t.Fatalf("re-provision (limited): %v", err)
	}
	if l1.ID() != l2.ID() {
		t.Errorf("limited re-provision id = %q, want %q (adopted)", l2.ID(), l1.ID())
	}
	if mode := effectiveNetworkMode(t, l1.ID()); mode != "none" {
		t.Errorf("limited container mode = %q, want none", mode)
	}
}
