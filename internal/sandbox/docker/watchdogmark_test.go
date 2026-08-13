package docker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// TestTheWatchdogReallyWritesTheMark closes the gap the tests above leave. They
// prove Exec reads a mark and classifies on it, but they hand the fake daemon a
// mark that no watchdog wrote — so on their own they would pass a wrapper that
// never marks anything.
//
// This runs the wrapper through the host's own /bin/bash, as the k8s backend's
// equivalent does, and watches the filesystem. No cluster and no daemon can be
// told to schedule a probe late, but the mark itself is plainly observable.
// Whether the sandbox *image* carries a userland able to run the script is the
// live contract test's job, not this one's.
//
// It pins the **mark**, deliberately, and not the kill beside it. Under `go test`
// the wrapper has no controlling terminal, so `set -m` need not make it a process
// group leader, and the watchdog's `kill -9 -"$self"` can name a group that does
// not exist — the command then runs to completion with the mark already written.
// That is an artifact of the harness rather than of the wrapper: the group kill is
// long-shipped behaviour, exercised against a real container by the shell suite
// (`TestShell/TimeoutDoesNotKillTheSession`, the very test #390 was observed in).
// Which is why the commands here are short: a wrapper whose kill does not land
// costs this test the command's full run time.
func TestTheWatchdogReallyWritesTheMark(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("no /bin/bash on this host, and the wrapper is a bash script")
	}
	dir := t.TempDir()
	// The wrapper `exec`s the command, so the wrapper process *is* the command and
	// a punctual kill takes it: Run reports the signal, which is the case under
	// test rather than a failure of it.
	run := func(t *testing.T, name, command string, settle time.Duration) bool {
		t.Helper()
		state := filepath.Join(dir, name)
		cmd := exec.Command("/bin/bash", "-c", execWrapper, "map-exec", command, strconv.Itoa(1), state)
		_ = cmd.Run()
		// Give a watchdog that did NOT fire time to reach the instant it would
		// have fired at, so "no mark" means it declined rather than that nobody
		// looked yet.
		time.Sleep(settle)
		_, err := os.Stat(state + ".killed")
		return err == nil
	}

	t.Run("AKillOnTheDeadlineIsMarked", func(t *testing.T) {
		if !run(t, "killed", "sleep 3", 0) {
			t.Error("the watchdog killed the command on its deadline and left no mark, " +
				"so a late probe has nothing to read and #390 is back")
		}
	})

	t.Run("ACommandThatFinishedOnItsOwnIsNotMarked", func(t *testing.T) {
		// The negative half, and the reason the mark means anything: the watchdog
		// re-checks `kill -0` before marking, so a command already gone is never
		// claimed as its kill.
		if run(t, "early", "exit 0", 1500*time.Millisecond) {
			t.Error("a command that exited on its own was marked as a watchdog kill")
		}
	})
}

