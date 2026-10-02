package api_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// The multiagent roster (plan 35 slice 1, decision 10): an agent's
// `multiagent` is resolved at create/update into pinned {id,type,version}
// references, and a session snapshots each member's full definition.

// rosterOf reads the response's multiagent.agents as a slice of maps.
func rosterOf(t *testing.T, res map[string]any) []map[string]any {
	t.Helper()
	ma, ok := res["multiagent"].(map[string]any)
	if !ok {
		t.Fatalf("multiagent = %v, want an object", res["multiagent"])
	}
	if ma["type"] != "coordinator" {
		t.Errorf("multiagent.type = %v, want coordinator", ma["type"])
	}
	raw, ok := ma["agents"].([]any)
	if !ok {
		t.Fatalf("multiagent.agents = %v, want an array", ma["agents"])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("multiagent.agents entry = %v, want an object", e)
		}
		out = append(out, m)
	}
	return out
}

func member(t *testing.T, s *tserver, name string) string {
	t.Helper()
	return createAgent(t, s, map[string]any{"name": name, "model": "claude-opus-4-8", "system": name + " system"})["id"].(string)
}

// Create resolves the three entry forms: a bare id and a versionless reference
// pin the member's current version eagerly, an explicit version is kept, and
// self resolves to the coordinator's own id and version 1.
func TestRosterCreateResolvesEntries(t *testing.T) {
	s := newTestServer(t)
	a := member(t, s, "worker-a")
	b := member(t, s, "worker-b")
	// b at version 2: a versionless reference must pin 2, an explicit one 1.
	if status, body := s.do(http.MethodPost, "/v1/agents/"+b, map[string]any{"system": "b v2"}); status != http.StatusOK {
		t.Fatalf("update b: %d %v", status, body)
	}
	coord := createAgent(t, s, map[string]any{
		"name": "coordinator", "model": "claude-opus-4-8",
		"multiagent": map[string]any{"type": "coordinator", "agents": []any{
			a,
			map[string]any{"type": "agent", "id": b},
			map[string]any{"type": "self"},
		}},
	})
	got := rosterOf(t, coord)
	if len(got) != 3 {
		t.Fatalf("roster = %v, want 3 entries", got)
	}
	want := []map[string]any{
		{"id": a, "type": "agent", "version": float64(1)},
		{"id": b, "type": "agent", "version": float64(2)},
		{"id": coord["id"], "type": "agent", "version": float64(1)},
	}
	for i := range want {
		for k, v := range want[i] {
			if got[i][k] != v {
				t.Errorf("agents[%d].%s = %v, want %v", i, k, got[i][k], v)
			}
		}
		if len(got[i]) != 3 {
			t.Errorf("agents[%d] = %v, want exactly id/type/version", i, got[i])
		}
	}

	// The eager pin survives a member update: b's roster entry stays at 2.
	if status, body := s.do(http.MethodPost, "/v1/agents/"+b, map[string]any{"system": "b v3"}); status != http.StatusOK {
		t.Fatalf("update b again: %d %v", status, body)
	}
	_, again := s.do(http.MethodGet, "/v1/agents/"+coord["id"].(string), nil)
	if rosterOf(t, again)[1]["version"] != float64(2) {
		t.Errorf("after member update: %v, want b still pinned at 2", rosterOf(t, again))
	}
	// An explicit version is kept as given.
	pinned := createAgent(t, s, map[string]any{
		"name": "coordinator-2", "model": "claude-opus-4-8",
		"multiagent": map[string]any{"type": "coordinator", "agents": []any{
			map[string]any{"type": "agent", "id": b, "version": 1},
		}},
	})
	if rosterOf(t, pinned)[0]["version"] != float64(1) {
		t.Errorf("explicit version: %v, want 1", rosterOf(t, pinned))
	}
}

