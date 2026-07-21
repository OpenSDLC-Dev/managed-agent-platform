package api_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestCreateValidationEdgeCases sweeps the request-shape branches shared by
// the parsers: wrong JSON types, bad unions, bad nested fields.
func TestCreateValidationEdgeCases(t *testing.T) {
	s := newTestServer(t)

	agentCases := map[string]any{
		"model wrong type":        map[string]any{"name": "x", "model": true},
		"model without id":        map[string]any{"name": "x", "model": map[string]any{"speed": "fast"}},
		"model bad speed":         map[string]any{"name": "x", "model": map[string]any{"id": "m", "speed": "warp"}},
		"name wrong type":         map[string]any{"name": 7, "model": "m"},
		"tools not array":         map[string]any{"name": "x", "model": "m", "tools": map[string]any{}},
		"tools entry not object":  map[string]any{"name": "x", "model": "m", "tools": []any{"bash"}},
		"mcp_servers not array":   map[string]any{"name": "x", "model": "m", "mcp_servers": "docs"},
		"mcp_servers bad entry":   map[string]any{"name": "x", "model": "m", "mcp_servers": []any{map[string]any{"type": "stdio", "name": "d", "url": "u"}}},
		"mcp_servers non-object":  map[string]any{"name": "x", "model": "m", "mcp_servers": []any{1}},
		"skills bad type":         map[string]any{"name": "x", "model": "m", "skills": []any{map[string]any{"type": "community", "skill_id": "s"}}},
		"skills missing skill_id": map[string]any{"name": "x", "model": "m", "skills": []any{map[string]any{"type": "custom"}}},
		"skills entry non-object": map[string]any{"name": "x", "model": "m", "skills": []any{[]any{}}},
		"metadata not object":     map[string]any{"name": "x", "model": "m", "metadata": []any{}},
		"metadata non-string val": map[string]any{"name": "x", "model": "m", "metadata": map[string]any{"k": 1}},
		"body is array":           `[1,2]`,
		"unknown field":           map[string]any{"name": "x", "model": "m", "sytem": "typo"},
	}
	for name, body := range agentCases {
		status, res := s.do(http.MethodPost, "/v1/agents", body)
		if status != http.StatusBadRequest {
			t.Errorf("agent %s: status %d, want 400 (%v)", name, status, res)
			continue
		}
		wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	}

	envCases := map[string]any{
		"config not object":     map[string]any{"name": "x", "config": []any{}},
		"networking not object": map[string]any{"name": "x", "config": map[string]any{"type": "cloud", "networking": []any{}}},
		"packages not object":   map[string]any{"name": "x", "config": map[string]any{"type": "cloud", "packages": []any{}}},
		"self_hosted extras":    map[string]any{"name": "x", "config": map[string]any{"type": "self_hosted", "packages": map[string]any{}}},
		"cloud unknown field":   map[string]any{"name": "x", "config": map[string]any{"type": "cloud", "gpu": true}},
		"scope wrong type":      map[string]any{"name": "x", "scope": 1},
		"scope unknown value":   map[string]any{"name": "x", "scope": "galaxy"},
		"unknown top-level":     map[string]any{"name": "x", "descripton": "typo"},
		"networking typo field": map[string]any{"name": "x", "config": map[string]any{"type": "cloud", "networking": map[string]any{"type": "limited", "allowedHosts": []any{"a"}}}},
		"unrestricted extras":   map[string]any{"name": "x", "config": map[string]any{"type": "cloud", "networking": map[string]any{"type": "unrestricted", "allowed_hosts": []any{"a"}}}},
		"hosts not a list":      map[string]any{"name": "x", "config": map[string]any{"type": "cloud", "networking": map[string]any{"type": "limited", "allowed_hosts": "internal.corp"}}},
		"flag not a bool":       map[string]any{"name": "x", "config": map[string]any{"type": "cloud", "networking": map[string]any{"type": "limited", "allow_mcp_servers": "yes"}}},
		"package not a list":    map[string]any{"name": "x", "config": map[string]any{"type": "cloud", "packages": map[string]any{"pip": "requests"}}},
	}
	for name, body := range envCases {
		status, res := s.do(http.MethodPost, "/v1/environments", body)
		if status != http.StatusBadRequest {
			t.Errorf("environment %s: status %d, want 400 (%v)", name, status, res)
			continue
		}
		wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	}

	agentID, envID := fixture(t, s)
	sessionCases := map[string]any{
		"agent wrong type":        map[string]any{"agent": 7, "environment_id": envID},
		"agent object no id":      map[string]any{"agent": map[string]any{"type": "agent"}, "environment_id": envID},
		"negative version":        map[string]any{"agent": map[string]any{"type": "agent", "id": agentID, "version": -1}, "environment_id": envID},
		"null model override":     map[string]any{"agent": map[string]any{"type": "agent_with_overrides", "id": agentID, "model": nil}, "environment_id": envID},
		"explicit version zero":   map[string]any{"agent": map[string]any{"type": "agent", "id": agentID, "version": 0}, "environment_id": envID},
		"non-string sys override": map[string]any{"agent": map[string]any{"type": "agent_with_overrides", "id": agentID, "system": 5}, "environment_id": envID},
		"bad override tools":      map[string]any{"agent": map[string]any{"type": "agent_with_overrides", "id": agentID, "tools": []any{map[string]any{"type": "bogus"}}}, "environment_id": envID},
		"title wrong type":        map[string]any{"agent": agentID, "environment_id": envID, "title": 3},
		"resources not array":     map[string]any{"agent": agentID, "environment_id": envID, "resources": map[string]any{}},
		"unknown field":           map[string]any{"agent": agentID, "environment_id": envID, "titel": "typo"},
	}
	for name, body := range sessionCases {
		status, res := s.do(http.MethodPost, "/v1/sessions", body)
		if status != http.StatusBadRequest {
			t.Errorf("session %s: status %d, want 400 (%v)", name, status, res)
			continue
		}
		wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	}
}

