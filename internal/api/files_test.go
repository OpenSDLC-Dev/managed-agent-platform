package api_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
)

// fileForm builds a multipart body with one part named "file". A nil
// contentType omits the part Content-Type header; an empty filename omits the
// filename from the Content-Disposition.
func fileForm(t *testing.T, filename string, contentType *string, content string) (ct, body string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	h := textproto.MIMEHeader{}
	if filename != "" {
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, filename))
	} else {
		h.Set("Content-Disposition", `form-data; name="file"`)
	}
	if contentType != nil {
		h.Set("Content-Type", *contentType)
	}
	pw, err := w.CreatePart(h)
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := pw.Write([]byte(content)); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close form: %v", err)
	}
	return w.FormDataContentType(), buf.String()
}

// uploadFile uploads one file and returns the create response object.
func (s *tserver) uploadFile(t *testing.T, filename string, contentType *string, content string) map[string]any {
	t.Helper()
	ct, body := fileForm(t, filename, contentType, content)
	status, obj := s.doForm("POST", "/v1/files", ct, body)
	if status != http.StatusOK {
		t.Fatalf("upload %s: status %d body %v", filename, status, obj)
	}
	return obj
}

// TestFileUploadRoundTrip is the slice-1 end-to-end integration test: a real
// control-plane handler over a real Postgres (pgtest) and blob store, exercised
// entirely through HTTP — upload → get → the metadata contract.
func TestFileUploadRoundTrip(t *testing.T) {
	s := newTestServer(t)
	pdf := "application/pdf"
	created := s.uploadFile(t, "report.pdf", &pdf, "%PDF-1.7 fake body")

	wantFields(t, created, "id", "created_at", "filename", "mime_type", "size_bytes", "type", "downloadable", "scope")
	id, _ := created["id"].(string)
	if !strings.HasPrefix(id, "file_") {
		t.Errorf("id = %q, want a file_ id", id)
	}
	if created["filename"] != "report.pdf" {
		t.Errorf("filename = %v, want report.pdf", created["filename"])
	}
	if created["mime_type"] != "application/pdf" {
		t.Errorf("mime_type = %v, want application/pdf", created["mime_type"])
	}
	if created["type"] != "file" {
		t.Errorf("type = %v, want file", created["type"])
	}
	if created["downloadable"] != false {
		t.Errorf("downloadable = %v, want false", created["downloadable"])
	}
	if created["scope"] != nil {
		t.Errorf("scope = %v, want null for an upload", created["scope"])
	}
	// size_bytes is a JSON number; the harness decodes it as float64.
	if n, _ := created["size_bytes"].(float64); int(n) != len("%PDF-1.7 fake body") {
		t.Errorf("size_bytes = %v, want %d", created["size_bytes"], len("%PDF-1.7 fake body"))
	}

	// The object landed in the store under files/{id}.
	if _, _, err := s.blobs.Get(context.Background(), "files/"+id); err != nil {
		t.Errorf("blob files/%s not stored: %v", id, err)
	}

	status, got := s.do("GET", "/v1/files/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("get: %d %v", status, got)
	}
	for _, k := range []string{"id", "filename", "mime_type", "type", "downloadable"} {
		if got[k] != created[k] {
			t.Errorf("get %s = %v, create returned %v", k, got[k], created[k])
		}
	}
}

// TestFileMimeTypeFallback covers the derivation: an explicit part Content-Type
// wins; a generic/absent one falls back to the filename extension.
func TestFileMimeTypeFallback(t *testing.T) {
	s := newTestServer(t)

	octet := "application/octet-stream"
	byExt := s.uploadFile(t, "notes.txt", &octet, "hello")
	if mt, _ := byExt["mime_type"].(string); !strings.HasPrefix(mt, "text/plain") {
		t.Errorf("generic part type should fall back to the .txt extension, got %v", byExt["mime_type"])
	}

	noType := s.uploadFile(t, "data.bin", nil, "\x00\x01\x02")
	if noType["mime_type"] != "application/octet-stream" {
		t.Errorf("unknown extension should be application/octet-stream, got %v", noType["mime_type"])
	}
}

