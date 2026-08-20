package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// The control plane's half of delegation (plan 35 slice 4, decisions 6 and
// 7): no client may answer a delegation call, and a child that ends outside
// its own settlement — interrupted, archived — tells its coordinator so and
// wakes it when nothing else would.

const createAgentCall = `{"name":"create_agent","input":{},"evaluated_permission":"allow","session_thread_id":null}`

// A delegation call is answered in the commit that emits it, so this is a
// state production never reaches — the call is planted unanswered on purpose.
// The guard is worth having anyway: the log is append-only, and a forged
// child report is a report the coordinator would act on.
func TestClientResultForADelegationCallIsRefused(t *testing.T) {
	s := newTestServer(t)
	sid := selfHostedSession(t, s)
	child := insertChild(t, s, sid, "running")
	useID := appendOn(t, s, sid, domain.ID(child), false, domain.EventAgentToolUse, createAgentCall)

	status, res := s.do(http.MethodPost, "/v1/sessions/"+sid+"/events", map[string]any{"events": []any{
		map[string]any{"type": "user.tool_result", "tool_use_id": useID,
			"content": []any{map[string]any{"type": "text", "text": "sthr_forged"}}},
	}})
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	if msg, _ := res["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "platform-executed") {
		t.Errorf("message = %q, want the platform-executed refusal", msg)
	}
}

// Archiving the last child a coordinator could still be waiting on — one
// parked on requires_action, which is a child that will still report once its
// human answers — takes away the only thing that would ever have woken it, so
// the notice comes with the wake. Without it the session folds idle holding a
// notice nothing will read and no turn is coming.
func TestArchivingAChildWakesTheParkedCoordinator(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	primary := domain.PrimaryThreadID(domain.ID(sid)).String()
	setThread(t, s, primary, "idle", `{"type":"end_turn"}`)
	child := gatedChild(t, s, sid)

	status, _ := s.do(http.MethodPost, "/v1/sessions/"+sid+"/threads/"+child+"/archive", nil)
	if status != http.StatusOK {
		t.Fatalf("archive: %d", status)
	}
	notice := lastEventOfType(t, s, sid, "agent.thread_message_received")
	if notice["from_session_thread_id"] != child || notice["from_agent_name"] != "worker" {
		t.Errorf("notice = %v, want it from the archived child", notice)
	}
	if text := noticeText(t, notice); !strings.Contains(text, "worker") || !strings.Contains(text, "archived") {
		t.Errorf("notice text = %q, want the outcome named", text)
	}
	if got := s.threadStatus(t, primary); got != "running" {
		t.Errorf("coordinator = %q, want woken to read the notice", got)
	}
	if n := s.liveWork(sid, queue.ModelTurn); n != 1 {
		t.Errorf("coordinator turns queued = %d, want the woken one", n)
	}
	if got := s.sessionStatus(sid); got != "running" {
		t.Errorf("session = %q, want running under its woken coordinator", got)
	}
	// The wake is written before the child's own ending, so the session never
	// folds idle between the two and no client sees an idle it never rested at.
	for _, ty := range s.eventTypes(sid) {
		if ty == "session.status_idle" {
			t.Fatalf("the session idled between the notice and the wake: %v", s.eventTypes(sid))
		}
	}
}