func TestUpdateValidationEdgeCases(t *testing.T) {
	s := newTestServer(t)
	agent := createAgent(t, s, map[string]any{"name": "u", "model": "m"})
	agentID := agent["id"].(string)
	env := createEnvironment(t, s, map[string]any{"name": "ue"})
	envID := env["id"].(string)
	sess := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})
	sessID := sess["id"].(string)

	for name, tc := range map[string]struct {
		path string
		body any
	}{
		"agent version wrong type": {"/v1/agents/" + agentID, map[string]any{"version": "one"}},
		"agent model cleared":      {"/v1/agents/" + agentID, map[string]any{"version": 1, "model": nil}},
		"agent name cleared":       {"/v1/agents/" + agentID, map[string]any{"version": 1, "name": nil}},
		"agent metadata bad":       {"/v1/agents/" + agentID, map[string]any{"version": 1, "metadata": []any{}}},
		"agent malformed body":     {"/v1/agents/" + agentID, `{"version"`},
		"env name cleared":         {"/v1/environments/" + envID, map[string]any{"name": ""}},
		"env bad config":           {"/v1/environments/" + envID, map[string]any{"config": map[string]any{"type": "bad"}}},
		"env account scope":        {"/v1/environments/" + envID, map[string]any{"scope": "account"}},
		"session agent not object": {"/v1/sessions/" + sessID, map[string]any{"agent": "raw"}},
		"session bad tools":        {"/v1/sessions/" + sessID, map[string]any{"agent": map[string]any{"tools": "x"}}},
		"session bad mcp":          {"/v1/sessions/" + sessID, map[string]any{"agent": map[string]any{"mcp_servers": []any{map[string]any{"type": "url"}}}}},
		"session metadata bad":     {"/v1/sessions/" + sessID, map[string]any{"metadata": "x"}},
		"session title wrong type": {"/v1/sessions/" + sessID, map[string]any{"title": []any{}}},
	} {
		status, res := s.do(http.MethodPost, tc.path, tc.body)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%v)", name, status, res)
			continue
		}
		wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	}

	// Null title on session update clears it; null metadata is a no-op.
	status, updated := s.do(http.MethodPost, "/v1/sessions/"+sessID, map[string]any{"title": nil, "metadata": nil, "agent": nil})
	if status != http.StatusOK || updated["title"] != "" {
		t.Errorf("null title: %d %v", status, updated["title"])
	}
	// Null description clears; omitted config is preserved on environments.
	status, envUpd := s.do(http.MethodPost, "/v1/environments/"+envID, map[string]any{"description": nil, "config": nil})
	if status != http.StatusOK || envUpd["description"] != "" {
		t.Errorf("env null description: %d %v", status, envUpd["description"])
	}
	if cfg, _ := envUpd["config"].(map[string]any); cfg["type"] != "cloud" {
		t.Errorf("env config not preserved: %v", envUpd["config"])
	}
}

