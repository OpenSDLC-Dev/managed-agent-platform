package k8s

import (
	"context"
	"strings"
	"sync"
	"time"
)

// verdict is what the sandbox saw of a command's life from outside the pod, at
// the only two instants that decide a timeout. It mirrors the docker backend's
// verdict exactly; only the liveness primitive underneath differs.
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
		// gone; or the daemon would not answer, and hiding an overrun is worse
		// than mislabelling one, so assume still running.
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
	live, err := pd.probeAlive(ctx, state)
	if err != nil {
		return true
	}
	return live
}

// probeAlive runs one liveness check in the pod: read the recorded command pid
// and signal it with kill -0. A pod that has vanished answers gone.
func (pd *pod) probeAlive(ctx context.Context, state string) (bool, error) {
	out, _, err := pd.client.execOutput(ctx, pd.name, containerName,
		[]string{"/bin/bash", "-c", aliveScript, "map-alive", state})
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "A", nil
}
