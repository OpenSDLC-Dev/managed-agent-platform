package k8s

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// verdict is what the sandbox saw of a command's life from outside the pod, at
// the two instants it looks. It has the same shape as the docker backend's, but
// it does not carry the same weight: docker's liveness primitive is a cheap
// out-of-band daemon call, while this one is a whole in-pod exec, so its
// pre-deadline answer is too late to be relied on for a punctual kill. What
// decides that here is the watchdog's own mark (see deadline.go's execWrapper);
// these two instants are the reach around it, and classifyTimeout weighs all
// three.
type verdict struct {
	// aliveAtDeadline: still running as the deadline arrived, so a SIGKILL that
	// follows is the watchdog's and not the command's own.
	aliveAtDeadline bool
	// overran: still running once the deadline and the measurement slop had both
	// passed, so no exit code it later reports can be believed.
	overran bool
}

// probeDeadline answers those two questions and nothing else, on two clocks.
//
// sleepCtx times the waits and is cancelled the moment the command's exec stream
// closes: a probe still sleeping then never mattered, because the stream cannot
// close while the process holding it is alive. confirmCtx times only the overrun
// liveness check — a check that has already reached the overrun instant and is
// mid-request; the stream closing must not cancel it, or a command that overran
// and then exited during its own confirming probe would have the cancellation
// read as "gone" and its overrun erased. The two instants are watched
// independently so a slow first probe can never delay the overrun one.
func (pd *pod) probeDeadline(sleepCtx, confirmCtx context.Context, state string, deadline time.Duration, start time.Time) <-chan verdict {
	answer := make(chan verdict, 1)
	var atDeadline, overran bool
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if sleepUntil(sleepCtx, start.Add(deadline-pd.probeLead)) {
			atDeadline = pd.alive(sleepCtx, state)
		}
	}()
	go func() {
		defer wg.Done()
		if sleepUntil(sleepCtx, start.Add(deadline+pd.overrunSlop)) {
			overran = pd.aliveOrTimedOut(confirmCtx, state)
		}
	}()
	go func() {
		wg.Wait()
		answer <- verdict{aliveAtDeadline: atDeadline, overran: overran}
	}()
	return answer
}

// sleepUntil reports whether it got there.
func sleepUntil(ctx context.Context, t time.Time) bool {
	timer := time.NewTimer(time.Until(t))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// alive answers the pre-deadline probe. Its context is the one the stream close
// cancels, and it reads that cancellation as "process gone": the stream cannot
// close while the process holding it is alive, so a close before the deadline is
// a command that finished early.
func (pd *pod) alive(ctx context.Context, state string) bool {
	live, err := pd.probeAlive(ctx, state)
	if err != nil {
		// The stream closed (ctx cancelled), so the process it was holding is
		// gone; or the probe would not answer (the daemon, or the probe process
		// killed out from under us), and hiding an overrun is worse than
		// mislabelling one, so assume still running.
		//
		// Unlike the docker backend this does NOT retry. Docker's probe is an
		// unkillable daemon call, so a second look only recovers from a transient
		// glitch; the k8s probe is a killable in-pod exec, so a retry could read a
		// command that exited between the two looks as "dead" — and on the overrun
		// path (aliveOrTimedOut) that would erase an overrun it must never erase.
		// The safe default is sticky.
		if ctx.Err() != nil {
			return false
		}
		return true
	}
	return live
}

// aliveOrTimedOut answers the overrun probe, which has already reached the
// overrun instant. Its context is not the one the stream close cancels, so a
// cancellation here is Exec running out of its own bound, not the process
// finishing; that, and a daemon that will not answer, both read as still
// running — erasing an overrun breaks the guarantee, over-reporting one costs a
// tool call.
func (pd *pod) aliveOrTimedOut(ctx context.Context, state string) bool {
	// Sticky by construction: this instant is the guarantee. Once the overrun
	// instant is reached, any answer other than a clean "the command is gone"
	// must read as still-running — an errored or killed probe, or a command that
	// exits a moment later, cannot be allowed to revise an overrun away. So no
	// retry: a second look that caught the command already exited would erase it.
	live, err := pd.probeAlive(ctx, state)
	if err != nil {
		return true
	}
	return live
}

// probeAlive runs one liveness check in the pod: read the recorded command pid
// and signal it with kill -0. A pod that has vanished answers gone. aliveScript
// always exits 0 with its verdict, so a non-zero exit is the probe itself killed
// or unable to run — an error, never a "dead" reading a command could arrange by
// killing the probe process before it prints its answer.
func (pd *pod) probeAlive(ctx context.Context, state string) (bool, error) {
	out, code, err := pd.client.execOutput(ctx, pd.name, containerName,
		[]string{"/bin/bash", "-c", aliveScript, "map-alive", state})
	if err != nil {
		return false, err
	}
	if code != 0 {
		return false, fmt.Errorf("k8s: liveness probe exited %d", code)
	}
	return strings.TrimSpace(out) == "A", nil
}
