package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

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
		"latest_heartbeat_at", "metadata", "started_at", "state", "stop_requested_at",
		"stopped_at", "type")

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

// TestWorkPollRejectsWrongMethodAndPath pins the wire error envelope on the
// work subtree: a known route with the wrong method is 405, an unknown work
// path is 404 — both authenticated first.
func TestWorkPollRejectsWrongMethodAndPath(t *testing.T) {
	s := newTestServer(t)
	const key = "ek-route"
	envID, _ := selfHostedWorker(t, s, key)
	auth := map[string]string{"Authorization": "Bearer " + key}

	res := s.doRaw(http.MethodPost, "/v1/environments/"+envID+"/work/poll", nil, auth)
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
}
