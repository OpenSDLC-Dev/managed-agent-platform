package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// The dream runner's own agent and environment are hidden rows (plan 41 §4.4):
// unlisted, unretrievable through any id-addressed route, and refused by every
// resolver but the runner's own. The ids are not secret — a dream's pipeline
// session renders agent.id and environment_id to any viewer — so the resolvers
// are the half that matters: without them a developer key could open a session,
// or point a deployment, at an always_allow agent no operator created or can see.

// internalAgentBody and internalEnvBody are the request bodies plan 41 §4.3
// spells out, verbatim: the runner hands them to the same insert bodies
// POST /v1/agents and POST /v1/environments use, so the stored rows are the
// handlers' own normalization and no test (or plan) has to spell a stored shape.
const internalAgentBody = `{
	"name": "dream",
	"model": {"id": "dream-placeholder"},
	"system": "",
	"tools": [{"type": "agent_toolset_20260401",
	           "default_config": {"enabled": true, "permission_policy": {"type": "always_allow"}},
	           "configs": [{"type": "web_fetch", "name": "web_fetch", "enabled": false},
	                       {"type": "web_search", "name": "web_search", "enabled": false}]}],
	"mcp_servers": [],
	"skills": [],
	"multiagent": {"type": "coordinator", "agents": [{"type": "self"}]}
}`

const internalEnvBody = `{
	"name": "dream",
	"config": {"type": "cloud", "networking": {"type": "limited", "allowed_hosts": []}, "packages": {}}
}`

// insertInternalPair writes the runner's two hidden rows through the shared
// insert bodies and returns their ids.
func insertInternalPair(t *testing.T, s *tserver) (agentID, envID string) {
	t.Helper()
	ctx := context.Background()
	agentID = domain.NewID(domain.PrefixAgent).String()
	envID = domain.NewID(domain.PrefixEnvironment).String()
	inserted, err := api.InsertAgentForTest(ctx, s.pool, internalAgentBody, agentID, true)
	if err != nil || !inserted {
		t.Fatalf("insert internal agent: inserted=%v err=%v", inserted, err)
	}
	inserted, err = api.InsertEnvironmentForTest(ctx, s.pool, internalEnvBody, envID, true)
	if err != nil || !inserted {
		t.Fatalf("insert internal environment: inserted=%v err=%v", inserted, err)
	}
	return agentID, envID
}

func TestInternalRowsAreUnlistedAndUnretrievable(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := insertInternalPair(t, s)
	visibleAgent, visibleEnv := fixture(t, s)

	for _, tc := range []struct{ path, hidden, shown string }{
		{"/v1/agents", agentID, visibleAgent},
		{"/v1/agents?include_archived=true", agentID, visibleAgent},
		{"/v1/environments", envID, visibleEnv},
		{"/v1/environments?include_archived=true", envID, visibleEnv},
	} {
		status, body := s.do(http.MethodGet, tc.path, nil)
		if status != http.StatusOK {
			t.Fatalf("GET %s: status %d (%v)", tc.path, status, body)
		}
		var sawHidden, sawShown bool
		for _, row := range listData(t, body) {
			sawHidden = sawHidden || row["id"] == tc.hidden
			sawShown = sawShown || row["id"] == tc.shown
		}
		if sawHidden {
			t.Errorf("GET %s lists the internal row %s", tc.path, tc.hidden)
		}
		if !sawShown {
			t.Errorf("GET %s does not list %s, so the query lost more than the internal row", tc.path, tc.shown)
		}
	}

	// Every id-addressed route answers the 404 an unknown id gets — the
	// versions list among them, which would otherwise render the internal spec
	// to anyone holding the id — and the console API's three environment-key
	// routes with them. Off the wire is not off the rule: a listing that
	// answered 200 confirms the row, and issuance refusing with "is a cloud
	// environment" says what kind it is.
	//
	// The revoke arm only carries the rule with a key to aim at — a missing key
	// answers 404 by itself — so one is minted straight through the store,
	// which is the only way to give a cloud environment a key at all.
	internalKeyID := storeIssuedKeyID(t, s, envID)

	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/v1/agents/" + agentID, nil},
		{http.MethodGet, "/v1/agents/" + agentID + "?version=1", nil},
		{http.MethodPost, "/v1/agents/" + agentID, nil},
		{http.MethodGet, "/v1/agents/" + agentID + "/versions", nil},
		{http.MethodPost, "/v1/agents/" + agentID + "/archive", nil},
		{http.MethodGet, "/v1/environments/" + envID, nil},
		{http.MethodPost, "/v1/environments/" + envID, nil},
		{http.MethodDelete, "/v1/environments/" + envID, nil},
		{http.MethodPost, "/v1/environments/" + envID + "/archive", nil},
		{http.MethodPost, consoleTokens(envID), map[string]any{"name": "issued to a hidden row"}},
		{http.MethodGet, consoleTokens(envID), nil},
		{http.MethodPost, consoleRevoke(envID, internalKeyID), nil},
	} {
		status, body := s.do(tc.method, tc.path, tc.body)
		wantErr(t, status, body, http.StatusNotFound, "not_found_error")
	}
}

