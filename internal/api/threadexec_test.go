package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// The thread-aware trigger arms (plan 35 slice 3, decisions 4, 5, 9): a
// session's threads are addressed one at a time, the session's status is the
// fold over theirs, and a thread-scoped interrupt never touches the shared
// exec item. Child rows are planted through the slice-3 test seam; slice 4's
// delegation spawns them for real.

// setThread moves a fixture thread row to status with the given idle stop
// reason JSON ("" for none), the way the platform's transitions would.
func setThread(t *testing.T, s *tserver, threadID, status, stopJSON string) {
	t.Helper()
	var stop *string
	if stopJSON != "" {
		stop = &stopJSON
	}
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE session_threads SET status = $2, stop_reason = $3 WHERE id = $1`, threadID, status, stop); err != nil {
		t.Fatal(err)
	}
}

// appendOn plants one event on a thread's log — cross-posted when the brain
// would have done so (a child's platform call) — and returns its id.
func appendOn(t *testing.T, s *tserver, sessionID string, threadID domain.ID, crossPosted bool,
	typ domain.EventType, payload string) string {
	t.Helper()
	evs, err := events.NewLog(s.pool).Append(context.Background(), domain.ID(sessionID),
		[]events.NewEvent{{Type: typ, ThreadID: threadID, CrossPosted: crossPosted, Payload: []byte(payload)}})
	if err != nil {
		t.Fatal(err)
	}
	return evs[0].ID.String()
}

const allowBashCall = `{"name":"bash","input":{},"evaluated_permission":"allow","session_thread_id":null}`
const askBashCall = `{"name":"bash","input":{},"evaluated_permission":"ask","session_thread_id":null}`

// coordinatorFixture is a session whose primary is parked idle (end_turn, a
// coordinator waiting on its workers), child A idle on an ask it cross-posted
// to the session view, child B running — the session running under the fold.
func coordinatorFixture(t *testing.T, s *tserver) (sid, a, b, askID string) {
	t.Helper()
	sid = eventsFixture(t, s)
	setThread(t, s, domain.PrimaryThreadID(domain.ID(sid)).String(), "idle", `{"type":"end_turn"}`)
	a = insertChild(t, s, sid, "idle")
	b = insertChild(t, s, sid, "running")
	askID = appendOn(t, s, sid, domain.ID(a), true, domain.EventAgentToolUse, askBashCall)
	setThread(t, s, a, "idle", `{"type":"requires_action","event_ids":["`+askID+`"]}`)
	if _, err := s.pool.Exec(context.Background(), `UPDATE sessions SET status = 'running' WHERE id = $1`, sid); err != nil {
		t.Fatal(err)
	}
	return sid, a, b, askID
}

// threadOf reads the thread column an event is stored on ("" for the
// primary): the agent.* result shapes carry no session_thread_id on the wire.
func (s *tserver) threadOf(t *testing.T, eventID string) string {
	t.Helper()
	var tid string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT COALESCE(thread_id, '') FROM events WHERE id = $1`, eventID).Scan(&tid); err != nil {
		t.Fatal(err)
	}
	return tid
}

