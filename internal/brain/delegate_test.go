package brain_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// Settlement-executed delegation (plan 35 slice 4, decisions 6, 7 and 8): the
// six tools are resolved inside the transaction that commits the turn calling
// them, every call is answered in that same commit, and the commit schedules
// whatever the turn left to do — a spawned child's turn, a woken parent's, or
// the caller's own next one.

// roster makes the fixture session a coordinator over the named agents. The
// members carry the SessionThreadAgent shape the API writes
// (internal/api/roster.go), because a spawn stores a member's bytes as they
// are.
func (h *harness) roster(t *testing.T, names ...string) {
	t.Helper()
	members := make([]string, len(names))
	for i, n := range names {
		members[i] = fmt.Sprintf(`{"id":"agent_%s","type":"agent","version":1,"name":%q,"description":"",`+
			`"model":{"id":"fixture-model"},"system":"","tools":[],"mcp_servers":[],"skills":[]}`, n, n)
	}
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET resolved_agent = jsonb_set(resolved_agent, '{multiagent}', $2::jsonb)
		  WHERE id = $1`,
		h.sessionID.String(), `{"type":"coordinator","agents":[`+strings.Join(members, ",")+`]}`); err != nil {
		t.Fatalf("roster fixture: %v", err)
	}
}

// The two answers wait_for_agents can give, spelled out. They are what the
// probe inside the call decides, and — because the settlement's terminal
// branch chains on the same input either way — the only thing about a wait
// that a test can see and the chain cannot also produce.
const (
	waitStartedAnswer      = `{"message":"Wait started. Reports arrive as messages; do not conclude yet.","timed_out":false}`
	nothingToWaitForAnswer = `{"message":"No agents are running and no reports are pending.","timed_out":true}`
)

// toolCall is toolUseChunk with an input of the caller's choosing.
func toolCall(id, name, input string) provider.Chunk {
	return provider.Chunk{Kind: provider.KindToolUse, ToolUse: &provider.ToolUse{
		ID: id, Name: name, Input: json.RawMessage(input)}}
}

// liveTurns counts one thread's not-yet-finished model_turn items; the empty
// id is the primary's.
func (h *harness) liveTurns(t *testing.T, tid domain.ID) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM work_items WHERE session_id = $1 AND kind = 'model_turn'
		   AND thread_id IS NOT DISTINCT FROM $2 AND state <> 'stopped'`,
		h.sessionID.String(), events.NullableThread(tid)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

type childRow struct {
	id     domain.ID
	name   string
	status string
}

// children lists the session's child thread rows in creation order.
func (h *harness) children(t *testing.T) []childRow {
	t.Helper()
	rows, err := h.pool.Query(context.Background(),
		`SELECT id, agent_name, status FROM session_threads
		  WHERE session_id = $1 AND parent_thread_id IS NOT NULL ORDER BY created_at, id`,
		h.sessionID.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []childRow
	for rows.Next() {
		var c childRow
		var id string
		if err := rows.Scan(&id, &c.name, &c.status); err != nil {
			t.Fatal(err)
		}
		c.id = domain.ID(id)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// builtins offers the fixture agent the built-in toolset, so a call it makes
// is the platform's to run rather than a client-executed custom one.
func (h *harness) builtins(t *testing.T) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET resolved_agent = jsonb_set(resolved_agent, '{tools}',
		   '[{"type":"agent_toolset_20260401"}]'::jsonb) WHERE id = $1`, h.sessionID.String()); err != nil {
		t.Fatalf("builtins fixture: %v", err)
	}
}

// runningChild plants a child thread already running — what gives a
// wait_for_agents something to wait for.
func (h *harness) runningChild(t *testing.T) domain.ID {
	t.Helper()
	child := pgtest.NewChildThread(t, h.pool, h.sessionID)
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE session_threads SET status = 'running' WHERE id = $1`, child.String()); err != nil {
		t.Fatal(err)
	}
	return child
}

type toolAnswer struct {
	text  string
	isErr bool
}

// answers reads every agent.tool_result on the log in order — the settlement's
// own answers to the delegation calls.
func (h *harness) answers(t *testing.T) []toolAnswer {
	t.Helper()
	evs, err := h.log.List(context.Background(), h.sessionID,
		events.ListQuery{Types: []string{string(domain.EventAgentToolResult)}})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]toolAnswer, 0, len(evs))
	for _, ev := range evs {
		var p struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"is_error"`
		}
		if err := json.Unmarshal(ev.Body, &p); err != nil {
			t.Fatalf("tool result %s: %v", ev.ID, err)
		}
		a := toolAnswer{isErr: p.IsError}
		if len(p.Content) > 0 {
			a.text = p.Content[0].Text
		}
		out = append(out, a)
	}
	return out
}

// threadTypes lists the event types stored on one thread's own rows.
func (h *harness) threadTypes(t *testing.T, tid domain.ID) []string {
	t.Helper()
	evs, err := h.log.List(context.Background(), h.sessionID,
		events.ListQuery{Scope: events.ScopeThread, ThreadID: tid})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = string(ev.Type)
	}
	return out
}

// receivedTexts reads the messages delivered to one thread.
func (h *harness) receivedTexts(t *testing.T, tid domain.ID) []string {
	t.Helper()
	evs, err := h.log.List(context.Background(), h.sessionID, events.ListQuery{
		Scope: events.ScopeThread, ThreadID: tid,
		Types: []string{string(domain.EventAgentThreadMessageReceived)}})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(evs))
	for i, ev := range evs {
		var p struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(ev.Body, &p); err != nil {
			t.Fatal(err)
		}
		if len(p.Content) > 0 {
			out[i] = p.Content[0].Text
		}
	}
	return out
}

// The plan's headline shape: one turn, one commit. Three spawns and a wait
// leave three running threads with three queued turns, the four projection
// events each spawn owes, four answers, a parked coordinator — and no work
// item for any driver, because no driver can run a delegation call.
func TestCoordinatorSpawnsThreeAgentsAndParksInOneCommit(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		toolCall("t1", "create_agent", `{"agent_name":"researcher","message":"find the papers"}`),
		toolCall("t2", "create_agent", `{"agent_name":"writer","message":"draft the summary"}`),
		toolCall("t3", "create_agent", `{"agent_name":"reviewer","message":"review the draft"}`),
		toolCall("t4", "wait_for_agents", `{}`),
		done("tool_use", 4),
	}}, nil)
	h.roster(t, "researcher", "writer", "reviewer")
	h.wake(t, "coordinate the work")
	h.runOnce(t)

	kids := h.children(t)
	if len(kids) != 3 {
		t.Fatalf("child threads = %d, want 3", len(kids))
	}
	// Three spawns in one commit, and each child reads its own call's message
	// and no sibling's: the task is the whole of what a child is told, so a
	// crossed pair is a session doing the wrong work in silence.
	tasks := map[string]string{
		"researcher": "find the papers",
		"writer":     "draft the summary",
		"reviewer":   "review the draft",
	}
	for _, k := range kids {
		if k.status != "running" {
			t.Errorf("child %s (%s) = %q, want running from birth", k.id, k.name, k.status)
		}
		if n := h.liveTurns(t, k.id); n != 1 {
			t.Errorf("child %s has %d queued turns, want 1", k.id, n)
		}
		if got := h.threadTypes(t, k.id); !slicesEq(got,
			[]string{"agent.thread_message_received", "session.thread_status_running"}) {
			t.Errorf("child %s log = %v, want its task and its status", k.id, got)
		}
		if got := h.receivedTexts(t, k.id); len(got) != 1 || got[0] != tasks[k.name] {
			t.Errorf("child %s (%s) received %v, want its own task %q", k.id, k.name, got, tasks[k.name])
		}
	}
	for _, typ := range []string{"session.thread_created", "agent.thread_message_sent"} {
		if n := h.countType(t, typ); n != 3 {
			t.Errorf("%s = %d, want one per spawn", typ, n)
		}
	}
	got := h.answers(t)
	if len(got) != 4 {
		t.Fatalf("answers = %v, want one per call", got)
	}
	for i, a := range got {
		if a.isErr {
			t.Errorf("answer %d is an error: %s", i, a.text)
		}
	}
	// Each spawn is answered with its own thread's id. The three rows share a
	// created_at — one transaction — so they come back ordered by id rather
	// than by spawn order, and the ids are compared as a set.
	answered := map[string]bool{}
	for _, a := range got[:3] {
		answered[a.text] = true
	}
	for _, k := range kids {
		if want := `{"session_thread_id":"` + k.id.String() + `"}`; !answered[want] {
			t.Errorf("no spawn answered %s; answers were %v", want, got[:3])
		}
	}
	if got[3].text != waitStartedAnswer {
		t.Errorf("wait answered %q", got[3].text)
	}

	// The wait parks the coordinator: idle end_turn, nothing enqueued for it.
	if s := h.threadStatus(t, domain.PrimaryThreadID(h.sessionID)); s != "idle" {
		t.Errorf("coordinator = %q, want idle on the wait", s)
	}
	if n := h.liveTurns(t, ""); n != 0 {
		t.Errorf("the parked coordinator has %d queued turns, want none", n)
	}
	// The session stays running under its children, and no driver was asked
	// for anything.
	if s := h.status(t); s != "running" {
		t.Errorf("session = %q, want running while its children run", s)
	}
	if n := h.countType(t, "session.status_idle"); n != 0 {
		t.Errorf("session idled while three children run")
	}
	if n := h.liveOf(t, queue.ToolExec); n != 0 {
		t.Errorf("tool_exec items = %d, want none", n)
	}
	if n := h.countType(t, "session.thread_status_idle"); n != 1 {
		t.Errorf("thread_status_idle = %d, want the coordinator's park alone", n)
	}
}

// A turn whose calls were all settlement-executed and none of them a wait or
// a report has nothing left to wait for, so the same commit hands the item
// back for the caller's next turn — the executor's rule after its last answer.
func TestAllSettlementTurnChainsTheCallersNextTurn(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		toolCall("t1", "create_agent", `{"agent_name":"researcher","message":"find the papers"}`),
		toolCall("t2", "list_agents", `{}`),
		done("tool_use", 2),
	}}, nil)
	h.roster(t, "researcher")
	h.wake(t, "delegate")
	h.runOnce(t)

	if s := h.threadStatus(t, domain.PrimaryThreadID(h.sessionID)); s != "running" {
		t.Errorf("coordinator = %q, want running with its next turn queued", s)
	}
	if n := h.liveTurns(t, ""); n != 1 {
		t.Fatalf("coordinator turns queued = %d, want 1", n)
	}
	kids := h.children(t)
	if len(kids) != 1 {
		t.Fatalf("children = %v", kids)
	}
	got := h.answers(t)
	if len(got) != 2 {
		t.Fatalf("answers = %v", got)
	}
	want := fmt.Sprintf(`[{"session_thread_id":%q,"agent_name":"researcher","status":"running"}]`, kids[0].id)
	if got[1].text != want {
		t.Errorf("list_agents answered %s, want %s", got[1].text, want)
	}
}

// A wait with no child running and no report unread would park a thread
// nothing can wake, so it times out in-commit and the turn continues.
func TestWaitWithNothingToWaitForDoesNotPark(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		toolCall("t1", "wait_for_agents", `{}`),
		done("tool_use", 1),
	}}, nil)
	h.roster(t, "researcher")
	h.wake(t, "wait for nobody")
	h.runOnce(t)

	got := h.answers(t)
	if len(got) != 1 || got[0].isErr {
		t.Fatalf("answers = %v", got)
	}
	if got[0].text != nothingToWaitForAnswer {
		t.Errorf("wait answered %q", got[0].text)
	}
	if s := h.threadStatus(t, domain.PrimaryThreadID(h.sessionID)); s != "running" {
		t.Errorf("coordinator = %q, want running — the wait had nothing to park on", s)
	}
	if n := h.liveTurns(t, ""); n != 1 {
		t.Errorf("coordinator turns queued = %d, want its next turn", n)
	}
}

// Two children reporting at once cost one queued parent turn: they serialize
// on the session row lock, the first finds the parent idle and wakes it, the
// second finds it running and leaves the chain to the parent's own settlement.
func TestTwoReportsWakeTheIdleParentOnce(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		{toolCall("t1", "submit_result", `{"result":"found three papers"}`), done("tool_use", 1)},
		{toolCall("t2", "submit_result", `{"result":"draft ready"}`), done("tool_use", 1)},
	}, nil)
	first := h.childTurn(t, "find the papers")
	second := h.childTurn(t, "draft the summary")
	h.runOnce(t)
	h.runOnce(t)

	if n := h.liveTurns(t, ""); n != 1 {
		t.Errorf("parent turns queued = %d, want exactly one for two reports", n)
	}
	if s := h.threadStatus(t, domain.PrimaryThreadID(h.sessionID)); s != "running" {
		t.Errorf("parent = %q, want woken", s)
	}
	for _, child := range []domain.ID{first, second} {
		if s := h.threadStatus(t, child); s != "idle" {
			t.Errorf("child %s = %q, want idle — submit_result ends the turn", child, s)
		}
		if n := h.liveTurns(t, child); n != 0 {
			t.Errorf("reported child %s still has %d turns queued", child, n)
		}
	}
	if got := h.receivedTexts(t, ""); len(got) != 2 {
		t.Errorf("reports on the parent's log = %v, want both", got)
	}
	if n := h.countType(t, "session.status_idle"); n != 0 {
		t.Errorf("the session idled between a child's report and the parent's wake")
	}
	// Counted before it is inspected: an unanswered delegation call is the one
	// thing this settlement may not commit, and a bare range over the answers
	// would pass on an empty set — leaving every other assertion here true of a
	// run where the reports were never acknowledged at all.
	answers := h.answers(t)
	if len(answers) != 2 {
		t.Fatalf("answers = %v, want one per report", answers)
	}
	for _, a := range answers {
		if a.isErr || a.text != "Result reported." {
			t.Errorf("answer = %+v", a)
		}
	}
}

// The cap is 25 live threads with the primary counted, so the spawn that
// would make a 26th is refused — and refused as an answer, not as a failure:
// the turn goes on.
func TestSpawnBeyondTheThreadCapIsRefused(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		toolCall("t1", "create_agent", `{"agent_name":"researcher","message":"go"}`),
		done("tool_use", 1),
	}}, nil)
	h.roster(t, "researcher")
	for i := 0; i < 24; i++ {
		pgtest.NewChildThread(t, h.pool, h.sessionID)
	}
	h.wake(t, "spawn one more")
	h.runOnce(t)

	if n := len(h.children(t)); n != 24 {
		t.Errorf("children = %d, want the 24 already there", n)
	}
	got := h.answers(t)
	if len(got) != 1 || !got[0].isErr || !strings.Contains(got[0].text, "25") {
		t.Fatalf("answer = %+v, want an is_error naming the cap", got)
	}
}

// A spawn for a name the roster does not carry is answered, not obeyed, and
// the answer names what the roster does list.
func TestSpawnOfAnUnknownAgentIsRefused(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		toolCall("t1", "create_agent", `{"agent_name":"nobody","message":"go"}`),
		done("tool_use", 1),
	}}, nil)
	h.roster(t, "researcher", "writer")
	h.wake(t, "spawn a stranger")
	h.runOnce(t)

	if n := len(h.children(t)); n != 0 {
		t.Errorf("children = %d, want none", n)
	}
	got := h.answers(t)
	if len(got) != 1 || !got[0].isErr ||
		!strings.Contains(got[0].text, "researcher") || !strings.Contains(got[0].text, "writer") {
		t.Fatalf("answer = %+v, want an is_error listing the roster", got)
	}
}

// Reporting and working at once would end the turn with its other calls
// unanswered, so the report is refused and nothing ends.
func TestSubmitResultSharingItsTurnWithAToolCallReportsNothing(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		toolUseChunk("t1", "lookup"),
		toolCall("t2", "submit_result", `{"result":"done"}`),
		done("tool_use", 2),
	}}, nil)
	child := h.childTurn(t, "do the work")
	h.runOnce(t)

	got := h.answers(t)
	if len(got) != 1 || !got[0].isErr || !strings.Contains(got[0].text, "after your tool calls") {
		t.Fatalf("answer = %+v, want the report refused", got)
	}
	if s := h.threadStatus(t, child); s != "running" {
		t.Errorf("child = %q, want still running on its own tool call", s)
	}
	if n := h.countType(t, "agent.thread_message_sent"); n != 0 {
		t.Errorf("a refused report still messaged the coordinator")
	}
}

// send_to_agent addresses by agent name when one live thread runs it, and the
// delivery wakes a target nothing else would move.
func TestSendToAgentWakesAnIdleChild(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		toolCall("t1", "send_to_agent", `{"agent_name":"worker","message":"one more thing"}`),
		done("tool_use", 1),
	}}, nil)
	h.roster(t, "worker")
	child := pgtest.NewChildThread(t, h.pool, h.sessionID)
	h.wake(t, "follow up")
	h.runOnce(t)

	got := h.answers(t)
	if len(got) != 1 || got[0].isErr || got[0].text != "Message sent." {
		t.Fatalf("answer = %+v", got)
	}
	if s := h.threadStatus(t, child); s != "running" {
		t.Errorf("target = %q, want woken", s)
	}
	if n := h.liveTurns(t, child); n != 1 {
		t.Errorf("target turns queued = %d, want 1", n)
	}
	if got := h.receivedTexts(t, child); len(got) != 1 || got[0] != "one more thing" {
		t.Errorf("target received %v", got)
	}
}

// Two threads running one agent make the name ambiguous; the answer names the
// ids so the model can address one.
func TestSendToAgentRefusesAnAmbiguousName(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		toolCall("t1", "send_to_agent", `{"agent_name":"worker","message":"hello"}`),
		done("tool_use", 1),
	}}, nil)
	h.roster(t, "worker")
	one := pgtest.NewChildThread(t, h.pool, h.sessionID)
	two := pgtest.NewChildThread(t, h.pool, h.sessionID)
	h.wake(t, "follow up")
	h.runOnce(t)

	got := h.answers(t)
	if len(got) != 1 || !got[0].isErr ||
		!strings.Contains(got[0].text, one.String()) || !strings.Contains(got[0].text, two.String()) {
		t.Fatalf("answer = %+v, want both candidate ids", got)
	}
	if n := h.countType(t, "agent.thread_message_sent"); n != 0 {
		t.Errorf("an unaddressed message was still delivered")
	}
}

// Every malformed or unaddressable call is answered rather than dropped: an
// unanswered delegation call is one no driver can ever resolve, so the turn
// must carry an is_error back to the model instead.
func TestMalformedCoordinatorCallsAreAnswered(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.provider.scripts = [][]provider.Chunk{{
		toolCall("t1", "create_agent", `{}`),
		toolCall("t2", "send_to_agent", `{"message":"hello"}`),
		toolCall("t3", "send_to_agent",
			`{"session_thread_id":"`+domain.PrimaryThreadID(h.sessionID).String()+`","message":"hello"}`),
		toolCall("t4", "send_to_agent", `{"agent_name":"ghost","message":"hello"}`),
		done("tool_use", 4),
	}}
	h.roster(t, "researcher")
	h.wake(t, "make four mistakes")
	h.runOnce(t)

	got := h.answers(t)
	if len(got) != 4 {
		t.Fatalf("answers = %v, want one per call", got)
	}
	for i, want := range []string{"agent_name and message", "session_thread_id or an agent_name",
		"your own thread", "no live thread runs"} {
		if !got[i].isErr || !strings.Contains(got[i].text, want) {
			t.Errorf("answer %d = %+v, want an is_error mentioning %q", i, got[i], want)
		}
	}
	if n := len(h.children(t)); n != 0 {
		t.Errorf("children = %d, want none", n)
	}
}

// A thread that calls the other role's tool is answered, not left holding it.
// The brain offers a child two names and a coordinator four, so the other half
// is a tool this thread never had — and an unanswered call is the one thing a
// settlement may not commit: with no result and no enqueue the thread stays
// running forever, and because the session's status folds over its threads, the
// session cannot be archived either. Only an interrupt would end it.
func TestACallFromTheWrongRoleIsAnsweredRatherThanStranded(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		toolCall("t1", "wait_for_agents", `{}`),
		done("tool_use", 1),
	}}, nil)
	child := h.childTurn(t, "do the work")
	h.runOnce(t)

	got := h.answers(t)
	if len(got) != 1 {
		t.Fatalf("answers = %v, want the call answered", got)
	}
	if !got[0].isErr || !strings.Contains(got[0].text, "submit_result") {
		t.Errorf("answer = %+v, want an is_error pointing the child at its own tools", got[0])
	}
	// The turn settled, so the child carries on rather than wedging: a chained
	// turn is queued and nothing spawned.
	if n := h.liveTurns(t, child); n != 1 {
		t.Errorf("queued turns = %d, want the settlement to have chained one", n)
	}
	if n := len(h.children(t)); n != 1 {
		t.Errorf("children = %d, want only the child running this turn — a child cannot spawn", n)
	}
}

// wrongRole has two arms and the test above drives one. This is the other: a
// coordinator reaching for a worker's tool. It matters as much, because the two
// halves fail in opposite directions — a stranded child leaves the session
// unarchivable, while a stranded coordinator leaves the user with no answer and
// every child's report unread — and because the answer has to name the tool
// that would have worked, which is a different tool on each side.
func TestACoordinatorCallingAWorkersToolIsAnsweredToo(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		toolCall("t1", "submit_result", `{"result":"all done"}`),
		done("tool_use", 1),
	}}, nil)
	h.roster(t, "researcher")
	h.wake(t, "report to yourself")
	h.runOnce(t)

	got := h.answers(t)
	if len(got) != 1 {
		t.Fatalf("answers = %v, want the call answered", got)
	}
	if !got[0].isErr || !strings.Contains(got[0].text, "send_to_agent") {
		t.Errorf("answer = %+v, want an is_error pointing the coordinator at its own tools", got[0])
	}
	// Answered, not obeyed: the coordinator's turn is not a child's report, so
	// nothing ended and its next turn is queued.
	if s := h.threadStatus(t, domain.PrimaryThreadID(h.sessionID)); s != "running" {
		t.Errorf("coordinator = %q, want running — an is_error ends no turn", s)
	}
	if n := h.liveTurns(t, ""); n != 1 {
		t.Errorf("coordinator turns queued = %d, want the settlement to have chained one", n)
	}
}

// The child's two tools refuse an empty payload the same way.
func TestMalformedChildCallsAreAnswered(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		toolCall("t1", "submit_result", `{}`),
		toolCall("t2", "send_to_parent", `{}`),
		done("tool_use", 2),
	}}, nil)
	child := h.childTurn(t, "do the work")
	h.runOnce(t)

	got := h.answers(t)
	if len(got) != 2 {
		t.Fatalf("answers = %v, want one per call", got)
	}
	for i, want := range []string{"submit_result needs a result", "send_to_parent needs a message"} {
		if !got[i].isErr || !strings.Contains(got[i].text, want) {
			t.Errorf("answer %d = %+v, want an is_error mentioning %q", i, got[i], want)
		}
	}
	if s := h.threadStatus(t, child); s != "running" {
		t.Errorf("child = %q, want still running — a refused report ends nothing", s)
	}
	if n := h.countType(t, "agent.thread_message_sent"); n != 0 {
		t.Errorf("a refused call still messaged the coordinator")
	}
}

// send_to_parent reports progress without ending anything: the child's turn
// carries on, and the coordinator is woken to read the message.
func TestSendToParentLeavesTheChildsTurnRunning(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		toolCall("t1", "send_to_parent", `{"message":"halfway there"}`),
		done("tool_use", 1),
	}}, nil)
	child := h.childTurn(t, "do the work")
	h.runOnce(t)

	got := h.answers(t)
	if len(got) != 1 || got[0].isErr || got[0].text != "Message sent." {
		t.Fatalf("answer = %+v", got)
	}
	if s := h.threadStatus(t, child); s != "running" {
		t.Errorf("child = %q, want still running — send_to_parent ends nothing", s)
	}
	if n := h.liveTurns(t, child); n != 1 {
		t.Errorf("child turns queued = %d, want its next turn", n)
	}
	if got := h.receivedTexts(t, ""); len(got) != 1 || got[0] != "halfway there" {
		t.Errorf("coordinator received %v", got)
	}
	if n := h.liveTurns(t, ""); n != 1 {
		t.Errorf("coordinator turns queued = %d, want 1", n)
	}
}

// Nothing re-enqueues a thread that exhausted its retries, so acknowledging a
// message to one would be a lie the coordinator acts on.
func TestSendToAgentRefusesAThreadOutOfRetries(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		toolCall("t1", "send_to_agent", `{"agent_name":"worker","message":"try again"}`),
		done("tool_use", 1),
	}}, nil)
	h.roster(t, "worker")
	child := pgtest.NewChildThread(t, h.pool, h.sessionID)
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE session_threads SET stop_reason = '{"type":"retries_exhausted"}'::jsonb WHERE id = $1`,
		child.String()); err != nil {
		t.Fatal(err)
	}
	h.wake(t, "nudge it")
	h.runOnce(t)

	got := h.answers(t)
	if len(got) != 1 || !got[0].isErr || !strings.Contains(got[0].text, "retries") {
		t.Fatalf("answer = %+v, want the stopped thread named", got)
	}
	if n := h.countType(t, "agent.thread_message_sent"); n != 0 {
		t.Errorf("a message was delivered to a thread nothing will wake")
	}
	if s := h.threadStatus(t, child); s != "idle" {
		t.Errorf("target = %q, want left alone", s)
	}
}

