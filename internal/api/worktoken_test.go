package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// The sessions token (plan 36 decision 15): a per-item bearer minted when a
// polled item's session attaches a memory store, carried as the work item's
// `secret`, and accepted on the routes the reference worker calls with it.
// Until slice 6 lifts the self_hosted refusal, the attachment is planted
// through the test seam — the stored element, written into the session's
// resources directly, the way plan 35's slice 3 landed its substrate.

// storeWorker provisions a self_hosted environment with its worker key, an
// agent, a session with a store attached through the seam, and one queued
// tool_exec item for the session.
func storeWorker(t *testing.T, s *tserver, name string) (agentID, envID, sessionID, storeID, key string) {
	t.Helper()
	agent := createAgent(t, s, map[string]any{"name": "w-" + name, "model": "claude-opus-4-8"})
	agentID = agent["id"].(string)
	env := createEnvironment(t, s, map[string]any{"name": "wh-" + name, "config": map[string]any{"type": "self_hosted"}})
	envID = env["id"].(string)
	sess := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})
	sessionID = sess["id"].(string)
	storeID = attachStoreBySeam(t, s, sessionID, "Notes "+name)
	enqueueOn(t, s, envID, sessionID)
	return agentID, envID, sessionID, storeID, issueKey(t, s.pool, envID, name)
}

// attachStoreBySeam creates a store and appends its stored resource element
// (memoryResourceJSON's shape) to the session's resources.
func attachStoreBySeam(t *testing.T, s *tserver, sessionID, name string) string {
	t.Helper()
	storeID := createMemoryStore(t, s, name)
	elem, _ := json.Marshal([]map[string]any{{
		"type": "memory_store", "memory_store_id": storeID, "access": "read_write",
		"description": "", "instructions": nil, "mount_path": "/mnt/memory/notes", "name": name,
	}})
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE sessions SET resources = resources || $2::jsonb WHERE id = $1`, sessionID, elem); err != nil {
		t.Fatalf("attach store by seam: %v", err)
	}
	return storeID
}

func enqueueOn(t *testing.T, s *tserver, envID, sessionID string) {
	t.Helper()
	if _, err := queue.New(s.pool).Enqueue(context.Background(), s.pool, domain.ID(envID), domain.ID(sessionID), queue.ToolExec); err != nil {
		t.Fatalf("enqueue tool_exec: %v", err)
	}
}

func asBearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// pollItem polls with the environment key and returns the item handed out —
// its id, its raw secret (nil when null) and the sessions token decoded from
// it ("" when there is none).
func pollItem(t *testing.T, s *tserver, envID, key string) (workID string, secret any, token string) {
	t.Helper()
	res, body := s.poll(t, envID, asBearer(key))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("poll = %d: %s", res.StatusCode, body)
	}
	if cc := res.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("the poll's Cache-Control = %q, want no-store (its secret is a credential)", cc)
	}
	var item map[string]any
	if err := json.Unmarshal([]byte(body), &item); err != nil || item == nil {
		t.Fatalf("poll handed out nothing: %s", body)
	}
	workID, _ = item["id"].(string)
	secret = item["secret"]
	if str, ok := secret.(string); ok {
		token = sessionsTokenFromSecret(t, str)
	}
	return workID, secret, token
}

// sessionsTokenFromSecret decodes the secret the way the v1.66.0 reference
// worker does (lib/environments/worker.go sessionsTokenFromSecret): URL-safe
// base64 with any padding stripped, of a JSON object with a sessions_token key.
func sessionsTokenFromSecret(t *testing.T, secret string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(secret, "="))
	if err != nil {
		t.Fatalf("secret is not URL-safe base64: %v", err)
	}
	var env struct {
		SessionsToken string `json:"sessions_token"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("secret is not a JSON object: %v (%s)", err, raw)
	}
	return env.SessionsToken
}

func status(t *testing.T, s *tserver, method, path string, body any, headers map[string]string) int {
	t.Helper()
	res := s.doRaw(method, path, body, headers)
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return res.StatusCode
}

