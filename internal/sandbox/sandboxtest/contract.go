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
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// Harness is one backend under test. Image must name a Linux image carrying
// /bin/bash (the plan's image contract) and a POSIX userland — plus `tar`, which
// a backend that extracts a bulk write's archive inside the sandbox needs (the
// k8s backend does; the docker daemon extracts on the host, so that backend does
// not).
//
// Gate, when non-nil, declares that the backend runs the per-session egress
// gate: it is called once per gated subtest and returns the fixture those rows
// provision with (a GateSpec plus the egress targets the fixture serves). A
// backend that does not yet run a gate leaves it nil — the gated rows are not
// registered, and the ungated limited-networking row keeps the fail-closed
// no-route expectation for every backend.
//
// EnforcesPidsLimit declares that the backend's runtime can cap a single
// sandbox's process count (sandbox.Hardening.PidsLimit). Docker can; the
// Kubernetes Pod API cannot express a per-pod pids limit at all, so that
// backend leaves it false and the row is not registered — one of the two
// hardening dimensions the backends genuinely cannot share, both recorded in
// docs/DIVERGENCES.md.
//
// The other one, EphemeralStorageBytes, is Kubernetes-only and gets no flag
// here, because no flag would gate anything: every row in this suite asserts
// what a tool *inside* the sandbox can observe, and a disk cap is enforced by
// the kubelet evicting the pod rather than by any cgroup file the sandbox could
// read. It is asserted at the provider level on both backends instead — the pod
// spec carries it, the Docker create payload provably does not — the way the
// memory cap is, since observing that one means being OOM-killed.
type Harness struct {
	Provider          sandbox.Provider
	Image             string
	Gate              func(t *testing.T) GateFixture
	EnforcesPidsLimit bool
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

	// provisionHardened gives a subtest a sandbox created with the containment it
	// asks for. Hardening is bound at create, exactly as Env and Networking are,
	// so a row that wants a different one needs its own sandbox.
	provisionHardened := func(t *testing.T, hardening sandbox.Hardening) sandbox.Sandbox {
		t.Helper()
		h := newHarness(t)
		sb, err := h.Provider.Provision(context.Background(), sandbox.Spec{
			SessionID: domain.NewID("sesn"), Image: h.Image, Workdir: workdir,
			Networking: unrestricted, Hardening: hardening,
		})
		if err != nil {
			t.Fatalf("provision (hardening %+v): %v", hardening, err)
		}
		t.Cleanup(func() {
			if err := sb.Destroy(context.Background()); err != nil {
				t.Errorf("destroy: %v", err)
			}
		})
		return sb
	}

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

	// Spec.Env is injected at provision time and visible to every tool exec —
	// the seam slice 4 uses to hand the sandbox its egress-proxy address and
	// vault placeholder env vars. A value with a space proves no word-splitting;
	// a value containing `$(...)` proves the two backends agree it is opaque —
	// Kubernetes would otherwise expand it while Docker keeps it literal.
	t.Run("SpecEnvReachesExec", func(t *testing.T) {
		h := newHarness(t)
		ctx := context.Background()
		sb, err := h.Provider.Provision(ctx, sandbox.Spec{
			SessionID: domain.NewID("sesn"), Image: h.Image, Workdir: workdir,
			Networking: unrestricted,
			Env: map[string]string{
				"VAULT_SEAM_ONE":   "alpha",
				"VAULT_SEAM_TWO":   "beta gamma",
				"VAULT_SEAM_THREE": "$(VAULT_SEAM_ONE)/$lit",
			},
		})
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		t.Cleanup(func() {
			if err := sb.Destroy(context.Background()); err != nil {
				t.Errorf("destroy: %v", err)
			}
		})
		res, err := sb.Exec(ctx, sandbox.ExecRequest{
			Command: `printf '%s|%s|%s' "$VAULT_SEAM_ONE" "$VAULT_SEAM_TWO" "$VAULT_SEAM_THREE"`,
		})
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		const want = `alpha|beta gamma|$(VAULT_SEAM_ONE)/$lit`
		if res.Stdout != want {
			t.Errorf("Spec.Env not visible to exec verbatim: stdout=%q, want %q", res.Stdout, want)
		}
	})

	// An invalid env-var name is refused up front by both backends, identically —
	// never a silent Docker mis-parse (KEY "A=B" folded into the value) or an
	// opaque Kubernetes pod rejection at create — and nothing is created.
	t.Run("SpecEnvRejectsInvalidKey", func(t *testing.T) {
		h := newHarness(t)
		ctx := context.Background()
		sid := domain.NewID("sesn")
		sb, err := h.Provider.Provision(ctx, sandbox.Spec{
			SessionID: sid, Image: h.Image, Workdir: workdir,
			Networking: unrestricted,
			Env:        map[string]string{"A=B": "x"},
		})
		if err == nil {
			// Nothing should have been created, but don't leak if it was.
			_ = sb.Destroy(ctx)
			t.Fatal("provision accepted an invalid env-var name")
		}
		// The rejection must happen before anything is created: a later provision
		// of the SAME session with a valid Env creates a fresh sandbox and sees
		// exactly that Env. Had the rejected attempt leaked a container/pod, the
		// idempotent provision would adopt it — and adoption never re-applies Env
		// — so the valid value would be missing.
		sb2, err := h.Provider.Provision(ctx, sandbox.Spec{
			SessionID: sid, Image: h.Image, Workdir: workdir,
			Networking: unrestricted,
			Env:        map[string]string{"VALID_KEY": "ok"},
		})
		if err != nil {
			t.Fatalf("provision after a rejected one: %v", err)
		}
		t.Cleanup(func() {
			if err := sb2.Destroy(context.Background()); err != nil {
				t.Errorf("destroy: %v", err)
			}
		})
		res, err := sb2.Exec(ctx, sandbox.ExecRequest{Command: `printf '%s' "$VALID_KEY"`})
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		if res.Stdout != "ok" {
			t.Errorf("a resource leaked from the rejected provision (stale sandbox adopted): stdout=%q", res.Stdout)
		}
	})

	// Env is bound when the sandbox is first created. Re-provisioning the same
	// session with a changed Env adopts the running sandbox — same id — and does
	// NOT re-apply the new value, the property the egress gate relies on to keep
	// a session's placeholders stable across an executor restart.
	t.Run("SpecEnvBoundAtProvision", func(t *testing.T) {
		h := newHarness(t)
		ctx := context.Background()
		sid := domain.NewID("sesn")
		spec := func(v string) sandbox.Spec {
			return sandbox.Spec{
				SessionID: sid, Image: h.Image, Workdir: workdir,
				Networking: unrestricted, Env: map[string]string{"BOUND": v},
			}
		}
		sb1, err := h.Provider.Provision(ctx, spec("first"))
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		t.Cleanup(func() {
			if err := sb1.Destroy(context.Background()); err != nil {
				t.Errorf("destroy: %v", err)
			}
		})
		sb2, err := h.Provider.Provision(ctx, spec("second"))
		if err != nil {
			t.Fatalf("re-provision: %v", err)
		}
		if sb2.ID() != sb1.ID() {
			t.Errorf("re-provision created a new sandbox %q, want the adopted %q", sb2.ID(), sb1.ID())
		}
		res, err := sb2.Exec(ctx, sandbox.ExecRequest{Command: `printf '%s' "$BOUND"`})
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		if res.Stdout != "first" {
			t.Errorf("adoption re-applied Env: BOUND=%q, want the create-time %q", res.Stdout, "first")
		}
	})

	// The rest of a spec that is fixed at create — the image among it — is not
	// silently kept the way Env is: re-provisioning the same session under a
	// changed image is refused with ErrSpecMismatch, and the refusal deletes
	// nothing — the original spec still adopts its sandbox (#29 docker,
	// #296 k8s). The refusal happens before anything is created or pulled, so
	// the mismatching image reference never needs to exist.
	t.Run("SpecMismatchRefusesAdoption", func(t *testing.T) {
		h := newHarness(t)
		ctx := context.Background()
		sid := domain.NewID("sesn")
		spec := sandbox.Spec{SessionID: sid, Image: h.Image, Workdir: workdir, Networking: unrestricted}
		sb1, err := h.Provider.Provision(ctx, spec)
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		t.Cleanup(func() {
			if err := sb1.Destroy(context.Background()); err != nil {
				t.Errorf("destroy: %v", err)
			}
		})
		changed := spec
		changed.Image = spec.Image + "-mismatch"
		if _, err := h.Provider.Provision(ctx, changed); !errors.Is(err, sandbox.ErrSpecMismatch) {
			t.Fatalf("re-provision under a changed image: err = %v, want sandbox.ErrSpecMismatch", err)
		}
		sb2, err := h.Provider.Provision(ctx, spec)
		if err != nil {
			t.Fatalf("re-provision under the original spec after the refusal: %v", err)
		}
		if sb2.ID() != sb1.ID() {
			t.Errorf("the refusal broke adoption: id %q, want %q", sb2.ID(), sb1.ID())
		}
	})

	// A hung command must not hang the executor, and it must not poison the
	// sandbox: the next tool call still works. Nothing the sandbox does to
	// enforce the deadline may show up as output the command appears to have
	// written.
	t.Run("ExecTimeoutKillsAndSurvives", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		start := time.Now()
		res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: `sleep 300`, Timeout: time.Second})
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		// Elapsed goes in the message because it is what tells the failure modes
		// apart: a backend that mis-read a punctual kill returns at about the
		// deadline, one that gave up waiting returns a killGrace later (#95, #110).
		if !res.TimedOut {
			t.Errorf("result = %+v after %s, want TimedOut", res, time.Since(start))
		}
		if res.Stdout != "" || res.Stderr != "" {
			t.Errorf("the kill leaked into the tool result: stdout=%q stderr=%q", res.Stdout, res.Stderr)
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

	// Killing the command's shell is not killing the command: its children
	// keep running, and they hold the pipes the sandbox is reading.
	t.Run("ExecTimeoutKillsTheWholeProcessTree", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		// 987654 is just a distinctive duration to count for afterwards.
		start := time.Now()
		res, err := sb.Exec(ctx, sandbox.ExecRequest{
			Command: `sleep 987654 & wait`, Timeout: time.Second,
		})
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		if !res.TimedOut {
			t.Fatalf("result = %+v after %s, want TimedOut", res, time.Since(start))
		}
		if n := countProcesses(t, sb, "sleep 987654"); n != 0 {
			t.Errorf("%d descendant(s) of the killed command survived the deadline", n)
		}
	})

	// Whatever enforces the deadline must die with the command it guarded. A
	// long-running agent issues thousands of quick commands under a multi-minute
	// tool timeout; one leftover process each would fill the sandbox.
	t.Run("ExecLeavesNoDeadlineMachineryBehind", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		before := countProcesses(t, sb, "sleep ")
		const runs = 3
		for range runs {
			res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: `echo hi`, Timeout: time.Minute})
			if err != nil || res.ExitCode != 0 {
				t.Fatalf("exec: %+v, %v", res, err)
			}
			// Reaping the deadline's machinery must be as quiet as arming it.
			if res.Stderr != "" {
				t.Errorf("tearing down the deadline leaked %q into the tool result", res.Stderr)
			}
		}
		// The watchdog is self-cleaning but not instant: it learns the command
		// finished on its next `kill -0` poll, up to one poll interval (~1s)
		// after the command exits. Give the last run's watchdog well past that
		// to drain rather than racing the poll.
		var after int
		for range 30 {
			if after = countProcesses(t, sb, "sleep "); after <= before {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Errorf("%d process(es) outlived %d commands that exited at once", after-before, runs)
	})

	// A command is timed by its own life, not by the life of what it leaves
	// behind. `server & echo started` returns at once; the backgrounded process
	// inherits the command's stdout and holds it open. A backend that times the
	// output stream instead of the command charges that straggler's lifetime to
	// a command that already exited cleanly, and calls it a timeout.
	t.Run("ExecTimesTheCommandNotItsStragglers", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		start := time.Now()
		res, err := sb.Exec(ctx, sandbox.ExecRequest{
			Command: `sleep 987123 & echo started`, Timeout: time.Second,
		})
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		if res.TimedOut {
			t.Errorf("a command that exited at once was reported as timed out: %+v", res)
		}
		if res.ExitCode != 0 {
			t.Errorf("exit = %d, want the command's own 0: %+v", res.ExitCode, res)
		}
		if !strings.Contains(res.Stdout, "started") {
			t.Errorf("stdout = %q, want the command's output", res.Stdout)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("a command that exited at once took %s to report", elapsed)
		}
	})

	// The crown-jewel invariant, stated so every backend is bound to it however
	// it enforces the deadline: a command cannot both outlive its deadline and
	// be reported as finished on time. It may leave an orphan behind — that is
	// the container's to reap — but it may not hide that it ran long.
	//
	// Each case attacks the deadline the way a past backend was actually
	// defeated, then holds a uniquely named marker alive far past the deadline.
	t.Run("ExecCannotOutliveItsDeadlineUnreported", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		// Kill every process guarding the deadline that the command can see: a
		// watchdog a backend runs beside the command (a child of its parent) or
		// below it (a child of the command). Killed individually, not by group,
		// so the command does not take itself down. Once they are gone, only an
		// enforcement point outside the sandbox can still catch the overrun.
		const killWatchdog = `
		  for parent in $$ $PPID; do
		    for p in $(cat /proc/$parent/task/$parent/children 2>/dev/null); do
		      [ "$p" != "$$" ] && kill -9 "$p" 2>/dev/null
		    done
		  done
		`
		// ...and then kill the parent itself: a backend that decides the timeout
		// by watching a wrapper the command runs under reports it finished while
		// it runs on. (Where the command is itself the watched process, $PPID is
		// outside the sandbox's namespace and this signals the command's own
		// group — it simply dies, still no hidden overrun.)
		const killWrapper = killWatchdog + "kill -9 \"$PPID\" 2>/dev/null\n"

		for _, tc := range []struct {
			name, sabotage, marker string
			mustTimeout            bool
		}{
			{"killing its watchdog", killWatchdog, "sleep 987321", true},
			{"killing the process a prober would watch", killWrapper, "sleep 987322", false},
			// The mirror of the forever-markers: overrun the deadline and then
			// exit clean, leaving nothing behind to count. The marker invariant
			// cannot see this — the process is gone — so the backend must call it
			// a timeout from the overrun alone. This is the case a backend that
			// judges the deadline only while the command is still visible would
			// miss, and it exercises the command-exits path rather than the
			// give-up path the forever-markers take.
			{"overrunning its deadline then exiting clean", killWatchdog, "sleep 2", true},
			// Muting its own output does not hide an overrun. Closing the exec's
			// stdout and stderr EOFs the output stream while the process runs on,
			// so a backend that reads the stream closing as the command finishing
			// would cancel its outside-the-sandbox deadline check and report the
			// overrun as a clean finish. The process is still there to be counted.
			{"muting its output then overrunning", killWatchdog + "exec 1>&- 2>&-\n", "sleep 987323", true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				start := time.Now()
				res, err := sb.Exec(ctx, sandbox.ExecRequest{
					Command: tc.sabotage + tc.marker, Timeout: time.Second,
				})
				if err != nil {
					t.Fatalf("exec: %v", err)
				}
				if elapsed := time.Since(start); elapsed > 20*time.Second {
					t.Fatalf("the call outran its 1s deadline by %s", elapsed)
				}
				// The invariant, and it is one-sided: a timeout may leave the
				// marker running (an orphan the container reaps), but a live
				// marker with no timeout is a command that hid its overrun.
				if !res.TimedOut && countProcesses(t, sb, tc.marker) > 0 {
					t.Errorf("a command outran its deadline but was reported finished: %+v", res)
				}
				// Where the command provably survived its own sabotage to run
				// long, the honest answer is a timeout and the backend must give
				// it — a stronger check than the invariant, for the case that
				// admits one.
				if tc.mustTimeout && !res.TimedOut {
					t.Errorf("a command that disarmed its watchdog and ran long was not a timeout: %+v", res)
				}
			})
		}
	})

	// SIGKILL is how a deadline is enforced, so a command that dies of SIGKILL
	// on its own could be mistaken for one. It must not be: the kill has to
	// have arrived at the deadline, not before it. (And a command that finishes
	// early is reported early — a deadline is never enforced by waiting it out.)
	t.Run("ExecSelfInflictedKillIsNotATimeout", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		start := time.Now()
		res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
			Command: `kill -9 $$`, Timeout: 30 * time.Second,
		})
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		if res.TimedOut {
			t.Error("a command that killed itself well inside the deadline read as a timeout")
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Errorf("a command that exited at once took %s to report", elapsed)
		}
	})

	// 124 is the exit code GNU timeout(1) gives a command it killed, so a
	// backend might be tempted to read it as its own timeout signal. It must
	// not: a command that merely exits 124 well inside its deadline finished on
	// its own, and only the enforcement point — not the exit code — says timeout.
	t.Run("ExecFastExit124IsNotATimeout", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
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
			t.Error("a command that exited 124 well inside its deadline read as a timeout")
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

	// A payload spanning many stream buffers, which the small round-trip above
	// does not reach. A backend that ships file bytes over a stream can lose a
	// chunk of one and still see every call finish cleanly, so the write reports
	// success and the file is short — the failure mode behind issue #103. Size is
	// well under MaxFileBytes; the content is deterministic so a mismatch names
	// the first bad offset rather than dumping a megabyte.
	t.Run("FileRoundTripLargePayload", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		ctx := context.Background()
		// xorshift32, so the content does not repeat within the payload: a
		// periodic filler whose period divides the loss would match past the gap
		// and report the wrong first-difference offset.
		want := make([]byte, 1<<20)
		x := uint32(0x9E3779B9)
		for i := range want {
			x ^= x << 13
			x ^= x >> 17
			x ^= x << 5
			want[i] = byte(x)
		}
		path := workdir + "/large.bin"
		if err := sb.WriteFile(ctx, path, want); err != nil {
			t.Fatalf("write %d bytes: %v", len(want), err)
		}
		got, err := sb.ReadFile(ctx, path)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("read back %d bytes, want %d; first difference at %d",
				len(got), len(want), firstDiff(got, want))
		}
	})

	// WriteFileStream is the streaming counterpart to WriteFile: it must land
	// exactly the bytes src yields (creating parents, overwriting), across many
	// stream buffers, without buffering the whole payload. A file mount rides
	// this path so a 500 MB object never fully buffers in the executor.
	t.Run("FileStreamRoundTrip", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		ctx := context.Background()
		// A payload spanning many buffers, deterministic so a mismatch names the
		// first bad offset (the same xorshift filler the buffered case uses).
		want := make([]byte, 1<<20)
		x := uint32(0x1234567)
		for i := range want {
			x ^= x << 13
			x ^= x >> 17
			x ^= x << 5
			want[i] = byte(x)
		}
		path := workdir + "/streamed/deep/mount.bin"
		if err := sb.WriteFileStream(ctx, path, bytes.NewReader(want), int64(len(want))); err != nil {
			t.Fatalf("stream write %d bytes: %v", len(want), err)
		}
		got, err := sb.ReadFile(ctx, path)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("read back %d bytes, want %d; first difference at %d",
				len(got), len(want), firstDiff(got, want))
		}

		// A src yielding fewer bytes than the declared size is an error, never a
		// silently truncated file — the integrity guarantee the size argument buys.
		short := workdir + "/short.bin"
		if err := sb.WriteFileStream(ctx, short, strings.NewReader("hi"), 100); err == nil {
			t.Error("stream write with a short src returned nil, want an error")
		}
	})

	// ReadFileStream is the read-out counterpart: the deliverables harvest
	// moves files above the fixed ReadFile cap out of the sandbox, so the
	// ceiling is per-call. The payload is deliberately over MaxFileBytes —
	// the size this method exists for — and deterministic so a mismatch
	// names the first bad offset.
	t.Run("ReadFileStreamRoundTripAndCap", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		ctx := context.Background()
		want := make([]byte, sandbox.MaxFileBytes+1<<20)
		x := uint32(0xBADC0DE)
		for i := range want {
			x ^= x << 13
			x ^= x >> 17
			x ^= x << 5
			want[i] = byte(x)
		}
		path := workdir + "/outputs/report.bin"
		if err := sb.WriteFileStream(ctx, path, bytes.NewReader(want), int64(len(want))); err != nil {
			t.Fatalf("write %d bytes: %v", len(want), err)
		}
		// Over the fixed cap, ReadFile refuses — this is the read only the
		// streaming method can make.
		if _, err := sb.ReadFile(ctx, path); !errors.Is(err, sandbox.ErrFileTooLarge) {
			t.Fatalf("ReadFile above the cap = %v, want ErrFileTooLarge", err)
		}
		rc, size, err := sb.ReadFileStream(ctx, path, int64(len(want))+1)
		if err != nil {
			t.Fatalf("ReadFileStream: %v", err)
		}
		got, err := io.ReadAll(rc)
		if cerr := rc.Close(); cerr != nil {
			t.Errorf("close: %v", cerr)
		}
		if err != nil {
			t.Fatalf("read stream: %v", err)
		}
		if size != int64(len(want)) || !bytes.Equal(got, want) {
			t.Errorf("streamed %d bytes (size %d), want %d; first difference at %d",
				len(got), size, len(want), firstDiff(got, want))
		}

		// A file over the caller's ceiling is refused before its bytes travel.
		if _, _, err := sb.ReadFileStream(ctx, path, 1024); !errors.Is(err, sandbox.ErrFileTooLarge) {
			t.Errorf("over maxBytes = %v, want ErrFileTooLarge", err)
		}

		// An empty file streams zero bytes, not an error.
		empty := workdir + "/outputs/empty.txt"
		if err := sb.WriteFile(ctx, empty, nil); err != nil {
			t.Fatalf("write empty: %v", err)
		}
		rc, size, err = sb.ReadFileStream(ctx, empty, 1024)
		if err != nil {
			t.Fatalf("stream empty: %v", err)
		}
		if got, err := io.ReadAll(rc); err != nil || size != 0 || len(got) != 0 {
			t.Errorf("empty stream = %d bytes (size %d), err %v", len(got), size, err)
		}
		_ = rc.Close()

		// The same path sentinels as ReadFile: a missing file, a directory,
		// and a symlink (never followed — its target's size is not the
		// link's, so following would defeat the ceiling).
		if _, _, err := sb.ReadFileStream(ctx, workdir+"/outputs/nope", 1024); !errors.Is(err, sandbox.ErrFileNotExist) {
			t.Errorf("missing file = %v, want ErrFileNotExist", err)
		}
		if _, _, err := sb.ReadFileStream(ctx, workdir+"/outputs", 1024); !errors.Is(err, sandbox.ErrIsDirectory) {
			t.Errorf("directory = %v, want ErrIsDirectory", err)
		}
		if res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: "ln -s report.bin " + workdir + "/outputs/link.bin"}); err != nil || res.ExitCode != 0 {
			t.Fatalf("ln: %+v %v", res, err)
		}
		if _, _, err := sb.ReadFileStream(ctx, workdir+"/outputs/link.bin", 1<<30); !errors.Is(err, sandbox.ErrNotRegularFile) {
			t.Errorf("symlink = %v, want ErrNotRegularFile", err)
		}
	})

	// A write is atomic: the bytes land under a temporary name in the target's own
	// directory and are renamed into place, so a transfer that fails part way
	// through leaves the target as it was. Without that, the failed write's own
	// residue is what the next read returns — and `edit` writes back what it read,
	// so a truncation handed to the model is a truncation committed to disk
	// (issue #71). A short src is the reachable way to fail a transfer mid-flight.
	t.Run("WriteThatFailsLeavesTheTargetAlone", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		ctx := context.Background()
		kept := workdir + "/kept.txt"
		if err := sb.WriteFile(ctx, kept, []byte("original")); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := sb.WriteFileStream(ctx, kept, strings.NewReader("hi"), 100); err == nil {
			t.Error("stream write with a short src returned nil, want an error")
		}
		if got, err := sb.ReadFile(ctx, kept); err != nil || string(got) != "original" {
			t.Errorf("after a failed write the target holds %q, %v; want %q untouched", got, err, "original")
		}

		// And a failed write of a file that did not exist creates none: a
		// half-written file is not a file the model should find.
		fresh := workdir + "/never.txt"
		if err := sb.WriteFileStream(ctx, fresh, strings.NewReader("hi"), 100); err == nil {
			t.Error("stream write with a short src returned nil, want an error")
		}
		if _, err := sb.ReadFile(ctx, fresh); !errors.Is(err, sandbox.ErrFileNotExist) {
			t.Errorf("after a failed write of a new file: err = %v, want ErrFileNotExist", err)
		}

		// Nor does the residue linger beside the target: a 500 MB mount that dies
		// part way through must not leave 400 of them in the sandbox.
		res, err := sb.Exec(ctx, sandbox.ExecRequest{
			Command: "ls -A " + workdir + " | grep -c '^" + sandbox.TempPrefix + "' || true",
		})
		if err != nil {
			t.Fatalf("list the workdir: %v", err)
		}
		if got := strings.TrimSpace(res.Stdout); got != "0" {
			t.Errorf("%s files left in the workdir = %s, want 0", sandbox.TempPrefix, got)
		}
	})

	// A path blocked by something that is not a directory is the model naming a
	// path that cannot exist, not the sandbox failing: every backend says so with
	// ErrNotDirectory, so the tool hands the model an error it can act on instead
	// of faulting the executor into abandoning the work item and retrying the same
	// doomed call (issue #71). The block is tested at two distances, because the
	// backends' underlying failures differ between them.
	t.Run("WriteUnderNonDirectory", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		ctx := context.Background()
		plain := workdir + "/plain.txt"
		if err := sb.WriteFile(ctx, plain, []byte("i am a file")); err != nil {
			t.Fatalf("stage a regular file: %v", err)
		}
		for _, path := range []string{plain + "/child", plain + "/deeper/child"} {
			if err := sb.WriteFile(ctx, path, []byte("x")); !errors.Is(err, sandbox.ErrNotDirectory) {
				t.Errorf("write %s: err = %v, want ErrNotDirectory", path, err)
			}
			if err := sb.WriteFileStream(ctx, path, strings.NewReader("x"), 1); !errors.Is(err, sandbox.ErrNotDirectory) {
				t.Errorf("stream write %s: err = %v, want ErrNotDirectory", path, err)
			}
			// A read of such a path is the same mistake, answered the same way —
			// `edit` reads before it writes, so both halves must be recoverable.
			if _, err := sb.ReadFile(ctx, path); !errors.Is(err, sandbox.ErrNotDirectory) {
				t.Errorf("read %s: err = %v, want ErrNotDirectory", path, err)
			}
		}
		// Streamed and large: the same classification for a quarter-megabyte
		// body, which on k8s means the failed `mkdir` must drain what it cannot
		// land — an exit that leaves this much unread deadlocks the exec stream
		// instead of answering (#304). Hangs rather than passes on regression.
		big := bytes.Repeat([]byte("y"), 256<<10)
		if err := sb.WriteFileStream(ctx, plain+"/child", bytes.NewReader(big), int64(len(big))); !errors.Is(err, sandbox.ErrNotDirectory) {
			t.Errorf("large stream write under a non-directory: err = %v, want ErrNotDirectory", err)
		}
		if got, err := sb.ReadFile(ctx, plain); err != nil || string(got) != "i am a file" {
			t.Errorf("the file in the way holds %q, %v; want it untouched", got, err)
		}
	})

	// A write *onto* a directory fails, and leaves the directory whole. The docker
	// daemon's archive extraction does the opposite when left to itself: it deletes
	// the directory, puts the file in its place, and reports success — everything
	// the directory held, gone on a write the model thought it was making to one
	// file (issue #71).
	t.Run("WriteOntoDirectory", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		ctx := context.Background()
		dir := workdir + "/adir"
		if err := sb.WriteFile(ctx, dir+"/inside.txt", []byte("kept")); err != nil {
			t.Fatalf("stage a directory with a file in it: %v", err)
		}
		if err := sb.WriteFile(ctx, dir, []byte("clobber")); !errors.Is(err, sandbox.ErrIsDirectory) {
			t.Errorf("write onto a directory: err = %v, want ErrIsDirectory", err)
		}
		if err := sb.WriteFileStream(ctx, dir, strings.NewReader("clobber"), 7); !errors.Is(err, sandbox.ErrIsDirectory) {
			t.Errorf("stream write onto a directory: err = %v, want ErrIsDirectory", err)
		}
		if got, err := sb.ReadFile(ctx, dir+"/inside.txt"); err != nil || string(got) != "kept" {
			t.Errorf("the file inside the directory holds %q, %v; want it untouched", got, err)
		}
	})

	// A file bind-mounted into the sandbox cannot be renamed onto — rename(2)
	// refuses a mount point with EBUSY — so the atomic write genuinely cannot be
	// made there. Every sandbox this suite runs has such a file at /etc/hosts
	// (the docker daemon and the kubelet both manage it), which makes it the one
	// target both backends can be asked about identically. The answer must be
	// ErrNotReplaceable, a tool result the model can act on with bash redirection
	// — not an unclassified fault the executor retries until the lease runs out
	// (issue #205).
	t.Run("WriteOntoBindMountedTarget", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		ctx := context.Background()
		before, err := sb.ReadFile(ctx, "/etc/hosts")
		if err != nil {
			t.Fatalf("read the bind-mounted target: %v", err)
		}
		if err := sb.WriteFile(ctx, "/etc/hosts", []byte("clobber\n")); !errors.Is(err, sandbox.ErrNotReplaceable) {
			t.Errorf("write onto a bind-mounted file: err = %v, want ErrNotReplaceable", err)
		}
		if err := sb.WriteFileStream(ctx, "/etc/hosts", strings.NewReader("clobber\n"), 8); !errors.Is(err, sandbox.ErrNotReplaceable) {
			t.Errorf("stream write onto a bind-mounted file: err = %v, want ErrNotReplaceable", err)
		}
		if got, err := sb.ReadFile(ctx, "/etc/hosts"); err != nil || string(got) != string(before) {
			t.Errorf("the bind-mounted target changed (err %v); want it holding what it held", err)
		}
		res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: "ls /etc/" + sandbox.TempPrefix + "* 2>/dev/null | wc -l"})
		if err != nil {
			t.Fatalf("count write residue: %v", err)
		}
		if got := strings.TrimSpace(res.Stdout); got != "0" {
			t.Errorf("%s files left beside the refused target = %s, want 0", sandbox.TempPrefix, got)
		}
	})

	// A device node is the other target a rename cannot honestly serve, and the
	// one where the two backends used to part ways: k8s's `mv` succeeded and
	// *supplanted* the node — /dev/null quietly stopped being a sink — while
	// docker failed unclassified. Both must refuse with ErrNotReplaceable and
	// leave the node what it was (issue #205).
	t.Run("WriteOntoDeviceNode", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		ctx := context.Background()
		if err := sb.WriteFile(ctx, "/dev/null", []byte("x")); !errors.Is(err, sandbox.ErrNotReplaceable) {
			t.Errorf("write onto a device node: err = %v, want ErrNotReplaceable", err)
		}
		if err := sb.WriteFileStream(ctx, "/dev/null", strings.NewReader("x"), 1); !errors.Is(err, sandbox.ErrNotReplaceable) {
			t.Errorf("stream write onto a device node: err = %v, want ErrNotReplaceable", err)
		}
		// A quarter-megabyte body, because the refusal must consume what it
		// refuses: the k8s probe fires before anything reads stdin, and an exit
		// that leaves stdin unread deadlocks the exec stream's flow control once
		// the body outgrows its in-flight window — this cell hangs forever where
		// the one-byte ones above still return (#303).
		if err := sb.WriteFile(ctx, "/dev/null", bytes.Repeat([]byte("y"), 256<<10)); !errors.Is(err, sandbox.ErrNotReplaceable) {
			t.Errorf("large write onto a device node: err = %v, want ErrNotReplaceable", err)
		}
		res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: "[ -c /dev/null ] && echo char || echo supplanted"})
		if err != nil {
			t.Fatalf("ask what /dev/null is now: %v", err)
		}
		if got := strings.TrimSpace(res.Stdout); got != "char" {
			t.Errorf("/dev/null is %q after the refused writes, want the character device intact", got)
		}
		// This sees what the sandbox can see: the k8s tee'd temporary, which the
		// refusal must shed. Docker's daemon-side copy lands on the overlay under
		// the tmpfs at /dev, invisible to any in-container observer either way.
		res, err = sb.Exec(ctx, sandbox.ExecRequest{Command: "ls /dev/" + sandbox.TempPrefix + "* 2>/dev/null | wc -l"})
		if err != nil {
			t.Fatalf("count write residue: %v", err)
		}
		if got := strings.TrimSpace(res.Stdout); got != "0" {
			t.Errorf("%s files left beside the refused target = %s, want 0", sandbox.TempPrefix, got)
		}
	})

	// The classification must not depend on the target's parent being writable.
	// Under a read-only root the write fails before the rename script can ask
	// anything — docker's daemon refuses the archive PUT outright, k8s's tee
	// cannot create the temporary — and both used to surface the raw refusal:
	// the unclassified fault the executor retries until the lease runs out. So
	// docker asks the probe in its own exec when the PUT is refused, and k8s
	// asks it before creating the temporary file (#303).
	t.Run("WriteOntoUnreplaceableTargetUnderReadOnlyRoot", func(t *testing.T) {
		sb := provisionHardened(t, sandbox.Hardening{ReadOnlyRootfs: true})
		ctx := context.Background()
		if err := sb.WriteFile(ctx, "/etc/hosts", []byte("clobber\n")); !errors.Is(err, sandbox.ErrNotReplaceable) {
			t.Errorf("write onto a bind-mounted file under a read-only root: err = %v, want ErrNotReplaceable", err)
		}
		// Streamed and large: the WriteOntoDeviceNode row pins the drain on a
		// writable root, this one pins it where the refusal actually fires early.
		big := bytes.Repeat([]byte("y"), 256<<10)
		if err := sb.WriteFileStream(ctx, "/etc/hosts", bytes.NewReader(big), int64(len(big))); !errors.Is(err, sandbox.ErrNotReplaceable) {
			t.Errorf("stream write onto a bind-mounted file under a read-only root: err = %v, want ErrNotReplaceable", err)
		}
		if err := sb.WriteFile(ctx, "/dev/null", []byte("x")); !errors.Is(err, sandbox.ErrNotReplaceable) {
			t.Errorf("write onto a device node under a read-only root: err = %v, want ErrNotReplaceable", err)
		}
		res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: "[ -c /dev/null ] && echo char || echo supplanted"})
		if err != nil {
			t.Fatalf("ask what /dev/null is now: %v", err)
		}
		if got := strings.TrimSpace(res.Stdout); got != "char" {
			t.Errorf("/dev/null is %q after the refused writes, want the character device intact", got)
		}
	})

	// The other route to the same unreachable classification: a uid that cannot
	// create the temporary file next to the target, because /etc and /dev are
	// root-owned. k8s used to surface the raw creation failure where docker's
	// daemon-side temporary (extracted as root) still classified; asking the
	// probe before the temporary keeps the backends identical (#303). RunAsUser
	// travels with ReadOnlyRootfs for the reason HardeningRunsAsTheConfiguredUser
	// states — the entrypoint's `mkdir -p <workdir>` runs as this uid — and that
	// pairing is also this row's honest bound: with the read-only root along,
	// the refusal fires for both reasons at once, so the row cannot isolate the
	// uid mechanism. What it pins is the shipped posture — the probe, its
	// mountinfo read, and docker's classify-on-refusal exec running end-to-end
	// as uid 65534, where a probe that ever came to need root would fail here
	// and nowhere else.
	t.Run("WriteOntoUnreplaceableTargetAsNonRoot", func(t *testing.T) {
		uid := int64(65534)
		sb := provisionHardened(t, sandbox.Hardening{RunAsUser: &uid, ReadOnlyRootfs: true})
		ctx := context.Background()
		if err := sb.WriteFile(ctx, "/etc/hosts", []byte("clobber\n")); !errors.Is(err, sandbox.ErrNotReplaceable) {
			t.Errorf("write onto a bind-mounted file as non-root: err = %v, want ErrNotReplaceable", err)
		}
		if err := sb.WriteFile(ctx, "/dev/null", []byte("x")); !errors.Is(err, sandbox.ErrNotReplaceable) {
			t.Errorf("write onto a device node as non-root: err = %v, want ErrNotReplaceable", err)
		}
	})

	// What the rename does to a symlink, which is the one non-regular target both
	// backends can reach identically: it replaces the *name*, so the link is
	// supplanted by a regular file and whatever it pointed at is left alone —
	// never written through. A link to a directory is a directory here, as it is
	// to every other question the backends ask, and is refused like one. Pinned
	// because it is a behavior change (docker's extraction removed the link,
	// k8s's `tee` wrote through it) and because the deleted `/dev/null` row used
	// to be what said anything at all about non-regular targets.
	t.Run("WriteOntoSymlink", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		ctx := context.Background()
		if err := sb.WriteFile(ctx, workdir+"/pointee.txt", []byte("pointee")); err != nil {
			t.Fatalf("stage the link's target: %v", err)
		}
		res, err := sb.Exec(ctx, sandbox.ExecRequest{
			Command: "mkdir -p adir && ln -s pointee.txt tolink && ln -s adir todir && chmod 0600 pointee.txt",
		})
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("stage the symlinks: %+v, %v", res, err)
		}

		if err := sb.WriteFile(ctx, workdir+"/tolink", []byte("through")); err != nil {
			t.Fatalf("write onto a symlink to a file: %v", err)
		}
		if got, err := sb.ReadFile(ctx, workdir+"/tolink"); err != nil || string(got) != "through" {
			t.Errorf("the symlink's name holds %q, %v; want the bytes just written", got, err)
		}
		if got, err := sb.ReadFile(ctx, workdir+"/pointee.txt"); err != nil || string(got) != "pointee" {
			t.Errorf("what the link pointed at holds %q, %v; want it untouched", got, err)
		}
		if res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: "test -L tolink && echo still-a-link"}); err != nil {
			t.Fatalf("inspect the replaced name: %v", err)
		} else if strings.TrimSpace(res.Stdout) != "" {
			t.Errorf("the name is still a symlink; want a regular file in its place")
		}
		// The mode says the same thing from the other side, and it is the half that
		// would fail silently: a link carries no mode worth having, so the file
		// replacing it is a fresh one. `stat -c %a` on a symlink is an lstat, so a
		// preservation that did not skip links would land the *link's* own 0777 here
		// — world-writable, on an agent-controlled filesystem (#204).
		if got := fileMode(t, sb, workdir+"/tolink"); got != "644" {
			t.Errorf("the file replacing the symlink has mode %s, want 644", got)
		}
		if got := fileMode(t, sb, workdir+"/pointee.txt"); got != "600" {
			t.Errorf("what the link pointed at has mode %s, want 600 — untouched", got)
		}

		if err := sb.WriteFile(ctx, workdir+"/todir", []byte("x")); !errors.Is(err, sandbox.ErrIsDirectory) {
			t.Errorf("write onto a symlink to a directory: err = %v, want ErrIsDirectory", err)
		}
	})

	// The other thing a rename replaces if nothing stops it: the target's mode.
	// The file that lands carries the *temporary* file's bits, so the workflow
	// that breaks is an ordinary one — `write` a script, `chmod +x` it in bash,
	// `edit` it, and it no longer runs (issue #204). Both backends put the
	// target's mode back on the temporary file before the move, or this row is
	// papering over a divergence rather than pinning a contract.
	t.Run("WriteKeepsTheTargetsMode", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		ctx := context.Background()
		script := workdir + "/run.sh"
		if err := sb.WriteFile(ctx, script, []byte("#!/bin/sh\necho first\n")); err != nil {
			t.Fatalf("stage the script: %v", err)
		}
		// A target that did not exist has no mode to carry over, and every backend
		// lands the same one — the convergence the preservation is built on. It is
		// 0644 by two different routes, and the platform fixes both: the docker
		// backend's tar header says so, and the k8s write script creates the file
		// itself and chmods it to 0644 before the bytes stream, under a `umask 022`
		// it sets for what the chmod cannot reach. Until #212 the k8s half of that
		// came from the image instead, so this row held there by the coincidence of
		// the suite's image running a 022 umask; the umask alone then still lost to
		// a parent directory's default POSIX ACL, which the chmod answers (#213).
		// Neither is visible from here — this image runs 022 and carries no ACLs —
		// so what this row pins is the convergence, and the k8s script's own suite
		// pins the mechanism.
		if got := fileMode(t, sb, script); got != "644" {
			t.Errorf("a file that did not exist lands mode %s, want 644", got)
		}

		if res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: "chmod 0755 " + script}); err != nil || res.ExitCode != 0 {
			t.Fatalf("chmod the script executable: %+v, %v", res, err)
		}
		if err := sb.WriteFile(ctx, script, []byte("#!/bin/sh\necho edited\n")); err != nil {
			t.Fatalf("rewrite the script: %v", err)
		}
		if got := fileMode(t, sb, script); got != "755" {
			t.Errorf("after a rewrite the mode is %s, want 755", got)
		}
		// Which is the whole point of the bits: the thing still runs.
		if res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: script}); err != nil ||
			res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "edited" {
			t.Errorf("run the rewritten script: %+v, %v; want it executable and printing %q", res, err, "edited")
		}

		// The streaming write shares the rename, so it must share the answer.
		streamed := "#!/bin/sh\necho streamed\n"
		if err := sb.WriteFileStream(ctx, script, strings.NewReader(streamed), int64(len(streamed))); err != nil {
			t.Fatalf("stream-write the script: %v", err)
		}
		if got := fileMode(t, sb, script); got != "755" {
			t.Errorf("after a streamed rewrite the mode is %s, want 755", got)
		}
	})

	// A bulk write lands a whole set of files for a fixed couple of execs, and
	// every member must land exactly as WriteFile lands one: parents created, bytes verbatim,
	// overwrites truncating. The shape is a skill being materialized — many small
	// files across nested directories — with one large member, because the batch
	// travels as a single archive and a member spanning many stream buffers is
	// where a truncated one would hide (#206).
	t.Run("BulkWriteRoundTrip", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		ctx := context.Background()

		// An empty batch is a no-op, not an error: a caller with nothing to write
		// must not have to check first.
		if err := sb.WriteFiles(ctx, nil); err != nil {
			t.Errorf("empty batch: err = %v, want nil", err)
		}

		root := workdir + "/skills/pack"
		large := make([]byte, 1<<20)
		x := uint32(0xC0FFEE11)
		for i := range large {
			x ^= x << 13
			x ^= x >> 17
			x ^= x << 5
			large[i] = byte(x)
		}
		batch := []sandbox.FileWrite{
			// Bytes no shell round-trip would survive, as the single write's row uses.
			{Path: root + "/SKILL.md", Data: []byte{0x00, 0x01, 0xff, 0xfe, 'h', 'i', 0x00}},
			{Path: root + "/scripts/run.sh", Data: []byte("#!/bin/sh\necho hi\n")},
			{Path: root + "/deep/nested/dir/blob.bin", Data: large},
			{Path: root + "/empty", Data: nil},
		}
		for i := 0; i < 20; i++ {
			batch = append(batch, sandbox.FileWrite{
				Path: root + "/many/f" + strconv.Itoa(i),
				Data: []byte("member " + strconv.Itoa(i)),
			})
		}
		if err := sb.WriteFiles(ctx, batch); err != nil {
			t.Fatalf("bulk write %d files: %v", len(batch), err)
		}
		for _, f := range batch {
			got, err := sb.ReadFile(ctx, f.Path)
			if err != nil {
				t.Errorf("read %s: %v", f.Path, err)
				continue
			}
			if !bytes.Equal(got, f.Data) {
				t.Errorf("%s read back %d bytes, want %d; first difference at %d",
					f.Path, len(got), len(f.Data), firstDiff(got, f.Data))
			}
		}

		// A file the batch creates lands 0644 and a directory it creates 0755 —
		// the same answer `mkdir -p` and the single write's own tar header give,
		// so a tree written in bulk is indistinguishable from one written a file
		// at a time. Both backends extract the same archive; only the untar
		// differs, and this is where the two would drift.
		if got := fileMode(t, sb, root+"/SKILL.md"); got != "644" {
			t.Errorf("a file the batch created has mode %s, want 644", got)
		}
		if got := fileMode(t, sb, root+"/deep/nested"); got != "755" {
			t.Errorf("a directory the batch created has mode %s, want 755", got)
		}

		// Overwriting truncates rather than merging, and a second batch over the
		// first is the ordinary re-materialization case.
		if err := sb.WriteFiles(ctx, []sandbox.FileWrite{
			{Path: root + "/SKILL.md", Data: []byte("x")},
			{Path: root + "/scripts/run.sh", Data: []byte("#!/bin/sh\necho rewritten\n")},
		}); err != nil {
			t.Fatalf("second batch: %v", err)
		}
		if got, err := sb.ReadFile(ctx, root+"/SKILL.md"); err != nil || string(got) != "x" {
			t.Errorf("after overwrite = %q, %v; want %q", got, err, "x")
		}

		// Nothing of the machinery is left behind: neither the temporary files nor
		// the manifest the batch travelled with.
		assertNoWriteResidue(t, sb, workdir, root, root+"/many")
	})

	// The batch is not a transaction, and this row pins what it is instead: the
	// first failure stops the run, what already landed stays, and the rest is
	// never written. A target that is a directory is the reachable way to fail one
	// member — the same fault WriteOntoDirectory pins for a single write — and the
	// error must name *which* member, since the caller handed over a set.
	t.Run("BulkWriteStopsAtTheFirstFailure", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		ctx := context.Background()
		dir := workdir + "/batch/adir"
		if err := sb.WriteFile(ctx, dir+"/inside.txt", []byte("kept")); err != nil {
			t.Fatalf("stage a directory with a file in it: %v", err)
		}
		before := workdir + "/batch/before.txt"
		after := workdir + "/batch/after.txt"
		err := sb.WriteFiles(ctx, []sandbox.FileWrite{
			{Path: before, Data: []byte("landed")},
			{Path: dir, Data: []byte("clobber")},
			{Path: after, Data: []byte("never")},
		})
		if !errors.Is(err, sandbox.ErrIsDirectory) {
			t.Errorf("bulk write onto a directory: err = %v, want ErrIsDirectory", err)
		}
		if err != nil && !strings.Contains(err.Error(), dir) {
			t.Errorf("err = %v, want it to name the member it stopped on (%s)", err, dir)
		}
		if got, err := sb.ReadFile(ctx, dir+"/inside.txt"); err != nil || string(got) != "kept" {
			t.Errorf("the file inside the directory holds %q, %v; want it untouched", got, err)
		}
		if got, err := sb.ReadFile(ctx, before); err != nil || string(got) != "landed" {
			t.Errorf("the member before the failure holds %q, %v; want %q", got, err, "landed")
		}
		if _, err := sb.ReadFile(ctx, after); !errors.Is(err, sandbox.ErrFileNotExist) {
			t.Errorf("the member after the failure: err = %v, want ErrFileNotExist", err)
		}
		assertNoWriteResidue(t, sb, workdir, workdir+"/batch", dir)
	})

	// A batch whose parent path is blocked by a non-directory is the same mistake
	// a single write makes there, and gets the same sentinel — so the caller can
	// tell a path it named wrongly from the sandbox breaking. Nothing of the batch
	// lands, and the file in the way is untouched.
	t.Run("BulkWriteUnderNonDirectory", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		ctx := context.Background()
		plain := workdir + "/blocker.txt"
		if err := sb.WriteFile(ctx, plain, []byte("i am a file")); err != nil {
			t.Fatalf("stage a regular file: %v", err)
		}
		err := sb.WriteFiles(ctx, []sandbox.FileWrite{
			{Path: workdir + "/fine.txt", Data: []byte("x")},
			{Path: plain + "/deeper/child", Data: []byte("x")},
		})
		if !errors.Is(err, sandbox.ErrNotDirectory) {
			t.Errorf("bulk write under a non-directory: err = %v, want ErrNotDirectory", err)
		}
		if got, err := sb.ReadFile(ctx, plain); err != nil || string(got) != "i am a file" {
			t.Errorf("the file in the way holds %q, %v; want it untouched", got, err)
		}
		// The member the batch could have written lands nowhere: the delivery is
		// refused as a whole, and what it had staged is shed with it.
		if _, err := sb.ReadFile(ctx, workdir+"/fine.txt"); !errors.Is(err, sandbox.ErrFileNotExist) {
			t.Errorf("the batch's other member: err = %v, want ErrFileNotExist", err)
		}
		assertNoWriteResidue(t, sb, workdir)
	})

	// A batch renames its members into place like every other write, so it must
	// put the target's mode back the same way: `write` a script, `chmod +x` it,
	// re-materialize the skill, and it still runs (#204).
	t.Run("BulkWriteKeepsTheTargetsMode", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		ctx := context.Background()
		script := workdir + "/bulk/run.sh"
		if err := sb.WriteFiles(ctx, []sandbox.FileWrite{
			{Path: script, Data: []byte("#!/bin/sh\necho first\n")},
		}); err != nil {
			t.Fatalf("stage the script: %v", err)
		}
		if res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: "chmod 0755 " + script}); err != nil || res.ExitCode != 0 {
			t.Fatalf("chmod the script executable: %+v, %v", res, err)
		}
		if err := sb.WriteFiles(ctx, []sandbox.FileWrite{
			{Path: script, Data: []byte("#!/bin/sh\necho edited\n")},
			{Path: workdir + "/bulk/other.txt", Data: []byte("x")},
		}); err != nil {
			t.Fatalf("rewrite the script in a batch: %v", err)
		}
		if got := fileMode(t, sb, script); got != "755" {
			t.Errorf("after a bulk rewrite the mode is %s, want 755", got)
		}
		if res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: script}); err != nil ||
			res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "edited" {
			t.Errorf("run the rewritten script: %+v, %v; want it executable and printing %q", res, err, "edited")
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

	// The other side of that boundary: the cap is a limit, not a forbidden value,
	// so a file of exactly MaxFileBytes must read back whole. It is where a
	// backend that frames or counts its own stream is likeliest to mis-account by
	// the length of that framing and fail the largest legal read (issue #105).
	t.Run("ReadFileAtTheCap", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		ctx := context.Background()
		res, err := sb.Exec(ctx, sandbox.ExecRequest{
			Command: fmt.Sprintf("head -c %d /dev/zero > cap.bin", sandbox.MaxFileBytes),
		})
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("stage a file at the cap: %+v, %v", res, err)
		}
		got, err := sb.ReadFile(ctx, workdir+"/cap.bin")
		if err != nil || len(got) != sandbox.MaxFileBytes {
			t.Fatalf("read = %d bytes, %v; want %d and no error", len(got), err, sandbox.MaxFileBytes)
		}
		// Content too, not just the count: mis-accounting by a framing length
		// shifts the bytes as readily as it shortens them.
		if want := make([]byte, sandbox.MaxFileBytes); !bytes.Equal(got, want) {
			t.Errorf("read back the right length but the wrong bytes; first difference at %d", firstDiff(got, want))
		}
	})

	t.Run("ReadFileDirectory", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		_, err := sb.ReadFile(context.Background(), workdir)
		if !errors.Is(err, sandbox.ErrIsDirectory) {
			t.Errorf("err = %v, want ErrIsDirectory", err)
		}
	})

	// A read of a non-regular file (a FIFO here) is the reader asking for a path
	// that is not a file, not the sandbox failing — every backend reports it as
	// ErrNotRegularFile so the toolset can hand the model a recoverable error.
	t.Run("ReadFileNonRegular", func(t *testing.T) {
		sb, _, _ := provision(t, unrestricted)
		ctx := context.Background()
		if res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: "mkfifo fifo"}); err != nil || res.ExitCode != 0 {
			t.Fatalf("mkfifo: %+v, %v", res, err)
		}
		if _, err := sb.ReadFile(ctx, workdir+"/fifo"); !errors.Is(err, sandbox.ErrNotRegularFile) {
			t.Errorf("err = %v, want ErrNotRegularFile", err)
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
		// A bulk write is two round trips where a single one is one, so it has two
		// places to notice the sandbox is gone; both must say so with the sentinel
		// the executor reads as a sandbox fault rather than as the path's fault.
		if err := sb.WriteFiles(ctx, []sandbox.FileWrite{
			{Path: workdir + "/anything", Data: []byte("x")},
		}); !errors.Is(err, sandbox.ErrNotFound) {
			t.Errorf("bulk write after destroy: %v, want ErrNotFound", err)
		}
	})

	// The reaper contract (plan 24): Owned answers "whose sandboxes does this
	// endpoint hold", Reap destroys a session's holdings without a live handle.
	// The endpoint is shared with parallel tests, so Owned rows assert
	// membership only, never the whole set.
	t.Run("OwnedListsProvisionedSession", func(t *testing.T) {
		_, h, sid := provision(t, unrestricted)
		owned, err := h.Provider.Owned(context.Background())
		if err != nil {
			t.Fatalf("owned: %v", err)
		}
		if !containsID(owned, sid) {
			t.Errorf("owned does not list the provisioned session %s", sid)
		}
	})

	t.Run("ReapRemovesProvisionedSandbox", func(t *testing.T) {
		sb, h, sid := provision(t, unrestricted)
		ctx := context.Background()
		if err := h.Provider.Reap(ctx, sid); err != nil {
			t.Fatalf("reap: %v", err)
		}
		// The handle predates the reap; the sandbox behind it must be gone by
		// the time Reap returns — the reaper's caller uses that to reason
		// "reaped means no longer running", so a graceful, still-terminating
		// backend must wait, not just request deletion.
		if _, err := sb.Exec(ctx, sandbox.ExecRequest{Command: `echo hi`}); !errors.Is(err, sandbox.ErrNotFound) {
			t.Errorf("exec after reap: %v, want ErrNotFound", err)
		}
		owned, err := h.Provider.Owned(ctx)
		if err != nil {
			t.Fatalf("owned: %v", err)
		}
		if containsID(owned, sid) {
			t.Errorf("owned still lists %s after reap", sid)
		}
	})

	t.Run("ReapIsIdempotent", func(t *testing.T) {
		_, h, sid := provision(t, unrestricted)
		ctx := context.Background()
		if err := h.Provider.Reap(ctx, sid); err != nil {
			t.Fatalf("first reap: %v", err)
		}
		if err := h.Provider.Reap(ctx, sid); err != nil {
			t.Errorf("second reap: %v, want nil", err)
		}
	})

	t.Run("ReapUnknownSessionIsNoop", func(t *testing.T) {
		h := newHarness(t)
		if err := h.Provider.Reap(context.Background(), domain.NewID("sesn")); err != nil {
			t.Errorf("reap of a never-provisioned session: %v, want nil", err)
		}
	})

	// Without a gate (a backend not yet gate-wired, or a deployment not opted
	// in), `limited` networking is enforced as no egress at all: fail closed,
	// never silently unrestricted. The routing table is the honest probe — a
	// network namespace can carry down, unconfigured tunnel devices from the
	// host kernel and still reach nothing. The gated meaning of `limited`
	// ("only allowed_hosts, through the gate") is the gate rows' contract.
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

	// --- Spec.Hardening (#65) ---------------------------------------------
	//
	// Hardening is bound when the sandbox is created, so every row here
	// provisions its own. Each asserts what a tool inside the sandbox can see:
	// the kernel's own report of the applied cgroup limit, an operation the
	// dropped capability gates, a write outside the writable mounts, `id -u`.

	// A CPU quota is the containment for a process that escaped the deadline's
	// process-group kill — a `setsid` child outlives the kill and would
	// otherwise pin a core until the sandbox is destroyed. cpu.max is the
	// kernel's answer in "<quota> <period>" microseconds, so 500 millicores is
	// half of the 100ms period.
	t.Run("HardeningCapsCPU", func(t *testing.T) {
		sb := provisionHardened(t, sandbox.Hardening{CPUMillis: 500})
		if got := catInSandbox(t, sb, "/sys/fs/cgroup/cpu.max"); got != "50000 100000" {
			t.Errorf("cpu.max = %q, want %q for a 500-millicore limit", got, "50000 100000")
		}
	})

	// The pids limit is the other half of that containment, and the cap on the
	// process pressure a command needs to stall the daemon probe the deadline
	// labels an overrun with. Only a backend whose runtime can express a
	// per-sandbox limit registers the row: the Kubernetes Pod API carries no
	// per-pod pids limit (it is the kubelet's `podPidsLimit`), a divergence
	// recorded in docs/DIVERGENCES.md rather than faked with a passing row.
	if newHarness(t).EnforcesPidsLimit {
		t.Run("HardeningCapsProcesses", func(t *testing.T) {
			sb := provisionHardened(t, sandbox.Hardening{PidsLimit: 64})
			if got := catInSandbox(t, sb, "/sys/fs/cgroup/pids.max"); got != "64" {
				t.Errorf("pids.max = %q, want 64", got)
			}
		})
	}

	// A differential, because a one-sided assertion would pass for the wrong
	// reason — `chown` also fails for an image running as a user who does not
	// own the file, and would then "prove" a drop that never happened.
	t.Run("HardeningDropsCapabilities", func(t *testing.T) {
		probe := "touch " + workdir + "/owned && chown 1 " + workdir + "/owned"
		plain, _, _ := provision(t, unrestricted)
		if res, err := plain.Exec(context.Background(), sandbox.ExecRequest{Command: probe}); err != nil || res.ExitCode != 0 {
			t.Fatalf("chown without a drop = %+v, %v; the row cannot tell a drop from an unrelated failure", res, err)
		}
		dropped := provisionHardened(t, sandbox.Hardening{CapDrop: []string{"CHOWN"}})
		res, err := dropped.Exec(context.Background(), sandbox.ExecRequest{Command: probe})
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		if res.ExitCode == 0 {
			t.Error("chown succeeded with CAP_CHOWN dropped")
		}
	})

	// A read-only root is not a bare flag: the platform itself writes several
	// paths inside the sandbox — the workdir, /tmp, the persistent shell's state
	// root, the session-resource mount root — and a read-only root that reached
	// any of them would be a sandbox whose very first bash call faults instead
	// of answering. Every path sandbox.WritablePaths names is asserted, so a
	// future writer that forgets to add its path here fails the row rather than
	// the deployment.
	t.Run("HardeningReadOnlyRootFilesystem", func(t *testing.T) {
		sb := provisionHardened(t, sandbox.Hardening{ReadOnlyRootfs: true})
		ctx := context.Background()
		res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: "touch /etc/map-probe"})
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		if res.ExitCode == 0 {
			t.Error("a write to /etc succeeded on a read-only root filesystem")
		}
		for _, dir := range sandbox.WritablePaths(workdir) {
			res, err := sb.Exec(ctx, sandbox.ExecRequest{
				Command: "mkdir -p " + dir + "/probe.d && touch " + dir + "/probe.d/map-probe",
			})
			if err != nil || res.ExitCode != 0 {
				t.Errorf("write under %s = %+v, %v; a read-only root must still leave it writable", dir, res, err)
			}
		}
		// And the file primitives, not just exec: the workdir is what the write
		// tool lands bytes in.
		if err := sb.WriteFile(ctx, workdir+"/probe.txt", []byte("hi")); err != nil {
			t.Errorf("WriteFile on a read-only-rootfs sandbox: %v", err)
		}
		// A large streamed write to a *replaceable* path the read-only root
		// still blocks — no mount, no device, so the refusal is not #303's:
		// docker's daemon rejects the archive PUT and k8s cannot create the
		// temporary file, whose failure branch must drain the body it cannot
		// land or the exec stream deadlocks instead of answering (#304 — its
		// 64 MiB form deadlocked three of three before the drain). The refusal
		// is classified: the model's error, not the platform's fault, exactly
		// as the reference toolset answers every write failure (plan 23,
		// #306). Large first, so a drain that stops draining hangs here
		// rather than passes.
		big := bytes.Repeat([]byte("y"), 256<<10)
		if err := sb.WriteFileStream(ctx, "/etc/map-304-blocked", bytes.NewReader(big), int64(len(big))); !errors.Is(err, sandbox.ErrNotWritable) {
			t.Errorf("a quarter-megabyte streamed write onto the read-only root: err = %v, want ErrNotWritable", err)
		}
		if err := sb.WriteFile(ctx, "/etc/map-306-blocked", []byte("x")); !errors.Is(err, sandbox.ErrNotWritable) {
			t.Errorf("a buffered write onto the read-only root: err = %v, want ErrNotWritable", err)
		}
		// The other route to the same refusal: the parent itself cannot be
		// made — mkdir of a new top-level directory fails on the read-only
		// root before any temporary is attempted (docker classifies in
		// mkdirAll, k8s in the write script's mkdir branch).
		if err := sb.WriteFile(ctx, "/map-306-newtop/probe.txt", []byte("x")); !errors.Is(err, sandbox.ErrNotWritable) {
			t.Errorf("a write under an unmakeable parent on the read-only root: err = %v, want ErrNotWritable", err)
		}
	})

	// A uid the image did not choose. It travels with ReadOnlyRootfs because the
	// container's entrypoint runs `mkdir -p <workdir>` as that uid: the writable
	// mount a read-only root brings is what makes the workdir *exist* where the
	// uid could not have created it. Whether the uid can also *write* it is the
	// image's business on Docker (a fresh anonymous volume is root-owned) and the
	// kubelet's on Kubernetes (an emptyDir is world-writable), so this row
	// asserts only what holds on both — see Hardening.RunAsUser.
	t.Run("HardeningRunsAsTheConfiguredUser", func(t *testing.T) {
		uid := int64(65534)
		sb := provisionHardened(t, sandbox.Hardening{RunAsUser: &uid, ReadOnlyRootfs: true})
		res, err := sb.Exec(context.Background(), sandbox.ExecRequest{Command: "id -u"})
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		if got := strings.TrimSpace(res.Stdout); got != "65534" {
			t.Errorf("id -u = %q, want 65534 (the image's own user is root)", got)
		}
	})

	// The gated rows run only for a backend that declares gate support; the
	// suite never fabricates a GateSpec of its own.
	if newHarness(t).Gate != nil {
		gateRows(t, newHarness)
	}
}

