package docker

import (
	"strings"
	"testing"
)

// TestClassifyTimeout is where #390 was lost. Under host load the two probes are
// scheduled after the watchdog's kill has already landed; `processAlive` then
// answers, honestly, that the process is gone, and both terms of the old
// expression came back false beside an exit code of 137. A command the platform
// genuinely timed out was reported to the model as an ordinary kill, losing the
// one signal that says "too slow" rather than "died".
//
// The k8s backend lost the same classification to the same race (#95, #110) and
// answered it with a watchdog that marks its own kill. These cases are that
// backend's table, re-asked of this one — the two classifiers now agree case for
// case, which is the point: a shared contract suite is worth little if the two
// implementations disagree about what a timeout is.
func TestClassifyTimeout(t *testing.T) {
	for _, c := range []struct {
		name          string
		undeadlined   bool
		code          int
		watchdogFired bool
		v             verdict
		want          bool
	}{
		// The bug, stated as a test: the watchdog fired, and both probes looked
		// after the fact. Nothing but the mark can save this one.
		{name: "LateProbesStillSeeTheWatchdogsMark",
			code: sigkillExit, watchdogFired: true, want: true},
		// The paths that already worked, which the mark must not disturb.
		{name: "PunctualProbeSeesItAliveAtTheDeadline",
			code: sigkillExit, v: verdict{aliveAtDeadline: true}, want: true},
		{name: "AnOverrunNeedsNoExitCodeAtAll",
			code: 0, v: verdict{overran: true}, want: true},
		// **The monotonicity rows.** Weighing state written inside the container is
		// only defensible because a mark can add a timeout and never remove one, and
		// a review pass found that property resting on nothing: mutating the last
		// term to `v.overran && !watchdogFired` — letting the mark suppress the one
		// verdict that carries the deadline's guarantee — left the whole suite green.
		// These are the rows that would have caught it. A mark must not subtract from
		// an overrun, whatever the exit code beside it.
		{name: "AMarkNeverSuppressesAnOverrun",
			code: 0, watchdogFired: true, v: verdict{overran: true}, want: true},
		{name: "AMarkNeverSuppressesAnOverrunOnASigkillEither",
			code: sigkillExit, watchdogFired: true, v: verdict{overran: true}, want: true},
		{name: "AMarkNeverSuppressesAPunctualProbe",
			code: sigkillExit, watchdogFired: true, v: verdict{aliveAtDeadline: true}, want: true},
		{name: "AFinishedCommandIsNotATimeout",
			code: 0, want: false},
		// A mark without a SIGKILL is not evidence of a kill: the watchdog marks
		// between its `kill -0` and its `kill -9`, so a command that exits in that
		// window is marked without having been killed. Requiring 137 alongside is
		// what makes that harmless.
		{name: "AMarkWithoutASigkillIsNotATimeout",
			code: 0, watchdogFired: true, want: false},
		{name: "AMarkDoesNotRescueSomeOtherExitCode",
			code: 1, watchdogFired: true, want: false},
		// A command with no deadline was never given a watchdog, so nothing can
		// honestly have marked it. Refusing the mark here is what stops a tenant
		// who plants one from labelling its own untimed command a timeout.
		{name: "NoDeadlineIgnoresAPlantedMark",
			undeadlined: true, code: sigkillExit, watchdogFired: true, want: false},
		{name: "NoDeadlineIgnoresAnOverrunItCannotHaveHad",
			undeadlined: true, code: sigkillExit, v: verdict{overran: true}, want: false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyTimeout(!c.undeadlined, c.code, c.watchdogFired, c.v); got != c.want {
				t.Errorf("classifyTimeout(%v, %d, %v, %+v) = %v, want %v",
					!c.undeadlined, c.code, c.watchdogFired, c.v, got, c.want)
			}
		})
	}
}

// TestWatchdogMarksBeforeItKills pins the two properties of the mark that the
// classification rests on, in the one place they are expressed: the wrapper.
//
// The ordering is pinned because the mark must not depend on the watchdog
// outliving the signal it sends. It is *not* pinned because the watchdog dies
// with the command: `set -m` is job control, so the background subshell gets its
// own process group, and a `mkdir` after `kill -9 -"$self"` demonstrably runs.
// Which way that goes is a bash detail that varies with how the shell was
// started, and a classification resting on it would be resting on an accident —
// so the wrapper writes first and the assertion keeps it that way.
//
// `mkdir` rather than a redirect, for the reason the k8s wrapper gives: the mark
// must never be able to hold the kill back. `: > "$3.killed"` opens the path, and
// a tenant who plants a FIFO there — the state path is its own parent's argv, so
// it is readable from /proc — blocks that open forever, and the runaway this
// machinery exists to kill would never die. `mkdir` cannot block: it creates the
// path or fails immediately, whatever is already sitting there.
func TestWatchdogMarksBeforeItKills(t *testing.T) {
	mark := strings.Index(execWrapper, `mkdir "$3.killed"`)
	if mark < 0 {
		t.Fatalf("the watchdog does not mark its kill:\n%s", execWrapper)
	}
	kill := strings.Index(execWrapper, `kill -9 -"$self"`)
	if kill < 0 {
		t.Fatalf("the watchdog does not kill the group:\n%s", execWrapper)
	}
	if mark > kill {
		t.Errorf("the mark is written after the kill that would erase the watchdog writing it")
	}
	// A redirect would be the natural way to write a mark and is the one spelling
	// that can hang the watchdog forever.
	if strings.Contains(execWrapper, `> "$3.killed"`) {
		t.Errorf("the mark is written with a redirect, which a planted FIFO can block:\n%s", execWrapper)
	}
}