// TestMetadataRejectsNUL sweeps every metadata-accepting endpoint for U+0000.
// It is well-formed JSON (the \u0000 escape) but Postgres cannot store it in a
// jsonb value, nor bind it in the text[] of delete keys the work patch uses, so
// without a guard a well-formed request becomes a 500 at insert time. The guard
// lives in decodeObject, which every JSON body passes through (see
// TestStringFieldsRejectNUL for the rest of the surface it covers), and this
// sweep is what keeps the metadata endpoints from drifting apart — it is the
// same rejection internal/events already applies to inbound event payloads.
//
// Keys are rejected alongside values, and a delete key alongside an upsert: a
// NUL is unstorable wherever it appears, and on the Go-side merge a NUL delete
// key would otherwise be silently accepted as a no-op while the identical patch
// against the work endpoint's SQL-side merge failed.
func TestMetadataRejectsNUL(t *testing.T) {
	s := newTestServer(t)
	agent := createAgent(t, s, map[string]any{"name": "nul", "model": "m"})
	agentID := agent["id"].(string)
	env := createEnvironment(t, s, map[string]any{"name": "nul-env"})
	envID := env["id"].(string)
	sess := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})
	sessID := sess["id"].(string)

	const workKey = "ek-nul"
	workEnvID, workSessionID := selfHostedWorker(t, s, workKey)
	workID := s.enqueueAndPoll(t, workEnvID, workSessionID, workKey)

	var (
		nulValue  = map[string]any{"k": "a\x00b"}
		nulKey    = map[string]any{"a\x00b": "v"}
		nulDelete = map[string]any{"a\x00b": nil}
	)

	for name, tc := range map[string]struct {
		path string
		body any
	}{
		"agent create value":    {"/v1/agents", map[string]any{"name": "x", "model": "m", "metadata": nulValue}},
		"agent create key":      {"/v1/agents", map[string]any{"name": "x", "model": "m", "metadata": nulKey}},
		"agent update value":    {"/v1/agents/" + agentID, map[string]any{"version": 1, "metadata": nulValue}},
		"agent update delete":   {"/v1/agents/" + agentID, map[string]any{"version": 1, "metadata": nulDelete}},
		"env create value":      {"/v1/environments", map[string]any{"name": "x", "metadata": nulValue}},
		"env create key":        {"/v1/environments", map[string]any{"name": "x", "metadata": nulKey}},
		"env update value":      {"/v1/environments/" + envID, map[string]any{"metadata": nulValue}},
		"env update delete":     {"/v1/environments/" + envID, map[string]any{"metadata": nulDelete}},
		"session create value":  {"/v1/sessions", map[string]any{"agent": agentID, "environment_id": envID, "metadata": nulValue}},
		"session create key":    {"/v1/sessions", map[string]any{"agent": agentID, "environment_id": envID, "metadata": nulKey}},
		"session update value":  {"/v1/sessions/" + sessID, map[string]any{"metadata": nulValue}},
		"session update delete": {"/v1/sessions/" + sessID, map[string]any{"metadata": nulDelete}},
	} {
		status, res := s.do(http.MethodPost, tc.path, tc.body)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%v)", name, status, res)
			continue
		}
		wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	}

	// The work patch is the same parser behind worker auth (Bearer environment
	// key), and the only endpoint whose merge happens in SQL.
	workPath := "/v1/environments/" + workEnvID + "/work/" + workID
	for name, body := range map[string]any{
		"work value":  map[string]any{"metadata": nulValue},
		"work key":    map[string]any{"metadata": nulKey},
		"work delete": map[string]any{"metadata": nulDelete},
	} {
		res, obj, raw := s.workReq(t, http.MethodPost, workPath, workKey, body)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%s)", name, res.StatusCode, raw)
			continue
		}
		wantErr(t, res.StatusCode, obj, http.StatusBadRequest, "invalid_request_error")
	}
}

