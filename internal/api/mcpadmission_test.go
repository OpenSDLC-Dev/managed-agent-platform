package api_test

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// mcpAgent creates an agent declaring one MCP server per {name, url} pair, in
// order, each with its toolset.
func mcpAgent(t *testing.T, s *tserver, servers ...[2]string) string {
	t.Helper()
	var decl, tools []any
	for _, sv := range servers {
		decl = append(decl, map[string]any{"type": "url", "name": sv[0], "url": sv[1]})
		tools = append(tools, mcpToolset(sv[0]))
	}
	return createAgent(t, s, map[string]any{
		"name": "mcp-agent", "model": "claude-opus-4-8", "mcp_servers": decl, "tools": tools,
	})["id"].(string)
}

func envWith(t *testing.T, s *tserver, config map[string]any) string {
	t.Helper()
	return createEnvironment(t, s, map[string]any{"name": "mcp-env", "config": config})["id"].(string)
}

func limited(allowed []string, allowMCP bool) map[string]any {
	return map[string]any{"type": "cloud", "networking": map[string]any{
		"type": "limited", "allowed_hosts": allowed, "allow_mcp_servers": allowMCP,
	}}
}

func sessionCount(t *testing.T, s *tserver) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	return n
}

const deepwiki = "https://mcp.deepwiki.com/mcp"

// A session whose agent declares an MCP server its environment's `limited`
// policy does not admit is refused at create, and never exists: the recorded
// 400, byte for byte (2026-09-03 batch2.json `sess.create.mcp-egress-blocked`,
// sent with initial_events as here).
func TestSessionCreateRefusesAnMCPHostItsEnvironmentBlocks(t *testing.T) {
	s := newTestServer(t)
	agentID := mcpAgent(t, s, [2]string{"deepwiki", deepwiki})
	envID := envWith(t, s, limited([]string{}, false))

	status, res := s.do(http.MethodPost, "/v1/sessions", map[string]any{
		"agent": agentID, "environment_id": envID,
		"initial_events": []any{map[string]any{"type": "user.message",
			"content": []any{map[string]any{"type": "text", "text": "hello"}}}},
	})
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	want := `MCP server host(s) blocked by environment network policy: "deepwiki" (mcp.deepwiki.com). ` +
		`Add these hosts to the environment's allowed_hosts, or set allow_mcp_servers=true.`
	if msg := res["error"].(map[string]any)["message"]; msg != want {
		t.Errorf("message = %q\nwant      %q", msg, want)
	}
	if n := sessionCount(t, s); n != 0 {
		t.Errorf("%d sessions exist after the refusal, want none", n)
	}
}

// A percent-encoded host is judged decoded, by its A-label: url.Parse decodes
// it, so the encoded Kelvin sign is admitted under an exact "k.example", and a
// refusal names the decoded host. The reference judged the spelling as written
// and refused the first (managed-agents-wire-recordings
// 2026-11-07-unicode-session-alternatives/setup.json idx 11 and 20). #1030
// tracks aligning; this pins today's answer, which docs/DIVERGENCES.md records.
func TestAPercentEncodedMCPHostIsJudgedDecoded(t *testing.T) {
	s := newTestServer(t)
	kelvin := mcpAgent(t, s, [2]string{"unicode", "https://%E2%84%AA.example/mcp"})
	createSession(t, s, map[string]any{"agent": kelvin,
		"environment_id": envWith(t, s, limited([]string{"k.example"}, false))})

	bucher := mcpAgent(t, s, [2]string{"unicode", "https://b%C3%BCcher.example/mcp"})
	createSession(t, s, map[string]any{"agent": bucher,
		"environment_id": envWith(t, s, limited([]string{"xn--bcher-kva.example"}, false))})
	status, res := s.do(http.MethodPost, "/v1/sessions", map[string]any{"agent": bucher,
		"environment_id": envWith(t, s, limited([]string{"other.example"}, false))})
	wantErrMsg(t, status, res, http.StatusBadRequest, "invalid_request_error",
		`MCP server host(s) blocked by environment network policy: "unicode" (b`+"\u00fc"+`cher.example). `+
			`Add these hosts to the environment's allowed_hosts, or set allow_mcp_servers=true.`)
}

