package api_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// enqueueToolExec creates a fresh session in envID and enqueues a tool_exec work
// item for it, returning the session id. Enqueue is idempotent per (session,
// kind), so each work item needs its own session.
func enqueueToolExec(t *testing.T, s *tserver, agentID, envID string) string {
	t.Helper()
	sess := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})
	sid := sess["id"].(string)
	q := queue.New(s.pool)
	if _, err := q.Enqueue(context.Background(), s.pool, domain.ID(envID), domain.ID(sid), queue.ToolExec); err != nil {
		t.Fatalf("enqueue tool_exec: %v", err)
	}
	return sid
}

// TestWorkListReturnsScopedItems pins the work-items list: it returns the
// environment's self_hosted tool_exec items as BetaSelfHostedWork objects in the
// {data, next_page} envelope, and nothing else — not a model_turn row (the
// brain's queue) and not another environment's work.
func TestWorkListReturnsScopedItems(t *testing.T) {
	s := newTestServer(t)
	const keyA, keyB = "ek-wl-a", "ek-wl-b"
	envA, _ := selfHostedWorker(t, s, keyA)
	envB, sessionB := selfHostedWorker(t, s, keyB)
	agentA := createAgent(t, s, map[string]any{"name": "wl", "model": "claude-opus-4-8"})
	bearerA := map[string]string{"Authorization": "Bearer " + keyA}

	// Two tool_exec items in env A.
	s1 := enqueueToolExec(t, s, agentA["id"].(string), envA)
	s2 := enqueueToolExec(t, s, agentA["id"].(string), envA)

	// A model_turn in env A (the brain's queue) must not appear.
	modelSess := createSession(t, s, map[string]any{"agent": agentA["id"], "environment_id": envA})
	q := queue.New(s.pool)
	if _, err := q.Enqueue(context.Background(), s.pool, domain.ID(envA), domain.ID(modelSess["id"].(string)), queue.ModelTurn); err != nil {
		t.Fatalf("enqueue model_turn: %v", err)
	}
	// A tool_exec in env B must not appear in env A's list.
	if _, err := q.Enqueue(context.Background(), s.pool, domain.ID(envB), domain.ID(sessionB), queue.ToolExec); err != nil {
		t.Fatalf("enqueue env B: %v", err)
	}

	st, body := readJSON(t, s.doRaw(http.MethodGet, "/v1/environments/"+envA+"/work", nil, bearerA))
	if st != http.StatusOK {
		t.Fatalf("list = %d, want 200 (body %v)", st, body)
	}
	wantFields(t, body, "data", "next_page")
	if body["next_page"] != nil {
		t.Errorf("next_page = %v, want null (all items fit one page)", body["next_page"])
	}
	data := listData(t, body)
	if len(data) != 2 {
		t.Fatalf("list returned %d items, want 2 (its own tool_exec only): %v", len(data), data)
	}
	gotSessions := map[string]bool{}
	for _, item := range data {
		if item["type"] != "work" {
			t.Errorf("item type = %v, want work", item["type"])
		}
		if item["environment_id"] != envA {
			t.Errorf("item environment_id = %v, want %s", item["environment_id"], envA)
		}
		d, _ := item["data"].(map[string]any)
		if d == nil || d["type"] != "session" {
			t.Errorf("item data = %v, want a session reference", item["data"])
		} else {
			gotSessions[d["id"].(string)] = true
		}
	}
	if !gotSessions[s1] || !gotSessions[s2] {
		t.Errorf("list sessions = %v, want both %s and %s", gotSessions, s1, s2)
	}
}

