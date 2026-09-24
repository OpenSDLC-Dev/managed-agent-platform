package api_test

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// No events surface of a session that delegates renders a delegation call or
// the settlement's answer to it (#675): the reference's lists show neither on
// the session view, the primary thread's or a child's own, while an ordinary
// tool call — a cross-posted ask-gated bash among them — reaches every surface
// it did before. The rows stay in the log; only the wire projection drops them.

// rosterSession creates a coordinator's session, a roster of one member, in an
// environment of the given kind — the only kind of session whose brain stamps
// a delegation call allow, and so the only one whose surfaces hide them.
func rosterSession(t *testing.T, s *tserver, envKind string) string {
	t.Helper()
	member := createAgent(t, s, map[string]any{"name": "worker", "model": "claude-opus-4-8"})["id"]
	coord := createAgent(t, s, map[string]any{"name": "coordinator", "model": "claude-opus-4-8",
		"multiagent": map[string]any{"type": "coordinator", "agents": []any{member}}})["id"]
	env := createEnvironment(t, s, map[string]any{"name": "roster-env", "config": map[string]any{"type": envKind}})["id"]
	return createSession(t, s, map[string]any{"agent": coord, "environment_id": env})["id"].(string)
}

// delegationPair plants one delegation call on a thread's log and the
// agent.tool_result a settlement answers it with, and returns both ids.
func delegationPair(t *testing.T, s *tserver, sid string, tid domain.ID, name string) []string {
	return answeredDelegation(t, s, sid, tid, name, false)
}

// answeredDelegation is delegationPair with the answer's is_error chosen: a
// refused call — a name off the roster, the thread cap, the other role's half —
// is stamped allow like any other and answered is_error in the same commit.
func answeredDelegation(t *testing.T, s *tserver, sid string, tid domain.ID, name string, isError bool) []string {
	t.Helper()
	use := appendOn(t, s, sid, tid, false, domain.EventAgentToolUse,
		`{"name":"`+name+`","input":{},"evaluated_permission":"allow","session_thread_id":null}`)
	res := appendOn(t, s, sid, tid, false, domain.EventAgentToolResult,
		`{"tool_use_id":"`+use+`","content":[{"type":"text","text":"Message sent."}],"is_error":`+strconv.FormatBool(isError)+`}`)
	return []string{use, res}
}

// answeredBash plants an allowed bash call on a thread's log and the
// platform's agent.tool_result answering it — the ordinary pair whose result
// has the same type as a delegation answer's, and so is what proves the
// filter keys on the call's name.
func answeredBash(t *testing.T, s *tserver, sid string, tid domain.ID) (use, res string) {
	t.Helper()
	use = appendOn(t, s, sid, tid, false, domain.EventAgentToolUse, allowBashCall)
	res = appendOn(t, s, sid, tid, false, domain.EventAgentToolResult,
		`{"tool_use_id":"`+use+`","content":[{"type":"text","text":"ok"}],"is_error":false}`)
	return use, res
}

