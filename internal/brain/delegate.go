package brain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
	"github.com/jackc/pgx/v5"
)

// Delegation is settlement-executed (plan 35 decision 6). The six tools no
// driver can run are resolved here, inside the transaction that commits the
// turn that called them, under the session row lock every settlement already
// holds — so a spawn's thread row, its child's queued turn and the events
// announcing both land together or not at all.
//
// A name the model was not offered at all rides the same machinery
// (delegatedCall.unoffered, #567): nothing else in the platform will ever run
// it or answer it either, so it is settled here too, with its own dedicated
// answer rather than any of the six handlers below.
//
// Two rules hold this together and every branch below keeps them. First,
// every call is answered by an agent.tool_result in this same commit: an
// unanswered one is in the exec drivers' runnable set, and the runner would
// answer it "unknown tool", telling a coordinator its spawn failed. Second,
// the commit schedules what follows — the spawned child's turn, a woken
// parent's, or the caller's own next turn when the turn left nothing to wait
// for — because today's tool branch would otherwise complete the item and
// enqueue nothing, leaving the thread running with no live item and no
// trigger.

// maxLiveThreads is the documented cap of 25 concurrent threads per session,
// the primary counted (plan 35 decision 8, INFERRED): the create_agent that
// would make a 26th is the is_error.
const maxLiveThreads = 25

// maxSettlementChain bounds a run of consecutive turns that chained on
// nothing but their own settlement answers. Such a turn has nothing left to
// wait for, so it hands its own model_turn straight back with no delay — and
// every shape that reaches it is one a model repeats: list_agents, a
// wait_for_agents answered timed_out, a create_agent maxLiveThreads refused,
// send_to_parent. Unbounded, the thread never idles, so the session's fold
// keeps it running, so archive and delete refuse it; and because queue.Claim
// orders by created_at, which a requeue deliberately does not move, the
// looping item stays the oldest queued model_turn and starves every other
// session on a single-brain deployment (#442). A turn that woke another
// thread is excluded: a successful create_agent or send_to_agent
// scheduled somebody else's work, so it is progress, not a repeat.
//
// maxLiveThreads bounds spawn amplification and does not help here: at the
// cap a create_agent becomes an is_error, which sets neither park nor ended,
// so it chains like the rest.
//
// The number is generous on purpose. A coordinator's own settlement-only runs
// are two or three turns — spawn, list, then a wait that parks — so a thread
// that reaches 25 has stopped making progress rather than working through a
// roster.
//
// What it bounds is one thread chaining its OWN turn with nothing feeding it.
// A client's message resets the count, and so does a peer agent's, which
// arrives as a fresh item: two agents messaging each other in a loop are not
// bounded here and are not meant to be — that is maxSessionDelegationTurns
// below, which counts on the session row precisely because this one cannot
// survive the idle a peer's wake goes through (#447).
const maxSettlementChain = 25

// maxSessionDelegationTurns bounds what a session may spend when nothing
// outside it is asking for anything: turns that called a delegation tool since
// a client last made a demand. It is the bound two agents messaging each other
// cannot escape, and it lives on the sessions row because every cheaper home
// leaks — a work_items row is reset by the wake that re-queues it, and the
// `woke` and chainInput predicates both read a peer's message as progress.
//
// The number is derived rather than picked: the session may spend what every
// thread it is permitted to hold could each spend under the thread-local cap.
// That matters because the arithmetic is not obvious — a coordinator working a
// 24-child roster over ten rounds, each round costing a report and a re-task,
// is roughly 480 counted turns with no client input, so a rounder-looking 250
// would cut legitimate work.
//
// What it does NOT bound, so nobody reads it as a runaway guard: a single agent
// looping bash forever off one client message is exactly as unbounded as
// before. A pair emitting one real tool call alongside each message is bounded
// on a cloud environment but not on a self_hosted one, and the difference is
// which event answers the call: an executor posts agent.tool_result, which is
// not pending input, so the refusal fires; a self_hosted client POSTs
// user.tool_result, which is, so it declines rather than strand the thread.
// Client-executed custom tools take the self_hosted lane on either kind. A
// general spend bound is #432's, and is blocked on a per-model price table a
// self-hosted deployment cannot have.
const maxSessionDelegationTurns = maxLiveThreads * maxSettlementChain

// maxDelegationText bounds the model-supplied text a delegation call carries
// into an event payload — a task, a report, a message. Generous enough that no
// real instruction is cut, small enough that the text cannot be used as a
// payload on a log that lives as long as its session; the same reasoning
// failTurn's cap makes, at the same boundary.
const maxDelegationText = 16 << 10

// The answers that are not machine-shaped. They are byte-stable because the
// model reads them on every later replay of this thread's log, and a wording
// that drifts between two turns is a prefix that moves.
const (
	answerMessageSent      = "Message sent."
	answerReported         = "Result reported."
	answerWaitStarted      = `{"message":"Wait started. Reports arrive as messages; do not conclude yet.","timed_out":false}`
	answerNothingToWaitFor = `{"message":"No agents are running and no reports are pending.","timed_out":true}`
)

// delegation is what a turn's settlement-executed calls did: the events they
// projected and the answers to them in model order, the threads whose next
// model_turn this commit must enqueue, and the two outcomes that decide the
// calling thread's own fate.
type delegation struct {
	events []events.NewEvent
	wakes  []domain.ID
	// park is set by a wait_for_agents that found something to wait for: the
	// coordinator idles end_turn with nothing enqueued, and a report wakes it
	// (plan 35 decision 7's W1). What the turn as a whole holds can still
	// override it — the commit decides.
	park bool
	// ended is set by a submit_result that reported: the child's turn is over
	// and nothing re-enqueues it until a message arrives — or has already.
	ended bool
}

// delegate runs one turn's delegation calls. It carries what every call reads
// — the session, the calling thread and the agent it runs — and accumulates
// what they produce.
type delegate struct {
	sid   domain.ID
	agent domain.ResolvedAgent
	// caller is the calling thread as a message peer: the coordinator is the
	// zero value (the primary has no roster name), a child carries its own.
	caller    events.ThreadPeer
	watermark int64
	// settlementOnly is false when the turn also holds a call some driver must
	// run, which is what makes a submit_result sharing that turn an is_error
	// (decision 6): reporting and working at once would end the turn with work
	// outstanding.
	settlementOnly bool
	// waited caches the first wait_for_agents' verdict: the calls run in model
	// order, so the first wait sees the spawns this turn already made, and a
	// second wait in the same turn must not answer differently from the first.
	waited *waitVerdict

	out delegation
}