// TestWorkListPaginates walks the opaque cursor: a limit smaller than the item
// count returns a next_page, and following it yields the remaining items with no
// overlap and a null terminal cursor.
func TestWorkListPaginates(t *testing.T) {
	s := newTestServer(t)
	const key = "ek-wl-page"
	env, _ := selfHostedWorker(t, s, key)
	agent := createAgent(t, s, map[string]any{"name": "wlp", "model": "claude-opus-4-8"})
	bearer := map[string]string{"Authorization": "Bearer " + key}

	want := map[string]bool{}
	for i := 0; i < 3; i++ {
		want[enqueueToolExec(t, s, agent["id"].(string), env)] = true
	}

	seen := map[string]bool{}
	pageURL := "/v1/environments/" + env + "/work?limit=2"
	for pages := 0; pages < 5; pages++ {
		st, body := readJSON(t, s.doRaw(http.MethodGet, pageURL, nil, bearer))
		if st != http.StatusOK {
			t.Fatalf("list page = %d, want 200 (body %v)", st, body)
		}
		data := listData(t, body)
		for _, item := range data {
			d := item["data"].(map[string]any)
			sid := d["id"].(string)
			if seen[sid] {
				t.Errorf("session %s appeared on two pages", sid)
			}
			seen[sid] = true
		}
		next, _ := body["next_page"].(string)
		if next == "" {
			if len(data) > 2 {
				t.Errorf("page returned %d items, want <= limit 2", len(data))
			}
			break
		}
		pageURL = "/v1/environments/" + env + "/work?limit=2&page=" + url.QueryEscape(next)
	}
	if len(seen) != 3 {
		t.Errorf("walked %d items across pages, want 3 (%v)", len(seen), seen)
	}
	for sid := range want {
		if !seen[sid] {
			t.Errorf("item for session %s never appeared in the paginated walk", sid)
		}
	}
}

// TestWorkListAuthAndEmpty pins the auth boundary and the empty shape: the work
// list takes the environment Bearer key (a key is scoped to one environment),
// never the management x-api-key, and an environment with no work returns an
// empty data array with a null cursor.
func TestWorkListAuthAndEmpty(t *testing.T) {
	s := newTestServer(t)
	const keyA, keyB = "ek-wl-auth-a", "ek-wl-auth-b"
	envA, _ := selfHostedWorker(t, s, keyA)
	selfHostedWorker(t, s, keyB)
	listA := "/v1/environments/" + envA + "/work"

	// Management key and a key for another environment are both rejected.
	st, body := readJSON(t, s.doRaw(http.MethodGet, listA, nil, map[string]string{"x-api-key": testKey}))
	wantErr(t, st, body, http.StatusUnauthorized, "authentication_error")
	st, body = readJSON(t, s.doRaw(http.MethodGet, listA, nil, map[string]string{"Authorization": "Bearer " + keyB}))
	wantErr(t, st, body, http.StatusUnauthorized, "authentication_error")

	// The env's own key on an empty queue: 200 with an empty data array.
	st, body = readJSON(t, s.doRaw(http.MethodGet, listA, nil, map[string]string{"Authorization": "Bearer " + keyA}))
	if st != http.StatusOK {
		t.Fatalf("empty list = %d, want 200 (body %v)", st, body)
	}
	wantFields(t, body, "data", "next_page")
	if data := listData(t, body); len(data) != 0 {
		t.Errorf("empty list data = %v, want []", data)
	}
	if body["next_page"] != nil {
		t.Errorf("empty list next_page = %v, want null", body["next_page"])
	}
}

// TestWorkListRejectsBadRequest pins the list's request-level errors: a
// non-GET method is 405, and a malformed page cursor is a 400.
func TestWorkListRejectsBadRequest(t *testing.T) {
	s := newTestServer(t)
	const key = "ek-wl-bad"
	envID, _ := selfHostedWorker(t, s, key)
	bearer := map[string]string{"Authorization": "Bearer " + key}
	list := "/v1/environments/" + envID + "/work"

	st, body := readJSON(t, s.doRaw(http.MethodPost, list, nil, bearer))
	wantErr(t, st, body, http.StatusMethodNotAllowed, "invalid_request_error")

	st, body = readJSON(t, s.doRaw(http.MethodGet, list+"?page=not-a-cursor", nil, bearer))
	wantErr(t, st, body, http.StatusBadRequest, "invalid_request_error")

	// limit is validated to 1–100, not clamped: out of range is a 400.
	st, body = readJSON(t, s.doRaw(http.MethodGet, list+"?limit=101", nil, bearer))
	wantErr(t, st, body, http.StatusBadRequest, "invalid_request_error")
}
