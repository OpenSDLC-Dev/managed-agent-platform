package brain

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/transcript"
)

// buildRequest replays the event log into one provider request: the log IS
// the conversation (plan component 3 — "replay = read events in order and
// rebuild provider messages"). It returns the request and the replay
// watermark (the highest seq replayed), past which the turn's settlement
// looks for input that arrived while it ran. It reorders history in place.
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
	// results ahead of other content), in the order of the tool_use blocks
	// they answer. The log holds results in the order they were processed,
	// which is not always the calls' order — a denial that resumes a thread
	// beside a later call's result is written behind the resume, after that
	// result (#793) — and a backend that pairs results with calls by position,
	// as an OpenAI-compatible endpoint reads its tool messages, needs them as
	// the calls ran.
	var (
		role       string
		results    []toolAnswer      // tool_result blocks of the open user turn
		blocks     []json.RawMessage // other blocks of the open turn
		uses       []string          // tool_use ids of the open assistant turn
		answering  map[string]int    // the last assistant turn's, by position
		systemTail string
		budgets    = map[string]int64{} // max_iterations by outcome_id, from each definition
		acks       []ackBlock           // acknowledgment prompts of the open user turn
	)
	flush := func() error {
		if role == "" {
			return nil
		}
		// A prompt client input followed in its turn loses its stop instruction.
		for _, a := range acks {
			if !a.answered {
				continue
			}
			blk, err := json.Marshal(map[string]any{"type": "text", "text": a.verdict})
			if err != nil {
				return err
			}
			blocks[a.at] = blk
		}
		// Stable, so a result answering no call of the last assistant turn
		// keeps its place after the ones that do.
		slices.SortStableFunc(results, func(a, b toolAnswer) int {
			return cmp.Compare(callPosition(answering, a.id), callPosition(answering, b.id))
		})
		content := make([]json.RawMessage, 0, len(results)+len(blocks))
		for _, r := range results {
			content = append(content, r.block)
		}
		raw, err := json.Marshal(append(content, blocks...))
		if err != nil {
			return err
		}
		req.Messages = append(req.Messages, provider.Message{Role: role, Content: raw})
		if role == "assistant" {
			answering = make(map[string]int, len(uses))
			for i, id := range uses {
				answering[id] = i
			}
		}
		role, results, blocks, uses, acks = "", nil, nil, nil, nil
		return nil
	}
	// clientInput marks the open turn's acknowledgment prompts as followed by
	// client input, which flush takes their stop instruction off for.
	clientInput := func() {
		for i := range acks {
			acks[i].answered = true
		}
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

	// Rows replay in consumption order (events.ConsumptionOrder): an input
	// that landed while one of this thread's requests was in flight renders
	// after that request's reply and results, where the next request consumed
	// it, not at its receipt seq ahead of a reply that never saw it (#793). So
	// the row replayed last need not be the highest seq, and the watermark —
	// what the settlement's chain check keys on — is taken as the max. The
	// order is built in history's own slice, which the turn reads for nothing
	// else, rather than in a copy of the whole log on every request.
	for _, ev := range events.ConsumptionOrder(history[:0], history) {
		if ev.Seq > watermark {
			watermark = ev.Seq
		}
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
			clientInput()

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
				MaxIterations int64  `json:"max_iterations"`
				OutcomeID     string `json:"outcome_id"`
			}
			if err := json.Unmarshal(ev.Body, &p); err != nil {
				return req, 0, fmt.Errorf("event %s: %w", ev.ID, err)
			}
			budgets[p.OutcomeID] = p.MaxIterations
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
			clientInput()

		case domain.EventSpanOutcomeEvalEnd:
			// Grader feedback re-enters the conversation deterministically
			// from the log (no extra persisted event — crash-safe replay;
			// renderings ours, INFERRED). needs_revision carries the failed
			// criteria into the next revision cycle; max_iterations_reached
			// prompts the one final acknowledgment turn the docs describe, and
			// so does a satisfied or failed verdict on the budget's last cycle
			// (iteration + 1 >= max_iterations, settleVerdict's lastCycle),
			// which is followed by the same turn (#670, reading (B)): the
			// model is told the verdict, as the recorded acknowledgment ("The
			// outcome is complete. All criteria have been satisfied.") shows
			// the reference's model was. With budget left a satisfied or
			// failed verdict idles the session, "session goes idle" as the SDK
			// says of satisfied, and it and an interrupted end are state, not
			// conversation: nothing is rendered, so a later user.message
			// replays as it always has.
			//
			// An acknowledgment prompt closes on "Do not continue working"
			// unless client input follows the verdict in its user turn
			// (ackBlock, settled at flush). That input — a message posted
			// around the verdict, the next outcome a client defined on seeing
			// the end event, or the next message of a session whose
			// acknowledgment never ran or predates the turn — is what the
			// model must answer, and the instruction would tell it not to. A
			// child's row does not count: the ending notice a grading window
			// held reads after the verdict, and a notice is no input worth a
			// turn after a terminal verdict (#801, gradingChain), so the
			// acknowledgment is still told to stop.
			// Which it is is fixed once a reply closes the turn, so every later
			// replay renders it as its request did. A reply that persisted
			// nothing — an empty end_turn, or thinking alone, neither of which
			// replays — closes nothing: a later message joins the open turn, as
			// it joins any user turn such a reply left open, and the
			// instruction goes, since that message is then what the model
			// must answer.
			var p struct {
				OutcomeID   string `json:"outcome_id"`
				Iteration   int64  `json:"iteration"`
				Result      string `json:"result"`
				Explanation string `json:"explanation"`
			}
			if err := json.Unmarshal(ev.Body, &p); err != nil {
				return req, 0, fmt.Errorf("event %s: %w", ev.ID, err)
			}
			budget, defined := budgets[p.OutcomeID]
			lastCycle := defined && lastOutcomeCycle(p.Iteration, budget)
			var text, stop string
			switch {
			case p.Result == verdictNeedsRevision:
				text = "The outcome grader reviewed your work and found it does not yet satisfy the rubric:\n\n" +
					p.Explanation + "\n\nRevise your work to address these findings."
			case p.Result == domain.OutcomeResultMaxIterationsReached:
				text = "The outcome's evaluation budget is exhausted and the rubric is still unmet:\n\n" + p.Explanation
				stop = "\n\nDo not continue working. Briefly acknowledge what was completed and what remains."
			case p.Result == domain.OutcomeResultSatisfied && lastCycle:
				text = "The outcome grader reviewed your work and found it satisfies the rubric:\n\n" + p.Explanation
				stop = "\n\nDo not continue working. Briefly acknowledge the outcome."
			case p.Result == domain.OutcomeResultFailed && lastCycle:
				text = "The outcome grader found that the rubric cannot be applied to your work:\n\n" + p.Explanation
				stop = "\n\nDo not continue working. Briefly acknowledge the outcome."
			}
			if text != "" {
				blk, err := json.Marshal(map[string]any{"type": "text", "text": text + stop})
				if err != nil {
					return req, 0, err
				}
				if err := turn("user"); err != nil {
					return req, 0, err
				}
				if stop != "" {
					acks = append(acks, ackBlock{at: len(blocks), verdict: text})
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
			// tells the model who is speaking. Deterministic from the stored
			// row — its payload and its session id — so every replay of this
			// log rebuilds the same block (rendering ours, INFERRED —
			// docs/DIVERGENCES.md).
			var p struct {
				FromSessionThreadID domain.ID `json:"from_session_thread_id"`
				FromAgentName       string    `json:"from_agent_name"`
			}
			if err := json.Unmarshal(ev.Body, &p); err != nil {
				return req, 0, fmt.Errorf("event %s: %w", ev.ID, err)
			}
			// The primary agent is named by its role, whatever the row holds.
			// Its name was null before #675 — the only sender ever written
			// without one — and is the session agent's since, but this block
			// heads every request a child assembles: keyed on the name, the
			// change would have reworded that head for every child spawned
			// after it, and a child whose log straddles it would hear one
			// coordinator under two names. Old and new rows render alike.
			from := p.FromAgentName
			if p.FromSessionThreadID == domain.PrimaryThreadID(ev.SessionID) {
				from = "your coordinator"
			}
			blk, err := json.Marshal(map[string]any{
				"type": "text",
				"text": "[message from " + from + "]\n\n" + transcript.ContentText(ev.Body),
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
				p.Name = domain.MCPModelName(p.Server, p.Name)
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
			uses = append(uses, ev.ID.String())

		case domain.EventUserToolResult, domain.EventUserCustomToolRes,
			domain.EventAgentToolResult, domain.EventAgentMCPToolResult:
			answer, err := toolResultBlock(ev)
			if err != nil {
				return req, 0, err
			}
			if err := turn("user"); err != nil {
				return req, 0, err
			}
			results = append(results, answer)

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

// ackBlock is an acknowledgment prompt in the open user turn: blocks[at],
// rendered with its stop instruction, which flush takes off (leaving verdict)
// when client input followed it in the turn (answered).
type ackBlock struct {
	at       int
	verdict  string
	answered bool
}

// toolAnswer is one tool_result block and the tool-use event id it answers.
type toolAnswer struct {
	id    string
	block json.RawMessage
}

// callPosition is where id's tool_use sits in the assistant turn uses indexes,
// every id it does not hold after every id it does.
func callPosition(uses map[string]int, id string) int {
	if i, ok := uses[id]; ok {
		return i
	}
	return len(uses)
}

// toolResultBlock maps any of the four result event shapes onto the wire
// tool_result block. The *_use_id field name varies per event type; the
// value is always the tool-use EVENT id.
func toolResultBlock(ev domain.Event) (toolAnswer, error) {
	var p struct {
		ToolUseID       string          `json:"tool_use_id"`
		CustomToolUseID string          `json:"custom_tool_use_id"`
		MCPToolUseID    string          `json:"mcp_tool_use_id"`
		Content         json.RawMessage `json:"content"`
		IsError         *bool           `json:"is_error"`
	}
	if err := json.Unmarshal(ev.Body, &p); err != nil {
		return toolAnswer{}, fmt.Errorf("event %s: %w", ev.ID, err)
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
	raw, err := json.Marshal(blk)
	return toolAnswer{id: id, block: raw}, err
}