// A wait parks only on work still to come. A report already waiting above the
// turn's watermark is something to read, not something to wait for, so the
// turn chains instead — otherwise the coordinator would park on a message it
// already has. The answer is asserted whole: with a child still running the
// thread state below would look the same either way (the terminal branch
// chains on that same report), so the answer is the part of this that only
// the wait can produce.
func TestWaitDoesNotParkOnAnUnreadReport(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		toolCall("t1", "wait_for_agents", `{}`),
		done("tool_use", 1),
	}}, nil)
	h.roster(t, "researcher")
	child := h.runningChild(t)
	h.wake(t, "coordinate")
	h.provider.onGenerate = func(callIndex int) {
		if callIndex != 0 {
			return
		}
		_, received, err := events.ThreadMessage(h.sessionID,
			events.ThreadPeer{ThreadID: child, AgentName: "worker"}, events.ThreadPeer{}, "found three papers")
		if err != nil {
			t.Errorf("build report: %v", err)
			return
		}
		if _, err := h.log.Append(context.Background(), h.sessionID, []events.NewEvent{received}); err != nil {
			t.Errorf("deliver report: %v", err)
		}
	}
	h.runOnce(t)

	got := h.answers(t)
	if len(got) != 1 || got[0].isErr || got[0].text != waitStartedAnswer {
		t.Fatalf("answer = %+v, want exactly %s", got, waitStartedAnswer)
	}
	if s := h.threadStatus(t, domain.PrimaryThreadID(h.sessionID)); s != "running" {
		t.Errorf("coordinator = %q, want chained on the report it already has", s)
	}
	if n := h.liveTurns(t, ""); n != 1 {
		t.Errorf("coordinator turns queued = %d, want the chained one", n)
	}
}