// storeIssuedKeyID mints one worker credential for an environment through the
// store and returns its id.
func storeIssuedKeyID(t *testing.T, s *tserver, envID string) string {
	t.Helper()
	issueKey(t, s.pool, envID, "for the revoke arm")
	keys, _, err := api.ListEnvironmentKeys(context.Background(), s.pool, envID, 10, 0)
	if err != nil || len(keys) != 1 {
		t.Fatalf("list environment keys for %s: %d keys, err %v", envID, len(keys), err)
	}
	return keys[0].ID
}

func TestInternalRowsRefusedByEveryResolver(t *testing.T) {
	s := newTestServer(t)
	internalAgent, internalEnv := insertInternalPair(t, s)
	agentID, envID := fixture(t, s)

	cases := map[string]struct {
		path string
		body map[string]any
	}{
		"session create, internal agent": {"/v1/sessions",
			map[string]any{"agent": internalAgent, "environment_id": envID}},
		"session create, internal environment": {"/v1/sessions",
			map[string]any{"agent": agentID, "environment_id": internalEnv}},
		"deployment create, internal agent": {"/v1/deployments",
			deploymentBody(internalAgent, envID)},
		"deployment create, internal environment": {"/v1/deployments",
			deploymentBody(agentID, internalEnv)},
		// A roster member is the fourth resolver, and it answers the same 404
		// rather than the 400 its own "not found" carries for an unknown id.
		"roster member": {"/v1/agents", map[string]any{
			"name": "coordinator", "model": "claude-opus-4-8",
			"multiagent": map[string]any{"type": "coordinator",
				"agents": []any{map[string]any{"type": "self"}, internalAgent}},
		}},
	}
	for name, tc := range cases {
		status, body := s.do(http.MethodPost, tc.path, tc.body)
		t.Run(name, func(t *testing.T) {
			wantErr(t, status, body, http.StatusNotFound, "not_found_error")
		})
	}

	// The runner's own create is the one path that resolves them, and it does
	// so with the id it minted before the row existed.
	want := domain.NewID(domain.PrefixSession).String()
	got, err := api.CreateSessionForTest(context.Background(), s.pool, want, internalEnv,
		`{"type":"agent_with_overrides","id":"`+internalAgent+`","model":"claude-opus-4-8","system":"pipeline"}`, true)
	if err != nil {
		t.Fatalf("internal session create: %v", err)
	}
	if got != want {
		t.Errorf("session id = %s, want the pre-minted %s", got, want)
	}
	// The pipeline session itself is public: it renders the hidden agent's id,
	// which is why the resolvers above have to refuse it.
	status, session := s.do(http.MethodGet, "/v1/sessions/"+got, nil)
	if status != http.StatusOK {
		t.Fatalf("get pipeline session: status %d (%v)", status, session)
	}
	if agent, _ := session["agent"].(map[string]any); agent["id"] != internalAgent {
		t.Errorf("session agent.id = %v, want the internal agent %s", session["agent"], internalAgent)
	}

	// A second create on the same id fails the insert like any other unique
	// violation, which is what the runner retries a start after.
	if _, err := api.CreateSessionForTest(context.Background(), s.pool, want, internalEnv,
		`"`+internalAgent+`"`, true); err == nil {
		t.Error("second create on a taken session id succeeded, want the unique violation")
	}
	// Without the bypass the same body is refused, so the flag is what admits
	// the hidden rows and not the seam.
	if _, err := api.CreateSessionForTest(context.Background(), s.pool, "", internalEnv,
		`"`+internalAgent+`"`, false); err == nil {
		t.Error("internal environment resolved without the bypass")
	}
}

