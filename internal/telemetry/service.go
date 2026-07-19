package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// flushTimeout bounds the export of whatever telemetry is still buffered when
// the process is on its way out — including the fatal-exit log Run just
// emitted, which is the record an operator most wants and the last one queued.
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
// is what makes that ordering unavailable to a caller.
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
