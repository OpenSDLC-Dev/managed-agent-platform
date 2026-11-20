package executor

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api/apitest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

func TestCloudArchiveHarvest(t *testing.T) {
	const filename, contents = "report.txt", "finished before the archive"
	h := newHarness(t, &fakeSandbox{files: map[string]string{outputsDir + "/" + filename: contents}})
	ctx := t.Context()
	h.prov.owned = []domain.ID{h.sid}
	h.prov.markRunning(h.sid)
	// A queued harvest from an earlier idle still holds the session's slot.
	h.enqueueIdleHarvest(t)
	child := pgtest.NewChildThread(t, h.pool, h.sid)
	if _, err := h.pool.Exec(ctx, "UPDATE session_threads SET status = 'rescheduling' WHERE id = $1", child); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx, "UPDATE sessions SET status = 'rescheduling' WHERE id = $1", h.sid); err != nil {
		t.Fatal(err)
	}

	handler := api.NewHandler(h.pool, h.blobs, h.cipher, nil)
	key := apitest.OwnedKey(t, h.pool)
	request := func(method, path string) []byte {
		t.Helper()
		req := httptest.NewRequestWithContext(ctx, method, path, nil)
		req.Header.Set("x-api-key", key)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
		}
		return rec.Body.Bytes()
	}
	archive := "/v1/sessions/" + h.sid.String() + "/archive"
	var archived struct{ Status string }
	if err := json.Unmarshal(request(http.MethodPost, archive), &archived); err != nil {
		t.Fatal(err)
	}
	if archived.Status != "terminated" {
		t.Errorf("archived status = %q, want terminated", archived.Status)
	}
	var status string
	if err := h.pool.QueryRow(ctx, "SELECT status FROM sessions WHERE id = $1", h.sid).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "idle" {
		t.Fatalf("stored status = %q, want idle after ending the child", status)
	}

	// The archive wakes the reaper. Exercise it before the queue consumer to
	// make the race deterministic.
	if err := h.exec.reapPass(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.prov.reapedSnapshot(); len(got) != 0 {
		t.Errorf("reaped before the harvest = %v", got)
	}
	if worked, err := h.exec.step(ctx); err != nil || !worked {
		t.Errorf("executor step = %v, %v, want an archive harvest", worked, err)
	}

	var files struct {
		Data []struct{ ID, Filename string }
	}
	if err := json.Unmarshal(request(http.MethodGet, "/v1/files?scope_id="+h.sid.String()), &files); err != nil {
		t.Fatal(err)
	}
	if len(files.Data) != 1 || files.Data[0].Filename != filename {
		t.Fatalf("session files = %+v, want %s", files.Data, filename)
	}
	if got := string(request(http.MethodGet, "/v1/files/"+files.Data[0].ID+"/content")); got != contents {
		t.Errorf("download = %q, want %q", got, contents)
	}
	if h.prov.provisions != 0 {
		t.Errorf("Provision called %d times, want 0", h.prov.provisions)
	}
	if err := h.exec.reapPass(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.prov.reapedSnapshot(); len(got) != 1 || got[0] != h.sid {
		t.Errorf("reaped after the harvest = %v, want %s", got, h.sid)
	}
	request(http.MethodPost, archive)
	if worked, err := h.exec.step(ctx); err != nil || worked {
		t.Errorf("executor step after re-archive = %v, %v, want no new work", worked, err)
	}
}