// A child that runs out of retries stops for good, and only a message on the
// primary's log says so — with the primary woken to read it.
func TestChildTerminalFailureTellsTheCoordinator(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{}}, []error{errors.New("the endpoint refused")})
	child := h.childTurn(t, "do the work")
	h.runOnce(t)

	if s := h.threadStatus(t, child); s != "idle" {
		t.Errorf("child = %q, want idle", s)
	}
	got := h.receivedTexts(t, "")
	if len(got) != 1 || !strings.Contains(got[0], "worker") || !strings.Contains(got[0], "retries") {
		t.Fatalf("coordinator was told %v, want the child's end named", got)
	}
	if s := h.threadStatus(t, domain.PrimaryThreadID(h.sessionID)); s != "running" {
		t.Errorf("coordinator = %q, want woken to read the notice", s)
	}
	if n := h.liveTurns(t, ""); n != 1 {
		t.Errorf("coordinator turns queued = %d, want 1", n)
	}
	// The wake moved the session column and the child's own idle left it
	// there: the net is what the settlement must record, not the last move.
	if s := h.status(t); s != "running" {
		t.Errorf("session = %q, want running with its woken coordinator", s)
	}
}

// A report that lands while the coordinator's own turn is running is not
// pending input — an agent.* event is stamped processed at write — so the
// settlement finds it by seq and chains rather than idling past it.
func TestReportLandingMidTurnChainsTheCoordinator(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		agentReply("still thinking"),
		agentReply("read the report"),
	}, nil)
	h.roster(t, "researcher")
	h.wake(t, "coordinate")
	h.provider.onGenerate = func(callIndex int) {
		if callIndex != 0 {
			return
		}
		_, received, err := events.ThreadMessage(h.sessionID,
			events.ThreadPeer{ThreadID: "sthr_child", AgentName: "researcher"}, events.ThreadPeer{}, "found three papers")
		if err != nil {
			t.Errorf("build report: %v", err)
			return
		}
		if _, err := h.log.Append(context.Background(), h.sessionID, []events.NewEvent{received}); err != nil {
			t.Errorf("deliver report: %v", err)
		}
	}
	h.runOnce(t)

	if s := h.threadStatus(t, domain.PrimaryThreadID(h.sessionID)); s != "running" {
		t.Errorf("coordinator = %q, want chained on the unread report", s)
	}
	if n := h.liveTurns(t, ""); n != 1 {
		t.Errorf("coordinator turns queued = %d, want the chained one", n)
	}
	if n := h.countType(t, "session.status_idle"); n != 0 {
		t.Errorf("the coordinator idled past an unread report")
	}
}

