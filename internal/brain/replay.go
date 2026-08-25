package brain

import (
	"encoding/json"
	"fmt"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// buildRequest replays the event log into one provider request: the log IS
// the conversation (plan component 3 — "replay = read events in order and
// rebuild provider messages"). It returns the request and the replay
// watermark (the highest seq consumed), which the turn's settlement stamps
// as processed.
//
// The tool definitions arrive already assembled (resolveTools), because what
// the model may call is not a question the log answers: it comes from the
// agent's tools[] and the session's MCP catalog, and the same resolution has to
// decide what a name the model calls back means. So the agent reaches here as
// its system prompt and nothing more — everything else it contributes to a
// request has been resolved by the time replay runs.
//
// Replay mapping, v1:
//   - user.message           → user text/blocks
//   - system.message         → appended to the system prompt (documented
//     assumption; the Messages API has one system slot)
//   - agent.message          → assistant text blocks
//   - agent.thread_message_received → user text block naming the sender
//   - agent.*tool_use        → assistant tool_use block, id = the EVENT id
//     (the provider-side tool id was discarded at emission; the event id is
//     the durable name results reference)
//   - *.tool_result          → user tool_result block
//   - session.*/span.*/user.interrupt/user.tool_confirmation → not
//     conversation material; skipped
//
// agent.thinking replays as nothing: the wire event carries no content, so
// thinking is never reconstructed (and v1 never requests extended thinking).
func buildRequest(system string, tools []json.RawMessage, history []domain.Event, skillsBlock, filesBlock, reposBlock, memoryBlock string) (provider.Request, int64, error) {
	req := provider.Request{System: system, Tools: tools}
	// Startup metadata blocks sit after the agent's own system prompt and before
	// any runtime system.message text (systemTail), which is appended at the end:
	// the Level-1 skills block first, then the Mounted-files block, then the
	// Mounted-repositories block, then the Memory-stores block. Placement is an
	// inference (docs/DIVERGENCES.md).
	for _, block := range []string{skillsBlock, filesBlock, reposBlock, memoryBlock} {
		if block != "" {
			if req.System != "" {
				req.System += "\n\n"
			}
			req.System += block
		}
	}
	var watermark int64

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

		case domain.EventUserDefineOutcome:
			// The outcome definition renders as a user-role message built
			// deterministically from the payload — the task description plus
			// an inline text rubric — so any fresh brain reconstructs the same
			// conversation (rendering ours, INFERRED — docs/DIVERGENCES.md).
			// A file rubric's content reaches the grader from its acceptance
			// snapshot (plan 21 slice 3); the conversation carries the
			// description alone.
			var p struct {
				Description string `json:"description"`
				Rubric      struct {
					Type    string `json:"type"`
					Content string `json:"content"`
				} `json:"rubric"`
			}
			if err := json.Unmarshal(ev.Body, &p); err != nil {
				return req, 0, fmt.Errorf("event %s: %w", ev.ID, err)
			}
			text := "Work toward this outcome: " + p.Description
			if p.Rubric.Type == "text" {
				text += "\n\nYour work will be evaluated against this rubric:\n" + p.Rubric.Content
			}
			// The deliverables contract, stated where the outcome is: the
			// harvest walks /mnt/session/outputs/ and nothing else, so a
			// deliverable written anywhere else never reaches the files
			// registry or the grader (the live acceptance's first satisfied
			// run harvested zero files for exactly this reason).
			text += "\n\nWrite your deliverable files under /mnt/session/outputs/ — files anywhere else are not collected."
			blk, err := json.Marshal(map[string]any{"type": "text", "text": text})
			if err != nil {
				return req, 0, err
			}
			if err := turn("user"); err != nil {
				return req, 0, err
			}
			blocks = append(blocks, blk)

		case domain.EventSpanOutcomeEvalEnd:
			// Grader feedback re-enters the conversation deterministically
			// from the log (no extra persisted event — crash-safe replay;
			// renderings ours, INFERRED). needs_revision carries the failed
			// criteria into the next revision cycle; max_iterations_reached
			// prompts the one final acknowledgment turn the docs describe.
			// Terminal satisfied/failed/interrupted ends are state, not
			// conversation.
			var p struct {
				Result      string `json:"result"`
				Explanation string `json:"explanation"`
			}
			if err := json.Unmarshal(ev.Body, &p); err != nil {
				return req, 0, fmt.Errorf("event %s: %w", ev.ID, err)
			}
			var text string
			switch p.Result {
			case verdictNeedsRevision:
				text = "The outcome grader reviewed your work and found it does not yet satisfy the rubric:\n\n" +
					p.Explanation + "\n\nRevise your work to address these findings."
			case domain.OutcomeResultMaxIterationsReached:
				text = "The outcome's evaluation budget is exhausted and the rubric is still unmet:\n\n" +
					p.Explanation + "\n\nDo not continue working. Briefly acknowledge what was completed and what remains."
			}
			if text != "" {
				blk, err := json.Marshal(map[string]any{"type": "text", "text": text})
				if err != nil {
					return req, 0, err
				}
				if err := turn("user"); err != nil {
					return req, 0, err
				}
				blocks = append(blocks, blk)
			}

		case domain.EventAgentThreadMessageReceived:
			// A message from another thread renders as user-role text, which is
			// the shape a task notification takes in Claude Code's harness
			// (plan 35 decision 7). It is a whole message rather than a
			// tool_result even when it answers a spawn, because the sender's
			// own delegation call was answered in the commit that made it: this
			// arrives turns later, out of band, and the bracketed prefix is what
			// tells the model who is speaking. Deterministic from the payload,
			// so every replay of this log rebuilds the same block (rendering
			// ours, INFERRED — docs/DIVERGENCES.md).
			var p struct {
				FromAgentName string `json:"from_agent_name"`
			}
			if err := json.Unmarshal(ev.Body, &p); err != nil {
				return req, 0, fmt.Errorf("event %s: %w", ev.ID, err)
			}
			// The field is null when the sender is the primary agent, which has
			// a role rather than a roster name.
			from := "your coordinator"
			if p.FromAgentName != "" {
				from = p.FromAgentName
			}
			blk, err := json.Marshal(map[string]any{
				"type": "text",
				"text": "[message from " + from + "]\n\n" + contentText(ev.Body),
			})
			if err != nil {
				return req, 0, err
			}
			if err := turn("user"); err != nil {
				return req, 0, err
			}
			blocks = append(blocks, blk)

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
				Name   string          `json:"name"`
				Server string          `json:"mcp_server_name"`
				Input  json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal(ev.Body, &p); err != nil {
				return req, 0, fmt.Errorf("event %s: %w", ev.ID, err)
			}
			if ev.Type == domain.EventAgentMCPToolUse {
				// The event splits the name the model was offered back into the
				// server and the bare tool, which is the shape the wire wants
				// and the shape a Messages request cannot carry. Replay puts it
				// together again: a tool_use block naming a tool this request
				// does not offer is a conversation the endpoint may refuse, and
				// every later turn replays this same block.
				p.Name = mcpModelName(p.Server, p.Name)
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
			// conversation. agent.thread_message_sent is here too, and
			// deliberately: the sender's own delegation tool_use and the
			// tool_result answering it already carry the message into its
			// conversation, so rendering the projection as well would say it
			// twice (plan 35 decision 6, Design C).
		}
	}
	if err := flush(); err != nil {
		return req, 0, err
	}
	req.System += systemTail
	return req, watermark, nil
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