// TestPathAndQueryRejectNUL is the body sweeps' companion for the surface #114
// left open: path IDs and id-shaped query parameters. Go's http.ServeMux
// percent-decodes %00 into a real NUL in PathValue / URL.Query, so without a
// shape guard the byte binds straight into Postgres and fails with SQLSTATE
// 22021 — a 500. Every affected surface must instead return the wire error an
// unknown or absent id already gets: a 404 on a path id (or work item), a 400 on
// an id-shaped query filter, the page cursor, or the free-form types[] filter.
// See #135.
func TestPathAndQueryRejectNUL(t *testing.T) {
	s := newTestServer(t)
	agent := createAgent(t, s, map[string]any{"name": "nul-id", "model": "m"})
	agentID := agent["id"].(string)
	env := createEnvironment(t, s, map[string]any{"name": "nul-id-env"})
	envID := env["id"].(string)
	sess := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})
	sessID := sess["id"].(string)

	// nul is a percent-encoded U+0000 the mux decodes into a real NUL byte in the
	// PathValue / query value the handler reads.
	const nul = "%00"

	// Path ids → 404 not_found_error (the shape an unknown id already returns).
	for name, tc := range map[string]struct {
		method, path string
		body         any
	}{
		"agent get":       {http.MethodGet, "/v1/agents/agent_" + nul, nil},
		"agent update":    {http.MethodPost, "/v1/agents/agent_" + nul, map[string]any{"version": 1}},
		"agent versions":  {http.MethodGet, "/v1/agents/agent_" + nul + "/versions", nil},
		"agent archive":   {http.MethodPost, "/v1/agents/agent_" + nul + "/archive", nil},
		"env get":         {http.MethodGet, "/v1/environments/env_" + nul, nil},
		"env update":      {http.MethodPost, "/v1/environments/env_" + nul, map[string]any{"name": "x"}},
		"env delete":      {http.MethodDelete, "/v1/environments/env_" + nul, nil},
		"env archive":     {http.MethodPost, "/v1/environments/env_" + nul + "/archive", nil},
		"session get":     {http.MethodGet, "/v1/sessions/sesn_" + nul, nil},
		"session update":  {http.MethodPost, "/v1/sessions/sesn_" + nul, map[string]any{"title": "x"}},
		"session delete":  {http.MethodDelete, "/v1/sessions/sesn_" + nul, nil},
		"session archive": {http.MethodPost, "/v1/sessions/sesn_" + nul + "/archive", nil},
		"events send":     {http.MethodPost, "/v1/sessions/sesn_" + nul + "/events", map[string]any{"events": []any{}}},
		"events list":     {http.MethodGet, "/v1/sessions/sesn_" + nul + "/events", nil},
		"events stream":   {http.MethodGet, "/v1/sessions/sesn_" + nul + "/events/stream", nil},
		// Invalid UTF-8 (a percent-decoded %80) is unstorable the same way, and the
		// alphabet check rejects it on shape too.
		"agent get invalid utf-8": {http.MethodGet, "/v1/agents/agent_%80", nil},
	} {
		status, res := s.do(tc.method, tc.path, tc.body)
		if status != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404 (%v)", name, status, res)
			continue
		}
		wantErr(t, status, res, http.StatusNotFound, "not_found_error")
	}

	// Id-shaped query filters, the page cursor, and the free-form types[] filter
	// → 400 invalid_request_error. The cursor carries the NUL in its decoded id.
	nulCursor := base64.RawURLEncoding.EncodeToString([]byte("k1|n|t|1|sesn_\x00"))
	for name, path := range map[string]string{
		"agent_id filter":            "/v1/sessions?agent_id=agent_" + nul,
		"page cursor id":             "/v1/sessions?page=" + nulCursor,
		"types filter":               "/v1/sessions/" + sessID + "/events?types[]=user." + nul,
		"types filter invalid utf-8": "/v1/sessions/" + sessID + "/events?types[]=user.%80",
	} {
		status, res := s.do(http.MethodGet, path, nil)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%v)", name, status, res)
			continue
		}
		wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	}

	// The work API (Bearer environment-key auth): a NUL work_id is the 404 an
	// unknown item gets at every work-item endpoint. Body validation still runs
	// first, so the metadata patch reaches its malformed work_id (a 404) only with
	// a valid body — the same order TestWorkPollRejectsWrongMethodAndPath pins.
	const key = "ek-nul-idsweep"
	workEnvID, _ := selfHostedWorker(t, s, key)
	workNULID := "/v1/environments/" + workEnvID + "/work/work_" + nul
	for name, tc := range map[string]struct {
		method, path string
		body         any
	}{
		"work get":       {http.MethodGet, workNULID, nil},
		"work update":    {http.MethodPost, workNULID, map[string]any{"metadata": map[string]any{"k": "v"}}},
		"work ack":       {http.MethodPost, workNULID + "/ack", nil},
		"work heartbeat": {http.MethodPost, workNULID + "/heartbeat?expected_last_heartbeat=x", nil},
		"work stop":      {http.MethodPost, workNULID + "/stop", nil},
	} {
		res, obj, raw := s.workReq(t, tc.method, tc.path, key, tc.body)
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404 (%s)", name, res.StatusCode, raw)
			continue
		}
		wantErr(t, res.StatusCode, obj, http.StatusNotFound, "not_found_error")
	}

	// The worker's session read (GET /v1/sessions/{id}) is dual-auth: with a
	// Bearer key it runs through requireEnvironmentKeyForSession, which binds the
	// id before the handler — a NUL there must be the same 404, never a 500.
	res := s.doRaw(http.MethodGet, "/v1/sessions/sesn_"+nul, nil,
		map[string]string{"Authorization": "Bearer " + key})
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("worker session read: status %d, want 404", res.StatusCode)
	}
	res.Body.Close()
}