// A report that lands while the primary runs its GRADING turn is the same
// unread message, and the grading settlement makes the same check: grading
// consumes no input, so its inbound half stays unfiltered, and the report
// half is found by seq past the head the cycle read.
func TestReportLandingDuringGradingChainsThePrimary(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		agentReply("delegated"),
		graderReply("all criteria met", "satisfied"),
		agentReply("read the report"),
	}, nil)
	h.wakeOutcome(t, "Build a DCF model", 3)
	h.runOnce(t) // the agent's end_turn schedules the evaluation cycle
	h.provider.onGenerate = func(callIndex int) {
		if callIndex != 1 { // the grader's own call
			return
		}
		_, received, err := events.ThreadMessage(h.sessionID,
			events.ThreadPeer{ThreadID: "sthr_child", AgentName: "researcher"}, events.ThreadPeer{}, "found three papers")
		if err != nil {
			t.Errorf("build report: %v", err)
			return
		}
		if _, err := h.log.Append(context.Background(), h.sessionID, []events.NewEvent{received}); err != nil {
			t.Errorf("deliver report: %v", err)
		}
	}
	h.runOnce(t) // the grading turn

	if s := h.status(t); s != "running" {
		t.Errorf("session after the verdict = %q, want chained on the report that landed mid-grade", s)
	}
	if n := h.liveWork(t); n != 1 {
		t.Errorf("live items after the verdict = %d, want the chained turn", n)
	}
	if evals := h.outcomes(t); len(evals) != 1 || evals[0].Result != domain.OutcomeResultSatisfied {
		t.Errorf("outcomes = %+v, want the verdict settled all the same", evals)
	}
}

