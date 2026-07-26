package docker_test

import (
	"context"
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
