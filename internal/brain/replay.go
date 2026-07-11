package brain

import (
	"encoding/json"
	"fmt"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
)

// buildRequest replays the event log into one provider request: the log IS
// the conversation (plan component 3 — "replay = read events in order and
// rebuild provider messages"). It returns the request and the replay
// watermark (the highest seq consumed), which the turn's settlement stamps
// as processed.
//
// Replay mapping, v1:
//   - user.message           → user text/blocks
//   - system.message         → appended to the system prompt (documented
//     assumption; the Messages API has one system slot)
//   - agent.message          → assistant text blocks
//   - agent.*tool_use        → assistant tool_use block, id = the EVENT id
//     (the provider-side tool id was discarded at emission; the event id is
//     the durable name results reference)
//   - *.tool_result          → user tool_result block
//   - session.*/span.*/user.interrupt/user.tool_confirmation → not
//     conversation material; skipped
//
// agent.thinking replays as nothing: the wire event carries no content, so
// thinking is never reconstructed (and v1 never requests extended thinking).
func buildRequest(agent domain.ResolvedAgent, history []domain.Event) (provider.Request, int64, error) {
	req := provider.Request{System: agent.System}
	var watermark int64

	// Custom tools are real Messages-API tool definitions minus the union
	// discriminator; an agent_toolset entry expands to the built-in tools it
	// enables (bash/read/write/edit/glob/grep), which the executor runs in the
	// sandbox. mcp_toolset still waits for the MCP client — documented.
	for _, raw := range agent.Tools {
		var probe struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return req, 0, fmt.Errorf("agent tool: %w", err)
		}
		switch probe.Type {
		case "custom":
			def, err := json.Marshal(map[string]any{
				"name": probe.Name, "description": probe.Description, "input_schema": probe.InputSchema,
			})
			if err != nil {
				return req, 0, err
			}
			req.Tools = append(req.Tools, def)
		case "agent_toolset_20260401":
			defs, err := toolset.Tools(raw)
			if err != nil {
				return req, 0, fmt.Errorf("agent tool: %w", err)
			}
			req.Tools = append(req.Tools, defs...)
		}
	}

	// Merge runs of same-role events into single messages; within a user
	// message, tool_result blocks sort first (the Messages API requires
	// results ahead of other content).
	var (
		role       string
		results    []json.RawMessage // tool_result blocks of the open user turn
		blocks     []json.RawMessage // other blocks of the open turn
		systemTail string
	)
	flush := func() error {
		if role == "" {
			return nil
		}
		content, err := json.Marshal(append(results, blocks...))
		if err != nil {
			return err
		}
		req.Messages = append(req.Messages, provider.Message{Role: role, Content: content})
		role, results, blocks = "", nil, nil
		return nil
	}
	turn := func(r string) error {
		if role != r {
			if err := flush(); err != nil {
				return err
			}
			role = r
		}
		return nil
	}

	for _, ev := range history {
		watermark = ev.Seq
		switch ev.Type {
		case domain.EventUserMessage:
			var p struct {
				Content json.RawMessage `json:"content"`
			}
			if err := json.Unmarshal(ev.Body, &p); err != nil {
				return req, 0, fmt.Errorf("event %s: %w", ev.ID, err)
			}
			items, err := contentBlocks(p.Content)
			if err != nil {
				return req, 0, fmt.Errorf("event %s: %w", ev.ID, err)
			}
			if err := turn("user"); err != nil {
				return req, 0, err
			}
			blocks = append(blocks, items...)

		case domain.EventSystemMessage:
			var p struct {
				Content []domain.ContentBlock `json:"content"`
			}
			if err := json.Unmarshal(ev.Body, &p); err != nil {
				return req, 0, fmt.Errorf("event %s: %w", ev.ID, err)
			}
			for _, blk := range p.Content {
				if systemTail != "" || req.System != "" {
					systemTail += "\n\n"
				}
				systemTail += blk.Text
			}

		case domain.EventAgentMessage:
			var p struct {
				Content json.RawMessage `json:"content"`
			}
			if err := json.Unmarshal(ev.Body, &p); err != nil {
				return req, 0, fmt.Errorf("event %s: %w", ev.ID, err)
			}
			items, err := contentBlocks(p.Content)
			if err != nil {
				return req, 0, fmt.Errorf("event %s: %w", ev.ID, err)
			}
			if err := turn("assistant"); err != nil {
				return req, 0, err
			}
			blocks = append(blocks, items...)

		case domain.EventAgentToolUse, domain.EventAgentMCPToolUse, domain.EventAgentCustomToolUse:
			var p struct {
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal(ev.Body, &p); err != nil {
				return req, 0, fmt.Errorf("event %s: %w", ev.ID, err)
			}
			input := p.Input
			if len(input) == 0 || string(input) == "null" {
				input = json.RawMessage("{}")
			}
			blk, err := json.Marshal(map[string]any{
				"type": "tool_use", "id": ev.ID, "name": p.Name, "input": input,
			})
			if err != nil {
				return req, 0, err
			}
			if err := turn("assistant"); err != nil {
				return req, 0, err
			}
			blocks = append(blocks, blk)

		case domain.EventUserToolResult, domain.EventUserCustomToolRes,
			domain.EventAgentToolResult, domain.EventAgentMCPToolResult:
			blk, err := toolResultBlock(ev)
			if err != nil {
				return req, 0, err
			}
			if err := turn("user"); err != nil {
				return req, 0, err
			}
			results = append(results, blk)

		default:
			// Lifecycle, spans, interrupts, confirmations: state, not
			// conversation.
		}
	}
	if err := flush(); err != nil {
		return req, 0, err
	}
	req.System += systemTail
	return req, watermark, nil
}