// TestStringFieldsRejectNUL is the metadata sweep's twin for every other
// client-supplied string: a U+0000 anywhere in the request body is a 400, not a
// 500. Before the guard moved to decodeObject these all reached Postgres, where
// a text bind fails with 22021 and a jsonb bind with 22P05 — neither an
// *apiError, so writeError rendered both as a 500 api_error.
//
// The nested cases are the reason the guard walks the whole decoded body rather
// than sitting on stringField/requiredString: config.packages.npm[],
// networking.allowed_hosts[], and the tools/mcp_servers/skills entries are
// parsed straight out of raw JSON and would slip past a per-field check.
func TestStringFieldsRejectNUL(t *testing.T) {
	s := newTestServer(t)
	agentID := createAgent(t, s, map[string]any{"name": "nul-str", "model": "m"})["id"].(string)
	envID := createEnvironment(t, s, map[string]any{"name": "nul-str-env"})["id"].(string)
	sessID := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})["id"].(string)

	const nul = "a\x00b"

	for name, tc := range map[string]struct {
		path string
		body any
		// field is the path the message must name, so a client can find the
		// offending value instead of hunting through its own request.
		field string
	}{
		"agent create name":        {"/v1/agents", map[string]any{"name": nul, "model": "m"}, "name"},
		"agent create model":       {"/v1/agents", map[string]any{"name": "x", "model": nul}, "model"},
		"agent create system":      {"/v1/agents", map[string]any{"name": "x", "model": "m", "system": nul}, "system"},
		"agent create description": {"/v1/agents", map[string]any{"name": "x", "model": "m", "description": nul}, "description"},
		"agent create tool name": {"/v1/agents", map[string]any{"name": "x", "model": "m", "tools": []any{
			map[string]any{"type": "custom", "name": nul, "description": "d", "input_schema": map[string]any{}},
		}}, "tools[0].name"},
		"agent create mcp server url": {"/v1/agents", map[string]any{"name": "x", "model": "m", "mcp_servers": []any{
			map[string]any{"type": "url", "name": "n", "url": nul},
		}}, "mcp_servers[0].url"},
		"agent create skill id": {"/v1/agents", map[string]any{"name": "x", "model": "m", "skills": []any{
			map[string]any{"type": "anthropic", "skill_id": nul},
		}}, "skills[0].skill_id"},
		"agent update name":   {"/v1/agents/" + agentID, map[string]any{"version": 1, "name": nul}, "name"},
		"agent update system": {"/v1/agents/" + agentID, map[string]any{"version": 1, "system": nul}, "system"},

		"env create name":        {"/v1/environments", map[string]any{"name": nul}, "name"},
		"env create description": {"/v1/environments", map[string]any{"name": "x", "description": nul}, "description"},
		"env create package": {"/v1/environments", map[string]any{"name": "x", "config": map[string]any{
			"type": "cloud", "packages": map[string]any{"npm": []any{nul}},
		}}, "config.packages.npm[0]"},
		"env create allowed host": {"/v1/environments", map[string]any{"name": "x", "config": map[string]any{
			"type": "cloud", "networking": map[string]any{"type": "limited", "allowed_hosts": []any{nul}},
		}}, "config.networking.allowed_hosts[0]"},
		"env update description": {"/v1/environments/" + envID, map[string]any{"description": nul}, "description"},
		"env update package": {"/v1/environments/" + envID, map[string]any{"config": map[string]any{
			"type": "cloud", "packages": map[string]any{"npm": []any{nul}},
		}}, "config.packages.npm[0]"},

		"session create title": {"/v1/sessions", map[string]any{
			"agent": agentID, "environment_id": envID, "title": nul,
		}, "title"},
		"session update title": {"/v1/sessions/" + sessID, map[string]any{"title": nul}, "title"},

		// A NUL in a *top-level* key has no field path to quote, so the message
		// names the body itself — and must not echo the key, which would put the
		// rejected byte back into the response.
		"top-level key": {"/v1/agents", map[string]any{"name": "x", "model": "m", nul: "v"}, "the request body"},
	} {
		status, res := s.do(http.MethodPost, tc.path, tc.body)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%v)", name, status, res)
			continue
		}
		wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
		inner, _ := res["error"].(map[string]any)
		msg, _ := inner["message"].(string)
		if !strings.Contains(msg, "U+0000") || !strings.Contains(msg, tc.field) {
			t.Errorf("%s: message = %q, want it to name %q and U+0000", name, msg, tc.field)
		}
	}
}

