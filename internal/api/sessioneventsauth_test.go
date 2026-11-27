package api_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

func managementOnlyRefusal(index int, eventType string) string {
	return fmt.Sprintf("events[%d]: `%s` may only be sent with management credentials "+
		"(an API key or Console session); "+
		"an environment key or sessions token cannot post this event type", index, eventType)
}

// readJSON drains a raw response into a status code and decoded object.
func readJSON(t *testing.T, res *http.Response) (int, map[string]any) {
	t.Helper()
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	var obj map[string]any
	_ = json.Unmarshal(raw, &obj)
	return res.StatusCode, obj
}

// TestEnvironmentKeyAuthorizesSessionEvents pins the dual-auth boundary on a
// session's worker-facing routes: a BYOC worker's Authorization: Bearer
// environment key authorizes list/send (and stream) plus the GET
// /v1/sessions/{id} read for a session in its own environment, is not-found for a
// session in another environment (or a missing one), and the management x-api-key
// keeps working. The reference environment key authorizes "both the work-poll
// calls and the session-level calls" (session Get via SetupSkills + the events
// tool runner).
func TestEnvironmentKeyAuthorizesSessionEvents(t *testing.T) {
	s := newTestServer(t)
	_, sessionA, keyA := selfHostedWorker(t, s, "ek-sess-a")
	_, sessionB, _ := selfHostedWorker(t, s, "ek-sess-b") // a second environment + session
	bearerA := map[string]string{"Authorization": "Bearer " + keyA}
	eventsA := "/v1/sessions/" + sessionA + "/events"

	// The env key lists its own session's events.
	if st, body := readJSON(t, s.doRaw(http.MethodGet, eventsA, nil, bearerA)); st != http.StatusOK {
		t.Fatalf("env-key list = %d, want 200 (body %v)", st, body)
	} else if _, ok := body["data"]; !ok {
		t.Errorf("env-key list missing data array: %v", body)
	}

	// The env key cannot post management-only events (such as user.message)
	// to its own session — refused 403 permission_error — and can still post
	// user.tool_result for an open built-in call.
	post := map[string]any{"events": []any{userMessage("hi from the worker")}}
	st, body := readJSON(t, s.doRaw(http.MethodPost, eventsA, post, bearerA))
	wantErrMsg(t, st, body, http.StatusForbidden, "permission_error", managementOnlyRefusal(0, "user.message"))

	useID := appendToolUse(t, s, sessionA, domain.EventAgentToolUse)
	toolPost := map[string]any{"events": []any{map[string]any{
		"type": "user.tool_result", "tool_use_id": useID,
		"content": []any{map[string]any{"type": "text", "text": "ok"}},
	}}}
	if st, body := readJSON(t, s.doRaw(http.MethodPost, eventsA, toolPost, bearerA)); st != http.StatusOK {
		t.Fatalf("env-key user.tool_result send = %d, want 200 (body %v)", st, body)
	}

	// A session in another environment is not-found (no cross-env read/write, and
	// cross-env existence never leaks).
	st, body = readJSON(t, s.doRaw(http.MethodGet, "/v1/sessions/"+sessionB+"/events", nil, bearerA))
	wantErr(t, st, body, http.StatusNotFound, "not_found_error")

	// So is a session that does not exist.
	st, body = readJSON(t, s.doRaw(http.MethodGet, "/v1/sessions/"+domain.NewID("sesn").String()+"/events", nil, bearerA))
	wantErr(t, st, body, http.StatusNotFound, "not_found_error")

	// The stream is covered by the same boundary: a cross-env stream is not-found
	// (rejected before the handler, so it does not hang).
	st, body = readJSON(t, s.doRaw(http.MethodGet, "/v1/sessions/"+sessionB+"/events/stream", nil, bearerA))
	wantErr(t, st, body, http.StatusNotFound, "not_found_error")

	// An invalid, empty, or missing credential is unauthorized.
	st, body = readJSON(t, s.doRaw(http.MethodGet, eventsA, nil, map[string]string{"Authorization": "Bearer nope"}))
	wantErr(t, st, body, http.StatusUnauthorized, "authentication_error")
	st, body = readJSON(t, s.doRaw(http.MethodGet, eventsA, nil, map[string]string{"Authorization": "Bearer "}))
	wantErr(t, st, body, http.StatusUnauthorized, "authentication_error")
	st, body = readJSON(t, s.doRaw(http.MethodGet, eventsA, nil, map[string]string{}))
	wantErr(t, st, body, http.StatusUnauthorized, "authentication_error")

	// The management x-api-key still authorizes session events (regression).
	if st, _ := readJSON(t, s.doRaw(http.MethodGet, eventsA, nil, map[string]string{"x-api-key": testKey})); st != http.StatusOK {
		t.Errorf("management list = %d, want 200", st)
	}

	// The env key also reads its own session (GET /v1/sessions/{id}) — the
	// reference worker's SetupSkills call — but not another environment's.
	if st, body := readJSON(t, s.doRaw(http.MethodGet, "/v1/sessions/"+sessionA, nil, bearerA)); st != http.StatusOK {
		t.Errorf("env-key session read = %d, want 200 (body %v)", st, body)
	} else if body["id"] != sessionA {
		t.Errorf("env-key session read id = %v, want %s", body["id"], sessionA)
	}
	st, body = readJSON(t, s.doRaw(http.MethodGet, "/v1/sessions/"+sessionB, nil, bearerA))
	wantErr(t, st, body, http.StatusNotFound, "not_found_error")

	// A valid management x-api-key is honored even alongside a stray Bearer
	// header: x-api-key present means a management caller, so the request is not
	// knocked onto environment-key auth.
	dual := map[string]string{"x-api-key": testKey, "Authorization": "Bearer " + keyA}
	if st, _ := readJSON(t, s.doRaw(http.MethodGet, eventsA, nil, dual)); st != http.StatusOK {
		t.Errorf("management list with stray Bearer = %d, want 200", st)
	}
}

