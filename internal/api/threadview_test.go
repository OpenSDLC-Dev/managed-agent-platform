package api_test

import (
	"net/http"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// The self_hosted view rule (plan 35 decision 13 i): a BYOC worker reads the
// session view and nothing else, so on a self_hosted session that view also
// carries every child thread's tool call and the results answering it. Cloud
// sessions keep the condensed view.

// childToolCalls plants, on one child thread of sid, the allow-policy call a
// worker must run, the worker's answer, the platform's synthesized answer,
// and an MCP call no worker ever runs.
func childToolCalls(t *testing.T, s *tserver, sid string) (child, use, workerRes, agentRes, mcpUse string) {
	t.Helper()
	child = insertChild(t, s, sid, "running")
	tid := domain.ID(child)
	use = appendOn(t, s, sid, tid, false, domain.EventAgentToolUse, allowBashCall)
	workerRes = appendOn(t, s, sid, tid, false, domain.EventUserToolResult,
		`{"tool_use_id":"`+use+`","content":[{"type":"text","text":"ok"}],"is_error":false,"session_thread_id":null}`)
	agentRes = appendOn(t, s, sid, tid, false, domain.EventAgentToolResult,
		`{"tool_use_id":"`+use+`","content":[{"type":"text","text":"interrupted"}],"is_error":true}`)
	mcpUse = appendOn(t, s, sid, tid, false, domain.EventAgentMCPToolUse,
		`{"name":"mcp__docs__search","input":{},"session_thread_id":null}`)
	return child, use, workerRes, agentRes, mcpUse
}

// eventsByID reads an events list and keys it by event id.
func eventsByID(t *testing.T, s *tserver, path string) map[string]map[string]any {
	t.Helper()
	status, res := s.do(http.MethodGet, path, nil)
	if status != http.StatusOK {
		t.Fatalf("GET %s: %d %v", path, status, res)
	}
	out := map[string]map[string]any{}
	for _, ev := range listData(t, res) {
		id, _ := ev["id"].(string)
		out[id] = ev
	}
	return out
}

func TestSelfHostedSessionViewCarriesChildToolCalls(t *testing.T) {
	s := newTestServer(t)
	sid := selfHostedSession(t, s)
	primary := domain.PrimaryThreadID(domain.ID(sid)).String()
	st := s.stream(t, "/v1/sessions/"+sid+"/events/stream")
	child, use, workerRes, agentRes, mcpUse := childToolCalls(t, s, sid)

	// The session view and the primary thread's are the same surface.
	for _, path := range []string{"/v1/sessions/" + sid + "/events", "/v1/sessions/" + sid + "/threads/" + primary + "/events"} {
		view := eventsByID(t, s, path)
		if ev := view[use]; ev == nil || ev["session_thread_id"] != child {
			t.Errorf("%s: the child's call = %v, want it carrying session_thread_id %s", path, ev, child)
		}
		if ev := view[workerRes]; ev == nil || ev["session_thread_id"] != child {
			t.Errorf("%s: the worker's result = %v, want it carrying session_thread_id %s", path, ev, child)
		}
		// agent.tool_result has no session_thread_id on the wire (SDK
		// betasessionevent.go:878-902), so it is rendered without one.
		ev := view[agentRes]
		if ev == nil {
			t.Errorf("%s: the platform's result is missing from the view", path)
		} else if _, ok := ev["session_thread_id"]; ok {
			t.Errorf("%s: the platform's result renders session_thread_id: %v", path, ev)
		}
		if _, ok := view[mcpUse]; ok {
			t.Errorf("%s: the child's MCP call reached the session view", path)
		}
	}

	// The child's own surface is unchanged: its rows, with the stored null.
	own := eventsByID(t, s, "/v1/sessions/"+sid+"/threads/"+child+"/events")
	if len(own) != 4 {
		t.Errorf("the child's own view holds %d events, want its four", len(own))
	}
	if ev := own[use]; ev == nil || ev["session_thread_id"] != nil {
		t.Errorf("the child's call on its own view = %v, want session_thread_id null", ev)
	}

	// The stream widens with the list — it rebuilds its own query per wake.
	for _, want := range []struct{ id, typ string }{
		{use, "agent.tool_use"}, {workerRes, "user.tool_result"}, {agentRes, "agent.tool_result"},
	} {
		f := st.next(t)
		if f.name != want.typ || f.data["id"] != want.id {
			t.Fatalf("stream frame = %q %v, want %s %s", f.name, f.data, want.typ, want.id)
		}
	}
}

func TestCloudSessionViewOmitsChildToolCalls(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	st := s.stream(t, "/v1/sessions/"+sid+"/events/stream")
	_, use, workerRes, agentRes, mcpUse := childToolCalls(t, s, sid)

	view := eventsByID(t, s, "/v1/sessions/"+sid+"/events")
	for _, id := range []string{use, workerRes, agentRes, mcpUse} {
		if ev, ok := view[id]; ok {
			t.Errorf("a cloud session's view carries the child row %v", ev)
		}
	}

	// A row the view does keep is the barrier: the stream reaches it without
	// having emitted any of the child's.
	marker := appendOn(t, s, sid, "", false, domain.EventAgentMessage, `{"content":[{"type":"text","text":"done"}]}`)
	if f := st.next(t); f.data["id"] != marker {
		t.Errorf("the cloud stream's first frame = %q %v, want the primary's message", f.name, f.data)
	}
}
