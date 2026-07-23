package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// selfHostedWorker provisions a self-hosted environment with a session and a
// live worker (Bearer) key, returning the environment id, the session id, and
// the key value the worker authenticates with.
func selfHostedWorker(t *testing.T, s *tserver, key string) (envID, sessionID string) {
	t.Helper()
	agent := createAgent(t, s, map[string]any{"name": "w", "model": "claude-opus-4-8"})
	env := createEnvironment(t, s, map[string]any{"name": "wh", "config": map[string]any{"type": "self_hosted"}})
	envID = env["id"].(string)
	sess := createSession(t, s, map[string]any{"agent": agent["id"], "environment_id": envID})
	sessionID = sess["id"].(string)
	if err := api.EnsureEnvironmentKey(context.Background(), s.pool, envID, key); err != nil {
		t.Fatalf("EnsureEnvironmentKey: %v", err)
	}
	return envID, sessionID
}

func (s *tserver) poll(t *testing.T, envID string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	res := s.doRaw(http.MethodGet, "/v1/environments/"+envID+"/work/poll", nil, headers)
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return res, string(raw)
}

// TestWorkPollRequiresEnvironmentKey pins the worker-auth boundary: the work
// API takes an Authorization: Bearer environment key, never the management
// x-api-key, and a key is scoped to exactly one environment.
func TestWorkPollRequiresEnvironmentKey(t *testing.T) {
	s := newTestServer(t)
	const key = "ek-worker-1"
	envID, _ := selfHostedWorker(t, s, key)
	otherEnv, _ := selfHostedWorker(t, s, "ek-worker-2")

	cases := map[string]struct {
		env     string
		headers map[string]string
	}{
		"missing Authorization": {envID, map[string]string{}},
		"management key only":   {envID, map[string]string{"x-api-key": testKey}},
		"invalid bearer":        {envID, map[string]string{"Authorization": "Bearer nope"}},
		"empty bearer":          {envID, map[string]string{"Authorization": "Bearer "}},
		"key for other env":     {otherEnv, map[string]string{"Authorization": "Bearer " + key}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			res, raw := s.poll(t, tc.env, tc.headers)
			var body map[string]any
			_ = json.Unmarshal([]byte(raw), &body)
			wantErr(t, res.StatusCode, body, http.StatusUnauthorized, "authentication_error")
			if res.Header.Get("request-id") == "" {
				t.Error("work-API error responses must carry a request-id header")
			}
		})
	}
}

// TestWorkPollEmptyQueueReturnsNull pins the empty-poll shape: an authenticated
// poll of an idle queue is 200 with a null body, which the reference client
// reads as "no work" and spaces with its own jitter sleep.
func TestWorkPollEmptyQueueReturnsNull(t *testing.T) {
	s := newTestServer(t)
	const key = "ek-empty"
	envID, _ := selfHostedWorker(t, s, key)

	res, raw := s.poll(t, envID, map[string]string{"Authorization": "Bearer " + key})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("empty poll status = %d, want 200 (body %q)", res.StatusCode, raw)
	}
	if strings.TrimSpace(raw) != "null" {
		t.Fatalf("empty poll body = %q, want null", raw)
	}
}