// Both paths the message names admit the session, the flag still false — an
// exact host and a wildcard over it, as recorded (`sess.create.mcp-host-exact-gate`,
// `sess.create.mcp-host-wildcard-gate`) — and so does the flag itself. A policy
// with nothing to check is no gate: `unrestricted`, and `self_hosted`, whose
// config carries no networking at all (recorded admitted, FINDINGS2 §D).
func TestSessionCreateAdmitsAnMCPHostItsEnvironmentAdmits(t *testing.T) {
	for name, config := range map[string]map[string]any{
		"exact host":   limited([]string{"mcp.deepwiki.com"}, false),
		"wildcard":     limited([]string{"*.deepwiki.com"}, false),
		"the flag":     limited([]string{}, true),
		"unrestricted": {"type": "cloud", "networking": map[string]any{"type": "unrestricted"}},
		"self_hosted":  {"type": "self_hosted"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newTestServer(t)
			agentID := mcpAgent(t, s, [2]string{"deepwiki", deepwiki})
			createSession(t, s, map[string]any{"agent": agentID, "environment_id": envWith(t, s, config)})
		})
	}
}

// The wildcard admits a subdomain and nothing else — not its apex, not a
// lookalike domain — as recorded (`sess.create.wild-apex`,
// `sess.create.wild-other-domain`), and the matcher is the one allowed_hosts
// already uses. The message names only what was blocked, each server beside
// its host, in declaration order; how the reference joins several is unrecorded
// (docs/DIVERGENCES.md).
func TestSessionCreateNamesEveryBlockedMCPHost(t *testing.T) {
	for name, tc := range map[string]struct {
		servers [][2]string
		allowed []string
		blocked string
	}{
		"apex under a wildcard": {
			[][2]string{{"srv", "https://deepwiki.com/mcp"}}, []string{"*.deepwiki.com"},
			`"srv" (deepwiki.com)`,
		},
		"another domain": {
			[][2]string{{"srv", "https://mcp.notdeepwiki.com/mcp"}}, []string{"*.deepwiki.com"},
			`"srv" (mcp.notdeepwiki.com)`,
		},
		// The host is judged whatever the scheme: whether the dial can use it
		// is the dial's question, not the policy's.
		"a scheme the dial cannot use": {
			[][2]string{{"x", "wss://mcp.blocked.example/mcp"}}, []string{},
			`"x" (mcp.blocked.example)`,
		},
		"two of three": {
			[][2]string{{"a", "https://a.example/mcp"}, {"ok", "https://ok.example:8443/mcp"}, {"c", "https://c.example/mcp"}},
			[]string{"ok.example"},
			`"a" (a.example), "c" (c.example)`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := newTestServer(t)
			agentID := mcpAgent(t, s, tc.servers...)
			envID := envWith(t, s, limited(tc.allowed, false))

			status, res := s.do(http.MethodPost, "/v1/sessions", map[string]any{"agent": agentID, "environment_id": envID})
			wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
			want := "MCP server host(s) blocked by environment network policy: " + tc.blocked +
				". Add these hosts to the environment's allowed_hosts, or set allow_mcp_servers=true."
			if msg := res["error"].(map[string]any)["message"]; msg != want {
				t.Errorf("message = %q\nwant      %q", msg, want)
			}
		})
	}
}

