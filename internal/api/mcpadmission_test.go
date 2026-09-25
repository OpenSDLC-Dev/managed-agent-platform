package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
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
// the message's advice could fix: both are left to the dial, which refuses them
// with reasons of its own.
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
