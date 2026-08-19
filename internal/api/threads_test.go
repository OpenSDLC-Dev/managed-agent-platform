package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
)

// The session-thread resource (plan 35 slice 2, decisions 1, 2, 12): every
// session has a primary thread, derived id and read-through agent; its events
// are the session's; every session.status_* comes with the primary thread's
// own status event in front of it; child threads (rows this slice only reads)
// have their own view and cross-post into the session's.

// threadRows counts a session's session_threads rows.
func threadRows(t *testing.T, s *tserver, sid string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM session_threads WHERE session_id = $1`, sid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func listThreads(t *testing.T, s *tserver, sid string) []map[string]any {
	t.Helper()
	status, res := s.do(http.MethodGet, "/v1/sessions/"+sid+"/threads", nil)
	if status != http.StatusOK {
		t.Fatalf("list threads: %d %v", status, res)
	}
	return listData(t, res)
}

// insertChild writes a child thread row directly — slice 3 spawns them; this
// slice reads, archives and filters by them.
func insertChild(t *testing.T, s *tserver, sid, status string) string {
	t.Helper()
	id := domain.NewID(domain.PrefixSessionThread).String()
	primary := domain.PrimaryThreadID(domain.ID(sid)).String()
	if _, err := s.pool.Exec(context.Background(),
		`INSERT INTO session_threads (id, session_id, parent_thread_id, agent, agent_name, status)
		 VALUES ($1, $2, $3, $4, 'worker', $5)`,
		id, sid, primary, `{"id":"agent_x","type":"agent","version":1,"name":"worker","description":"",`+
			`"model":{"id":"claude-opus-4-8"},"system":"","tools":[],"mcp_servers":[],"skills":[]}`, status); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestThreadsPrimaryOnEverySession(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	primary := domain.PrimaryThreadID(domain.ID(sid)).String()

	threads := listThreads(t, s, sid)
	if len(threads) != 1 {
		t.Fatalf("threads = %v, want the one primary", threads)
	}
	th := threads[0]
	wantFields(t, th, "id", "type", "session_id", "parent_thread_id", "agent", "status", "usage", "stats",
		"created_at", "updated_at", "archived_at")
	if len(th) != 11 {
		t.Errorf("thread carries %d keys, want the SDK's 11: %v", len(th), keysOf(th))
	}
	if th["id"] != primary || th["type"] != "session_thread" || th["session_id"] != sid ||
		th["parent_thread_id"] != nil || th["status"] != "idle" || th["archived_at"] != nil {
		t.Errorf("primary thread = %v", th)
	}
	if st, _ := th["stats"].(map[string]any); st["active_seconds"] != float64(0) || st["startup_seconds"] != float64(0) {
		t.Errorf("stats = %v, want the empty shape", th["stats"])
	}
	// The agent is the session's resolved agent minus the roster — the
	// SessionThreadAgent shape, tools materialized.
	agent, _ := th["agent"].(map[string]any)
	fields := []string{"id", "type", "version", "name", "description", "model", "system", "tools", "mcp_servers", "skills"}
	wantFields(t, agent, fields...)
	if len(agent) != len(fields) || agent["type"] != "agent" || agent["name"] != "task-agent" || agent["system"] != "base system" {
		t.Errorf("primary agent = %v, want the session's agent as a SessionThreadAgent", agent)
	}
	// GET one; the session_ spelling derives the same id; unknown → 404.
	status, one := s.do(http.MethodGet, "/v1/sessions/"+sid+"/threads/"+primary, nil)
	if status != http.StatusOK || one["id"] != primary {
		t.Errorf("get thread: %d %v", status, one)
	}
	alt := "session_" + strings.TrimPrefix(sid, "sesn_")
	if status, one := s.do(http.MethodGet, "/v1/sessions/"+alt+"/threads/"+primary, nil); status != http.StatusOK || one["id"] != primary {
		t.Errorf("get thread via session_ spelling: %d %v", status, one)
	}
	status, body := s.do(http.MethodGet, "/v1/sessions/"+sid+"/threads/sthr_0000000000000000000000000", nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
	status, body = s.do(http.MethodGet, "/v1/sessions/"+sid+"/threads/bogus", nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
	// Another session's primary is not this session's — and the schema
	// refuses a child hung off another session's thread.
	other := eventsFixture(t, s)
	status, body = s.do(http.MethodGet, "/v1/sessions/"+other+"/threads/"+primary, nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
	if _, err := s.pool.Exec(context.Background(),
		`INSERT INTO session_threads (id, session_id, parent_thread_id, agent, agent_name, status)
		 VALUES ($1, $2, $3, '{"type":"agent"}', 'x', 'idle')`,
		domain.NewID(domain.PrefixSessionThread).String(), other, primary); err == nil {
		t.Error("a child in one session accepted a parent from another")
	}
	// List validation: the documented default, 1000, is also our cap; page is forward-only.
	status, body = s.do(http.MethodGet, "/v1/sessions/"+sid+"/threads?limit=1001", nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	status, body = s.do(http.MethodGet, "/v1/sessions/sesn_0000000000000000000000000/threads", nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")

	// A coordinator session's primary agent carries no roster either.
	a := createAgent(t, s, map[string]any{"name": "worker", "model": "claude-opus-4-8"})["id"].(string)
	coord := createAgent(t, s, map[string]any{"name": "coordinator", "model": "claude-opus-4-8",
		"multiagent": map[string]any{"type": "coordinator", "agents": []any{a}}})["id"].(string)
	envID := createEnvironment(t, s, map[string]any{"name": "env"})["id"].(string)
	csid := createSession(t, s, map[string]any{"agent": coord, "environment_id": envID})["id"].(string)
	cagent, _ := listThreads(t, s, csid)[0]["agent"].(map[string]any)
	if _, ok := cagent["multiagent"]; ok || cagent["name"] != "coordinator" {
		t.Errorf("coordinator primary agent = %v, want no multiagent key", cagent)
	}
}

// Every session.status_* is preceded, in the same batch, by the primary
// thread's session.thread_status_* naming the thread and the agent; the
// thread row's status follows the session's.
func TestStatusEventsComeInPrimaryThreadPairs(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	primary := domain.PrimaryThreadID(domain.ID(sid)).String()
	sendEvents(t, s, sid, userMessage("go"))
	if th := listThreads(t, s, sid)[0]; th["status"] != "running" {
		t.Errorf("thread status after wake = %v, want running", th["status"])
	}
	sendEvents(t, s, sid, map[string]any{"type": "user.interrupt"})
	if th := listThreads(t, s, sid)[0]; th["status"] != "idle" {
		t.Errorf("thread status after interrupt = %v, want idle", th["status"])
	}

	_, res := s.do(http.MethodGet, "/v1/sessions/"+sid+"/events", nil)
	evs := listData(t, res)
	pairs := 0
	for i, ev := range evs {
		typ := ev["type"].(string)
		if !strings.HasPrefix(typ, "session.status_") {
			continue
		}
		pairs++
		if i == 0 {
			t.Fatalf("%s at the head of the log, no thread event before it", typ)
		}
		prev := evs[i-1]
		if prev["type"] != "session.thread_status_"+strings.TrimPrefix(typ, "session.status_") {
			t.Errorf("%s preceded by %v, want the primary thread's event", typ, prev["type"])
			continue
		}
		if prev["session_thread_id"] != primary || prev["agent_name"] != "task-agent" {
			t.Errorf("thread event = %v, want session_thread_id %s and agent_name task-agent", prev, primary)
		}
		if typ == "session.status_idle" {
			ps, _ := prev["stop_reason"].(map[string]any)
			ss, _ := ev["stop_reason"].(map[string]any)
			if ps["type"] != ss["type"] || ps["type"] != "end_turn" {
				t.Errorf("stop reasons: thread %v, session %v, want end_turn on both", prev["stop_reason"], ev["stop_reason"])
			}
		}
	}
	if pairs != 2 {
		t.Errorf("saw %d session status events, want running + idle", pairs)
	}
}

// The primary thread's events are the session's: the same rows, the same
// ids, on the list and on the stream; the thread list carries no filters.
func TestPrimaryThreadEventsAreTheSessionView(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	primary := domain.PrimaryThreadID(domain.ID(sid)).String()
	tpath := "/v1/sessions/" + sid + "/threads/" + primary
	sendEvents(t, s, sid, userMessage("m0"))
	sendEvents(t, s, sid, map[string]any{"type": "user.interrupt"})

	_, sess := s.do(http.MethodGet, "/v1/sessions/"+sid+"/events", nil)
	status, thr := s.do(http.MethodGet, tpath+"/events", nil)
	if status != http.StatusOK {
		t.Fatalf("thread events: %d %v", status, thr)
	}
	sd, td := listData(t, sess), listData(t, thr)
	if len(sd) != len(td) || len(sd) != 6 {
		t.Fatalf("session view %d events, thread view %d, want 6 each", len(sd), len(td))
	}
	for i := range sd {
		if sd[i]["id"] != td[i]["id"] {
			t.Errorf("event %d: session %v, thread %v", i, sd[i]["id"], td[i]["id"])
		}
	}
	// Paging with the seq cursor, ascending only.
	_, p1 := s.do(http.MethodGet, tpath+"/events?limit=4", nil)
	if np := nextPage(t, p1); np == "" {
		t.Fatal("no next_page at limit=4 over 6 events")
	} else if _, p2 := s.do(http.MethodGet, tpath+"/events?limit=4&page="+np, nil); len(listData(t, p2)) != 2 {
		t.Errorf("second page = %v, want the last 2", p2)
	}
	for _, qs := range []string{"?order=desc", "?types[]=user.message", "?created_at[gt]=2020-01-01T00:00:00Z"} {
		status, body := s.do(http.MethodGet, tpath+"/events"+qs, nil)
		wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
		if msg := errMessage(body); !strings.Contains(msg, "not supported on a thread's events") {
			t.Errorf("%s: message = %q", qs, msg)
		}
	}
	// A descending cursor minted by the session list is foreign here: it
	// would walk backwards past the order refusal.
	_, desc := s.do(http.MethodGet, "/v1/sessions/"+sid+"/events?order=desc&limit=2", nil)
	status, body := s.do(http.MethodGet, tpath+"/events?page="+nextPage(t, desc), nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	status, body = s.do(http.MethodGet, "/v1/sessions/"+sid+"/threads/sthr_0000000000000000000000000/events", nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
	// A missing session is named as such on every thread route, and a bad
	// parameter on it is still the 400 every list answers first.
	gone := "/v1/sessions/sesn_0000000000000000000000000/threads/" + primary
	for _, p := range []string{"", "/events", "/stream"} {
		status, body = s.do(http.MethodGet, gone+p, nil)
		wantErr(t, status, body, http.StatusNotFound, "not_found_error")
		if msg := errMessage(body); !strings.Contains(msg, "session sesn_0000000000000000000000000 not found") {
			t.Errorf("%s on a missing session: message = %q", p, msg)
		}
	}
	status, body = s.do(http.MethodPost, gone+"/archive", nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
	status, body = s.do(http.MethodGet, gone+"/events?limit=0", nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	status, body = s.do(http.MethodGet, "/v1/sessions/sesn_0000000000000000000000000/events?limit=0", nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	status, body = s.do(http.MethodGet, gone+"/stream?event_deltas[]=bogus", nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")

	// The stream: the same frames as the session's, from connect time.
	st := s.stream(t, tpath+"/stream")
	echo := sendEvents(t, s, sid, userMessage("m1"))
	for _, want := range []string{"user.message", "session.thread_status_running", "session.status_running"} {
		if f := st.next(t); f.name != want {
			t.Errorf("thread stream frame = %q, want %q", f.name, want)
		} else if want == "user.message" && f.data["id"] != echo[0]["id"] {
			t.Errorf("streamed id %v, want %v", f.data["id"], echo[0]["id"])
		}
	}
	st.expectNone(t)
	status, res := s.do(http.MethodGet, tpath+"/stream?event_deltas[]=bogus", nil)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
}

// A child's rows are its own view; only what it cross-posts reaches the
// session view, named there by session_thread_id and unnamed on its own.
func TestChildThreadViewAndCrossPosts(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	child := insertChild(t, s, sid, "idle")
	primary := domain.PrimaryThreadID(domain.ID(sid)).String()
	sendEvents(t, s, sid, userMessage("m0"))

	log := events.NewLog(s.pool)
	appended, err := log.Append(context.Background(), domain.ID(sid), []events.NewEvent{
		{Type: domain.EventAgentMessage, ThreadID: domain.ID(child),
			Payload: []byte(`{"content":[{"type":"text","text":"private"}]}`)},
		{Type: domain.EventAgentToolUse, ThreadID: domain.ID(child), CrossPosted: true,
			Payload: []byte(`{"tool_use_id":"toolu_1","name":"bash","input":{},"session_thread_id":null}`)},
		{Type: domain.EventSessionThreadStatusIdle, ThreadID: domain.ID(child), CrossPosted: true,
			Payload: []byte(`{"session_thread_id":"` + child + `","agent_name":"worker","stop_reason":{"type":"end_turn"}}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	byID := func(evs []map[string]any, id string) map[string]any {
		for _, ev := range evs {
			if ev["id"] == id {
				return ev
			}
		}
		return nil
	}
	// Session view: the cross-posts, not the private message; the tool use
	// names its thread; the primary's view is the same.
	_, res := s.do(http.MethodGet, "/v1/sessions/"+sid+"/events", nil)
	sess := listData(t, res)
	if byID(sess, appended[0].ID.String()) != nil {
		t.Errorf("session view carries the child's private agent.message")
	}
	if tu := byID(sess, appended[1].ID.String()); tu == nil || tu["session_thread_id"] != child {
		t.Errorf("session view tool_use = %v, want it named with session_thread_id %s", tu, child)
	}
	if idle := byID(sess, appended[2].ID.String()); idle == nil || idle["session_thread_id"] != child {
		t.Errorf("session view child idle = %v, want it cross-posted", idle)
	}
	_, pres := s.do(http.MethodGet, "/v1/sessions/"+sid+"/threads/"+primary+"/events", nil)
	if prim := listData(t, pres); len(prim) != len(sess) {
		t.Errorf("primary view has %d events, session view %d", len(prim), len(sess))
	}
	// The child's own view: all three, the tool use unnamed.
	status, cres := s.do(http.MethodGet, "/v1/sessions/"+sid+"/threads/"+child+"/events", nil)
	if status != http.StatusOK {
		t.Fatalf("child events: %d %v", status, cres)
	}
	own := listData(t, cres)
	if len(own) != 3 {
		t.Fatalf("child view = %v, want its three rows only", own)
	}
	if tu := byID(own, appended[1].ID.String()); tu["session_thread_id"] != nil {
		t.Errorf("child's own tool_use session_thread_id = %v, want null", tu["session_thread_id"])
	}
	// The child's stream tails its own rows.
	st := s.stream(t, "/v1/sessions/"+sid+"/threads/"+child+"/stream")
	if _, err := log.Append(context.Background(), domain.ID(sid), []events.NewEvent{
		{Type: domain.EventAgentMessage, Payload: []byte(`{"content":[]}`)},
		{Type: domain.EventAgentMessage, ThreadID: domain.ID(child), Payload: []byte(`{"content":[]}`)},
	}); err != nil {
		t.Fatal(err)
	}
	if f := st.next(t); f.name != "agent.message" {
		t.Errorf("child stream frame = %q", f.name)
	}
	st.expectNone(t)
}