// Update: self resolves to the version the update produces; a sent roster
// replaces the stored one as a whole; null clears it; an omitted key leaves
// it exactly as stored.
func TestRosterUpdateSelfAndReplacement(t *testing.T) {
	s := newTestServer(t)
	a := member(t, s, "worker-a")
	b := member(t, s, "worker-b")
	coord := createAgent(t, s, map[string]any{"name": "coordinator", "model": "claude-opus-4-8"})
	id := coord["id"].(string)
	if coord["multiagent"] != nil {
		t.Fatalf("fresh agent multiagent = %v, want null", coord["multiagent"])
	}

	status, res := s.do(http.MethodPost, "/v1/agents/"+id, map[string]any{
		"multiagent": map[string]any{"type": "coordinator", "agents": []any{a, map[string]any{"type": "self"}}},
	})
	if status != http.StatusOK {
		t.Fatalf("update: %d %v", status, res)
	}
	got := rosterOf(t, res)
	if len(got) != 2 || got[0]["id"] != a || got[0]["version"] != float64(1) || got[1]["id"] != id || got[1]["version"] != float64(2) {
		t.Errorf("roster after update = %v, want [a@1, self@2]", got)
	}

	// The rendered roster round-trips: self renders as an ordinary reference
	// to the coordinator at its current version, and echoing it back (the
	// read-modify-write of a roster "replaced as a whole") is the self entry
	// again — any entry naming the coordinator's own id is.
	_, rendered := s.do(http.MethodGet, "/v1/agents/"+id, nil)
	status, res = s.do(http.MethodPost, "/v1/agents/"+id, map[string]any{"name": "echoed", "multiagent": rendered["multiagent"]})
	if status != http.StatusOK {
		t.Fatalf("echo: %d %v", status, res)
	}
	if got := rosterOf(t, res); len(got) != 2 || got[0]["id"] != a || got[0]["version"] != float64(1) || got[1]["id"] != id || got[1]["version"] != float64(3) {
		t.Errorf("roster after echo = %v, want [a@1, self@3]", got)
	}
	// A bare own id, or one pinned to the version being written, is self
	// too; an own id at some other version is not this coordinator's self.
	status, res = s.do(http.MethodPost, "/v1/agents/"+id, map[string]any{"multiagent": map[string]any{"type": "coordinator", "agents": []any{id, a}}})
	if status != http.StatusOK || rosterOf(t, res)[0]["version"] != float64(4) {
		t.Errorf("bare own id: %d %v, want self@4 first", status, res)
	}
	status, res = s.do(http.MethodPost, "/v1/agents/"+id, map[string]any{"multiagent": map[string]any{"type": "coordinator", "agents": []any{
		map[string]any{"type": "agent", "id": id, "version": 5}, a}}})
	if status != http.StatusOK || rosterOf(t, res)[0]["version"] != float64(5) {
		t.Errorf("own id at the written version: %d %v, want self@5 first", status, res)
	}
	for _, entries := range [][]any{
		{map[string]any{"type": "agent", "id": id, "version": 1}},
		{map[string]any{"type": "self"}, id},
	} {
		status, body := s.do(http.MethodPost, "/v1/agents/"+id, map[string]any{"multiagent": map[string]any{"type": "coordinator", "agents": entries}})
		wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	}
	if _, cur := s.do(http.MethodGet, "/v1/agents/"+id, nil); cur["version"] != float64(5) {
		t.Fatalf("rejected updates changed the agent: %v", cur)
	}
	// Back to [a, self] for the rest.
	status, res = s.do(http.MethodPost, "/v1/agents/"+id, map[string]any{"multiagent": map[string]any{"type": "coordinator", "agents": []any{a, map[string]any{"type": "self"}}}})
	if status != http.StatusOK || rosterOf(t, res)[1]["version"] != float64(6) {
		t.Fatalf("reset: %d %v", status, res)
	}

	// Omitted: a name-only update keeps the roster's members as pinned, but
	// self moves with the coordinator — it means this coordinator at the
	// version a session resolves, so a session on version 7 still finds it.
	status, res = s.do(http.MethodPost, "/v1/agents/"+id, map[string]any{"name": "coordinator-renamed"})
	if status != http.StatusOK {
		t.Fatalf("rename: %d %v", status, res)
	}
	if got := rosterOf(t, res); len(got) != 2 || got[0]["id"] != a || got[0]["version"] != float64(1) || got[1]["id"] != id || got[1]["version"] != float64(7) {
		t.Errorf("roster after rename = %v, want [a@1, self@7]", got)
	}
	envID := createEnvironment(t, s, map[string]any{"name": "env"})["id"].(string)
	sess := createSession(t, s, map[string]any{
		"agent":          map[string]any{"type": "agent_with_overrides", "id": id, "system": "overridden"},
		"environment_id": envID,
	})
	sa, _ := sess["agent"].(map[string]any)
	if snap := rosterOf(t, sa); len(snap) != 2 || snap[1]["system"] != "overridden" || snap[1]["name"] != "coordinator-renamed" {
		t.Errorf("session on the kept roster = %v, want the self copy from the overridden version-7 spec", snap)
	}
	// Replaced as a whole.
	status, res = s.do(http.MethodPost, "/v1/agents/"+id, map[string]any{
		"multiagent": map[string]any{"type": "coordinator", "agents": []any{b}},
	})
	if status != http.StatusOK {
		t.Fatalf("replace: %d %v", status, res)
	}
	if got := rosterOf(t, res); len(got) != 1 || got[0]["id"] != b {
		t.Errorf("roster after replace = %v, want [b]", got)
	}
	// Cleared.
	status, res = s.do(http.MethodPost, "/v1/agents/"+id, map[string]any{"multiagent": nil})
	if status != http.StatusOK {
		t.Fatalf("clear: %d %v", status, res)
	}
	if res["multiagent"] != nil {
		t.Errorf("roster after clear = %v, want null", res["multiagent"])
	}
	// The pinned versions render the roster of their time.
	_, v2 := s.do(http.MethodGet, "/v1/agents/"+id+"?version=2", nil)
	if got := rosterOf(t, v2); len(got) != 2 || got[0]["id"] != a || got[0]["version"] != float64(1) || got[1]["id"] != id || got[1]["version"] != float64(2) {
		t.Errorf("version 2 roster = %v, want [a@1, self@2]", got)
	}
}

