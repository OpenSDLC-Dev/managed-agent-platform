package api_test

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// TestFileContentEnvironmentKeyLane pins slice 4's worker download lane: a BYOC
// worker pulls a mounted file's bytes with the same Authorization: Bearer
// environment key it polls work with, over GET /v1/files/{id}/content — and only
// that route. Unlike workspace-global skills, file content can be sensitive, so
// the key is scoped to files a session in its own environment mounts (decision
// 10): the downloadable gate is skipped on this lane, but a file no session in
// the environment mounts is answered as absent.
func TestFileContentEnvironmentKeyLane(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := selfHostedFixture(t, s)
	wkey := issueKey(t, s.pool, envID, "files-lane")
	bearer := map[string]string{"Authorization": "Bearer " + wkey}
	oct := "application/octet-stream"

	// A file mounted by a session in this environment: the worker reads its bytes
	// over the env lane even though an upload is downloadable=false. What the
	// session mounts is its own copy (#578), the id its resources[] echo and a
	// worker reads; the upload itself no session mounts, so it is answered as
	// absent like any other.
	upload := s.uploadFile(t, "mounted.bin", &oct, "mounted secret")
	uploadID := upload["id"].(string)
	mountedID := mountedFileID(t, createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{map[string]any{"type": "file", "file_id": uploadID}},
	}))
	status, obj := readJSON(t, s.doRaw("GET", "/v1/files/"+uploadID+"/content", nil, bearer))
	wantErrMsg(t, status, obj, http.StatusNotFound, "not_found_error", "Not found")

	res := s.doRaw("GET", "/v1/files/"+mountedID+"/content", nil, bearer)
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("env-key download of a mounted upload = %d, want 200 (gate skipped on the lane)", res.StatusCode)
	}
	if !bytes.Equal(body, []byte("mounted secret")) {
		t.Errorf("downloaded bytes = %q, want the uploaded content", body)
	}

	// A file no session in this environment mounts: 404, indistinguishable from
	// absent, so a leaked env key can neither read arbitrary files nor probe their
	// existence. Every 404 on this lane is the reference's bare "Not found"
	// (2026-09-03 batch2 idx 20 `envkey.files.content-wrong-environment`,
	// #540), the message included: an absent id, a malformed one and an
	// expired mounted file answer it too, below.
	unmounted := s.uploadFile(t, "unmounted.bin", &oct, "private")
	unmountedID := unmounted["id"].(string)
	status, obj = readJSON(t, s.doRaw("GET", "/v1/files/"+unmountedID+"/content", nil, bearer))
	wantErrMsg(t, status, obj, http.StatusNotFound, "not_found_error", "Not found")

	// A file mounted only by a session in a DIFFERENT environment: still 404 for
	// this key — the scope is per-environment, not workspace-global.
	otherEnv := createEnvironment(t, s, map[string]any{"name": "other-env"})
	otherID := otherEnv["id"].(string)
	crossed := s.uploadFile(t, "crossed.bin", &oct, "other env secret")
	crossedID := mountedFileID(t, createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": otherID,
		"resources": []any{map[string]any{"type": "file", "file_id": crossed["id"]}},
	}))
	status, obj = readJSON(t, s.doRaw("GET", "/v1/files/"+crossedID+"/content", nil, bearer))
	wantErrMsg(t, status, obj, http.StatusNotFound, "not_found_error", "Not found")

	// An absent id, a malformed one and a mounted file past its expiry take the
	// same words on this lane; the management lane, never recorded missing a
	// file, keeps ours.
	expiring := s.uploadFile(t, "expiring.bin", &oct, "soon gone")
	expiringID := mountedFileID(t, createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{map[string]any{"type": "file", "file_id": expiring["id"]}},
	}))
	expire(t, s, expiringID)
	absentID := "file_0000000000000000000000ab"
	for _, id := range []string{absentID, "file_0000000000000000000000ok", expiringID} {
		status, obj = readJSON(t, s.doRaw("GET", "/v1/files/"+id+"/content", nil, bearer))
		wantErrMsg(t, status, obj, http.StatusNotFound, "not_found_error", "Not found")
	}
	status, obj = s.do("GET", "/v1/files/"+absentID+"/content", nil)
	wantErrMsg(t, status, obj, http.StatusNotFound, "not_found_error", "file "+absentID+" not found")

	// The metadata GET, the list, and mutations stay management-only: an
	// environment key gets the management lane's 401, never the file lane.
	for _, probe := range []struct{ method, path string }{
		{"GET", "/v1/files/" + mountedID},    // metadata read is NOT in the file lane
		{"GET", "/v1/files"},                 // the collection list
		{"DELETE", "/v1/files/" + mountedID}, // a mutation
	} {
		res := s.doRaw(probe.method, probe.path, nil, bearer)
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("env-key %s %s = %d, want 401 (management-only)", probe.method, probe.path, res.StatusCode)
		}
	}

	// An invalid env key is rejected; a valid x-api-key alongside a Bearer keeps
	// the management lane, where the downloadable gate still 400s the upload.
	res = s.doRaw("GET", "/v1/files/"+mountedID+"/content", nil, map[string]string{"Authorization": "Bearer nope"})
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad env key = %d, want 401", res.StatusCode)
	}
	res = s.doRaw("GET", "/v1/files/"+mountedID+"/content", nil,
		map[string]string{"Authorization": "Bearer nope", "x-api-key": testKey})
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("x-api-key alongside a Bearer = %d, want the management lane's downloadable-gate 400", res.StatusCode)
	}
}

// TestFileContentCloudKeyRefusalIsLogged: a cloud environment's key is refused
// the file lane before any file is looked up, in the reference's bare words
// (2026-09-03 batch2 idx 18 `envkey.files.content-original`, idx 19
// `envkey.files.content-session-copy`; #540) — the same words as an absent
// file, so the wire no longer says which of the lane's refusals fired. The
// operator's log does, naming the key's environment.
func TestFileContentCloudKeyRefusalIsLogged(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	oct := "application/octet-stream"
	fileID := s.uploadFile(t, "mounted.bin", &oct, "cloud mounted")["id"].(string)
	createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{map[string]any{"type": "file", "file_id": fileID}},
	})
	bearer := asBearer(issueViaConsole(t, s, envID, "cloud-files"))

	logs := captureLogs(t, slog.LevelInfo)
	status, body := readJSON(t, s.doRaw("GET", "/v1/files/"+fileID+"/content", nil, bearer))
	wantErrMsg(t, status, body, http.StatusNotFound, "not_found_error", "Not found")
	line := ""
	for _, l := range strings.Split(logs(), "\n") {
		if strings.Contains(l, "file download refused: environment key is not self_hosted") {
			line = l
		}
	}
	for _, want := range []string{"environment_id=" + envID, "environment_kind=cloud",
		"request_id=" + body["request_id"].(string)} {
		if !strings.Contains(line, want) {
			t.Errorf("cloud-key refusal log line %q lacks %q", line, want)
		}
	}
}