// The check reads only what it judges by, and only when there is something to
// judge: a stored config whose packages will not decode — a tolerated corrupt
// row (TestEnvironmentPackagesTypeEchoSkipsACorruptRow) — still admits a
// session, with an MCP server or without one, and still refuses a blocked host
// under a `limited` block beside it. A url whose authority names no host is not
// a host the policy refuses, and a policy type nothing recognizes is not one
// the message's advice could fix: both are left to the executor, which refuses
// them before any dial, with reasons of its own.
func TestSessionCreateJudgesOnlyWhatItCanRead(t *testing.T) {
	s := newTestServer(t)
	storeConfig := func(envID, config string) {
		t.Helper()
		if _, err := s.pool.Exec(context.Background(), "UPDATE environments SET config = $2 WHERE id = $1",
			envID, []byte(config)); err != nil {
			t.Fatalf("store config: %v", err)
		}
	}
	mcp := mcpAgent(t, s, [2]string{"deepwiki", deepwiki})

	open := envWith(t, s, map[string]any{"type": "cloud", "networking": map[string]any{"type": "unrestricted"}})
	storeConfig(open, `{"type":"cloud","networking":{"type":"unrestricted"},"packages":"not-an-object"}`)
	plain, _ := fixture(t, s)
	createSession(t, s, map[string]any{"agent": plain, "environment_id": open})
	createSession(t, s, map[string]any{"agent": mcp, "environment_id": open})

	closed := envWith(t, s, limited([]string{}, false))
	storeConfig(closed, `{"type":"cloud","networking":{"type":"limited","allowed_hosts":[]},"packages":"not-an-object"}`)
	status, res := s.do(http.MethodPost, "/v1/sessions", map[string]any{"agent": mcp, "environment_id": closed})
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")

	hostless := mcpAgent(t, s, [2]string{"srv", "https://:443/mcp"})
	createSession(t, s, map[string]any{"agent": hostless, "environment_id": envWith(t, s, limited([]string{}, false))})

	unknown := envWith(t, s, limited([]string{}, false))
	storeConfig(unknown, `{"type":"cloud","networking":{"type":"open","allowed_hosts":["mcp.deepwiki.com"]}}`)
	createSession(t, s, map[string]any{"agent": mcp, "environment_id": unknown})
}

// A deployment fire goes through the same create, so a blocked host settles the
// run on the reference's own run-error type for it rather than rolling back.
func TestDeploymentRunRecordsABlockedMCPHost(t *testing.T) {
	s := newTestServer(t)
	agentID := mcpAgent(t, s, [2]string{"deepwiki", deepwiki})
	envID := envWith(t, s, limited([]string{}, false))
	deployment := createDeployment(t, s, deploymentBody(agentID, envID))["id"].(string)

	run := runDeployment(t, s, deployment)
	if run["session_id"] != nil {
		t.Errorf("session_id = %v, want null on failure", run["session_id"])
	}
	re, _ := run["error"].(map[string]any)
	if re["type"] != "mcp_egress_blocked_error" {
		t.Errorf("error.type = %v, want mcp_egress_blocked_error (message %v)", re["type"], re["message"])
	}
	if msg, _ := re["message"].(string); !strings.Contains(msg, `"deepwiki" (mcp.deepwiki.com)`) {
		t.Errorf("error.message = %q, want it to name the blocked server and host", msg)
	}
}

