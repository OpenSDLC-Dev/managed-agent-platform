package k8s

import "time"

// execWrapper runs the command and enforces its deadline from inside the pod,
// the way the docker backend's does — but adapted to Kubernetes, which (unlike
// Docker's exec-inspect) exposes no out-of-band handle on a running exec. So the
// command runs as a background child rather than via `exec`, and the wrapper
// records the two things the provider needs to judge the deadline from outside:
// the command's pid ($3.pid) and, once it finishes, its exit code ($3.exit).
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
// actually timed out is decided outside the pod, by Exec, from the pid it guards
// here.
// The command's stderr is handed fd 3 (the exec's real stderr, saved first), and
// the wrapper's own stderr is sent to /dev/null — so bash's "Killed" job
// notification, printed to the wrapper's stderr when it reaps a SIGKILLed job,
// never reaches the tool result. The command's stdout stays the exec's.
const execWrapper = `
exec 3>&2 2>/dev/null
setsid /bin/bash -c "$1" 2>&3 &
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
    kill -0 "$cmd" 2>/dev/null && kill -9 -"$cmd" 2>/dev/null
  ) >/dev/null 2>&1 &
fi
wait "$cmd"
echo "$?" > "$3.exit"
`

// aliveScript answers whether the command pid recorded in $1.pid is still alive.
// A missing pid file (the command has not recorded itself yet) reads as alive so
// a probe can never hide an overrun by racing the wrapper's first write.
const aliveScript = `
p=$(cat "$1.pid" 2>/dev/null)
if [ -z "$p" ] || kill -0 "$p" 2>/dev/null; then echo A; else echo D; fi
`

// exitScript prints the recorded exit code, or nothing if the command has not
// finished (the file is absent).
const exitScript = `cat "$1.exit" 2>/dev/null`

// sigkillExit is what bash reports for a job killed by SIGKILL (128 + 9).
const sigkillExit = 137

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
	// command is still alive — before, not at, since the watchdog fires at the
	// deadline and a command already killed by it looks like one never there.
	defaultProbeLead = 50 * time.Millisecond
)
