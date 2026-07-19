package k8s

import "time"

// execWrapper runs the command and enforces its deadline from inside the pod,
// the way the docker backend's does — but adapted to Kubernetes, which (unlike
// Docker's exec-inspect) exposes no out-of-band handle on a running exec. So the
// command runs as a background child rather than via `exec`, and the wrapper
// records the three things the provider needs to judge the deadline from
// outside: the command's pid ($3.pid), its exit code once it finishes ($3.exit),
// and — written by the watchdog, not the wrapper — whether the deadline's own
// kill was what ended it ($3.killed).
//
// $1 is the command, $2 the timeout in whole seconds ("0" = no limit), $3 the
// state-file base path (unique per exec).
//
// `setsid` puts the command in its own session — and so its own process group,
// led by its own pid — so the watchdog's `kill -9 -"$cmd"` takes its children
// with it (a group, not a tree: a child that calls setsid again escapes, bounded
// by the pod's teardown). It also keeps the wrapper out of job-control monitor
// mode, so bash never prints a "Killed" job notification onto the exec's stderr
// when the watchdog SIGKILLs the command. (setsid execs the command in place
// here — the wrapper is not a process-group leader — so `$!` is the command's
// own pid and its parent is still the wrapper, which is what the $PPID-sabotage
// case relies on.) The command inherits the exec's stdout/stderr; the wrapper
// writes only to the state files and the watchdog only to /dev/null, so nothing
// pollutes the tool output. The watchdog polls `kill -0 "$cmd"` rather than
// sleeping the whole deadline, so an honest command that finishes early takes
// its watchdog with it within one poll. It is best effort by construction — a
// process inside the pod that the command can find and kill — so whether a call
// actually timed out is decided outside the pod, by Exec, from the evidence this
// wrapper leaves: the pid it guards, the exit code it records, and whether the
// watchdog reports having fired.
//
// That last piece — the watchdog marking `$3.killed` between its final `kill -0`
// and its `kill -9` — is what makes a punctual kill classifiable at all here
// (#95, #110); why the mark rather than a probe, and why it is safe to weigh
// in-pod state at all, is argued once at classifyTimeout, where the decision is
// made. What belongs here is how the mark is written.
//
// It is a **directory**, made with `mkdir`, and that is the whole point rather
// than an oddity. The mark must never be able to hold the kill back, and a
// redirect cannot promise that: `: > "$3.killed"` opens the path, and a tenant
// who plants a FIFO there (the state path is its own parent's argv, so it is
// readable from /proc) blocks that open forever — the watchdog would hang before
// `kill -9` and the runaway would never die, a strictly worse outcome than the
// bug this fixes. `mkdir` is the one creation primitive that cannot block: it
// either creates the path or fails immediately, whatever is already sitting
// there. Nor is it a shell special builtin, so a redirection failure cannot
// abort the watchdog subshell under a POSIX-mode bash. A tenant can still make
// the `mkdir` fail and suppress its own mark — that only returns the
// classification to where it stood before this existed, which is the direction
// this trade is allowed to fail in.
//
// The command's stderr is handed fd 3 (the exec's real stderr, saved first) and
// then fd 3 is closed in the command (`2>&3 3>&-`): its stderr still reaches the
// exec, but the extra descriptor does not linger for a straggler to inherit and
// hold the stream open long after the command exited (the docker backend, whose
// command's stderr *is* the exec's, has no such spare fd). The wrapper's own
// stderr is sent to /dev/null — so bash's "Killed" job notification, printed
// when it reaps a SIGKILLed job, never reaches the tool result. The command's
// stdout stays the exec's. The watchdog subshell likewise closes its inherited
// fd 3 (`3>&-`): only the command and the short-lived wrapper should hold the
// exec's stderr open, so the stream EOFs the moment the command finishes — a
// watchdog still asleep in `sleep 1` must not pin it open and delay every timed
// command's return by up to a poll interval.
const execWrapper = `
exec 3>&2 2>/dev/null
setsid /bin/bash -c "$1" 2>&3 3>&- &
cmd=$!
echo "$cmd" > "$3.pid"
if [ "$2" != "0" ]; then
  (
    n=0
    while [ "$n" -lt "$2" ]; do
      kill -0 "$cmd" 2>/dev/null || exit 0
      sleep 1
      n=$((n + 1))
    done
    if kill -0 "$cmd" 2>/dev/null; then
      mkdir "$3.killed" 2>/dev/null
      kill -9 -"$cmd" 2>/dev/null
    fi
  ) >/dev/null 2>&1 3>&- &
fi
wait "$cmd"
echo "$?" > "$3.exit"
`