// A session update that names agent.mcp_servers meets the check create runs,
// and answers in the recorded words (2026-10-18-session-update-running
// `idle.agent_mcp_servers`, sent alone to an idle session on this policy; #1017).
// The list a patch sends is judged whole. A patch that names no server judges
// none: the recording sent mcp_servers alone, so that half is ours, and the dial
// still refuses (docs/DIVERGENCES.md, the session-update edges under INFERRED).
func TestSessionUpdateRefusesAnMCPHostItsEnvironmentBlocks(t *testing.T) {
	s := newTestServer(t)
	agentID, _ := fixture(t, s)
	server := map[string]any{"type": "url", "name": "rec32_context7", "url": "https://mcp.context7.com/mcp"}
	naming := map[string]any{"agent": map[string]any{"mcp_servers": []any{server}}}
	const blocked = `MCP server host(s) blocked by environment network policy: "rec32_context7" (mcp.context7.com). ` +
		`Add these hosts to the environment's allowed_hosts, or set allow_mcp_servers=true.`
	inStatus := func(envID string, status domain.SessionStatus) string {
		t.Helper()
		sid := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})["id"].(string)
		setSessionStatus(t, s, sid, string(status))
		return sid
	}
	refused := func(sid, msg string) {
		t.Helper()
		_, before := s.do(http.MethodGet, "/v1/sessions/"+sid, nil)
		status, res := s.do(http.MethodPost, "/v1/sessions/"+sid, naming)
		wantErrMsg(t, status, res, http.StatusBadRequest, "invalid_request_error", msg)
		_, after := s.do(http.MethodGet, "/v1/sessions/"+sid, nil)
		for _, k := range []string{"agent", "updated_at"} {
			if !reflect.DeepEqual(after[k], before[k]) {
				t.Errorf("%s after the refusal = %v, want %v", k, after[k], before[k])
			}
		}
	}

	envID := envWith(t, s, limited([]string{}, false))
	refused(inStatus(envID, domain.SessionIdle), blocked)
	refused(inStatus(envID, domain.SessionRescheduling), blocked)
	// The status refusals answer first (`running.agent_mcp_servers`).
	refused(inStatus(envID, domain.SessionRunning),
		"Cannot update agent while session is running. Send a `user.interrupt` event first.")
	refused(inStatus(envID, domain.SessionTerminated), "Cannot modify archived session")
	// The merged-result check answers ahead of it, in the order create runs them.
	status, res := s.do(http.MethodPost, "/v1/sessions/"+inStatus(envID, domain.SessionIdle),
		map[string]any{"agent": map[string]any{"mcp_servers": []any{server}, "tools": []any{mcpToolset("ghost")}}})
	wantErrMsg(t, status, res, http.StatusBadRequest, "invalid_request_error",
		`mcp_toolset references mcp_server "ghost", which is not declared in mcp_servers`)

	// A cleared list has no host to judge (`idle.mcp_empty`, on this same policy).
	if status, res := s.do(http.MethodPost, "/v1/sessions/"+inStatus(envID, domain.SessionIdle),
		map[string]any{"agent": map[string]any{"mcp_servers": []any{}}}); status != http.StatusOK {
		t.Errorf("clearing the servers: status %d (body %v)", status, res)
	}

	// Each path the message names admits the same patch, and so does no policy.
	for _, config := range []map[string]any{
		limited([]string{"mcp.context7.com"}, false),
		limited([]string{"*.context7.com"}, false),
		limited([]string{}, true),
		{"type": "cloud", "networking": map[string]any{"type": "unrestricted"}},
	} {
		sid := inStatus(envWith(t, s, config), domain.SessionIdle)
		if status, res := s.do(http.MethodPost, "/v1/sessions/"+sid, naming); status != http.StatusOK {
			t.Errorf("config %v: status %d (body %v)", config, status, res)
		}
	}

	// A policy that tightens after the server was stored. A patch to tools alone
	// names no server and lands. A patch that lists the stored server is judged.
	tightens := envWith(t, s, limited([]string{"mcp.context7.com"}, false))
	sid := createSession(t, s, map[string]any{"environment_id": tightens,
		"agent": map[string]any{"type": "agent_with_overrides", "id": agentID,
			"mcp_servers": []any{server}, "tools": []any{mcpToolset("rec32_context7")}}})["id"].(string)
	if status, res := s.do(http.MethodPost, "/v1/environments/"+tightens,
		map[string]any{"config": limited([]string{}, false)}); status != http.StatusOK {
		t.Fatalf("tighten the policy: status %d (body %v)", status, res)
	}
	if status, res := s.do(http.MethodPost, "/v1/sessions/"+sid,
		map[string]any{"agent": map[string]any{"tools": []any{}}}); status != http.StatusOK {
		t.Errorf("a patch to tools alone: status %d (body %v)", status, res)
	}
	refused(sid, blocked)
}