// TestEnvironmentKeyDoesNotAuthorizeMutatingSessionCRUD pins the scope boundary:
// the environment key authorizes a session's worker-facing routes (the events
// subtree and the GET /v1/sessions/{id} read), never mutating CRUD or the
// collection — those stay management-only. A Bearer-only request to one falls to
// management auth and is rejected for the missing x-api-key. The bare GET is
// covered as authorized in TestEnvironmentKeyAuthorizesSessionEvents.
func TestEnvironmentKeyDoesNotAuthorizeMutatingSessionCRUD(t *testing.T) {
	s := newTestServer(t)
	_, sessionID, key := selfHostedWorker(t, s, "ek-crud")
	bearer := map[string]string{"Authorization": "Bearer " + key}

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/sessions"},                            // list sessions
		{http.MethodPost, "/v1/sessions/" + sessionID},              // update session
		{http.MethodDelete, "/v1/sessions/" + sessionID},            // delete session
		{http.MethodPost, "/v1/sessions/" + sessionID + "/archive"}, // archive session
	} {
		st, body := readJSON(t, s.doRaw(tc.method, tc.path, nil, bearer))
		wantErr(t, st, body, http.StatusUnauthorized, "authentication_error")
	}
}

// TestEnvironmentKeyEncodedSlashDoesNotForgeEventsPath pins the auth/routing
// path-confusion boundary. dispatchAuth classifies on the escaped path — the
// same representation ServeMux routes on — so a %2F cannot forge a session-events
// segment for the auth choice that the router does not also see. The decoded form
// of /v1/sessions/{id}%2Fevents looks like an events path (Bearer-authable), but
// ServeMux matches the single escaped {id} segment as /v1/sessions/{id} CRUD; the
// environment key must therefore fall to management auth and be rejected, never
// reach a management-only CRUD handler.
func TestEnvironmentKeyEncodedSlashDoesNotForgeEventsPath(t *testing.T) {
	s := newTestServer(t)
	_, sessionID, key := selfHostedWorker(t, s, "ek-encoded")
	bearer := map[string]string{"Authorization": "Bearer " + key}

	path := "/v1/sessions/" + sessionID + "%2Fevents"
	st, body := readJSON(t, s.doRaw(http.MethodPost, path, nil, bearer))
	wantErr(t, st, body, http.StatusUnauthorized, "authentication_error")
}

// TestEnvironmentKeyEncodedLiteralSegmentFailsClosed documents the safe side of
// the same boundary: because the worker lanes recognize a route only when its
// literal segments appear unencoded, a request that percent-encodes a literal
// segment (%65vents == "events") is NOT taken as a worker route — it falls to
// management auth and is denied (401), never over-authorized. Real clients always
// send the literal segments unencoded, so this fail-closed case has no driver;
// the test pins it as intended, guarding against a regression that would instead
// over-authorize the environment key onto an encoded path.
func TestEnvironmentKeyEncodedLiteralSegmentFailsClosed(t *testing.T) {
	s := newTestServer(t)
	_, sessionID, key := selfHostedWorker(t, s, "ek-enc-literal")
	bearer := map[string]string{"Authorization": "Bearer " + key}

	path := "/v1/sessions/" + sessionID + "/%65vents" // %65 == 'e'
	st, body := readJSON(t, s.doRaw(http.MethodGet, path, nil, bearer))
	wantErr(t, st, body, http.StatusUnauthorized, "authentication_error")
}