// TestLateProbesReadTheWatchdogsMark is #390, reproduced at the level the bug
// actually lives at rather than by racing a loaded host.
//
// The observed failure was `TimedOut=false` beside `ExitCode:137` on a command
// given a one-second deadline, in a run where the package took 135 seconds. Both
// probes keep their own clocks, and under that load they were scheduled after the
// watchdog's kill had already landed; `top` then answered, honestly, that the
// process was gone. Neither probe was wrong about what it saw. Both simply looked
// after the fact.
//
// The two cases below are the same exec as far as anything outside the container
// can see — same exit code, same probe answers, same timings — and they are
// distinguished only by whether the watchdog left its mark. That is the whole
// content of the fix: a witness that does not depend on when a probe was
// scheduled.
func TestLateProbesReadTheWatchdogsMark(t *testing.T) {
	// The process is gone by the time anything looks, and the stream closes with
	// it, so both probes answer false — exactly the state the misclassification
	// lived in.
	const gone = time.Millisecond

	t.Run("MarkedByTheWatchdogIsATimeout", func(t *testing.T) {
		res, err := execDaemon(t, fakeExec{aliveFor: gone, code: sigkillExit, killed: true}).
			Exec(context.Background(), sandbox.ExecRequest{Command: "sleep 300", Timeout: time.Second})
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		if !res.TimedOut {
			t.Errorf("a command its watchdog marked as killed read as a plain SIGKILL: %+v", res)
		}
		if res.ExitCode != sigkillExit {
			t.Errorf("exit code = %d, want %d — the mark must not rewrite it", res.ExitCode, sigkillExit)
		}
	})

	t.Run("UnmarkedIsStillJustAnExitCode", func(t *testing.T) {
		// The negative half, and the reason the mark is not simply "137 past a
		// deadline is a timeout": a command that SIGKILLs itself leaves no mark,
		// and must keep reading as the plain kill it is.
		res, err := execDaemon(t, fakeExec{aliveFor: gone, code: sigkillExit}).
			Exec(context.Background(), sandbox.ExecRequest{Command: "kill -9 $$", Timeout: time.Second})
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		if res.TimedOut {
			t.Errorf("a self-inflicted SIGKILL read as a timeout on the strength of no mark at all: %+v", res)
		}
	})
}

// TestTheMarkIsReadOnlyWhereItCouldMatter pins the round trip's cost. The read is
// an extra request to the daemon on the tool-call path, so it is asked only in
// the state where the answer can still change the verdict: a deadlined command
// whose exit code says SIGKILL and whose probes both came back false. A command
// that finished cleanly, one with no deadline, and one already known to have
// overrun must each cost nothing.
func TestTheMarkIsReadOnlyWhereItCouldMatter(t *testing.T) {
	for _, c := range []struct {
		name string
		fe   fakeExec
		req  sandbox.ExecRequest
		want int
	}{
		{name: "AConsultedMark",
			fe:   fakeExec{aliveFor: time.Millisecond, code: sigkillExit},
			req:  sandbox.ExecRequest{Command: "kill -9 $$", Timeout: time.Second},
			want: 1},
		{name: "ACleanExitAsksNothing",
			fe:   fakeExec{aliveFor: time.Millisecond, code: 0},
			req:  sandbox.ExecRequest{Command: "echo hi", Timeout: time.Second},
			want: 0},
		{name: "NoDeadlineAsksNothing",
			fe:   fakeExec{aliveFor: time.Millisecond, code: sigkillExit},
			req:  sandbox.ExecRequest{Command: "kill -9 $$"},
			want: 0},
		{name: "AProbeThatSawItAliveAsksNothing",
			fe:   fakeExec{aliveFor: 1200 * time.Millisecond, code: sigkillExit},
			req:  sandbox.ExecRequest{Command: "sleep 300", Timeout: time.Second},
			want: 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			marks := 0
			c.fe.marks = &marks
			if _, err := execDaemon(t, c.fe).Exec(context.Background(), c.req); err != nil {
				t.Fatalf("exec: %v", err)
			}
			if marks != c.want {
				t.Errorf("the mark was read %d times, want %d", marks, c.want)
			}
		})
	}
}

// TestAnUnreadableMarkChangesNothing is the one place this file does not fail
// toward the timeout label. The mark exists to add precision; a daemon that will
// not answer leaves the classification exactly where it stood before the mark
// existed, rather than turning a hiccup into a timeout on a command that
// finished early and chose 137 for itself.
func TestAnUnreadableMarkChangesNothing(t *testing.T) {
	c := execDaemon(t, fakeExec{aliveFor: time.Millisecond, code: sigkillExit})
	// A base the daemon cannot serve: the read errors rather than 404s.
	c.api.base = "http://127.0.0.1:1"
	if got := c.watchdogFired(context.Background(), 1, sigkillExit, verdict{}, "/tmp/.map-exec-x"); got {
		t.Error("an unreadable mark was read as a watchdog kill")
	}
}
