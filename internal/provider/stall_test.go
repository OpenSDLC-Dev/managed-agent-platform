package provider_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// waitDone waits for ctx to end, failing the test if it outlasts limit — a
// guard that never trips must fail the test rather than hang it.
func waitDone(t *testing.T, ctx context.Context, limit time.Duration) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(limit):
		t.Fatalf("the guarded context was still live after %s", limit)
	}
}

func TestStallGuardCancelsAfterSilence(t *testing.T) {
	gctx, guard := provider.NewStallGuard(context.Background(), 100*time.Millisecond)
	defer guard.Stop()

	waitDone(t, gctx, 3*time.Second)
	if err := guard.Cause(nil); !errors.Is(err, provider.ErrStalled) {
		t.Errorf("Cause(nil) = %v, want an error matching ErrStalled", err)
	}
	// The error names the budget it exhausted: an operator reading the
	// session.error needs to know which one to raise.
	if err := guard.Cause(nil); err == nil || !strings.Contains(err.Error(), "100ms") {
		t.Errorf("Cause(nil) = %v, want the exhausted budget named", err)
	}
}

// A tripped guard must not merely report the stall — it must replace whatever
// the aborted read said, because that error is a bare "context canceled".
func TestStallGuardCauseReplacesTheCancellationError(t *testing.T) {
	gctx, guard := provider.NewStallGuard(context.Background(), 100*time.Millisecond)
	defer guard.Stop()

	waitDone(t, gctx, 3*time.Second)
	err := guard.Cause(context.Canceled)
	if !errors.Is(err, provider.ErrStalled) {
		t.Fatalf("Cause(context.Canceled) = %v, want an error matching ErrStalled", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Error("the stall error still reports itself as a cancellation — a session.error would read as a shutdown")
	}
}

// Every sign of life buys another budget: a stream that keeps delivering must
// never be killed, however long the whole turn runs.
func TestStallGuardProgressPostponesTheTrip(t *testing.T) {
	const budget = 150 * time.Millisecond
	gctx, guard := provider.NewStallGuard(context.Background(), budget)
	defer guard.Stop()

	// Four budgets' worth of wall clock, kept alive at a third of the budget.
	deadline := time.Now().Add(4 * budget)
	for time.Now().Before(deadline) {
		guard.Progress()
		time.Sleep(budget / 3)
		if gctx.Err() != nil {
			t.Fatalf("a stream reporting progress every %s was cancelled inside a %s budget", budget/3, budget)
		}
	}
	// Silence now, and the same guard must still trip: rescheduling must not
	// disarm it.
	waitDone(t, gctx, 3*time.Second)
	if err := guard.Cause(nil); !errors.Is(err, provider.ErrStalled) {
		t.Errorf("Cause(nil) = %v, want an error matching ErrStalled once progress stopped", err)
	}
}

func TestStallGuardStopEndsTheWatch(t *testing.T) {
	gctx, guard := provider.NewStallGuard(context.Background(), 50*time.Millisecond)
	guard.Stop()
	guard.Stop() // idempotent: a stream may be closed twice

	waitDone(t, gctx, time.Second) // Stop cancels the context it handed out
	upstream := errors.New("upstream said no")
	if err := guard.Cause(upstream); !errors.Is(err, upstream) {
		t.Errorf("Cause(err) = %v, want the upstream error: a stopped guard never stalled", err)
	}
}

// A parent that cancels — a lost lease, a shutdown — is not the endpoint going
// silent, and must not be reported as one.
func TestStallGuardParentCancellationIsNotAStall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	gctx, guard := provider.NewStallGuard(ctx, time.Minute)
	defer guard.Stop()

	cancel()
	waitDone(t, gctx, time.Second)
	time.Sleep(50 * time.Millisecond) // let the watcher observe the cancellation
	if err := guard.Cause(context.Canceled); !errors.Is(err, context.Canceled) {
		t.Errorf("Cause = %v, want the cancellation passed through unchanged", err)
	}
	if errors.Is(guard.Cause(context.Canceled), provider.ErrStalled) {
		t.Error("a caller's own cancellation was reported as an endpoint stall")
	}
}

// trickle delivers one byte every gap, forever — an endpoint that is quiet but
// alive, the shape a keepalive stream has at the socket.
type trickle struct{ gap time.Duration }

func (t trickle) Read(p []byte) (int, error) {
	time.Sleep(t.gap)
	p[0] = '.'
	return 1, nil
}
func (t trickle) Close() error { return nil }

// The wrapper is what feeds the guard: without it nothing above the socket sees
// a keepalive, because a ping is dropped by the SDK's decoder and an SSE comment
// is not an event at all.
func TestProgressBodyKeepsAGuardAliveWhileBytesArrive(t *testing.T) {
	const budget = 150 * time.Millisecond
	gctx, guard := provider.NewStallGuard(context.Background(), budget)
	defer guard.Stop()

	body := provider.ProgressBody(gctx, trickle{gap: budget / 3})
	buf := make([]byte, 1)
	for deadline := time.Now().Add(4 * budget); time.Now().Before(deadline); {
		if _, err := body.Read(buf); err != nil {
			t.Fatalf("read: %v", err)
		}
		if gctx.Err() != nil {
			t.Fatalf("a body delivering a byte every %s was cancelled inside a %s budget", budget/3, budget)
		}
	}
	// Reading stops, and the same guard still trips.
	waitDone(t, gctx, 3*time.Second)
	if err := guard.Cause(nil); !errors.Is(err, provider.ErrStalled) {
		t.Errorf("Cause(nil) = %v, want an error matching ErrStalled once the bytes stopped", err)
	}
}

// A context carrying no guard leaves the body untouched, so the wrapper can sit
// in a middleware that also serves requests nobody is guarding.
func TestProgressBodyWithoutAGuardIsPassthrough(t *testing.T) {
	body := io.NopCloser(strings.NewReader("hi"))
	if got := provider.ProgressBody(context.Background(), body); got != body {
		t.Error("ProgressBody wrapped a body although the context carries no guard")
	}
	if got := provider.ProgressBody(context.Background(), nil); got != nil {
		t.Errorf("ProgressBody(ctx, nil) = %v, want nil", got)
	}
}

// Zero means "take the default", not "trip at once" — a Config that leaves
// stall_timeout unset must still run turns.
func TestStallGuardZeroTakesTheDefault(t *testing.T) {
	if provider.DefaultStallTimeout < time.Minute {
		t.Fatalf("DefaultStallTimeout = %s: too tight to be a safe default for a queued endpoint", provider.DefaultStallTimeout)
	}
	gctx, guard := provider.NewStallGuard(context.Background(), 0)
	defer guard.Stop()

	time.Sleep(200 * time.Millisecond)
	if gctx.Err() != nil {
		t.Fatalf("an unset stall timeout cancelled the turn after 200ms: %v", gctx.Err())
	}
	if err := guard.Cause(nil); err != nil {
		t.Errorf("Cause(nil) = %v, want nil: nothing has stalled", err)
	}
}
