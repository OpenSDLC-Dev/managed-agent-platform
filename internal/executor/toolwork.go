package executor

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
)

// toolUse is one platform tool call the executor must run: the tool-use event's
// id (which scopes the bash shell's per-call state and which the result
// references), the tool name, its input, and the thread it was written on
// (empty for the primary) with whether it was cross-posted — the result
// answering it is written the same way (plan 35 decision 2).
type toolUse struct {
	id          domain.ID
	name        string
	input       json.RawMessage
	thread      domain.ID
	crossPosted bool
}

// runnableToolUses returns the session's runnable agent.tool_use events,
// oldest first across every thread — the work this item must run. Runnable
// (plan 35 decision 5), not merely unanswered: evaluated_permission allow, or
// ask with an allow confirmation recorded — so a sibling thread's gated
// command never runs in the shared sandbox on the strength of this thread's
// allow-policy call. It reads the committed log (custom tool uses are
// client-executed and never appear as agent.tool_use; mcp_tool_use waits for
// the MCP client), so a reclaim re-runs only what is still outstanding. An
// agent.tool_use is answered by either an agent.tool_result (this executor)
// or a user.tool_result (a self_hosted BYOC worker) — both reference it by
// tool_use_id — so both count, matching the canonical answered-set the
// control plane uses; counting only agent.tool_result would re-run a tool a
// worker already answered.
func (e *Executor) runnableToolUses(ctx context.Context, sid domain.ID) ([]toolUse, error) {
	uses, err := events.RunnableToolUses(ctx, e.pool, sid, domain.EventAgentToolUse)
	if err != nil {
		return nil, fmt.Errorf("list tool uses: %w", err)
	}
	var out []toolUse
	for _, u := range uses {
		var body struct {
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(u.Payload, &body); err != nil {
			return nil, fmt.Errorf("tool use %s: %w", u.ID, err)
		}
		out = append(out, toolUse{id: u.ID, name: body.Name, input: body.Input,
			thread: u.ThreadID, crossPosted: u.CrossPosted})
	}
	return out, nil
}