// Every documented constraint rejects with a 400, and the coordinator is not
// stored. A constraint the reference was recorded refusing on create answers in
// its words, verbatim (#540); the rest name the entry, in ours.
func TestRosterConstraints(t *testing.T) {
	s := newTestServer(t)
	a := member(t, s, "worker-a")
	archived := member(t, s, "worker-archived")
	if status, body := s.do(http.MethodPost, "/v1/agents/"+archived+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive: %d %v", status, body)
	}
	nested := createAgent(t, s, map[string]any{"name": "nested", "model": "claude-opus-4-8",
		"multiagent": map[string]any{"type": "coordinator", "agents": []any{a}}})["id"].(string)
	many := make([]any, 21)
	for i := range many {
		many[i] = member(t, s, fmt.Sprintf("w%d", i))
	}
	roster := func(entries ...any) map[string]any {
		return map[string]any{"type": "coordinator", "agents": entries}
	}
	self := map[string]any{"type": "self"}
	// roster() with no entries sends agents:null; the recorded request sent [].
	empty := map[string]any{"type": "coordinator", "agents": []any{}}
	// The reference's sentences, each request shaped like the recorded one with
	// this server's ids standing in for the reference's, which fail its id
	// alphabet. Recording: managed-agents-wire-recordings
	// 2026-09-02/batch2.json, the idx and probe each case names.
	recorded := []struct {
		name string
		body any
		want string
	}{
		{"C-1 empty (idx 122 agent.create.roster.empty)", empty,
			"multiagent.coordinator.agents: must contain at least 1 item"},
		{"C-2 duplicate ids (idx 118 agent.create.roster.duplicate-member)", roster(a, a),
			"Agent has invalid configuration: subagent " + a + " referenced multiple times"},
		{"C-2 one agent at two versions (idx 119 agent.create.roster.same-agent-two-versions)",
			roster(map[string]any{"type": "agent", "id": a, "version": 1}, map[string]any{"type": "agent", "id": a, "version": 2}),
			"Agent has invalid configuration: subagent " + a + " referenced multiple times"},
		{"C-3 two selfs (idx 120 agent.create.roster.two-self)", roster(a, self, self),
			`multiagent.agents.2: at most one {"type":"self"} entry is allowed`},
		{"C-4 missing version (idx 121 agent.create.roster.version-999)",
			roster(map[string]any{"type": "agent", "id": a, "version": 999}),
			"Agent has invalid configuration: subagent " + a + " version 999 not found"},
		{"C-4 version past int32", roster(map[string]any{"type": "agent", "id": a, "version": 2147483648}),
			"Agent has invalid configuration: subagent " + a + " version 2147483648 not found"},
		{"C-5 archived member (idx 149 agent.create.roster.archived-member)", roster(archived),
			"Agent has invalid configuration: subagent " + archived + " is archived"},
		{"C-6 nested coordinator (idx 137 agent.create.roster.member-has-multiagent)", roster(nested),
			"Agent has invalid configuration: subagent " + nested + " has its own subagents; maximum depth is 1"},
		// The recorded id itself, which this server's alphabet refuses too.
		{"entry not an agent id (idx 123 agent.create.roster.unknown-agent)", roster("agent_01UnknownAgentIdXXXXXXXX"),
			"Agent has invalid configuration: subagent agent_01UnknownAgentIdXXXXXXXX is not a valid agent ID"},
		{"entry not an agent id, object form", roster(map[string]any{"type": "agent", "id": "bogus"}),
			"Agent has invalid configuration: subagent bogus is not a valid agent ID"},
		{"entry null (idx 117 agent.create.roster.member-has-multiagent)", roster(nil),
			"Failed to parse request: multiagent.agents[0]: must be a string or an object (got null)"},
	}
	for _, tc := range recorded {
		t.Run(tc.name, func(t *testing.T) {
			status, body := s.do(http.MethodPost, "/v1/agents", map[string]any{
				"name": "coordinator", "model": "claude-opus-4-8", "multiagent": tc.body})
			wantErrMsg(t, status, body, http.StatusBadRequest, "invalid_request_error", tc.want)
		})
	}
	// Ours, the reference's answer being unrecorded.
	cases := []struct {
		name string
		body any
		want string
	}{
		{"C-1 over 20", roster(many...), "multiagent.agents must have between 1 and 20 entries"},
		{"C-1 agents null", roster(), "multiagent.agents must have between 1 and 20 entries"},
		{"C-4 missing agent", roster("agent_0000000000000000000000000"), "multiagent.agents[0]: agent agent_0000000000000000000000000 not found"},
		{"C-7 type not coordinator", map[string]any{"type": "advisor", "agents": []any{a}}, `type must be "coordinator"`},
		{"entry unknown type", roster(map[string]any{"type": "advisor", "model": "m"}), `entry type must be "agent" or "self"`},
		// The strict decoder's sentence every /v1 body shares names the bare
		// key, nested or not (rejectUnknownKeys, #540), so neither carries a
		// roster path. Neither was recorded on a roster.
		{"entry unknown key", roster(map[string]any{"type": "self", "name": "x"}), `Failed to parse request body: unknown field "name"`},
		{"entry bad version", roster(map[string]any{"type": "agent", "id": a, "version": 0}), "positive integer"},
		{"entry empty string", roster(""), "must not be empty"},
		{"entry a number", roster(7), "multiagent.agents[0]: entry must be an agent id string"},
		{"entry id not a string", roster(map[string]any{"type": "agent", "id": 7}), "id must be a string"},
		{"entry id missing", roster(map[string]any{"type": "agent"}), "id is required"},
		{"agents not an array", map[string]any{"type": "coordinator", "agents": "x"}, "agents must be an array"},
		{"unknown roster key", map[string]any{"type": "coordinator", "agents": []any{a}, "max": 3}, `Failed to parse request body: unknown field "max"`},
		{"not an object", []any{a}, "must be an object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := s.do(http.MethodPost, "/v1/agents", map[string]any{
				"name": "coordinator", "model": "claude-opus-4-8", "multiagent": tc.body})
			wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
			if msg := errMessage(body); !strings.Contains(msg, tc.want) {
				t.Errorf("message = %q, want it to mention %q", msg, tc.want)
			} else if strings.HasPrefix(tc.want, "Failed to parse") && msg != tc.want {
				t.Errorf("message = %q, want exactly %q", msg, tc.want)
			}
		})
	}
	// Nothing named "coordinator" was stored by the rejected creates.
	_, list := s.do(http.MethodGet, "/v1/agents?limit=100", nil)
	for _, e := range listData(t, list) {
		if e["name"] == "coordinator" {
			t.Errorf("a rejected coordinator was stored: %v", e)
		}
	}
	// The same constraints bind update, in the same words: a path opens with
	// "agent." there, as the reference's update paths were recorded doing
	// (2026-09-12/batch1.json idx 7 rec91.model.update.effort-bogus, idx 74
	// rec91.web.config.fetch-65), and a member's sentence names no path on
	// either route. An entry naming the coordinator's own id beside
	// {"type":"self"}, which only update can send, is unrecorded and ours.
	c := createAgent(t, s, map[string]any{"name": "c", "model": "claude-opus-4-8"})["id"].(string)
	for _, tc := range []struct {
		body any
		want string
	}{
		{roster(nested), "Agent has invalid configuration: subagent " + nested + " has its own subagents; maximum depth is 1"},
		{empty, "agent.multiagent.coordinator.agents: must contain at least 1 item"},
		{roster(nil), "Failed to parse request: agent.multiagent.agents[0]: must be a string or an object (got null)"},
		{roster(a, self, self), `agent.multiagent.agents.2: at most one {"type":"self"} entry is allowed`},
		{roster(self, c), "multiagent.agents[1]: at most one self entry"},
	} {
		status, body := s.do(http.MethodPost, "/v1/agents/"+c, map[string]any{"multiagent": tc.body})
		wantErrMsg(t, status, body, http.StatusBadRequest, "invalid_request_error", tc.want)
	}
	if _, res := s.do(http.MethodGet, "/v1/agents/"+c, nil); res["version"] != float64(1) || res["multiagent"] != nil {
		t.Errorf("rejected update changed the agent: %v", res)
	}
}