// TestWorkPollReturnsWireShape pins the BetaSelfHostedWork response: a queued
// tool_exec item is handed out with every required field present, its data a
// reference to the session the worker attaches to, and its state still queued
// (ack, not poll, transitions it).
func TestWorkPollReturnsWireShape(t *testing.T) {
	s := newTestServer(t)
	const key = "ek-shape"
	envID, sessionID := selfHostedWorker(t, s, key)

	q := queue.New(s.pool)
	if _, err := q.Enqueue(context.Background(), s.pool, domain.ID(envID), domain.ID(sessionID), queue.ToolExec); err != nil {
		t.Fatalf("enqueue tool_exec: %v", err)
	}

	res, raw := s.poll(t, envID, map[string]string{"Authorization": "Bearer " + key})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("poll status = %d, want 200 (body %q)", res.StatusCode, raw)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode work: %v (body %q)", err, raw)
	}
	wantFields(t, body,
		"id", "acknowledged_at", "created_at", "data", "environment_id",
		"latest_heartbeat_at", "metadata", "secret", "started_at", "state",
		"stop_requested_at", "stopped_at", "type")

	if id, _ := body["id"].(string); !domain.ID(id).HasPrefix("work") {
		t.Errorf("work id = %v, want work_-prefixed", body["id"])
	}
	if body["environment_id"] != envID {
		t.Errorf("environment_id = %v, want %s", body["environment_id"], envID)
	}
	if body["type"] != "work" {
		t.Errorf("type = %v, want work", body["type"])
	}
	if body["state"] != "queued" {
		t.Errorf("state = %v, want queued (ack transitions it)", body["state"])
	}
	// secret is present but always null: the credential payload it carries in the
	// reference needs vaults, which v1 does not implement (#50).
	if body["secret"] != nil {
		t.Errorf("secret = %v, want null", body["secret"])
	}
	// A still-queued item has reached none of the lifecycle timestamps.
	for _, k := range []string{"acknowledged_at", "started_at", "stop_requested_at", "stopped_at", "latest_heartbeat_at"} {
		if body[k] != nil {
			t.Errorf("%s = %v, want null for a queued item", k, body[k])
		}
	}
	data, _ := body["data"].(map[string]any)
	if data == nil {
		t.Fatalf("data missing or not an object: %v", body["data"])
	}
	if data["id"] != sessionID {
		t.Errorf("data.id = %v, want the session id %s", data["id"], sessionID)
	}
	if data["type"] != "session" {
		t.Errorf("data.type = %v, want session", data["type"])
	}
}

// TestWorkPollEmitsTraceContextHeader pins the enqueue→poll→worker tracing leg:
// an item enqueued under an active span is handed out with that span's W3C trace
// context in the response headers, so the worker can parent its tool-execution
// spans on the enqueuing turn. The wire body stays clean — the trace context
// rides a header and never leaks into the client-facing metadata namespace.
func TestWorkPollEmitsTraceContextHeader(t *testing.T) {
	s := newTestServer(t)
	const key = "ek-trace"
	envID, sessionID := selfHostedWorker(t, s, key)

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19},
		SpanID:     trace.SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	q := queue.New(s.pool)
	if _, err := q.Enqueue(ctx, s.pool, domain.ID(envID), domain.ID(sessionID), queue.ToolExec); err != nil {
		t.Fatalf("enqueue under span: %v", err)
	}

	res, raw := s.poll(t, envID, map[string]string{"Authorization": "Bearer " + key})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("poll status = %d, want 200 (body %q)", res.StatusCode, raw)
	}
	want := fmt.Sprintf("00-%s-%s-01", sc.TraceID(), sc.SpanID())
	if got := res.Header.Get("traceparent"); got != want {
		t.Errorf("poll response traceparent = %q, want %q", got, want)
	}
	// The trace context must not surface in the wire body: neither a leaked
	// trace_context field nor a polluted metadata namespace.
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode work: %v (body %q)", err, raw)
	}
	if _, ok := body["trace_context"]; ok {
		t.Error("trace_context leaked into the wire work body")
	}
	if meta, _ := body["metadata"].(map[string]any); len(meta) != 0 {
		t.Errorf("metadata = %v, want empty (trace context must not pollute it)", meta)
	}
}

// TestWorkPollRejectsWrongMethodAndPath pins the wire error envelope on the
// work subtree: a known route with a truly unhandled method is 405, an unknown
// work path is 404 — both authenticated first. A POST to .../work/poll is NOT a
// 405: it routes to the metadata update as work_id="poll". With a valid patch
// body it 404s on the nonexistent "poll" item (as the reference's own POST
// .../work/{work_id} does); with an empty body it is a 400, because body
// validation (metadata is required) precedes the item lookup — so the not-found
// contract holds only for a valid patch.
func TestWorkPollRejectsWrongMethodAndPath(t *testing.T) {
	s := newTestServer(t)
	const key = "ek-route"
	envID, _ := selfHostedWorker(t, s, key)
	auth := map[string]string{"Authorization": "Bearer " + key}

	res := s.doRaw(http.MethodPut, "/v1/environments/"+envID+"/work/poll", nil, auth)
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	wantErr(t, res.StatusCode, body, http.StatusMethodNotAllowed, "invalid_request_error")

	res = s.doRaw(http.MethodGet, "/v1/environments/"+envID+"/work/bogus", nil, auth)
	raw, _ = io.ReadAll(res.Body)
	res.Body.Close()
	body = nil
	_ = json.Unmarshal(raw, &body)
	wantErr(t, res.StatusCode, body, http.StatusNotFound, "not_found_error")

	// POST .../work/poll with a valid metadata body 404s on the nonexistent
	// "poll" item — the documented routing-collision outcome.
	res = s.doRaw(http.MethodPost, "/v1/environments/"+envID+"/work/poll",
		map[string]any{"metadata": map[string]any{"a": "1"}}, auth)
	raw, _ = io.ReadAll(res.Body)
	res.Body.Close()
	body = nil
	_ = json.Unmarshal(raw, &body)
	wantErr(t, res.StatusCode, body, http.StatusNotFound, "not_found_error")

	// POST .../work/poll with an empty body is a 400 (metadata required), not a
	// 404: validation runs before the item lookup.
	res = s.doRaw(http.MethodPost, "/v1/environments/"+envID+"/work/poll", nil, auth)
	raw, _ = io.ReadAll(res.Body)
	res.Body.Close()
	body = nil
	_ = json.Unmarshal(raw, &body)
	wantErr(t, res.StatusCode, body, http.StatusBadRequest, "invalid_request_error")
}