// wrongRole reports why this thread may not call name, or "" when it may. A
// child is a child by having a caller peer at all: the primary thread has no
// roster name, so the zero value is the coordinator.
//
// The message names the tool the model should have reached for, because the
// model can see neither its own role nor the half it was not offered — it has
// simply called something that is not in front of it, and the only useful
// answer says what is.
func (d *delegate) wrongRole(name string) string {
	child := d.caller.ThreadID != ""
	switch {
	case child && toolset.IsCoordinatorTool(name):
		return fmt.Sprintf("%s belongs to your coordinator, not to you. Report with %s, "+
			"or send your coordinator a message with %s.", name, toolset.ToolSubmitResult, toolset.ToolSendToParent)
	case !child && toolset.IsWorkerTool(name):
		return fmt.Sprintf("%s belongs to the agents you spawn, not to you. You are answering "+
			"the user directly; to reach an agent use %s.", name, toolset.ToolSendToAgent)
	}
	return ""
}

// waitVerdict is one turn's answer to wait_for_agents.
type waitVerdict struct {
	answer string
	park   bool
}

// run resolves the calls in model order, appending each call's projection
// events and then its answer, so the batch reads as the turn happened.
func (d *delegate) run(ctx context.Context, tx pgx.Tx, calls []delegatedCall) error {
	for _, call := range calls {
		if call.unoffered {
			// Not one of the six: the model called a name nothing offered it
			// at all (#567). wrongRole and the switch below both assume a
			// genuine delegation call, so this is answered here instead of
			// either — otherwise a hallucinated "create_agent" in a
			// single-agent session would attempt to spawn a roster member
			// that does not exist rather than being told the name is unknown.
			ev, err := delegationAnswer(call.eventID, unknownToolAnswer(call.name), true)
			if err != nil {
				return err
			}
			ev.ThreadID = d.caller.ThreadID
			d.out.events = append(d.out.events, ev)
			continue
		}
		var answer string
		var isErr bool
		var err error
		// The role gate, restated where the call is answered rather than only
		// where the tools are offered. The brain gives each thread one half of
		// the six, so a name from the other half is a tool this thread never
		// had — refused here with a result, because every handler past this
		// point assumes its caller is the role that owns it, and because a
		// delegation call left unanswered is a thread nothing can end.
		if wrong := d.wrongRole(call.name); wrong != "" {
			answer, isErr = wrong, true
		} else {
			switch call.name {
			case toolset.ToolCreateAgent:
				answer, isErr, err = d.createAgent(ctx, tx, call)
			case toolset.ToolSendToAgent:
				answer, isErr, err = d.sendToAgent(ctx, tx, call)
			case toolset.ToolListAgents:
				answer, isErr, err = d.listAgents(ctx, tx)
			case toolset.ToolWaitForAgents:
				answer, isErr, err = d.waitForAgents(ctx, tx)
			case toolset.ToolSubmitResult:
				answer, isErr, err = d.submitResult(ctx, tx, call)
			case toolset.ToolSendToParent:
				answer, isErr, err = d.sendToParent(ctx, tx, call)
			default:
				// Unreachable — the class that marked the call settlement-executed
				// is built from these six names. Answered rather than skipped all
				// the same, because an unanswered call is the one thing this
				// settlement may not commit.
				answer, isErr = fmt.Sprintf("%q is not a delegation tool.", call.name), true
			}
		}
		if err != nil {
			return err
		}
		ev, err := delegationAnswer(call.eventID, answer, isErr)
		if err != nil {
			return err
		}
		ev.ThreadID = d.caller.ThreadID
		d.out.events = append(d.out.events, ev)
	}
	return nil
}

// delegationAnswer renders one call's agent.tool_result on the calling
// thread's log, in the shape the executor's own answers take
// (internal/executor toolResultEvent), so a replay cannot tell a settled
// answer from an executed one. The text is never empty: a Messages endpoint
// rejects an empty text block, and this request is replayed on every later
// turn of the thread.
func delegationAnswer(useID domain.ID, text string, isErr bool) (events.NewEvent, error) {
	if text == "" {
		text = toolset.NoOutput
	}
	payload, err := json.Marshal(map[string]any{
		"tool_use_id": useID.String(),
		"content":     []map[string]any{{"type": "text", "text": text}},
		"is_error":    isErr,
	})
	if err != nil {
		return events.NewEvent{}, err
	}
	return events.NewEvent{Type: domain.EventAgentToolResult, Payload: payload}, nil
}

// delegationText is what the model wrote, made safe to store: a NUL would
// fault the jsonb append and wedge the very settlement that is answering the
// call (#228, one level up), and an unbounded string is a payload rather than
// a message.
func delegationText(s string) string {
	s = strings.TrimSpace(toolset.SanitizeText(s))
	if len(s) > maxDelegationText {
		s = toolset.TruncateRunes(s, maxDelegationText) + "[truncated]"
	}
	return s
}

// unknownToolAnswer is the is_error text answering a call to a name the
// model was not offered (#567): unknown tool %q: not offered to this agent.
// Unlike delegationText, which trims and bounds a message or a report before
// it is used, this quotes the name exactly as the model sent it — no
// TrimSpace, which would misreport a name that genuinely carries leading or
// trailing whitespace as one that does not — and bounds the fully rendered
// text rather than the raw name: %q escapes every quote, backslash and
// control byte in the name, so a name built from those can render two to
// four times its own length, and bounding the raw name first (as
// delegationText does) would let the quoted, prefixed, truncation-marked
// result exceed maxDelegationText by exactly that expansion.
func unknownToolAnswer(name string) string {
	const suffix = "[truncated]"
	text := fmt.Sprintf("unknown tool %q: not offered to this agent", toolset.SanitizeText(name))
	if len(text) > maxDelegationText {
		text = toolset.TruncateRunes(text, maxDelegationText-len(suffix)) + suffix
	}
	return text
}

