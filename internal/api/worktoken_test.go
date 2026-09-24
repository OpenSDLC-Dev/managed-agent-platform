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
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// The sessions token (plan 36 decision 15): a per-item bearer minted when a
// polled item's session attaches a memory store, carried as the work item's
// `secret`, and accepted on the routes the reference worker calls with it.
// storeWorker plants the attachment through a test seam — the stored element,
// written into the session's resources directly, the way plan 35's slice 3
// landed its substrate — a shortcut from before slice 6 lifted the
// self_hosted refusal; waitingStoreSession attaches through the API.

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

// sessionsTokenFromSecret decodes the secret the way the reference worker does
// (checked against anthropic-sdk-go v1.66.0 — worker.go
// sessionsTokenFromSecret): URL-safe base64 with any padding stripped, of a
// JSON object with a sessions_token key.
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
		{http.MethodGet, store + "/memories/"},
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
	// with a lease the heartbeat no longer extends; the token rides the
	// wind-down and the post-stop memory flush for a minute from the request,
	// the lease and the force-stop aside, and is dead after.
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
	if _, err := s.pool.Exec(context.Background(), `UPDATE work_items SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, workID); err != nil {
		t.Fatal(err)
	}
	if st := status(t, s, http.MethodPost, store+"/memories", map[string]any{"path": "/b2.md", "content": "the frozen lease lapsed"}, tok); st/100 != 2 {
		t.Errorf("a memory write while stopping, the frozen lease lapsed = %d, want 2xx", st)
	}
	if st := status(t, s, http.MethodPost, work+"/stop", map[string]any{"force": true}, tok); st != http.StatusNoContent {
		t.Errorf("force-stop with the token = %d, want 204", st)
	}
	if st := status(t, s, http.MethodPost, store+"/memories", map[string]any{"path": "/c.md", "content": "the flush"}, tok); st/100 != 2 {
		t.Errorf("a memory write in the post-stop grace = %d, want 2xx", st)
	}
	if _, err := s.pool.Exec(context.Background(), `UPDATE work_items SET stop_requested_at = now() - interval '61 seconds' WHERE id = $1`, workID); err != nil {
		t.Fatal(err)
	}
	if st := status(t, s, http.MethodGet, "/v1/sessions/"+sessionID, nil, tok); st != http.StatusUnauthorized {
		t.Errorf("the token a minute after its item's stop was requested = %d, want 401", st)
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
	// A heartbeat's renewal is what carries the token through a long run.
	if _, err := s.pool.Exec(ctx, `UPDATE work_items SET lease_expires_at = now() + interval '2 seconds' WHERE id = $1`, workID); err != nil {
		t.Fatal(err)
	}
	if st := status(t, s, http.MethodPost, work+"/heartbeat?expected_last_heartbeat=NO_HEARTBEAT", nil, asBearer(token)); st != http.StatusOK {
		t.Fatalf("heartbeat with the token = %d, want 200", st)
	}
	var renewed bool
	if err := s.pool.QueryRow(ctx, `SELECT lease_expires_at > now() + interval '10 seconds' FROM work_items WHERE id = $1`, workID).Scan(&renewed); err != nil || !renewed {
		t.Errorf("the heartbeat did not renew the lease (renewed=%v, %v)", renewed, err)
	}
	if st := status(t, s, http.MethodGet, session, nil, asBearer(token)); st != http.StatusOK {
		t.Errorf("the token after the renewal = %d, want 200", st)
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
	// An abandoned wind-down: a graceful stop parks the item in stopping and
	// its lease lapses with its worker. Inside WindDown no poll settles it —
	// a live worker stops heartbeating once it learns of the stop and may
	// still be flushing — and the token works; past it the poll settles the
	// item stopped, and the token is dead by the same window.
	work2 := "/v1/environments/" + envID + "/work/" + workID2
	if st := status(t, s, http.MethodPost, work2+"/ack", nil, asBearer(key)); st != http.StatusOK {
		t.Fatalf("ack of the re-hand-out = %d", st)
	}
	if st := status(t, s, http.MethodPost, work2+"/heartbeat?expected_last_heartbeat=NO_HEARTBEAT", nil, asBearer(token2)); st != http.StatusOK {
		t.Fatalf("heartbeat of the re-hand-out = %d", st)
	}
	if st := status(t, s, http.MethodPost, work2+"/stop", map[string]any{}, asBearer(token2)); st != http.StatusNoContent {
		t.Fatalf("graceful stop of the re-hand-out = %d", st)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE work_items SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, workID2); err != nil {
		t.Fatal(err)
	}
	if st := status(t, s, http.MethodGet, session, nil, asBearer(token2)); st != http.StatusOK {
		t.Errorf("the token of a wind-down whose lease lapsed = %d, want 200 (the minute runs from the request)", st)
	}
	s.pollQuery(t, envID, "?block_ms=1", asBearer(key)) // inside the window: nothing to settle
	var state string
	if err := s.pool.QueryRow(ctx, `SELECT state FROM work_items WHERE id = $1`, workID2).Scan(&state); err != nil || state != "stopping" {
		t.Errorf("a wind-down inside its window after a poll: state=%q, %v; want still stopping", state, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE work_items SET stop_requested_at = now() - interval '61 seconds' WHERE id = $1`, workID2); err != nil {
		t.Fatal(err)
	}
	s.pollQuery(t, envID, "?block_ms=1", asBearer(key)) // past it: settled
	if err := s.pool.QueryRow(ctx, `SELECT state FROM work_items WHERE id = $1`, workID2).Scan(&state); err != nil || state != "stopped" {
		t.Errorf("a wind-down past its window after a poll: state=%q, %v; want stopped", state, err)
	}
	if st := status(t, s, http.MethodGet, session, nil, asBearer(token2)); st != http.StatusUnauthorized {
		t.Errorf("the token past its item's window = %d, want 401", st)
	}
	// A fresh item for the archived-session condition.
	enqueueOn(t, s, envID, sessionID)
	_, _, token3 := pollItem(t, s, envID, key)
	if st := status(t, s, http.MethodGet, session, nil, asBearer(token3)); st != http.StatusOK {
		t.Fatalf("the fresh item's token = %d, want 200", st)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE sessions SET archived_at = now() WHERE id = $1`, sessionID); err != nil {
		t.Fatal(err)
	}
	if st := status(t, s, http.MethodGet, session, nil, asBearer(token3)); st != http.StatusUnauthorized {
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

// TestPollDoesNotDeadlockAgainstTheSessionLock: a worker's poll meeting a
// delete, or an interrupt, of the session it would claim for (#643), in each
// order the two can take their locks. The overlap is the ordinary self_hosted
// wait: a turn that calls a worker's tool idles the session on requires_action
// and queues its tool_exec in the same commit, and requireNotRunning admits a
// delete of an idle session. For a session that attaches a memory store the
// claim mints a sessions token, whose foreign key needs the session row, while
// the delete (the interrupt) holds the session row and then needs the item, to
// cascade into it (to cancel it). The claim used to take the item and then
// wait for the session — a cycle Postgres broke by aborting either side.
//
// The holder's statements are spelled out rather than driven through the API,
// because they are the ordering under test (#313's precedent):
// requireNotRunning's lock, then deleteSession's DELETE or the interrupt's
// CancelSession. Each order is pinned, not raced for. Session first: the poll
// runs while the holder has the row and must answer without waiting for it.
// Item first: a trigger parks the claim at its token insert, the item already
// taken, while the holder asks for the session; released, the claim finishes
// and the holder goes after it.
func TestPollDoesNotDeadlockAgainstTheSessionLock(t *testing.T) {
	for _, h := range []struct {
		name string
		act  func(ctx context.Context, s *tserver, tx pgx.Tx, sessionID string) error
	}{
		{"delete", func(ctx context.Context, _ *tserver, tx pgx.Tx, sessionID string) error {
			_, err := tx.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, sessionID)
			return err
		}},
		{"interrupt", func(ctx context.Context, s *tserver, tx pgx.Tx, sessionID string) error {
			return queue.New(s.pool).CancelSession(ctx, tx, domain.ID(sessionID))
		}},
	} {
		// hold runs the holder's whole transaction: the session row, then the act.
		hold := func(ctx context.Context, s *tserver, tx pgx.Tx, sessionID string) error {
			if _, err := tx.Exec(ctx, `SELECT status FROM sessions WHERE id = $1 FOR UPDATE`, sessionID); err != nil {
				return err
			}
			if err := h.act(ctx, s, tx, sessionID); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}

		t.Run(h.name+"/session_first", func(t *testing.T) {
			s := newTestServer(t)
			logs := captureWarnings(t)
			ctx := context.Background()
			envID, key, sessionID := waitingStoreSession(t, s)
			tx, err := s.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, `SELECT status FROM sessions WHERE id = $1 FOR UPDATE`, sessionID); err != nil {
				t.Fatalf("hold the session row: %v", err)
			}
			polled := s.pollAsync(envID, key)
			a, answered := answerOrWaiters(t, s, polled, 1)
			if !answered {
				t.Errorf("the poll waited for the session row the %s holds", h.name)
			}
			actErr := h.act(ctx, s, tx, sessionID)
			if actErr == nil {
				actErr = tx.Commit(ctx)
			}
			_ = tx.Rollback(ctx) // a failed act still holds its locks until its transaction ends
			if !answered {
				a = awaitAnswer(t, polled)
			}
			if actErr != nil {
				t.Errorf("the %s = %v", h.name, actErr)
			}
			if a.err != nil || a.code != http.StatusOK || a.body != "null" {
				t.Errorf("poll with the session held = %d %q (%v); want 200 null", a.code, a.body, a.err)
			}
			if strings.Contains(logs(), "40P01") {
				t.Errorf("aborted as a deadlock:\n%s", logs())
			}
		})

		t.Run(h.name+"/item_first", func(t *testing.T) {
			s := newTestServer(t)
			logs := captureWarnings(t)
			ctx := context.Background()
			envID, key, sessionID := waitingStoreSession(t, s)
			release := parkTokenInsert(t, s)
			polled := s.pollAsync(envID, key)
			if a, answered := answerOrWaiters(t, s, polled, 1); answered {
				t.Fatalf("the poll answered %d %q before its token insert", a.code, a.body)
			}
			held := make(chan error, 1)
			go func() {
				tx, err := s.pool.Begin(ctx)
				if err != nil {
					held <- err
					return
				}
				defer func() { _ = tx.Rollback(ctx) }()
				held <- hold(ctx, s, tx, sessionID)
			}()
			// Both parked: the claim at its insert, the holder on the claim.
			if a, answered := answerOrWaiters(t, s, polled, 2); answered {
				t.Fatalf("the poll answered %d %q before its token insert", a.code, a.body)
			}
			release()
			a := awaitAnswer(t, polled)
			var holdErr error
			select {
			case holdErr = <-held:
			case <-time.After(30 * time.Second):
				t.Fatalf("the %s never finished after the claim did", h.name)
			}
			if holdErr != nil {
				t.Errorf("the %s = %v", h.name, holdErr)
			}
			if a.err != nil || a.code != http.StatusOK || a.body == "null" {
				t.Errorf("poll that took the item first = %d %q (%v); want 200 and the item", a.code, a.body, a.err)
			}
			if strings.Contains(logs(), "40P01") {
				t.Errorf("aborted as a deadlock:\n%s", logs())
			}
		})
	}
}

// waitingStoreSession reaches #643's state the way production does: a
// self_hosted session with a memory store, whose turn called a worker's tool —
// idle on requires_action, its tool_exec queued. It returns the environment,
// its worker key and the session.
func waitingStoreSession(t *testing.T, s *tserver) (envID, key, sessionID string) {
	t.Helper()
	agent := createAgent(t, s, map[string]any{"name": "wait", "model": "claude-opus-4-8",
		"tools": []any{map[string]any{"type": "agent_toolset_20260401"}}})
	envID = createEnvironment(t, s, map[string]any{"name": "wait", "config": map[string]any{"type": "self_hosted"}})["id"].(string)
	key = issueKey(t, s.pool, envID, "wait")
	storeID := createMemoryStore(t, s, "wait")
	sessionID = createSession(t, s, map[string]any{"agent": agent["id"], "environment_id": envID,
		"resources": []any{map[string]any{"type": "memory_store", "memory_store_id": storeID}}})["id"].(string)
	sendEvents(t, s, sessionID, userMessage("run bash"))
	turn := []provider.Chunk{
		{Kind: provider.KindToolUse, ToolUse: &provider.ToolUse{ID: "toolu_bash", Name: "bash", Input: json.RawMessage(`{"command":"true"}`)}},
		{Kind: provider.KindDone, StopReason: "tool_use", Usage: &domain.ModelUsage{InputTokens: 1, OutputTokens: 1}},
	}
	if found, err := newScriptedBrain(t, s.pool, turn).RunOnce(context.Background()); err != nil || !found {
		t.Fatalf("brain: %v %v", found, err)
	}
	if st, live := s.sessionStatus(sessionID), s.liveWork(sessionID, queue.ToolExec); st != "idle" || live != 1 {
		t.Fatalf("session %s with %d live tool_exec; want idle with 1", st, live)
	}
	return envID, key, sessionID
}

// parkTokenInsert makes the claim's token insert wait until the returned
// release is called — a trigger takes an advisory lock this test holds — so a
// claim stops there with its item already taken.
func parkTokenInsert(t *testing.T, s *tserver) (release func()) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `
		CREATE FUNCTION map_test_park_token() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM pg_advisory_xact_lock(643); RETURN NEW; END $$;
		CREATE TRIGGER map_test_park_token BEFORE INSERT ON work_session_tokens
		FOR EACH ROW EXECUTE FUNCTION map_test_park_token()`); err != nil {
		t.Fatal(err)
	}
	gate, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Exec(ctx, `SELECT pg_advisory_xact_lock(643)`); err != nil {
		t.Fatal(err)
	}
	release = func() { _ = gate.Rollback(ctx) }
	t.Cleanup(release)
	return release
}

type pollAnswer struct {
	code int
	body string
	err  error
}

// pollAsync polls off the test's goroutine; the answer arrives on the channel.
func (s *tserver) pollAsync(envID, key string) <-chan pollAnswer {
	polled := make(chan pollAnswer, 1)
	go func() {
		code, body, err := s.request(http.MethodGet, "/v1/environments/"+envID+"/work/poll", "", asBearer(key))
		polled <- pollAnswer{code, body, err}
	}()
	return polled
}

// answerOrWaiters returns the poll's answer (true) or, first, the moment n
// backends of this test's database wait on a lock (false). pgtest gives each
// test a fresh database, so every waiter counted is this test's; the counting
// query itself runs, never waits.
func answerOrWaiters(t *testing.T, s *tserver, polled <-chan pollAnswer, n int) (pollAnswer, bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); ; {
		select {
		case a := <-polled:
			return a, true
		default:
		}
		var waiting int
		if err := s.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity
			  WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatalf("count lock waiters: %v", err)
		}
		if waiting >= n {
			return pollAnswer{}, false
		}
		if time.Now().After(deadline) {
			t.Fatalf("neither an answer nor %d lock waiter(s) (saw %d)", n, waiting)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func awaitAnswer(t *testing.T, polled <-chan pollAnswer) pollAnswer {
	t.Helper()
	select {
	case a := <-polled:
		return a
	case <-time.After(30 * time.Second):
		t.Fatal("the poll never answered")
		return pollAnswer{}
	}
}

// TestPollServesPastAHeldSession: a held session delays its own items and no
// one else's (#643). Item A, of a store session whose row a transaction holds
// FOR UPDATE — as an event append, a settlement or a delete does — is queued
// before item B of another session: the poll hands out B at once, leaves A as
// it was, and A goes out once the holder lets go.
func TestPollServesPastAHeldSession(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	agentID, envID, heldID, _, key := storeWorker(t, s, "held")
	otherID := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})["id"].(string)
	enqueueOn(t, s, envID, otherID)
	var itemA string
	if err := s.pool.QueryRow(ctx, `SELECT id FROM work_items WHERE session_id = $1`, heldID).Scan(&itemA); err != nil {
		t.Fatal(err)
	}
	holder, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, heldID); err != nil {
		t.Fatalf("hold the session row: %v", err)
	}

	a, answered := answerOrWaiters(t, s, s.pollAsync(envID, key), 1)
	if !answered {
		t.Fatal("the poll waited for the held session row")
	}
	var item struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if a.err != nil || a.code != http.StatusOK || json.Unmarshal([]byte(a.body), &item) != nil || item.Data.ID != otherID {
		t.Fatalf("poll with A's session held = %d %s (%v); want B, the other session's item", a.code, a.body, a.err)
	}
	var untouched bool
	if err := s.pool.QueryRow(ctx,
		`SELECT state = 'queued' AND lease_expires_at IS NULL FROM work_items WHERE id = $1`, itemA).Scan(&untouched); err != nil || !untouched {
		t.Errorf("A was claimed or reserved behind its held session (%v)", err)
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if workID, _, token := pollItem(t, s, envID, key); workID != itemA || token == "" {
		t.Errorf("the poll after the holder let go handed out %s (token %q); want A, %s, with a token", workID, token, itemA)
	}
}

// request is doRaw for a goroutine: it reports a transport failure instead
// of failing the test from off the test's own goroutine, and reads the body
// through. body is sent verbatim; "" sends none.
func (s *tserver) request(method, path, body string, header map[string]string) (int, string, error) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, s.url+path, rd)
	if err != nil {
		return 0, "", err
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	return res.StatusCode, strings.TrimSpace(string(raw)), err
}
