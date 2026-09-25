package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
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

	status, res := readJSON(t, s.doRaw(http.MethodPost, "/v1/sessions/"+sid+"/events", map[string]any{"events": []any{
		map[string]any{"type": "user.tool_result", "tool_use_id": useID,
			"content": []any{map[string]any{"type": "text", "text": "sthr_forged"}}},
	}}, workerAuth(t, s, sid)))
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
	// And in processing order (#793 item 3): the coordinator's running pair,
	// then the notice its woken turn reads, then the child's ending.
	if got := typesAmong(s.eventTypes(sid), "session.status_running", "session.thread_status_running",
		"agent.thread_message_received", "session.thread_status_terminated"); !sameStrings(got, []string{
		"session.status_running", "session.thread_status_running",
		"agent.thread_message_received", "session.thread_status_terminated"}) {
		t.Errorf("archive wrote %v, want the wake, the notice, then the ending", got)
	}
	// The notice is an input, not an emission: it lists unprocessed — the key
	// present, null (#78) — until the coordinator's woken request starts, which
	// stamps it 1 µs before its own start, as the reference does (#793).
	if v, ok := notice["processed_at"]; !ok || v != nil {
		t.Errorf("notice processed_at = %v (present %v), want a present null until consumed", v, ok)
	}
	if _, _, err := events.NewLog(s.pool).StartModelRequestOn(context.Background(), domain.ID(sid), "", events.Backend{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := lastEventOfType(t, s, sid, "agent.thread_message_received")["processed_at"]; got == nil {
		t.Error("notice still unprocessed after the coordinator's request started")
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
	// Processing order (#793 item 3, #539): the interrupt, then the
	// coordinator's running event, then the notice its woken turn reads, then
	// the child's idle. The session was already running under the child, so
	// the wake is the thread event alone.
	if got := typesAmong(wholeLogTypes(t, s, sid), "user.interrupt", "session.status_running",
		"session.thread_status_running", "agent.thread_message_received", "session.thread_status_idle"); !sameStrings(got,
		[]string{"user.interrupt", "session.thread_status_running", "agent.thread_message_received", "session.thread_status_idle"}) {
		t.Errorf("interrupt wrote %v, want the interrupt, the wake, the notice, then the child's idle", got)
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

// One request that stops the coordinator and one of its children must leave the
// coordinator stopped. The child's ending would otherwise wake it — that is the
// whole of the ending rule — and the wake would flip the very thread this batch
// just idled back to running and queue it a fresh turn.
//
// It is not the session-wide case in disguise: a session-wide interrupt names no
// thread, and the every-thread-named spelling requires *every* live thread, so an
// idle sibling left unnamed is enough to tell them apart. That is the ordinary
// client pattern — stop everything that is running — which is why this is the
// shape worth pinning.
func TestInterruptingThePrimaryAndAChildLeavesTheCoordinatorStopped(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	primary := domain.PrimaryThreadID(domain.ID(sid)).String()
	setThread(t, s, primary, "running", "null")
	child := insertChild(t, s, sid, "running")
	insertChild(t, s, sid, "idle") // live, unnamed, and so not the every-thread case
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE sessions SET status = 'running' WHERE id = $1`, sid); err != nil {
		t.Fatal(err)
	}

	sendEvents(t, s, sid,
		map[string]any{"type": "user.interrupt", "session_thread_id": primary},
		map[string]any{"type": "user.interrupt", "session_thread_id": child})

	if got := s.threadStatus(t, primary); got != "idle" {
		t.Errorf("coordinator = %q, want the interrupt that stopped it to have stuck", got)
	}
	if n := s.liveWork(sid, queue.ModelTurn); n != 0 {
		t.Errorf("model turns queued = %d, want none — this batch stopped the coordinator", n)
	}
}

// noticeText flattens a thread message's single text block.
// typesAmong keeps the types in keep, in log order.
func typesAmong(types []string, keep ...string) []string {
	want := map[string]bool{}
	for _, k := range keep {
		want[k] = true
	}
	var out []string
	for _, ty := range types {
		if want[ty] {
			out = append(out, ty)
		}
	}
	return out
}

// wholeLogTypes reads every row of the session's log in seq order, a child's
// own rows included — what no single wire surface shows.
func wholeLogTypes(t *testing.T, s *tserver, sid string) []string {
	t.Helper()
	evs, err := events.NewLog(s.pool).List(context.Background(), domain.ID(sid), events.ListQuery{Scope: events.ScopeAll})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = string(ev.Type)
	}
	return out
}

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