// catInSandbox reads a small file through Exec and trims it. The hardening rows
// use it for the kernel's own cgroup reports, which is what a tool would see.
func containsID(ids []domain.ID, want domain.ID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func catInSandbox(t *testing.T, sb sandbox.Sandbox, path string) string {
	t.Helper()
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{Command: "cat " + path})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("cat %s = %+v, %v", path, res, err)
	}
	return strings.TrimSpace(res.Stdout)
}

// firstDiff returns the index of the first byte where a and b differ, or the
// shorter length when one is a prefix of the other. It keeps a large-payload
// mismatch readable: the offset says whether a stream lost its head or its tail.
func firstDiff(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// fileMode reads a path's permission bits from inside the sandbox, as the octal
// digits `stat -c %a` prints them. The image contract carries a `stat` accepting
// `-c` for the write path itself, so asking this way costs the suite nothing the
// backends do not already require.
func fileMode(t *testing.T, sb sandbox.Sandbox, path string) string {
	t.Helper()
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{Command: "stat -c %a " + path})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("stat %s: exit %d: %s", path, res.ExitCode, res.Stderr)
	}
	return strings.TrimSpace(res.Stdout)
}

// assertNoWriteResidue fails unless every named directory is free of the
// temporary files a write travels under — the members' own, and the manifest a
// bulk write's archive carries. Whatever a write's outcome, what it leaves in the
// sandbox is the file it was asked for and nothing else.
func assertNoWriteResidue(t *testing.T, sb sandbox.Sandbox, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
			Command: "ls -A " + dir + " 2>/dev/null | grep -c '^" + sandbox.TempPrefix + "' || true",
		})
		if err != nil {
			t.Fatalf("list %s: %v", dir, err)
		}
		if got := strings.TrimSpace(res.Stdout); got != "0" {
			t.Errorf("%s files left in %s = %s, want 0", sandbox.TempPrefix, dir, got)
		}
	}
}

// countProcesses counts live processes in the sandbox whose command line starts
// with prefix. It reads /proc directly: a minimal image has no ps.
func countProcesses(t *testing.T, sb sandbox.Sandbox, prefix string) int {
	t.Helper()
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{Command: `
		n=0
		for p in /proc/[0-9]*; do
		  [ -r "$p/cmdline" ] || continue
		  case "$(tr '\0' ' ' < "$p/cmdline")" in
		    "` + prefix + `"*) n=$((n+1)) ;;
		  esac
		done
		echo "$n"`})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("count processes: exit %d: %s", res.ExitCode, res.Stderr)
	}
	n, err := strconv.Atoi(strings.TrimSpace(res.Stdout))
	if err != nil {
		t.Fatalf("count processes: %q: %v", res.Stdout, err)
	}
	return n
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