// A session snapshots the roster as full definitions: type "agent", every
// SessionThreadAgent field, no nested multiagent; the self member is the
// session's overridden coordinator spec, the others their pinned versions;
// tools echo resolved.
func TestRosterSessionSnapshot(t *testing.T) {
	s := newTestServer(t)
	a := member(t, s, "worker-a")
	coord := createAgent(t, s, map[string]any{
		"name": "coordinator", "model": "claude-opus-4-8", "system": "coordinator system",
		"tools":      []any{map[string]any{"type": "agent_toolset_20260401"}},
		"multiagent": map[string]any{"type": "coordinator", "agents": []any{a, map[string]any{"type": "self"}}},
	})["id"].(string)
	envID := createEnvironment(t, s, map[string]any{"name": "env"})["id"].(string)

	res := createSession(t, s, map[string]any{
		"agent":          map[string]any{"type": "agent_with_overrides", "id": coord, "system": "overridden"},
		"environment_id": envID,
	})
	agent, _ := res["agent"].(map[string]any)
	if agent["system"] != "overridden" {
		t.Fatalf("agent.system = %v, want the override", agent["system"])
	}
	got := rosterOf(t, agent)
	if len(got) != 2 {
		t.Fatalf("session roster = %v, want 2 members", got)
	}
	fields := []string{"id", "type", "version", "name", "description", "model", "system", "tools", "mcp_servers", "skills"}
	for i, m := range got {
		wantFields(t, m, fields...)
		if len(m) != len(fields) {
			t.Errorf("agents[%d] has keys %v, want exactly %v (no nested multiagent)", i, m, fields)
		}
		if m["type"] != "agent" {
			t.Errorf("agents[%d].type = %v, want agent", i, m["type"])
		}
	}
	if got[0]["id"] != a || got[0]["name"] != "worker-a" || got[0]["system"] != "worker-a system" {
		t.Errorf("member a = %v, want its own pinned definition", got[0])
	}
	if got[1]["id"] != coord || got[1]["name"] != "coordinator" || got[1]["system"] != "overridden" {
		t.Errorf("self member = %v, want the coordinator's overridden spec", got[1])
	}
	// The self member's tools echo resolved toolset configuration, like the
	// coordinator's own.
	tools, _ := got[1]["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("self member tools = %v, want the coordinator's one toolset", got[1]["tools"])
	}
	if ts, _ := tools[0].(map[string]any); ts["default_config"] == nil || ts["configs"] == nil {
		t.Errorf("self member toolset = %v, want it materialized (configs + default_config)", tools[0])
	}
	// GET renders the same snapshot.
	_, again := s.do(http.MethodGet, "/v1/sessions/"+res["id"].(string), nil)
	if a2, _ := again["agent"].(map[string]any); len(rosterOf(t, a2)) != 2 || rosterOf(t, a2)[1]["system"] != "overridden" {
		t.Errorf("GET session roster = %v", a2["multiagent"])
	}

	// A session update patching the coordinator's tools and mcp_servers
	// reaches its self copy — in the response and in the session.updated
	// event alike — and leaves the other member as stored.
	mcp := []any{map[string]any{"type": "url", "name": "docs", "url": "https://mcp.example.com"}}
	status, upd := s.do(http.MethodPost, "/v1/sessions/"+res["id"].(string), map[string]any{
		"agent": map[string]any{
			"tools":       []any{map[string]any{"type": "mcp_toolset", "mcp_server_name": "docs"}},
			"mcp_servers": mcp,
		},
	})
	if status != http.StatusOK {
		t.Fatalf("session update: %d %v", status, upd)
	}
	ua, _ := upd["agent"].(map[string]any)
	checkPatched := func(where string, agent map[string]any) {
		t.Helper()
		self := rosterOf(t, agent)[1]
		tools, _ := self["tools"].([]any)
		servers, _ := self["mcp_servers"].([]any)
		if len(tools) != 1 || len(servers) != 1 {
			t.Fatalf("%s: self member after patch = %v, want the patched tools and mcp_servers", where, self)
		}
		if ts, _ := tools[0].(map[string]any); ts["type"] != "mcp_toolset" || ts["default_config"] == nil {
			t.Errorf("%s: self member tools = %v, want the patched mcp_toolset, materialized", where, tools)
		}
		if sv, _ := servers[0].(map[string]any); sv["name"] != "docs" {
			t.Errorf("%s: self member mcp_servers = %v, want the patched server", where, servers)
		}
		if member := rosterOf(t, agent)[0]; member["system"] != "worker-a system" || len(member["mcp_servers"].([]any)) != 0 {
			t.Errorf("%s: other member after patch = %v, want untouched", where, member)
		}
	}
	checkPatched("response", ua)
	_, listed := s.do(http.MethodGet, "/v1/sessions/"+res["id"].(string)+"/events", nil)
	var updated map[string]any
	for _, ev := range listData(t, listed) {
		if ev["type"] == "session.updated" {
			updated = ev
		}
	}
	if updated == nil {
		t.Fatalf("no session.updated event in %v", listed)
	}
	ea, _ := updated["agent"].(map[string]any)
	checkPatched("session.updated", ea)

	// A single-agent session still renders multiagent null.
	plain := createSession(t, s, map[string]any{"agent": a, "environment_id": envID})
	if pa, _ := plain["agent"].(map[string]any); pa["multiagent"] != nil {
		t.Errorf("single-agent session multiagent = %v, want null", pa["multiagent"])
	}
}

