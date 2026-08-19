package executor

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/mcp/mcptest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/jackc/pgx/v5"
)

// The drivers over sibling threads (plan 35 slice 3, decisions 5 and 9): one
// session-keyed exec item, the runnable set, the per-thread wake, and the
// answered-means-cancelled check. Child rows come from the slice-3 test seam.

// childThread plants a running child under the harness's session.
func (h *harness) childThread(t *testing.T, status string) domain.ID {
	t.Helper()
	tid := pgtest.NewChildThread(t, h.pool, h.sid)
	h.setThread(t, tid, status, "")
	return tid
}

func (h *harness) setThread(t *testing.T, tid domain.ID, status, stopJSON string) {
	t.Helper()
	var stop *string
	if stopJSON != "" {
		stop = &stopJSON
	}
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE session_threads SET status = $2, stop_reason = $3 WHERE id = $1`, tid.String(), status, stop); err != nil {
		t.Fatal(err)
	}
}

// appendOn plants one event on a thread's log, cross-posted as a child's
// platform call would be.
func (h *harness) appendOn(t *testing.T, tid domain.ID, typ domain.EventType, payload string) domain.ID {
	t.Helper()
	evs, err := h.log.Append(context.Background(), h.sid, []events.NewEvent{
		{Type: typ, ThreadID: tid, CrossPosted: tid != "", Payload: json.RawMessage(payload)}})
	if err != nil {
		t.Fatal(err)
	}
	return evs[0].ID
}

func (h *harness) enqueueToolExec(t *testing.T) {
	t.Helper()
	if _, err := h.queue.Enqueue(context.Background(), h.pool, h.envID, h.sid, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
}

// liveTurnThreads lists the thread ids of the session's live model_turn items,
// sorted ("" — the primary — first).
func (h *harness) liveTurnThreads(t *testing.T) []string {
	t.Helper()
	rows, err := h.pool.Query(context.Background(),
		`SELECT COALESCE(thread_id, '') FROM work_items
		 WHERE session_id = $1 AND kind = 'model_turn' AND state IN ('queued','starting','active')
		 ORDER BY COALESCE(thread_id, '')`,
		h.sid.String())
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

// resultFor returns the tool_result answering the use, with the thread column
// and cross-post flag it was stored under; ok false when none.
func (h *harness) resultFor(t *testing.T, useID domain.ID) (thread string, crossPosted, ok bool) {
	t.Helper()
	err := h.pool.QueryRow(context.Background(),
		`SELECT COALESCE(thread_id, ''), cross_posted FROM events
		 WHERE session_id = $1 AND type = 'agent.tool_result' AND payload->>'tool_use_id' = $2`,
		h.sid.String(), useID.String()).Scan(&thread, &crossPosted)
	if err == pgx.ErrNoRows {
		return "", false, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return thread, crossPosted, true
}

func withPerm(use, perm string) string {
	var m map[string]any
	_ = json.Unmarshal([]byte(use), &m)
	m["evaluated_permission"] = perm
	b, _ := json.Marshal(m)
	return string(b)
}

// The HITL non-bypass: thread A holds an ask, thread B's allow-policy call
// rides the session's tool_exec, and the executor runs B's call and not A's
// until A's allow lands — then A's, on A's thread, waking A alone.
func TestExecutorRunsOnlyTheRunnableSetAcrossThreads(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	a := h.childThread(t, "idle")
	b := h.childThread(t, "running")
	askA := h.appendOn(t, a, domain.EventAgentToolUse, withPerm(writeUse("a.txt", "from A"), "ask"))
	h.setThread(t, a, "idle", `{"type":"requires_action","event_ids":["`+askA.String()+`"]}`)
	useB := h.appendOn(t, b, domain.EventAgentToolUse, withPerm(writeUse("b.txt", "from B"), "allow"))
	h.enqueueToolExec(t)

	if worked, err := h.exec.step(context.Background()); err != nil || !worked {
		t.Fatalf("step: worked=%v err=%v", worked, err)
	}
	if _, _, ok := h.resultFor(t, askA); ok {
		t.Error("A's ask-gated call was run before its confirmation")
	}
	if thread, cross, ok := h.resultFor(t, useB); !ok || thread != b.String() || !cross {
		t.Errorf("B's result = (thread %q, cross_posted %v, ok %v), want on B's thread, cross-posted", thread, cross, ok)
	}
	if sb.files["/workspace/a.txt"] != "" || sb.files["/workspace/b.txt"] != "from B" {
		t.Errorf("sandbox files = %v, want B's write alone", sb.files)
	}
	if n := h.liveOf(t, queue.ToolExec); n != 0 {
		t.Errorf("live tool_exec = %d, want the item completed — nothing runnable is left", n)
	}
	// The wake is per thread: B (running, answered) and the primary (running,
	// nothing outstanding) get their turns; A, parked idle, does not.
	if turns := h.liveTurnThreads(t); len(turns) != 2 || turns[0] != "" || turns[1] != b.String() {
		t.Errorf("live turns = %v, want the primary's and B's", turns)
	}

	// A's allow lands (the API's confirmation arm: the confirmation on A's
	// thread, A running, the tool_exec enqueued). Now A's call runs.
	h.appendOn(t, a, domain.EventUserToolConfirm,
		`{"tool_use_id":"`+askA.String()+`","result":"allow","session_thread_id":null}`)
	h.setThread(t, a, "running", "")
	h.enqueueToolExec(t)
	if worked, err := h.exec.step(context.Background()); err != nil || !worked {
		t.Fatalf("second step: worked=%v err=%v", worked, err)
	}
	if thread, cross, ok := h.resultFor(t, askA); !ok || thread != a.String() || !cross {
		t.Errorf("A's result = (thread %q, cross_posted %v, ok %v), want on A's thread, cross-posted", thread, cross, ok)
	}
	if sb.files["/workspace/a.txt"] != "from A" {
		t.Errorf("A's write = %q", sb.files["/workspace/a.txt"])
	}
	turns := h.liveTurnThreads(t)
	var aWoken bool
	for _, tid := range turns {
		aWoken = aWoken || tid == a.String()
	}
	if !aWoken || len(turns) != 3 {
		t.Errorf("live turns after A's call = %v, want A's added to the two", turns)
	}
}

// A thread-scoped interrupt never stops the shared item (decision 9): the
// driver drops the late result of a call answered under it and cancels the
// in-flight call on the keeper's beat, then goes on with the sibling's calls.
func TestAnAnsweredCallIsCancelledOnTheKeeperBeat(t *testing.T) {
	sb := &fakeSandbox{entered: make(chan struct{}, 1), gate: make(chan struct{}), gateFrom: 2}
	prov := &fakeProvider{sb: sb}
	h := newHarnessWith(t, prov, Config{LeaseTTL: 300 * time.Millisecond})
	h.prov = prov
	a := h.childThread(t, "running")
	// The primary's call first (it passes the gate), A's second (it wedges
	// until cancelled), then a second primary call queued behind A's.
	useP := h.appendOn(t, "", domain.EventAgentToolUse, writeUse("p.txt", "primary"))
	useA := h.appendOn(t, a, domain.EventAgentToolUse, writeUse("a.txt", "from A"))
	useQ := h.appendOn(t, "", domain.EventAgentToolUse, writeUse("q.txt", "after"))
	h.enqueueToolExec(t)

	done := make(chan error, 1)
	go func() {
		_, err := h.exec.step(context.Background())
		done <- err
	}()
	<-sb.entered
	// A's interrupt answers A's call on A's log — what the API's thread-scoped
	// interrupt writes — and never touches the item.
	h.appendOn(t, a, domain.EventAgentToolResult,
		`{"tool_use_id":"`+useA.String()+`","content":[{"type":"text","text":"`+events.InterruptResultText+`"}],"is_error":true}`)
	h.setThread(t, a, "idle", `{"type":"end_turn"}`)
	// The gate stays shut for A's write: only the watch's cancel lets the
	// pass move on — to the primary's second call, which enters the gate
	// next and is let through.
	select {
	case <-sb.entered:
		close(sb.gate)
	case <-time.After(5 * time.Second):
		t.Fatal("the pass did not move past the answered call on the keeper beat")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("step: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the pass did not move past the answered call on the keeper beat")
	}
	if n, _ := h.pool.Exec(context.Background(),
		`SELECT 1 FROM events WHERE session_id = $1 AND type = 'agent.tool_result' AND payload->>'tool_use_id' = $2`,
		h.sid.String(), useA.String()); n.RowsAffected() != 1 {
		t.Errorf("results for A's call = %d, want the interrupt's alone (the late one dropped)", n.RowsAffected())
	}
	for _, use := range []domain.ID{useP, useQ} {
		if _, _, ok := h.resultFor(t, use); !ok {
			t.Errorf("the primary's call %s was not answered", use)
		}
	}
	if sb.files["/workspace/a.txt"] != "" || sb.files["/workspace/p.txt"] != "primary" || sb.files["/workspace/q.txt"] != "after" {
		t.Errorf("sandbox files = %v", sb.files)
	}
	if n := h.liveOf(t, queue.ToolExec); n != 0 {
		t.Errorf("live tool_exec = %d, want completed", n)
	}
	if turns := h.liveTurnThreads(t); len(turns) != 1 || turns[0] != "" {
		t.Errorf("live turns = %v, want the primary's alone (A idled on its interrupt)", turns)
	}
}

// A child's own MCP server is discovered from the child's list, into the
// child's catalog rows, and the wake that follows is the child's turn.
func TestAChildsMCPServerIsDiscoveredFromItsOwnList(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "lookup", Description: "looks up"})
	h := mcpHarness(t)
	agent := `{"type":"agent","id":"agent_worker","version":1,"name":"worker","model":{"id":"fixture-model"},` +
		`"system":"","description":"","tools":[],"skills":[],` +
		`"mcp_servers":[{"type":"url","name":"childsrv","url":"` + url + `"}]}`
	child := pgtest.NewChildThreadWithAgent(t, h.pool, h.sid, agent)
	h.setThread(t, child, "running", "")
	h.enqueueMCP(t)

	h.stepOnce(t)

	var thread, status string
	err := h.pool.QueryRow(context.Background(),
		`SELECT COALESCE(thread_id, ''), status FROM mcp_catalogs WHERE session_id = $1 AND server_name = 'childsrv'`,
		h.sid.String()).Scan(&thread, &status)
	if err != nil {
		t.Fatalf("child's catalog row: %v", err)
	}
	if thread != child.String() || status != "ready" {
		t.Errorf("row = (thread %q, status %q), want the child's ready row", thread, status)
	}
	var primaryRows int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM mcp_catalogs WHERE session_id = $1 AND thread_id IS NULL`, h.sid.String()).Scan(&primaryRows); err != nil {
		t.Fatal(err)
	}
	if primaryRows != 0 {
		t.Errorf("primary catalog rows = %d, want none — the primary declares no server", primaryRows)
	}
	turns := h.liveTurnThreads(t)
	if len(turns) != 2 || turns[0] != "" || turns[1] != child.String() {
		t.Errorf("live turns = %v, want the primary's and the child's (both running, nothing outstanding)", turns)
	}
}
