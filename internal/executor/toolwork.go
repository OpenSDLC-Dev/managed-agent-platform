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
// references), the tool name, and its input.
type toolUse struct {
	id    domain.ID
	name  string
	input json.RawMessage
}

// unansweredToolUses returns the session's agent.tool_use events that still
// lack a result, oldest first — the work this item must run. It reads the
// committed log (custom tool uses are client-executed and never appear as
// agent.tool_use; mcp_tool_use waits for the MCP client), diffing the tool-use
// events against the results already on the log so a reclaim re-runs only what
// is still outstanding. An agent.tool_use is answered by either an
// agent.tool_result (this executor) or a user.tool_result (a self_hosted BYOC
// worker) — both reference it by tool_use_id — so both count, matching the
// canonical answered-set the control plane uses (events.HasUnansweredToolUse);
// counting only agent.tool_result would re-run a tool a worker already answered.
func (e *Executor) unansweredToolUses(ctx context.Context, sid domain.ID) ([]toolUse, error) {
	uses, err := e.log.List(ctx, sid, events.ListQuery{Types: []string{string(domain.EventAgentToolUse)}})
	if err != nil {
		return nil, fmt.Errorf("list tool uses: %w", err)
	}
	results, err := e.log.List(ctx, sid, events.ListQuery{Types: []string{
		string(domain.EventAgentToolResult),
		string(domain.EventUserToolResult),
	}})
	if err != nil {
		return nil, fmt.Errorf("list tool results: %w", err)
	}
	answered := make(map[string]bool, len(results))
	for _, r := range results {
		var ref struct {
			ToolUseID string `json:"tool_use_id"`
		}
		if err := json.Unmarshal(r.Body, &ref); err != nil {
			return nil, fmt.Errorf("tool result %s: %w", r.ID, err)
		}
		answered[ref.ToolUseID] = true
	}

	var out []toolUse
	for _, u := range uses {
		if answered[u.ID.String()] {
			continue
		}
		var body struct {
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(u.Body, &body); err != nil {
			return nil, fmt.Errorf("tool use %s: %w", u.ID, err)
		}
		out = append(out, toolUse{id: u.ID, name: body.Name, input: body.Input})
	}
	return out, nil
}