// TestWorkUpdateMetadata pins the metadata patch endpoint (POST .../work/{work_id}):
// a string value upserts, an explicit null deletes, the response is the updated
// BetaSelfHostedWork, and the endpoint is scoped, authed, and validated like the
// rest of the work API.
func TestWorkUpdateMetadata(t *testing.T) {
	s := newTestServer(t)
	const key = "ek-meta"
	envID, sessionID := selfHostedWorker(t, s, key)
	workID := s.enqueueAndPoll(t, envID, sessionID, key)
	path := "/v1/environments/" + envID + "/work/" + workID

	// Upsert two keys; the response is the updated work object with the metadata.
	res, body, raw := s.workReq(t, http.MethodPost, path, key, map[string]any{
		"metadata": map[string]any{"a": "1", "b": "2"},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d, want 200 (body %q)", res.StatusCode, raw)
	}
	if body["type"] != "work" || body["id"] != workID {
		t.Errorf("update returned %v, want the work object", body)
	}
	md, _ := body["metadata"].(map[string]any)
	if md["a"] != "1" || md["b"] != "2" {
		t.Errorf("metadata after upsert = %v, want a=1 b=2", md)
	}

	// A mixed patch: upsert a, delete b (explicit null), add c.
	_, body, _ = s.workReq(t, http.MethodPost, path, key, map[string]any{
		"metadata": map[string]any{"a": "9", "b": nil, "c": "3"},
	})
	md, _ = body["metadata"].(map[string]any)
	if len(md) != 2 || md["a"] != "9" || md["c"] != "3" {
		t.Errorf("metadata after mixed patch = %v, want a=9 c=3 (b deleted)", md)
	}

	// An empty-string value is a LITERAL upsert, NOT a delete — the work rule is
	// emptyDeletes=false (unlike the environment rule, where "" also deletes). This
	// pins the semantic so a future flip of the flag can't silently start dropping
	// keys whose value is "".
	_, body, _ = s.workReq(t, http.MethodPost, path, key, map[string]any{
		"metadata": map[string]any{"c": ""},
	})
	md, _ = body["metadata"].(map[string]any)
	if len(md) != 2 || md["a"] != "9" {
		t.Errorf("metadata after empty-string patch = %v, want a=9 kept", md)
	}
	if v, ok := md["c"]; !ok || v != "" {
		t.Errorf("empty-string value: c = %v (present=%v), want stored as \"\" (literal, not deleted)", v, ok)
	}

	// Validation: a body with no metadata field is 400 (the wire marks it required).
	res, body, _ = s.workReq(t, http.MethodPost, path, key, map[string]any{})
	wantErr(t, res.StatusCode, body, http.StatusBadRequest, "invalid_request_error")

	// An unknown top-level field is rejected, matching the reference's strict
	// parameter validation and every other update endpoint here — a typo'd field
	// must not vanish into accepted-but-ignored input.
	res, body, _ = s.workReq(t, http.MethodPost, path, key, map[string]any{
		"metadata": map[string]any{"a": "1"}, "metadate": map[string]any{"b": "2"},
	})
	wantErr(t, res.StatusCode, body, http.StatusBadRequest, "invalid_request_error")

	// A non-string, non-null metadata value is 400.
	res, body, _ = s.workReq(t, http.MethodPost, path, key, map[string]any{"metadata": map[string]any{"a": 5}})
	wantErr(t, res.StatusCode, body, http.StatusBadRequest, "invalid_request_error")

	// Scoping: an unknown work id is 404 (as POST .../work/poll also is).
	res, body, _ = s.workReq(t, http.MethodPost, "/v1/environments/"+envID+"/work/work_nope", key, map[string]any{
		"metadata": map[string]any{"a": "1"},
	})
	wantErr(t, res.StatusCode, body, http.StatusNotFound, "not_found_error")

	// Auth: the management key (no Bearer) is rejected on the work API.
	res2 := s.doRaw(http.MethodPost, path, map[string]any{"metadata": map[string]any{"a": "1"}},
		map[string]string{"x-api-key": testKey})
	res2.Body.Close()
	if res2.StatusCode != http.StatusUnauthorized {
		t.Errorf("management key on work update = %d, want 401", res2.StatusCode)
	}
}

// TestWorkPollClampsHugeReclaim pins that an over-large reclaim_older_than_ms is
// clamped rather than overflowing time.Duration into a past reservation: after
// a poll hands out the item, an immediate second poll must still see it reserved
// (null), not re-hand it out.
func TestWorkPollClampsHugeReclaim(t *testing.T) {
	s := newTestServer(t)
	const key = "ek-clamp"
	envID, sessionID := selfHostedWorker(t, s, key)
	q := queue.New(s.pool)
	if _, err := q.Enqueue(context.Background(), s.pool, domain.ID(envID), domain.ID(sessionID), queue.ToolExec); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	auth := map[string]string{"Authorization": "Bearer " + key}
	path := "/v1/environments/" + envID + "/work/poll?reclaim_older_than_ms=9223372036854775807"
	res := s.doRaw(http.MethodGet, path, nil, auth)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("first poll status = %d, want 200", res.StatusCode)
	}
	// If the huge value had overflowed into a negative lease, the reservation
	// would already be in the past and this poll would re-hand-out the item.
	res2, raw2 := s.poll(t, envID, auth)
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("second poll status = %d, want 200", res2.StatusCode)
	}
	if strings.TrimSpace(raw2) != "null" {
		t.Fatalf("second poll re-handed-out a reserved item (reclaim overflow not clamped): %q", raw2)
	}
}