func TestNoEventsSurfaceRendersADelegationCall(t *testing.T) {
	s := newTestServer(t)
	sid := rosterSession(t, s, "cloud")
	primary := domain.PrimaryThreadID(domain.ID(sid)).String()
	child := insertChild(t, s, sid, "running")
	base := "/v1/sessions/" + sid
	sessionStream := s.stream(t, base+"/events/stream")
	primaryStream := s.stream(t, base+"/threads/"+primary+"/stream")
	childStream := s.stream(t, base+"/threads/"+child+"/stream")

	// All six, each on the thread that is offered it, each answered in the
	// commit a settlement would write — planted ahead of every row a surface
	// keeps, so a stream that emitted one would emit it first.
	var hidden []string
	for _, name := range []string{"create_agent", "send_to_agent", "list_agents", "wait_for_agents"} {
		hidden = append(hidden, delegationPair(t, s, sid, "", name)...)
	}
	for _, name := range []string{"send_to_parent", "submit_result"} {
		hidden = append(hidden, delegationPair(t, s, sid, domain.ID(child), name)...)
	}
	// Refused calls go the same way — one recorded refusal matches, the rest
	// are INFERRED (docs/DIVERGENCES.md, "Refused delegation calls"): a spawn
	// the roster refused, and each role reaching for the other's half — both
	// classed as settlement work, stamped allow.
	hidden = append(hidden, answeredDelegation(t, s, sid, "", "create_agent", true)...)
	hidden = append(hidden, answeredDelegation(t, s, sid, "", "submit_result", true)...)
	hidden = append(hidden, answeredDelegation(t, s, sid, domain.ID(child), "create_agent", true)...)
	bashUse, bashRes := answeredBash(t, s, sid, "")
	ask := appendOn(t, s, sid, domain.ID(child), true, domain.EventAgentToolUse, askBashCall)

	surfaces := []struct {
		path string
		want []string
	}{
		// The session view and the primary thread's are one surface.
		{base + "/events", []string{bashUse, bashRes, ask}},
		{base + "/threads/" + primary + "/events", []string{bashUse, bashRes, ask}},
		{base + "/threads/" + child + "/events", []string{ask}},
	}
	for _, sf := range surfaces {
		view := eventsByID(t, s, sf.path)
		for _, id := range hidden {
			if ev, ok := view[id]; ok {
				t.Errorf("%s renders a delegation row: %v", sf.path, ev)
			}
		}
		for _, id := range sf.want {
			if _, ok := view[id]; !ok {
				t.Errorf("%s is missing the ordinary tool row %s", sf.path, id)
			}
		}
	}
	if ev := eventsByID(t, s, base+"/events")[ask]; ev["session_thread_id"] != child {
		t.Errorf("the cross-posted ask = %v, want it naming its thread %s", ev, child)
	}

	for name, st := range map[string]struct {
		stream *sseStream
		want   []string
	}{
		"session": {sessionStream, []string{bashUse, bashRes, ask}},
		"primary": {primaryStream, []string{bashUse, bashRes, ask}},
		"child":   {childStream, []string{ask}},
	} {
		for _, id := range st.want {
			if f := st.stream.next(t); f.data["id"] != id {
				t.Fatalf("%s stream frame = %q %v, want %s — no delegation row ahead of it", name, f.name, f.data, id)
			}
		}
	}
}

// The self_hosted widening pulls a child's tool calls and their results back
// into the session view (plan 35 decision 13 i) — and a child's send_to_parent
// or submit_result with them, which a cloud session's view never carried
// because neither is cross-posted. The name filter holds across both kinds, so
// the exposure no longer differs by environment.
func TestSelfHostedSessionViewHidesAChildsDelegationCalls(t *testing.T) {
	s := newTestServer(t)
	sid := rosterSession(t, s, "self_hosted")
	primary := domain.PrimaryThreadID(domain.ID(sid)).String()
	st := s.stream(t, "/v1/sessions/"+sid+"/events/stream")
	child := insertChild(t, s, sid, "running")
	tid := domain.ID(child)

	hidden := append(delegationPair(t, s, sid, tid, "send_to_parent"), delegationPair(t, s, sid, tid, "submit_result")...)
	use, res := answeredBash(t, s, sid, tid)

	for _, path := range []string{"/v1/sessions/" + sid + "/events", "/v1/sessions/" + sid + "/threads/" + primary + "/events"} {
		view := eventsByID(t, s, path)
		for _, id := range hidden {
			if ev, ok := view[id]; ok {
				t.Errorf("%s: a child's delegation row reached the widened view: %v", path, ev)
			}
		}
		if ev := view[use]; ev == nil || ev["session_thread_id"] != child {
			t.Errorf("%s: the child's bash call = %v, want it widened in, naming %s", path, ev, child)
		}
		if _, ok := view[res]; !ok {
			t.Errorf("%s: the result answering the child's bash call is missing", path)
		}
	}
	for _, id := range []string{use, res} {
		if f := st.next(t); f.data["id"] != id {
			t.Fatalf("stream frame = %q %v, want %s — no delegation row ahead of it", f.name, f.data, id)
		}
	}
}