// The grading cycle's other settlement makes the same check: a grader call
// that failed renders no verdict, but the report that landed while it ran is
// still unread, so the failure chains the primary instead of idling it
// retries_exhausted with a message nothing would ever read.
func TestReportLandingDuringAFailedGradeChainsThePrimary(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		agentReply("delegated"),
		{}, // the grader's own call fails
	}, []error{nil, contextualError("model endpoint 500")})
	h.wakeOutcome(t, "Build a DCF model", 3)
	h.runOnce(t) // the agent's end_turn schedules the evaluation cycle
	h.provider.onGenerate = func(callIndex int) {
		if callIndex != 1 { // the grader's own call
			return
		}
		_, received, err := events.ThreadMessage(h.sessionID,
			events.ThreadPeer{ThreadID: "sthr_child", AgentName: "researcher"}, events.ThreadPeer{}, "found three papers")
		if err != nil {
			t.Errorf("build report: %v", err)
			return
		}
		if _, err := h.log.Append(context.Background(), h.sessionID, []events.NewEvent{received}); err != nil {
			t.Errorf("deliver report: %v", err)
		}
	}
	h.runOnce(t) // the grading turn, whose call fails

	if s := h.status(t); s != "running" {
		t.Errorf("session after the failed grade = %q, want chained on the report that landed mid-grade", s)
	}
	if n := h.liveWork(t); n != 1 {
		t.Errorf("live items = %d, want the chained turn", n)
	}
	// The chain is what the error event advertises, so the difference is
	// visible to a client as well as to the queue.
	if got := h.retryStatus(t); got != "retrying" {
		t.Errorf("retry_status = %q, want retrying — the turn is coming back", got)
	}
	if evals := h.outcomes(t); len(evals) != 1 || evals[0].Result != domain.OutcomeResultRunning {
		t.Errorf("outcomes = %+v, want the entry reverted to running", evals)
	}
}

// A wait parks nothing while a call this turn made is still outstanding: the
// turn suspends running like any tool turn and the driver's drain wakes it
// (decision 6). Parking would idle the thread with its call unanswered, and
// no drain, result trigger or message moves an idle thread.
func TestWaitSharingItsTurnWithAToolCallDoesNotPark(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		toolCall("t1", "bash", `{"command":"ls"}`),
		toolCall("t2", "wait_for_agents", `{}`),
		done("tool_use", 2),
	}}, nil)
	h.builtins(t)
	h.roster(t, "researcher")
	h.runningChild(t)
	h.wake(t, "coordinate")
	h.runOnce(t)

	if s := h.threadStatus(t, domain.PrimaryThreadID(h.sessionID)); s != "running" {
		t.Errorf("coordinator = %q, want running with its bash call outstanding", s)
	}
	if n := h.countType(t, "session.thread_status_idle"); n != 0 {
		t.Errorf("the coordinator parked while its own tool call was outstanding")
	}
	if n := h.liveOf(t, queue.ToolExec); n != 1 {
		t.Errorf("tool_exec items = %d, want the one the bash call needs", n)
	}
	if n := h.liveTurns(t, ""); n != 0 {
		t.Errorf("coordinator turns queued = %d, want none — the drain wakes it", n)
	}
	got := h.answers(t)
	if len(got) != 1 || got[0].isErr || !strings.Contains(got[0].text, `"timed_out":false`) {
		t.Fatalf("answers = %v, want the wait answered and the bash call left to its driver", got)
	}
}

// The same rule for the call a client answers: the thread stays running so
// the control plane's tool-result trigger — which fires only on a running
// thread — can resume it when the custom tool's result arrives.
func TestWaitSharingItsTurnWithAClientToolCallDoesNotPark(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		toolCall("t1", "lookup", `{"q":"papers"}`),
		toolCall("t2", "wait_for_agents", `{}`),
		done("tool_use", 2),
	}}, nil)
	h.roster(t, "researcher")
	h.runningChild(t)
	h.wake(t, "coordinate")
	h.runOnce(t)

	if s := h.threadStatus(t, domain.PrimaryThreadID(h.sessionID)); s != "running" {
		t.Errorf("coordinator = %q, want running with its custom call outstanding", s)
	}
	if n := h.liveWork(t); n != 0 {
		t.Errorf("live items = %d, want none — the client's result is what resumes this turn", n)
	}
	if n := h.countType(t, "session.thread_status_idle"); n != 0 {
		t.Errorf("the coordinator parked while a client call was outstanding")
	}
}