// enqueueAndPoll enqueues a tool_exec item for the session and polls it out over
// HTTP as the worker would, returning its work id.
func (s *tserver) enqueueAndPoll(t *testing.T, envID, sessionID, key string) string {
	t.Helper()
	q := queue.New(s.pool)
	if _, err := q.Enqueue(context.Background(), s.pool, domain.ID(envID), domain.ID(sessionID), queue.ToolExec); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	res, raw := s.poll(t, envID, map[string]string{"Authorization": "Bearer " + key})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("poll status = %d (body %q)", res.StatusCode, raw)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode poll: %v (body %q)", err, raw)
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("poll returned no work id: %q", raw)
	}
	return id
}

// workReq issues an authenticated work-API request and returns the response, the
// decoded JSON object (nil when the body is not a JSON object, e.g. a 204), and
// the raw body.
func (s *tserver) workReq(t *testing.T, method, path, key string, body any) (*http.Response, map[string]any, string) {
	t.Helper()
	res := s.doRaw(method, path, body, map[string]string{"Authorization": "Bearer " + key})
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	var obj map[string]any
	_ = json.Unmarshal(raw, &obj)
	return res, obj, string(raw)
}

// TestWorkGetReturnsItem pins GET .../work/{work_id}: it returns the full
// BetaSelfHostedWork wire shape for a polled (still-queued) item, and 404 for an
// unknown id.
func TestWorkGetReturnsItem(t *testing.T) {
	s := newTestServer(t)
	const key = "ek-get"
	envID, sessionID := selfHostedWorker(t, s, key)
	workID := s.enqueueAndPoll(t, envID, sessionID, key)

	res, body, raw := s.workReq(t, http.MethodGet, "/v1/environments/"+envID+"/work/"+workID, key, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200 (body %q)", res.StatusCode, raw)
	}
	wantFields(t, body,
		"id", "acknowledged_at", "created_at", "data", "environment_id",
		"latest_heartbeat_at", "metadata", "secret", "started_at", "state",
		"stop_requested_at", "stopped_at", "type")
	if body["id"] != workID || body["state"] != "queued" || body["type"] != "work" {
		t.Errorf("get = %v, want id %s / queued / work", body, workID)
	}

	res, body, _ = s.workReq(t, http.MethodGet, "/v1/environments/"+envID+"/work/"+domain.NewID("work").String(), key, nil)
	wantErr(t, res.StatusCode, body, http.StatusNotFound, "not_found_error")
}