func (s *tserver) threadStatus(t *testing.T, threadID string) string {
	t.Helper()
	var status string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT status FROM session_threads WHERE id = $1`, threadID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

// liveTurns lists the thread ids of the session's live model_turn items, ""
// for the primary.
func (s *tserver) liveTurns(t *testing.T, sessionID string) []string {
	t.Helper()
	rows, err := s.pool.Query(context.Background(),
		`SELECT COALESCE(thread_id, '') FROM work_items
		 WHERE session_id = $1 AND kind = 'model_turn' AND state IN ('queued','starting','active') ORDER BY created_at`, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var tid string
		if err := rows.Scan(&tid); err != nil {
			t.Fatal(err)
		}
		out = append(out, tid)
	}
	return out
}

// A confirmation for thread A while B runs resumes A alone: A's thread event,
// the shared tool_exec for A's allowed call, no session event — the session
// never left running — and no turn for anyone.
func TestConfirmationForAThreadWhileASiblingRuns(t *testing.T) {
	s := newTestServer(t)
	sid, a, b, askID := coordinatorFixture(t, s)

	sendEvents(t, s, sid, confirm(askID, "allow", nil))

	if got := s.sessionStatus(sid); got != "running" {
		t.Errorf("session status = %q, want running", got)
	}
	if got := s.threadStatus(t, a); got != "running" {
		t.Errorf("thread A status = %q, want running", got)
	}
	if got := s.threadStatus(t, b); got != "running" {
		t.Errorf("thread B status = %q, want running", got)
	}
	types := s.eventTypes(sid)
	if !sameStrings(types, []string{"agent.tool_use", "user.tool_confirmation", "session.thread_status_running"}) {
		t.Errorf("session view = %v, want the ask, its confirmation and A's thread event alone", types)
	}
	ev := lastEventOfType(t, s, sid, "session.thread_status_running")
	if ev["session_thread_id"] != a || ev["agent_name"] != "worker" {
		t.Errorf("thread event = %v, want A's", ev)
	}
	if n := s.liveWork(sid, queue.ToolExec); n != 1 {
		t.Errorf("live tool_exec = %d, want 1", n)
	}
	if turns := s.liveTurns(t, sid); len(turns) != 0 {
		t.Errorf("live turns = %v, want none", turns)
	}
}

// A denial for thread A answers A's call on A's surfaces and wakes A's own
// turn — keyed to A — while B keeps running.
func TestDenialForAThreadWakesThatThreadOnly(t *testing.T) {
	s := newTestServer(t)
	sid, a, _, askID := coordinatorFixture(t, s)

	sendEvents(t, s, sid, confirm(askID, "deny", nil))

	if got := s.sessionStatus(sid); got != "running" {
		t.Errorf("session status = %q, want running", got)
	}
	if turns := s.liveTurns(t, sid); !sameStrings(turns, []string{a}) {
		t.Errorf("live turns = %v, want A's alone", turns)
	}
	if n := s.liveWork(sid, queue.ToolExec); n != 0 {
		t.Errorf("live tool_exec = %d, want 0", n)
	}
	res := lastEventOfType(t, s, sid, "agent.tool_result")
	if res["tool_use_id"] != askID || res["is_error"] != true || s.threadOf(t, res["id"].(string)) != a {
		t.Errorf("denial result on the session view = %v, want A's error result on A's thread", res)
	}
	_, own := s.do(http.MethodGet, "/v1/sessions/"+sid+"/threads/"+a+"/events", nil)
	var ownTypes []string
	for _, ev := range listData(t, own) {
		ownTypes = append(ownTypes, ev["type"].(string))
	}
	if !sameStrings(ownTypes, []string{"agent.tool_use", "user.tool_confirmation", "agent.tool_result", "session.thread_status_running"}) {
		t.Errorf("A's own view = %v", ownTypes)
	}
}

// A thread-scoped interrupt ends that thread alone: its outstanding call is
// answered, its row idles end_turn, the session stays running on its sibling,
// and the shared exec item — which the sibling's call still rides on — stays
// live (decision 9). A session-wide interrupt afterwards ends everything.
func TestThreadScopedInterruptLeavesTheSharedExecItem(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	setThread(t, s, domain.PrimaryThreadID(domain.ID(sid)).String(), "idle", `{"type":"end_turn"}`)
	a := insertChild(t, s, sid, "running")
	b := insertChild(t, s, sid, "running")
	useA := appendOn(t, s, sid, domain.ID(a), true, domain.EventAgentToolUse, allowBashCall)
	useB := appendOn(t, s, sid, domain.ID(b), true, domain.EventAgentToolUse, allowBashCall)
	if _, err := s.pool.Exec(context.Background(), `UPDATE sessions SET status = 'running' WHERE id = $1`, sid); err != nil {
		t.Fatal(err)
	}
	envID := s.environmentID(sid)
	q := queue.New(s.pool)
	if _, err := q.Enqueue(context.Background(), s.pool, domain.ID(envID), domain.ID(sid), queue.ToolExec); err != nil {
		t.Fatal(err)
	}

	sendEvents(t, s, sid, map[string]any{"type": "user.interrupt", "session_thread_id": a})

	if got := s.sessionStatus(sid); got != "running" {
		t.Errorf("session status after A's interrupt = %q, want running (B still runs)", got)
	}
	if got := s.threadStatus(t, a); got != "idle" {
		t.Errorf("thread A = %q, want idle", got)
	}
	if n := s.liveWork(sid, queue.ToolExec); n != 1 {
		t.Errorf("live tool_exec after A's interrupt = %d, want 1 — B's call still rides on it", n)
	}
	types := s.eventTypes(sid)
	// Processing order (#539, #793): the result the interrupt synthesizes
	// comes before it and A's idle after it; the notice, which no wake is for
	// while B still works, is the coordinator's to read later, so it comes last.
	if !sameStrings(types, []string{"agent.tool_use", "agent.tool_use", "agent.tool_result", "user.interrupt",
		"session.thread_status_idle", "agent.thread_message_received"}) {
		t.Errorf("session view = %v, want A's call answered, its coordinator told, and A's thread event, "+
			"no session event", types)
	}
	res := lastEventOfType(t, s, sid, "agent.tool_result")
	if res["tool_use_id"] != useA || s.threadOf(t, res["id"].(string)) != a {
		t.Errorf("interrupt result = %v, want A's call answered on A's thread", res)
	}
	idle := lastEventOfType(t, s, sid, "session.thread_status_idle")
	if idle["session_thread_id"] != a || stopReasonType(t, idle) != "end_turn" {
		t.Errorf("A's idle event = %v", idle)
	}
	// The client's own interrupt is on both surfaces — A's thread named on
	// the session view, omitted on A's own — like the answers to a
	// cross-posted call.
	interrupt := lastEventOfType(t, s, sid, "user.interrupt")
	if interrupt["session_thread_id"] != a {
		t.Errorf("the interrupt on the session view = %v, want session_thread_id A", interrupt)
	}
	// Consumed by the arm that ended A's turn — the only turn that could
	// ever have stamped it — so it is processed on append.
	if interrupt["processed_at"] == nil {
		t.Errorf("a child-scoped interrupt renders processed_at null: %v", interrupt)
	}
	_, own := s.do(http.MethodGet, "/v1/sessions/"+sid+"/threads/"+a+"/events", nil)
	for _, ev := range listData(t, own) {
		if _, named := ev["session_thread_id"]; ev["type"] == "user.interrupt" && named {
			t.Errorf("the interrupt on A's own view = %v, want no session_thread_id", ev)
		}
	}

	// Session-wide: B's call is answered, every live item cancelled, the
	// session idles end_turn with the primary's pair.
	sendEvents(t, s, sid, map[string]any{"type": "user.interrupt"})
	if got := s.sessionStatus(sid); got != "idle" {
		t.Errorf("session status after the session-wide interrupt = %q, want idle", got)
	}
	if n := s.liveWork(sid, queue.ToolExec); n != 0 {
		t.Errorf("live tool_exec after the session-wide interrupt = %d, want 0", n)
	}
	res = lastEventOfType(t, s, sid, "agent.tool_result")
	if res["tool_use_id"] != useB || s.threadOf(t, res["id"].(string)) != b {
		t.Errorf("second interrupt's result = %v, want B's call answered", res)
	}
	if n := countEventType(t, s, sid, "session.status_idle"); n != 1 {
		t.Errorf("session.status_idle count = %d, want the one the session-wide interrupt emits", n)
	}
}

// Inbound session_thread_id is accepted and validated (decision 9): a
// confirmation's claim must be the thread of the call it answers (the
// primary's own id names the primary), an interrupt's claim a live thread of
// this session, and the value a thread id at all.
func TestInboundThreadClaimValidation(t *testing.T) {
	s := newTestServer(t)
	sid, a, b, askID := coordinatorFixture(t, s)
	primary := domain.PrimaryThreadID(domain.ID(sid)).String()
	primaryAsk := appendOn(t, s, sid, "", false, domain.EventAgentToolUse, askBashCall)
	archived := insertChild(t, s, sid, "terminated")
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE session_threads SET archived_at = now() WHERE id = $1`, archived); err != nil {
		t.Fatal(err)
	}
	post := func(ev map[string]any) (int, map[string]any) {
		return s.do(http.MethodPost, "/v1/sessions/"+sid+"/events", map[string]any{"events": []any{ev}})
	}
	for _, tc := range []struct {
		name string
		ev   map[string]any
		want string
	}{
		{"confirmation naming the sibling", confirm(askID, "allow", map[string]any{"session_thread_id": b}),
			"does not match the thread of tool use"},
		{"confirmation naming the primary for a child's call", confirm(askID, "allow", map[string]any{"session_thread_id": primary}),
			"does not match the thread of tool use"},
		{"confirmation naming a child for the primary's call", confirm(primaryAsk, "allow", map[string]any{"session_thread_id": a}),
			"does not match the thread of tool use"},
		{"not a thread id", confirm(askID, "allow", map[string]any{"session_thread_id": "sesn_" + strings.Repeat("0", 25)}),
			"is not a session thread id"},
		{"interrupt naming an unknown thread", map[string]any{"type": "user.interrupt", "session_thread_id": "sthr_" + strings.Repeat("0", 25)},
			"does not name a thread in this session"},
		{"interrupt naming an archived thread", map[string]any{"type": "user.interrupt", "session_thread_id": archived},
			"is archived"},
	} {
		status, body := post(tc.ev)
		wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
		if msg := errMessage(body); !strings.Contains(msg, tc.want) {
			t.Errorf("%s: message %q does not mention %q", tc.name, msg, tc.want)
		}
	}
	// Nothing above moved anything.
	if got := s.threadStatus(t, a); got != "idle" {
		t.Errorf("thread A = %q after the refused batches, want idle", got)
	}
	// The primary's own id on the primary's call, and a child's on its own,
	// are accepted; the stored payload keeps session_thread_id null and the
	// event lands on the call's thread.
	if status, body := post(confirm(primaryAsk, "deny", map[string]any{"session_thread_id": primary})); status != http.StatusOK {
		t.Fatalf("primary claim: %d %v", status, body)
	}
	if status, body := post(confirm(askID, "deny", map[string]any{"session_thread_id": a})); status != http.StatusOK {
		t.Fatalf("child claim: %d %v", status, body)
	}
	var owners []string
	rows, err := s.pool.Query(context.Background(),
		`SELECT COALESCE(thread_id, '') FROM events WHERE session_id = $1 AND type = 'user.tool_confirmation' ORDER BY seq`, sid)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var tid string
		if err := rows.Scan(&tid); err != nil {
			t.Fatal(err)
		}
		owners = append(owners, tid)
	}
	rows.Close()
	if !sameStrings(owners, []string{"", a}) {
		t.Errorf("confirmation thread columns = %v, want [primary A]", owners)
	}
	ev := lastEventOfType(t, s, sid, "user.tool_confirmation")
	if ev["session_thread_id"] != a {
		t.Errorf("a child's confirmation renders its thread: %v", ev)
	}
}

