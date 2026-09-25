package events

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// ToolFlow separates receipt (which prevents execution and duplicate answers)
// from processing (which releases a thread). Calls advance in model/log order;
// an out-of-order reply remains durable without advancing past an earlier call.
type ToolFlow struct {
	Pending          []domain.ID
	Unsettled        bool
	PlatformRunnable bool
}

// ToolWaitIDs advertises every external input the just-emitted turn will need,
// including calls beyond its current processing position.
func ToolWaitIDs(batch []NewEvent, kind string, platformOwned func(string) bool) []domain.ID {
	var ids []domain.ID
	for _, ev := range batch {
		if ev.Type != domain.EventAgentCustomToolUse && ev.Type != domain.EventAgentToolUse && ev.Type != domain.EventAgentMCPToolUse {
			continue
		}
		var p struct {
			Name       string `json:"name"`
			Permission string `json:"evaluated_permission"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		if p.Permission == string(domain.EvalPermDeny) {
			continue
		}
		c := orderedCall{typ: ev.Type, name: p.Name}
		if ev.Type == domain.EventAgentCustomToolUse || p.Permission == string(domain.EvalPermAsk) || workerCall(c, kind, platformOwned) {
			ids = append(ids, ev.ID)
		}
	}
	return ids
}

type orderedCall struct {
	id                       domain.ID
	typ                      domain.EventType
	payload                  json.RawMessage
	name, permission         string
	resultID, confirmationID string
	confirmation             json.RawMessage
	confirmed, resolved      bool
}

func threadCalls(ctx context.Context, q Querier, sid, tid domain.ID) ([]orderedCall, string, error) {
	var kind string
	if err := q.QueryRow(ctx, `SELECT e.kind FROM sessions s JOIN environments e ON e.id=s.environment_id WHERE s.id=$1`, sid.String()).Scan(&kind); err != nil {
		return nil, "", err
	}
	rows, err := q.Query(ctx, `SELECT tu.id,tu.type,tu.payload,
 COALESCE(r.id,''),COALESCE(c.id,''),c.payload,COALESCE(c.processed_at IS NOT NULL,false)
 FROM events tu
 LEFT JOIN LATERAL (SELECT id,processed_at FROM events r WHERE r.session_id=tu.session_id
   AND r.type=ANY($3) AND COALESCE(r.payload->>'tool_use_id',r.payload->>'custom_tool_use_id',r.payload->>'mcp_tool_use_id')=tu.id LIMIT 1) r ON true
 LEFT JOIN LATERAL (SELECT id,payload,processed_at FROM events c WHERE c.session_id=tu.session_id
   AND c.type='user.tool_confirmation' AND c.payload->>'tool_use_id'=tu.id LIMIT 1) c ON true
 WHERE tu.session_id=$1 AND tu.type=ANY($2) AND tu.thread_id IS NOT DISTINCT FROM $4
   AND r.processed_at IS NULL ORDER BY tu.seq`, sid.String(), toolUseTypes, toolResultTypes, nullableID(tid))
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var calls []orderedCall
	for rows.Next() {
		var c orderedCall
		if err := rows.Scan(&c.id, &c.typ, &c.payload, &c.resultID, &c.confirmationID, &c.confirmation, &c.confirmed); err != nil {
			return nil, "", err
		}
		var p struct {
			Name       string `json:"name"`
			Permission string `json:"evaluated_permission"`
		}
		if err := json.Unmarshal(c.payload, &p); err != nil {
			return nil, "", err
		}
		c.name, c.permission = p.Name, p.Permission
		calls = append(calls, c)
	}
	return calls, kind, rows.Err()
}

func workerCall(c orderedCall, kind string, platformOwned func(string) bool) bool {
	return kind == string(domain.EnvSelfHosted) && c.typ == domain.EventAgentToolUse && !platformOwned(c.name)
}

func summarizeTools(calls []orderedCall, kind string, platformOwned func(string) bool) ToolFlow {
	var out ToolFlow
	for _, c := range calls {
		if c.resolved {
			continue
		}
		external := c.typ == domain.EventAgentCustomToolUse || workerCall(c, kind, platformOwned)
		gated := c.permission == string(domain.EvalPermAsk) && !c.confirmed
		if external || gated {
			out.Pending = append(out.Pending, c.id)
		}
		if !out.Unsettled {
			out.PlatformRunnable = !external && !gated && c.resultID == ""
		}
		out.Unsettled = true
	}
	return out
}

// ThreadToolFlow is a read-only view, including accepted but unprocessed input.
func ThreadToolFlow(ctx context.Context, q Querier, sid, tid domain.ID, platformOwned func(string) bool) (ToolFlow, error) {
	calls, kind, err := threadCalls(ctx, q, sid, tid)
	if err != nil {
		return ToolFlow{}, err
	}
	return summarizeTools(calls, kind, platformOwned), nil
}

// PendingThreadApprovals counts approvals not yet processed, independently of
// an external worker's result wait after authorization.
func PendingThreadApprovals(ctx context.Context, q Querier, sid, tid domain.ID) (bool, error) {
	calls, _, err := threadCalls(ctx, q, sid, tid)
	if err != nil {
		return false, err
	}
	for _, c := range calls {
		if c.permission == string(domain.EvalPermAsk) && !c.confirmed {
			return true, nil
		}
	}
	return false, nil
}

// AdvanceThreadTools consumes the ready prefix while holding the session row
// lock. It never executes a tool. A processed allow releases execution, not the
// result wait; a denial produces its ordinary error result before advancing.
func (l *Log) AdvanceThreadTools(ctx context.Context, tx pgx.Tx, sid, tid domain.ID, platformOwned func(string) bool) (ToolFlow, error) {
	calls, kind, err := threadCalls(ctx, tx, sid, tid)
	if err != nil {
		return ToolFlow{}, err
	}
	stamp := func(id string) error {
		_, err := tx.Exec(ctx, `UPDATE events SET processed_at=clock_timestamp() WHERE session_id=$1 AND id=$2 AND processed_at IS NULL`, sid.String(), id)
		return err
	}
	err = walkReady(calls, func(c *orderedCall) (bool, error) {
		if c.resultID != "" {
			if c.confirmationID != "" {
				if err := stamp(c.confirmationID); err != nil {
					return false, err
				}
			}
			if err := stamp(c.resultID); err != nil {
				return false, err
			}
			c.resolved = true
			return true, nil
		}
		if err := stamp(c.confirmationID); err != nil {
			return false, err
		}
		c.confirmed = true
		results, _, err := DenialResults(ctx, tx, sid, []NewEvent{{Type: domain.EventUserToolConfirm, Payload: c.confirmation}})
		if err != nil || len(results) == 0 {
			return false, err
		}
		if _, err := l.AppendInTx(ctx, tx, sid, results, AppendOptions{}); err != nil {
			return false, err
		}
		c.resolved = true
		return true, nil
	})
	if err != nil {
		return ToolFlow{}, err
	}
	return summarizeTools(calls, kind, platformOwned), nil
}

// walkReady walks a thread's calls in log order as far as processing can go:
// it hands consume each call that has its result, or an ask gate that has its
// confirmation, and stops at the first call with neither. consume reports
// whether the call is now resolved — a result or a denial resolves it, an
// allow only releases it to run — and the walk goes on only past a resolved
// call. AdvanceThreadTools stamps what it is handed; PendingAnswers only
// notes it, so the two agree on where processing stops.
func walkReady(calls []orderedCall, consume func(c *orderedCall) (bool, error)) error {
	for i := range calls {
		c := &calls[i]
		if c.resultID == "" && (c.permission != string(domain.EvalPermAsk) || c.confirmationID == "") {
			return nil
		}
		resolved, err := consume(c)
		if err != nil || !resolved {
			return err
		}
	}
	return nil
}

// PendingAnswers is which of a send's answers — among posted, its
// user.tool_result, user.custom_tool_result and user.tool_confirmation events,
// routed to their threads — the send's own settlement leaves unprocessed. It is
// read before they are appended, so the send can lay each out where it
// belongs. advanced names the threads whose tools the settlement advances; on
// those, the posted answers and synthesized (the results an interrupt of the
// same send writes) are read as though already on the log and walked as
// AdvanceThreadTools will walk them, so an answer past the first call that
// must still wait is pending. Every answer on a thread the settlement does not
// advance is pending.
func PendingAnswers(ctx context.Context, q Querier, sid domain.ID, posted, synthesized []NewEvent, advanced map[domain.ID]bool) (map[domain.ID]bool, error) {
	pending := map[domain.ID]bool{}
	threads := map[domain.ID]bool{}
	for _, ev := range posted {
		switch ev.Type {
		case domain.EventUserToolResult, domain.EventUserCustomToolRes, domain.EventUserToolConfirm:
			pending[ev.ID] = true
			threads[ev.ThreadID] = advanced[ev.ThreadID]
		}
	}
	for tid, walked := range threads {
		if !walked {
			continue
		}
		calls, _, err := threadCalls(ctx, q, sid, tid)
		if err != nil {
			return nil, err
		}
		byID := make(map[string]*orderedCall, len(calls))
		for i := range calls {
			byID[calls[i].id.String()] = &calls[i]
		}
		for _, ev := range slices.Concat(posted, synthesized) {
			var ref struct {
				ToolUseID       string `json:"tool_use_id"`
				CustomToolUseID string `json:"custom_tool_use_id"`
				MCPToolUseID    string `json:"mcp_tool_use_id"`
			}
			_ = json.Unmarshal(ev.Payload, &ref)
			c := byID[cmp.Or(ref.ToolUseID, ref.CustomToolUseID, ref.MCPToolUseID)]
			switch {
			case c == nil:
			case ev.Type == domain.EventUserToolConfirm:
				if c.confirmationID == "" {
					c.confirmationID, c.confirmation = ev.ID.String(), ev.Payload
				}
			case c.resultID == "":
				c.resultID = ev.ID.String()
			}
		}
		if err := walkReady(calls, func(c *orderedCall) (bool, error) {
			delete(pending, domain.ID(c.confirmationID))
			delete(pending, domain.ID(c.resultID))
			return c.resultID != "" || denies(c.confirmation), nil
		}); err != nil {
			return nil, err
		}
	}
	return pending, nil
}

// denies reports whether a confirmation refuses its call, the one confirmation
// that answers the call as well as settling its gate (DenialResults).
func denies(confirmation json.RawMessage) bool {
	var c struct {
		Result string `json:"result"`
	}
	_ = json.Unmarshal(confirmation, &c)
	return c.Result == "deny"
}

// ToolFlowThreads selects threads whose tool state can advance. Idle end_turn
// and terminated threads are never revived by a shared executor settlement.
func ToolFlowThreads(ctx context.Context, q Querier, sid domain.ID) ([]domain.ID, error) {
	rows, err := q.Query(ctx, `SELECT CASE WHEN parent_thread_id IS NULL THEN '' ELSE id END
 FROM session_threads WHERE session_id=$1 AND archived_at IS NULL
 AND (status='running' OR (status='idle' AND stop_reason->>'type'='requires_action'))
 UNION ALL SELECT '' FROM sessions WHERE id=$1 AND archived_at IS NULL AND status='running'
 AND NOT EXISTS(SELECT 1 FROM session_threads WHERE session_id=$1)`, sid.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []domain.ID
	for rows.Next() {
		var id domain.ID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SettleToolFlow emits only a changed status or changed blocker set. In
// particular self-hosted approval keeps the same result blocker and emits no
// artificial running transition. A platform call may run before a later wait.
func (l *Log) SettleToolFlow(ctx context.Context, tx pgx.Tx, sid, tid domain.ID, flow ToolFlow) (*domain.SessionStatus, error) {
	want := domain.SessionRunning
	var stop *domain.StopReason
	if flow.Unsettled && !flow.PlatformRunnable {
		if len(flow.Pending) == 0 {
			return nil, fmt.Errorf("thread %s has unsettled tools without a wait or runnable call", tid)
		}
		want = domain.SessionIdle
		stop = &domain.StopReason{Type: domain.StopRequiresAction, EventIDs: flow.Pending}
	}
	var status string
	var oldStop []byte
	threadID := tid
	if threadID == "" {
		threadID = domain.PrimaryThreadID(sid)
	}
	err := tx.QueryRow(ctx, `SELECT status,stop_reason FROM session_threads WHERE session_id=$1 AND id=$2`, sid.String(), threadID.String()).Scan(&status, &oldStop)
	if err == pgx.ErrNoRows && tid == "" {
		err = tx.QueryRow(ctx, `SELECT status,NULL::jsonb FROM sessions WHERE id=$1`, sid.String()).Scan(&status, &oldStop)
	}
	if err != nil {
		return nil, err
	}
	if status == string(domain.SessionTerminated) || status == string(domain.SessionRescheduling) {
		return nil, nil
	}
	var previous domain.StopReason
	_ = json.Unmarshal(oldStop, &previous)
	same := status == string(want)
	if stop != nil {
		same = same && previous.Type == stop.Type && len(previous.EventIDs) == len(stop.EventIDs)
		if same {
			for i, id := range stop.EventIDs {
				if id != previous.EventIDs[i] {
					same = false
					break
				}
			}
		}
	}
	if same {
		return nil, nil
	}
	evs, moved, err := TransitionThread(ctx, tx, sid, ThreadTransition{ThreadID: tid, Status: want, Stop: stop, Reemit: want == domain.SessionIdle})
	if err != nil {
		return nil, err
	}
	if _, err := l.AppendInTx(ctx, tx, sid, evs, AppendOptions{}); err != nil {
		return nil, err
	}
	return moved, nil
}
