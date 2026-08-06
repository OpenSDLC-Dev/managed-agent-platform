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

// nonRootDockerfile is the sandbox image contract met by an image that does NOT
// run as root: the platform's own default (`debian:stable-slim`) does, so the
// suite's every other row is blind to the difference. The workdir is given to that
// user, which is what an operator hardening a sandbox image does — without it
// nothing can write there at all, this backend included, and the row would be
// testing the wrong thing.
const nonRootDockerfile = `FROM debian:stable-slim
RUN useradd -m app && mkdir -p /workspace && chown app:app /workspace
USER app
`

// A bulk write must work on an image whose default user is not root, and this row
// exists because it did not: the daemon extracts an archive on the HOST, as root,
// so the parent directories it created for the members belonged to root, and the
// rename exec — which runs as the image's user — could move nothing into them.
// Skill materialization failed outright on every non-root image, where the
// file-at-a-time loop it replaced had worked, because that loop's own `mkdir -p`
// ran inside the sandbox. The batch now makes its directories the same way.
//
// It is a docker-backend row rather than a contract row because it is the docker
// backend's problem alone: the k8s backend extracts inside the pod, where
// everything is already the sandbox user's.
func TestBulkWriteOnANonRootImage(t *testing.T) {
	image := "map-nonroot-test:latest"
	build := exec.Command("docker", "build", "-q", "-t", image, "-")
	build.Stdin = strings.NewReader(nonRootDockerfile)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the non-root image: %v\n%s", err, out)
	}

	p, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	ctx := context.Background()
	sb, err := p.Provision(ctx, sandbox.Spec{
		SessionID: domain.NewID("sesn"), Image: image, Workdir: "/workspace",
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Cleanup(func() {
		if err := sb.Destroy(context.Background()); err != nil {
			t.Errorf("destroy: %v", err)
		}
	})
	if res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: "id -un"}); err != nil ||
		strings.TrimSpace(res.Stdout) != "app" {
		t.Fatalf("the sandbox runs as %q, %v; want the image's non-root user", res.Stdout, err)
	}

	// The baseline: a single write into a fresh nested directory works here, and
	// always did. A bulk write that cannot match it is a regression, not a limit.
	if err := sb.WriteFile(ctx, "/workspace/single/deep/a.txt", []byte("one")); err != nil {
		t.Fatalf("single write into a fresh nested directory: %v", err)
	}

	batch := []sandbox.FileWrite{
		{Path: "/workspace/skills/pack/SKILL.md", Data: []byte("# skill")},
		{Path: "/workspace/skills/pack/scripts/run.sh", Data: []byte("echo hi")},
	}
	if err := sb.WriteFiles(ctx, batch); err != nil {
		t.Fatalf("bulk write into a fresh nested directory: %v", err)
	}
	for _, f := range batch {
		if got, err := sb.ReadFile(ctx, f.Path); err != nil || string(got) != string(f.Data) {
			t.Errorf("%s = %q, %v; want %q", f.Path, got, err, f.Data)
		}
	}

	// The mechanism, not just the outcome: the directories belong to the sandbox
	// user, which is the whole reason the batch makes them itself.
	res, err := sb.Exec(ctx, sandbox.ExecRequest{
		Command: "stat -c '%U' /workspace/skills /workspace/skills/pack /workspace/skills/pack/scripts",
	})
	if err != nil {
		t.Fatalf("stat the directories: %v", err)
	}
	for _, owner := range strings.Fields(res.Stdout) {
		if owner != "app" {
			t.Errorf("a directory the batch created is owned by %q, want the sandbox user: "+
				"a root-owned parent is one the sandbox cannot rename into", owner)
		}
	}
	// And it left nothing of itself behind in the workdir.
	res, err = sb.Exec(ctx, sandbox.ExecRequest{
		Command: "ls -A /workspace | grep -c '^" + sandbox.TempPrefix + "' || true",
	})
	if err != nil {
		t.Fatalf("list the workdir: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "0" {
		t.Errorf("%s files left in the workdir = %s, want 0", sandbox.TempPrefix, got)
	}
}

