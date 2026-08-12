package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/blobtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/local"
)

// sessionRequiredFields is the BetaManagedAgentsSession wire surface; all
// fields are api:"required" except deployment_id (nullable but present).
var sessionRequiredFields = []string{
	"id", "type", "agent", "environment_id", "status", "title", "metadata",
	"usage", "stats", "outcome_evaluations", "resources", "vault_ids",
	"deployment_id", "created_at", "updated_at", "archived_at",
}

// sessionAgentRequiredFields is the resolved-agent snapshot embedded in a
// session (BetaManagedAgentsSessionAgent) — all api:"required".
var sessionAgentRequiredFields = []string{
	"id", "type", "name", "version", "model", "system", "description",
	"tools", "mcp_servers", "skills", "multiagent",
}

// Slice 3: a session attaches vaults at create time (top-level vault_ids). The
// ids must name existing, unarchived vaults; the list round-trips on the
// response and on GET; update still rejects vault_ids changes (create-only).
func TestSessionVaultAttachment(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	vaultA := createVault(t, s, "attach-a")
	vaultB := createVault(t, s, "attach-b")

	res := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID, "vault_ids": []any{vaultA, vaultB}})
	got := res["vault_ids"].([]any)
	if len(got) != 2 || got[0] != vaultA || got[1] != vaultB {
		t.Fatalf("vault_ids not round-tripped in order: %v", got)
	}
	sid := res["id"].(string)
	_, fetched := s.do(http.MethodGet, "/v1/sessions/"+sid, nil)
	if got := fetched["vault_ids"].([]any); len(got) != 2 || got[0] != vaultA || got[1] != vaultB {
		t.Fatalf("GET did not echo vault_ids in order: %v", got)
	}

	// Empty/omitted vault_ids is fine and echoes an empty array.
	res2 := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})
	if len(res2["vault_ids"].([]any)) != 0 {
		t.Fatalf("omitted vault_ids should echo []: %v", res2["vault_ids"])
	}

	// An archived vault fails the create with the standard error envelope.
	s.do(http.MethodPost, "/v1/vaults/"+vaultB+"/archive", nil)
	status, body := s.do(http.MethodPost, "/v1/sessions", map[string]any{
		"agent": agentID, "environment_id": envID, "vault_ids": []any{vaultA, vaultB}})
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	// The unarchived vault alone still succeeds.
	createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID, "vault_ids": []any{vaultA}})
}