// createAgent spawns a roster member as a new thread. The order of the writes
// is forced: the row must exist before TransitionThread updates it and before
// the work item's thread_id foreign key can name it.
func (d *delegate) createAgent(ctx context.Context, tx pgx.Tx, call delegatedCall) (string, bool, error) {
	var in struct {
		AgentName string `json:"agent_name"`
		Message   string `json:"message"`
	}
	if err := json.Unmarshal(call.input, &in); err != nil {
		return "create_agent takes an agent_name and a message.", true, nil
	}
	name := strings.TrimSpace(in.AgentName)
	message := delegationText(in.Message)
	if name == "" || message == "" {
		return "create_agent needs both agent_name and message.", true, nil
	}
	member, known := rosterMember(d.agent.Multiagent, name)
	if member == nil {
		return fmt.Sprintf("no agent named %q is on your roster; it lists: %s.",
			name, strings.Join(known, ", ")), true, nil
	}
	// Live is what the session's own fold means by it — unarchived and not
	// terminated (events.foldSession) — so the cap and the status rollup can
	// never disagree about which threads exist.
	var live int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM session_threads
		  WHERE session_id = $1 AND archived_at IS NULL AND status <> 'terminated'`,
		d.sid.String()).Scan(&live); err != nil {
		return "", false, err
	}
	if live >= maxLiveThreads {
		// live counts the primary too (decision 8), but list_agents shows the
		// children alone — so reporting live here would name a number the model
		// cannot reconcile with anything it can see. It is told the cap, which
		// is the fact it can act on.
		return fmt.Sprintf("this session already runs the maximum of %d agent threads "+
			"(your own included). Archive a finished one to free a slot.", maxLiveThreads), true, nil
	}

	// The wait cache is stale the moment this turn spawns: a wait that already
	// answered "nothing to wait for" said so about a session without this child,
	// and a second wait in the same turn must not repeat it — the tool's own
	// description tells the model not to conclude before its agents report, and
	// answering from the cache would tell it the opposite on a log that replays
	// that answer for the rest of the session.
	d.waited = nil

	child := domain.NewID("sthr")
	// created_at is set here rather than left to the column's now(), which is
	// transaction_timestamp() and so identical for every child a single
	// settlement spawns — the same hazard events/log.go already avoids with
	// clock_timestamp(). Without it "creation order" is a tie broken by a random
	// sthr_ token, and three tools plus a public route claim to present these
	// threads in the order they were made.
	//
	// The scope columns are copied from the session rather than left to their
	// defaults: a session in a non-default org would otherwise spawn its
	// children outside its own scope, which is why migration 0025's backfill
	// copies them too. The row is born running — it and the child's queued
	// turn commit together, so there is no window in which a turn is queued
	// for a thread that is not yet running.
	if _, err := tx.Exec(ctx,
		`INSERT INTO session_threads (id, session_id, parent_thread_id, org_id, workspace_id, project_id,
		                              agent, agent_name, status, created_at, updated_at)
		 SELECT $1, s.id, $2, s.org_id, s.workspace_id, s.project_id, $3::jsonb, $4, 'running',
		        clock_timestamp(), clock_timestamp()
		   FROM sessions s WHERE s.id = $5`,
		child.String(), domain.PrimaryThreadID(d.sid).String(), []byte(member), name, d.sid.String()); err != nil {
		return "", false, err
	}
	created, err := json.Marshal(map[string]any{
		"agent_name": name, "session_thread_id": child.String(),
	})
	if err != nil {
		return "", false, err
	}
	// The spawn's four projection events, in the order the session view shows
	// them: the creation, the coordinator's own copy of the task, the task as
	// the child's first input, and the child's status.
	d.out.events = append(d.out.events, events.NewEvent{Type: domain.EventSessionThreadCreated, Payload: created})
	target := events.ThreadPeer{ThreadID: child, AgentName: name}
	sent, received, err := events.ThreadMessage(d.sid, d.caller, target, message)
	if err != nil {
		return "", false, err
	}
	d.out.events = append(d.out.events, sent, received)
	pair, _, err := events.TransitionThread(ctx, tx, d.sid, events.ThreadTransition{
		ThreadID: child, Status: domain.SessionRunning})
	if err != nil {
		return "", false, err
	}
	d.out.events = append(d.out.events, pair...)
	d.out.wakes = append(d.out.wakes, child)

	answer, err := json.Marshal(map[string]any{"session_thread_id": child.String()})
	if err != nil {
		return "", false, err
	}
	return string(answer), false, nil
}

// rosterMember finds a member by name in the session's snapshot and returns
// its bytes as stored: migration 0025's CHECK ties a child row to a stored
// snapshot and the SDK's ten-field member shape is pinned where the roster is
// written (internal/api/roster.go), so re-marshalling here could only lose it.
// A member that will not decode is one nothing can name, so it is skipped
// rather than failing the turn — a deterministic decode error inside a
// settlement is a reclaim loop that grinds forever telling nobody.
func rosterMember(roster json.RawMessage, name string) (json.RawMessage, []string) {
	var p struct {
		Agents []json.RawMessage `json:"agents"`
	}
	if json.Unmarshal(roster, &p) != nil {
		return nil, nil
	}
	var known []string
	var found json.RawMessage
	for _, raw := range p.Agents {
		var m struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &m) != nil || m.Name == "" {
			continue
		}
		known = append(known, m.Name)
		if m.Name == name && found == nil {
			found = raw
		}
	}
	return found, known
}

// childThread is one child row as the addressing tools read it.
type childThread struct {
	id        domain.ID
	agentName string
	status    string
	stop      string // the idle stop reason's type; empty otherwise
}

// liveChildren lists the session's unarchived, unterminated child threads in
// creation order — what list_agents renders and what send_to_agent addresses.
func (d *delegate) liveChildren(ctx context.Context, tx pgx.Tx) ([]childThread, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, agent_name, status, COALESCE(stop_reason->>'type', '')
		   FROM session_threads
		  WHERE session_id = $1 AND parent_thread_id IS NOT NULL
		    AND archived_at IS NULL AND status <> 'terminated'
		  ORDER BY created_at, id`, d.sid.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []childThread
	for rows.Next() {
		var c childThread
		var id string
		if err := rows.Scan(&id, &c.agentName, &c.status, &c.stop); err != nil {
			return nil, err
		}
		c.id = domain.ID(id)
		out = append(out, c)
	}
	return out, rows.Err()
}