// hookedDockerfile is the non-root image contract plus a bash startup hook the
// image's own environment names — an operator's `ENV BASH_ENV=…` pointing at a
// path the sandbox user can write. Unusual, but it is the sharp case for any
// privileged exec the cleanup might reach for, and nothing stops an operator
// from shipping it.
const hookedDockerfile = `FROM debian:stable-slim
RUN useradd -m app && mkdir -p /workspace && chown app:app /workspace
ENV BASH_ENV=/workspace/hook
USER app
`

// The platform runs nothing in the sandbox as anyone but the sandbox's own
// user, and this is the row that keeps it that way. #310's first fix cleaned up
// with an exec as uid 0, and on this image it executed the agent's file
// instead: `bash -c` sources $BASH_ENV before it runs anything, so the hook's
// record came back carrying uid 0 — twice per cleanup, once for the wrapper's
// shell and once for the command's. No exec does the cleaning now (the daemon
// empties what it landed), so what this row pins is the invariant rather than
// one hole in it: the hook still runs for the sandbox's own execs, because that
// shell is the agent's and its own uid is no escalation, and uid 0 must never
// be among them.
func TestTheRootShedRunsNoAgentCodeOnANonRootImage(t *testing.T) {
	image := "map-hooked-test:latest"
	build := exec.Command("docker", "build", "-q", "-t", image, "-")
	build.Stdin = strings.NewReader(hookedDockerfile)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the hooked image: %v\n%s", err, out)
	}

	p, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	ctx := context.Background()
	sb, err := p.Provision(ctx, sandbox.Spec{
		SessionID: domain.NewID("sesn"), Image: image, Workdir: "/workspace",
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Cleanup(func() {
		if err := sb.Destroy(context.Background()); err != nil {
			t.Errorf("destroy: %v", err)
		}
	})

	// The agent plants the hook, as itself — every shell in the container from
	// here on records the uid that ran it.
	if err := sb.WriteFile(ctx, "/workspace/hook", []byte("id -u >> /workspace/ran\n")); err != nil {
		t.Fatalf("plant the hook: %v", err)
	}
	// A write whose rename the sandbox user is refused — the route whose shed is
	// the daemon's, and the one an exec as uid 0 would have cleaned up.
	if err := sb.WriteFile(ctx, "/etc/map-310-hooked.txt", []byte("x")); !errors.Is(err, sandbox.ErrNotWritable) {
		t.Fatalf("err = %v, want ErrNotWritable — the route whose shed needs the daemon's credential", err)
	}

	res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: "cat /workspace/ran 2>/dev/null || true"})
	if err != nil {
		t.Fatalf("read the hook's record: %v", err)
	}
	// An image that never sourced the hook at all would satisfy "no uid 0
	// appears" without proving anything, so the record must first show the hook
	// firing for the sandbox's own execs — the mechanism a privileged exec would
	// have inherited.
	if len(strings.Fields(res.Stdout)) == 0 {
		t.Fatalf("the hook recorded nothing (%q): this row cannot tell a closed channel from an unused one", res.Stdout)
	}
	for _, uid := range strings.Fields(res.Stdout) {
		if uid == "0" {
			t.Fatalf("the hook ran as uid 0 (record: %q): the root shed must run no agent-chosen code", res.Stdout)
		}
	}
}