// agent_with_overrides cannot carry a roster: an explicit 400, not a silent
// drop — while an explicit null, the value every single-agent session
// renders, reads as absent.
func TestRosterOverrideRejected(t *testing.T) {
	s := newTestServer(t)
	a := member(t, s, "worker-a")
	envID := createEnvironment(t, s, map[string]any{"name": "env"})["id"].(string)
	status, body := s.do(http.MethodPost, "/v1/sessions", map[string]any{
		"agent":          map[string]any{"type": "agent_with_overrides", "id": a, "multiagent": map[string]any{"type": "coordinator", "agents": []any{a}}},
		"environment_id": envID,
	})
	// The reference's strict decoder's sentence (#540). Recording:
	// managed-agents-wire-recordings 2026-09-02/batch2.json idx 139
	// session.create.agent_with_overrides.multiagent.
	wantErrMsg(t, status, body, http.StatusBadRequest, "invalid_request_error",
		`Failed to parse request body: unknown field "multiagent"`)
	status, body = s.do(http.MethodPost, "/v1/sessions", map[string]any{
		"agent":          map[string]any{"type": "agent_with_overrides", "id": a, "system": "x", "multiagent": nil},
		"environment_id": envID,
	})
	if status != http.StatusOK {
		t.Errorf("override multiagent null: %d %v, want a session", status, body)
	}
}