// TestWorkAckTransitionsToStarting pins POST .../work/{work_id}/ack: it advances
// a polled item queued → starting, stamps acknowledged_at, and echoes the wire
// item.
func TestWorkAckTransitionsToStarting(t *testing.T) {
	s := newTestServer(t)
	const key = "ek-ack"
	envID, sessionID := selfHostedWorker(t, s, key)
	workID := s.enqueueAndPoll(t, envID, sessionID, key)

	res, body, raw := s.workReq(t, http.MethodPost, "/v1/environments/"+envID+"/work/"+workID+"/ack", key, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("ack status = %d, want 200 (body %q)", res.StatusCode, raw)
	}
	if body["state"] != "starting" {
		t.Errorf("state after ack = %v, want starting", body["state"])
	}
	if body["acknowledged_at"] == nil {
		t.Error("acknowledged_at not set after ack")
	}
	if body["type"] != "work" {
		t.Errorf("type = %v, want work", body["type"])
	}
}

// TestWorkHeartbeatClaimsLeaseAndExtends pins POST .../work/{work_id}/heartbeat:
// the wire heartbeat response shape, the NO_HEARTBEAT claim (→ active), the
// echo-to-extend round trip with desired_ttl_seconds clamping, a missing
// precondition (400), and a mismatch (412).
func TestWorkHeartbeatClaimsLeaseAndExtends(t *testing.T) {
	s := newTestServer(t)
	const key = "ek-hb"
	envID, sessionID := selfHostedWorker(t, s, key)
	workID := s.enqueueAndPoll(t, envID, sessionID, key)
	base := "/v1/environments/" + envID + "/work/" + workID + "/heartbeat"
	if _, _, raw := s.workReq(t, http.MethodPost, "/v1/environments/"+envID+"/work/"+workID+"/ack", key, nil); raw == "" {
		t.Fatal("ack returned empty body")
	}

	// First heartbeat claims the lease: starting → active, full wire shape.
	res, body, raw := s.workReq(t, http.MethodPost, base+"?expected_last_heartbeat=NO_HEARTBEAT", key, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("claim heartbeat status = %d, want 200 (body %q)", res.StatusCode, raw)
	}
	wantFields(t, body, "last_heartbeat", "lease_extended", "state", "ttl_seconds", "type")
	if body["state"] != "active" || body["lease_extended"] != true || body["type"] != "work_heartbeat" {
		t.Errorf("claim heartbeat = %v, want active/extended/work_heartbeat", body)
	}
	if body["ttl_seconds"].(float64) != 30 {
		t.Errorf("ttl_seconds = %v, want 30 (default)", body["ttl_seconds"])
	}
	prev, _ := body["last_heartbeat"].(string)
	if prev == "" {
		t.Fatal("claim heartbeat returned no last_heartbeat to echo")
	}

	// Echo the server's value to extend; an over-large TTL is clamped to the max.
	res, body, raw = s.workReq(t, http.MethodPost, base+"?expected_last_heartbeat="+url.QueryEscape(prev)+"&desired_ttl_seconds=100000", key, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("extend heartbeat status = %d, want 200 (body %q)", res.StatusCode, raw)
	}
	if body["lease_extended"] != true || body["ttl_seconds"].(float64) != 300 {
		t.Errorf("extend heartbeat = %v, want extended with ttl clamped to 300", body)
	}

	// A missing precondition is a 400; the superseded value is now a 412.
	res, body, _ = s.workReq(t, http.MethodPost, base, key, nil)
	wantErr(t, res.StatusCode, body, http.StatusBadRequest, "invalid_request_error")
	res, body, _ = s.workReq(t, http.MethodPost, base+"?expected_last_heartbeat="+url.QueryEscape(prev), key, nil)
	wantErr(t, res.StatusCode, body, http.StatusPreconditionFailed, "invalid_request_error")
}