// TestPollMintsASessionsTokenForAStoreSession: `secret` is null for a session
// without stores (today's path, byte for byte) and, for a session with one, the
// reference worker's envelope carrying a wtk_ token — on the poll response
// only, with nothing but its hash at rest.
func TestPollMintsASessionsTokenForAStoreSession(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	envID, sessionID, key := selfHostedWorker(t, s, "storeless")
	enqueueOn(t, s, envID, sessionID)
	if _, secret, _ := pollItem(t, s, envID, key); secret != nil {
		t.Errorf("a storeless session's item carried secret %v; want null", secret)
	}
	var rows int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM work_session_tokens`).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("%d token rows after a storeless poll (%v); want none", rows, err)
	}

	_, envID, sessionID, _, key = storeWorker(t, s, "stored")
	workID, secret, token := pollItem(t, s, envID, key)
	if !strings.HasPrefix(token, "wtk_") || len(token) < 40 {
		t.Fatalf("secret %v decoded to %q; want a wtk_ token", secret, token)
	}
	var hash, rowWork, rowSession string
	if err := s.pool.QueryRow(ctx, `SELECT token_hash, work_id, session_id FROM work_session_tokens`).Scan(&hash, &rowWork, &rowSession); err != nil {
		t.Fatalf("token row: %v", err)
	}
	sum := sha256.Sum256([]byte(token))
	if hash != hex.EncodeToString(sum[:]) {
		t.Errorf("token_hash = %s; want the token's sha256", hash)
	}
	if rowWork != workID || rowSession != sessionID {
		t.Errorf("token row names (%s, %s); want (%s, %s)", rowWork, rowSession, workID, sessionID)
	}
	// Every other retrieval path keeps null.
	for _, path := range []string{"/v1/environments/" + envID + "/work/" + workID, "/v1/environments/" + envID + "/work"} {
		res := s.doRaw(http.MethodGet, path, nil, asBearer(key))
		raw, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK || strings.Contains(string(raw), "sessions_token") || !strings.Contains(string(raw), `"secret":null`) {
			t.Errorf("GET %s = %d %s; want secret null", path, res.StatusCode, raw)
		}
	}
}

// TestSessionsTokenAdmissionMatrix pins where the token is a credential — the
// reference worker's calls, scoped to its own item, session and stores — and
// where it is not.
func TestSessionsTokenAdmissionMatrix(t *testing.T) {
	s := newTestServer(t)
	agentID, envID, sessionID, storeID, key := storeWorker(t, s, "matrix")
	workID, _, token := pollItem(t, s, envID, key)
	tok := asBearer(token)
	work := "/v1/environments/" + envID + "/work/" + workID
	// The reference poller acks with the environment key before the per-item
	// flow runs; a heartbeat before ack is the lifecycle's 412, not the lane's.
	if st := status(t, s, http.MethodPost, work+"/ack", nil, asBearer(key)); st != http.StatusOK {
		t.Fatalf("ack = %d", st)
	}

	// Its own item's heartbeat; its own session's read, events list and send.
	if st := status(t, s, http.MethodPost, work+"/heartbeat?expected_last_heartbeat=NO_HEARTBEAT", nil, tok); st != http.StatusOK {
		t.Errorf("heartbeat with the token = %d, want 200", st)
	}
	for _, probe := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/v1/sessions/" + sessionID, nil},
		{http.MethodGet, "/v1/sessions/" + sessionID + "/events", nil},
		{http.MethodPost, "/v1/sessions/" + sessionID + "/events", map[string]any{"events": []any{userMessage("from the worker")}}},
	} {
		if st := status(t, s, probe.method, probe.path, probe.body, tok); st != http.StatusOK {
			t.Errorf("%s %s with the token = %d, want 200", probe.method, probe.path, st)
		}
	}
	// The skill reads, workspace-global as they are for the environment key.
	skill := s.createSkill(t)
	if st := status(t, s, http.MethodGet, "/v1/skills/"+skill["id"].(string), nil, tok); st != http.StatusOK {
		t.Errorf("skill read with the token = %d, want 200", st)
	}
	// The memories of a store its session attaches — the five calls the
	// worker's sync makes; the store's own read is not among them.
	store := "/v1/memory_stores/" + storeID
	if st := status(t, s, http.MethodGet, store, nil, tok); st != http.StatusUnauthorized {
		t.Errorf("store read with the token = %d, want 401", st)
	}
	res := s.doRaw(http.MethodPost, store+"/memories", map[string]any{"path": "/a.md", "content": "one"}, tok)
	_, created := readJSON(t, res)
	mid, _ := created["id"].(string)
	if mid == "" {
		t.Fatalf("memory create with the token: %v", created)
	}
	vid := created["memory_version_id"].(string)
	// The worker's write is the session's version, as the executor's sync
	// pushes are — never an api_actor.
	if _, version := s.do(http.MethodGet, store+"/memory_versions/"+vid, nil); version["created_by"] == nil {
		t.Errorf("the worker's version has no actor: %v", version)
	} else if by := version["created_by"].(map[string]any); by["type"] != "session_actor" || by["session_id"] != sessionID {
		t.Errorf("the worker's version is by %v; want session_actor %s", by, sessionID)
	}
	for _, probe := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, store + "/memories", nil},
		{http.MethodGet, store + "/memories/" + mid, nil},
		{http.MethodPost, store + "/memories/" + mid, map[string]any{"content": "two"}},
		{http.MethodDelete, store + "/memories/" + mid, nil},
	} {
		if st := status(t, s, probe.method, probe.path, probe.body, tok); st/100 != 2 {
			t.Errorf("%s %s with the token = %d, want 2xx", probe.method, probe.path, st)
		}
	}

	// Not-found, never a leak: a store the session does not attach, and a
	// sibling session in the same environment.
	other := createMemoryStore(t, s, "unattached")
	if st := status(t, s, http.MethodGet, "/v1/memory_stores/"+other+"/memories", nil, tok); st != http.StatusNotFound {
		t.Errorf("an unattached store with the token = %d, want 404", st)
	}
	sibling := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})["id"].(string)
	if st := status(t, s, http.MethodGet, "/v1/sessions/"+sibling, nil, tok); st != http.StatusNotFound {
		t.Errorf("a sibling session with the token = %d, want 404", st)
	}

	// Refused: the rest of the work API, the store's own read, versions and
	// lifecycle, every management route, another environment's item, and an
	// escaped path that only decodes to an admitted one.
	otherEnv, _, _ := selfHostedWorker(t, s, "elsewhere")
	for _, probe := range []struct {
		method, path string
	}{
		{http.MethodPost, work + "/ack"},
		{http.MethodGet, work},
		{http.MethodPost, work},
		{http.MethodGet, "/v1/environments/" + envID + "/work"},
		{http.MethodGet, "/v1/environments/" + envID + "/work/poll"},
		{http.MethodPost, "/v1/environments/" + otherEnv + "/work/" + workID + "/heartbeat?expected_last_heartbeat=NO_HEARTBEAT"},
		{http.MethodGet, store + "/memory_versions"},
		{http.MethodGet, store + "/memory_versions/" + vid},
		{http.MethodPost, store + "/memory_versions/" + vid + "/redact"},
		{http.MethodPatch, store + "/memories/" + mid},
		{http.MethodGet, store + "%2Fmemories"},
		{http.MethodGet, "/v1/sessions/" + sessionID + "/%65vents"},
		{http.MethodPost, store + "/archive"},
		{http.MethodPost, store},
		{http.MethodDelete, store},
		{http.MethodGet, "/v1/memory_stores"},
		{http.MethodPost, "/v1/memory_stores"},
		{http.MethodPost, "/v1/sessions"},
		{http.MethodGet, "/v1/sessions"},
		{http.MethodPost, "/v1/sessions/" + sessionID + "/archive"},
	} {
		if st := status(t, s, probe.method, probe.path, map[string]any{}, tok); st != http.StatusUnauthorized {
			t.Errorf("%s %s with the token = %d, want 401", probe.method, probe.path, st)
		}
	}

	// On the memory routes the environment key is refused and a management
	// key admitted.
	if st := status(t, s, http.MethodGet, store+"/memories", nil, asBearer(key)); st != http.StatusUnauthorized {
		t.Errorf("the environment key on a memory route = %d, want 401", st)
	}
	if st, _ := s.do(http.MethodGet, store+"/memories", nil); st != http.StatusOK {
		t.Errorf("the management key on a memory route = %d, want 200", st)
	}

	// Its own item's stop. A graceful stop parks the active item in stopping
	// with its lease, and the token rides the wind-down; the force-stop that
	// ends it starts the minute of grace the worker's post-stop memory flush
	// needs, after which the token is dead.
	if st := status(t, s, http.MethodPost, work+"/stop", map[string]any{}, tok); st != http.StatusNoContent {
		t.Errorf("graceful stop with the token = %d, want 204", st)
	}
	var state string
	if err := s.pool.QueryRow(context.Background(), `SELECT state FROM work_items WHERE id = $1`, workID).Scan(&state); err != nil || state != "stopping" {
		t.Fatalf("after the graceful stop: state=%q, %v; want stopping", state, err)
	}
	if st := status(t, s, http.MethodPost, store+"/memories", map[string]any{"path": "/b.md", "content": "while stopping"}, tok); st/100 != 2 {
		t.Errorf("a memory write while stopping = %d, want 2xx", st)
	}
	if st := status(t, s, http.MethodPost, work+"/stop", map[string]any{"force": true}, tok); st != http.StatusNoContent {
		t.Errorf("force-stop with the token = %d, want 204", st)
	}
	if st := status(t, s, http.MethodPost, store+"/memories", map[string]any{"path": "/c.md", "content": "the flush"}, tok); st/100 != 2 {
		t.Errorf("a memory write in the post-stop grace = %d, want 2xx", st)
	}
	if _, err := s.pool.Exec(context.Background(), `UPDATE work_items SET stopped_at = now() - interval '61 seconds' WHERE id = $1`, workID); err != nil {
		t.Fatal(err)
	}
	if st := status(t, s, http.MethodGet, "/v1/sessions/"+sessionID, nil, tok); st != http.StatusUnauthorized {
		t.Errorf("the token a minute after its item stopped = %d, want 401", st)
	}
}

// TestSessionsTokenJoinConditions: the token authenticates only while the row
// it names is a live item — ack keeps it, a lapsed lease ends it, a re-hand-out
// supersedes it, an archived session ends it.
func TestSessionsTokenJoinConditions(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	_, envID, sessionID, _, key := storeWorker(t, s, "joins")
	workID, _, token := pollItem(t, s, envID, key)
	session := "/v1/sessions/" + sessionID
	work := "/v1/environments/" + envID + "/work/" + workID

	if st := status(t, s, http.MethodPost, work+"/ack", nil, asBearer(key)); st != http.StatusOK {
		t.Fatalf("ack = %d", st)
	}
	if st := status(t, s, http.MethodGet, session, nil, asBearer(token)); st != http.StatusOK {
		t.Errorf("the token after ack = %d, want 200 (ack is the entry transition)", st)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE work_items SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, workID); err != nil {
		t.Fatal(err)
	}
	if st := status(t, s, http.MethodGet, session, nil, asBearer(token)); st != http.StatusUnauthorized {
		t.Errorf("the token after its lease lapsed = %d, want 401", st)
	}
	// The re-hand-out mints a fresh item id and a fresh token; the old token's
	// work id names nothing live.
	workID2, _, token2 := pollItem(t, s, envID, key)
	if workID2 == workID || token2 == token || token2 == "" {
		t.Fatalf("re-hand-out: item %s → %s, token reissued = %v", workID, workID2, token2 != token)
	}
	if st := status(t, s, http.MethodGet, session, nil, asBearer(token)); st != http.StatusUnauthorized {
		t.Errorf("the superseded token = %d, want 401", st)
	}
	if st := status(t, s, http.MethodGet, session, nil, asBearer(token2)); st != http.StatusOK {
		t.Errorf("the re-hand-out's token = %d, want 200", st)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE sessions SET archived_at = now() WHERE id = $1`, sessionID); err != nil {
		t.Fatal(err)
	}
	if st := status(t, s, http.MethodGet, session, nil, asBearer(token2)); st != http.StatusUnauthorized {
		t.Errorf("the token after its session archived = %d, want 401", st)
	}
}