// Archive rules: the primary is refused, a non-idle child is refused, an idle
// child terminates with its event cross-posted; idempotent; the session's own
// archive ends every live child and mirrors onto the primary.
func TestThreadArchive(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	primary := domain.PrimaryThreadID(domain.ID(sid)).String()
	path := "/v1/sessions/" + sid + "/threads/"

	status, body := s.do(http.MethodPost, path+primary+"/archive", nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	if msg := errMessage(body); !strings.Contains(msg, "primary thread cannot be archived") {
		t.Errorf("message = %q", msg)
	}
	running := insertChild(t, s, sid, "running")
	status, body = s.do(http.MethodPost, path+running+"/archive", nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	if msg := errMessage(body); !strings.Contains(msg, "only an idle thread can be archived") {
		t.Errorf("message = %q", msg)
	}
	status, body = s.do(http.MethodPost, path+"sthr_0000000000000000000000000/archive", nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
	// A running child makes the session running under the fold (plan 35
	// decision 4); the session was born idle, so the child parks idle from
	// here — still live, so the session's archive below has it to terminate.
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE session_threads SET status = 'idle' WHERE id = $1`, running); err != nil {
		t.Fatal(err)
	}

	// An idle child parked on requires_action: its cross-posted ask and a
	// private custom-tool call are closed with error results on the surfaces
	// each was on, then the thread terminates.
	idle := insertChild(t, s, sid, "idle")
	parked, err := events.NewLog(s.pool).Append(context.Background(), domain.ID(sid), []events.NewEvent{
		{Type: domain.EventAgentToolUse, ThreadID: domain.ID(idle), CrossPosted: true,
			Payload: []byte(`{"tool_use_id":"toolu_a","name":"bash","input":{},"evaluated_permission":"ask","session_thread_id":null}`)},
		{Type: domain.EventAgentCustomToolUse, ThreadID: domain.ID(idle),
			Payload: []byte(`{"custom_tool_use_id":"toolu_b","name":"lookup","input":{},"session_thread_id":null}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, res := s.do(http.MethodPost, path+idle+"/archive", nil)
	if status != http.StatusOK || res["status"] != "terminated" || res["archived_at"] == nil {
		t.Fatalf("archive idle child: %d %v", status, res)
	}
	archivedAt := res["archived_at"]
	status, again := s.do(http.MethodPost, path+idle+"/archive", nil)
	if status != http.StatusOK || again["archived_at"] != archivedAt {
		t.Errorf("second archive: %d %v, want the same archived_at", status, again)
	}
	types := s.eventTypes(sid)
	if !sameStrings(types, []string{"agent.tool_use", "agent.tool_result", "session.thread_status_terminated"}) {
		t.Errorf("session view after child archive = %v, want the ask, its closing result and the terminated event", types)
	}
	_, cres := s.do(http.MethodGet, path+idle+"/events", nil)
	own := listData(t, cres)
	ownTypes := make([]string, 0, len(own))
	for _, ev := range own {
		ownTypes = append(ownTypes, ev["type"].(string))
	}
	if !sameStrings(ownTypes, []string{"agent.tool_use", "agent.custom_tool_use", "agent.tool_result",
		"user.custom_tool_result", "session.thread_status_terminated"}) {
		t.Fatalf("child's own view = %v, want both calls answered before the terminated event", ownTypes)
	}
	if own[2]["tool_use_id"] != parked[0].ID.String() || own[2]["is_error"] != true ||
		own[3]["custom_tool_use_id"] != parked[1].ID.String() || own[3]["processed_at"] == nil {
		t.Errorf("closing results = %v / %v, want error results answering the parked calls, stamped processed", own[2], own[3])
	}
	if own[4]["session_thread_id"] != idle || own[4]["agent_name"] != "worker" {
		t.Errorf("terminated event = %v", own[4])
	}
	if thr := listThreads(t, s, sid); len(thr) != 3 {
		t.Errorf("threads = %d, want primary + 2 children", len(thr))
	}
	// The list pages in creation order on a forward-only cursor: primary,
	// running, idle; a cursor from another list shape is refused.
	var walked []string
	page := ""
	for range 4 {
		_, res := s.do(http.MethodGet, "/v1/sessions/"+sid+"/threads?limit=1"+page, nil)
		for _, th := range listData(t, res) {
			walked = append(walked, th["id"].(string))
		}
		if np := nextPage(t, res); np != "" {
			page = "&page=" + np
			continue
		}
		break
	}
	if !sameStrings(walked, []string{primary, running, idle}) {
		t.Errorf("cursor walk = %v, want [%s %s %s]", walked, primary, running, idle)
	}
	_, evres := s.do(http.MethodGet, "/v1/sessions/"+sid+"/events?limit=1", nil)
	status, body = s.do(http.MethodGet, "/v1/sessions/"+sid+"/threads?page="+nextPage(t, evres), nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")

	// The session's archive: the running child is terminated too, the
	// primary carries the session's archived_at.
	if status, res := s.do(http.MethodPost, "/v1/sessions/"+sid+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive session: %d %v", status, res)
	}
	for _, th := range listThreads(t, s, sid) {
		if th["archived_at"] == nil {
			t.Errorf("thread %v not archived with its session", th["id"])
		}
		if th["id"] != primary && th["status"] != "terminated" {
			t.Errorf("child %v status = %v after the session's archive, want terminated", th["id"], th["status"])
		}
		if th["id"] == primary && th["status"] == "terminated" {
			t.Errorf("the primary terminated on session archive; it never does")
		}
	}
	_, cres = s.do(http.MethodGet, path+running+"/events", nil)
	if own := listData(t, cres); len(own) != 1 || own[0]["type"] != "session.thread_status_terminated" {
		t.Errorf("running child's view after session archive = %v", own)
	}

	// Delete takes the rows with the session.
	if n := threadRows(t, s, sid); n != 3 {
		t.Fatalf("thread rows before delete = %d", n)
	}
	if status, res := s.do(http.MethodDelete, "/v1/sessions/"+sid, nil); status != http.StatusOK {
		t.Fatalf("delete session: %d %v", status, res)
	}
	if n := threadRows(t, s, sid); n != 0 {
		t.Errorf("thread rows after delete = %d, want 0", n)
	}
}

// Deleting a session ends its live children too (decision 12) — the rows go
// with the session, so the termination is broadcast to the open streams, the
// child's own and the session's, ahead of session.deleted.
func TestSessionDeleteEndsLiveChildren(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	child := insertChild(t, s, sid, "idle")
	done := insertChild(t, s, sid, "idle")
	if status, res := s.do(http.MethodPost, "/v1/sessions/"+sid+"/threads/"+done+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive: %d %v", status, res)
	}
	childStream := s.stream(t, "/v1/sessions/"+sid+"/threads/"+child+"/stream")
	sessionStream := s.stream(t, "/v1/sessions/"+sid+"/events/stream")
	if status, res := s.do(http.MethodDelete, "/v1/sessions/"+sid, nil); status != http.StatusOK {
		t.Fatalf("delete: %d %v", status, res)
	}
	for name, st := range map[string]*sseStream{"child": childStream, "session": sessionStream} {
		f := st.next(t)
		if f.name != "session.thread_status_terminated" || f.data["session_thread_id"] != child || f.data["agent_name"] != "worker" {
			t.Errorf("%s stream first frame = %s %v, want the live child's termination", name, f.name, f.data)
		}
		if f := st.next(t); f.name != "session.deleted" {
			t.Errorf("%s stream second frame = %s, want session.deleted", name, f.name)
		}
	}
	if n := threadRows(t, s, sid); n != 0 {
		t.Errorf("thread rows after delete = %d, want 0", n)
	}
}

// A session update that rewrites the resolved agent reaches the primary's
// thread agent on the next read — no second write, nothing to go stale.
func TestPrimaryThreadAgentReadsThrough(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	primary := domain.PrimaryThreadID(domain.ID(sid)).String()
	status, upd := s.do(http.MethodPost, "/v1/sessions/"+sid, map[string]any{
		"agent": map[string]any{"tools": []any{map[string]any{"type": "agent_toolset_20260401"}}},
	})
	if status != http.StatusOK {
		t.Fatalf("update: %d %v", status, upd)
	}
	_, th := s.do(http.MethodGet, "/v1/sessions/"+sid+"/threads/"+primary, nil)
	agent, _ := th["agent"].(map[string]any)
	tools, _ := agent["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("thread agent tools = %v, want the patched toolset", agent["tools"])
	}
	if ts, _ := tools[0].(map[string]any); ts["default_config"] == nil {
		t.Errorf("thread agent toolset = %v, want it materialized", tools[0])
	}
	var raw json.RawMessage
	if err := s.pool.QueryRow(context.Background(), `SELECT agent FROM session_threads WHERE id = $1`, primary).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != nil {
		t.Errorf("primary thread row stores an agent: %s", raw)
	}
	// A child's snapshot renders the SDK's required arrays as [] even when the
	// stored document omits them; the schema refuses a child without one.
	bare := domain.NewID(domain.PrefixSessionThread).String()
	if _, err := s.pool.Exec(context.Background(),
		`INSERT INTO session_threads (id, session_id, parent_thread_id, agent, agent_name, status)
		 VALUES ($1, $2, $3, '{"id":"agent_y","type":"agent","version":1,"name":"bare","model":{"id":"m"}}', 'bare', 'idle')`,
		bare, sid, primary); err != nil {
		t.Fatal(err)
	}
	_, bth := s.do(http.MethodGet, "/v1/sessions/"+sid+"/threads/"+bare, nil)
	bagent, _ := bth["agent"].(map[string]any)
	for _, k := range []string{"tools", "mcp_servers", "skills"} {
		if arr, ok := bagent[k].([]any); !ok || len(arr) != 0 {
			t.Errorf("child agent %s = %v, want []", k, bagent[k])
		}
	}
	if _, err := s.pool.Exec(context.Background(),
		`INSERT INTO session_threads (id, session_id, parent_thread_id, agent_name, status) VALUES ($1, $2, $3, 'x', 'idle')`,
		domain.NewID(domain.PrefixSessionThread).String(), sid, primary); err == nil {
		t.Error("a child row without an agent snapshot was accepted")
	}
}