func TestFileUploadValidation(t *testing.T) {
	s := newTestServer(t)
	oct := "application/octet-stream"

	t.Run("MissingFilename", func(t *testing.T) {
		ct, body := fileForm(t, "", &oct, "x")
		status, obj := s.doForm("POST", "/v1/files", ct, body)
		wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")
	})
	t.Run("ForbiddenChar", func(t *testing.T) {
		ct, body := fileForm(t, "a/b.txt", &oct, "x")
		status, obj := s.doForm("POST", "/v1/files", ct, body)
		wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")
	})
	t.Run("WrongPartName", func(t *testing.T) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="files[]"; filename="x.txt"`)
		pw, _ := w.CreatePart(h)
		_, _ = pw.Write([]byte("x"))
		_ = w.Close()
		status, obj := s.doForm("POST", "/v1/files", w.FormDataContentType(), buf.String())
		wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")
	})
	t.Run("DuplicatePart", func(t *testing.T) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		for _, n := range []string{"a.txt", "b.txt"} {
			h := textproto.MIMEHeader{}
			h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, n))
			pw, _ := w.CreatePart(h)
			_, _ = pw.Write([]byte("x"))
		}
		_ = w.Close()
		status, obj := s.doForm("POST", "/v1/files", w.FormDataContentType(), buf.String())
		wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")
	})
	t.Run("NotMultipart", func(t *testing.T) {
		status, obj := s.doForm("POST", "/v1/files", "application/json", `{}`)
		wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")
	})
	t.Run("FilenameTooLong", func(t *testing.T) {
		ct, body := fileForm(t, strings.Repeat("a", 256)+".bin", &oct, "x")
		status, obj := s.doForm("POST", "/v1/files", ct, body)
		wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")
	})
	t.Run("OversizedBody", func(t *testing.T) {
		restore := api.SetMaxFileUploadBytesForTest(1 << 10)
		defer restore()
		ct, body := fileForm(t, "big.bin", &oct, strings.Repeat("x", 4<<10))
		status, obj := s.doForm("POST", "/v1/files", ct, body)
		wantErr(t, status, obj, http.StatusRequestEntityTooLarge, "request_too_large")
	})
}

// TestFileDownloadGate: an uploaded file is not downloadable (public docs: 400),
// while a file marked downloadable streams its bytes with the metadata headers.
// The downloadable path is seeded directly (no API produces one in slice 1).
func TestFileDownloadGate(t *testing.T) {
	s := newTestServer(t)
	oct := "application/octet-stream"
	created := s.uploadFile(t, "upload.bin", &oct, "secret upload")
	id, _ := created["id"].(string)

	// Uploaded → 400.
	res := s.doRaw("GET", "/v1/files/"+id+"/content", nil, map[string]string{"x-api-key": testKey})
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("download of an upload: status %d, want 400", res.StatusCode)
	}

	// Seed a downloadable file (an object plus a row) as a tool would.
	genID := "file_0000000000000000000000gk"
	content := []byte("generated chart bytes")
	if err := s.blobs.Put(context.Background(), "files/"+genID, bytes.NewReader(content), int64(len(content)), "image/png"); err != nil {
		t.Fatalf("seed blob: %v", err)
	}
	if _, err := s.pool.Exec(context.Background(),
		`INSERT INTO files (id, filename, mime_type, size_bytes, downloadable) VALUES ($1,$2,$3,$4,true)`,
		genID, "chart.png", "image/png", len(content)); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	res = s.doRaw("GET", "/v1/files/"+genID+"/content", nil, map[string]string{"x-api-key": testKey})
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("download of a downloadable file: status %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("content-type = %q, want image/png", ct)
	}
	if cl := res.Header.Get("Content-Length"); cl != fmt.Sprint(len(content)) {
		t.Errorf("content-length = %q, want %d", cl, len(content))
	}
	if cd := res.Header.Get("Content-Disposition"); !strings.Contains(cd, "chart.png") {
		t.Errorf("content-disposition = %q, want it to name chart.png", cd)
	}
	got, _ := io.ReadAll(res.Body)
	if !bytes.Equal(got, content) {
		t.Errorf("downloaded bytes = %q, want %q", got, content)
	}
}

func TestFileDelete(t *testing.T) {
	s := newTestServer(t)
	oct := "application/octet-stream"
	created := s.uploadFile(t, "gone.bin", &oct, "bytes")
	id, _ := created["id"].(string)

	status, obj := s.do("DELETE", "/v1/files/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("delete: %d %v", status, obj)
	}
	if obj["id"] != id || obj["type"] != "file_deleted" {
		t.Errorf("delete response = %v, want {id:%s, type:file_deleted}", obj, id)
	}
	// The object is gone too.
	if _, _, err := s.blobs.Get(context.Background(), "files/"+id); err == nil {
		t.Errorf("blob files/%s still present after delete", id)
	}
	// Second delete → 404; get → 404.
	status, obj = s.do("DELETE", "/v1/files/"+id, nil)
	wantErr(t, status, obj, http.StatusNotFound, "not_found_error")
	status, obj = s.do("GET", "/v1/files/"+id, nil)
	wantErr(t, status, obj, http.StatusNotFound, "not_found_error")
}

func TestFileNotFound(t *testing.T) {
	s := newTestServer(t)
	// Well-formed but unknown → 404; malformed id → 404 (shape-rejected).
	for _, id := range []string{"file_0000000000000000000000ok", "not-a-file-id", "agent_0000000000000000000000ok"} {
		status, obj := s.do("GET", "/v1/files/"+id, nil)
		wantErr(t, status, obj, http.StatusNotFound, "not_found_error")
	}
}

func TestFileList(t *testing.T) {
	s := newTestServer(t)
	oct := "application/octet-stream"
	ids := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		created := s.uploadFile(t, fmt.Sprintf("f%d.bin", i), &oct, fmt.Sprintf("body-%d", i))
		ids = append(ids, created["id"].(string))
	}

	// Full list: newest-first (created_at desc), so the last uploaded is first.
	status, body := s.do("GET", "/v1/files", nil)
	if status != http.StatusOK {
		t.Fatalf("list: %d %v", status, body)
	}
	wantFields(t, body, "data", "has_more", "first_id", "last_id")
	data := listData(t, body)
	if len(data) != 5 {
		t.Fatalf("list returned %d files, want 5", len(data))
	}
	if data[0]["id"] != ids[4] || data[4]["id"] != ids[0] {
		t.Errorf("list order = %v, want newest-first", []any{data[0]["id"], data[4]["id"]})
	}
	if body["has_more"] != false {
		t.Errorf("has_more = %v, want false", body["has_more"])
	}
	if body["first_id"] != ids[4] || body["last_id"] != ids[0] {
		t.Errorf("first_id/last_id = %v/%v, want %s/%s", body["first_id"], body["last_id"], ids[4], ids[0])
	}

	// Paginate forward with limit=2 + after_id.
	status, body = s.do("GET", "/v1/files?limit=2", nil)
	if status != http.StatusOK {
		t.Fatalf("list p1: %d %v", status, body)
	}
	data = listData(t, body)
	if len(data) != 2 || data[0]["id"] != ids[4] || data[1]["id"] != ids[3] {
		t.Fatalf("page 1 = %v, want [%s %s]", pageIDs(data), ids[4], ids[3])
	}
	if body["has_more"] != true {
		t.Errorf("page 1 has_more = %v, want true", body["has_more"])
	}
	status, body = s.do("GET", "/v1/files?limit=2&after_id="+ids[3], nil)
	data = listData(t, body)
	if len(data) != 2 || data[0]["id"] != ids[2] || data[1]["id"] != ids[1] {
		t.Fatalf("page 2 = %v, want [%s %s]", pageIDs(data), ids[2], ids[1])
	}

	// Paginate backward with before_id: newer than ids[1] is ids[2..4].
	status, body = s.do("GET", "/v1/files?limit=2&before_id="+ids[2], nil)
	data = listData(t, body)
	if len(data) != 2 || data[0]["id"] != ids[4] || data[1]["id"] != ids[3] {
		t.Fatalf("before page = %v, want [%s %s]", pageIDs(data), ids[4], ids[3])
	}

	// scope_id matches nothing (uploads have no scope in v1).
	status, body = s.do("GET", "/v1/files?scope_id=sesn_0000000000000000000000ok", nil)
	if status != http.StatusOK {
		t.Fatalf("scope list: %d %v", status, body)
	}
	if data := listData(t, body); len(data) != 0 {
		t.Errorf("scope_id filter returned %d files, want 0", len(data))
	}

	// Unknown cursor → empty page. Bad limit / both cursors → 400.
	status, body = s.do("GET", "/v1/files?after_id=file_0000000000000000000000ok", nil)
	if status != http.StatusOK || len(listData(t, body)) != 0 {
		t.Errorf("unknown cursor: status %d, data %v", status, body["data"])
	}
	for _, q := range []string{"limit=0", "limit=1001", "limit=abc", "after_id=x&before_id=y"} {
		status, obj := s.do("GET", "/v1/files?"+q, nil)
		wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")
	}
}

func TestFileMethodNotAllowed(t *testing.T) {
	s := newTestServer(t)
	status, obj := s.do("PUT", "/v1/files", nil)
	wantErr(t, status, obj, http.StatusMethodNotAllowed, "invalid_request_error")
}

func pageIDs(data []map[string]any) []any {
	out := make([]any, len(data))
	for i, d := range data {
		out[i] = d["id"]
	}
	return out
}