// The runner's rows are the handler's own writes: byte-equal spec and metadata,
// the version-1 row included, and the roster's `self` resolved to the agent's
// own id — the one thing a hand-written row could not have got right.
func TestInsertAgentInTxWritesWhatTheHandlerWrites(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	body := `{"name":"twin","model":{"id":"claude-opus-4-8"},"system":"be brief",
	          "tools":[{"type":"agent_toolset_20260401","default_config":{"enabled":true}}],
	          "mcp_servers":[],"skills":[],"metadata":{"k":"v"}}`
	status, handlerRow := s.do(http.MethodPost, "/v1/agents", body)
	if status != http.StatusOK {
		t.Fatalf("create agent: status %d (%v)", status, handlerRow)
	}
	handlerID := handlerRow["id"].(string)

	insertedID := domain.NewID(domain.PrefixAgent).String()
	inserted, err := api.InsertAgentForTest(ctx, s.pool, body, insertedID, false)
	if err != nil || !inserted {
		t.Fatalf("insertAgentInTx: inserted=%v err=%v", inserted, err)
	}

	read := func(id string) (spec, metadata, versionSpec, versionName string, version int64) {
		t.Helper()
		if err := s.pool.QueryRow(ctx,
			`SELECT a.spec::text, a.metadata::text, a.version, v.spec::text, v.name
			   FROM agents a JOIN agent_versions v ON v.agent_id = a.id AND v.version = 1
			  WHERE a.id = $1`, id).Scan(&spec, &metadata, &version, &versionSpec, &versionName); err != nil {
			t.Fatalf("read agent %s: %v", id, err)
		}
		return spec, metadata, versionSpec, versionName, version
	}
	wantSpec, wantMeta, wantVersionSpec, wantVersionName, wantVersion := read(handlerID)
	gotSpec, gotMeta, gotVersionSpec, gotVersionName, gotVersion := read(insertedID)
	if gotSpec != wantSpec || gotMeta != wantMeta {
		t.Errorf("stored spec/metadata = %s / %s, want %s / %s", gotSpec, gotMeta, wantSpec, wantMeta)
	}
	if gotVersionSpec != wantVersionSpec || gotVersionName != wantVersionName || gotVersion != wantVersion {
		t.Errorf("version 1 row = %s / %s / %d, want %s / %s / %d",
			gotVersionSpec, gotVersionName, gotVersion, wantVersionSpec, wantVersionName, wantVersion)
	}

	// Twice on one id is the runner's steady state: the second dream finds the
	// rows the first wrote and changes nothing, whatever body it carries.
	other := `{"name":"different","model":{"id":"claude-haiku-4-8"},"metadata":{}}`
	inserted, err = api.InsertAgentForTest(ctx, s.pool, other, insertedID, false)
	if err != nil {
		t.Fatalf("second insertAgentInTx: %v", err)
	}
	if inserted {
		t.Error("second insert on a taken id reported inserted=true")
	}
	if spec, meta, versionSpec, _, _ := read(insertedID); spec != wantSpec || meta != wantMeta || versionSpec != wantVersionSpec {
		t.Error("the second insert rewrote the first's rows")
	}

	// The §4.3 body's `{"type":"self"}` is a request spelling: resolveRoster
	// rewrites it to the agent's own id and version 1 before storage, which is
	// what makes the runner's threads run under the session's overrides.
	internalID := domain.NewID(domain.PrefixAgent).String()
	if _, err := api.InsertAgentForTest(ctx, s.pool, internalAgentBody, internalID, true); err != nil {
		t.Fatalf("insert internal agent: %v", err)
	}
	var selfID string
	var selfVersion int64
	if err := s.pool.QueryRow(ctx,
		`SELECT spec->'multiagent'->'agents'->0->>'id', (spec->'multiagent'->'agents'->0->>'version')::bigint
		   FROM agents WHERE id = $1`, internalID).Scan(&selfID, &selfVersion); err != nil {
		t.Fatalf("read stored roster: %v", err)
	}
	if selfID != internalID || selfVersion != 1 {
		t.Errorf("stored roster self = %s v%d, want %s v1", selfID, selfVersion, internalID)
	}
}
