package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// readJSON drains a raw response into a status code and decoded object.
func readJSON(t *testing.T, res *http.Response) (int, map[string]any) {
	t.Helper()
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	var obj map[string]any
	_ = json.Unmarshal(raw, &obj)
	return res.StatusCode, obj
}

// TestEnvironmentKeyAuthorizesSessionEvents pins the dual-auth boundary on the
// session events subtree: a BYOC worker's Authorization: Bearer environment key
// authorizes list/send (and stream) for a session in its own environment, is
// not-found for a session in another environment (or a missing one), and the
// management x-api-key keeps working. The reference environment key authorizes
// "both the work-poll calls and the session-level calls".
func TestEnvironmentKeyAuthorizesSessionEvents(t *testing.T) {
	s := newTestServer(t)
	const keyA, keyB = "ek-sess-a", "ek-sess-b"
	_, sessionA := selfHostedWorker(t, s, keyA)
	_, sessionB := selfHostedWorker(t, s, keyB) // a second environment + session
	bearerA := map[string]string{"Authorization": "Bearer " + keyA}
	eventsA := "/v1/sessions/" + sessionA + "/events"

	// The env key lists its own session's events.
	if st, body := readJSON(t, s.doRaw(http.MethodGet, eventsA, nil, bearerA)); st != http.StatusOK {
		t.Fatalf("env-key list = %d, want 200 (body %v)", st, body)
	} else if _, ok := body["data"]; !ok {
		t.Errorf("env-key list missing data array: %v", body)
	}

	// The env key posts a valid inbound event to its own session — authorized.
	post := map[string]any{"events": []any{userMessage("hi from the worker")}}
	if st, body := readJSON(t, s.doRaw(http.MethodPost, eventsA, post, bearerA)); st != http.StatusOK {
		t.Fatalf("env-key send = %d, want 200 (body %v)", st, body)
	}

	// A session in another environment is not-found (no cross-env read/write, and
	// cross-env existence never leaks).
	st, body := readJSON(t, s.doRaw(http.MethodGet, "/v1/sessions/"+sessionB+"/events", nil, bearerA))
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
}

// TestEnvironmentKeyDoesNotAuthorizeSessionCRUD pins the scope boundary: the
// environment key authorizes only the events subtree, never session CRUD — those
// stay management-only. A Bearer-only request to a non-events session route falls
// to management auth and is rejected for the missing x-api-key.
func TestEnvironmentKeyDoesNotAuthorizeSessionCRUD(t *testing.T) {
	s := newTestServer(t)
	const key = "ek-crud"
	_, sessionID := selfHostedWorker(t, s, key)
	bearer := map[string]string{"Authorization": "Bearer " + key}

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/sessions/" + sessionID},               // get session
		{http.MethodGet, "/v1/sessions"},                            // list sessions
		{http.MethodPost, "/v1/sessions/" + sessionID},              // update session
		{http.MethodPost, "/v1/sessions/" + sessionID + "/archive"}, // archive session
	} {
		st, body := readJSON(t, s.doRaw(tc.method, tc.path, nil, bearer))
		wantErr(t, st, body, http.StatusUnauthorized, "authentication_error")
	}
}