// classifyTools maps each tool the agent offers to the event type its use is
// emitted as: a custom tool is client-executed (agent.custom_tool_use), an
// expanded agent_toolset tool is platform-executed in the sandbox
// (agent.tool_use). mcp_toolset waits for the MCP client, so its tools are not
// offered and never appear here. A name the model calls that is in no set falls
// back to custom at emission — the client can reject it — since the platform
// only runs names it recognises as its own.
func classifyTools(agent domain.ResolvedAgent) (map[string]domain.EventType, error) {
	kind := make(map[string]domain.EventType)
	for _, raw := range agent.Tools {
		var probe struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return nil, fmt.Errorf("agent tool: %w", err)
		}
		switch probe.Type {
		case "custom":
			kind[probe.Name] = domain.EventAgentCustomToolUse
		case "agent_toolset_20260401":
			defs, err := toolset.Tools(raw)
			if err != nil {
				return nil, fmt.Errorf("agent tool: %w", err)
			}
			for _, def := range defs {
				var d struct {
					Name string `json:"name"`
				}
				if err := json.Unmarshal(def, &d); err != nil {
					return nil, err
				}
				kind[d.Name] = domain.EventAgentToolUse
			}
		}
	}
	return kind, nil
}

// classifyPolicies maps each platform-executed built-in tool the agent offers
// to its resolved permission policy (always_allow / always_ask), the brain's
// input for stamping evaluated_permission and deciding whether a turn suspends
// for confirmation. Only agent_toolset tools carry a policy: custom tools are
// client-executed (permission is the client's concern) and mcp_toolset waits
// for the MCP client, so neither appears here.
func classifyPolicies(agent domain.ResolvedAgent) (map[string]domain.PermissionPolicyType, error) {
	policy := make(map[string]domain.PermissionPolicyType)
	for _, raw := range agent.Tools {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return nil, fmt.Errorf("agent tool: %w", err)
		}
		if probe.Type != "agent_toolset_20260401" {
			continue
		}
		pols, err := toolset.Policies(raw)
		if err != nil {
			return nil, fmt.Errorf("agent tool: %w", err)
		}
		for name, p := range pols {
			policy[name] = p
		}
	}
	return policy, nil
}

// contentBlocks normalizes wire message content (a bare string or an array
// of blocks) into individual raw blocks, preserved verbatim.
func contentBlocks(raw json.RawMessage) ([]json.RawMessage, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		blk, err := json.Marshal(map[string]string{"type": "text", "text": s})
		if err != nil {
			return nil, err
		}
		return []json.RawMessage{blk}, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("content must be a string or an array of blocks")
	}
	return items, nil
}

// toolResultBlock maps any of the four result event shapes onto the wire
// tool_result block. The *_use_id field name varies per event type; the
// value is always the tool-use EVENT id.
func toolResultBlock(ev domain.Event) (json.RawMessage, error) {
	var p struct {
		ToolUseID       string          `json:"tool_use_id"`
		CustomToolUseID string          `json:"custom_tool_use_id"`
		MCPToolUseID    string          `json:"mcp_tool_use_id"`
		Content         json.RawMessage `json:"content"`
		IsError         *bool           `json:"is_error"`
	}
	if err := json.Unmarshal(ev.Body, &p); err != nil {
		return nil, fmt.Errorf("event %s: %w", ev.ID, err)
	}
	id := p.ToolUseID
	if id == "" {
		id = p.CustomToolUseID
	}
	if id == "" {
		id = p.MCPToolUseID
	}
	blk := map[string]any{"type": "tool_result", "tool_use_id": id}
	if len(p.Content) > 0 && string(p.Content) != "null" {
		blk["content"] = p.Content
	}
	if p.IsError != nil {
		blk["is_error"] = *p.IsError
	}
	return json.Marshal(blk)
}