// The wake is exactly as wide as the wedge, and these are the two ways it is
// not one. A coordinator that is running reads the notice at its own settle
// (by seq), and an ending that leaves another child still working leaves a
// report still coming — whose own arrival wakes it. Either way the archive
// stays what it looks like, housekeeping, and starts no turn the client did
// not ask for.
func TestArchivingAChildStartsNoTurnWhenSomethingElseWillMoveTheCoordinator(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T, s *tserver, sid, primary string)
	}{
		{"the coordinator is running", func(t *testing.T, s *tserver, sid, primary string) {
			setThread(t, s, primary, "running", "")
		}},
		{"a sibling is still working", func(t *testing.T, s *tserver, sid, primary string) {
			setThread(t, s, primary, "idle", `{"type":"end_turn"}`)
			insertChild(t, s, sid, "running")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			sid := eventsFixture(t, s)
			primary := domain.PrimaryThreadID(domain.ID(sid)).String()
			child := gatedChild(t, s, sid)
			tc.arrange(t, s, sid, primary)

			status, _ := s.do(http.MethodPost, "/v1/sessions/"+sid+"/threads/"+child+"/archive", nil)
			if status != http.StatusOK {
				t.Fatalf("archive: %d", status)
			}
			if text := noticeText(t, lastEventOfType(t, s, sid, "agent.thread_message_received")); !strings.Contains(text, "archived") {
				t.Errorf("notice text = %q, want the archive still delivered", text)
			}
			if n := s.liveWork(sid, queue.ModelTurn); n != 0 {
				t.Errorf("coordinator turns queued = %d, want none", n)
			}
		})
	}
}

// A thread-scoped interrupt ends a child mid-turn, so the report it owed will
// never come: the coordinator is told and, parked on that child alone, woken
// to read it.
func TestInterruptingAChildWakesTheParkedCoordinator(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	primary := domain.PrimaryThreadID(domain.ID(sid)).String()
	setThread(t, s, primary, "idle", `{"type":"end_turn"}`)
	child := insertChild(t, s, sid, "running")
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE sessions SET status = 'running' WHERE id = $1`, sid); err != nil {
		t.Fatal(err)
	}

	sendEvents(t, s, sid, map[string]any{"type": "user.interrupt", "session_thread_id": child})

	notice := lastEventOfType(t, s, sid, "agent.thread_message_received")
	if notice["from_session_thread_id"] != child || notice["from_agent_name"] != "worker" {
		t.Errorf("notice = %v, want it from the interrupted child", notice)
	}
	if text := noticeText(t, notice); !strings.Contains(text, "interrupted") {
		t.Errorf("notice text = %q, want the outcome named", text)
	}
	if got := s.threadStatus(t, primary); got != "running" {
		t.Errorf("coordinator = %q, want woken to read the notice", got)
	}
	if n := s.liveWork(sid, queue.ModelTurn); n != 1 {
		t.Errorf("coordinator turns queued = %d, want the woken one", n)
	}
}

// gatedChild plants a child parked on requires_action — idle, so it can be
// archived, and still going to report, so a wait_for_agents parks on it.
func gatedChild(t *testing.T, s *tserver, sid string) string {
	t.Helper()
	child := insertChild(t, s, sid, "idle")
	askID := appendOn(t, s, sid, domain.ID(child), true, domain.EventAgentToolUse, askBashCall)
	setThread(t, s, child, "idle", `{"type":"requires_action","event_ids":["`+askID+`"]}`)
	return child
}

// A session-wide interrupt stops the coordinator too, so telling it about
// each child would be noise on a session the human just stopped — and a wake
// there would resurrect the very thread the interrupt just ended.
func TestSessionWideInterruptTellsTheCoordinatorNothing(t *testing.T) {
	s := newTestServer(t)
	sid, _, _, _ := coordinatorFixture(t, s)

	sendEvents(t, s, sid, map[string]any{"type": "user.interrupt"})

	for _, ty := range s.eventTypes(sid) {
		if ty == "agent.thread_message_received" {
			t.Fatalf("a session-wide interrupt reported its children to a coordinator it stopped: %v",
				s.eventTypes(sid))
		}
	}
	if got := s.threadStatus(t, domain.PrimaryThreadID(domain.ID(sid)).String()); got != "idle" {
		t.Errorf("coordinator = %q, want stopped with the rest", got)
	}
	if n := s.liveWork(sid, queue.ModelTurn); n != 0 {
		t.Errorf("model turns queued = %d, want none — the interrupt stopped the session", n)
	}
}

// noticeText flattens a thread message's single text block.
func noticeText(t *testing.T, ev map[string]any) string {
	t.Helper()
	blocks, _ := ev["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("content = %v, want one text block", ev["content"])
	}
	block, _ := blocks[0].(map[string]any)
	text, _ := block["text"].(string)
	return text
}
