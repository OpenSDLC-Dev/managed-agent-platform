package docker_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	stopContainer(t, sb.ID())

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
	if out, err := dockerCLI(context.Background(), "create", "--name", name, testImage, "sleep", "1").
		CombinedOutput(); err != nil {
		t.Fatalf("docker create %s: %v: %s", name, err, out)
	}
	t.Cleanup(func() {
		if out, err := dockerCLI(context.Background(), "rm", "-f", name).CombinedOutput(); err != nil {
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

// cliBudget bounds the CLI calls that answer in well under a second, so a
// daemon that accepts the connection and then says nothing fails a named test
// rather than the whole package's `go test` alarm. The image builds are
// deliberately outside it — a build's duration is not this test's to bound.
const cliBudget = 30 * time.Second

// stopContainer leaves a fixture container stopped but still present, which is
// a state the sandbox API cannot reach: it has no stop, and Reap destroys. The
// stop must not travel through the container either — an in-container `kill 1`
// takes down the exec carrying it along with the container, so that exec's own
// status races the teardown it asked for (#625). The deadline is what makes a
// wedged daemon fail here by name rather than hang the package to its own.
func stopContainer(t *testing.T, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), cliBudget)
	defer cancel()
	// The budget is named in the message because a killed child reports
	// "signal: killed", which otherwise reads as though the container was.
	if out, err := dockerCLI(ctx, "stop", "-t", "0", id).CombinedOutput(); err != nil {
		t.Fatalf("docker stop %s within %s: %v: %s", id, cliBudget, err, out)
	}
}

func containerState(t *testing.T, id string) string {
	t.Helper()
	// The two streams are kept apart in both directions: the status is parsed
	// out of stdout, so a warning must not land inside it, and the daemon's own
	// message ("No such object: ...") is what makes a failure here readable
	// rather than a bare "exit status 1".
	ctx, cancel := context.WithTimeout(context.Background(), cliBudget)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := dockerCLI(ctx, "inspect", "-f", "{{.State.Status}}", id)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("docker inspect %s: %v: %s", id, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return strings.TrimSpace(stdout.String())
}

// The whole mechanism rests on `--host` outranking the context, which is a
// property of the CLI rather than of this code — and one that could change under
// a CLI version this repo does not pin. So it is asserted against the real one.
// The first half is the control: a context that does not exist must break a bare
// `docker`, or the second half would prove only that it had been ignored (#627).
func TestTheHostFlagOutranksTheContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), cliBudget)
	defer cancel()
	t.Setenv("DOCKER_CONTEXT", "map-no-such-context")
	if out, err := exec.CommandContext(ctx, "docker", "version").CombinedOutput(); err == nil {
		t.Fatalf("a context that does not exist was ignored, so this test proves nothing: %s", out)
	}
	if out, err := dockerCLI(ctx, "version").CombinedOutput(); err != nil {
		t.Fatalf("--host did not outrank DOCKER_CONTEXT: %v: %s", err, out)
	}
}

// containerState parses the daemon's reply, so what it trims off matters: the
// status arrives without a trailing newline often enough that slicing a fixed
// last byte off it returned "exite". A fake `docker` is the only way to pin the
// reply's exact shape, and it needs no daemon (#627).
func TestTheStateHelperTrimsRatherThanSlices(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"),
		[]byte("#!/bin/sh\nprintf 'exited'\n"), 0o755); err != nil {
		t.Fatalf("write the fake docker: %v", err)
	}
	t.Setenv("PATH", dir)
	if got := containerState(t, "any-id"); got != "exited" {
		t.Errorf("state %q, want %q", got, "exited")
	}
}

// dockerCLI is `docker`, aimed at the daemon this package's provider resolved.
// Left alone the CLI follows the active `docker context`, while the provider
// reads only DOCKER_HOST and then the well-known socket — so on a host where
// those name different daemons a fixture is created against one and read back
// from the other, and the test reports a missing container as though the product
// had lost it (#627). Every `docker` this package's own test files run goes
// through here — but not the gate image `sandboxtest` builds for the contract
// suite, which is in a package that cannot reach this seam.
//
// The address is a flag rather than DOCKER_HOST because DOCKER_CONTEXT overrides
// that variable; `--host` outranks both, which is measurable: `DOCKER_CONTEXT`
// naming no context fails on its own and succeeds beside `--host`.
func dockerCLI(ctx context.Context, arg ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "docker",
		append([]string{"--host", docker.DaemonHostForTest()}, arg...)...)
}
