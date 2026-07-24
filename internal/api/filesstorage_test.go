package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/blobtest"
)

// TestFilesUnavailableWithoutObjectStorage: a deployment configured without
// object storage keeps serving everything else; the storage-backed file routes
// report the absence cleanly (500 api_error), the read routes still answer.
func TestFilesUnavailableWithoutObjectStorage(t *testing.T) {
	pool := newPoolWithKey(t)
	srv := httptest.NewServer(api.NewHandler(pool, nil, nil))
	t.Cleanup(srv.Close)
	s := &tserver{t: t, url: srv.URL, pool: pool}
	oct := "application/octet-stream"

	ct, body := fileForm(t, "x.bin", &oct, "x")
	status, obj := s.doForm("POST", "/v1/files", ct, body)
	wantErr(t, status, obj, http.StatusInternalServerError, "api_error")

	status, obj = s.do("DELETE", "/v1/files/file_0000000000000000000000gk", nil)
	wantErr(t, status, obj, http.StatusInternalServerError, "api_error")

	res := s.doRaw("GET", "/v1/files/file_0000000000000000000000gk/content", nil,
		map[string]string{"x-api-key": testKey})
	var dl map[string]any
	raw, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatalf("read download error body: %v", err)
	}
	if err := json.Unmarshal(raw, &dl); err != nil {
		t.Fatalf("decode download error body: %v", err)
	}
	wantErr(t, res.StatusCode, dl, http.StatusInternalServerError, "api_error")

	// The list read still answers from the database.
	if status, obj := s.do("GET", "/v1/files", nil); status != http.StatusOK {
		t.Errorf("list without object storage: %d %v", status, obj)
	}
}

// TestFileFailedPutCommitsNoRows: a storage Put failure must never leave a
// committed metadata row behind (the claim-row → put → commit ordering).
// failingStore is defined in skills_test.go (same package).
func TestFileFailedPutCommitsNoRows(t *testing.T) {
	pool := newPoolWithKey(t)
	working := blobtest.Mem()
	okSrv := httptest.NewServer(api.NewHandler(pool, working, nil))
	t.Cleanup(okSrv.Close)
	badSrv := httptest.NewServer(api.NewHandler(pool, failingStore{working}, nil))
	t.Cleanup(badSrv.Close)
	ok := &tserver{t: t, url: okSrv.URL, pool: pool, blobs: working}
	bad := &tserver{t: t, url: badSrv.URL, pool: pool, blobs: working}
	oct := "application/octet-stream"

	ct, body := fileForm(t, "x.bin", &oct, "bytes")
	status, obj := bad.doForm("POST", "/v1/files", ct, body)
	wantErr(t, status, obj, http.StatusInternalServerError, "api_error")
	if status, list := ok.do("GET", "/v1/files", nil); status != http.StatusOK || len(listData(t, list)) != 0 {
		t.Fatalf("after failed create: %d %v, want an empty list", status, list)
	}
}

// TestFileScopeRendered: a scoped file (as a session-created output would be)
// renders its scope object and is reachable through the ?scope_id= filter. The
// row is seeded directly — no API produces a scoped file in slice 1.
func TestFileScopeRendered(t *testing.T) {
	s := newTestServer(t)
	id := "file_0000000000000000000000gk"
	sess := "sesn_0000000000000000000000gk"
	content := []byte("scoped output")
	if err := s.blobs.Put(context.Background(), "files/"+id, bytes.NewReader(content), int64(len(content)), "image/png"); err != nil {
		t.Fatalf("seed blob: %v", err)
	}
	if _, err := s.pool.Exec(context.Background(),
		`INSERT INTO files (id, filename, mime_type, size_bytes, downloadable, scope_type, scope_id)
		 VALUES ($1,$2,$3,$4,true,'session',$5)`,
		id, "chart.png", "image/png", len(content), sess); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	status, obj := s.do("GET", "/v1/files/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("get scoped file: %d %v", status, obj)
	}
	scope, _ := obj["scope"].(map[string]any)
	if scope == nil || scope["id"] != sess || scope["type"] != "session" {
		t.Errorf("scope = %v, want {id:%s, type:session}", obj["scope"], sess)
	}

	status, body := s.do("GET", "/v1/files?scope_id="+sess, nil)
	if status != http.StatusOK {
		t.Fatalf("scoped list: %d %v", status, body)
	}
	data := listData(t, body)
	if len(data) != 1 || data[0]["id"] != id {
		t.Errorf("scope_id filter = %v, want [%s]", pageIDs(data), id)
	}
}