// TestWorkStopGracefulThenForce pins POST .../work/{work_id}/stop: success is a
// bodiless 204 with no JSON Content-Type — the reference service sends no body
// even though the generated SDK method is typed *BetaSelfHostedWork, which is
// exactly why its work poller bypasses the strict decoder (anthropic-sdk-go
// lib/environments/poller.go, stopWork). The resulting state is read back with
// GET: a graceful stop moves the item to stopping, re-stopping a stopping item
// is 409, and force escalates it to stopped.
func TestWorkStopGracefulThenForce(t *testing.T) {
	s := newTestServer(t)
	const key = "ek-stop"
	envID, sessionID := selfHostedWorker(t, s, key)
	workID := s.enqueueAndPoll(t, envID, sessionID, key)
	stop := "/v1/environments/" + envID + "/work/" + workID + "/stop"
	get := "/v1/environments/" + envID + "/work/" + workID

	wantNoContent(t, s, stop, key, nil)
	_, body, _ := s.workReq(t, http.MethodGet, get, key, nil)
	if body["type"] != "work" || body["state"] != "stopping" || body["stop_requested_at"] == nil {
		t.Errorf("after graceful stop GET returned %v, want a work object in state stopping", body)
	}

	// Re-graceful-stopping a stopping item is a conflict — errors keep the JSON envelope.
	res, body, _ := s.workReq(t, http.MethodPost, stop, key, nil)
	wantErr(t, res.StatusCode, body, http.StatusConflict, "invalid_request_error")

	// force escalates stopping → stopped, again with no response body.
	wantNoContent(t, s, stop, key, map[string]any{"force": true})
	_, body, _ = s.workReq(t, http.MethodGet, get, key, nil)
	if body["state"] != "stopped" || body["stopped_at"] == nil {
		t.Errorf("after force stop GET returned %v, want stopped with stopped_at", body)
	}
}

// wantNoContent posts a stop and asserts the wire's success shape: 204, zero
// body bytes, and no Content-Type header at all (not merely a non-JSON one) —
// the strict Go decoder in the reference SDK keys off exactly that absence.
func wantNoContent(t *testing.T, s *tserver, path, key string, reqBody map[string]any) {
	t.Helper()
	res, _, raw := s.workReq(t, http.MethodPost, path, key, reqBody)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("stop status = %d, want 204 (body %q)", res.StatusCode, raw)
	}
	if len(raw) != 0 {
		t.Errorf("stop returned %d body bytes (%q), want an empty body", len(raw), raw)
	}
	// Check the header map directly: Header.Get returns "" for an absent header
	// and for a present-but-empty one alike, so it cannot prove absence.
	if ct, ok := res.Header["Content-Type"]; ok {
		t.Errorf("stop set Content-Type %q, want the header absent entirely", ct)
	}
}

// TestWorkLifecycleRoutesScopeAndMethod pins that the new lifecycle routes share
// the work-API auth boundary — a key scoped to another environment is rejected
// (401) — and that a known route reached with the wrong method is the wire 405.
func TestWorkLifecycleRoutesScopeAndMethod(t *testing.T) {
	s := newTestServer(t)
	const key = "ek-scope"
	envID, sessionID := selfHostedWorker(t, s, key)
	selfHostedWorker(t, s, "ek-scope-other") // a second env whose key must not reach envID
	workID := s.enqueueAndPoll(t, envID, sessionID, key)
	base := "/v1/environments/" + envID + "/work/" + workID

	// A key valid only for the other environment cannot act on envID's work item.
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, base},
		{http.MethodPost, base + "/ack"},
		{http.MethodPost, base + "/heartbeat?expected_last_heartbeat=NO_HEARTBEAT"},
		{http.MethodPost, base + "/stop"},
	} {
		res, body, _ := s.workReq(t, tc.method, tc.path, "ek-scope-other", nil)
		wantErr(t, res.StatusCode, body, http.StatusUnauthorized, "authentication_error")
	}

	// Wrong method on a known lifecycle route is the wire 405, not a 404.
	for _, tc := range []struct{ method, path string }{
		{http.MethodDelete, base},
		{http.MethodGet, base + "/ack"},
		{http.MethodGet, base + "/heartbeat"},
		{http.MethodGet, base + "/stop"},
	} {
		res, body, _ := s.workReq(t, tc.method, tc.path, key, nil)
		wantErr(t, res.StatusCode, body, http.StatusMethodNotAllowed, "invalid_request_error")
	}
}