// A message delivered to a child that was running when it landed is read at
// that child's next settle, and its own submit_result is one: the report goes
// out, and the unread message chains the child's next turn instead of
// stranding on an idle thread nothing will wake.
func TestSubmitResultChainsOnAMessageDeliveredMidTurn(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		toolCall("t1", "submit_result", `{"result":"found three papers"}`),
		done("tool_use", 1),
	}}, nil)
	child := h.childTurn(t, "find the papers")
	h.provider.onGenerate = func(callIndex int) {
		if callIndex != 0 {
			return
		}
		_, received, err := events.ThreadMessage(h.sessionID, events.ThreadPeer{},
			events.ThreadPeer{ThreadID: child, AgentName: "worker"}, "also check the citations")
		if err != nil {
			t.Errorf("build message: %v", err)
			return
		}
		if _, err := h.log.Append(context.Background(), h.sessionID, []events.NewEvent{received}); err != nil {
			t.Errorf("deliver message: %v", err)
		}
	}
	h.runOnce(t)

	if got := h.receivedTexts(t, ""); len(got) != 1 || got[0] != "found three papers" {
		t.Errorf("coordinator received %v, want the report", got)
	}
	if s := h.threadStatus(t, child); s != "running" {
		t.Errorf("child = %q, want chained on the message it has not read", s)
	}
	if n := h.liveTurns(t, child); n != 1 {
		t.Errorf("child turns queued = %d, want the chained one", n)
	}
}

// What that chain can end in, and the reason the ending notice is worded
// about the turn rather than the thread: the chained turn answers the
// coordinator's message in prose, calling no submit_result, so the
// coordinator is told its child yielded without answering — the only thing
// that says so — one message after the report it just read. The wording is
// asserted because it is the whole point: "it never reported" would
// contradict the report above it on the same log.
func TestAReportedChildThatYieldsIsToldOnForTheTurn(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		{toolCall("t1", "submit_result", `{"result":"found three papers"}`), done("tool_use", 1)},
		agentReply("nothing to add"),
	}, nil)
	// The coordinator runs throughout, so neither the report nor the notice
	// wakes it and the only queued turn is the child's own chain — which is
	// what makes the second claim below deterministic.
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE session_threads SET status = 'running' WHERE id = $1`,
		domain.PrimaryThreadID(h.sessionID).String()); err != nil {
		t.Fatal(err)
	}
	child := h.childTurn(t, "find the papers")
	h.provider.onGenerate = func(callIndex int) {
		if callIndex != 0 {
			return
		}
		_, received, err := events.ThreadMessage(h.sessionID, events.ThreadPeer{},
			events.ThreadPeer{ThreadID: child, AgentName: "worker"}, "also check the citations")
		if err != nil {
			t.Errorf("build message: %v", err)
			return
		}
		if _, err := h.log.Append(context.Background(), h.sessionID, []events.NewEvent{received}); err != nil {
			t.Errorf("deliver message: %v", err)
		}
	}
	h.runOnce(t) // the report, chained on the message that landed mid-turn
	h.runOnce(t) // the chained turn, answered in prose

	got := h.receivedTexts(t, "")
	if len(got) != 2 || got[0] != "found three papers" {
		t.Fatalf("coordinator received %v, want the report then the notice", got)
	}
	if !strings.Contains(got[1], "without reporting") || !strings.Contains(got[1], "this turn") {
		t.Errorf("notice = %q, want the turn named rather than the thread's whole life", got[1])
	}
	if s := h.threadStatus(t, child); s != "idle" {
		t.Errorf("child = %q, want idle on its own end_turn", s)
	}
}

// A user.message that lands mid-turn enqueues nothing — the API's message arm
// needs an idle thread — so every settlement chains on it instead. A wait
// that parked on one would leave it unread with no trigger at all, so having
// something to read beats having something to wait for.
func TestWaitDoesNotParkOnAMessageDeliveredMidTurn(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		toolCall("t1", "wait_for_agents", `{}`),
		done("tool_use", 1),
	}}, nil)
	h.roster(t, "researcher")
	h.runningChild(t)
	h.wake(t, "coordinate")
	h.provider.onGenerate = func(callIndex int) {
		if callIndex != 0 {
			return
		}
		payload, _ := json.Marshal(map[string]any{
			"content": []map[string]string{{"type": "text", "text": "one more thing"}},
		})
		if _, err := h.log.Append(context.Background(), h.sessionID,
			[]events.NewEvent{{Type: domain.EventUserMessage, Payload: payload}}); err != nil {
			t.Errorf("deliver message: %v", err)
		}
	}
	h.runOnce(t)

	got := h.answers(t)
	if len(got) != 1 || got[0].isErr || got[0].text != waitStartedAnswer {
		t.Fatalf("answer = %+v, want exactly %s", got, waitStartedAnswer)
	}
	if s := h.threadStatus(t, domain.PrimaryThreadID(h.sessionID)); s != "running" {
		t.Errorf("coordinator = %q, want chained on the message it has not read", s)
	}
	if n := h.liveTurns(t, ""); n != 1 {
		t.Errorf("coordinator turns queued = %d, want the chained one", n)
	}
}

// Nothing running and a report already waiting is the one combination where
// the wait's own answer is the whole of the difference: it must not say that
// no reports are pending — the coordinator has one, and would be told to
// conclude on a report it has not read — and it must not park either, because
// the turn chains to read it. Neither of the two tests above can catch this:
// with a child running the answer is the same whatever the probe decides.
func TestWaitWithNothingRunningStartsAWaitWhenAReportIsUnread(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		toolCall("t1", "wait_for_agents", `{}`),
		done("tool_use", 1),
	}}, nil)
	h.roster(t, "researcher")
	// Idle on no reason at all: nothing to wait for — the child has finished.
	child := pgtest.NewChildThread(t, h.pool, h.sessionID)
	h.wake(t, "coordinate")
	h.provider.onGenerate = func(callIndex int) {
		if callIndex != 0 {
			return
		}
		_, received, err := events.ThreadMessage(h.sessionID,
			events.ThreadPeer{ThreadID: child, AgentName: "worker"}, events.ThreadPeer{}, "found three papers")
		if err != nil {
			t.Errorf("build report: %v", err)
			return
		}
		if _, err := h.log.Append(context.Background(), h.sessionID, []events.NewEvent{received}); err != nil {
			t.Errorf("deliver report: %v", err)
		}
	}
	h.runOnce(t)

	got := h.answers(t)
	if len(got) != 1 || got[0].isErr || got[0].text != waitStartedAnswer {
		t.Fatalf("answer = %+v, want exactly %s", got, waitStartedAnswer)
	}
	if s := h.threadStatus(t, domain.PrimaryThreadID(h.sessionID)); s != "running" {
		t.Errorf("coordinator = %q, want chained on the report it already has", s)
	}
	if n := h.liveTurns(t, ""); n != 1 {
		t.Errorf("coordinator turns queued = %d, want the chained one", n)
	}
	if n := h.countType(t, "session.thread_status_idle"); n != 0 {
		t.Errorf("the coordinator idled with a report unread")
	}
}

// A child whose turn simply ends — no submit_result, nothing to report — is
// the ending nothing else announces, and a coordinator parked on a wait has
// no other trigger. So it is delivered like every other ending, and woken
// like the one out of retries: both are endings with no client behind them.
func TestChildEndingWithoutReportingTellsTheCoordinator(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{agentReply("I looked, and found nothing")}, nil)
	child := h.childTurn(t, "find the papers")
	h.runOnce(t)

	if s := h.threadStatus(t, child); s != "idle" {
		t.Errorf("child = %q, want idle on its own end_turn", s)
	}
	got := h.receivedTexts(t, "")
	if len(got) != 1 || !strings.Contains(got[0], "worker") || !strings.Contains(got[0], "without reporting") {
		t.Fatalf("coordinator was told %v, want the child's ending named", got)
	}
	if s := h.threadStatus(t, domain.PrimaryThreadID(h.sessionID)); s != "running" {
		t.Errorf("coordinator = %q, want woken to read the notice", s)
	}
	if n := h.liveTurns(t, ""); n != 1 {
		t.Errorf("coordinator turns queued = %d, want 1", n)
	}
	if s := h.status(t); s != "running" {
		t.Errorf("session = %q, want running with its woken coordinator", s)
	}
}

// The request prefix is what a coordinator pays for on every turn: each turn
// must assemble the same tools and the same leading messages as the turn before
// it, or the cached prefix is lost each time. Three turns, because the first
// replays only its seeding message — it is the delegation tool_use and the
// tool_result answering it, event-id-keyed and composed inside the settlement,
// that a change would move.
func TestTwoCoordinatorTurnsShareARequestPrefix(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		{toolCall("t1", "create_agent", `{"agent_name":"researcher","message":"find the papers"}`), done("tool_use", 1)},
		{toolCall("t2", "list_agents", `{}`), done("tool_use", 1)},
		agentReply("delegated"),
	}, nil)
	h.roster(t, "researcher")
	h.wake(t, "coordinate")
	h.runOnce(t) // the spawn chains the coordinator's next turn
	h.runOnce(t) // so does the list — the child's own turn is younger work
	h.runOnce(t)

	calls := h.provider.calls
	if len(calls) != 3 {
		t.Fatalf("%d model calls, want three coordinator turns", len(calls))
	}
	// The compared prefix must hold what the settlement wrote, or this test
	// passes over a seeding message and proves nothing.
	second := requestJSON(t, calls[1].Messages)
	for _, want := range []string{`"type":"tool_use"`, `"type":"tool_result"`} {
		if !strings.Contains(strings.Join(second, "\n"), want) {
			t.Fatalf("the replayed prefix carries no %s block:\n%s", want, strings.Join(second, "\n"))
		}
	}
	for turn := 1; turn < len(calls); turn++ {
		prev, cur := calls[turn-1], calls[turn]
		if len(prev.Tools) != len(cur.Tools) {
			t.Fatalf("turn %d: tools = %d, want the %d of the turn before", turn, len(cur.Tools), len(prev.Tools))
		}
		for i := range prev.Tools {
			if string(prev.Tools[i]) != string(cur.Tools[i]) {
				t.Errorf("turn %d: tool %d moved:\n%s\n%s", turn, i, prev.Tools[i], cur.Tools[i])
			}
		}
		before, after := requestJSON(t, prev.Messages), requestJSON(t, cur.Messages)
		if len(after) <= len(before) {
			t.Fatalf("turn %d replayed %d messages, want more than the %d before it", turn, len(after), len(before))
		}
		for i := range before {
			if before[i] != after[i] {
				t.Errorf("turn %d: message %d moved:\n%s\n%s", turn, i, before[i], after[i])
			}
		}
	}
}

// requestJSON renders one request's replayed messages for comparison. A
// discarded marshal error would compare "" to "" and pass, which is the one
// way this comparison can lie.
func requestJSON(t *testing.T, msgs []provider.Message) []string {
	t.Helper()
	out := make([]string, len(msgs))
	for i, m := range msgs {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		out[i] = string(raw)
	}
	return out
}

// slicesEq compares two string slices.
func slicesEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Bounding the settlement chain (#442). A turn whose calls were all
// settlement-executed hands its own model_turn straight back, so nothing
// outside the model's own choices ends the run — and every shape that reaches
// it is one a model repeats. The count rides on the item; maxSettlementChain
// is what reads it.

// chainCount reads the consecutive-settlement count off the primary's live
// model_turn item — queue.RequeueSettlement's own record of how long the
// current chain is.
func (h *harness) chainCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT COALESCE((metadata->>'settlement_chain')::int, 0) FROM work_items
		  WHERE session_id = $1 AND kind = 'model_turn' AND thread_id IS NULL
		    AND state <> 'stopped'`,
		h.sessionID.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// setChainCount puts the primary's live turn partway into a chain, so a test
