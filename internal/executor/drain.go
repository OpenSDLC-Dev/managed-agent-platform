package executor

import (
	"context"
	"encoding/json"
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
// on the lock commits nothing) — the chain to another kind still runs, since
// only that kind's driver can answer those calls — unless nothing runnable
// of any kind remains, when it has nothing left to do and completes.
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
	if kind == "" {
		return e.queue.Complete(ctx, tx, item)
	}
	if kind != own {
		if _, err := e.queue.Enqueue(ctx, tx, item.EnvironmentID, item.SessionID, kind); err != nil {
			return err
		}
	}
	switch {
	case leaveLive:
		// The reclaim's pass answers what it can; its complete settlement
		// chains whatever else is left.
		return e.queue.Assert(ctx, tx, item)
	case kind == own:
		return e.queue.Requeue(ctx, tx, item)
	default:
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
// caller — and once more, under the lock, by commitResults, for the answer
// that lands between the last beat and the return). The check is
// best-effort: a failed read is one missed beat, never a cancelled call; the
// read runs under the watch's own context, so stopping the watch never waits
// on a read in flight.
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
				if ok, err := events.Answered(wctx, e.pool, sid, useID); err == nil && ok {
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

// commitResults appends a driver's results and runs its settlement in one
// transaction under the session row lock, dropping first any result whose
// call is already answered on the log — a thread-scoped interrupt's own
// result committed between the watch's last beat and the call's return
// (decision 9), or a sibling claimant's: a late result is dropped under the
// lock, never appended as a second answer every replay would then carry. An
// event riding beside a dropped result — the MCP lane's failure explaining
// it — goes with it. One query for the whole batch, so the lock is held for
// two round trips however large it is. An emptied batch still settles.
func (e *Executor) commitResults(ctx context.Context, sid domain.ID, results []events.NewEvent, settle func(context.Context, pgx.Tx) error) error {
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, sid.String()); err != nil {
		return err
	}
	refs := make([]domain.ID, len(results))
	for i, r := range results {
		refs[i] = resultRef(r)
	}
	answered, err := events.AnsweredSet(ctx, tx, sid, refs)
	if err != nil {
		return err
	}
	kept := results[:0:0]
	var dropped domain.ID
	for i, r := range results {
		switch {
		case refs[i] != "" && answered[refs[i]]:
			dropped = refs[i]
		case refs[i] == "" && dropped != "" && r.Type == domain.EventSessionError:
			// The failure beside the result just dropped.
		default:
			dropped = ""
			kept = append(kept, r)
		}
	}
	if _, err := e.log.AppendInTx(ctx, tx, sid, kept, events.AppendOptions{Then: settle}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// resultRef is the tool-use id a driver's result answers (agent.tool_result's
// tool_use_id, agent.mcp_tool_result's mcp_tool_use_id); empty for an event
// that answers none (the MCP lane's session.error beside a result).
func resultRef(r events.NewEvent) domain.ID {
	var ref struct {
		ToolUseID    string `json:"tool_use_id"`
		MCPToolUseID string `json:"mcp_tool_use_id"`
	}
	_ = json.Unmarshal(r.Payload, &ref)
	if ref.MCPToolUseID != "" {
		return domain.ID(ref.MCPToolUseID)
	}
	return domain.ID(ref.ToolUseID)
}