// aliveScript answers whether the command pid recorded in $1.pid is still alive.
// A missing pid file (the command has not recorded itself yet) reads as alive so
// a probe can never hide an overrun by racing the wrapper's first write.
//
// The pid lives in a file in the sandbox's own filesystem, unlike the docker
// backend, whose probe reads the exec's pid from the daemon out of band where the
// command can neither reach nor forge it. Kubernetes exposes no such out-of-band
// handle, so this probe is best effort against a command that deliberately
// overwrites its own pid file to look dead — the same malicious-tenant case the
// derived-name adoption check (`ours`) does not defend either. Against an honest
// runaway or an accident — the deadline machinery's actual job — it is faithful:
// the honest command leaves its real pid, and the watchdog kills off that same
// pid regardless of the file. The script always exits 0 with A or D, so a caller
// treats any non-zero exit as the probe itself failing, not as a verdict.
const aliveScript = `
p=$(cat "$1.pid" 2>/dev/null)
if [ -z "$p" ] || kill -0 "$p" 2>/dev/null; then echo A; else echo D; fi
`

// exitScript collects both halves of the answer in one exec and takes the whole
// exec's state with it: the watchdog's mark if it fired, then the exit code the
// wrapper recorded (nothing, if the command never finished or the wrapper was
// killed before it could write one). The read happens once the probes are done
// (Exec has the verdict before it calls this), so the cleanup cannot race a
// probe, and it keeps /tmp from accumulating three entries per command over a
// session's thousands of execs.
//
// The mark is printed *first* because it is the more load-bearing of the two and
// this stream is unframed: client-go stops copying stdout at its first error, so
// what a lost stream drops is always a suffix. Losing the code leaves a
// synthesized SIGKILL and a mark that still says the deadline caused it; losing
// the mark instead would put a real timeout back on the probe race #95 was filed
// for. Reading the mark here rather than in the wrapper is what lets it survive
// the wrapper's own sabotage: a command that kills its parent before the exit
// code is recorded leaves the mark, and the timeout still shows.
//
// `rm -rf` on the mark, because the tenant chooses what type of thing sits at
// that path; `rm -f` would leave a directory or a planted FIFO behind forever.
const exitScript = `
k=
[ -d "$1.killed" ] && k=K
c=$(cat "$1.exit" 2>/dev/null)
rm -f "$1.pid" "$1.exit" 2>/dev/null
rm -rf "$1.killed" 2>/dev/null
echo "$k $c"
`

// sigkillExit is what bash reports for a job killed by SIGKILL (128 + 9).
const sigkillExit = 137

// killedMark is what exitScript prints ahead of the exit code when it finds the
// watchdog's mark.
const killedMark = "K"

const (
	// defaultKillGrace is how long Exec waits past a command's deadline for the
	// in-pod watchdog to finish the kill before reporting the timeout on its own
	// authority.
	defaultKillGrace = 2 * time.Second
	// defaultOverrunSlop is how much of the measured time Exec charges to itself
	// rather than the command: the API round trips and the poll interval blur the
	// moment a command exited. It must stay under killGrace.
	defaultOverrunSlop = 500 * time.Millisecond
	// defaultProbeLead is how far before the deadline Exec asks whether the
	// command is still alive — before, not at, since a command the watchdog has
	// already killed looks like one never there. It is a lead on Exec's own clock
	// only, which is why it is not what classifies a punctual timeout here; see
	// classifyTimeout.
	defaultProbeLead = 50 * time.Millisecond
)
