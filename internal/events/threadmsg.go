package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// Agent-to-agent messages (plan 35 decisions 6 and 7). A coordinator's spawn
// or send, a child's report, and the platform's own notice about a child that
// stopped are all one message from one thread to another, projected as a pair:
// the sender's agent.thread_message_sent and the target's
// agent.thread_message_received. Each half is stored on its own thread's log
// and neither is cross-posted — the primary's rows are the session view
// already, and neither type carries a session_thread_id for a child's row to
// render there with (SDK betasessionevent.go:544, :714), so a cross-posted one
// would show on the session view as a message the primary itself exchanged.

// ThreadPeer names one end of a message: the thread, and the agent it runs.
// Both are empty for the primary thread, whose agent name is absent — null on
// the wire — because it has a role rather than a roster name.
type ThreadPeer struct {
	ThreadID  domain.ID
	AgentName string
}

// wire is the peer's id as the payload carries it: to_/from_session_thread_id
// are required fields, so the primary's derived id stands where the thread_id
// column holds NULL.
func (p ThreadPeer) wire(sessionID domain.ID) string {
	if p.ThreadID == "" {
		return domain.PrimaryThreadID(sessionID).String()
	}
	return p.ThreadID.String()
}

// ThreadMessage renders one message as its two events. A message an agent
// sent appends both; the platform's own notice about a thread that ended
// appends received alone, because nothing sent that one.
//
// text is the caller's to sanitize and bound: it reaches jsonb, where a NUL
// faults the append, and the model supplies it on every path but the notices.
func ThreadMessage(sessionID domain.ID, from, to ThreadPeer, text string) (sent, received NewEvent, err error) {
	content := []map[string]any{{"type": "text", "text": text}}
	// The nullable agent name is written as a present null rather than
	// omitted, the convention the tool-use events' session_thread_id keeps, so
	// the two directions render alike whichever way the message went.
	sentPayload, err := json.Marshal(map[string]any{
		"content":              content,
		"to_session_thread_id": to.wire(sessionID),
		"to_agent_name":        nullableName(to.AgentName),
	})
	if err != nil {
		return NewEvent{}, NewEvent{}, err
	}
	recvPayload, err := json.Marshal(map[string]any{
		"content":                content,
		"from_session_thread_id": from.wire(sessionID),
		"from_agent_name":        nullableName(from.AgentName),
	})
	if err != nil {
		return NewEvent{}, NewEvent{}, err
	}
	return NewEvent{Type: domain.EventAgentThreadMessageSent, Payload: sentPayload, ThreadID: from.ThreadID},
		NewEvent{Type: domain.EventAgentThreadMessageReceived, Payload: recvPayload, ThreadID: to.ThreadID},
		nil
}

// nullableName binds the peer's agent name: NULL for the primary agent.
func nullableName(name string) *string {
	if name == "" {
		return nil
	}
	return &name
}

// WakeThread decides the one question a delivered message raises: whether
// anything else will ever move the target thread. A thread idle on end_turn —
// or on no reason at all, which a primary row the thread migration backfilled
// can be — has no turn coming, so it is flipped to running here and the
// caller enqueues its model_turn; a thread parked on requires_action is idle
// only until its human answers and replays the message when its turn resumes;
// a running one reads it at its next settle (plan 35 decision 6). A thread
// that exhausted its retries is not woken: nothing here can make its turn
// succeed, and its coordinator was already told it stopped.
//
// It runs in the caller's transaction, under the session row lock every
// settlement and trigger holds, which is what serializes two children
// reporting at once: the first finds the parent idle and wakes it, the second
// finds it running and leaves the chain to the parent's own settlement.
func WakeThread(ctx context.Context, tx pgx.Tx, sessionID, threadID domain.ID) (pair []NewEvent, moved *domain.SessionStatus, woke bool, err error) {
	tid := threadID
	if tid == "" {
		tid = domain.PrimaryThreadID(sessionID)
	}
	var status string
	var stopJSON []byte
	err = tx.QueryRow(ctx,
		`SELECT status, stop_reason FROM session_threads WHERE id = $1 AND session_id = $2`,
		tid.String(), sessionID.String()).Scan(&status, &stopJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, false, fmt.Errorf("thread %s not found in session %s", tid, sessionID)
	}
	if err != nil {
		return nil, nil, false, err
	}
	if domain.SessionStatus(status) != domain.SessionIdle {
		return nil, nil, false, nil
	}
	if len(stopJSON) > 0 {
		var stop domain.StopReason
		if err := json.Unmarshal(stopJSON, &stop); err != nil {
			return nil, nil, false, fmt.Errorf("thread %s stop_reason: %w", tid, err)
		}
		if stop.Type != domain.StopEndTurn {
			return nil, nil, false, nil
		}
	}
	// The last thing the API's message trigger checks before resuming a
	// thread, checked here for the same reason: a replay carrying an
	// assistant tool_use no result answers ends the resumed turn in
	// retries_exhausted. The API gates all three of its enqueues on it
	// (internal/api/events.go); the wake paths — send_to_agent, a child's
	// report, an ending — reach the same transition and had no such gate.
	//
	// The state is unreachable through the product: #181 routes a
	// tool-carrying turn away from the end_turn settle, delegation answers
	// every call in the commit that makes it, and a failed turn settles
	// retries_exhausted, which the stop check above already refuses. So this
	// is parity on state nothing should produce, not a live bug — the API's
	// own regression test has to forge it with pgtest.SetSessionStatus.
	// Refusing looks like every other refusal here: no wake, no error.
	// threadID, not tid: the predicate scopes by the events row, and the
	// primary thread's rows carry thread_id NULL — the derived sthr_ id
	// matches none of them.
	unanswered, err := HasUnansweredThreadToolUse(ctx, tx, sessionID, threadID, nil)
	if err != nil {
		return nil, nil, false, err
	}
	if unanswered {
		// Logged, not returned silently, and the one refusal here that is:
		// the other two mean the thread is running or is waiting on a human,
		// and whoever answers wakes it. This one means nothing will — the
		// message is delivered onto a thread no trigger can resume — while
		// the caller still answers the model "Message sent." An operator
		// needs the one line that says so, the same line the API's own arm
		// writes for the same state.
		slog.WarnContext(ctx, "thread not woken: it is idle with an unanswered tool_use",
			"session_id", sessionID, "session_thread_id", tid)
		return nil, nil, false, nil
	}
	pair, moved, err = TransitionThread(ctx, tx, sessionID, ThreadTransition{
		ThreadID: threadID, Status: domain.SessionRunning})
	return pair, moved, err == nil, err
}

