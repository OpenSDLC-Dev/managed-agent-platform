package shell_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/docker"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/shell"
)

const testImage = "debian:stable-slim"

// provision gives the whole test one real container; each subtest scopes its
// own checkpoint with a fresh session id, so they share the container but not
// shell state. A missing daemon is a hard failure, as with the other suites.
func provision(t *testing.T) sandbox.Sandbox {
	t.Helper()
	provider, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("shell tests require Docker: %v", err)
	}
	sb, err := provider.Provision(context.Background(), sandbox.Spec{
		SessionID:  domain.NewID("sesn"),
		Image:      testImage,
		Workdir:    "/workspace",
		Networking: domain.Networking{Type: domain.NetUnrestricted},
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Cleanup(func() {
		if err := sb.Destroy(context.Background()); err != nil {
			t.Errorf("destroy: %v", err)
		}
	})
	return sb
}

// newShell is a subtest's shell, scoped to a fresh session; each call gets a
// fresh tool id (the reclaim key). It also returns the session so a test can
// issue a restart on the same shell.
func newShell(t *testing.T, sb sandbox.Sandbox) (func(cmd string, timeout time.Duration) shell.Result, domain.ID) {
	t.Helper()
	session := domain.NewID("sesn")
	run := func(cmd string, timeout time.Duration) shell.Result {
		t.Helper()
		res, err := shell.Run(context.Background(), sb, session, domain.NewID("sevt"),
			shell.Request{Command: cmd, Timeout: timeout})
		if err != nil {
			t.Fatalf("shell.Run(%q): %v", cmd, err)
		}
		return res
	}
	return run, session
}

func TestShell(t *testing.T) {
	sb := provision(t)

	t.Run("StatePersistsAcrossCalls", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh("export X=1; cd /tmp; f() { echo hi; }", 0)
		got := sh(`echo "$X:$(pwd):$(f)"`, 0)
		if strings.TrimSpace(got.Stdout) != "1:/tmp:hi" {
			t.Errorf("stdout = %q, want 1:/tmp:hi (env/cwd/function did not persist)", got.Stdout)
		}
		if got.ExitCode != 0 {
			t.Errorf("exit = %d, want 0", got.ExitCode)
		}
	})

	t.Run("OptionsPersistAcrossCalls", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh("set -o pipefail; shopt -s nullglob", 0)
		got := sh(`[[ -o pipefail ]] && echo PIPEFAIL_ON; shopt -q nullglob && echo NULLGLOB_ON`, 0)
		if !strings.Contains(got.Stdout, "PIPEFAIL_ON") {
			t.Errorf("set -o option did not persist; stdout=%q", got.Stdout)
		}
		if !strings.Contains(got.Stdout, "NULLGLOB_ON") {
			t.Errorf("shopt option did not persist; stdout=%q", got.Stdout)
		}
	})

	t.Run("ExitCodeFidelityThroughTheTrap", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		for _, tc := range []struct {
			cmd  string
			want int
		}{
			{"true", 0},
			{"false", 1},
			{"(exit 7)", 7},
			{"exit 3", 3},
			{"bash -c 'exit 42'", 42},
		} {
			if got := sh(tc.cmd, 0); got.ExitCode != tc.want {
				t.Errorf("%q: exit = %d, want %d", tc.cmd, got.ExitCode, tc.want)
			}
		}
	})

	t.Run("TimeoutDoesNotKillTheSession", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh("export KEEP=yes; cd /var", 0)
		to := sh("sleep 300", time.Second)
		if !to.TimedOut {
			t.Fatalf("sleep 300 under a 1s timeout: TimedOut=false (%+v)", to)
		}
		after := sh(`echo "$KEEP:$(pwd)"`, 0)
		if strings.TrimSpace(after.Stdout) != "yes:/var" {
			t.Errorf("session state lost after a timeout: stdout=%q, want yes:/var", after.Stdout)
		}
	})

	t.Run("TimeoutDropsTheTimedOutCallsMutations", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh("cd /workspace", 0)
		to := sh("cd /tmp; sleep 300", time.Second)
		if !to.TimedOut {
			t.Fatalf("expected TimedOut, got %+v", to)
		}
		after := sh("pwd", 0)
		if strings.TrimSpace(after.Stdout) == "/tmp" {
			t.Error("a timed-out command's cd persisted; the EXIT trap must be skipped by SIGKILL")
		}
	})

	t.Run("FastCommandUnderTimeoutIsNotATimeout", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		got := sh("echo quick", time.Second)
		if got.TimedOut {
			t.Error("a fast command read as a timeout — the checkpoint bracket must fit inside the deadline")
		}
		if strings.TrimSpace(got.Stdout) != "quick" {
			t.Errorf("stdout=%q", got.Stdout)
		}
	})

	// The documented fidelity boundary: a backgrounded PROCESS survives across
	// calls (reachable by pid), but the shell's jobs table does not carry.
	t.Run("BackgroundProcessSurvivesButJobsTableDoesNot", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh("sleep 987654 >/dev/null 2>&1 &", 0)
		got := sh(`jobs; echo __END__`, 0)
		if before, _, _ := strings.Cut(got.Stdout, "__END__"); strings.TrimSpace(before) != "" {
			t.Errorf("jobs table carried across calls: %q — divergence not holding", before)
		}
		if n := countProc(t, sb, "sleep 987654"); n != 1 {
			t.Errorf("backgrounded process count = %d, want 1 (it must survive across calls)", n)
		}
	})

	t.Run("RestartResetsTheShellButKeepsFiles", func(t *testing.T) {
		sh, session := newShell(t, sb)
		sh("export GONE=1; cd /tmp; echo keep > /workspace/restart_probe", 0)
		res, err := shell.Run(context.Background(), sb, session, domain.NewID("sevt"),
			shell.Request{Restart: true})
		if err != nil {
			t.Fatalf("restart: %v", err)
		}
		if !res.Restarted {
			t.Error("Restarted not reported")
		}
		after := sh(`echo "[${GONE:-unset}][$(pwd)][$(cat /workspace/restart_probe)]"`, 0)
		if strings.TrimSpace(after.Stdout) != "[unset][/workspace][keep]" {
			t.Errorf("stdout = %q, want [unset][/workspace][keep] (shell reset, file kept)", after.Stdout)
		}
	})

	t.Run("OutputIsSeparatedAndCapped", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		// NUL bytes and the literal MAPDONE must survive — there is no sentinel.
		got := sh(`printf 'a\0b MAPDONE'; printf 'to-err' >&2`, 0)
		if got.Stdout != "a\x00b MAPDONE" {
			t.Errorf("stdout = %q, want binary-safe a\\0b MAPDONE", got.Stdout)
		}
		if got.Stderr != "to-err" {
			t.Errorf("stderr = %q, streams must not cross", got.Stderr)
		}
		big := sh(`yes a | head -c 1400000; echo err >&2`, 0)
		if len(big.Stdout) != sandbox.MaxOutputBytes {
			t.Errorf("stdout kept %d bytes, want the %d cap", len(big.Stdout), sandbox.MaxOutputBytes)
		}
		if !big.Truncated {
			t.Error("Truncated not reported past the cap")
		}
		if strings.TrimSpace(big.Stderr) != "err" {
			t.Errorf("stderr = %q — capping one stream must not lose the other", big.Stderr)
		}
	})

	// The whole point of re-exec over a resident shell: the sandbox's
	// outside-the-container deadline is inherited verbatim through the bracket.
	t.Run("TimeoutGuaranteeInheritedThroughTheBracket", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		got := sh(killWatchdog+"sleep 5", time.Second)
		if !got.TimedOut {
			t.Errorf("a command that killed its watchdog and overran was not a timeout: %+v", got)
		}
	})
}

// killWatchdog tears down the in-container watchdog a command can see, so only
// the outside-the-sandbox probe can still catch the overrun (mirrors the
// sandbox contract suite).
const killWatchdog = `
  for parent in $$ $PPID; do
    for p in $(cat /proc/$parent/task/$parent/children 2>/dev/null); do
      [ "$p" != "$$" ] && kill -9 "$p" 2>/dev/null
    done
  done
`

func countProc(t *testing.T, sb sandbox.Sandbox, prefix string) int {
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
		t.Fatalf("count: %v", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(res.Stdout))
	if err != nil {
		t.Fatalf("count %q: %v", res.Stdout, err)
	}
	return n
}