// TestNULGuardKeepsOutOfRangeNumbers pins the one way this guard could have made
// a working request fail. It inspects the body by decoding it a second time, and
// a plain `any` decode turns every number into a float64 — so a literal outside
// float64 range, such as the 1e400 a JSON Schema may legitimately carry in a
// passthrough field, would have failed that decode and become a 400 on a body
// with nothing wrong with it. Postgres stores the value fine, so the only correct
// answer is to store it. Both halves matter: the number alone must still be
// accepted, and a body carrying the number *and* a NUL must still be rejected for
// the NUL, naming the field rather than blaming the body's shape.
func TestNULGuardKeepsOutOfRangeNumbers(t *testing.T) {
	s := newTestServer(t)
	schema := map[string]any{"type": "object", "maximum": json.RawMessage("1e400")}
	tool := func(name string) any {
		return map[string]any{"type": "custom", "name": name, "description": "d", "input_schema": schema}
	}

	// Status only, not the parsed body: the response echoes the spec back, and
	// this test client decodes into float64 — the very decode the fix had to stop
	// doing server-side, here demonstrating the hazard from the other end.
	if status, res := s.do(http.MethodPost, "/v1/agents", map[string]any{
		"name": "big-number", "model": "m", "tools": []any{tool("t")},
	}); status != http.StatusOK {
		t.Fatalf("agent create with an out-of-range number literal: status %d, body %v", status, res)
	}

	status, res := s.do(http.MethodPost, "/v1/agents", map[string]any{
		"name": "big-number-nul", "model": "m", "tools": []any{tool("a\x00b")},
	})
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	inner, _ := res["error"].(map[string]any)
	if msg, _ := inner["message"].(string); !strings.Contains(msg, "tools[0].name") {
		t.Errorf("message = %q, want it to name tools[0].name", msg)
	}
}

