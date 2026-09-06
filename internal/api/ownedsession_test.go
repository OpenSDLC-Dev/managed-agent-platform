package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// The session a live dream owns is read-only to the public API (plan 41 §4.4):
// it lists, reads and streams like any session, and every mutation a developer
// key could make answers 400 while the dream is open, because the hidden agent
// behind it runs an always_allow toolset the dream steers.

// openDream writes a running dream naming the session, as the start arm's write
// transaction leaves it. Only the ids are parameters; the rest are the NOT NULL
// columns filled with the shapes dreams_test.go's create produces.
func openDream(t *testing.T, s *tserver, sessionID string) string {
	t.Helper()
	id := domain.NewID(domain.PrefixDream).String()
	storeID := createMemoryStore(t, s, "dream-input")
	if _, err := s.pool.Exec(context.Background(),
		`INSERT INTO dreams (id, status, stage, inputs, input_memory_store_id, input_session_ids,
		   model, output_behavior, session_id)
		 VALUES ($1, 'running', 1, '[]'::jsonb, $2, '{}'::text[],
		   '{"id":"claude-opus-4-8"}'::jsonb, '{"type":"create_new"}'::jsonb, $3)`,
		id, storeID, sessionID); err != nil {
		t.Fatalf("insert dream: %v", err)
	}
	return id
}