// can reach the cap without paying for the turns that would get there.
func (h *harness) setChainCount(t *testing.T, n int) {
	t.Helper()
	tag, err := h.pool.Exec(context.Background(),
		`UPDATE work_items SET metadata = jsonb_set(metadata, '{settlement_chain}', to_jsonb($2::int), true)
		  WHERE session_id = $1 AND kind = 'model_turn' AND thread_id IS NULL AND state <> 'stopped'`,
		h.sessionID.String(), n)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("seed chain count: %d rows, want 1", tag.RowsAffected())
	}
}

// threadStop reads a thread's stored stop_reason. The session's own fold can
// differ — a coordinator idling under a running child leaves the session
// running — so a thread-level settlement is asserted here, not on the session.
func (h *harness) threadStop(t *testing.T, tid domain.ID) domain.StopReason {
	t.Helper()
	var raw []byte
	if err := h.pool.QueryRow(context.Background(),
		`SELECT stop_reason FROM session_threads WHERE id = $1`, tid.String()).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var sr domain.StopReason
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &sr); err != nil {
			t.Fatal(err)
		}
	}
	return sr
}

// Each chained turn adds one to the count, and the item carries it across
// turns — it has to, because the next turn of a chain may be claimed by a
// different brain.
func TestSettlementChainCountsItsConsecutiveTurns(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		{toolCall("t1", "list_agents", `{}`), done("tool_use", 1)},
		{toolCall("t2", "list_agents", `{}`), done("tool_use", 1)},
	}, nil)
	h.roster(t, "researcher")
	h.wake(t, "delegate")

	h.runOnce(t)
	if n := h.chainCount(t); n != 1 {
		t.Errorf("count after one chained turn = %d, want 1", n)
	}
	h.runOnce(t)
	if n := h.chainCount(t); n != 2 {
		t.Errorf("count after two chained turns = %d, want 2", n)
	}
	if s := h.threadStatus(t, domain.PrimaryThreadID(h.sessionID)); s != "running" {
		t.Errorf("coordinator = %q, want running with its next turn queued", s)
	}
}