// sendToAgent delivers a message to one of the session's child threads,
// addressed by thread id or — when it is unambiguous — by agent name.
func (d *delegate) sendToAgent(ctx context.Context, tx pgx.Tx, call delegatedCall) (string, bool, error) {
	var in struct {
		SessionThreadID string `json:"session_thread_id"`
		AgentName       string `json:"agent_name"`
		Message         string `json:"message"`
	}
	if err := json.Unmarshal(call.input, &in); err != nil {
		return "send_to_agent takes a message and either a session_thread_id or an agent_name.", true, nil
	}
	message := delegationText(in.Message)
	if message == "" {
		return "send_to_agent needs a message.", true, nil
	}
	children, err := d.liveChildren(ctx, tx)
	if err != nil {
		return "", false, err
	}
	var target *childThread
	switch {
	case strings.TrimSpace(in.SessionThreadID) != "":
		id := domain.ID(strings.TrimSpace(in.SessionThreadID))
		if id == domain.PrimaryThreadID(d.sid) {
			return "that is your own thread; send_to_agent addresses the agents you spawned.", true, nil
		}
		for i := range children {
			if children[i].id == id {
				target = &children[i]
			}
		}
		if target == nil {
			return fmt.Sprintf("no live agent thread %q in this session; %s.", id, agentsPhrase(children)), true, nil
		}
	case strings.TrimSpace(in.AgentName) != "":
		name := strings.TrimSpace(in.AgentName)
		var matches []childThread
		for _, c := range children {
			if c.agentName == name {
				matches = append(matches, c)
			}
		}
		switch len(matches) {
		case 0:
			return fmt.Sprintf("no live thread runs %q; %s.", name, agentsPhrase(children)), true, nil
		case 1:
			target = &matches[0]
		default:
			ids := make([]string, len(matches))
			for i, m := range matches {
				ids[i] = m.id.String()
			}
			return fmt.Sprintf("%d live threads run %q; address one by session_thread_id: %s.",
				len(matches), name, strings.Join(ids, ", ")), true, nil
		}
	default:
		return "send_to_agent needs a session_thread_id or an agent_name.", true, nil
	}
	if target.status == string(domain.SessionIdle) && target.stop == string(domain.StopRetriesExhausted) {
		// Nothing would ever read the message: no result, no confirmation and
		// no trigger re-enqueues a thread that exhausted its retries, so an
		// acknowledgement here would be a lie the coordinator acts on.
		return fmt.Sprintf("agent %q stopped: its turn exhausted its retries. "+
			"Archive it to free the slot, or spawn a replacement.", target.agentName), true, nil
	}
	sent, received, err := events.ThreadMessage(d.sid, d.caller,
		events.ThreadPeer{ThreadID: target.id, AgentName: target.agentName}, message)
	if err != nil {
		return "", false, err
	}
	d.out.events = append(d.out.events, sent, received)
	if err := d.wake(ctx, tx, target.id); err != nil {
		return "", false, err
	}
	return answerMessageSent, false, nil
}

// agentsPhrase names the live agent threads for an addressing error.
func agentsPhrase(children []childThread) string {
	if len(children) == 0 {
		return "this session has no live agent threads"
	}
	parts := make([]string, len(children))
	for i, c := range children {
		parts[i] = fmt.Sprintf("%s (%s)", c.id, c.agentName)
	}
	return "live threads are " + strings.Join(parts, ", ")
}

// listAgents renders the session's live child threads. The shape is ours
// (INFERRED): the id to address, the agent running there, its status, and — on
// an idle one — why it stopped.
func (d *delegate) listAgents(ctx context.Context, tx pgx.Tx) (string, bool, error) {
	children, err := d.liveChildren(ctx, tx)
	if err != nil {
		return "", false, err
	}
	type entry struct {
		SessionThreadID string `json:"session_thread_id"`
		AgentName       string `json:"agent_name"`
		Status          string `json:"status"`
		// Idle says three different things, and the other two tools already tell
		// them apart: wait_for_agents counts a requires_action child as still
		// working, and send_to_agent refuses one that exhausted its retries.
		// Rendering "idle" alone would let a coordinator that polls instead of
		// waiting read "still needs a human" and "dead" as "finished". The
		// column is already selected for exactly those two callers.
		StopReason string `json:"stop_reason,omitempty"`
	}
	list := make([]entry, 0, len(children))
	for _, c := range children {
		list = append(list, entry{SessionThreadID: c.id.String(), AgentName: c.agentName,
			Status: c.status, StopReason: c.stop})
	}
	raw, err := json.Marshal(list)
	if err != nil {
		return "", false, err
	}
	return string(raw), false, nil
}

// waitForAgents answers at once and parks the coordinator when there is
// something to wait for (plan 35 decision 7's W1). Something to wait for is a
// child still working — running, or idle only until its human answers — and
// nothing else: a wait with no such child would park a thread nothing can
// wake, which is the wedge W1 exists to avoid. Input already waiting to be
// read is the third case and the one that must not park, whichever kind it
// is: a report above the head this turn replayed, or a client's message that
// landed mid-turn and enqueued nothing (the API's message arm needs an idle
// thread). Either chains the turn, so the test here is the one every
// settlement chains on — a wait that has something to read parks on nothing.
//
// events.BusyChild is what "still working" means, and the same call decides
// whether a child's ending must wake a coordinator already parked
// (events.WakeOnThreadEnded): a park taken on one notion and released on
// another is a coordinator waiting for nothing.
func (d *delegate) waitForAgents(ctx context.Context, tx pgx.Tx) (string, bool, error) {
	if d.waited == nil {
		busy, err := events.BusyChild(ctx, tx, d.sid, "")
		if err != nil {
			return "", false, err
		}
		unread, err := chainInput(ctx, tx, d.sid, d.caller.ThreadID, d.watermark)
		if err != nil {
			return "", false, err
		}
		if !busy && !unread {
			d.waited = &waitVerdict{answer: answerNothingToWaitFor}
		} else {
			// !unread is belt-and-braces: the settlement re-runs this same
			// chainInput predicate and turns a park with input waiting into a
			// chain anyway, so a bare park: busy behaves identically today and
			// no test can tell them apart. It stays because the invariant this
			// function documents is the wait's own — a wait that has something
			// to read parks on nothing — and a verdict that says otherwise
			// would be true only for as long as the downstream check exists.
			d.waited = &waitVerdict{answer: answerWaitStarted, park: busy && !unread}
		}
	}
	if d.waited.park {
		d.out.park = true
	}
	return d.waited.answer, false, nil
}

