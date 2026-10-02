package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gateconfig"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// envKeyRoute is what a cloud environment's key gets on one route an
// environment key reaches. A route outside the work API answers its lane's
// refusal (cloudKeyRefusal, or the file download's recorded "Not found"); the
// work API keeps the answers recorded on the reference's cloud environments
// (workkind_test.go).
type envKeyRoute struct {
	query  string // appended to the path
	body   any
	status int
	// errType is the error envelope's type; "" for a success.
	errType string
	// refused marks the lane refusal, whose message is asserted too:
	// cloudKeyRefusal, or refusal where that is set.
	refused bool
	refusal string
	// selfHostedServed marks a route a self_hosted key is served on (200)
	// in this fixture, so the test also shows that key still works there.
	selfHostedServed bool
}

// cloudKeyRefusal is the message of the one refusal a key on a non-self_hosted
// environment gets on the session lane. The file content download refuses it
// with the reference's bare "Not found" instead (2026-09-03 batch2 idx 18
// `envkey.files.content-original`, idx 19 `envkey.files.content-session-copy`;
// #540).
func cloudKeyRefusal(envID string) string {
	return fmt.Sprintf("environment %s is not a self_hosted environment; only a self_hosted environment's key reaches its sessions and the files they mount", envID)
}

// TestACloudEnvironmentKeyReachesOnlyWhatTheReferenceServesIt pins what a key
// issued on a cloud environment reaches, now that the console issues one there
// (#820). Such an environment's work is the platform executor's, so the key
// has no worker to serve and must not act on that environment's sessions:
// approve an always_ask call, answer a custom tool, define an outcome, read
// the event stream, or download a mounted file a management key is refused.
// The skill reads stay served, as the reference was recorded serving them to a
// cloud environment's key (2026-09-05 batch2 idx 57, 59, 62).
//
// The routes are not listed from memory. Every registration in server.go is
// requested twice, with a self_hosted environment's key and with a cloud
// environment's: the routes the self_hosted key is not refused on with a 401
// are the ones an environment key reaches, and that set must be exactly the
// table below — so a route that joins an environment-key lane later fails here
// until someone decides what a cloud key gets on it. On each, the self_hosted
// key still works.
func TestACloudEnvironmentKeyReachesOnlyWhatTheReferenceServesIt(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	agentID := createAgent(t, s, map[string]any{"name": "lanes", "model": "claude-opus-4-8"})["id"].(string)
	skill := s.createSkill(t)
	oct := "application/octet-stream"

	// One environment of each kind, each with a session that mounts an upload
	// (downloadable=false, so only the worker lane serves its bytes), a queued
	// work item and a key issued on the console route.
	type world struct{ env, session, file, work, key string }
	provision := func(kind string) world {
		t.Helper()
		body := map[string]any{"name": "lanes-" + kind}
		if kind == "self_hosted" {
			body["config"] = map[string]any{"type": "self_hosted"}
		}
		envID := createEnvironment(t, s, body)["id"].(string)
		fileID := s.uploadFile(t, kind+".bin", &oct, "mounted by "+kind)["id"].(string)
		sessionID := createSession(t, s, map[string]any{
			"agent": agentID, "environment_id": envID,
			"resources": []any{map[string]any{"type": "file", "file_id": fileID}},
		})["id"].(string)
		if _, err := queue.New(s.pool).Enqueue(ctx, s.pool, domain.ID(envID), domain.ID(sessionID), queue.ToolExec); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		var workID string
		if err := s.pool.QueryRow(ctx, `SELECT id FROM work_items WHERE session_id = $1 AND kind = 'tool_exec'`, sessionID).Scan(&workID); err != nil {
			t.Fatalf("read the work item: %v", err)
		}
		return world{envID, sessionID, fileID, workID, issueViaConsole(t, s, envID, "lanes-"+kind)}
	}
	cloud, selfHosted := provision("cloud"), provision("self_hosted")

	refused := envKeyRoute{status: http.StatusNotFound, errType: "not_found_error", refused: true, selfHostedServed: true}
	served := envKeyRoute{status: http.StatusOK, selfHostedServed: true}
	workItem404 := envKeyRoute{status: http.StatusNotFound, errType: "not_found_error"}
	want := map[string]envKeyRoute{
		// The work API: the reference's recorded answers on a cloud
		// environment, and the work-item 404 on the item routes.
		"GET /v1/environments/{id}/work":           {status: http.StatusNotFound, errType: "not_found_error"},
		"GET /v1/environments/{id}/work/poll":      {status: http.StatusBadRequest, errType: "invalid_request_error"},
		"GET /v1/environments/{id}/work/stats":     {status: http.StatusOK},
		"GET /v1/environments/{id}/work/{work_id}": workItem404,
		"POST /v1/environments/{id}/work/{work_id}": {body: map[string]any{"metadata": map[string]any{"k": "v"}},
			status: http.StatusNotFound, errType: "not_found_error"},
		"POST /v1/environments/{id}/work/{work_id}/ack": workItem404,
		"POST /v1/environments/{id}/work/{work_id}/heartbeat": {query: "?expected_last_heartbeat=2026-09-05T00:00:00Z",
			status: http.StatusNotFound, errType: "not_found_error"},
		"POST /v1/environments/{id}/work/{work_id}/stop": {body: map[string]any{"force": true},
			status: http.StatusNotFound, errType: "not_found_error"},
		// The session lane and the file content download: refused.
		"GET /v1/sessions/{id}":        refused,
		"GET /v1/sessions/{id}/events": refused,
		"POST /v1/sessions/{id}/events": {body: map[string]any{"events": []any{userMessage("from a cloud key")}},
			status: http.StatusNotFound, errType: "not_found_error", refused: true, selfHostedServed: true},
		"GET /v1/sessions/{id}/events/stream": refused,
		"GET /v1/files/{id}/content": {status: http.StatusNotFound, errType: "not_found_error",
			refused: true, refusal: "Not found", selfHostedServed: true},
		// The skill reads: served, as recorded.
		"GET /v1/skills/{id}":                            served,
		"GET /v1/skills/{id}/versions":                   served,
		"GET /v1/skills/{id}/versions/{version}":         served,
		"GET /v1/skills/{id}/versions/{version}/content": served,
	}

	reached := map[string]bool{}
	for _, reg := range parseRoutes(t, "server.go") {
		if reg.isFunc {
			continue // a 404 or 405 closure, not a route
		}
		pattern := resolveRoutePattern(t, reg.pattern)
		method, template, _ := strings.Cut(pattern, " ")
		route := want[pattern]
		send := func(w world) (int, map[string]any) {
			t.Helper()
			path := fillRoute(template, w.env, w.session, w.file, w.work, skill["id"].(string), skill["latest_version_id"].(string))
			// Bounded, because the event stream holds its response open: the
			// status is all this needs, and it arrives with the headers.
			reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			res, err := s.roundTrip(reqCtx, method, path+route.query, route.body, asBearer(w.key))
			if err != nil {
				t.Fatalf("%s: %v", pattern, err)
			}
			defer res.Body.Close()
			if strings.HasSuffix(template, "/stream") && res.StatusCode == http.StatusOK {
				return res.StatusCode, nil
			}
			raw, _ := io.ReadAll(res.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			return res.StatusCode, body
		}

		shStatus, shBody := send(selfHosted)
		cloudStatus, cloudBody := send(cloud)
		if shStatus == http.StatusUnauthorized {
			// Not an environment-key route: the key is refused before anything
			// is routed, whatever its environment.
			if cloudStatus != http.StatusUnauthorized {
				t.Errorf("%s: a self_hosted key is refused with 401 but a cloud key gets %d %v", pattern, cloudStatus, cloudBody)
			}
			continue
		}
		reached[pattern] = true
		if _, listed := want[pattern]; !listed {
			t.Errorf("%s: an environment key reaches it (a self_hosted key got %d), and the table does not say what a cloud key gets there", pattern, shStatus)
			continue
		}

		t.Run(pattern, func(t *testing.T) {
			if route.errType == "" {
				if cloudStatus != route.status {
					t.Errorf("cloud key: status %d, body %v; want %d", cloudStatus, cloudBody, route.status)
				}
			} else {
				wantErr(t, cloudStatus, cloudBody, route.status, route.errType)
			}
			if route.refused {
				want := cloudKeyRefusal(cloud.env)
				if route.refusal != "" {
					want = route.refusal
				}
				if msg := envelopeMessage(cloudBody); msg != want {
					t.Errorf("cloud key: message %q; want the lane refusal %q", msg, want)
				}
			}
			// The self_hosted key still works on every lane the refusal
			// guards, and on the skill reads beside them.
			if route.selfHostedServed && shStatus != http.StatusOK {
				t.Errorf("self_hosted key: status %d, body %v; want 200", shStatus, shBody)
			}
			if envelopeMessage(shBody) == cloudKeyRefusal(selfHosted.env) {
				t.Errorf("self_hosted key: refused as a cloud key (%d %v)", shStatus, shBody)
			}
		})
	}

	var missing []string
	for pattern := range want {
		if !reached[pattern] {
			missing = append(missing, pattern)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the table lists routes no environment key reaches any more: %v", missing)
	}

	// Nothing the cloud key sent landed: no event on its session, and its
	// work item is as it was enqueued.
	var events int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'user.message'`, cloud.session).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 0 {
		t.Errorf("the cloud session holds %d user.message events after the cloud key's post; want none", events)
	}
	var state string
	if err := s.pool.QueryRow(ctx, `SELECT state FROM work_items WHERE id = $1`, cloud.work).Scan(&state); err != nil {
		t.Fatalf("read the cloud item: %v", err)
	}
	if state != "queued" {
		t.Errorf("cloud item state %q after the cloud key's requests; want queued", state)
	}
}

// TestACloudEnvironmentKeyCannotAnswerItsSessions names, one by one, the
// posts that made a cloud environment's key dangerous: on a cloud session
// parked on an always_ask call it can neither approve the call (the executor
// would then run it), answer a custom tool, nor define an outcome. Each is the
// lane's 404 and the log is untouched, so the call still waits for a caller
// allowed to answer it — and the management key's approval that follows
// resumes the session.
func TestACloudEnvironmentKeyCannotAnswerItsSessions(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	sid := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})["id"].(string)
	askID := appendAskToolUse(t, s, sid, "bash")
	bearer := asBearer(issueViaConsole(t, s, envID, "cloud-answers"))
	before := s.eventTypes(sid)

	for _, ev := range []map[string]any{
		confirm(askID, "allow", nil),
		{"type": "user.custom_tool_result", "custom_tool_use_id": askID,
			"content": []any{map[string]any{"type": "text", "text": "done"}}},
		{"type": "user.define_outcome", "description": "Grade it.",
			"rubric": map[string]any{"type": "text", "content": "It exists."}},
	} {
		st, res := readJSON(t, s.doRaw(http.MethodPost, "/v1/sessions/"+sid+"/events", map[string]any{"events": []any{ev}}, bearer))
		wantErrMsg(t, st, res, http.StatusNotFound, "not_found_error", cloudKeyRefusal(envID))
	}
	if got := s.eventTypes(sid); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Errorf("the log is %v after the cloud key's posts, want it unchanged at %v", got, before)
	}
	if got := s.sessionStatus(sid); got != "idle" {
		t.Errorf("status after the cloud key's posts = %q, want idle, still waiting on the confirmation", got)
	}
	sendEvents(t, s, sid, confirm(askID, "allow", nil))
	if got := s.sessionStatus(sid); got != "running" {
		t.Errorf("status after the management key's approval = %q, want running", got)
	}
}

// TestAnArchivedSelfHostedEnvironmentKeyKeepsItsLanes: archival is not the
// kind gate. An archived self_hosted environment's key still reads its
// sessions, their events, its mounted files and the skills, as before #820,
// so a worker can finish what the archive left in flight.
func TestAnArchivedSelfHostedEnvironmentKeyKeepsItsLanes(t *testing.T) {
	s := newTestServer(t)
	oct := "application/octet-stream"
	envID, _, key := selfHostedWorker(t, s, "archived-lanes")
	agentID := createAgent(t, s, map[string]any{"name": "archived-lanes", "model": "claude-opus-4-8"})["id"].(string)
	fileID := s.uploadFile(t, "archived.bin", &oct, "still mine")["id"].(string)
	sessionID := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{map[string]any{"type": "file", "file_id": fileID}},
	})["id"].(string)
	skill := s.createSkill(t)
	if status, body := s.do(http.MethodPost, "/v1/environments/"+envID+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive: status %d, body %v", status, body)
	}

	for _, path := range []string{
		"/v1/sessions/" + sessionID,
		"/v1/sessions/" + sessionID + "/events",
		"/v1/files/" + fileID + "/content",
		"/v1/skills/" + skill["id"].(string) + "/versions/" + skill["latest_version_id"].(string) + "/content",
	} {
		res := s.doRaw(http.MethodGet, path, nil, asBearer(key))
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s with an archived self_hosted environment's key: status %d; want 200", path, res.StatusCode)
		}
	}
}

// envelopeMessage is an error envelope's message, or "" for any other body.
func envelopeMessage(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	msg, _ := e["message"].(string)
	return msg
}

// consoleRoutePaths spells out the path constants server.go registers the
// console namespaces and the gate config under, so the enumeration above
// requests them too. An unknown name fails the test rather than going
// unrequested.
var consoleRoutePaths = map[string]string{
	"consoleTokensPath":    "/api/oauth/organizations/default/environments/{id}/tokens",
	"consoleRevokePath":    "/api/oauth/organizations/default/environments/{id}/tokens/{token_id}/revoke",
	"consoleWorkspacePath": "/api/console/organizations/default/workspaces/default",
	"consoleAPIKeysPath":   "/api/console/organizations/default/workspaces/default/api_keys",
	"consoleAPIKeyPath":    "/api/console/organizations/default/workspaces/default/api_keys/{key_id}",
	"consoleOrgAPIKeyPath": "/api/console/organizations/default/api_keys/{key_id}",
	"gateconfig.Path":      gateconfig.Path,
}

// resolveRoutePattern turns a rendered registration (`GET /v1/agents`, or
// `POST +consoleTokensPath`) into a method and a path template.
func resolveRoutePattern(t *testing.T, rendered string) string {
	t.Helper()
	method, rest, ok := strings.Cut(rendered, " ")
	if !ok {
		t.Fatalf("registration %q has no method", rendered)
	}
	if name, isConst := strings.CutPrefix(rest, "+"); isConst {
		path, known := consoleRoutePaths[name]
		if !known {
			t.Fatalf("registration %q names a path constant this test does not know; add it to consoleRoutePaths", rendered)
		}
		return method + " " + path
	}
	return rendered
}

// fillRoute fills a path template's wildcards with a world's ids: the
// collection a wildcard follows decides which id it is, and a wildcard on a
// management-only route gets a placeholder, since the key never gets past the
// dispatcher there.
func fillRoute(template, envID, sessionID, fileID, workID, skillID, versionID string) string {
	segs := strings.Split(template, "/")
	for i, seg := range segs {
		if !strings.HasPrefix(seg, "{") {
			continue
		}
		switch {
		case seg == "{work_id}":
			segs[i] = workID
		case seg == "{version}":
			segs[i] = versionID
		case seg == "{id}" && segs[i-1] == "environments":
			segs[i] = envID
		case seg == "{id}" && segs[i-1] == "sessions":
			segs[i] = sessionID
		case seg == "{id}" && segs[i-1] == "files":
			segs[i] = fileID
		case seg == "{id}" && segs[i-1] == "skills":
			segs[i] = skillID
		default:
			segs[i] = "placeholder_01"
		}
	}
	return strings.Join(segs, "/")
}