// One turn below the cap still chains: the boundary is the cap itself, not
// its neighbourhood.
func TestSettlementChainBelowTheCapStillChains(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		{toolCall("t1", "list_agents", `{}`), done("tool_use", 1)},
	}, nil)
	h.roster(t, "researcher")
	h.wake(t, "delegate")
	h.setChainCount(t, 23) // this turn is the 24th; the 25th is the cut

	h.runOnce(t)
	if s := h.threadStatus(t, domain.PrimaryThreadID(h.sessionID)); s != "running" {
		t.Errorf("coordinator = %q, want running one turn below the cap", s)
	}
	if n := h.chainCount(t); n != 24 {
		t.Errorf("count = %d, want 24", n)
	}
	if n := h.countType(t, "session.error"); n != 0 {
		t.Errorf("session.error = %d, want none below the cap", n)
	}
}

// At the cap the chain is cut. The thread idles on end_turn — so the fold
// lets the session leave running, and archive and delete stop refusing — and
// a session.error says why, which an end_turn alone could not: it is
// indistinguishable from a coordinator that finished. The call is still
// answered: the turn is settled, not failed.
func TestSettlementChainIsCutAtTheCap(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		{toolCall("t1", "list_agents", `{}`), done("tool_use", 1)},
	}, nil)
	h.roster(t, "researcher")
	h.wake(t, "delegate")
	h.setChainCount(t, 24) // this turn is the 25th — maxSettlementChain

	h.runOnce(t)
	primary := domain.PrimaryThreadID(h.sessionID)
	if s := h.threadStatus(t, primary); s != "idle" {
		t.Errorf("coordinator = %q, want idle once the chain is cut", s)
	}
	if got := h.threadStop(t, primary).Type; got != domain.StopEndTurn {
		t.Errorf("stop_reason = %q, want end_turn", got)
	}
	if n := h.liveTurns(t, ""); n != 0 {
		t.Errorf("coordinator turns queued = %d, want none", n)
	}
	errs := h.eventsOfType(t, domain.EventSessionError)
	if len(errs) != 1 {
		t.Fatalf("session.error = %d, want the one naming the cut chain", len(errs))
	}
	var payload struct {
		Error struct {
			Type        string `json:"type"`
			Message     string `json:"message"`
			RetryStatus struct {
				Type string `json:"type"`
			} `json:"retry_status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(errs[0].Body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error.Type != "delegation_chain_exhausted_error" {
		t.Errorf("error type = %q, want delegation_chain_exhausted_error", payload.Error.Type)
	}
	if !strings.Contains(payload.Error.Message, "25") {
		t.Errorf("message = %q, want the count it actually ran", payload.Error.Message)
	}
	// Required on every variant of the reference's error union, and carried
	// by every other session.error this platform writes.
	if payload.Error.RetryStatus.Type != "exhausted" {
		t.Errorf("retry_status = %q, want exhausted — nothing will retry on its own",
			payload.Error.RetryStatus.Type)
	}
	if got := h.answers(t); len(got) != 1 || got[0].isErr {
		t.Errorf("answers = %v, want list_agents answered as usual", got)
	}
}

// Input is progress, so it chains past the cap and resets the count: the
// bound exists for a thread nobody is feeding, not for a busy coordinator
// that has been working for a while.
func TestInputAtTheCapChainsAnywayAndResetsTheCount(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		{toolCall("t1", "list_agents", `{}`), done("tool_use", 1)},
	}, nil)
	h.roster(t, "researcher")
	h.provider.onGenerate = func(call int) {
		if call != 0 {
			return
		}
		payload, _ := json.Marshal(map[string]any{"content": "keep going"})
		if _, err := h.log.Append(context.Background(), h.sessionID, []events.NewEvent{
			{Type: domain.EventUserMessage, Payload: payload},
		}); err != nil {
			t.Errorf("mid-turn append: %v", err)
		}
	}
	h.wake(t, "delegate")
	h.setChainCount(t, 24) // this turn would otherwise be the cut

	h.runOnce(t)
	if s := h.threadStatus(t, domain.PrimaryThreadID(h.sessionID)); s != "running" {
		t.Errorf("coordinator = %q, want running: input chains past the cap", s)
	}
	if n := h.chainCount(t); n != 0 {
		t.Errorf("count = %d, want 0 — the chain the input took is not the one being bounded", n)
	}
	if n := h.countType(t, "session.error"); n != 0 {
		t.Errorf("session.error = %d, want none: nothing was cut", n)
	}
}

// The ask-gated settlement branch (#442 item 3). An ask gate wins for the
// turn's exec calls, but the delegation calls execute anyway — they are the
// platform's own and no confirmation exists for them — and the child a gated
// turn spawned still gets its turn. That last one is the difference between a
// coordinator that pauses for a human and one that pauses forever.
func TestAskGatedDelegatedTurnGatesExecButStillRunsItsChild(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		toolCall("t1", "create_agent", `{"agent_name":"researcher","message":"find the papers"}`),
		toolCall("t2", "bash", `{"command":"ls"}`),
		done("tool_use", 2),
	}}, nil)
	// Default always_allow, bash overridden to always_ask; roster added after,
	// since h.roster patches multiagent onto whatever spec is stored.
	agentJSON := `{"type":"agent","id":"agent_x","version":1,"name":"coordinator",
		"model":{"id":"fixture-model"},"system":"s","description":"",
		"tools":[{"type":"agent_toolset_20260401","configs":[{"name":"bash","permission_policy":{"type":"always_ask"}}]}],
		"mcp_servers":[],"skills":[],"multiagent":null}`
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET resolved_agent = $2 WHERE id = $1`, h.sessionID.String(), agentJSON); err != nil {
		t.Fatal(err)
	}
	h.roster(t, "researcher")
	h.wake(t, "delegate and look around")
	h.runOnce(t)

	// The delegation call ran in the settlement; the gated bash did not, and
	// nothing was enqueued for it.
	if got := h.answers(t); len(got) != 1 || got[0].isErr {
		t.Fatalf("answers = %v, want create_agent's alone", got)
	}
	if n := h.liveOf(t, queue.ToolExec); n != 0 {
		t.Errorf("tool_exec = %d, want none: a gated turn enqueues nothing", n)
	}

	// The coordinator suspends requires_action naming the bash call alone.
	primary := domain.PrimaryThreadID(h.sessionID)
	if s := h.threadStatus(t, primary); s != "idle" {
		t.Errorf("coordinator = %q, want idle awaiting the human", s)
	}
	stop := h.threadStop(t, primary)
	if stop.Type != domain.StopRequiresAction || len(stop.EventIDs) != 1 {
		t.Fatalf("stop_reason = %+v, want requires_action naming one event", stop)
	}
	uses := h.eventsOfType(t, domain.EventAgentToolUse)
	askID := domain.ID("")
	for _, u := range uses {
		var b struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(u.Body, &b); err != nil {
			t.Fatal(err)
		}
		if b.Name == "bash" {
			askID = u.ID
		}
	}
	if askID == "" || stop.EventIDs[0] != askID {
		t.Errorf("stop_reason names %v, want the bash use %s", stop.EventIDs, askID)
	}

	// And the child it spawned actually runs.
	kids := h.children(t)
	if len(kids) != 1 {
		t.Fatalf("children = %v, want the spawned researcher", kids)
	}
	if n := h.liveTurns(t, kids[0].id); n != 1 {
		t.Errorf("child turns queued = %d, want 1 — a gated coordinator must not strand its child", n)
	}
	if n := h.liveTurns(t, ""); n != 0 {
		t.Errorf("coordinator turns queued = %d, want none while it waits", n)
	}
}
