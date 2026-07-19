package k8s

import "time"

// execWrapper runs the command and enforces its deadline from inside the pod,
// the way the docker backend's does — but adapted to Kubernetes, which (unlike
// Docker's exec-inspect) exposes no out-of-band handle on a running exec. So the
// command runs as a background child rather than via `exec`, and the wrapper
// records what the provider needs to judge the deadline from outside: the
// command's pid ($3.pid) and, once it finishes, its exit code together with
// whether the watchdog was the one that killed it ($3.exit).
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
// and its `kill -9`, which the wrapper folds into `$3.exit` — is what makes a
// punctual kill classifiable at all here (#95, #110). Exec's pre-deadline
// liveness probe is itself an in-pod exec, so its answer reflects an instant one
// apiserver round trip after it was asked for; when that round trip outruns the
// one that started this wrapper, the probe reads a command the watchdog has just
// killed as one that was never running, and a real timeout came back
// `ExitCode: 137, TimedOut: false`. The watchdog knows what the probe was
// guessing at. The mark is written *before* the signal so it is on disk by the
// time `wait` returns, and its write is deliberately not chained to the kill: a
// mark that cannot be written (a read-only /tmp, or a path a tenant pre-created
// as a directory) must never suppress the kill itself.
//
// The mark is in-pod state, which the docker backend deliberately does not keep
// (docs/HISTORY.md § the docker deadline: a marker file was its first design and
// was removed). Two things make it sound here and not there: Kubernetes exposes
// no out-of-band handle, so this backend's verdict already rests on in-pod state
// (`$3.pid`) either way; and the mark is only ever an *additional* reason to
// call a timeout, weighed by Exec alongside a recorded SIGKILL, never a reason
// to withdraw one. Only its content is untrusted, so only its existence is read.
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
      : > "$3.killed"
      kill -9 -"$cmd" 2>/dev/null
    fi
  ) >/dev/null 2>&1 3>&- &
fi
wait "$cmd"
rc=$?
k=
[ -f "$3.killed" ] && k=K
echo "$rc $k" > "$3.exit"
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

// exitScript prints the line the wrapper recorded — the exit code, then the
// watchdog's mark if it fired — or nothing if the command has not finished (the
// file is absent), then removes the exec's state files. The read happens once
// the probes are done (Exec has the verdict before it calls this), so the
// cleanup cannot race a probe, and it keeps /tmp from accumulating three files
// per command over a session's thousands of execs.
//
// The mark rides in the same file rather than being read as a second one, so
// "the wrapper recorded nothing at all" stays exactly one thing — empty output —
// and keeps meaning what it has always meant to readExit.
const exitScript = `cat "$1.exit" 2>/dev/null; rm -f "$1.pid" "$1.exit" "$1.killed" 2>/dev/null`

// sigkillExit is what bash reports for a job killed by SIGKILL (128 + 9).
const sigkillExit = 137

// killedMark is what the wrapper writes after the exit code when the watchdog
// reports having fired.
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
	// already killed looks like one never there. It is only a lead on Exec's own
	// clock: the watchdog's starts when the wrapper reaches the pod, and the
	// probe's answer arrives an exec round trip after it is asked for, so this
	// buys no reliable margin against the kill and is not what classifies a
	// timeout — the watchdog's own mark is (see execWrapper). What the lead still
	// earns is the case the mark cannot cover: a SIGKILL the watchdog did not
	// deliver.
	defaultProbeLead = 50 * time.Millisecond
)