// TestNullFieldLeniency: the reference treats explicit null as "clear" (or
// "absent") for optional fields; none of these may error.
func TestNullFieldLeniency(t *testing.T) {
	s := newTestServer(t)

	agent := createAgent(t, s, map[string]any{
		"name": "n", "model": "m",
		"system": nil, "description": nil, "multiagent": nil,
		"tools": nil, "mcp_servers": nil, "skills": nil, "metadata": nil,
	})
	if agent["system"] != "" || agent["description"] != "" {
		t.Errorf("null strings should clear: %v/%v", agent["system"], agent["description"])
	}
	for _, k := range []string{"tools", "mcp_servers", "skills"} {
		if arr, ok := agent[k].([]any); !ok || len(arr) != 0 {
			t.Errorf("null %s should render []: %v", k, agent[k])
		}
	}

	status, updated := s.do(http.MethodPost, "/v1/agents/"+agent["id"].(string),
		map[string]any{"version": 1, "system": nil, "tools": nil})
	if status != http.StatusOK || updated["system"] != "" {
		t.Errorf("update null system: %d %v", status, updated["system"])
	}

	env := createEnvironment(t, s, map[string]any{
		"name": "e", "description": nil, "scope": "organization",
		"config": map[string]any{
			"type":       "cloud",
			"networking": map[string]any{"type": "limited"},
			"packages": map[string]any{
				"apt": []any{"jq"}, "cargo": nil, "gem": []any{}, "go": []any{"golang.org/x/tools"},
				"npm": []any{"left-pad"}, "pip": []any{"requests"},
			},
		},
	})
	cfg, _ := env["config"].(map[string]any)
	nw, _ := cfg["networking"].(map[string]any)
	if hosts, ok := nw["allowed_hosts"].([]any); !ok || len(hosts) != 0 {
		t.Errorf("limited without allowed_hosts should default []: %v", nw)
	}
	// Explicit null allowed_hosts also normalizes to [].
	nullHosts := createEnvironment(t, s, map[string]any{
		"name": "e2",
		"config": map[string]any{"type": "cloud",
			"networking": map[string]any{"type": "limited", "allowed_hosts": nil}},
	})
	cfg2, _ := nullHosts["config"].(map[string]any)
	if nw2, _ := cfg2["networking"].(map[string]any); nw2["allowed_hosts"] == nil {
		t.Errorf("null allowed_hosts should render []: %v", nw2)
	}
	pkgs, _ := cfg["packages"].(map[string]any)
	if cargo, ok := pkgs["cargo"].([]any); !ok || len(cargo) != 0 {
		t.Errorf("null cargo should render []: %v", pkgs["cargo"])
	}

	sess := createSession(t, s, map[string]any{
		"agent": map[string]any{
			"type": "agent_with_overrides", "id": agent["id"],
			"skills":      []any{map[string]any{"type": "anthropic", "skill_id": "xlsx"}},
			"mcp_servers": []any{map[string]any{"type": "url", "name": "d", "url": "https://x"}},
		},
		"environment_id": env["id"],
		"title":          nil, "metadata": nil, "resources": nil, "vault_ids": nil,
	})
	a, _ := sess["agent"].(map[string]any)
	if skills, _ := a["skills"].([]any); len(skills) != 1 {
		t.Errorf("skills override lost: %v", a["skills"])
	}
	if mcp, _ := a["mcp_servers"].([]any); len(mcp) != 1 {
		t.Errorf("mcp_servers override lost: %v", a["mcp_servers"])
	}

	// A literal null body is an empty object, so required-field checks fire.
	status, body := s.do(http.MethodPost, "/v1/agents", "null")
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
}

func TestQueryParamValidation(t *testing.T) {
	s := newTestServer(t)
	agent := createAgent(t, s, map[string]any{"name": "q", "model": "m"})
	id := agent["id"].(string)

	for name, path := range map[string]string{
		"bad include_archived":    "/v1/agents?include_archived=banana",
		"agent version zero":      "/v1/agents/" + id + "?version=0",
		"agent version not a num": "/v1/agents/" + id + "?version=abc",
		"session agent_version":   "/v1/sessions?agent_id=" + id + "&agent_version=abc",
	} {
		status, body := s.do(http.MethodGet, path, nil)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%v)", name, status, body)
			continue
		}
		wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	}
}

