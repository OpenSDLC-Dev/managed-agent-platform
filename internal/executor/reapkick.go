package executor

// The consumer half of the session-delete reap kick (#354, plan 48). The
// control plane cannot destroy a sandbox — it holds no provider — so what a
// session's end publishes is a bare wake, and this is what hears it: one
// connection on LISTEN, held outside the pool so it stays clear of the
// nested-acquisition budget cmd/executor's pool floor guards, turning every
// notification into a request for the ordinary sweep.
//
// Nothing here is load-bearing for correctness. The durable statement of what
// is owed is the deleted_sessions tombstone, which the sweep re-reads; a kick
// that is lost, duplicated, or delivered to an executor owning nothing of that
// session costs at most one interval, one redundant listing, or nothing at all.
// That is why this may fail all it likes without ever faulting Run.

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
)

// reapKickBackoff paces reconnection. Longer than the broker's 200ms because
// the failure this actually meets is a standing one — a pooler that never
// passes LISTEN through — where the cost of retrying is a warning line rather
// than a missed event, and the ticker is already covering the gap. A var so
// the recovery tests need not spend it in real time; never written in
// production.
var reapKickBackoff = 5 * time.Second

// listenReapKicks holds one dedicated connection on LISTEN and wakes the reap
// loop for each notification, reconnecting until the context ends. An empty
// ReapKickDSN leaves it unstarted, which is the pre-kick behaviour rather than
// a fault.
func (e *Executor) listenReapKicks(ctx context.Context) {
	if e.cfg.ReapKickDSN == "" {
		return
	}
	// Said once, on the first success rather than every reconnect: behind a
	// transaction pooler LISTEN never establishes at all, and a kick that is
	// silently inert looks exactly like one that is merely slow.
	announced := false
	for ctx.Err() == nil {
		conn, err := pgx.Connect(ctx, e.cfg.ReapKickDSN)
		if err != nil {
			e.reapKickWait(ctx, "connect", err)
			continue
		}
		if _, err := conn.Exec(ctx, "LISTEN "+events.ChannelReapKick); err != nil {
			_ = conn.Close(context.Background())
			e.reapKickWait(ctx, "listen", err)
			continue
		}
		if !announced {
			announced = true
			slog.InfoContext(ctx, "sandbox reap kick listening", "channel", events.ChannelReapKick)
		}
		// Sweep once on every establish, not only on the first. A kick fired
		// while this connection was down was not queued anywhere — LISTEN
		// delivers only to connections already listening — so the reconnect
		// is the moment to look for whatever it would have said.
		e.wake()
		for {
			if _, err := conn.WaitForNotification(ctx); err != nil {
				break
			}
			e.wake()
		}
		// The wait ended by cancellation or a broken connection; either way
		// this connection is finished with.
		_ = conn.Close(context.Background())
		if ctx.Err() == nil {
			e.reapKickWait(ctx, "wait", nil)
		}
	}
}

// reapKickWait reports a listener that is not currently covering, and paces the
// retry. The message names the consequence rather than the mechanism, because
// the operator reading it wants to know what it costs: teardown falls back to
// the interval, which is where it was before the kick existed.
func (e *Executor) reapKickWait(ctx context.Context, stage string, err error) {
	if ctx.Err() != nil {
		return
	}
	slog.WarnContext(ctx, "sandbox reap kick not listening; teardown waits for the reap interval",
		"stage", stage, "retry_in", reapKickBackoff, "interval", e.cfg.ReapInterval, "error", err)
	select {
	case <-ctx.Done():
	case <-time.After(reapKickBackoff):
	}
}
