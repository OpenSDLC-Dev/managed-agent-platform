package api_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
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
	agentID, envID := fixture(t, s)
	const wkey = "wkey_files_lane"
	if err := api.EnsureEnvironmentKey(context.Background(), s.pool, envID, wkey); err != nil {
		t.Fatalf("EnsureEnvironmentKey: %v", err)
	}
	bearer := map[string]string{"Authorization": "Bearer " + wkey}
	oct := "application/octet-stream"

	// A file mounted by a session in this environment: the worker reads its bytes
	// over the env lane even though an upload is downloadable=false.
	mounted := s.uploadFile(t, "mounted.bin", &oct, "mounted secret")
	mountedID := mounted["id"].(string)
	createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{map[string]any{"type": "file", "file_id": mountedID}},
	})

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
	// existence.
	unmounted := s.uploadFile(t, "unmounted.bin", &oct, "private")
	unmountedID := unmounted["id"].(string)
	res = s.doRaw("GET", "/v1/files/"+unmountedID+"/content", nil, bearer)
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("env-key download of an unmounted file = %d, want 404", res.StatusCode)
	}

	// A file mounted only by a session in a DIFFERENT environment: still 404 for
	// this key — the scope is per-environment, not workspace-global.
	otherEnv := createEnvironment(t, s, map[string]any{"name": "other-env"})
	otherID := otherEnv["id"].(string)
	crossed := s.uploadFile(t, "crossed.bin", &oct, "other env secret")
	crossedID := crossed["id"].(string)
	createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": otherID,
		"resources": []any{map[string]any{"type": "file", "file_id": crossedID}},
	})
	res = s.doRaw("GET", "/v1/files/"+crossedID+"/content", nil, bearer)
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("env-key download of a file mounted in another environment = %d, want 404 (cross-env denied)", res.StatusCode)
	}

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