// submitResult reports to the coordinator and ends the child's turn.
func (d *delegate) submitResult(ctx context.Context, tx pgx.Tx, call delegatedCall) (string, bool, error) {
	var in struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(call.input, &in); err != nil {
		return "submit_result takes a result.", true, nil
	}
	result := delegationText(in.Result)
	if result == "" {
		return "submit_result needs a result.", true, nil
	}
	if !d.settlementOnly {
		// Ending the turn here would strand the calls sharing it: they commit
		// with this event and nothing would ever answer them.
		return "report after your tool calls have returned.", true, nil
	}
	if err := d.report(ctx, tx, result); err != nil {
		return "", false, err
	}
	d.out.ended = true
	return answerReported, false, nil
}

// sendToParent messages the coordinator without ending the child's turn.
func (d *delegate) sendToParent(ctx context.Context, tx pgx.Tx, call delegatedCall) (string, bool, error) {
	var in struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(call.input, &in); err != nil {
		return "send_to_parent takes a message.", true, nil
	}
	message := delegationText(in.Message)
	if message == "" {
		return "send_to_parent needs a message.", true, nil
	}
	if err := d.report(ctx, tx, message); err != nil {
		return "", false, err
	}
	return answerMessageSent, false, nil
}

// report delivers a child's text to the primary thread and wakes it. The wake
// runs before the caller idles the child, so a session whose last running
// thread is this child never folds idle between the two.
func (d *delegate) report(ctx context.Context, tx pgx.Tx, text string) error {
	sent, received, err := events.ThreadMessage(d.sid, d.caller, events.ThreadPeer{}, text)
	if err != nil {
		return err
	}
	d.out.events = append(d.out.events, sent, received)
	return d.wake(ctx, tx, "")
}

// wake flips a target that nothing else will move and records the turn its
// caller must enqueue.
func (d *delegate) wake(ctx context.Context, tx pgx.Tx, target domain.ID) error {
	pair, _, woke, err := events.WakeThread(ctx, tx, d.sid, target)
	if err != nil {
		return err
	}
	d.out.events = append(d.out.events, pair...)
	if woke {
		d.out.wakes = append(d.out.wakes, target)
	}
	return nil
}

