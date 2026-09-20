package brain

import (
	"context"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
	"github.com/jackc/pgx/v5"
)

func (b *Brain) enqueueRunnableTools(ctx context.Context, tx pgx.Tx, item *queue.Item) error {
	class, err := events.RunnableExecClass(ctx, tx, item.SessionID, nil, nil, toolset.IsWebTool, toolset.IsDelegationTool)
	if err != nil {
		return err
	}
	var kind queue.Kind
	switch class {
	case events.ExecMCP:
		kind = queue.MCPExec
	case events.ExecWeb:
		kind = queue.WebExec
	case events.ExecTool:
		kind = queue.ToolExec
	}
	if kind == "" {
		return nil
	}
	_, err = b.queue.Enqueue(ctx, tx, item.EnvironmentID, item.SessionID, kind)
	return err
}

func (b *Brain) settleTools(ctx context.Context, tx pgx.Tx, item *queue.Item) (*domain.SessionStatus, error) {
	flow, err := b.log.AdvanceThreadTools(ctx, tx, item.SessionID, item.ThreadID, func(name string) bool { return toolset.IsWebTool(name) || toolset.IsDelegationTool(name) })
	if err != nil {
		return nil, err
	}
	moved, err := b.log.SettleToolFlow(ctx, tx, item.SessionID, item.ThreadID, flow)
	if err != nil {
		return nil, err
	}
	if err := b.enqueueRunnableTools(ctx, tx, item); err != nil {
		return nil, err
	}
	if !flow.Unsettled {
		_, err = b.queue.EnqueueThread(ctx, tx, item.EnvironmentID, item.SessionID, item.ThreadID, queue.ModelTurn)
	}
	return moved, err
}