// TestSessionsTokenInsertFailureLeavesTheItemUnclaimed: the token row is
// inserted in the claim's own transaction, so an insert that fails rolls the
// claim back — the poll is a 500 and the item is still queued for the next.
func TestSessionsTokenInsertFailureLeavesTheItemUnclaimed(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	_, envID, sessionID, _, key := storeWorker(t, s, "fault")
	if _, err := s.pool.Exec(ctx, `
		CREATE FUNCTION map_test_refuse_token() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'token insert refused by the test'; END $$;
		CREATE TRIGGER map_test_refuse_token BEFORE INSERT ON work_session_tokens
		FOR EACH ROW EXECUTE FUNCTION map_test_refuse_token()`); err != nil {
		t.Fatal(err)
	}
	res, body := s.poll(t, envID, asBearer(key))
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("poll with the insert refused = %d %s; want 500", res.StatusCode, body)
	}
	var unclaimed bool
	if err := s.pool.QueryRow(ctx,
		`SELECT state = 'queued' AND lease_expires_at IS NULL FROM work_items WHERE session_id = $1`, sessionID).Scan(&unclaimed); err != nil || !unclaimed {
		t.Errorf("the item was claimed under a failed token insert (%v)", err)
	}
	if _, err := s.pool.Exec(ctx, `DROP TRIGGER map_test_refuse_token ON work_session_tokens; DROP FUNCTION map_test_refuse_token()`); err != nil {
		t.Fatal(err)
	}
	if _, _, token := pollItem(t, s, envID, key); token == "" {
		t.Error("the next poll did not hand the item out with a token")
	}
}