// TestUnauthenticatedRequestsAreNotRedirectedBeforeAuth pins that auth runs
// before any ServeMux redirect: an unauthenticated request to a subtree-root or
// a path-cleanable URL gets the 401 wire envelope, never a bare 3xx redirect.
func TestUnauthenticatedRequestsAreNotRedirectedBeforeAuth(t *testing.T) {
	s := newTestServer(t)
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	for _, path := range []string{
		"/v1/environments/env_x/work", // work subtree root, no trailing slash
		"/v1//agents",                 // path-cleaning redirect candidate
	} {
		req, err := http.NewRequest(http.MethodGet, s.url+path, nil)
		if err != nil {
			t.Fatalf("new request %s: %v", path, err)
		}
		res, err := noFollow.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("unauthenticated %s: status %d, want 401 (auth must precede any redirect)", path, res.StatusCode)
		}
	}
}

// TestWorkStats pins the work-queue stats endpoint (GET .../work/stats): the wire
// shape (BetaSelfHostedWorkQueueStats), the depth→pending transition a reserving
// poll drives, and the workers_polling integration — a poll carrying an
// Anthropic-Worker-ID is counted, a header-less poll is not. Scoped and authed
// like the rest of the work API.
func TestWorkStats(t *testing.T) {
	s := newTestServer(t)
	const key = "ek-stats"
	envID, sessionID := selfHostedWorker(t, s, key)
	auth := map[string]string{"Authorization": "Bearer " + key}
	statsPath := "/v1/environments/" + envID + "/work/stats"

	getStats := func() map[string]any {
		t.Helper()
		res := s.doRaw(http.MethodGet, statsPath, nil, auth)
		raw, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("stats status = %d (body %q)", res.StatusCode, raw)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decode stats: %v (body %q)", err, raw)
		}
		return body
	}

	// Empty queue: full wire shape, all counts zero, oldest null.
	body := getStats()
	wantFields(t, body, "depth", "oldest_queued_at", "pending", "type", "workers_polling")
	if body["type"] != "work_queue_stats" {
		t.Errorf("type = %v, want work_queue_stats", body["type"])
	}
	if body["depth"] != float64(0) || body["pending"] != float64(0) || body["workers_polling"] != float64(0) {
		t.Errorf("empty stats = %v, want zeros", body)
	}
	if body["oldest_queued_at"] != nil {
		t.Errorf("oldest_queued_at = %v, want null on an empty queue", body["oldest_queued_at"])
	}

	// Enqueue a tool_exec item → depth 1, oldest set.
	q := queue.New(s.pool)
	if _, err := q.Enqueue(context.Background(), s.pool, domain.ID(envID), domain.ID(sessionID), queue.ToolExec); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	body = getStats()
	if body["depth"] != float64(1) || body["pending"] != float64(0) {
		t.Errorf("after enqueue = depth %v pending %v, want 1/0", body["depth"], body["pending"])
	}
	if body["oldest_queued_at"] == nil {
		t.Error("oldest_queued_at is null, want the queued item's timestamp")
	}

	// Poll carrying a worker id → the item is reserved (depth→pending) and the
	// worker is counted.
	res, _ := s.poll(t, envID, map[string]string{"Authorization": "Bearer " + key, "Anthropic-Worker-ID": "worker-1"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("poll status = %d", res.StatusCode)
	}
	body = getStats()
	if body["depth"] != float64(0) || body["pending"] != float64(1) {
		t.Errorf("after reserving poll = depth %v pending %v, want 0/1", body["depth"], body["pending"])
	}
	if body["workers_polling"] != float64(1) {
		t.Errorf("workers_polling = %v, want 1 (worker-1 polled)", body["workers_polling"])
	}

	// A poll without the Anthropic-Worker-ID header is not attributed to a worker.
	if r, _ := s.poll(t, envID, auth); r.StatusCode != http.StatusOK {
		t.Fatalf("header-less poll status = %d", r.StatusCode)
	}
	if body := getStats(); body["workers_polling"] != float64(1) {
		t.Errorf("workers_polling after a header-less poll = %v, want still 1", body["workers_polling"])
	}

	// Auth: the management key is rejected on the work API.
	res = s.doRaw(http.MethodGet, statsPath, nil, map[string]string{"x-api-key": testKey})
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("management key on stats = %d, want 401", res.StatusCode)
	}

	// Scoping: a key for another environment cannot read this one's stats.
	otherEnv, _ := selfHostedWorker(t, s, "ek-stats-other")
	res = s.doRaw(http.MethodGet, "/v1/environments/"+otherEnv+"/work/stats", nil, auth)
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("cross-env key on stats = %d, want 401", res.StatusCode)
	}
}