// commitDelegatedTurn settles a turn that called at least one delegation
// tool, or a name the model was not offered at all (#567 — delegated carries
// both). The calls run inside the append transaction and are answered there,
// whatever else the turn holds; what the commit schedules is decided by the
// turn's own shape, in this order:
//
//   - an ask gate still wins for the exec calls: the thread suspends
//     requires_action as any gated turn does, and a parking wait is suppressed
//     because a thread cannot idle on two reasons and the human's verdict is
//     the one a client can act on. The delegation calls execute all the same —
//     they are the platform's own, and no confirmation exists for them.
//   - a turn that also holds a call someone else must answer — a driver's or a
//     client's — suspends running like any tool turn, whatever a wait in it
//     answered; that answer is what wakes the thread.
//   - a parking wait_for_agents, and a submit_result that reported, each idle
//     the thread on end_turn with nothing enqueued: a message wakes it. Both
//     make the chain check first, so input that landed mid-turn is read rather
//     than stranded on a thread that just went idle.
//   - anything else has nothing left to wait for, so this commit hands the
//     item straight back for the caller's next turn. Requeue rather than a
//     fresh Enqueue, which this very item's live slot would swallow (the
//     settle chain's own reason).
func (b *Brain) commitDelegatedTurn(ctx context.Context, sid domain.ID, item *queue.Item,
	agent domain.ResolvedAgent, head []events.NewEvent, opts events.AppendOptions,
	workKind queue.Kind, askIDs []domain.ID, delegated []delegatedCall,
	settlementOnly bool, watermark int64) error {

	return b.commitUnderLock(ctx, sid, func(ctx context.Context, tx pgx.Tx) ([]events.NewEvent, events.AppendOptions, error) {
		before, err := sessionStatusNow(ctx, tx, sid)
		if err != nil {
			return nil, opts, err
		}
		d := &delegate{sid: sid, agent: agent, watermark: watermark, settlementOnly: settlementOnly}
		if item.ThreadID != "" {
			d.caller = events.ThreadPeer{ThreadID: item.ThreadID, AgentName: agent.Name}
		}
		// Whether this turn made at least one genuine delegation call — the
		// session-wide delegation budget's own distinction (#567): a turn whose
		// delegated set is entirely names the model was not offered spent no
		// delegation budget, whatever else it did. !dc.unoffered alone is not
		// enough: resolveTools classes the *other* half's names too (so a
		// cross-half call is answered wrongRole rather than "unknown"), so a
		// child calling create_agent is !unoffered but was never offered
		// create_agent — d.wrongRole is the same check d.run itself answers the
		// call with, so a call this budget counts is exactly a call d.run will
		// actually dispatch to one of the six handlers. The per-thread chain cap
		// below makes neither distinction — item.Chain counts a turn of any of
		// the three kinds, and so does its cut — because that count exists for a
		// thread nobody is feeding, which every one of them is equally; only the
		// session-wide count is scoped to delegation specifically.
		genuine := false
		for _, dc := range delegated {
			if !dc.unoffered && d.wrongRole(dc.name) == "" {
				genuine = true
				break
			}
		}
		if err := d.run(ctx, tx, delegated); err != nil {
			return nil, opts, err
		}
		batch := append(head, d.out.events...)

		gated := len(askIDs) > 0
		// A wait parks nothing while a call this turn made is still
		// outstanding: settlementOnly is exactly "nothing else to answer", and
		// without it the thread would idle on a call whose answer wakes
		// nobody — neither the exec drivers' drain (it resumes running threads)
		// nor the API's result trigger (it fires on a running thread) moves an
		// idle one. Such a turn suspends running like any tool turn instead,
		// and whoever answers the call wakes it.
		park := d.out.park && !gated && settlementOnly
		chain := !gated && settlementOnly && !park && !d.out.ended
		idle := !gated && (park || d.out.ended)
		// A chain the settlement took on its own answers is the one nobody
		// feeds, so it is the one that has to be counted. Cutting it idles the
		// thread instead: the session's fold can then leave running, archive
		// and delete stop refusing, and a message still resumes the work.
		// A turn that woke another thread scheduled work for somebody,
		// whatever else it did, so it is not part of the run being bounded —
		// which is the run that schedules nothing for anybody. This is what
		// keeps a coordinator spawning one agent per turn away from the cut:
		// its own requeued item keeps its created_at and stays the oldest
		// queued turn, so no spawned child runs in between to break the run,
		// and without this the 24th spawn of a 25-thread roster would trip it.
		// d.out.wakes holds only the wakes that actually took (delegate.wake),
		// so a send_to_agent at a child already running counts, as it should.
		woke := len(d.out.wakes) > 0
		capped := chain && !woke && item.Chain+1 >= maxSettlementChain
		byInput := false
		if idle || capped {
			// The chain-or-idle check every other terminal settlement makes
			// (settle, settleEndTurn), for the same reason: input that arrived
			// past the head this turn replayed has no trigger left once this
			// thread is idle. A coordinator's send_to_agent to a child that was
			// running is the case decision 6 promises the child "reads at its
			// next settle" — this is that settle. A capped turn asks it too:
			// cutting the chain must not strand input either, and input is the
			// progress that resets the count.
			chained, err := chainInput(ctx, tx, sid, item.ThreadID, watermark)
			if err != nil {
				return nil, opts, err
			}
			switch {
			case chained:
				idle, chain, capped, byInput = false, true, false, true
			case capped:
				idle, chain = true, false
			}
		}
		if capped {
			// The chain is bounded identically whether it ran on delegation
			// calls or on unoffered ones (below), and item.Chain counts every
			// turn of either kind — so this notice fires on the chain's shape,
			// never on this turn's kind (#567): a child capped by an unoffered
			// call after a run of genuine ones must still be reported and its
			// parent woken, or the W1 wedge the comments below rule out is back.
			// chainCapped's own message is worded to name no kind, so it
			// stays accurate whichever mix actually built the chain.
			ev, err := chainCapped(item.ThreadID, agent.Name)
			if err != nil {
				return nil, opts, err
			}
			batch = append(batch, ev)
			// The cut ends a child's work without a report, so it owes its
			// coordinator the notice every other ending owes it (decision 7).
			// The two idles this branch was written for do not: a park is the
			// coordinator's own, and a submit_result already reported. Without
			// it a coordinator parked on a wait has nothing left to wake it —
			// WakeOnThreadEnded fires on a child that stopped, and a child
			// merely going idle is not something anyone else watches. That is
			// the W1 wedge. Notice, then the wake, then the idle below, in
			// that order so the session never folds idle in between.
			notice, nerr := childEndedNotice(ctx, tx, sid, item.ThreadID, func(agentName string) string {
				return fmt.Sprintf("[agent %s stopped: %d consecutive turns resolved entirely by "+
					"settlement-owned answers — delegation calls, names it was never offered — with "+
					"no external input and no newly woken thread]\n\nNothing was reported on them; "+
					"anything it reported earlier stands. It is idle until you message it — send it "+
					"what is missing, or archive it to free the slot.", agentName, maxSettlementChain)
			})
			if nerr != nil {
				return nil, opts, nerr
			}
			if notice != nil {
				pair, _, parentWoke, werr := events.WakeOnThreadEnded(ctx, tx, sid, item.ThreadID)
				if werr != nil {
					return nil, opts, werr
				}
				batch = append(append(batch, *notice), pair...)
				if parentWoke {
					// Through the same loop every other wake this settlement
					// takes goes through, so a woken thread never ends up
					// running with no item.
					d.out.wakes = append(d.out.wakes, "")
				}
			}
		}
		switch {
		case gated:
			pair, _, err := events.TransitionThread(ctx, tx, sid, events.ThreadTransition{
				ThreadID: item.ThreadID, Status: domain.SessionIdle,
				Stop: &domain.StopReason{Type: domain.StopRequiresAction, EventIDs: askIDs}})
			if err != nil {
				return nil, opts, err
			}
			batch = append(batch, pair...)
		case idle:
			pair, _, err := events.TransitionThread(ctx, tx, sid, events.ThreadTransition{
				ThreadID: item.ThreadID, Status: domain.SessionIdle,
				Stop: &domain.StopReason{Type: domain.StopEndTurn}})
			if err != nil {
				return nil, opts, err
			}
			batch = append(batch, pair...)
		}

		after, err := sessionStatusNow(ctx, tx, sid)
		if err != nil {
			return nil, opts, err
		}
		if after != before {
			opts.SetStatus = &after
		}
		opts.Then = func(ctx context.Context, tx pgx.Tx) error {
			// The session delegation bound (#447), counted here because this
			// is the one commit every delegation call passes through — the
			// caller's branch owns every shape a turn holding one takes, the
			// ask gate and the mixed exec turn included. The six delegation
			// tools are the only channel by which one thread reaches another,
			// so every turn that continues an agent-to-agent loop is counted:
			// continuing the loop requires a delegation call.
			//
			// Deliberately NOT restricted to settlement-only turns. A model
			// that puts one trivial bash call in the same turn as each message
			// would otherwise never move the counter at all — and that turn is
			// exactly as much autonomous spend as a bare message is.
			//
			// The reset is not here: it rides events.AppendInTx, keyed on
			// domain.EventType.StartsNewWork, so every path that appends a
			// client demand returns the budget without remembering to.
			//
			// Gated on genuine (#567): a turn whose delegated set holds no call
			// this thread's own role was actually offered — every name unknown
			// to class entirely, every name that belongs to the other half, or
			// a mix of the two — made no delegation call at all, so it must not
			// spend a budget that exists to bound agent-to-agent activity — a
			// session that only ever hallucinates tool names, or a child that
			// only ever reaches for its coordinator's half, would otherwise be
			// refused session_delegation_exhausted_error despite never once
			// reaching a call one of the six handlers actually ran.
			if genuine {
				if _, err := tx.Exec(ctx,
					`UPDATE sessions SET delegation_turns = delegation_turns + 1 WHERE id = $1`,
					sid.String()); err != nil {
					return err
				}
			}
			if chain {
				// Input and a wake are both progress, and plain Requeue
				// clears the count: the run being bounded is the one that
				// neither is fed nor feeds anyone.
				var err error
				if byInput || woke {
					err = b.queue.Requeue(ctx, tx, item)
				} else {
					err = b.queue.RequeueSettlement(ctx, tx, item, item.Chain+1)
				}
				if err != nil {
					return err
				}
			} else {
				if err := b.queue.Complete(ctx, tx, item); err != nil {
					return err
				}
				// The gated turn enqueues nothing: even its allow-policy calls
				// wait for the human, as they do on any suspended turn.
				if !gated && workKind != "" {
					if _, err := b.queue.Enqueue(ctx, tx, item.EnvironmentID, sid, workKind); err != nil {
						return err
					}
				}
			}
			for _, tid := range d.out.wakes {
				if _, err := b.queue.EnqueueThread(ctx, tx, item.EnvironmentID, sid, tid, queue.ModelTurn); err != nil {
					return err
				}
			}
			return nil
		}
		return batch, opts, nil
	})
}