// The tool_exec stop re-arm (decision 13 iii): a tool_exec stopped through
// the work API with runnable platform calls still on the log leaves a fresh
// queued item of the kind the send trigger would pick — mcp_exec when an MCP
// call is among them — and one stopped with nothing runnable leaves none,
// whether the log is fully answered, gated, or holding a call no driver runs.
func TestWorkStopReArmsForRunnableCalls(t *testing.T) {
	s := newTestServer(t)
	envID, sid, key := selfHostedWorker(t, s, "ek-rearm")
	primary := domain.ID("")
	stopForce := func(workID string) {
		wantStopped(t, s, "/v1/environments/"+envID+"/work/"+workID, key, map[string]any{"force": true})
	}

	// Runnable bash call + an MCP call: the re-arm is an mcp_exec.
	mcpID := appendOn(t, s, sid, primary, false, domain.EventAgentMCPToolUse,
		`{"name":"lookup","mcp_server_name":"srv","input":{},"evaluated_permission":"allow","session_thread_id":null}`)
	bashID := appendOn(t, s, sid, primary, false, domain.EventAgentToolUse, allowBashCall)

	workID := s.enqueueAndPoll(t, envID, sid, key)
	stopForce(workID)
	if n := s.liveWork(sid, queue.MCPExec); n != 1 {
		t.Errorf("live mcp_exec after the stop = %d, want the re-armed item", n)
	}
	if n := s.liveWork(sid, queue.ToolExec); n != 0 {
		t.Errorf("live tool_exec after the stop = %d, want 0 (mcp_exec first)", n)
	}
	var fresh string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT id FROM work_items WHERE session_id = $1 AND kind = 'mcp_exec'`, sid).Scan(&fresh); err != nil {
		t.Fatal(err)
	}
	if fresh == workID {
		t.Error("the re-armed item reuses the stopped work id")
	}

	// Everything answered: a stop leaves nothing queued. (An ask-gated call
	// is not runnable either — the runnable set, not the unanswered one.)
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE work_items SET state = 'stopped' WHERE session_id = $1 AND state <> 'stopped'`, sid); err != nil {
		t.Fatal(err)
	}
	appendOn(t, s, sid, primary, false, domain.EventAgentToolResult,
		`{"tool_use_id":"`+bashID+`","content":[],"is_error":false,"session_thread_id":null}`)
	appendOn(t, s, sid, primary, false, domain.EventAgentMCPToolResult,
		`{"tool_use_id":"`+mcpID+`","content":[],"is_error":false,"session_thread_id":null}`)
	appendOn(t, s, sid, primary, false, domain.EventAgentToolUse, askBashCall)
	workID = s.enqueueAndPoll(t, envID, sid, key)
	stopForce(workID)
	if n := s.liveWork(sid, queue.ToolExec) + s.liveWork(sid, queue.MCPExec) + s.liveWork(sid, queue.WebExec); n != 0 {
		t.Errorf("live exec items after a stop with nothing runnable = %d, want 0", n)
	}

	// Nor is a delegation call, which only the settlement that emitted one
	// answers: no worker and no driver will ever run it, so re-arming for one
	// would loop stop → re-arm → nothing runnable → stop on a session nobody
	// is driving. Unreachable today — every such call is answered in its own
	// commit — and cheap to hold here, where the classification is shared with
	// the cloud driver's own drain.
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE work_items SET state = 'stopped' WHERE session_id = $1 AND state <> 'stopped'`, sid); err != nil {
		t.Fatal(err)
	}
	appendOn(t, s, sid, primary, false, domain.EventAgentToolUse, createAgentCall)
	workID = s.enqueueAndPoll(t, envID, sid, key)
	stopForce(workID)
	if n := s.liveWork(sid, queue.ToolExec) + s.liveWork(sid, queue.MCPExec) + s.liveWork(sid, queue.WebExec); n != 0 {
		t.Errorf("live exec items after a stop with a stray delegation call = %d, want 0", n)
	}
}

// A repeat stop of a tool_exec already stopped answers the current object and
// owes no second re-arm: the first stop's commit re-armed the session, and
// whatever became of that item since is the new item's business. Re-arming
// again would hand the same calls out once more for each repeated stop, and
// the recorded reference worker repeats them routinely: each of the ten force
// stops of active work in the 2026-09-19 self-hosted recordings is followed by
// a graceful one on the same item (#804).
func TestWorkRepeatStopDoesNotReArm(t *testing.T) {
	s := newTestServer(t)
	envID, sid, key := selfHostedWorker(t, s, "ek-rearm-again")
	appendOn(t, s, sid, "", false, domain.EventAgentToolUse, allowBashCall)
	workID := s.enqueueAndPoll(t, envID, sid, key)
	get := "/v1/environments/" + envID + "/work/" + workID

	first := wantStopped(t, s, get, key, map[string]any{"force": true})
	if n := s.liveWork(sid, queue.ToolExec); n != 1 {
		t.Fatalf("live tool_exec after the first stop = %d, want the re-armed item", n)
	}
	// The re-armed item goes away by a path that owes no re-arm of its own,
	// so the call is runnable and nothing live covers it: exactly what a
	// second re-arm would act on.
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE work_items SET state = 'stopped' WHERE session_id = $1 AND id <> $2`, sid, workID); err != nil {
		t.Fatal(err)
	}

	for _, req := range []map[string]any{nil, {"force": true}} {
		if again := wantStopped(t, s, get, key, req); !reflect.DeepEqual(again, first) {
			t.Errorf("repeat stop %v = %v, want the unchanged %v", req, again, first)
		}
	}
	if n := s.liveWork(sid, queue.ToolExec) + s.liveWork(sid, queue.MCPExec) + s.liveWork(sid, queue.WebExec); n != 0 {
		t.Errorf("live exec items after repeat stops = %d, want 0 — a repeat stop re-armed the session", n)
	}
}

