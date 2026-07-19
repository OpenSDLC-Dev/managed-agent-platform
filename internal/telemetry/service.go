package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// flushTimeout is the deadline handed to the exit flush. It is the deadline the
// three provider shutdowns share, not a bound on how long exit takes: an export
// already in flight on a background context can outlive it, so a process aimed
// at an unreachable collector takes appreciably longer than this to die. What
// the deadline is for is making sure the flush ends at all.
const flushTimeout = 5 * time.Second

// Run initializes telemetry from cfg, runs body as the process's main body, and
// flushes on the way out. It reports whether the process should exit zero: a
// nil error and a context.Canceled (how a signal-stopped process reports its
// own clean shutdown) are clean; anything else is logged as fatal and reports
// false.
//
// The fatal log belongs here, in the same sequence as the flush, rather than in
// each main() after this returns. The telemetry shutdown stops the log
// processor, and sdk/log's BatchProcessor drops records once stopped — silently,
// since the fan-out's console half still prints. A binary that logged its own
// exit after Run therefore reached stderr and never the collector, leaving the
// one line that explains why the process died as the only one missing from the
// backend an operator was looking at. Owning init, body, log and flush together
// is what keeps that ordering in one place a test can reach; Init stays exported
// for the suite, so this is the shape to use rather than a shape it enforces.
//
// A body that panics is outside all of this: the fatal log below never runs, and
// the panic reaches stderr through the runtime. The deferred flush still ships
// whatever was already queued, and a panic in a goroutine the body started takes
// the process without reaching even that. #93 is about the errors a body
// returns.
//
// Init runs before body rather than partway through it for the same reason:
// every failure a service can report — a missing DATABASE_URL, a sandbox
// backend that will not construct — is then already inside the bridge's
// lifetime. Only Init's own failure escapes, and it can only reach stderr,
// there being no bridge yet to carry it.
func Run(ctx context.Context, cfg Config, body func(context.Context) error) (ok bool) {
	shutdown, err := Init(ctx, cfg)
	if err != nil {
		slog.ErrorContext(ctx, cfg.ServiceName+" exiting", "err", err)
		return false
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), flushTimeout)
		defer cancel()
		_ = shutdown(flushCtx)
	}()

	if err := body(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.ErrorContext(ctx, cfg.ServiceName+" exiting", "err", err)
		return false
	}
	return true
}