// A session's resolved agent answers to the same whole-spec caps as a stored
// agent (#66): an agent_with_overrides that swaps in an over-cap or
// inconsistent tools/mcp_servers set must reject at create, not become a
// session whose every turn the provider refuses (#287).
func TestSessionOverridesValidateAgentSpec(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	withOverrides := func(extra map[string]any) map[string]any {
		agent := map[string]any{"type": "agent_with_overrides", "id": agentID}
		for k, v := range extra {
			agent[k] = v
		}
		return map[string]any{"agent": agent, "environment_id": envID}
	}
	wantRejected := func(body map[string]any, frag string) {
		t.Helper()
		status, res := s.do(http.MethodPost, "/v1/sessions", body)
		wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
		inner, _ := res["error"].(map[string]any)
		if msg, _ := inner["message"].(string); !strings.Contains(msg, frag) {
			t.Errorf("error message %q does not mention %q", msg, frag)
		}
	}

	tools := make([]any, 0, 129)
	for i := 0; i < 129; i++ {
		tools = append(tools, customTool(fmt.Sprintf("t%03d", i)))
	}
	wantRejected(withOverrides(map[string]any{"tools": tools}), "128")
	createSession(t, s, withOverrides(map[string]any{"tools": tools[:128]}))

	servers := make([]any, 0, 21)
	toolsets := make([]any, 0, 21)
	for i := 0; i < 21; i++ {
		name := fmt.Sprintf("srv%02d", i)
		servers = append(servers, mcpServer(name))
		toolsets = append(toolsets, mcpToolset(name))
	}
	wantRejected(withOverrides(map[string]any{
		"mcp_servers": servers, "tools": toolsets}), "20")
	wantRejected(withOverrides(map[string]any{
		"mcp_servers": []any{mcpServer("srv"), mcpServer("srv")},
		"tools":       []any{mcpToolset("srv")}}), "srv")
	wantRejected(withOverrides(map[string]any{
		"mcp_servers": []any{mcpServer("srv")}}), "srv")
	wantRejected(withOverrides(map[string]any{
		"tools": []any{customTool("dup"), customTool("dup")}}), "dup")

	// The check runs on every resolve, not only when overrides are present: a
	// stored spec that predates #66's enforcement can violate the caps, and a
	// plain reference to it fails at session create, not on every turn.
	pre := createAgent(t, s, map[string]any{
		"name": "pre-66", "model": "claude-opus-4-8", "system": "base system"})["id"].(string)
	planted, err := json.Marshal([]any{mcpServer("srv")})
	if err != nil {
		t.Fatalf("marshal servers: %v", err)
	}
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE agents SET spec = jsonb_set(spec, '{mcp_servers}', $1::jsonb) WHERE id = $2`,
		string(planted), pre); err != nil {
		t.Fatalf("plant pre-validation spec: %v", err)
	}
	wantRejected(map[string]any{"agent": pre, "environment_id": envID}, "srv")
}

// Session update validates the resulting resolved agent, exactly as agent
// update validates the merged spec: a patch that strands a stored mcp_server
// or grows tools past the cap rejects, one that keeps the spec consistent
// lands (#287).
func TestSessionUpdateValidatesResultingAgent(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	created := createSession(t, s, map[string]any{
		"agent": map[string]any{"type": "agent_with_overrides", "id": agentID,
			"mcp_servers": []any{mcpServer("srv")},
			"tools":       []any{mcpToolset("srv")}},
		"environment_id": envID,
	})
	sid := created["id"].(string)

	// Clearing tools alone strands the stored server.
	status, res := s.do(http.MethodPost, "/v1/sessions/"+sid,
		map[string]any{"agent": map[string]any{"tools": []any{}}})
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")

	tools := make([]any, 0, 129)
	for i := 0; i < 129; i++ {
		tools = append(tools, customTool(fmt.Sprintf("t%03d", i)))
	}
	status, res = s.do(http.MethodPost, "/v1/sessions/"+sid, map[string]any{
		"agent": map[string]any{"tools": tools, "mcp_servers": []any{}}})
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")

	// The rejected updates left the stored snapshot intact — compared whole,
	// so a partial write (tools cleared, server kept) cannot pass.
	status, got := s.do(http.MethodGet, "/v1/sessions/"+sid, nil)
	if status != http.StatusOK {
		t.Fatalf("get after rejected updates: %d", status)
	}
	if !reflect.DeepEqual(got["agent"], created["agent"]) {
		t.Errorf("agent snapshot changed across rejected updates:\n got  %v\nwant %v",
			got["agent"], created["agent"])
	}

	// Clearing both halves together leaves nothing stranded.
	if status, res := s.do(http.MethodPost, "/v1/sessions/"+sid, map[string]any{
		"agent": map[string]any{"tools": []any{}, "mcp_servers": []any{}}}); status != http.StatusOK {
		t.Fatalf("clearing both: status %d (body %v)", status, res)
	}
}

// The SDK documents session metadata with the same sentence as agents and
// vaults — 16 pairs, 64-char keys, 512-char values (betasession.go) — so
// sessions run the same shared check, counted in runes like the others (#289).
func TestSessionMetadataCaps(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	base := func(md map[string]any) map[string]any {
		return map[string]any{"agent": agentID, "environment_id": envID, "metadata": md}
	}
	wantRejected := func(md map[string]any, frag string) {
		t.Helper()
		status, res := s.do(http.MethodPost, "/v1/sessions", base(md))
		wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
		inner, _ := res["error"].(map[string]any)
		if msg, _ := inner["message"].(string); !strings.Contains(msg, frag) {
			t.Errorf("error message %q does not mention %q", msg, frag)
		}
	}

	over := map[string]any{}
	for i := 0; i < 17; i++ {
		over[fmt.Sprintf("k%02d", i)] = "v"
	}
	wantRejected(over, "16")
	wantRejected(map[string]any{strings.Repeat("键", 65): "v"}, "64")
	wantRejected(map[string]any{"k": strings.Repeat("值", 513)}, "512")

	// The documented boundary is accepted, counted in runes: 16 pairs, one
	// carrying a 64-rune CJK key and a 512-rune CJK value.
	atCap := map[string]any{strings.Repeat("键", 64): strings.Repeat("值", 512)}
	for i := 0; i < 15; i++ {
		atCap[fmt.Sprintf("k%02d", i)] = "v"
	}
	sid := createSession(t, s, base(atCap))["id"].(string)

	// Update binds the resulting stored bag, exactly as agents and vaults: a
	// 17th key rejects, an upsert and a delete-one-add-one stay within it.
	status, res := s.do(http.MethodPost, "/v1/sessions/"+sid,
		map[string]any{"metadata": map[string]any{"k16": "v"}})
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	if status, res := s.do(http.MethodPost, "/v1/sessions/"+sid,
		map[string]any{"metadata": map[string]any{"k00": "changed"}}); status != http.StatusOK {
		t.Fatalf("upsert within cap: status %d (body %v)", status, res)
	}
	if status, res := s.do(http.MethodPost, "/v1/sessions/"+sid,
		map[string]any{"metadata": map[string]any{"k01": nil, "k16": "v"}}); status != http.StatusOK {
		t.Fatalf("delete-and-add within cap: status %d (body %v)", status, res)
	}
}

// The SDK bounds an agent_with_overrides replacement system prompt at 100,000
// characters (betasession.go) — a bound specific to the session override; the
// stored agent's own system documents none. Counted in runes, the
// filesupload.go precedent for character-documented limits (#291).
func TestSessionOverrideSystemCap(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	withSystem := func(sys string) map[string]any {
		return map[string]any{
			"agent":          map[string]any{"type": "agent_with_overrides", "id": agentID, "system": sys},
			"environment_id": envID,
		}
	}
	wantRejected := func(sys string) {
		t.Helper()
		status, res := s.do(http.MethodPost, "/v1/sessions", withSystem(sys))
		wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
		inner, _ := res["error"].(map[string]any)
		if msg, _ := inner["message"].(string); !strings.Contains(msg, "100000") {
			t.Errorf("error message %q does not name the limit", msg)
		}
	}

	// The documented boundary is accepted; one rune past it rejects.
	createSession(t, s, withSystem(strings.Repeat("a", 100_000)))
	wantRejected(strings.Repeat("a", 100_001))

	// 100,000 CJK runes are 300,000 bytes — counted as runes, accepted; one
	// more rune rejects.
	createSession(t, s, withSystem(strings.Repeat("界", 100_000)))
	wantRejected(strings.Repeat("界", 100_001))

	// The bound binds the override only: the stored agent's own system has no
	// documented ceiling, so an over-cap stored system still resolves — via a
	// plain reference and via an override that omits system (omit preserves).
	big := createAgent(t, s, map[string]any{"name": "big-sys",
		"model": "claude-opus-4-8", "system": strings.Repeat("b", 100_001)})["id"].(string)
	createSession(t, s, map[string]any{"agent": big, "environment_id": envID})
	createSession(t, s, map[string]any{
		"agent":          map[string]any{"type": "agent_with_overrides", "id": big},
		"environment_id": envID,
	})
	// system:null (clear) is never counted, even over an over-cap stored system.
	createSession(t, s, map[string]any{
		"agent":          map[string]any{"type": "agent_with_overrides", "id": big, "system": nil},
		"environment_id": envID,
	})
}

// fixture creates an agent and an environment and returns their ids.
func fixture(t *testing.T, s *tserver) (agentID, envID string) {
	t.Helper()
	a := createAgent(t, s, map[string]any{"name": "task-agent", "model": "claude-opus-4-8", "system": "base system"})
	e := createEnvironment(t, s, map[string]any{"name": "task-env"})
	return a["id"].(string), e["id"].(string)
}

func createSession(t *testing.T, s *tserver, body map[string]any) map[string]any {
	t.Helper()
	status, res := s.do(http.MethodPost, "/v1/sessions", body)
	if status != http.StatusOK {
		t.Fatalf("create session: status %d, body %v", status, res)
	}
	return res
}

// The unknown-field rejection covers every API path that accepts a tools array,
// not just agent create: a misspelled permission_policy in a session's inline
// agent_with_overrides tools, or in a session-update agent.tools patch, is a 400
// before the malformed toolset is stored on the session snapshot (issue #26).
func TestSessionToolsetRejectsUnknownField(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	badTools := []any{map[string]any{
		"type":           "agent_toolset_20260401",
		"default_config": map[string]any{"permission_polciy": map[string]any{"type": "always_ask"}},
	}}

	// Session create, via agent_with_overrides.tools.
	status, body := s.do(http.MethodPost, "/v1/sessions", map[string]any{
		"agent":          map[string]any{"type": "agent_with_overrides", "id": agentID, "tools": badTools},
		"environment_id": envID,
	})
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	// The rejected session was not stored.
	_, list := s.do(http.MethodGet, "/v1/sessions", nil)
	if entries := listData(t, list); len(entries) != 0 {
		t.Errorf("sessions after rejected create = %d, want 0 (not persisted)", len(entries))
	}

	// Session update, via the agent.tools patch on an existing session.
	id := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})["id"].(string)
	status, body = s.do(http.MethodPost, "/v1/sessions/"+id, map[string]any{
		"agent": map[string]any{"tools": badTools},
	})
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
}

func TestSessionCreateWithAgentString(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	res := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})

	wantFields(t, res, sessionRequiredFields...)
	id, _ := res["id"].(string)
	if !strings.HasPrefix(id, "sesn_") {
		t.Errorf("id = %q, want sesn_ prefix", id)
	}
	if res["type"] != "session" || res["status"] != "idle" {
		t.Errorf("type/status = %v/%v, want session/idle", res["type"], res["status"])
	}
	if res["environment_id"] != envID {
		t.Errorf("environment_id = %v", res["environment_id"])
	}
	if res["title"] != "" {
		t.Errorf("title = %v, want empty string", res["title"])
	}

	agent, _ := res["agent"].(map[string]any)
	wantFields(t, agent, sessionAgentRequiredFields...)
	if agent["id"] != agentID || agent["version"] != float64(1) || agent["type"] != "agent" {
		t.Errorf("agent snapshot = %v", agent)
	}
	if agent["system"] != "base system" || agent["name"] != "task-agent" {
		t.Errorf("agent snapshot content = %v", agent)
	}

	usage, _ := res["usage"].(map[string]any)
	wantFields(t, usage, "input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation")
	cc, _ := usage["cache_creation"].(map[string]any)
	wantFields(t, cc, "ephemeral_1h_input_tokens", "ephemeral_5m_input_tokens")
	if usage["input_tokens"] != float64(0) {
		t.Errorf("usage.input_tokens = %v, want 0", usage["input_tokens"])
	}
	stats, _ := res["stats"].(map[string]any)
	wantFields(t, stats, "active_seconds", "duration_seconds")
	for _, k := range []string{"outcome_evaluations", "resources", "vault_ids"} {
		if arr, ok := res[k].([]any); !ok || len(arr) != 0 {
			t.Errorf("%s = %v, want []", k, res[k])
		}
	}
	if res["deployment_id"] != nil || res["archived_at"] != nil {
		t.Errorf("deployment_id/archived_at = %v/%v, want null/null", res["deployment_id"], res["archived_at"])
	}
	for _, k := range []string{"created_at", "updated_at"} {
		ts, _ := res[k].(string)
		if _, err := time.Parse(time.RFC3339, ts); err != nil {
			t.Errorf("%s = %q not RFC3339: %v", k, ts, err)
		}
		if !strings.HasSuffix(ts, "Z") {
			t.Errorf("%s = %q must be UTC (Z suffix), not a local offset", k, ts)
		}
	}
}

func TestSessionCreatePinsAgentVersionAndSupportsOverrides(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	// Bump the agent so v1 and v2 differ.
	status, _ := s.do(http.MethodPost, "/v1/agents/"+agentID, map[string]any{"version": 1, "system": "v2 system"})
	if status != http.StatusOK {
		t.Fatalf("agent update: %d", status)
	}

	// Plain string pins the latest version.
	latest := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})
	if a, _ := latest["agent"].(map[string]any); a["version"] != float64(2) || a["system"] != "v2 system" {
		t.Errorf("latest snapshot = %v", latest["agent"])
	}

	// {type:"agent", version:1} pins the old snapshot.
	pinned := createSession(t, s, map[string]any{
		"agent":          map[string]any{"type": "agent", "id": agentID, "version": 1},
		"environment_id": envID,
	})
	if a, _ := pinned["agent"].(map[string]any); a["version"] != float64(1) || a["system"] != "base system" {
		t.Errorf("pinned snapshot = %v", pinned["agent"])
	}

	// agent_with_overrides overlays fields; id/version still reference the base.
	tools := []any{map[string]any{"type": "agent_toolset_20260401"}, customTool("mine")}
	over := createSession(t, s, map[string]any{
		"agent": map[string]any{
			"type": "agent_with_overrides", "id": agentID,
			"system": "override system",
			"model":  map[string]any{"id": "claude-haiku-4-5"},
			"tools":  tools,
		},
		"environment_id": envID,
	})
	a, _ := over["agent"].(map[string]any)
	if a["system"] != "override system" || a["id"] != agentID || a["version"] != float64(2) {
		t.Errorf("override snapshot = %v", a)
	}
	if m, _ := a["model"].(map[string]any); m["id"] != "claude-haiku-4-5" {
		t.Errorf("override model = %v", a["model"])
	}
	// The override replaced the tool list. Materialization adds resolved config
	// to the toolset entry (pinned by TestToolsetConfigsEchoResolved), so this
	// cannot deep-compare the whole list any more — but every field an entry
	// identifies itself by has to survive the render, so those are asserted.
	if got := toolTypes(t, a["tools"]); !reflect.DeepEqual(got, []string{"agent_toolset_20260401", "custom"}) {
		t.Errorf("override tools = %v", a["tools"])
	}
	if custom, _ := a["tools"].([]any)[1].(map[string]any); custom["name"] != "mine" ||
		custom["description"] != "d" || custom["input_schema"] == nil {
		t.Errorf("custom tool lost its own fields in the override snapshot: %v", custom)
	}
	// The base agent resource is untouched by overrides.
	_, base := s.do(http.MethodGet, "/v1/agents/"+agentID, nil)
	if base["system"] != "v2 system" {
		t.Errorf("base agent mutated by overrides: %v", base["system"])
	}

	// A skills override is capped at 500 like the agent's own list.
	many := make([]any, 501)
	for i := range many {
		many[i] = map[string]any{"type": "custom", "skill_id": fmt.Sprintf("skill_%022d", i)}
	}
	status, obj := s.do(http.MethodPost, "/v1/sessions", map[string]any{
		"agent":          map[string]any{"type": "agent_with_overrides", "id": agentID, "skills": many},
		"environment_id": envID,
	})
	wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")

	// system:null explicitly clears the system prompt (SDK-documented).
	cleared := createSession(t, s, map[string]any{
		"agent":          map[string]any{"type": "agent_with_overrides", "id": agentID, "system": nil},
		"environment_id": envID,
	})
	if a, _ := cleared["agent"].(map[string]any); a["system"] != "" {
		t.Errorf("system:null override should clear, got %v", a["system"])
	}
}

func TestSessionCreateValidation(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)

	// Archived referents are rejected.
	archAgent := createAgent(t, s, map[string]any{"name": "aa", "model": "m"})
	s.do(http.MethodPost, "/v1/agents/"+archAgent["id"].(string)+"/archive", nil)
	archEnv := createEnvironment(t, s, map[string]any{"name": "ae"})
	s.do(http.MethodPost, "/v1/environments/"+archEnv["id"].(string)+"/archive", nil)

	for name, tc := range map[string]struct {
		body       any
		wantStatus int
		wantType   string
	}{
		"missing agent":        {map[string]any{"environment_id": envID}, 400, "invalid_request_error"},
		"missing environment":  {map[string]any{"agent": agentID}, 400, "invalid_request_error"},
		"unknown agent":        {map[string]any{"agent": "agent_missing", "environment_id": envID}, 404, "not_found_error"},
		"unknown agent object": {map[string]any{"agent": map[string]any{"type": "agent", "id": "agent_missing"}, "environment_id": envID}, 404, "not_found_error"},
		"unknown version":      {map[string]any{"agent": map[string]any{"type": "agent", "id": agentID, "version": 99}, "environment_id": envID}, 404, "not_found_error"},
		"bad agent union type": {map[string]any{"agent": map[string]any{"type": "wizard", "id": agentID}, "environment_id": envID}, 400, "invalid_request_error"},
		"unknown environment":  {map[string]any{"agent": agentID, "environment_id": "env_missing"}, 404, "not_found_error"},
		"archived agent":       {map[string]any{"agent": archAgent["id"], "environment_id": envID}, 400, "invalid_request_error"},
		"archived environment": {map[string]any{"agent": agentID, "environment_id": archEnv["id"]}, 400, "invalid_request_error"},
		"github resource missing token": {map[string]any{"agent": agentID, "environment_id": envID,
			"resources": []any{map[string]any{"type": "github_repository", "url": "https://github.com/x/y"}}}, 400, "invalid_request_error"},
		"memory resources unsupported": {map[string]any{"agent": agentID, "environment_id": envID,
			"resources": []any{map[string]any{"type": "memory_store", "memory_store_id": "mem_x"}}}, 400, "invalid_request_error"},
		"unknown vault": {map[string]any{"agent": agentID, "environment_id": envID,
			"vault_ids": []any{"vlt_missing0000000000000000"}}, 400, "invalid_request_error"},
		"malformed vault id": {map[string]any{"agent": agentID, "environment_id": envID,
			"vault_ids": []any{"env_wrongprefix"}}, 400, "invalid_request_error"},
		"malformed json": {`{"agent": `, 400, "invalid_request_error"},
	} {
		status, body := s.do(http.MethodPost, "/v1/sessions", tc.body)
		if status != tc.wantStatus {
			t.Errorf("%s: status %d, want %d (%v)", name, status, tc.wantStatus, body)
			continue
		}
		wantErr(t, status, body, tc.wantStatus, tc.wantType)
	}
}

func TestSessionGetAcceptsAltSessionPrefix(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	created := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID, "title": "alt"})
	id := created["id"].(string)

	// The wire accepts session_… as an alternate spelling of sesn_….
	alt := "session_" + strings.TrimPrefix(id, "sesn_")
	status, got := s.do(http.MethodGet, "/v1/sessions/"+alt, nil)
	if status != http.StatusOK || got["id"] != id {
		t.Fatalf("get with session_ prefix: %d %v", status, got)
	}

	status, body := s.do(http.MethodGet, "/v1/sessions/sesn_missing", nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
}

func TestSessionUpdate(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	created := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"metadata": map[string]any{"keep": "1", "drop": "2"},
	})
	id := created["id"].(string)

	// The patched pair must satisfy validateAgentSpec (#287): the server needs
	// a referencing mcp_toolset in the resulting tools.
	tools := []any{
		map[string]any{"type": "agent_toolset_20260401"},
		map[string]any{"type": "mcp_toolset", "mcp_server_name": "docs"},
	}
	mcp := []any{map[string]any{"type": "url", "name": "docs", "url": "https://mcp.example.com"}}
	status, updated := s.do(http.MethodPost, "/v1/sessions/"+id, map[string]any{
		"title":    "titled",
		"metadata": map[string]any{"drop": nil, "new": "3"},
		"agent":    map[string]any{"tools": tools, "mcp_servers": mcp},
	})
	if status != http.StatusOK {
		t.Fatalf("update: %d %v", status, updated)
	}
	if updated["title"] != "titled" {
		t.Errorf("title = %v", updated["title"])
	}
	if md, _ := updated["metadata"].(map[string]any); !reflect.DeepEqual(md, map[string]any{"keep": "1", "new": "3"}) {
		t.Errorf("metadata = %v", updated["metadata"])
	}
	a, _ := updated["agent"].(map[string]any)
	if got := toolTypes(t, a["tools"]); !reflect.DeepEqual(got,
		[]string{"agent_toolset_20260401", "mcp_toolset"}) {
		t.Errorf("agent.tools = %v", a["tools"])
	}
	// mcp_server_name is the field the brain's MCP expansion reads to pick a
	// server, and materialization rebuilds the entry around it, so the patched
	// snapshot is asserted to still carry it rather than only its type.
	if mcp, _ := a["tools"].([]any)[1].(map[string]any); mcp["mcp_server_name"] != "docs" {
		t.Errorf("patched mcp_toolset lost mcp_server_name: %v", mcp)
	}
	if !reflect.DeepEqual(jsonNorm(t, a["mcp_servers"]), jsonNorm(t, mcp)) {
		t.Errorf("agent.mcp_servers = %v", a["mcp_servers"])
	}
	// The rest of the snapshot is untouched.
	if a["system"] != "base system" || a["version"] != float64(1) {
		t.Errorf("snapshot fields changed: %v", a)
	}

	// vault_ids on update matches the reference server: not yet supported. Any
	// presence is rejected — an array or an explicit null.
	status, body := s.do(http.MethodPost, "/v1/sessions/"+id, map[string]any{"vault_ids": []any{"vlt_x"}})
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	status, body = s.do(http.MethodPost, "/v1/sessions/"+id, map[string]any{"vault_ids": nil})
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")

	// Only tools/mcp_servers are updatable inside agent.
	status, body = s.do(http.MethodPost, "/v1/sessions/"+id, map[string]any{"agent": map[string]any{"system": "sneaky"}})
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")

	status, body = s.do(http.MethodPost, "/v1/sessions/sesn_missing", map[string]any{"title": "x"})
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
}

func TestSessionListFiltersAndBidirectionalPagination(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	other := createAgent(t, s, map[string]any{"name": "other", "model": "m"})
	otherID := other["id"].(string)

	var ids []string
	for i := 0; i < 3; i++ {
		res := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})
		ids = append(ids, res["id"].(string))
	}
	createSession(t, s, map[string]any{"agent": otherID, "environment_id": envID})

	// agent_id filter.
	status, list := s.do(http.MethodGet, "/v1/sessions?agent_id="+agentID, nil)
	if status != http.StatusOK || len(listData(t, list)) != 3 {
		t.Errorf("agent_id filter: %d %v", status, list)
	}
	// agent_version filter (all fixture sessions pin version 1).
	status, list = s.do(http.MethodGet, "/v1/sessions?agent_id="+agentID+"&agent_version=2", nil)
	if status != http.StatusOK || len(listData(t, list)) != 0 {
		t.Errorf("agent_version filter: %d %v", status, list)
	}
	// statuses[] filter, bracket style as the SDK sends it.
	status, list = s.do(http.MethodGet, "/v1/sessions?statuses[]=idle&statuses[]=running", nil)
	if status != http.StatusOK || len(listData(t, list)) != 4 {
		t.Errorf("statuses filter: %d %v", status, list)
	}
	status, list = s.do(http.MethodGet, "/v1/sessions?statuses[]=terminated", nil)
	if status != http.StatusOK || len(listData(t, list)) != 0 {
		t.Errorf("terminated filter: %d %v", status, list)
	}
	status, body := s.do(http.MethodGet, "/v1/sessions?statuses[]=zombie", nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")

	// deployment_id / memory_store_id reference features we don't host: no
	// session can match, so the result is empty rather than an error.
	for _, q := range []string{"deployment_id=depl_x", "memory_store_id=memstore_x"} {
		status, list = s.do(http.MethodGet, "/v1/sessions?"+q, nil)
		if status != http.StatusOK || len(listData(t, list)) != 0 {
			t.Errorf("%s: %d %v", q, status, list)
		}
	}

	// Bidirectional pagination, default order desc.
	status, page1 := s.do(http.MethodGet, "/v1/sessions?agent_id="+agentID+"&limit=2", nil)
	if status != http.StatusOK {
		t.Fatalf("page 1: %d", status)
	}
	d1 := listData(t, page1)
	if len(d1) != 2 || d1[0]["id"] != ids[2] || d1[1]["id"] != ids[1] {
		t.Errorf("page 1 = %v, want newest first", d1)
	}
	if v, ok := page1["prev_page"]; !ok || v != nil {
		t.Errorf("page 1 prev_page = %v (present %v), want null", v, ok)
	}
	status, page2 := s.do(http.MethodGet, "/v1/sessions?agent_id="+agentID+"&limit=2&page="+nextPage(t, page1), nil)
	if status != http.StatusOK {
		t.Fatalf("page 2: %d", status)
	}
	d2 := listData(t, page2)
	if len(d2) != 1 || d2[0]["id"] != ids[0] {
		t.Errorf("page 2 = %v", d2)
	}
	if prev, _ := page2["prev_page"].(string); prev == "" {
		t.Errorf("page 2 prev_page = %v, want a cursor", page2["prev_page"])
	}

	// Following prev_page from page 2 returns exactly page 1 again.
	prev, _ := page2["prev_page"].(string)
	status, back := s.do(http.MethodGet, "/v1/sessions?agent_id="+agentID+"&limit=2&page="+prev, nil)
	if status != http.StatusOK {
		t.Fatalf("prev page: %d", status)
	}
	db := listData(t, back)
	if len(db) != 2 || db[0]["id"] != ids[2] || db[1]["id"] != ids[1] {
		t.Errorf("prev page = %v, want page 1 content", db)
	}
	// Walking backwards from page 2 there is nothing before page 1, and the
	// forward cursor must lead back to page 2.
	if v, ok := back["prev_page"]; !ok || v != nil {
		t.Errorf("prev_page of first page = %v (present %v), want null", v, ok)
	}
	next2, _ := back["next_page"].(string)
	if next2 == "" {
		t.Fatalf("next_page after backwards walk missing: %v", back)
	}
	status, fwd := s.do(http.MethodGet, "/v1/sessions?agent_id="+agentID+"&limit=2&page="+next2, nil)
	if status != http.StatusOK || listData(t, fwd)[0]["id"] != ids[0] {
		t.Errorf("forward after backwards walk: %d %v", status, fwd)
	}

	// order=asc flips it.
	status, list = s.do(http.MethodGet, "/v1/sessions?agent_id="+agentID+"&order=asc&limit=1", nil)
	if status != http.StatusOK || listData(t, list)[0]["id"] != ids[0] {
		t.Errorf("order=asc: %d %v", status, list)
	}
	status, body = s.do(http.MethodGet, "/v1/sessions?order=sideways", nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
}

func TestSessionArchiveAndDelete(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	sess := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})
	id := sess["id"].(string)

	status, archived := s.do(http.MethodPost, "/v1/sessions/"+id+"/archive", nil)
	if status != http.StatusOK {
		t.Fatalf("archive: %d %v", status, archived)
	}
	if ts, _ := archived["archived_at"].(string); ts == "" {
		t.Fatalf("archived_at = %v", archived["archived_at"])
	}
	_, list := s.do(http.MethodGet, "/v1/sessions", nil)
	if entries := listData(t, list); len(entries) != 0 {
		t.Errorf("default list shows archived session: %v", entries)
	}
	_, list = s.do(http.MethodGet, "/v1/sessions?include_archived=true", nil)
	if entries := listData(t, list); len(entries) != 1 {
		t.Errorf("include_archived = %v", entries)
	}

	status, deleted := s.do(http.MethodDelete, "/v1/sessions/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("delete: %d %v", status, deleted)
	}
	if deleted["id"] != id || deleted["type"] != "session_deleted" {
		t.Errorf("delete response = %v", deleted)
	}
	status, body := s.do(http.MethodGet, "/v1/sessions/"+id, nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
	status, body = s.do(http.MethodDelete, "/v1/sessions/sesn_missing", nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
}

// Plan 24 slice 1: the reference documents that a running session cannot be
// archived or deleted (an interrupt must land first). The reject status and
// message are ours — INFERRED in docs/DIVERGENCES.md.
// A deleted session's workspace checkpoint goes with the record (best-effort;
// the reaper's deleted tier is the other remover — this path covers a session
// whose sandbox is already gone, which no reap pass will visit again).
func TestDeleteSessionRemovesCheckpointBlob(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	sess := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})
	id := sess["id"].(string)
	ctx := context.Background()
	key := blob.SessionCheckpointKey(id)
	if err := s.blobs.Put(ctx, key, strings.NewReader("tar"), 3, "application/gzip"); err != nil {
		t.Fatalf("put checkpoint: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO session_checkpoints (session_id, blob_key, state) VALUES ($1, $2, 'ready')`,
		id, key); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	if status, body := s.do(http.MethodDelete, "/v1/sessions/"+id, nil); status != http.StatusOK {
		t.Fatalf("delete: %d %v", status, body)
	}
	if _, _, err := s.blobs.Get(ctx, key); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("checkpoint after delete: %v, want ErrNotFound", err)
	}
	// The marker row dies inside the deleting transaction — the reaper can
	// never own it, because a session whose sandbox was already idle-reaped
	// never reappears in Owned (found by plan 24's acceptance run).
	var markers int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM session_checkpoints WHERE session_id = $1`, id).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if markers != 0 {
		t.Error("the deleting transaction left the checkpoint marker row")
	}
	// The tombstone rides the deleting transaction — it is the reaper's
	// deleted-tier evidence, and its recorded kind is what keeps the tier
	// cloud-only (plan 24).
	var deadKind string
	if err := s.pool.QueryRow(ctx,
		`SELECT environment_kind FROM deleted_sessions WHERE id = $1`, id).Scan(&deadKind); err != nil {
		t.Fatalf("deleted session left no tombstone: %v", err)
	}
	if deadKind != "cloud" {
		t.Errorf("tombstone environment_kind = %q, want cloud", deadKind)
	}
}

// failingDeleteBlobStore fails every Delete; the rest delegates.
type failingDeleteBlobStore struct{ blob.Store }

func (f failingDeleteBlobStore) Delete(context.Context, string) error {
	return errors.New("storage down")
}

// TestDeleteSessionSurvivesCheckpointDeleteFailure: the checkpoint delete is
// post-commit and best-effort — a failing object store must not change the
// delete's response, and the row (with its tombstone) is already gone, so the
// reaper's deleted tier remains the retrying remover.
func TestDeleteSessionSurvivesCheckpointDeleteFailure(t *testing.T) {
	cipher, err := local.New(local.Config{KeyID: "test-1", Key: bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	pool := newPoolWithKey(t)
	srv := httptest.NewServer(api.NewHandler(pool, failingDeleteBlobStore{Store: blobtest.Mem()}, cipher, nil))
	t.Cleanup(srv.Close)
	s := &tserver{t: t, url: srv.URL, pool: pool}

	agentID, envID := fixture(t, s)
	sess := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})
	id := sess["id"].(string)

	status, body := s.do(http.MethodDelete, "/v1/sessions/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("delete with a failing blob store: %d %v", status, body)
	}
	if body["id"] != id || body["type"] != "session_deleted" {
		t.Errorf("delete response = %v, want {id: %s, type: session_deleted}", body, id)
	}
}

func TestRunningSessionArchiveAndDeleteRejected(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	sess := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})
	id := sess["id"].(string)

	if _, err := s.pool.Exec(context.Background(),
		`UPDATE sessions SET status = 'running' WHERE id = $1`, id); err != nil {
		t.Fatalf("set running: %v", err)
	}

	status, body := s.do(http.MethodPost, "/v1/sessions/"+id+"/archive", nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	status, body = s.do(http.MethodDelete, "/v1/sessions/"+id, nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")

	// Neither refusal mutated the session: still listed, still unarchived.
	status, got := s.do(http.MethodGet, "/v1/sessions/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("get after refusals: %d %v", status, got)
	}
	if got["archived_at"] != nil {
		t.Errorf("archived_at after refused archive = %v, want null", got["archived_at"])
	}

	// Once the session settles (the interrupt path's end state), both succeed.
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE sessions SET status = 'idle' WHERE id = $1`, id); err != nil {
		t.Fatalf("set idle: %v", err)
	}
	status, body = s.do(http.MethodPost, "/v1/sessions/"+id+"/archive", nil)
	if status != http.StatusOK {
		t.Fatalf("archive after idle: %d %v", status, body)
	}
	status, body = s.do(http.MethodDelete, "/v1/sessions/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("delete after idle: %d %v", status, body)
	}

	// The refusal is running-only — the registry's positive claim that
	// rescheduling (an auto-retrying session) does not refuse is load-bearing:
	// a guard tightened to "must be idle" would strand such a session's
	// archive behind its transient-error loop.
	resched := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})
	rid := resched["id"].(string)
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE sessions SET status = 'rescheduling' WHERE id = $1`, rid); err != nil {
		t.Fatalf("set rescheduling: %v", err)
	}
	status, body = s.do(http.MethodPost, "/v1/sessions/"+rid+"/archive", nil)
	if status != http.StatusOK {
		t.Fatalf("archive while rescheduling: %d %v", status, body)
	}
	status, body = s.do(http.MethodDelete, "/v1/sessions/"+rid, nil)
	if status != http.StatusOK {
		t.Fatalf("delete while rescheduling: %d %v", status, body)
	}
}

// The guard's row lock is what closes the check-then-act window: a flip to
// running committed while the archive is in flight must be observed, not
// raced past. The concurrent writer here holds the session row exclusively
// (as a confirmation dispatch flipping the session to running would), commits
// after the archive has reached the guard, and the archive must answer 400 —
// without FOR UPDATE the guard's plain SELECT reads the pre-flip snapshot,
// sails past, and the archive lands on a running session.
func TestArchiveObservesConcurrentRunningFlip(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	sess := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})
	id := sess["id"].(string)
	ctx := context.Background()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE sessions SET status = 'running' WHERE id = $1`, id); err != nil {
		t.Fatalf("flip to running: %v", err)
	}

	// The goroutine speaks plain net/http: tserver.do fatals on transport
	// errors, and t.Fatalf must not run outside the test goroutine — an
	// error is sent back instead, so the receives below can never hang.
	type resp struct {
		status int
		body   map[string]any
		err    error
	}
	ch := make(chan resp, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, s.url+"/v1/sessions/"+id+"/archive", nil)
		if err != nil {
			ch <- resp{err: err}
			return
		}
		req.Header.Set("x-api-key", testKey)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			ch <- resp{err: err}
			return
		}
		defer res.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			ch <- resp{err: err}
			return
		}
		ch <- resp{status: res.StatusCode, body: body}
	}()

	// Commit only once the guard's SELECT … FOR UPDATE is observably
	// waiting on the held row lock. A fixed sleep would leave a scheduling
	// window in which a lockless guard reads the post-commit row and
	// answers 400 for the wrong reason; a guard without FOR UPDATE never
	// waits at its SELECT, so this poll timing out is exactly how that
	// mutant fails. (The poll's own query never matches: its
	// wait_event_type is null, not Lock.)
	waitSQL := `SELECT EXISTS (
		SELECT 1 FROM pg_stat_activity
		WHERE datname = current_database()
		  AND wait_event_type = 'Lock'
		  AND query LIKE '%FROM sessions%FOR UPDATE%')`
	for deadline := time.Now().Add(10 * time.Second); ; {
		select {
		case r := <-ch:
			t.Fatalf("archive answered (%d, err %v) before the running flip committed", r.status, r.err)
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
			t.Fatal("guard never blocked on the held session row lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatalf("archive request: %v", r.err)
	}
	wantErr(t, r.status, r.body, http.StatusBadRequest, "invalid_request_error")
}