// A member's stored spec answers to the whole-spec caps at session create,
// as the coordinator's own does (resolveAgent): a pre-cap spec fails there.
// A roster is addressed by name and by nothing else, so two members sharing one
// is a member no coordinator could ever spawn: create_agent takes an agent_name,
// the settlement takes the first match, and the second is unreachable for the
// life of the session. Two agents may share a name — the resource has no unique
// constraint — so the refusal belongs where both are on one roster, at the
// session's snapshot, where the names are the pinned ones the coordinator will
// actually address.
func TestRosterRejectsTwoMembersSharingAName(t *testing.T) {
	s := newTestServer(t)
	first, second := member(t, s, "twin"), member(t, s, "twin")
	coord := createAgent(t, s, map[string]any{"name": "coordinator", "model": "claude-opus-4-8",
		"multiagent": map[string]any{"type": "coordinator", "agents": []any{first, second}}})["id"].(string)
	envID := createEnvironment(t, s, map[string]any{"name": "env"})["id"].(string)

	status, body := s.do(http.MethodPost, "/v1/sessions", map[string]any{"agent": coord, "environment_id": envID})
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	msg := errMessage(body)
	for _, want := range []string{second, first, `"twin"`, "spawns a member by name"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message = %q, want it to name %s", msg, want)
		}
	}

	// Distinct names on the same two agents are the control: nothing about a
	// two-member roster is refused, only the collision.
	ok := createAgent(t, s, map[string]any{"name": "coordinator-2", "model": "claude-opus-4-8",
		"multiagent": map[string]any{"type": "coordinator",
			"agents": []any{member(t, s, "alpha"), member(t, s, "beta")}}})["id"].(string)
	if status, body := s.do(http.MethodPost, "/v1/sessions",
		map[string]any{"agent": ok, "environment_id": envID}); status != http.StatusOK {
		t.Errorf("distinct names: status = %d, body %s", status, body)
	}
}

func TestRosterMemberValidatedAtSessionCreate(t *testing.T) {
	s := newTestServer(t)
	a := member(t, s, "worker-a")
	coord := createAgent(t, s, map[string]any{"name": "coordinator", "model": "claude-opus-4-8",
		"multiagent": map[string]any{"type": "coordinator", "agents": []any{a}}})["id"].(string)
	envID := createEnvironment(t, s, map[string]any{"name": "env"})["id"].(string)
	// Past the tools cap, written around the API as an old row could be.
	tools := make([]map[string]any, 129)
	for i := range tools {
		tools[i] = map[string]any{"type": "agent_toolset_20260401"}
	}
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE agent_versions SET spec = jsonb_set(spec, '{tools}', $2) WHERE agent_id = $1 AND version = 1`,
		a, mustJSON(t, tools)); err != nil {
		t.Fatal(err)
	}
	status, body := s.do(http.MethodPost, "/v1/sessions", map[string]any{"agent": coord, "environment_id": envID})
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	if msg := errMessage(body); !strings.Contains(msg, "multiagent member "+a) || !strings.Contains(msg, "tools lists at most") {
		t.Errorf("message = %q", msg)
	}
}
