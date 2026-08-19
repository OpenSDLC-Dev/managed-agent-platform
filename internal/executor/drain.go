package executor

import (
	"context"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
	"github.com/jackc/pgx/v5"
)

// What every exec driver does after its pass, now that a session's calls
// belong to sibling threads (plan 35 decision 5): the exec item stays
// session-keyed — one shared sandbox, one item covers every thread's backlog
// — but the drain is per thread, and so is the wake.

// execKinds names each runnable exec family's queue kind; ExecNone maps to
// the zero Kind.
var execKinds = map[events.ExecClass]queue.Kind{
	events.ExecMCP: queue.MCPExec, events.ExecWeb: queue.WebExec, events.ExecTool: queue.ToolExec,
}

// settleDrain schedules what follows a driver's pass, inside the settlement's
// transaction under the session lock, after the pass's results are appended.
// First the wake, per thread: every running thread whose own calls are all
// answered gets its model_turn (Enqueue dedupes against a live one). Then the
// chain: the exec kind the session's runnable calls still need — mcp_exec
// first, then web_exec, then tool_exec, the precedence every settlement
// shares — handed this very item back when it is the driver's own kind (a
// call committed under the live item by a sibling's settlement is found by
// the re-scan, never stranded: Enqueue is keyed over the live states, so a
// fresh item of this kind would drop on conflict), enqueued and the item
// completed when it is another's, the item completed when nothing is left.
// leaveLive is the fault path: the item stays leased for the reclaim to retry
// what it could not answer (the lease asserted, so a claim lost while blocked
// on the lock commits nothing) — unless nothing runnable of its own kind
// remains, when it has nothing left to do and completes.
func (e *Executor) settleDrain(ctx context.Context, tx pgx.Tx, item *queue.Item, own queue.Kind, leaveLive bool) error {
	threads, err := events.ResumableThreads(ctx, tx, item.SessionID)
	if err != nil {
		return err
	}
	for _, tid := range threads {
		if _, err := e.queue.EnqueueThread(ctx, tx, item.EnvironmentID, item.SessionID, tid, queue.ModelTurn); err != nil {
			return err
		}
	}
	class, err := events.RunnableExecClass(ctx, tx, item.SessionID, nil, nil, toolset.IsWebTool)
	if err != nil {
		return err
	}
	kind := execKinds[class]
	switch {
	case kind == "":
		return e.queue.Complete(ctx, tx, item)
	case leaveLive:
		// The reclaim's pass answers what it can; its complete settlement
		// chains whatever else is left.
		return e.queue.Assert(ctx, tx, item)
	case kind == own:
		return e.queue.Requeue(ctx, tx, item)
	default:
		if _, err := e.queue.Enqueue(ctx, tx, item.EnvironmentID, item.SessionID, kind); err != nil {
			return err
		}
		return e.queue.Complete(ctx, tx, item)
	}
}

// answeredWatch is the drivers' answered-means-cancelled check for a call in
// flight (decision 9): a thread-scoped interrupt answers its thread's calls
// itself and never stops the shared exec item, so the item's lease keeper
// cannot tell the driver — this does, on the keeper's own beat. It returns a
// context cancelled once a result references the use, and a stop func the
// caller runs when the call returns, which reports whether the watch was
// what cancelled it (a late result for such a call is dropped by the
// caller). The check is best-effort: a failed read is one missed beat, never
// a cancelled call.
func (e *Executor) answeredWatch(ctx context.Context, sid, useID domain.ID, beat time.Duration) (context.Context, func() bool) {
	wctx, cancel := context.WithCancel(ctx)
	if beat <= 0 {
		beat = time.Second
	}
	done := make(chan struct{})
	answered := false
	go func() {
		defer close(done)
		t := time.NewTicker(beat)
		defer t.Stop()
		for {
			select {
			case <-wctx.Done():
				return
			case <-t.C:
				if ok, err := events.Answered(ctx, e.pool, sid, useID); err == nil && ok {
					answered = true
					cancel()
					return
				}
			}
		}
	}()
	return wctx, func() bool {
		cancel()
		<-done
		return answered
	}
}

// nullableThread binds a thread id: NULL for the primary.
func nullableThread(threadID domain.ID) *string {
	if threadID == "" {
		return nil
	}
	s := threadID.String()
	return &s
}
