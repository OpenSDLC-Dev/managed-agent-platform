package docker_test

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/docker"
)

// A stopped container is a miss, not something to start. This is the refusal the
// shared contract cannot reach — it has no way to stop a sandbox without
// destroying it — and it is the whole difference between Attach and Provision,
// which starts exactly this container. A caller that only wants to write a file
// must not bring a session's sandbox back to life to do it.
func TestAttachRefusesAStoppedContainerAndLeavesItStopped(t *testing.T) {
	provider, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("this test requires Docker: %v", err)
	}
	ctx := context.Background()
	sid := domain.NewID("sesn")
	sb, err := provider.Provision(ctx, sandbox.Spec{
		SessionID:  sid,
		Image:      testImage,
		Networking: domain.Networking{Type: domain.NetUnrestricted},
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Cleanup(func() {
		if err := provider.Reap(context.Background(), sid); err != nil {
			t.Errorf("reap: %v", err)
		}
	})
	if out, err := exec.Command("docker", "stop", "-t", "0", sb.ID()).CombinedOutput(); err != nil {
		t.Fatalf("docker stop %s: %v: %s", sb.ID(), err, out)
	}

	got, err := provider.Attach(ctx, sid)
	if !errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("attach of a stopped container: %v, want ErrNotFound", err)
	}
	if got != nil {
		t.Errorf("attach returned a handle %v for a stopped container", got.ID())
	}
	if state := containerState(t, sb.ID()); state != "exited" {
		t.Errorf("container is %q after the refused attach, want it left exited", state)
	}
}

// A container holding the session's name without this platform's ownership label
// is an error rather than a miss — the same refusal every adoption path makes,
// and the reason it matters here is that a handle is a write primitive: reporting
// "no sandbox" for someone else's container would be right by accident today and
// wrong the moment a caller treats the miss as permission to create.
func TestAttachRefusesAContainerThatIsNotOurs(t *testing.T) {
	provider, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("this test requires Docker: %v", err)
	}
	sid := domain.NewID("sesn")
	name := "map-" + string(sid)
	if out, err := exec.Command("docker", "create", "--name", name, testImage, "sleep", "1").
		CombinedOutput(); err != nil {
		t.Fatalf("docker create %s: %v: %s", name, err, out)
	}
	t.Cleanup(func() {
		if out, err := exec.Command("docker", "rm", "-f", name).CombinedOutput(); err != nil {
			t.Errorf("docker rm %s: %v: %s", name, err, out)
		}
	})

	got, err := provider.Attach(context.Background(), sid)
	if err == nil || errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("attach of a container this platform does not own: %v, want a refusal", err)
	}
	if got != nil {
		t.Errorf("attach returned a handle %v for a container this platform does not own", got.ID())
	}
}

func containerState(t *testing.T, id string) string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Status}}", id).Output()
	if err != nil {
		t.Fatalf("docker inspect %s: %v", id, err)
	}
	return string(out[:len(out)-1])
}
