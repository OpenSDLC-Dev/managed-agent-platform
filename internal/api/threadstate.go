package api

import (
	"context"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
	"github.com/jackc/pgx/v5"
)

// What the send triggers and the work API's re-arm need of the thread
// substrate (plan 35 decision 5): the live threads with their own statuses,
// and the exec kind the session's runnable calls call for.

// threadState is one live thread as the send triggers see it: its id (empty
// for the primary) and its own status.
type threadState struct {
	id     domain.ID
	status string
}

// liveThreads lists a session's unarchived threads with their statuses, the
// primary first (its id empty) then children in creation order — the units
// the send triggers decide for. A session from before the thread resource,
// with no rows, is its primary alone at the session's status. The session
// row is the caller's to lock.
func liveThreads(ctx context.Context, tx pgx.Tx, sessionID, sessionStatus string) ([]threadState, error) {
	rows, err := tx.Query(ctx,
		`SELECT CASE WHEN parent_thread_id IS NULL THEN '' ELSE id END, status
		   FROM session_threads WHERE session_id = $1 AND archived_at IS NULL
		  ORDER BY parent_thread_id IS NOT NULL, created_at, id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []threadState
	for rows.Next() {
		var th threadState
		var id string
		if err := rows.Scan(&id, &th.status); err != nil {
			return nil, err
		}
		th.id = domain.ID(id)
		out = append(out, th)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		out = []threadState{{status: sessionStatus}}
	}
	return out, nil
}

// execKindFor picks the exec item a resume schedules for the session's
// runnable platform calls — events.RunnableExecClass's precedence, named as
// a queue kind; "" when nothing is runnable. answered are the ids the
// caller's batch already answers, allowed the ones its confirmations release.
func execKindFor(ctx context.Context, q events.Querier, sessionID domain.ID, answered, allowed []string) (queue.Kind, error) {
	class, err := events.RunnableExecClass(ctx, q, sessionID, answered, allowed, toolset.IsWebTool)
	if err != nil {
		return "", err
	}
	return execKinds[class], nil
}

// execKinds names each exec family's queue kind; ExecNone maps to "".
var execKinds = map[events.ExecClass]queue.Kind{
	events.ExecMCP: queue.MCPExec, events.ExecWeb: queue.WebExec, events.ExecTool: queue.ToolExec,
}

// nullableThread binds a thread id: NULL for the primary.
func nullableThread(threadID domain.ID) *string {
	if threadID == "" {
		return nil
	}
	s := threadID.String()
	return &s
}
