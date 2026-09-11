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

	wantFields(t, created, "id", "created_at", "filename", "mime_type", "size_bytes", "type", "downloadable", "expires_at")
	// The reference puts expires_at on every file object and omits scope unless
	// the file has one; this upload sends no expires_in_seconds and gets no
	// scope (#651, and the parameter itself in #655).
	wantNoFields(t, created, "scope")
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
	if created["expires_at"] != nil {
		t.Errorf("expires_at = %v, want null: this upload requested no lifetime", created["expires_at"])
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
	wantFields(t, got, "expires_at")
	wantNoFields(t, got, "scope")
	if got["expires_at"] != nil {
		t.Errorf("get expires_at = %v, want null", got["expires_at"])
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

	// The extension fallback must not depend on the host's mime tables (#277):
	// .jsonl is in the platform's pinned table (internal/mimetab) but in
	// neither Go's builtin table nor a stock /etc/mime.types, so a row for it
	// proves the upload path consults the shared table, not the process mime
	// registry the harvest path already abandoned (#264).
	jsonl := s.uploadFile(t, "data.jsonl", nil, "{\"a\":1}\n")
	if jsonl["mime_type"] != "text/plain; charset=utf-8" {
		t.Errorf(".jsonl should map through the pinned table to text/plain; charset=utf-8, got %v", jsonl["mime_type"])
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
	t.Run("OversizedContent", func(t *testing.T) {
		// Lower the per-file cap and send content past it: the part-content limit
		// (not just the whole-body budget) must reject it.
		restore := api.SetMaxFileBytesForTest(1 << 10)
		defer restore()
		ct, body := fileForm(t, "big.bin", &oct, strings.Repeat("x", 4<<10))
		status, obj := s.doForm("POST", "/v1/files", ct, body)
		wantErr(t, status, obj, http.StatusRequestEntityTooLarge, "request_too_large")
	})
}

// TestFileFilenameRuneLength: the 1–255 limit is counted in characters, not
// bytes — a multibyte name well under 255 runes but over 255 bytes is accepted.
func TestFileFilenameRuneLength(t *testing.T) {
	s := newTestServer(t)
	oct := "application/octet-stream"
	// 86 CJK runes = 258 UTF-8 bytes, under the 255-rune limit.
	name := strings.Repeat("文", 86) + ".bin"
	created := s.uploadFile(t, name, &oct, "x")
	if created["filename"] != name {
		t.Errorf("filename = %v, want the multibyte name preserved", created["filename"])
	}
	// 256 runes → rejected.
	ct, body := fileForm(t, strings.Repeat("文", 256), &oct, "x")
	status, obj := s.doForm("POST", "/v1/files", ct, body)
	wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")
}

// TestFileInvalidUTF8: an unstorable byte in the filename or the part
// Content-Type must not reach the text-column bind as a 500 (#135). An invalid
// filename is a 400; an invalid part Content-Type falls through to the
// extension-derived type, so the upload still succeeds.
func TestFileInvalidUTF8(t *testing.T) {
	s := newTestServer(t)
	oct := "application/octet-stream"

	// Invalid-UTF-8 filename → 400, not 500.
	ct, body := fileForm(t, "bad\xffname.txt", &oct, "x")
	status, obj := s.doForm("POST", "/v1/files", ct, body)
	wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")

	// Invalid-UTF-8 part Content-Type → ignored, falls back to the .txt
	// extension; the upload succeeds and stores a storable mime_type.
	badType := "text/\xffplain"
	created := s.uploadFile(t, "notes.txt", &badType, "x")
	if mt, _ := created["mime_type"].(string); !strings.HasPrefix(mt, "text/plain") {
		t.Errorf("mime_type = %q, want the .txt fallback (not the unstorable header)", mt)
	}
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
	// Both ways to reach this 404, which the wire deliberately cannot tell
	// apart. The first id is well-formed and names no row. The other three
	// never reach a lookup at all: checkFileID wants both a file_ prefix and a
	// token drawn from the id alphabet, and between them these three fail each
	// half — `o` is outside that alphabet (Crockford base32 drops i, l, o and
	// u). Rejecting on shape is what keeps a byte Postgres cannot store
	// out of a bind parameter, where it would answer 500 instead of this
	// (#135).
	for _, id := range []string{
		"file_0123456789abcdefghjkmnpq",
		"file_0000000000000000000000ok",
		"not-a-file-id",
		"agent_0000000000000000000000ok",
	} {
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
	stampCreatedAt(t, s, "files", ids...)

	// Full list: newest-first (created_at desc), so the last uploaded is first.
	status, body := s.do("GET", "/v1/files", nil)
	if status != http.StatusOK {
		t.Fatalf("list: %d %v", status, body)
	}
	wantFields(t, body, "data", "next_page", "has_more", "first_id", "last_id")
	data := listData(t, body)
	if len(data) != 5 {
		t.Fatalf("list returned %d files, want 5", len(data))
	}
	if data[0]["id"] != ids[4] || data[4]["id"] != ids[0] {
		t.Errorf("list order = %v, want newest-first", []any{data[0]["id"], data[4]["id"]})
	}
	// A listed file object is the same shape as a fetched one (#651).
	wantFields(t, data[0], "expires_at")
	wantNoFields(t, data[0], "scope")
	if data[0]["expires_at"] != nil {
		t.Errorf("listed expires_at = %v, want null", data[0]["expires_at"])
	}
	if body["has_more"] != false {
		t.Errorf("has_more = %v, want false", body["has_more"])
	}
	if body["first_id"] != ids[4] || body["last_id"] != ids[0] {
		t.Errorf("first_id/last_id = %v/%v, want %s/%s", body["first_id"], body["last_id"], ids[4], ids[0])
	}
	// Five rows fit one page, so the cursor is present and null — the shape
	// every recorded reference response carries (#544).
	if np := nextPage(t, body); np != "" {
		t.Errorf("next_page on a terminal page = %q, want null", np)
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
	if nextPage(t, body) == "" {
		t.Error("page 1 next_page is null while has_more is true, want a cursor")
	}
	status, body = s.do("GET", "/v1/files?limit=2&after_id="+ids[3], nil)
	if status != http.StatusOK {
		t.Fatalf("page 2: %d %v", status, body)
	}
	data = listData(t, body)
	if len(data) != 2 || data[0]["id"] != ids[2] || data[1]["id"] != ids[1] {
		t.Fatalf("page 2 = %v, want [%s %s]", pageIDs(data), ids[2], ids[1])
	}

	// Paginate backward with before_id: newer than ids[1] is ids[2..4].
	status, body = s.do("GET", "/v1/files?limit=2&before_id="+ids[2], nil)
	if status != http.StatusOK {
		t.Fatalf("before page: %d %v", status, body)
	}
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
	wantFields(t, body, "data", "next_page", "has_more", "first_id", "last_id")

	// Unknown cursor → empty page. Bad limit / both cursors → 400.
	status, body = s.do("GET", "/v1/files?after_id=file_0000000000000000000000ok", nil)
	if status != http.StatusOK || len(listData(t, body)) != 0 {
		t.Errorf("unknown cursor: status %d, data %v", status, body["data"])
	}
	wantFields(t, body, "data", "next_page", "has_more", "first_id", "last_id")

	// A cursor that actually decodes, so the exclusivity arms below reach the
	// guard they aim at. One that does not is refused before the handler ever
	// compares it against after_id/before_id, so it would pass this table with
	// the guard deleted.
	status, body = s.do("GET", "/v1/files?limit=2", nil)
	if status != http.StatusOK {
		t.Fatalf("cursor fetch: %d %v", status, body)
	}
	cur := nextPage(t, body)
	if cur == "" {
		t.Fatal("expected a cursor to test exclusivity with")
	}
	for _, q := range []string{"limit=0", "limit=1001", "limit=abc", "after_id=x&before_id=y",
		"page=not-a-cursor",
		"page=" + cur + "&after_id=" + ids[3],
		"page=" + cur + "&before_id=" + ids[3]} {
		status, obj := s.do("GET", "/v1/files?"+q, nil)
		wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")
	}
}

// TestFileListNextPageCursor walks the whole list on the cursor the envelope
// hands out, and checks it lands on the rows the after_id walk lands on. A
// next_page that is merely present proves nothing — it has to be the position
// the reference says it is: "an opaque page cursor returned in a prior list
// response's next_page", passed back as ?page= (anthropic-sdk-go v1.70.1
// betafile.go BetaFileListParams.Page).

// TestFileListIDs covers ?ids=, the documented batch filter: "Restrict the
// result set to Files whose `id` is in this list. At most 100 entries (after
// de-duplication). Mutually exclusive with `page` and `limit`. When supplied,
// the response is always a single page (`next_page` is null). IDs that do not
// resolve to a visible File — including deleted Files — are silently omitted."
// (#652)
func TestFileListIDs(t *testing.T) {
	s := newTestServer(t)
	oct := "application/octet-stream"
	ids := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		created := s.uploadFile(t, fmt.Sprintf("f%d.bin", i), &oct, fmt.Sprintf("body-%d", i))
		ids = append(ids, created["id"].(string))
	}
	stampCreatedAt(t, s, "files", ids...)

	// Both spellings reach the same filter, through the listParam helper the
	// repeatable parameters on the other lists already share. The public docs
	// write this one `ids[]`, which is also what the SDK's encoder emits.
	for _, key := range []string{"ids", "ids[]"} {
		status, body := s.do("GET", "/v1/files?"+key+"="+ids[0]+"&"+key+"="+ids[2], nil)
		if status != http.StatusOK {
			t.Fatalf("%s list: %d %v", key, status, body)
		}
		// Still the list's own newest-first order, not the order asked in.
		if got := pageIDs(listData(t, body)); len(got) != 2 || got[0] != ids[2] || got[1] != ids[0] {
			t.Errorf("%s = %v, want [%s %s] newest-first", key, got, ids[2], ids[0])
		}
		wantFields(t, body, "data", "next_page", "has_more", "first_id", "last_id")
		if np := nextPage(t, body); np != "" {
			t.Errorf("%s next_page = %q, want null: an ids page is always terminal", key, np)
		}
		if body["has_more"] != false {
			t.Errorf("%s has_more = %v, want false", key, body["has_more"])
		}
	}

	// Unresolvable entries drop out instead of failing the request: a well-formed
	// id nothing owns, a malformed one, and a deleted one.
	deleted := s.uploadFile(t, "gone.bin", &oct, "gone")["id"].(string)
	if status, body := s.do("DELETE", "/v1/files/"+deleted, nil); status != http.StatusOK {
		t.Fatalf("delete: %d %v", status, body)
	}
	// Alphabet-legal (idAlphabet is Crockford base32 minus i/l/o/u), so this one
	// really is a well-formed id nothing owns: it survives the shape filter and
	// reaches the query, which is the only way to exercise a non-empty ids set
	// that matches nothing.
	absent := "file_0000000000000000000000hk"
	// The NUL arm is the one the id-grammar filter exists for (#135): an
	// unstorable byte must never reach the bind parameter, where Postgres would
	// answer with a 500 instead of a filtered page.
	status, body := s.do("GET", "/v1/files?ids="+ids[1]+"&ids="+absent+"&ids=not-a-file-id&ids=sesn_0000000000000000000000hm&ids=file_%00"+strings.Repeat("a", 23)+"&ids="+deleted, nil)
	if status != http.StatusOK {
		t.Fatalf("ids with misses: %d %v", status, body)
	}
	if got := pageIDs(listData(t, body)); len(got) != 1 || got[0] != ids[1] {
		t.Errorf("ids with misses = %v, want [%s]", got, ids[1])
	}

	// Every entry a miss is an empty page, not an error — the cost of the
	// silent-omission rule, and a shape a cursor client already stops on.
	status, body = s.do("GET", "/v1/files?ids="+absent, nil)
	if status != http.StatusOK {
		t.Fatalf("all-miss ids: %d %v", status, body)
	}
	if got := listData(t, body); len(got) != 0 {
		t.Errorf("all-miss ids = %v, want []", pageIDs(got))
	}
	wantFields(t, body, "data", "next_page", "has_more", "first_id", "last_id")
	if body["first_id"] != nil || body["last_id"] != nil {
		t.Errorf("empty ids page first/last = %v/%v, want null", body["first_id"], body["last_id"])
	}

	// An all-malformed request is an empty page, not an unfiltered list. This is
	// the one arm that separates "ids was supplied" from "ids resolved to
	// something": drop the distinction and every entry being dropped reads as no
	// filter at all.
	status, body = s.do("GET", "/v1/files?ids=not-a-file-id&ids=also-bad", nil)
	if status != http.StatusOK {
		t.Fatalf("all-malformed ids: %d %v", status, body)
	}
	if got := listData(t, body); len(got) != 0 {
		t.Errorf("all-malformed ids = %v, want []: a dropped entry is not an absent filter", pageIDs(got))
	}
	// Not just an empty array: a regression that matched rows and then truncated
	// them away would leave has_more true behind the same empty data.
	if body["has_more"] != false {
		t.Errorf("all-malformed has_more = %v, want false", body["has_more"])
	}
	if np := nextPage(t, body); np != "" {
		t.Errorf("all-malformed next_page = %q, want null", np)
	}

	// An empty value is not an id: ?ids= is no filter, not a filter matching
	// nothing, so the whole list comes back.
	status, body = s.do("GET", "/v1/files?ids=", nil)
	if status != http.StatusOK {
		t.Fatalf("empty ids value: %d %v", status, body)
	}
	if got := listData(t, body); len(got) == 0 {
		t.Errorf("?ids= returned an empty page, want the unfiltered list")
	}

	// De-duplication.
	status, body = s.do("GET", "/v1/files?ids="+ids[3]+"&ids="+ids[3]+"&ids="+ids[3], nil)
	if status != http.StatusOK {
		t.Fatalf("duplicate ids: %d %v", status, body)
	}
	if got := pageIDs(listData(t, body)); len(got) != 1 || got[0] != ids[3] {
		t.Errorf("duplicate ids = %v, want [%s] once", got, ids[3])
	}

	// ...and the 100 cap, which the docs count on the de-duplicated set. Building
	// the arms from well-formed ids keeps the cap the only thing under test.
	var hundred, hundredOne []string
	for i := 0; i < 101; i++ {
		one := fmt.Sprintf("ids=file_00000000000000000000%04d", i)
		if i < 100 {
			hundred = append(hundred, one)
		}
		hundredOne = append(hundredOne, one)
	}
	for _, tc := range []struct {
		name  string
		query []string
		want  int
	}{
		{"100 distinct ids", hundred, http.StatusOK},
		{"101 distinct ids", hundredOne, http.StatusBadRequest},
		// 200 entries collapsing to 100 must pass: counting the query string
		// rather than the set would reject this.
		{"200 entries, 100 distinct", append(append([]string{}, hundred...), hundred...), http.StatusOK},
		// A malformed entry is still an entry, so it consumes cap: the bound is on
		// what the caller sent, de-duplicated, not on what resolved.
		{"100 valid plus one malformed", append(append([]string{}, hundred...), "ids=not-a-file-id"), http.StatusBadRequest},
	} {
		status, body := s.do("GET", "/v1/files?"+strings.Join(tc.query, "&"), nil)
		if status != tc.want {
			t.Errorf("%s: %d, want %d (%v)", tc.name, status, tc.want, body)
		}
	}

	// More matches than the default page size still come back as one page: an ids
	// request carries no limit of its own, so nothing may silently truncate it.
	// 22 > the list default of 20 (page.go defaultLimit), which is unexported.
	bulk := make([]string, 0, 22)
	for i := 0; i < cap(bulk); i++ {
		created := s.uploadFile(t, fmt.Sprintf("bulk%d.bin", i), &oct, "b")
		bulk = append(bulk, created["id"].(string))
	}
	query := make([]string, 0, len(bulk))
	for _, id := range bulk {
		query = append(query, "ids[]="+id)
	}
	status, body = s.do("GET", "/v1/files?"+strings.Join(query, "&"), nil)
	if status != http.StatusOK {
		t.Fatalf("bulk ids: %d %v", status, body)
	}
	if got := listData(t, body); len(got) != len(bulk) {
		t.Errorf("bulk ids returned %d rows, want all %d in one page", len(got), len(bulk))
	}
	if body["has_more"] != false {
		t.Errorf("bulk ids has_more = %v, want false", body["has_more"])
	}
	if np := nextPage(t, body); np != "" {
		t.Errorf("bulk ids next_page = %q, want null", np)
	}

	// Mutually exclusive with page and limit, and with the id cursors the docs
	// name in the same breath ("not combinable with `page` or `ids[]`"). The
	// page arm needs a real cursor, or it would fail on the decode instead.
	_, first := s.do("GET", "/v1/files?limit=2", nil)
	cursor := nextPage(t, first)
	if cursor == "" {
		t.Fatalf("expected a cursor from a limit=2 page over 6 files: %v", first)
	}
	for _, tc := range []struct{ name, query string }{
		{"ids+limit", "ids=" + ids[0] + "&limit=2"},
		{"ids+page", "ids=" + ids[0] + "&page=" + cursor},
		{"ids+after_id", "ids=" + ids[0] + "&after_id=" + ids[1]},
		{"ids+before_id", "ids=" + ids[0] + "&before_id=" + ids[1]},
		// A repeated parameter whose first value is empty must not slip past:
		// reading only the first value would call this absent.
		{"ids+empty-then-real limit", "ids=" + ids[0] + "&limit=&limit=2"},
		{"ids+empty-then-real after_id", "ids=" + ids[0] + "&after_id=&after_id=" + ids[1]},
	} {
		if status, body := s.do("GET", "/v1/files?"+tc.query, nil); status != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400 (%v)", tc.name, status, body)
		}
	}

	// scope_id is not on the exclusivity list, so the two filters intersect.
	scoped, sess := "file_0000000000000000000000h1", "sesn_0000000000000000000000h1"
	if _, err := s.pool.Exec(context.Background(),
		`INSERT INTO files (id, filename, mime_type, size_bytes, downloadable, scope_type, scope_id)
		 VALUES ($1,'out.txt','text/plain',3,true,'session',$2)`, scoped, sess); err != nil {
		t.Fatalf("seed scoped row: %v", err)
	}
	status, body = s.do("GET", "/v1/files?ids="+scoped+"&ids="+ids[0]+"&scope_id="+sess, nil)
	if status != http.StatusOK {
		t.Fatalf("ids+scope_id: %d %v", status, body)
	}
	if got := pageIDs(listData(t, body)); len(got) != 1 || got[0] != scoped {
		t.Errorf("ids+scope_id = %v, want [%s]: the two filters AND", got, scoped)
	}
}

func TestFileListNextPageCursor(t *testing.T) {
	s := newTestServer(t)
	oct := "application/octet-stream"
	ids := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		created := s.uploadFile(t, fmt.Sprintf("c%d.bin", i), &oct, fmt.Sprintf("body-%d", i))
		ids = append(ids, created["id"].(string))
	}
	stampCreatedAt(t, s, "files", ids...)

	// Newest-first, so the cursor walk should report ids[4]…ids[0] in order.
	var walked []any
	path := "/v1/files?limit=2"
	for i := 0; ; i++ {
		if i > 5 {
			t.Fatalf("cursor walk did not terminate: %v", walked)
		}
		status, body := s.do("GET", path, nil)
		if status != http.StatusOK {
			t.Fatalf("cursor walk %s: %d %v", path, status, body)
		}
		wantFields(t, body, "data", "next_page", "has_more", "first_id", "last_id")
		// Per page, not just in aggregate: a continuation that ignored ?limit=
		// and returned everything left would still produce the right sequence.
		data := listData(t, body)
		if len(data) > 2 {
			t.Fatalf("page %d returned %d rows, want at most the limit of 2", i, len(data))
		}
		for _, d := range data {
			walked = append(walked, d["id"])
		}
		cur := nextPage(t, body)
		// has_more and next_page answer the same question and must agree: the
		// bug this test pins is a page that says "more rows" and hands back a
		// null cursor (#544).
		if (cur != "") != (body["has_more"] == true) {
			t.Fatalf("has_more = %v but next_page = %q", body["has_more"], cur)
		}
		if cur == "" {
			break
		}
		path = "/v1/files?limit=2&page=" + cur
	}
	want := []any{ids[4], ids[3], ids[2], ids[1], ids[0]}
	if fmt.Sprint(walked) != fmt.Sprint(want) {
		t.Errorf("cursor walk = %v, want %v", walked, want)
	}

	// A cursor from this list, replayed on a smaller page, still positions by
	// (created_at, id) rather than by page number.
	status, body := s.do("GET", "/v1/files?limit=4", nil)
	if status != http.StatusOK {
		t.Fatalf("limit=4: %d %v", status, body)
	}
	status, body = s.do("GET", "/v1/files?limit=1&page="+nextPage(t, body), nil)
	if status != http.StatusOK {
		t.Fatalf("replay: %d %v", status, body)
	}
	if data := listData(t, body); len(data) != 1 || data[0]["id"] != ids[0] {
		t.Errorf("replay page = %v, want [%s]", pageIDs(data), ids[0])
	}
}

// TestFileListBeforeIDCursor pins the one claim in #544 that has_more cannot
// speak for: on a before_id page next_page is the position of the cursor row
// itself, so the backwards lane and the forward walk join up. has_more there
// answers the id lane's own question — whether rows remain the way that page
// was fetched — so it can be false on a page that still carries a cursor, and
// that disagreement is the behavior rather than a bug in it.
func TestFileListBeforeIDCursor(t *testing.T) {
	s := newTestServer(t)
	oct := "application/octet-stream"
	ids := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		created := s.uploadFile(t, fmt.Sprintf("b%d.bin", i), &oct, fmt.Sprintf("body-%d", i))
		ids = append(ids, created["id"].(string))
	}
	stampCreatedAt(t, s, "files", ids...)

	// Only ids[3] and ids[4] are newer than ids[2], so this page exhausts the
	// backwards walk: has_more is false while rows older than it plainly remain.
	status, body := s.do("GET", "/v1/files?limit=2&before_id="+ids[2], nil)
	if status != http.StatusOK {
		t.Fatalf("before page: %d %v", status, body)
	}
	if data := listData(t, body); len(data) != 2 || data[0]["id"] != ids[4] || data[1]["id"] != ids[3] {
		t.Fatalf("before page = %v, want [%s %s]", pageIDs(listData(t, body)), ids[4], ids[3])
	}
	if body["has_more"] != false {
		t.Errorf("before page has_more = %v, want false", body["has_more"])
	}
	cur := nextPage(t, body)
	if cur == "" {
		t.Fatal("before page next_page is null; want the cursor row's own position")
	}

	// Following it lands on the before_id cursor row — no gap, no repeat.
	status, body = s.do("GET", "/v1/files?page="+cur, nil)
	if status != http.StatusOK {
		t.Fatalf("continuation: %d %v", status, body)
	}
	data := listData(t, body)
	if len(data) != 3 || data[0]["id"] != ids[2] || data[2]["id"] != ids[0] {
		t.Fatalf("continuation = %v, want [%s %s %s]", pageIDs(data), ids[2], ids[1], ids[0])
	}
	if np := nextPage(t, body); np != "" {
		t.Errorf("continuation next_page = %q, want null at the end of the list", np)
	}

	// A before_id page that does have newer rows beyond it reports has_more and
	// still continues into the cursor row.
	status, body = s.do("GET", "/v1/files?limit=2&before_id="+ids[1], nil)
	if status != http.StatusOK {
		t.Fatalf("before page 2: %d %v", status, body)
	}
	if data := listData(t, body); len(data) != 2 || data[0]["id"] != ids[3] || data[1]["id"] != ids[2] {
		t.Fatalf("before page 2 = %v, want [%s %s]", pageIDs(listData(t, body)), ids[3], ids[2])
	}
	if body["has_more"] != true {
		t.Errorf("before page 2 has_more = %v, want true", body["has_more"])
	}
	status, body = s.do("GET", "/v1/files?limit=1&page="+nextPage(t, body), nil)
	if status != http.StatusOK {
		t.Fatalf("continuation 2: %d %v", status, body)
	}
	if data := listData(t, body); len(data) != 1 || data[0]["id"] != ids[1] {
		t.Fatalf("continuation 2 = %v, want [%s]", pageIDs(listData(t, body)), ids[1])
	}

	// Nothing is newer than the newest row, so this page is empty — and an
	// empty page carries no cursor, matching the recorded reference shape even
	// though this one arm sits at the top of the list rather than its end.
	status, body = s.do("GET", "/v1/files?before_id="+ids[4], nil)
	if status != http.StatusOK {
		t.Fatalf("empty before page: %d %v", status, body)
	}
	wantFields(t, body, "data", "next_page", "has_more", "first_id", "last_id")
	if data := listData(t, body); len(data) != 0 {
		t.Errorf("empty before page = %v, want none", pageIDs(data))
	}
	if np := nextPage(t, body); np != "" {
		t.Errorf("empty before page next_page = %q, want null", np)
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