// ThreadEnded is the notice a coordinator gets about a child that stopped:
// the received half of a message nothing sent, on the primary's log (plan 35
// decision 7 — an ending condition is delivered as text, like a report). The
// convention is a bracketed line naming the agent and what happened, then the
// next action open to the coordinator.
//
// It delivers; waking is WakeOnThreadEnded's, and the endings a client drives
// pair the two. A session-wide interrupt appends no notice at all — it ends
// the primary in the same batch, so there is nobody left to tell.
func ThreadEnded(sessionID, child domain.ID, agentName, text string) (NewEvent, error) {
	_, received, err := ThreadMessage(sessionID, ThreadPeer{ThreadID: child, AgentName: agentName}, ThreadPeer{}, text)
	return received, err
}

// busyChild is "a child that is still going to report": running, or idle only
// until its human answers (domain.StopRequiresAction — a gated child resumes
// and reports when its verdict lands). Written once because two decisions read
// it and a drift between them is a coordinator parked on nothing: whether a
// wait_for_agents has anything to wait for (plan 35 decision 7's W1), and
// whether an ending has just taken the last thing that could wake a
// coordinator already parked. Bound: $1 the session.
const busyChild = `session_id = $1 AND parent_thread_id IS NOT NULL AND archived_at IS NULL
	  AND (status = 'running' OR (status = 'idle' AND stop_reason->>'type' = 'requires_action'))`

// BusyChild reports whether the session has such a child. except is left out
// of the count when non-empty (no id is empty, so "" counts them all).
func BusyChild(ctx context.Context, q Querier, sessionID, except domain.ID) (bool, error) {
	var busy bool
	if err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM session_threads WHERE `+busyChild+` AND id <> $2)`,
		sessionID.String(), except.String()).Scan(&busy); err != nil {
		return false, fmt.Errorf("busy child check for %s: %w", sessionID, err)
	}
	return busy, nil
}

// WakeOnThreadEnded wakes the primary when the child that just ended was the
// last thing a parked coordinator could have been waiting for, and returns the
// events to append, the session status the wake moved to, and whether it woke
// (the caller enqueues the primary's model_turn then). Run before the ending
// thread's own transition, so this reads the status it ended from.
//
// Both halves of the test are load-bearing. A wait parks only while some busy
// child exists, so an ending that removes the last one is the last chance to
// wake the coordinator: without it the notice sits unread on a session folding
// idle with no turn coming — the wedge W1 exists to avoid, reached the long
// way round. And an ending that leaves another busy child behind — or that
// ends a child nothing was waiting on, an idle one a client tidied away — must
// NOT wake: the coordinator either has a report still coming, whose own
// arrival wakes it, or was never parked at all, and a turn it did not ask for
// costs a model call, blocks the session's own archive behind it, and puts the
// archive path's idle re-advertisement (decision 4's pick) out of reach.
//
// The primary is woken on the rule a report uses (WakeThread): idle on
// end_turn only. A running coordinator chains on the notice by seq at its own
// settle, and one parked on requires_action stays parked until its human
// answers.
//
// "Parked on a wait" is approximated by "a busy child existed and this was the
// last of them", because nothing on the log says a turn ended in a wait — the
// wait's answer is an ordinary tool_result. The approximation errs towards
// waking: a coordinator that idled without waiting is woken too, and reads the
// notice a turn earlier than it otherwise would. That costs a model call the
// client did not ask for; the other direction costs a coordinator parked on a
// child that no longer exists, which nothing can end.
func WakeOnThreadEnded(ctx context.Context, tx pgx.Tx, sessionID, child domain.ID) ([]NewEvent, *domain.SessionStatus, bool, error) {
	var wasBusy, othersBusy bool
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(bool_or(id = $2), false), COALESCE(bool_or(id <> $2), false)
		   FROM session_threads WHERE `+busyChild,
		sessionID.String(), child.String()).Scan(&wasBusy, &othersBusy); err != nil {
		return nil, nil, false, fmt.Errorf("ending wake check for %s: %w", child, err)
	}
	if !wasBusy || othersBusy {
		return nil, nil, false, nil
	}
	return WakeThread(ctx, tx, sessionID, "")
}