// TestWorkerConfirmationDoesNotEnqueueMCPExec is the end-to-end regression
// contract for self-hosted worker privilege escalation: neither a self_hosted
// environment key (sk-map-env01-...) nor a live per-item work token (wtk_...)
// may approve an always_ask MCP or sandbox tool call. Each attempt is refused
// 403 permission_error with nothing appended, the session stays idle, and no
// mcp_exec work is enqueued. A management key approving the same gated MCP
// call still succeeds and enqueues mcp_exec.
func TestWorkerConfirmationDoesNotEnqueueMCPExec(t *testing.T) {
	s := newTestServer(t)
	_, envID, sessionID, _, key := storeWorker(t, s, "mcp-gate")
	workID, _, token := pollItem(t, s, envID, key)
	if token == "" {
		t.Fatal("expected a live wtk_ work token on the poll")
	}
	if st := status(t, s, http.MethodPost, "/v1/environments/"+envID+"/work/"+workID+"/ack", nil, asBearer(key)); st != http.StatusOK {
		t.Fatalf("ack = %d", st)
	}

	mcpID := appendAskMCPToolUse(t, s, sessionID, "github", "create_issue")
	bashID := appendAskToolUse(t, s, sessionID, "bash")
	path := "/v1/sessions/" + sessionID + "/events"
	before := s.eventTypes(sessionID)

	creds := []struct {
		name    string
		headers map[string]string
	}{
		{"env key", asBearer(key)},
		{"work token", asBearer(token)},
	}
	for _, cred := range creds {
		for _, target := range []struct {
			label string
			id    string
		}{
			{"mcp", mcpID},
			{"sandbox", bashID},
		} {
			st, body := readJSON(t, s.doRaw(http.MethodPost, path, map[string]any{
				"events": []any{confirm(target.id, "allow", nil)},
			}, cred.headers))
			wantErrMsg(t, st, body, http.StatusForbidden, "permission_error", managementOnlyRefusal(0, "user.tool_confirmation"))
		}
	}

	if n := s.liveWork(sessionID, queue.MCPExec); n != 0 {
		t.Errorf("live mcp_exec after worker confirmations = %d, want 0", n)
	}
	if n := s.liveWork(sessionID, queue.ModelTurn); n != 0 {
		t.Errorf("live model_turn after worker confirmations = %d, want 0", n)
	}
	if got := s.sessionStatus(sessionID); got != "idle" {
		t.Errorf("status after worker confirmations = %q, want idle", got)
	}
	if got := s.eventTypes(sessionID); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Errorf("event log changed after refused worker confirmations: got %v, want %v", got, before)
	}

	// Control: the management key approving the same gated calls resumes the
	// session and enqueues mcp_exec.
	sendEvents(t, s, sessionID, confirm(mcpID, "allow", nil), confirm(bashID, "allow", nil))
	if got := s.sessionStatus(sessionID); got != "running" {
		t.Errorf("status after management approval = %q, want running", got)
	}
	if n := s.liveWork(sessionID, queue.MCPExec); n != 1 {
		t.Errorf("live mcp_exec after management approval = %d, want 1", n)
	}
}