// TestCorruptStoredDataSurfacesAsAPIError drives the defensive decode
// branches: rows corrupted out-of-band must produce the 500 api_error
// envelope, not a panic or a silent success.
func TestCorruptStoredDataSurfacesAsAPIError(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	agent := createAgent(t, s, map[string]any{"name": "c", "model": "m"})
	agentID := agent["id"].(string)
	env := createEnvironment(t, s, map[string]any{"name": "ce"})
	sess := createSession(t, s, map[string]any{"agent": agentID, "environment_id": env["id"]})
	sessID := sess["id"].(string)

	if _, err := s.pool.Exec(ctx, `UPDATE agents SET spec = '"corrupt"' WHERE id = $1`, agentID); err != nil {
		t.Fatalf("corrupt agent: %v", err)
	}
	status, body := s.do(http.MethodGet, "/v1/agents/"+agentID, nil)
	wantErr(t, status, body, http.StatusInternalServerError, "api_error")

	if _, err := s.pool.Exec(ctx, `UPDATE sessions SET resolved_agent = '"corrupt"' WHERE id = $1`, sessID); err != nil {
		t.Fatalf("corrupt session: %v", err)
	}
	status, body = s.do(http.MethodGet, "/v1/sessions/"+sessID, nil)
	wantErr(t, status, body, http.StatusInternalServerError, "api_error")

	if _, err := s.pool.Exec(ctx, `UPDATE environments SET metadata = '[]' WHERE id = $1`, env["id"]); err != nil {
		t.Fatalf("corrupt environment: %v", err)
	}
	status, body = s.do(http.MethodGet, "/v1/environments/"+env["id"].(string), nil)
	wantErr(t, status, body, http.StatusInternalServerError, "api_error")

	// The corrupt rows also break the list, session-update, archive, and
	// pinned-version paths that decode them. Order matters: archiving the
	// corrupt session commits before its render fails, hiding it from the
	// default list and freezing it against updates — so those checks run first.
	for _, req := range []struct {
		name, method, path string
		body               any
	}{
		{"agents list", http.MethodGet, "/v1/agents", nil},
		{"environments list", http.MethodGet, "/v1/environments", nil},
		{"sessions list", http.MethodGet, "/v1/sessions", nil},
		{"session update", http.MethodPost, "/v1/sessions/" + sessID,
			map[string]any{"agent": map[string]any{"tools": []any{}}}},
		{"session archive", http.MethodPost, "/v1/sessions/" + sessID + "/archive", nil},
	} {
		status, body := s.do(req.method, req.path, req.body)
		if status != http.StatusInternalServerError {
			t.Errorf("%s: status %d, want 500 (%v)", req.name, status, body)
			continue
		}
		wantErr(t, status, body, http.StatusInternalServerError, "api_error")
	}

	// Corrupt agent metadata breaks the versions list and the pinned read
	// (both join the parent row's metadata).
	if _, err := s.pool.Exec(ctx, `UPDATE agents SET metadata = '[]' WHERE id = $1`, agentID); err != nil {
		t.Fatalf("corrupt agent metadata: %v", err)
	}
	status, body = s.do(http.MethodGet, "/v1/agents/"+agentID+"/versions", nil)
	wantErr(t, status, body, http.StatusInternalServerError, "api_error")
	status, body = s.do(http.MethodGet, "/v1/agents/"+agentID+"?version=1", nil)
	wantErr(t, status, body, http.StatusInternalServerError, "api_error")

	// A second session with corrupt usage breaks its own GET.
	sess2 := createSession(t, s, map[string]any{"agent": mustCleanAgent(t, s), "environment_id": mustCleanEnv(t, s)})
	if _, err := s.pool.Exec(ctx, `UPDATE sessions SET usage = '"x"' WHERE id = $1`, sess2["id"]); err != nil {
		t.Fatalf("corrupt usage: %v", err)
	}
	status, body = s.do(http.MethodGet, "/v1/sessions/"+sess2["id"].(string), nil)
	wantErr(t, status, body, http.StatusInternalServerError, "api_error")
}

func mustCleanAgent(t *testing.T, s *tserver) string {
	t.Helper()
	return createAgent(t, s, map[string]any{"name": "clean", "model": "m"})["id"].(string)
}

func mustCleanEnv(t *testing.T, s *tserver) string {
	t.Helper()
	return createEnvironment(t, s, map[string]any{"name": "clean"})["id"].(string)
}

// TestDatabaseFailuresSurfaceAsAPIError drives the mid-transaction error
// branches by removing a table the write path needs.
func TestDatabaseFailuresSurfaceAsAPIError(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	agent := createAgent(t, s, map[string]any{"name": "d", "model": "m"})
	agentID := agent["id"].(string)

	if _, err := s.pool.Exec(ctx, `DROP TABLE agent_versions CASCADE`); err != nil {
		t.Fatalf("drop agent_versions: %v", err)
	}
	// Create and update both snapshot into agent_versions; versions list reads it.
	status, body := s.do(http.MethodPost, "/v1/agents", map[string]any{"name": "x", "model": "m"})
	wantErr(t, status, body, http.StatusInternalServerError, "api_error")
	status, body = s.do(http.MethodPost, "/v1/agents/"+agentID, map[string]any{"version": 1, "name": "y"})
	wantErr(t, status, body, http.StatusInternalServerError, "api_error")
	status, body = s.do(http.MethodGet, "/v1/agents/"+agentID+"/versions", nil)
	wantErr(t, status, body, http.StatusInternalServerError, "api_error")
}

// TestAuthDatabaseFailure: a broken pool must yield the 500 envelope from the
// auth middleware, not a hung or leaked request.
func TestAuthDatabaseFailure(t *testing.T) {
	s := newTestServer(t)
	s.pool.Close()
	status, body := s.do(http.MethodGet, "/v1/agents", nil)
	wantErr(t, status, body, http.StatusInternalServerError, "api_error")
}