// closeDream stamps the dream terminal and closed, as the closing arm does. The
// terminal status and ended_at travel with closed_at or the CHECKs refuse it.
func closeDream(t *testing.T, s *tserver, dreamID string) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE dreams SET status = 'completed', ended_at = now(), closed_at = now() WHERE id = $1`,
		dreamID); err != nil {
		t.Fatalf("close dream: %v", err)
	}
}

func wantDreamHold(t *testing.T, status int, body map[string]any, dreamID string) {
	t.Helper()
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	msg, _ := body["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, dreamID) {
		t.Errorf("message = %q, want it to name dream %s", msg, dreamID)
	}
}

func TestDreamOwnedSessionIsReadOnly(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	fileID := s.uploadFile(t, "note.md", nil, "hello")["id"].(string)
	sid := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID,
		"resources": []any{map[string]any{"type": "file", "file_id": fileID, "mount_path": "note.md"}}})["id"].(string)
	dreamID := openDream(t, s, sid)

	primary := domain.PrimaryThreadID(domain.ID(sid)).String()
	unknownResource := domain.NewID(domain.PrefixResource).String()
	mutations := map[string]struct {
		method, path string
		body         any
	}{
		"send events": {http.MethodPost, "/v1/sessions/" + sid + "/events", map[string]any{
			"events": []any{map[string]any{"type": "user.message",
				"content": []any{map[string]any{"type": "text", "text": "hi"}}}}}},
		"update":         {http.MethodPost, "/v1/sessions/" + sid, map[string]any{"title": "mine now"}},
		"delete":         {http.MethodDelete, "/v1/sessions/" + sid, nil},
		"archive":        {http.MethodPost, "/v1/sessions/" + sid + "/archive", nil},
		"archive thread": {http.MethodPost, "/v1/sessions/" + sid + "/threads/" + primary + "/archive", nil},
		"add resource": {http.MethodPost, "/v1/sessions/" + sid + "/resources",
			map[string]any{"type": "file", "file_id": fileID, "mount_path": "second.md"}},
		"rotate resource token": {http.MethodPost, "/v1/sessions/" + sid + "/resources/" + unknownResource,
			map[string]any{"authorization_token": "t"}},
		"delete resource": {http.MethodDelete, "/v1/sessions/" + sid + "/resources/" + unknownResource, nil},
	}
	for name, tc := range mutations {
		t.Run(name, func(t *testing.T) {
			status, body := s.do(tc.method, tc.path, tc.body)
			wantDreamHold(t, status, body, dreamID)
		})
	}

	// Reads and the stream are untouched: the reference exposes the pipeline
	// session, and a client watching a dream run needs both.
	for _, path := range []string{
		"/v1/sessions/" + sid,
		"/v1/sessions",
		"/v1/sessions/" + sid + "/events",
		"/v1/sessions/" + sid + "/threads",
		"/v1/sessions/" + sid + "/resources",
	} {
		if status, body := s.do(http.MethodGet, path, nil); status != http.StatusOK {
			t.Errorf("GET %s: status %d (%v), want 200", path, status, body)
		}
	}
	s.stream(t, "/v1/sessions/"+sid+"/events/stream").close()

	// A transcript row is the runner's while the dream is open: the executor
	// tolerates a deleted mount, so a delete here would blank a transcript
	// mid-dream.
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE files SET dream_id = $1 WHERE id = $2`, dreamID, fileID); err != nil {
		t.Fatalf("mark the file the dream's: %v", err)
	}
	status, body := s.do(http.MethodDelete, "/v1/files/"+fileID, nil)
	wantDreamHold(t, status, body, dreamID)
	// A file no dream owns still deletes.
	other := s.uploadFile(t, "other.md", nil, "bytes")["id"].(string)
	if status, body := s.do(http.MethodDelete, "/v1/files/"+other, nil); status != http.StatusOK {
		t.Errorf("delete an unowned file: status %d (%v), want 200", status, body)
	}

	// Once the dream closes the gate lifts, and the routes answer for the
	// session as they do for any other — no special case for a session a dream
	// used to own.
	closeDream(t, s, dreamID)
	if status, body := s.do(http.MethodDelete, "/v1/files/"+fileID, nil); status != http.StatusOK {
		t.Errorf("delete the transcript after the close: status %d (%v), want 200", status, body)
	}
	if status, body := s.do(http.MethodPost, "/v1/sessions/"+sid+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive after the close: status %d (%v), want 200", status, body)
	}
	if status, body := s.do(http.MethodPost, "/v1/sessions/"+sid+"/events", map[string]any{
		"events": []any{map[string]any{"type": "user.message",
			"content": []any{map[string]any{"type": "text", "text": "hi"}}}}}); status != http.StatusBadRequest ||
		!strings.Contains(body["error"].(map[string]any)["message"].(string), "archived") {
		t.Errorf("send after the archive: status %d (%v), want the archived refusal", status, body)
	}
	if status, body := s.do(http.MethodDelete, "/v1/sessions/"+sid, nil); status != http.StatusOK {
		t.Fatalf("delete after the close: status %d (%v), want 200", status, body)
	}
	var sessionID *string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT session_id FROM dreams WHERE id = $1`, dreamID).Scan(&sessionID); err != nil {
		t.Fatalf("read the dream: %v", err)
	}
	if sessionID != nil {
		t.Errorf("dream session_id = %v after the session's delete, want null", *sessionID)
	}
}

// The whole-session interrupt the runner's cancel and tick call. The handler's
// own interrupt cases (events_test.go) prove the lifted arm; this proves the
// seam around it — the event on the log, the session settled, and the two
// answers a session that is archived or gone gets.
func TestInterruptSessionInTx(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	agentID, envID := fixture(t, s)
	created := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID,
		"initial_events": []any{map[string]any{"type": "user.message",
			"content": []any{map[string]any{"type": "text", "text": "go"}}}}})
	sid := created["id"].(string)
	if created["status"] != "running" {
		t.Fatalf("session status = %v, want running", created["status"])
	}
	// An active outcome, so the interrupt has one to settle: a dream canceled
	// mid-run settles whatever the pipeline session had open.
	if status, body := s.do(http.MethodPost, "/v1/sessions/"+sid+"/events",
		map[string]any{"events": []any{defineOutcome("consolidate", nil)}}); status != http.StatusOK {
		t.Fatalf("define outcome: status %d (%v)", status, body)
	}

	if err := api.InterruptSessionForTest(ctx, s.pool, sid); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	status, body := s.do(http.MethodGet, "/v1/sessions/"+sid, nil)
	if status != http.StatusOK || body["status"] != "idle" {
		t.Fatalf("session status = %v (%d), want idle", body["status"], status)
	}
	_, log := s.do(http.MethodGet, "/v1/sessions/"+sid+"/events", nil)
	var sawInterrupt, sawIdle bool
	for _, ev := range listData(t, log) {
		sawInterrupt = sawInterrupt || ev["type"] == "user.interrupt"
		sawIdle = sawIdle || ev["type"] == "session.status_idle"
	}
	if !sawInterrupt || !sawIdle {
		t.Errorf("log has interrupt=%v idle=%v, want both", sawInterrupt, sawIdle)
	}
	outs, _ := body["outcome_evaluations"].([]any)
	if len(outs) != 1 {
		t.Fatalf("outcome_evaluations = %v, want the one entry", body["outcome_evaluations"])
	}
	if entry, _ := outs[0].(map[string]any); entry["result"] != "interrupted" {
		t.Errorf("outcome result = %v, want interrupted", outs[0])
	}

	// An archived session has nothing to interrupt, and its log is closed to
	// appends — the closing arm archives before the tick can look again.
	if status, body := s.do(http.MethodPost, "/v1/sessions/"+sid+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive: status %d (%v)", status, body)
	}
	if err := api.InterruptSessionForTest(ctx, s.pool, sid); err != nil {
		t.Errorf("interrupt of an archived session: %v, want a no-op", err)
	}
	if err := api.InterruptSessionForTest(ctx, s.pool, domain.NewID(domain.PrefixSession).String()); err == nil {
		t.Error("interrupt of a missing session succeeded, want not found")
	}
}
