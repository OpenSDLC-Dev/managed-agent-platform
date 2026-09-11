package executor

// The consumer half of the session-delete reap kick (#354, plan 48). The
// control plane cannot destroy a sandbox — it holds no provider — so what a
// session's end publishes is a bare wake, and this is what hears it: one
// connection on LISTEN, held outside the pool so it stays clear of the
// nested-acquisition budget cmd/executor's pool floor guards, turning every
// notification into a request for the ordinary sweep.
//
// Nothing here is load-bearing for correctness. The durable statement of what
// is owed is the session row itself — the deleted_sessions tombstone for a
// delete, archived_at for an archive — which the sweep re-reads; a kick that
// is lost, duplicated, or delivered to an executor owning nothing of that
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

// reapKickDialBudget bounds establishing the listener. Without it a server
// that answers the TCP handshake and then stalls — a half-open path through a
// firewall, a pooler with no backend to give — parks the goroutine on a dial
// that never returns and never reaches the backoff below, which is the one
// shape of "not listening" that also never says so.
const reapKickDialBudget = 10 * time.Second

// listenReapKicks holds one dedicated connection on LISTEN and wakes the reap
// loop for each notification, reconnecting until the context ends. A nil
// ReapKickConn leaves it unstarted, which is the pre-kick behaviour rather
// than a fault.
func (e *Executor) listenReapKicks(ctx context.Context) {
	if e.cfg.ReapKickConn == nil {
		return
	}
	// Said once, on the first success rather than every reconnect: a kick that
	// is silently inert looks exactly like one that is merely slow, and the
	// deployments where it is inert — a pooler that refuses LISTEN, or admits
	// the statement while multiplexing the backend that would deliver it —
	// are not ones this process can detect for itself.
	announced := false
	for ctx.Err() == nil {
		conn, err := e.dialReapKicks(ctx)
		if err != nil {
			e.reapKickWait(ctx, "connect", err)
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
		var waitErr error
		for {
			if _, waitErr = conn.WaitForNotification(ctx); waitErr != nil {
				break
			}
			e.wake()
		}
		// The wait ended by cancellation or a broken connection; either way
		// this connection is finished with. Closed under a deadline rather
		// than context.Background(), which pgconn compares by identity and
		// answers by installing no watcher at all — leaving the Terminate
		// flush to block for as long as a blackholed socket cares to, with
		// Run's shutdown waiting behind it.
		closeReapKickConn(conn)
		if ctx.Err() == nil {
			e.reapKickWait(ctx, "wait", waitErr)
		}
	}
}

// dialReapKicks opens the listening connection and puts it on LISTEN, both
// under one budget, and hands back nothing that is not already listening.
func (e *Executor) dialReapKicks(ctx context.Context) (*pgx.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, reapKickDialBudget)
	defer cancel()
	conn, err := pgx.ConnectConfig(dctx, e.cfg.ReapKickConn.Copy())
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(dctx, "LISTEN "+events.ChannelReapKick); err != nil {
		closeReapKickConn(conn)
		return nil, err
	}
	return conn, nil
}

// closeReapKickConn ends the connection under a deadline. Not the caller's
// context: a close is reached on the shutdown path, where that context is
// already cancelled and pgconn would abandon the Terminate immediately. The
// dial budget again, for want of a second number to justify — both bound one
// round trip to the same server. The error is the one pgx itself discards
// here, in the libpq behaviour Close documents.
func closeReapKickConn(conn *pgx.Conn) {
	cctx, cancel := context.WithTimeout(context.Background(), reapKickDialBudget)
	defer cancel()
	_ = conn.Close(cctx)
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