// TestWorkerCredentialsRefusedManagementEvents pins the full credential
// partition on POST /v1/sessions/{id}/events for both self_hosted environment
// keys and live per-item work tokens:
//   - EnvironmentCredential cannot post user.tool_confirmation, user.message,
//     user.interrupt, user.define_outcome, user.custom_tool_result, or
//     system.message (alone, trailing a user.tool_result, or via JSON key/escape
//     tricks) -> 403 permission_error with nothing appended and no work enqueued.
//   - EnvironmentCredential can still post user.tool_result on self_hosted
//     sessions, and still cannot post it on cloud (404 at lane) or under a
//     management key (403 permission_error).
//   - ManagementCredential can still post every management-only event type.
func TestWorkerCredentialsRefusedManagementEvents(t *testing.T) {
	s := newTestServer(t)
	_, envID, sessionID, _, key := storeWorker(t, s, "worker-partition")
	workID, _, token := pollItem(t, s, envID, key)
	if token == "" {
		t.Fatal("expected a live wtk_ work token on the poll")
	}
	if st := status(t, s, http.MethodPost, "/v1/environments/"+envID+"/work/"+workID+"/ack", nil, asBearer(key)); st != http.StatusOK {
		t.Fatalf("ack = %d", st)
	}

	toolUseA := appendToolUse(t, s, sessionID, domain.EventAgentToolUse)
	toolUseB := appendToolUse(t, s, sessionID, domain.EventAgentToolUse)
	askID := appendAskToolUse(t, s, sessionID, "bash")
	customID := appendToolUse(t, s, sessionID, domain.EventAgentCustomToolUse)
	path := "/v1/sessions/" + sessionID + "/events"
	before := s.eventTypes(sessionID)

	toolResultA := map[string]any{
		"type": "user.tool_result", "tool_use_id": toolUseA,
		"content": []any{map[string]any{"type": "text", "text": "ok"}},
	}
	toolResultB := map[string]any{
		"type": "user.tool_result", "tool_use_id": toolUseB,
		"content": []any{map[string]any{"type": "text", "text": "ok"}},
	}
	mgmtEvents := []struct {
		typ string
		ev  map[string]any
	}{
		{"user.tool_confirmation", confirm(askID, "allow", nil)},
		{"user.tool_confirmation", confirm(askID, "deny", map[string]any{"deny_message": "no"})},
		{"user.message", userMessage("injected turn")},
		{"user.interrupt", map[string]any{"type": "user.interrupt"}},
		{"user.define_outcome", map[string]any{
			"type": "user.define_outcome", "description": "forged",
			"rubric": map[string]any{"type": "text", "content": "pass"},
		}},
		{"user.custom_tool_result", map[string]any{
			"type": "user.custom_tool_result", "custom_tool_use_id": customID,
			"content": []any{map[string]any{"type": "text", "text": "forged"}},
		}},
		{"system.message", map[string]any{
			"type": "system.message", "content": []any{map[string]any{"type": "text", "text": "injected system"}},
		}},
	}

	for _, cred := range []struct {
		name    string
		headers map[string]string
	}{
		{"env key", asBearer(key)},
		{"work token", asBearer(token)},
	} {
		// Single-event batches for every management-only type.
		for _, tc := range mgmtEvents {
			st, body := readJSON(t, s.doRaw(http.MethodPost, path, map[string]any{"events": []any{tc.ev}}, cred.headers))
			wantErrMsg(t, st, body, http.StatusForbidden, "permission_error", managementOnlyRefusal(0, tc.typ))
		}
		// Mixed batches: a valid user.tool_result ahead of a management-only
		// event (including system.message) rolls back whole.
		for _, tc := range mgmtEvents {
			st, body := readJSON(t, s.doRaw(http.MethodPost, path, map[string]any{"events": []any{toolResultA, tc.ev}}, cred.headers))
			wantErrMsg(t, st, body, http.StatusForbidden, "permission_error", managementOnlyRefusal(1, tc.typ))
		}
		// Raw JSON duplicate-key and escaped-key spellings cannot smuggle a
		// management event past the credential check.
		for _, raw := range []struct {
			name, body, typ string
		}{
			{"duplicate type key", `{"events":[{"type":"user.tool_result","type":"user.tool_confirmation","result":"allow","tool_use_id":"` + askID + `"}]}`, "user.tool_confirmation"},
			{"escaped type key", `{"events":[{"\u0074ype":"user.message","content":[{"type":"text","text":"hi"}]}]}`, "user.message"},
		} {
			st, body := readJSON(t, s.doRaw(http.MethodPost, path, raw.body, cred.headers))
			wantErrMsg(t, st, body, http.StatusForbidden, "permission_error", managementOnlyRefusal(0, raw.typ))
		}
	}

	if got := s.eventTypes(sessionID); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Errorf("log mutated after worker refusals: got %v, want %v", got, before)
	}
	if n := s.liveWork(sessionID, queue.ModelTurn); n != 0 {
		t.Errorf("live model_turn after worker refusals = %d, want 0", n)
	}

	// EnvironmentCredential can still post user.tool_result on self_hosted:
	// one under the environment key and one under the work token.
	echo := sendEventsAs(t, s, asBearer(key), sessionID, toolResultA)
	if echo[0]["type"] != "user.tool_result" || echo[0]["tool_use_id"] != toolUseA {
		t.Errorf("env-key tool_result echo = %v", echo[0])
	}
	echo = sendEventsAs(t, s, asBearer(token), sessionID, toolResultB)
	if echo[0]["type"] != "user.tool_result" || echo[0]["tool_use_id"] != toolUseB {
		t.Errorf("work-token tool_result echo = %v", echo[0])
	}

	// ManagementCredential still cannot post user.tool_result (403).
	st, body := readJSON(t, s.doRaw(http.MethodPost, path, map[string]any{"events": []any{toolResultA}}, map[string]string{"x-api-key": testKey}))
	wantErrMsg(t, st, body, http.StatusForbidden, "permission_error", toolResultRefusal(0))

	// ManagementCredential can still post every management-only event type.
	sendEvents(t, s, sessionID,
		confirm(askID, "deny", map[string]any{"deny_message": "nope"}),
		map[string]any{
			"type": "user.custom_tool_result", "custom_tool_use_id": customID,
			"content": []any{map[string]any{"type": "text", "text": "done"}},
		},
		map[string]any{"type": "user.interrupt"},
		map[string]any{
			"type": "user.define_outcome", "description": "check",
			"rubric": map[string]any{"type": "text", "content": "criteria"},
		},
		userMessage("management turn"),
		map[string]any{
			"type": "system.message", "content": []any{map[string]any{"type": "text", "text": "operator note"}},
		},
	)
}
