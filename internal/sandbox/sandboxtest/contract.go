// Package sandboxtest is the contract suite every sandbox.Provider must pass
// (CLAUDE.md: backend variability lives behind an interface with one shared
// suite). It is test support; production code must never import it.
//
// The suite asserts observable behavior only — what a tool would see — never a
// backend's internals. A new backend adds one test file that calls Run.
package sandboxtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// Harness is one backend under test. Image must name a Linux image carrying
// /bin/bash (the plan's image contract) and a POSIX userland.
type Harness struct {
	Provider sandbox.Provider
	Image    string
}

const workdir = "/workspace"

// Run exercises the sandbox.Provider contract. newHarness is called once per
// subtest so a backend can isolate its own fixtures.
func Run(t *testing.T, newHarness func(t *testing.T) Harness) {
	t.Helper()

	// provision gives each subtest a fresh session's sandbox, destroyed at the
	// end whatever the outcome.
	provision := func(t *testing.T, net domain.Networking) (sandbox.Sandbox, Harness, domain.ID) {
		t.Helper()
		h := newHarness(t)
		sid := domain.NewID("sesn")
		ctx := context.Background()
		sb, err := h.Provider.Provision(ctx, sandbox.Spec{
			SessionID: sid, Image: h.Image, Workdir: workdir, Networking: net,
		})
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		t.Cleanup(func() {
			if err := sb.Destroy(context.Background()); err != nil {
				t.Errorf("destroy: %v", err)
			}
		})
		if sb.ID() == "" {
			t.Error("sandbox has no id")
		}
		return sb, h, sid
	}
	unrestricted := domain.Networking{Type: domain.NetUnrestricted}

	t.Run("ExecCapturesBothStreams", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
			Command: `echo out; echo err >&2`,
		})
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		if res.Stdout != "out\n" || res.Stderr != "err\n" {
			t.Errorf("stdout=%q stderr=%q", res.Stdout, res.Stderr)
		}
		if res.ExitCode != 0 || res.TimedOut || res.Truncated {
			t.Errorf("result = %+v", res)
		}
	})

	t.Run("ExecReportsExitCode", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		res, err := sb.Exec(context.Background(), sandbox.ExecRequest{Command: `exit 7`})
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		if res.ExitCode != 7 {
			t.Errorf("exit code = %d, want 7", res.ExitCode)
		}
		if res.TimedOut {
			t.Error("a plain non-zero exit must not read as a timeout")
		}
	})

	t.Run("ExecRunsInWorkdir", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		res, err := sb.Exec(context.Background(), sandbox.ExecRequest{Command: `pwd`})
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		if strings.TrimSpace(res.Stdout) != workdir {
			t.Errorf("pwd = %q, want %q", strings.TrimSpace(res.Stdout), workdir)
		}
	})

	// A hung command must not hang the executor, and it must not poison the
	// sandbox: the next tool call still works.
	t.Run("ExecTimeoutKillsAndSurvives", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		start := time.Now()
		res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: `sleep 300`, Timeout: time.Second})
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		if !res.TimedOut {
			t.Errorf("result = %+v, want TimedOut", res)
		}
		if elapsed := time.Since(start); elapsed > 30*time.Second {
			t.Errorf("timeout took %s — the command was not killed", elapsed)
		}

		after, err := sb.Exec(ctx, sandbox.ExecRequest{Command: `echo alive`})
		if err != nil {
			t.Fatalf("exec after timeout: %v", err)
		}
		if after.Stdout != "alive\n" || after.TimedOut {
			t.Errorf("sandbox unusable after a timeout: %+v", after)
		}
	})

	// A command that exits with timeout(1)'s own code, quickly, is not a
	// timeout: the flag comes from the sandbox killing it, not from a number.
	// And a command that finishes early is reported early — a deadline must
	// never be enforced by waiting the deadline out.
	t.Run("ExecFastExit124IsNotATimeout", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		start := time.Now()
		res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
			Command: `exit 124`, Timeout: 30 * time.Second,
		})
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		if res.ExitCode != 124 {
			t.Errorf("exit code = %d, want 124", res.ExitCode)
		}
		if res.TimedOut {
			t.Error("exit 124 before the deadline must not read as a timeout")
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Errorf("a command that exited at once took %s to report", elapsed)
		}
	})

	// Unbounded output must not be able to kill the executor, and the command
	// must still be allowed to finish rather than block on a full pipe.
	t.Run("ExecCapsOutput", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		res, err := sb.Exec(ctx, sandbox.ExecRequest{
			Command: `yes a | head -c 1400000; echo done >&2`,
		})
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		if len(res.Stdout) != sandbox.MaxOutputBytes {
			t.Errorf("stdout kept %d bytes, want the %d-byte cap", len(res.Stdout), sandbox.MaxOutputBytes)
		}
		if !res.Truncated {
			t.Error("Truncated not reported")
		}
		if res.ExitCode != 0 {
			t.Errorf("exit code = %d — the drained command did not finish cleanly", res.ExitCode)
		}
		if res.Stderr != "done\n" {
			t.Errorf("stderr = %q — capping one stream must not lose the other", res.Stderr)
		}
	})

	t.Run("FileRoundTripCreatesParents", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		ctx := context.Background()
		// Bytes no shell round-trip would survive: NUL, high bytes, no newline.
		want := []byte{0x00, 0x01, 0xff, 0xfe, 'h', 'i', 0x00}
		path := workdir + "/deep/nested/dir/blob.bin"
		if err := sb.WriteFile(ctx, path, want); err != nil {
			t.Fatalf("write: %v", err)
		}
		got, err := sb.ReadFile(ctx, path)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("read back %v, want %v", got, want)
		}

		// Overwrite truncates rather than merging.
		if err := sb.WriteFile(ctx, path, []byte("x")); err != nil {
			t.Fatalf("overwrite: %v", err)
		}
		got, err = sb.ReadFile(ctx, path)
		if err != nil {
			t.Fatalf("read after overwrite: %v", err)
		}
		if string(got) != "x" {
			t.Errorf("after overwrite = %q, want %q", got, "x")
		}

		// An empty file is a file, not a missing one.
		empty := workdir + "/empty"
		if err := sb.WriteFile(ctx, empty, nil); err != nil {
			t.Fatalf("write empty: %v", err)
		}
		if got, err := sb.ReadFile(ctx, empty); err != nil || len(got) != 0 {
			t.Errorf("read empty = %q, %v", got, err)
		}
	})

	// Files and commands see one filesystem — the whole point of the sandbox.
	t.Run("FilesAndExecShareTheFilesystem", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		ctx := context.Background()
		if err := sb.WriteFile(ctx, workdir+"/greeting.txt", []byte("hello\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
		res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: `cat greeting.txt`})
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		if res.Stdout != "hello\n" {
			t.Errorf("cat = %q", res.Stdout)
		}

		if _, err := sb.Exec(ctx, sandbox.ExecRequest{Command: `printf 'made by bash' > made.txt`}); err != nil {
			t.Fatalf("exec write: %v", err)
		}
		got, err := sb.ReadFile(ctx, workdir+"/made.txt")
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(got) != "made by bash" {
			t.Errorf("read = %q", got)
		}
	})

	t.Run("ReadFileMissing", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		_, err := sb.ReadFile(context.Background(), workdir+"/nope.txt")
		if !errors.Is(err, sandbox.ErrFileNotExist) {
			t.Errorf("err = %v, want ErrFileNotExist", err)
		}
	})

	// The sandbox filesystem is agent-controlled, so a read is an
	// untrusted-length allocation. Refuse it; never truncate silently.
	t.Run("ReadFileTooLarge", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		ctx := context.Background()
		res, err := sb.Exec(ctx, sandbox.ExecRequest{
			Command: fmt.Sprintf("head -c %d /dev/zero > big.bin", sandbox.MaxFileBytes+1),
		})
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("stage oversized file: %+v, %v", res, err)
		}
		if _, err := sb.ReadFile(ctx, workdir+"/big.bin"); !errors.Is(err, sandbox.ErrFileTooLarge) {
			t.Errorf("err = %v, want ErrFileTooLarge", err)
		}
	})

	t.Run("ReadFileDirectory", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		_, err := sb.ReadFile(context.Background(), workdir)
		if !errors.Is(err, sandbox.ErrIsDirectory) {
			t.Errorf("err = %v, want ErrIsDirectory", err)
		}
	})

	// Two executors handling two tool calls of the same session must land in
	// the same sandbox, not race to create two.
	t.Run("ProvisionIsIdempotentPerSession", func(t *testing.T) {
		h := newHarness(t)
		ctx := context.Background()
		spec := sandbox.Spec{
			SessionID: domain.NewID("sesn"), Image: h.Image,
			Workdir: workdir, Networking: unrestricted,
		}
		first, err := h.Provider.Provision(ctx, spec)
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		t.Cleanup(func() { _ = first.Destroy(context.Background()) })
		if err := first.WriteFile(ctx, workdir+"/state", []byte("kept")); err != nil {
			t.Fatalf("write: %v", err)
		}

		second, err := h.Provider.Provision(ctx, spec)
		if err != nil {
			t.Fatalf("re-provision: %v", err)
		}
		if second.ID() != first.ID() {
			t.Fatalf("re-provision made a new sandbox: %s != %s", second.ID(), first.ID())
		}
		got, err := second.ReadFile(ctx, workdir+"/state")
		if err != nil || string(got) != "kept" {
			t.Errorf("re-provisioned sandbox lost state: %q, %v", got, err)
		}
	})

	t.Run("DestroyIsIdempotentAndFinal", func(t *testing.T) {
		h := newHarness(t)
		ctx := context.Background()
		sb, err := h.Provider.Provision(ctx, sandbox.Spec{
			SessionID: domain.NewID("sesn"), Image: h.Image,
			Workdir: workdir, Networking: unrestricted,
		})
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		if err := sb.Destroy(ctx); err != nil {
			t.Fatalf("destroy: %v", err)
		}
		if err := sb.Destroy(ctx); err != nil {
			t.Errorf("second destroy: %v, want nil", err)
		}
		if _, err := sb.Exec(ctx, sandbox.ExecRequest{Command: `echo hi`}); !errors.Is(err, sandbox.ErrNotFound) {
			t.Errorf("exec after destroy: %v, want ErrNotFound", err)
		}
		if _, err := sb.ReadFile(ctx, workdir+"/anything"); !errors.Is(err, sandbox.ErrNotFound) {
			t.Errorf("read after destroy: %v, want ErrNotFound", err)
		}
	})

	// `limited` networking is enforced as no egress at all until the egress
	// proxy lands: fail closed, never silently unrestricted. The routing table
	// is the honest probe — a network namespace can carry down, unconfigured
	// tunnel devices from the host kernel and still reach nothing.
	t.Run("LimitedNetworkingHasNoEgressRoute", func(t *testing.T) {
		sb, _, _ := provision(t, domain.Networking{
			Type: domain.NetLimited, AllowedHosts: []string{"example.com"},
		})
		if routes := routeCount(t, sb); routes != 0 {
			t.Errorf("limited sandbox has %d routes, want none", routes)
		}
	})

	t.Run("UnrestrictedNetworkingHasAnEgressRoute", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		if routes := routeCount(t, sb); routes == 0 {
			t.Error("unrestricted sandbox has no route out")
		}
	})
}

// routeCount reads the sandbox's kernel routing table, minus its header line.
func routeCount(t *testing.T, sb sandbox.Sandbox) int {
	t.Helper()
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{Command: `cat /proc/net/route`})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("cat /proc/net/route: exit %d: %s", res.ExitCode, res.Stderr)
	}
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	return len(lines) - 1
}