// The re-arm is serialized with the settlements under the session row lock:
// a stop racing a settlement that appends a call under the lock waits for
// it, then sees the call — so the call never ends up without a live item.
func TestWorkStopWaitsForTheSessionLock(t *testing.T) {
	s := newTestServer(t)
	envID, sid, key := selfHostedWorker(t, s, "ek-race")
	workID := s.enqueueAndPoll(t, envID, sid, key)
	ctx := context.Background()

	// A "settlement" holding the session lock.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, sid); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	// A raw request: the fatal-on-error helpers must not Fatal off the test
	// goroutine, and a failed request must still unblock the receive below.
	stopped := make(chan int, 1)
	go func() {
		defer wg.Done()
		req, err := http.NewRequest(http.MethodPost,
			s.url+"/v1/environments/"+envID+"/work/"+workID+"/stop",
			strings.NewReader(`{"force":true}`))
		if err != nil {
			stopped <- 0
			return
		}
		req.Header.Set("Authorization", "Bearer "+key)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			stopped <- 0
			return
		}
		res.Body.Close()
		stopped <- res.StatusCode
	}()
	select {
	case status := <-stopped:
		t.Fatalf("the stop did not wait for the session lock (status %d)", status)
	case <-time.After(300 * time.Millisecond):
	}
	// The settlement appends its call under the lock and commits; the stop
	// proceeds and finds it.
	if _, err := events.NewLog(s.pool).AppendInTx(ctx, tx, domain.ID(sid), []events.NewEvent{
		{Type: domain.EventAgentToolUse, Payload: []byte(allowBashCall)}}, events.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if status := <-stopped; status != http.StatusOK {
		t.Fatalf("stop status = %d, want 200", status)
	}
	if n := s.liveWork(sid, queue.ToolExec); n != 1 {
		t.Errorf("live tool_exec after the racing stop = %d, want the re-armed item for the call committed under the lock", n)
	}
}