// A write into a parent the daemon can extract into but the sandbox user cannot
// write is the model's error, not the platform's fault (plan 23, #306). The PUT
// lands the temporary file because the daemon extracts as root; the `mv`,
// running as the image's user, is what gets refused — so the classification
// must happen at the rename, where the k8s backend (whose temporary file is
// created by the sandbox user) classifies the same sandbox at the create. A
// docker-backend row for TestBulkWriteOnANonRootImage's reason.
func TestWriteIntoARootOwnedParentOnANonRootImage(t *testing.T) {
	image := "map-nonroot-test:latest"
	build := exec.Command("docker", "build", "-q", "-t", image, "-")
	build.Stdin = strings.NewReader(nonRootDockerfile)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the non-root image: %v\n%s", err, out)
	}

	p, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	ctx := context.Background()
	sb, err := p.Provision(ctx, sandbox.Spec{
		SessionID: domain.NewID("sesn"), Image: image, Workdir: "/workspace",
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Cleanup(func() {
		if err := sb.Destroy(context.Background()); err != nil {
			t.Errorf("destroy: %v", err)
		}
	})

	err = sb.WriteFile(ctx, "/etc/map-306-rootparent.txt", []byte("x"))
	if !errors.Is(err, sandbox.ErrNotWritable) {
		t.Fatalf("err = %v, want ErrNotWritable", err)
	}
	var pnw *sandbox.PathNotWritableError
	if !errors.As(err, &pnw) || pnw.Reason != "Permission denied" {
		t.Fatalf("err = %v, want the refused move's reason", err)
	}

	// And the refused write's payload is gone. The temporary landed through the
	// daemon's root-credentialed extraction, so the sandbox user's own `rm -f`
	// could not shed it and the refused bytes would sit in a directory it
	// cannot touch for the container's whole life. The daemon takes back what
	// it put there, by extracting nothing over it: what is left where the
	// sandbox cannot unlink is an empty name, not a payload (#310).
	res, err := sb.Exec(ctx, sandbox.ExecRequest{
		Command: "cat /etc/" + sandbox.TempPrefix + "* 2>/dev/null | wc -c",
	})
	if err != nil {
		t.Fatalf("size what is left in /etc: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "0" {
		t.Errorf("%s bytes left in /etc = %s, want 0 (#310)", sandbox.TempPrefix, got)
	}
}

// The batch's own half of the row above (#316). #310 covered the single write;
// a bulk write lands the same way — the daemon extracts the members on the host,
// as root — and its only shed was the sandbox user's `rm`, in the script and in
// the discard exec alike. So a batch refused under a parent that user cannot
// write left every member's full payload in the sandbox for its whole life, one
// per member rather than one per write.
//
// The parent has to already exist for the gap to open: a batch makes its
// missing directories inside the sandbox, so those belong to the sandbox user —
// but `mkdir -p` leaves one that is already there exactly as it found it, and
// `/etc` is that directory on any image. Two members, so the row also pins that
// the emptying reaches the whole batch rather than the one the rename stopped
// on.
func TestBulkWriteIntoARootOwnedParentOnANonRootImage(t *testing.T) {
	image := "map-nonroot-test:latest"
	build := exec.Command("docker", "build", "-q", "-t", image, "-")
	build.Stdin = strings.NewReader(nonRootDockerfile)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the non-root image: %v\n%s", err, out)
	}

	p, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	ctx := context.Background()
	sb, err := p.Provision(ctx, sandbox.Spec{
		SessionID: domain.NewID("sesn"), Image: image, Workdir: "/workspace",
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Cleanup(func() {
		if err := sb.Destroy(context.Background()); err != nil {
			t.Errorf("destroy: %v", err)
		}
	})

	payload := strings.Repeat("A", 4096)
	err = sb.WriteFiles(ctx, []sandbox.FileWrite{
		{Path: "/etc/map-316-a.txt", Data: []byte(payload)},
		{Path: "/etc/map-316-b.txt", Data: []byte(payload)},
	})
	if err == nil {
		t.Fatal("the batch reported success writing into a parent the sandbox user cannot write")
	}

	// Neither member was written: the rename is what was refused.
	for _, path := range []string{"/etc/map-316-a.txt", "/etc/map-316-b.txt"} {
		if _, err := sb.ReadFile(ctx, path); !errors.Is(err, sandbox.ErrFileNotExist) {
			t.Errorf("%s: %v, want it never to have been written", path, err)
		}
	}

	// And what the daemon landed for them is gone. The sandbox user can neither
	// unlink the temporaries nor read them away, so what closes this is the
	// daemon emptying what it put there — leaving names without payloads.
	res, err := sb.Exec(ctx, sandbox.ExecRequest{
		Command: "cat /etc/" + sandbox.TempPrefix + "* 2>/dev/null | wc -c",
	})
	if err != nil {
		t.Fatalf("size what is left in /etc: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "0" {
		t.Errorf("%s bytes left in /etc = %s, want 0 (#316)", sandbox.TempPrefix, got)
	}
}
