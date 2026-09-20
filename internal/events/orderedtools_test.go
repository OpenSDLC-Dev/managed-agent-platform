package events_test

import (
	"context"
	"errors"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
	"github.com/jackc/pgx/v5/pgconn"
)

// MCP names belong to their server, even when a bare name matches a platform
// delegation or web tool. The event family chooses the execution lane.
func TestMCPToolNamesDoNotSelectBuiltinExecution(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	for _, name := range []string{"create_agent", "web_fetch", "bash"} {
		t.Run(name, func(t *testing.T) {
			sid := newSession(t, pool)
			id := appendEvent(t, log, sid, "", domain.EventAgentMCPToolUse,
				`{"name":"`+name+`","mcp_server_name":"service","input":{},"evaluated_permission":"allow"}`)
			class, err := events.RunnableExecClass(ctx, pool, sid, nil, nil, toolset.IsWebTool, toolset.IsDelegationTool)
			if err != nil || class != events.ExecMCP {
				t.Fatalf("MCP %q routed to %v: %v", name, class, err)
			}
			calls, err := events.RunnableToolUses(ctx, pool, sid, domain.EventAgentMCPToolUse, toolset.IsWebTool)
			if err != nil || len(calls) != 1 || calls[0].ID != id {
				t.Fatalf("MCP %q unavailable to MCP executor: %+v %v", name, calls, err)
			}
		})
	}
}

// An aborted transaction is an unavailable flow, never an empty settled flow.
// Every caller must abort its status/queue mutation when the shared reader fails.
func TestOrderedToolsFailClosedWhenTransactionAborts(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sid := newSession(t, pool)
	log := events.NewLog(pool)
	ask(t, log, sid)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT 1/0"); err == nil {
		t.Fatal("failed to abort transaction")
	}
	platform := func(string) bool { return false }
	checks := []struct {
		name string
		run  func() error
	}{
		{"read flow", func() error { _, err := events.ThreadToolFlow(ctx, tx, sid, "", platform); return err }},
		{"read approvals", func() error { _, err := events.PendingThreadApprovals(ctx, tx, sid, ""); return err }},
		{"read live threads", func() error { _, err := events.ToolFlowThreads(ctx, tx, sid); return err }},
		{"process tools", func() error { _, err := log.AdvanceThreadTools(ctx, tx, sid, "", platform); return err }},
		{"settle status", func() error { _, err := log.SettleToolFlow(ctx, tx, sid, "", events.ToolFlow{}); return err }},
		{"snapshot approval waits", func() error { _, err := events.PendingApprovalThreads(ctx, tx, sid); return err }},
		{"measure approval wait", func() error { _, err := events.ClearedApprovalWait(ctx, tx, sid, ""); return err }},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			err := check.run()
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "25P02" {
				t.Fatalf("lost transaction failure: %v", err)
			}
		})
	}
}