// Two stops of one live item race: the second reads the item live, waits on
// the session lock the first holds, and finds the item stopped once it gets
// it. It moved nothing, so it owes no re-arm; the first stop's commit carried
// that. A stop of an item already stopped is answered before any lock is
// taken, so this race is the one path on which a stop that moved nothing
// finds a stopped item it could re-arm for (#804).
func TestWorkStopLosingARaceDoesNotReArm(t *testing.T) {
	s := newTestServer(t)
	envID, sid, key := selfHostedWorker(t, s, "ek-race-lost")
	appendOn(t, s, sid, "", false, domain.EventAgentToolUse, allowBashCall)
	workID := s.enqueueAndPoll(t, envID, sid, key)
	get := "/v1/environments/" + envID + "/work/" + workID
	ctx := context.Background()

	// The first stop, holding the session lock.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, sid); err != nil {
		t.Fatal(err)
	}
	// The second, over the wire. A raw request, for the reason
	// TestWorkStopWaitsForTheSessionLock gives.
	type answer struct {
		status int
		body   map[string]any
	}
	second := make(chan answer, 1)
	go func() {
		var a answer
		defer func() { second <- a }()
		req, err := http.NewRequest(http.MethodPost, s.url+get+"/stop", strings.NewReader(`{"force":true}`))
		if err != nil {
			return
		}
		req.Header.Set("Authorization", "Bearer "+key)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		defer res.Body.Close()
		a.status = res.StatusCode
		_ = json.NewDecoder(res.Body).Decode(&a.body)
	}()
	// Move the item only once the second stop is observably waiting on the
	// lock, so it has read the item live: had it read the item stopped, it
	// would have answered without reaching the re-arm at all.
	waitSQL := `SELECT EXISTS (
		SELECT 1 FROM pg_stat_activity
		WHERE datname = current_database()
		  AND wait_event_type = 'Lock'
		  AND query LIKE '%FROM sessions%FOR UPDATE%')`
	for deadline := time.Now().Add(10 * time.Second); ; {
		select {
		case a := <-second:
			t.Fatalf("the second stop answered %d without waiting for the session lock", a.status)
		default:
		}
		var waiting bool
		if err := s.pool.QueryRow(ctx, waitSQL).Scan(&waiting); err != nil {
			t.Fatalf("poll pg_stat_activity: %v", err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the second stop never waited on the session lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The first stop moves the item and commits. Its own re-arm is left out,
	// so the call stays runnable with nothing live covering it: exactly what
	// a second re-arm would act on.
	if _, moved, err := queue.New(s.pool).StopWith(ctx, tx, domain.ID(envID), domain.ID(workID), true); err != nil || !moved {
		t.Fatalf("the first stop: moved %v, %v", moved, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	a := <-second
	if a.status != http.StatusOK || a.body["state"] != "stopped" {
		t.Fatalf("the second stop = %d %v, want 200 with the stopped item", a.status, a.body)
	}
	if _, now, _ := s.workReq(t, http.MethodGet, get, key, nil); !reflect.DeepEqual(a.body, now) {
		t.Errorf("the second stop answered %v, GET answers %v; want the item as it stands", a.body, now)
	}
	if n := s.liveWork(sid, queue.ToolExec) + s.liveWork(sid, queue.MCPExec) + s.liveWork(sid, queue.WebExec); n != 0 {
		t.Errorf("live exec items after the lost race = %d, want 0 — the second stop re-armed the session", n)
	}
}

// An interrupt naming the primary's own id on a single-agent session is the
// session-wide interrupt spelled out: every live item is cancelled, the shared
// exec item included, exactly as the unnamed form does.
func TestInterruptNamingThePrimaryCancelsEveryItem(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	primary := domain.PrimaryThreadID(domain.ID(sid)).String()
	pgtest.SetSessionStatus(t, s.pool, domain.ID(sid), "running")
	appendOn(t, s, sid, "", false, domain.EventAgentToolUse, allowBashCall)
	envID := s.environmentID(sid)
	if _, err := queue.New(s.pool).Enqueue(context.Background(), s.pool, domain.ID(envID), domain.ID(sid), queue.ToolExec); err != nil {
		t.Fatal(err)
	}

	sendEvents(t, s, sid, map[string]any{"type": "user.interrupt", "session_thread_id": primary})

	if got := s.sessionStatus(sid); got != "idle" {
		t.Errorf("session status = %q, want idle", got)
	}
	if n := s.liveWork(sid, queue.ToolExec); n != 0 {
		t.Errorf("live tool_exec = %d, want 0 — the primary is the only thread, so the interrupt is the session's", n)
	}
	if n := countEventType(t, s, sid, "session.status_idle"); n != 1 {
		t.Errorf("session.status_idle count = %d, want 1", n)
	}
}

// Archiving an idle child whose stop reason the session's idle pick carried
// re-advertises the pick payload-only — a retries_exhausted child beside a
// primary parked on end_turn, not only the requires_action case — and one
// whose stop reason the pick never carried re-advertises nothing.
func TestThreadArchiveReadvertisesTheIdlePick(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	setThread(t, s, domain.PrimaryThreadID(domain.ID(sid)).String(), "idle", `{"type":"end_turn"}`)
	exhausted := insertChild(t, s, sid, "idle")
	setThread(t, s, exhausted, "idle", `{"type":"retries_exhausted"}`)
	quiet := insertChild(t, s, sid, "idle")
	setThread(t, s, quiet, "idle", `{"type":"end_turn"}`)
	path := "/v1/sessions/" + sid + "/threads/"

	if status, res := s.do(http.MethodPost, path+quiet+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive the end_turn child: %d %v", status, res)
	}
	if n := countEventType(t, s, sid, "session.status_idle"); n != 0 {
		t.Errorf("archiving a child the pick never carried re-idled the session %d times, want 0", n)
	}
	if status, res := s.do(http.MethodPost, path+exhausted+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive the retries_exhausted child: %d %v", status, res)
	}
	if n := countEventType(t, s, sid, "session.status_idle"); n != 1 {
		t.Fatalf("archiving the child the pick carried re-idled the session %d times, want once", n)
	}
	if got := stopReasonType(t, lastEventOfType(t, s, sid, "session.status_idle")); got != "end_turn" {
		t.Errorf("re-advertised stop reason = %q, want end_turn (what remains)", got)
	}
	if got := s.sessionStatus(sid); got != "idle" {
		t.Errorf("session status = %q, want idle", got)
	}
}

// A wind-down the worker never finishes is finalized by the next poll and its
// session re-armed, so the poll hands the fresh item straight out: the calls
// the abandoned item covered are nobody's otherwise (decision 13 iii).
func TestWorkPollFinalizesAnAbandonedStopAndReArms(t *testing.T) {
	s := newTestServer(t)
	envID, sid, key := selfHostedWorker(t, s, "ek-abandon")
	appendOn(t, s, sid, "", false, domain.EventAgentToolUse, allowBashCall)
	workID := s.enqueueAndPoll(t, envID, sid, key)
	get := "/v1/environments/" + envID + "/work/" + workID
	if res, _, raw := s.workReq(t, http.MethodPost, get+"/ack", key, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("ack: %d %s", res.StatusCode, raw)
	}
	if res, _, raw := s.workReq(t, http.MethodPost, get+"/heartbeat?expected_last_heartbeat=NO_HEARTBEAT", key, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", res.StatusCode, raw)
	}
	wantStopped(t, s, get, key, nil) // graceful: stopping
	// The worker dies mid-wind-down: its lease lapses, and queue.WindDown passes.
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE work_items SET lease_expires_at = now() - interval '1 second', stop_requested_at = now() - interval '61 seconds' WHERE id = $1`, workID); err != nil {
		t.Fatal(err)
	}

	res, raw := s.poll(t, envID, map[string]string{"Authorization": "Bearer " + key})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("poll: %d %s", res.StatusCode, raw)
	}
	var handed map[string]any
	if err := json.Unmarshal([]byte(raw), &handed); err != nil {
		t.Fatal(err)
	}
	if id, _ := handed["id"].(string); id == "" || id == workID {
		t.Fatalf("poll handed back %q, want a fresh re-armed item (the abandoned one was %s)", id, workID)
	}
	_, old, _ := s.workReq(t, http.MethodGet, get, key, nil)
	if old["state"] != "stopped" {
		t.Errorf("the abandoned item = %v, want stopped", old["state"])
	}
}

// A session-wide interrupt ending several parked children re-idles the
// session once, with the fold after the last of them — not once per child.
func TestSessionWideInterruptReidlesOnce(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	setThread(t, s, domain.PrimaryThreadID(domain.ID(sid)).String(), "idle", `{"type":"end_turn"}`)
	var asks []string
	for range 2 {
		child := insertChild(t, s, sid, "idle")
		ask := appendOn(t, s, sid, domain.ID(child), true, domain.EventAgentToolUse, askBashCall)
		setThread(t, s, child, "idle", `{"type":"requires_action","event_ids":["`+ask+`"]}`)
		asks = append(asks, ask)
	}

	sendEvents(t, s, sid, map[string]any{"type": "user.interrupt"})

	if n := countEventType(t, s, sid, "session.status_idle"); n != 1 {
		t.Errorf("session.status_idle count = %d, want 1", n)
	}
	if got := stopReasonType(t, lastEventOfType(t, s, sid, "session.status_idle")); got != "end_turn" {
		t.Errorf("re-idle stop reason = %q, want end_turn (every ask answered)", got)
	}
	if n := countEventType(t, s, sid, "session.thread_status_idle"); n != 2 {
		t.Errorf("thread idle events = %d, want one per child", n)
	}
	if n := countEventType(t, s, sid, "agent.tool_result"); n != len(asks) {
		t.Errorf("results = %d, want both asks answered", n)
	}
}