// The filter is part of the query, not a pass over its result: a page is
// still limit rows long when delegation rows fall inside or at its edge, and
// the cursor minted from its last row resumes after exactly that row — a
// post-filter would return short pages, and one that trimmed after the cursor
// was minted would skip what it trimmed.
func TestDelegationRowsNeverShortenAPage(t *testing.T) {
	s := newTestServer(t)
	sid := rosterSession(t, s, "cloud")
	primary := domain.PrimaryThreadID(domain.ID(sid)).String()
	msg := func() string {
		return appendOn(t, s, sid, "", false, domain.EventAgentMessage, `{"content":[{"type":"text","text":"m"}]}`)
	}
	// Two visible rows, then a pair on the limit-2 page boundary, then rows
	// split by pairs: the hidden rows sit inside pages and on their edges.
	var visible []string
	visible = append(visible, msg(), msg())
	delegationPair(t, s, sid, "", "create_agent")
	visible = append(visible, msg())
	delegationPair(t, s, sid, "", "wait_for_agents")
	delegationPair(t, s, sid, "", "send_to_agent")
	visible = append(visible, msg(), msg())
	delegationPair(t, s, sid, "", "list_agents")

	reversed := slices.Clone(visible)
	slices.Reverse(reversed)
	for _, tc := range []struct {
		path, order string
		want        []string
	}{
		{"/v1/sessions/" + sid + "/events", "", visible},
		{"/v1/sessions/" + sid + "/events", "desc", reversed},
		{"/v1/sessions/" + sid + "/threads/" + primary + "/events", "", visible},
	} {
		for _, limit := range []int{1, 2, 3} {
			var got []string
			var pages [][]string
			cursor := ""
			for {
				q := url.Values{"limit": {strconv.Itoa(limit)}}
				if tc.order != "" {
					q.Set("order", tc.order)
				}
				if cursor != "" {
					q.Set("page", cursor)
				}
				status, res := s.do(http.MethodGet, tc.path+"?"+q.Encode(), nil)
				if status != http.StatusOK {
					t.Fatalf("GET %s?%s: %d %v", tc.path, q.Encode(), status, res)
				}
				var page []string
				for _, ev := range listData(t, res) {
					page = append(page, ev["id"].(string))
				}
				pages = append(pages, page)
				got = append(got, page...)
				next, _ := res["next_page"].(string)
				if next == "" {
					break
				}
				cursor = next
				if len(pages) > len(tc.want)+1 {
					t.Fatalf("%s order=%q limit=%d: more pages than visible rows: %v", tc.path, tc.order, limit, pages)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("%s order=%q limit=%d: walked %v, want %v", tc.path, tc.order, limit, got, tc.want)
			}
			for i, page := range pages[:len(pages)-1] {
				if len(page) != limit {
					t.Errorf("%s order=%q limit=%d: page %d holds %d rows, want a full page: %v",
						tc.path, tc.order, limit, i, len(page), pages)
				}
			}
		}
	}
}

// The filter is taken only on a session whose snapshot carries a roster —
// where the brain classes the six names as settlement work and stamps them
// allow. A single-agent session is offered none of them and stamps one deny
// (#567), which the filter would spare anyway, so its surfaces list without
// it: an allow-stamped pair planted there, a state no brain writes, stays on
// the session view, the primary thread's list and the stream alike.
func TestASingleAgentSessionListsWithoutTheDelegationFilter(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	primary := domain.PrimaryThreadID(domain.ID(sid)).String()
	base := "/v1/sessions/" + sid
	sessionStream := s.stream(t, base+"/events/stream")
	primaryStream := s.stream(t, base+"/threads/"+primary+"/stream")

	pair := delegationPair(t, s, sid, "", "create_agent")
	for _, path := range []string{base + "/events", base + "/threads/" + primary + "/events"} {
		view := eventsByID(t, s, path)
		for _, id := range pair {
			if _, ok := view[id]; !ok {
				t.Errorf("%s dropped %s: a single-agent session took the delegation filter", path, id)
			}
		}
	}
	for name, st := range map[string]*sseStream{"session": sessionStream, "primary": primaryStream} {
		for _, id := range pair {
			if f := st.next(t); f.data["id"] != id {
				t.Fatalf("%s stream frame = %q %v, want %s", name, f.name, f.data, id)
			}
		}
	}
}
