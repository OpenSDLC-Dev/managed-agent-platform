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

// provision gives the whole test one real container; each subtest scopes its own
// shell with a fresh session id, so they share the container but not its state.
// A missing daemon is a hard failure, as with the other suites.
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
// fresh tool id. It also returns the session, so a test can issue a restart
// against the same shell.
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

	// errexit is the option the snapshot is most likely to lose, because the save
	// has to turn it off before it can safely write: the option state must be
	// captured before that happens, or `set -e` can never carry.
	t.Run("ErrexitPersistsAcrossCalls", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh("set -o errexit", 0)
		got := sh(`[[ -o errexit ]] && echo ERREXIT_ON || echo ERREXIT_OFF`, 0)
		if !strings.Contains(got.Stdout, "ERREXIT_ON") {
			t.Errorf("set -e did not persist; stdout=%q", got.Stdout)
		}
	})

	// Only exported variables carry. Nothing in `declare` separates a user's plain
	// variables from bash's own internals, so the snapshot draws the line at
	// `export` — and the line has to hold in both directions.
	t.Run("PlainVariablesDoNotCarryButExportedOnesDo", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh("PLAIN=here; export EXPORTED=here", 0)
		got := sh(`echo "[${PLAIN:-gone}][${EXPORTED:-gone}]"`, 0)
		if strings.TrimSpace(got.Stdout) != "[gone][here]" {
			t.Errorf("stdout = %q, want [gone][here] — the snapshot draws the line at export", got.Stdout)
		}
	})

	// Traps do not carry. The next call is a fresh bash whose only EXIT trap is
	// the template's own save; a trap the command installed is not in it.
	t.Run("TrapsDoNotCarryAcrossCalls", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh(`trap 'echo BYE' EXIT; trap 'echo HUP' HUP`, 0)
		got := sh("trap -p", 0)
		if strings.Contains(got.Stdout, "BYE") || strings.Contains(got.Stdout, "HUP") {
			t.Errorf("trap -p = %q — a command's traps carried into the next call", got.Stdout)
		}
	})

	t.Run("AliasesPersistAcrossCalls", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh("shopt -s expand_aliases; alias greet='echo aliased'", 0)
		got := sh("greet", 0)
		if strings.TrimSpace(got.Stdout) != "aliased" {
			t.Errorf("stdout = %q, want aliased (alias did not persist)", got.Stdout)
		}
	})

	// A readonly export must be carried as a readonly export, not dropped. The
	// snapshot may only skip the names a fresh bash makes readonly for itself.
	t.Run("ReadonlyExportPersists", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh("declare -rx TOKEN=secret", 0)
		got := sh(`echo "[${TOKEN:-unset}]"`, 0)
		if strings.TrimSpace(got.Stdout) != "[secret]" {
			t.Errorf("stdout = %q, want [secret] — a readonly export was dropped", got.Stdout)
		}
	})

	// A line-oriented filter over `declare -px` would cut a multi-line value in
	// half and leave the env snapshot with an unterminated quote, taking every
	// variable declared after it down with it.
	t.Run("MultilineExportSurvivesAndDoesNotCorruptTheRest", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh(`export V=$'x\ndeclare -ar Y'; export AFTER=ok`, 0)
		got := sh(`[ "$V" = $'x\ndeclare -ar Y' ] && echo V_OK; echo "[${AFTER:-lost}]"`, 0)
		if !strings.Contains(got.Stdout, "V_OK") {
			t.Errorf("multi-line exported value did not survive; stdout=%q", got.Stdout)
		}
		if !strings.Contains(got.Stdout, "[ok]") {
			t.Errorf("a variable exported after the multi-line one was lost; stdout=%q", got.Stdout)
		}
	})

	// Each call is a fresh bash that re-inherits the container's environment, so a
	// variable the shell unset has to be unset again on restore or it silently
	// reappears — which is exactly what an agent scrubbing a secret must not see.
	t.Run("UnsetOfAnInheritedVariablePersists", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		if probe := sh(`echo "[${HOME:-unset}]"`, 0); strings.TrimSpace(probe.Stdout) == "[unset]" {
			t.Fatalf("test needs HOME inherited from the container, got %q", probe.Stdout)
		}
		sh("unset HOME", 0)
		got := sh(`echo "[${HOME:-UNSET}]"`, 0)
		if strings.TrimSpace(got.Stdout) != "[UNSET]" {
			t.Errorf("stdout = %q, want [UNSET] — an unset variable came back from the container env", got.Stdout)
		}
	})

	// A command that installs its own EXIT trap takes the only trap slot there is.
	// The snapshot has to survive that, or one `trap ... EXIT` silently discards
	// the whole call's state. The trap itself is discarded UNFIRED when the command
	// returns normally — the template clears it to run its own save — so an EXIT
	// trap only ever fires for a command that exits through it.
	t.Run("CommandsOwnExitTrapDoesNotLoseTheSnapshot", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		installed := sh(`cd /tmp; export T=1; trap 'echo BYE' EXIT`, 0)
		if strings.Contains(installed.Stdout, "BYE") {
			t.Errorf("stdout = %q — a normally-returning command's EXIT trap fired; the template clears it", installed.Stdout)
		}
		got := sh(`echo "[$(pwd)][${T:-unset}]"`, 0)
		if strings.TrimSpace(got.Stdout) != "[/tmp][1]" {
			t.Errorf("stdout = %q, want [/tmp][1] — a command's EXIT trap ate the snapshot", got.Stdout)
		}
	})

	// The template's own names must never be snapshotted, or a command that
	// defines one reaches across into the next call's machinery. (__map_save
	// itself is a poor probe: the template defines it on every call. A command
	// that redefines __map_save sabotages only its own call's snapshot, which is
	// the documented self-inflicted case.)
	t.Run("TemplateMachineryIsNotSnapshotted", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh(`__map_helper() { echo bad; }; export __map_state=hijacked; helper() { echo kept; }; cd /var`, 0)
		got := sh(`declare -F __map_helper >/dev/null && echo FN_LEAKED || echo FN_CLEAN
			[ "${__map_state:-}" = hijacked ] && echo VAR_LEAKED || echo VAR_CLEAN
			helper; pwd`, 0)
		if strings.Contains(got.Stdout, "FN_LEAKED") {
			t.Error("a command's __map_* function was carried into the next call")
		}
		if strings.Contains(got.Stdout, "VAR_LEAKED") {
			t.Error("a command's exported __map_* variable was carried into the next call")
		}
		if !strings.Contains(got.Stdout, "kept") || !strings.Contains(got.Stdout, "/var") {
			t.Errorf("ordinary state was lost alongside it; stdout=%q", got.Stdout)
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

	// The killed path: the SIGKILL skips the save outright.
	t.Run("TimeoutDropsTheTimedOutCallsMutations", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh("cd /workspace", 0)
		to := sh("cd /tmp; export EVIL=1; sleep 300", time.Second)
		if !to.TimedOut {
			t.Fatalf("expected TimedOut, got %+v", to)
		}
		after := sh(`echo "[$(pwd)][${EVIL:-unset}]"`, 0)
		if strings.TrimSpace(after.Stdout) != "[/workspace][unset]" {
			t.Errorf("stdout = %q, want [/workspace][unset] — a timed-out call's mutations persisted", after.Stdout)
		}
	})

	// The path a SIGKILL never reaches: the command kills the in-container
	// watchdog, overruns its deadline, and then exits on its own terms — so its
	// EXIT trap DOES run and its snapshot IS written. The call still reports a
	// timeout, so that snapshot is never committed, and the mutations still drop.
	t.Run("OverrunThatDodgedTheKillAlsoDropsItsMutations", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh("cd /workspace", 0)
		to := sh(killWatchdog+"cd /tmp; export EVIL=1; sleep 2", time.Second)
		if !to.TimedOut {
			t.Fatalf("a command that killed its watchdog and overran was not a timeout: %+v", to)
		}
		after := sh(`echo "[$(pwd)][${EVIL:-unset}]"`, 0)
		if strings.TrimSpace(after.Stdout) != "[/workspace][unset]" {
			t.Errorf("stdout = %q, want [/workspace][unset] — an overrun that ran its EXIT trap committed its state", after.Stdout)
		}
	})

	// A call can finish well inside its deadline and still never reach its save:
	// it replaced the shell with `exec`, the shell was killed outright, it exited
	// through an EXIT trap of its own, or it sent the save somewhere it could not
	// write. None of those is a timeout, so the deadline does not gate them — the
	// snapshot's own `done` marker does. Without that gate every one of them
	// points `head` at the empty directory the call created on its way in, which
	// loses not just that call's mutations but every earlier call's with them.
	t.Run("CallThatNeverFinishesItsSnapshotKeepsThePreviousState", func(t *testing.T) {
		for _, tc := range []struct{ name, ending string }{
			{"ExecReplacesTheShell", `exec echo replaced`},
			{"ShellKilledOutright", `kill -9 $$`},
			{"CommandExitsThroughItsOwnExitTrap", `trap 'echo bye' EXIT; exit 0`},
			{"CommandSendsTheSaveNowhere", `export __map_snap=/nonexistent/pwned`},
			// The save fails on ONE file and can still write the rest, including the
			// marker — which is what a mid-save ENOSPC or EIO looks like. The marker
			// must be gated on every write, not just on the last one: bash ignores
			// errexit inside a compound command on the left of `&&`, so the obvious
			// `( set -e; ... ) && : >done` creates the marker anyway.
			{"SaveCannotWriteOneOfItsFiles", `mkdir "$__map_snap/env"`},
			{"SaveCannotWriteTheOptions", `mkdir "$__map_snap/opts"`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				sh, _ := newShell(t, sb)
				sh("export KEEP=yes; cd /var", 0)
				got := sh("cd /tmp; export EVIL=1; "+tc.ending, 0)
				if got.TimedOut {
					t.Fatalf("%q reported a timeout — not the path under test", tc.ending)
				}
				after := sh(`echo "[${KEEP:-LOST}][${EVIL:-unset}][$(pwd)]"`, 0)
				if strings.TrimSpace(after.Stdout) != "[yes][unset][/var]" {
					t.Errorf("stdout = %q, want [yes][unset][/var] — a call that never saved must drop its own "+
						"mutations and leave the session's earlier state standing", after.Stdout)
				}
			})
		}
	})

	// The save writes the snapshot with builtins alone — no `mv`, no external
	// anything — so a command that breaks PATH is still snapshotted. This is the
	// hardening the restore already had, held to on the way out as well.
	t.Run("BrokenPATHDoesNotCostTheSnapshot", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh("export KEEP=yes; cd /var", 0)
		sh("export PATH=/nonexistent", 0)
		got := sh(`echo "[${KEEP:-LOST}][$(pwd)][$PATH]"`, 0)
		if strings.TrimSpace(got.Stdout) != "[yes][/var][/nonexistent]" {
			t.Errorf("stdout = %q, want [yes][/var][/nonexistent] — a broken PATH cost the snapshot", got.Stdout)
		}
	})

	t.Run("FastCommandUnderTimeoutIsNotATimeout", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		got := sh("echo quick", time.Second)
		if got.TimedOut {
			t.Error("a fast command read as a timeout — the snapshot bracket must fit inside the deadline")
		}
		if strings.TrimSpace(got.Stdout) != "quick" {
			t.Errorf("stdout=%q", got.Stdout)
		}
	})

	// A backgrounded PROCESS survives across calls (reachable by pid), but the
	// shell's jobs table does not carry.
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
// the outside-the-sandbox probe can still catch the overrun (mirrors the sandbox
// contract suite).
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