// sessionStatusNow reads the session's status column under the caller's lock.
// One delegation commit can move several threads — three spawns and a park, a
// woken parent and the child that woke it — and every TransitionThread writes
// the column itself, so what AppendOptions.SetStatus must carry is the NET
// move rather than any single call's: it writes the column again, and a stale
// value there would roll a later transition back (AppendTransition computes it
// the same way, and for the same reason).
func sessionStatusNow(ctx context.Context, tx pgx.Tx, sid domain.ID) (domain.SessionStatus, error) {
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM sessions WHERE id = $1`, sid.String()).Scan(&status)
	return domain.SessionStatus(status), err
}

// chainInput is the full chain test for a settlement that carries a
// watermark: unprocessed inbound input, or an agent-to-agent message
// delivered to this thread past the head its turn replayed. The second half
// is what makes a report reach a parent that was running when it landed — the
// delivering child saw the parent running and correctly did not wake it — and
// it must be detected by seq rather than by processed_at, because an agent.*
// event is stamped at write and pendingInput's predicate can never see one
// (plan 35 decision 7).
func chainInput(ctx context.Context, tx pgx.Tx, sid, threadID domain.ID, watermark int64) (bool, error) {
	var chained bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM events
		  WHERE session_id = $1 AND seq > $2 AND thread_id IS NOT DISTINCT FROM $3
		    AND ((type = ANY($4) AND processed_at IS NULL) OR type = $5))`,
		sid.String(), watermark, events.NullableThread(threadID), pendingInputTypes,
		string(domain.EventAgentThreadMessageReceived)).Scan(&chained)
	return chained, err
}

// chainCapped is the session.error a cut settlement chain leaves. An end_turn
// alone would be indistinguishable from a coordinator that finished, so the
// operator gets the reason; the model does not, because a session.error never
// reaches it in replay. It is the thread's own event, stamped and not
// cross-posted — the same shape failTurn's error takes. So on a child thread
// it reads on that child's own stream, not the session view, exactly as a
// child's failed turn does; the session view sees the thread's status change.
func chainCapped(threadID domain.ID, agentName string) (events.NewEvent, error) {
	payload, err := json.Marshal(map[string]any{"error": map[string]any{
		"type": "delegation_chain_exhausted_error",
		// Names no kind, because the chain this counts is either kind and a run
		// can mix them turn to turn (#567): "delegation calls" alone would
		// misdescribe a chain an unoffered name built any part of. Nor does it
		// say "ran no tool": every one of these turns did run a tool —
		// list_agents, wait_for_agents and the rest are real calls the
		// settlement itself answers — what none of them did is leave anything
		// running outside this thread's own settlement to wait for.
		"message": fmt.Sprintf("%s ran %d consecutive turns resolved entirely by settlement-owned "+
			"answers — delegation calls, names it was never offered — with no external input and "+
			"no newly woken thread. The thread is idle; a message, or a report from a child it "+
			"spawned, resumes it.",
			agentName, maxSettlementChain),
		// Required on every variant of the reference's error union, and
		// carried by every other session.error this platform writes.
		// "exhausted", not "retrying": the platform will make no further
		// attempt on its own, which is the whole point of the cut. What
		// resumes the thread is somebody else's message or a child's report.
		"retry_status": map[string]any{"type": "exhausted"},
	}})
	if err != nil {
		return events.NewEvent{}, err
	}
	return events.NewEvent{Type: domain.EventSessionError, Payload: payload, ThreadID: threadID}, nil
}

// runExhausted is the session.error a refused claim leaves — chainCapped's
// session-wide sibling (#447). The type is this platform's own: the reference
// publishes no bound of any kind on autonomous agent-to-agent activity, so
// there is no name of theirs to reuse. Its one activity ceiling, `budget`, is
// a monetary ceiling on tracked list cost, and its `budget_reached` tells a
// client to raise the budget to continue — a remedy that does not exist here,
// where `budget` is null and a POST of one is refused. docs/DIVERGENCES.md
// carries the argument.
func runExhausted(threadID domain.ID, agentName string) (events.NewEvent, error) {
	payload, err := json.Marshal(map[string]any{"error": map[string]any{
		"type": "session_delegation_exhausted_error",
		"message": fmt.Sprintf("This session spent %d turns calling delegation tools since a "+
			"client last asked it for anything, so %s was not started. Threads that were "+
			"running settle and stop. A message to any of them resumes the session with a "+
			"fresh budget.", maxSessionDelegationTurns, agentName),
		// "exhausted", not "retrying", for chainCapped's reason: the platform
		// will make no further attempt on its own. What resumes the session is
		// somebody outside it asking for something.
		"retry_status": map[string]any{"type": "exhausted"},
	}})
	if err != nil {
		return events.NewEvent{}, err
	}
	return events.NewEvent{Type: domain.EventSessionError, Payload: payload, ThreadID: threadID}, nil
}

