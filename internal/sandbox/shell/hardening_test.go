package shell_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/docker"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/shell"
)

// The persistent shell keeps its state at sandbox.ShellStateRoot, which is on
// the container's root filesystem — so a read-only-rootfs sandbox that did not
// mount writable space over it would fail the *first* bash call of every
// session, and fail it as a backend fault (the tool is left unanswered and the
// work item reclaims) rather than as a tool error the model could see. Every
// other row in this suite provisions an unhardened sandbox and cannot notice.
//
// The state carrying across calls is the second half: a writable mount that
// existed but was not shared between execs would pass a single command and lose
// cwd/env, which is the whole point of the persistent shell.
func TestShellSurvivesAReadOnlyRootFilesystem(t *testing.T) {
	provider, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("shell tests require Docker: %v", err)
	}
	sb, err := provider.Provision(context.Background(), sandbox.Spec{
		SessionID:  domain.NewID("sesn"),
		Image:      testImage,
		Workdir:    "/workspace",
		Networking: domain.Networking{Type: domain.NetUnrestricted},
		Hardening:  sandbox.Hardening{ReadOnlyRootfs: true},
	})
	if err != nil {
		t.Fatalf("provision a read-only-rootfs sandbox: %v", err)
	}
	t.Cleanup(func() {
		if err := sb.Destroy(context.Background()); err != nil {
			t.Errorf("destroy: %v", err)
		}
	})

	session := domain.NewID("sesn")
	run := func(cmd string) shell.Result {
		t.Helper()
		res, err := shell.Run(context.Background(), sb, session, domain.NewID("sevt"),
			shell.Request{Command: cmd, Timeout: 30 * time.Second})
		if err != nil {
			t.Fatalf("shell.Run(%q) faulted on a read-only-rootfs sandbox: %v", cmd, err)
		}
		return res
	}

	if got := run("echo hello"); strings.TrimSpace(got.Stdout) != "hello" {
		t.Fatalf("first bash call = %+v, want hello", got)
	}
	run("cd /tmp && export MAP_PROBE=carried")
	res := run("pwd; echo $MAP_PROBE")
	if !strings.Contains(res.Stdout, "/tmp") || !strings.Contains(res.Stdout, "carried") {
		t.Errorf("state across calls = %q, want the cwd and the exported variable", res.Stdout)
	}
}