// cutExhaustedRun refuses to start a turn whose session has spent its
// delegation budget, and reports whether it did (#447).
//
// It refuses at the CLAIM rather than cutting at the settlement, which is what
// keeps the change small and, more importantly, correct. A settlement-side cut
// has to force idle on a turn that may also hold real tool calls — on a mixed
// turn the fate block's gated/idle/capped/chain are all false, so the cut would
// idle the thread while opts.Then still enqueued its tool_exec, leaving a
// sandbox command in flight against a session that has already folded idle and
// become archivable, with events.ResumableThreads selecting only running
// threads so nothing would ever read the result. Refusing a claim cannot
// produce that state: it never intervenes in a turn's fate, it only declines to
// start one, and a thread holding an unanswered tool call has no item to
// refuse. Every enqueue that could reach one is already gated on the same
// unanswered-call predicate — the API's three (internal/api/events.go), the
// wake paths' (internal/events/threadmsg.go), and events.ResumableThreads,
// which is what both exec drivers drain through — and the one site that is
// not, the API's new-work-cycle arm, rides a user.message or
// user.define_outcome, which resets this counter in the same transaction. It
// also needs no gate on delegate.wake, so send_to_agent still
// answers its byte-pinned "Message sent." truthfully — the peer really is woken,
// and it is its own next claim that is refused.
//
// The refusal terminates rather than oscillating. Threads already mid-turn
// settle once more and may wake peers as they do today; each of those items is
// refused at its next claim, so overshoot is bounded at one model request per
// live thread — which is what the reference documents for its own budget,
// enforced between model requests rather than mid-request.
func (b *Brain) cutExhaustedRun(ctx context.Context, sid domain.ID, item *queue.Item,
	agent domain.ResolvedAgent) (bool, error) {
	// Fast path, unlocked: one primary-key read of a counter that is zero for
	// every session that never delegated. A stale read is safe both ways —
	// under the cap the turn runs, which is what it would have done anyway;
	// at or over it, the locked re-read below is the one that decides.
	var spent int
	if err := b.pool.QueryRow(ctx,
		`SELECT delegation_turns FROM sessions WHERE id = $1`, sid.String()).Scan(&spent); err != nil {
		return false, fmt.Errorf("read delegation turns: %w", err)
	}
	if spent < maxSessionDelegationTurns {
		return false, nil
	}

	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, sid.String()); err != nil {
		return false, err
	}
	// Re-read under the lock: a client's message may have landed since the
	// fast path, and its reset is this cut's entire remedy. No test reaches the
	// under-cap return below — it needs a message committed inside the window
	// between the unlocked read and this lock — so it is the one branch here
	// resting on the argument rather than on a run.
	if err := tx.QueryRow(ctx,
		`SELECT delegation_turns FROM sessions WHERE id = $1`, sid.String()).Scan(&spent); err != nil {
		return false, err
	}
	if spent < maxSessionDelegationTurns {
		return false, nil
	}
	// Pending input is the other reason to run rather than refuse. A client
	// message cannot reach this check — it reset the counter in the same
	// append, under this lock — but user.tool_result and user.custom_tool_result
	// are deliberately not resets, and idling a thread that holds one
	// unprocessed would strand it with no trigger left, breaking the
	// chain-or-idle contract every other terminal settlement honours.
	pending, err := pendingInput(ctx, tx, sid, item.ThreadID, 0)
	if err != nil {
		return false, err
	}
	if pending {
		return false, nil
	}

	// Read before the transitions below move it, so the metric counts a real
	// change: on a session whose siblings are still running the fold does not
	// move at all, and an unconditional SetStatus would record a transition to
	// the status the session already had. The same before/after test every
	// other settlement here makes.
	before, err := sessionStatusNow(ctx, tx, sid)
	if err != nil {
		return false, err
	}

	ev, err := runExhausted(item.ThreadID, agent.Name)
	if err != nil {
		return false, err
	}
	batch := []events.NewEvent{ev}
	// A refused child ends without a report, so it owes its coordinator the
	// notice every other ending owes it — the same W1 wedge chainCapped's cut
	// closes. Notice, then the wake, then the idle, in that order so the
	// session never folds idle in between.
	notice, err := childEndedNotice(ctx, tx, sid, item.ThreadID, func(agentName string) string {
		return fmt.Sprintf("[agent %s stopped: the session spent its delegation budget of %d "+
			"turns]\n\nNothing was reported on them; anything it reported earlier stands. The "+
			"whole session is idle until somebody messages it.", agentName, maxSessionDelegationTurns)
	})
	if err != nil {
		return false, err
	}
	wokeParent := false
	if notice != nil {
		pair, _, parentWoke, werr := events.WakeOnThreadEnded(ctx, tx, sid, item.ThreadID)
		if werr != nil {
			return false, werr
		}
		batch = append(append(batch, *notice), pair...)
		wokeParent = parentWoke
	}
	pair, _, err := events.TransitionThread(ctx, tx, sid, events.ThreadTransition{
		ThreadID: item.ThreadID, Status: domain.SessionIdle,
		Stop: &domain.StopReason{Type: domain.StopEndTurn}})
	if err != nil {
		return false, err
	}
	batch = append(batch, pair...)

	after, err := sessionStatusNow(ctx, tx, sid)
	if err != nil {
		return false, err
	}
	opts := events.AppendOptions{ThreadID: item.ThreadID}
	if after != before {
		opts.SetStatus = &after
	}
	opts.Then = func(ctx context.Context, tx pgx.Tx) error {
		if err := b.queue.Complete(ctx, tx, item); err != nil {
			return err
		}
		// A woken parent needs an item like any other wake, or it ends up
		// running with nothing queued. Its own claim is then refused too —
		// and because childEndedNotice returns nil for the primary, there is
		// no third hop.
		if wokeParent {
			if _, err := b.queue.EnqueueThread(ctx, tx, item.EnvironmentID, sid, "", queue.ModelTurn); err != nil {
				return err
			}
		}
		return nil
	}
	if _, err := b.log.AppendInTx(ctx, tx, sid, batch, opts); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	if opts.SetStatus != nil {
		events.RecordSessionStatus(ctx, after)
	}
	return true, nil
}
