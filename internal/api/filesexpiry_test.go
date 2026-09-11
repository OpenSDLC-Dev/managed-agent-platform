package api_test

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
)

// expiringFileForm builds a multipart body with a file part and an
// expires_in_seconds part carrying exactly the bytes given — no encoder
// normalization, so a test can send " 3600" or "3600.0" and see what the parser
// does with it. paramFirst writes the parameter ahead of the file, which is the
// order-independence the parser claims (RFC 7578 §5.2).
func expiringFileForm(t *testing.T, filename, content, expires string, paramFirst bool) (ct, body string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	writeParam := func() {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="expires_in_seconds"`)
		pw, err := w.CreatePart(h)
		if err != nil {
			t.Fatalf("create param part: %v", err)
		}
		if _, err := pw.Write([]byte(expires)); err != nil {
			t.Fatalf("write param part: %v", err)
		}
	}
	if paramFirst {
		writeParam()
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, filename))
	h.Set("Content-Type", "application/octet-stream")
	pw, err := w.CreatePart(h)
	if err != nil {
		t.Fatalf("create file part: %v", err)
	}
	if _, err := pw.Write([]byte(content)); err != nil {
		t.Fatalf("write file part: %v", err)
	}
	if !paramFirst {
		writeParam()
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close form: %v", err)
	}
	return w.FormDataContentType(), buf.String()
}

// wantTime parses an RFC 3339 field the wire is supposed to carry.
func wantTime(t *testing.T, obj map[string]any, field string) time.Time {
	t.Helper()
	raw, ok := obj[field].(string)
	if !ok {
		t.Fatalf("%s = %v, want an RFC 3339 string", field, obj[field])
	}
	ts, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("%s = %q: %v", field, raw, err)
	}
	return ts
}

// TestFileUploadExpiresInSeconds pins the documented upload parameter (#655):
// "an integer number of seconds between 3,600 (1 hour) and 7,776,000 (90 days)",
// with expires_at "the upload time plus that value". Before this, every name but
// "file" was a hard 400, so a client that set an expiry failed its upload.
func TestFileUploadExpiresInSeconds(t *testing.T) {
	s := newTestServer(t)

	// The instant is exact, not approximate: created_at defaults to now() and
	// expires_at is computed from the same now() in the same statement, so the
	// difference is the requested lifetime to the nanosecond. A Go-side clock
	// would make this assertion impossible to write.
	for _, order := range []struct {
		name       string
		paramFirst bool
	}{{"file first", false}, {"parameter first", true}} {
		t.Run(order.name, func(t *testing.T) {
			ct, body := expiringFileForm(t, "note.txt", "hello", "3600", order.paramFirst)
			status, obj := s.doForm("POST", "/v1/files", ct, body)
			if status != http.StatusOK {
				t.Fatalf("upload with expires_in_seconds: status %d body %v", status, obj)
			}
			created, expires := wantTime(t, obj, "created_at"), wantTime(t, obj, "expires_at")
			if d := expires.Sub(created); d != time.Hour {
				t.Errorf("expires_at - created_at = %v, want exactly 1h", d)
			}
			// And it survives the round trip to the metadata route, which reads
			// the column rather than the value the insert returned.
			gotStatus, got := s.do("GET", "/v1/files/"+obj["id"].(string), nil)
			if gotStatus != http.StatusOK {
				t.Fatalf("get: status %d body %v", gotStatus, got)
			}
			if reread := wantTime(t, got, "expires_at"); !reread.Equal(expires) {
				t.Errorf("stored expires_at = %v, want the uploaded %v", reread, expires)
			}
		})
	}

	// An upload that sends no parameter still answers the key, null — the shape
	// #651 fixed, and the one the docs call "null for files uploaded without an
	// expiration".
	oct := "application/octet-stream"
	plain := s.uploadFile(t, "plain.txt", &oct, "x")
	if v, ok := plain["expires_at"]; !ok || v != nil {
		t.Errorf("expires_at on an upload with no parameter = %v (present=%v), want null", v, ok)
	}

	// The bounds are inclusive at both ends, and everything that is not an
	// integer in range is a 400 rather than a silently clamped or ignored value.
	//
	// wantMsg is asserted where the refusal distinguishes two kinds of wrong:
	// a value that is not an integer, and one that is an integer outside the
	// range. Nothing on the wire reveals which a server chose, so without these
	// the two could be swapped and no test would notice.
	rangeMsg := "expires_in_seconds must be between 3600 and 7776000"
	integerMsg := "expires_in_seconds must be an integer number of seconds"
	for _, tc := range []struct {
		name, value string
		wantOK      bool
		wantMsg     string
	}{
		{"minimum", "3600", true, ""},
		{"maximum", "7776000", true, ""},
		{"one below the minimum", "3599", false, rangeMsg},
		{"one above the maximum", "7776001", false, rangeMsg},
		{"zero", "0", false, rangeMsg},
		{"negative", "-3600", false, rangeMsg},
		{"empty", "", false, integerMsg},
		{"not a number", "an hour", false, integerMsg},
		{"fractional", "3600.0", false, integerMsg},
		{"padded", " 3600", false, integerMsg},
		{"scientific", "36e2", false, integerMsg},
		{"absurdly long", strings.Repeat("9", 40), false, integerMsg},
		// strconv.ParseInt's grammar, pinned because it is not "digits" and
		// nothing in the wire error reveals which spellings are taken: a
		// different spelling of the same integer is the same request.
		{"signed", "+3600", true, ""},
		{"zero padded", "0000003600", true, ""},
		// Too large for an int64 — it parses, it is simply not a lifetime, so
		// it is reported as out of range rather than as malformed.
		{"beyond int64", strings.Repeat("9", 25), false, rangeMsg},
		// One byte past the cap, and the 32-byte prefix is a VALID in-range
		// value (3600). A parser that truncated instead of rejecting would
		// accept this as an hour; only a length reject fails it, so this is the
		// arm that can tell the two apart. The 40-nines arm above cannot: its
		// prefix is out of range either way.
		{"one past the cap, valid if truncated", strings.Repeat("0", 28) + "36000", false, integerMsg},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ct, body := expiringFileForm(t, "b.txt", "x", tc.value, false)
			status, obj := s.doForm("POST", "/v1/files", ct, body)
			if tc.wantOK {
				if status != http.StatusOK {
					t.Fatalf("expires_in_seconds=%q: status %d body %v, want 200", tc.value, status, obj)
				}
				if _, ok := obj["expires_at"].(string); !ok {
					t.Errorf("expires_in_seconds=%q: expires_at = %v, want an instant", tc.value, obj["expires_at"])
				}
				return
			}
			if tc.wantMsg != "" {
				wantErrMsg(t, status, obj, http.StatusBadRequest, "invalid_request_error", tc.wantMsg)
				return
			}
			wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")
		})
	}

	// Duplicates are refused the way a duplicate file part is: the parser does
	// not pick a winner.
	t.Run("duplicate parameter", func(t *testing.T) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="file"; filename="d.txt"`)
		pw, _ := w.CreatePart(h)
		_, _ = pw.Write([]byte("x"))
		for _, v := range []string{"3600", "7200"} {
			ph := textproto.MIMEHeader{}
			ph.Set("Content-Disposition", `form-data; name="expires_in_seconds"`)
			pp, _ := w.CreatePart(ph)
			_, _ = pp.Write([]byte(v))
		}
		_ = w.Close()
		status, obj := s.doForm("POST", "/v1/files", w.FormDataContentType(), buf.String())
		wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")
	})

	// A name that is neither part is still rejected: admitting one parameter did
	// not open the form to anything else.
	t.Run("still strict about other names", func(t *testing.T) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="file"; filename="e.txt"`)
		pw, _ := w.CreatePart(h)
		_, _ = pw.Write([]byte("x"))
		ph := textproto.MIMEHeader{}
		ph.Set("Content-Disposition", `form-data; name="expires_in"`)
		pp, _ := w.CreatePart(ph)
		_, _ = pp.Write([]byte("3600"))
		_ = w.Close()
		status, obj := s.doForm("POST", "/v1/files", w.FormDataContentType(), buf.String())
		wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")
	})
}

// expire moves a file's expiry just into the past. The documented minimum
// lifetime is an hour, so no upload can produce an expired file inside a test;
// the column is what the routes read, and expireBy writes it directly.
func expire(t *testing.T, s *tserver, id string) {
	t.Helper()
	expireBy(t, s, id, time.Second)
}

// TestExpiredFileLifecycle pins what the public docs say happens at expires_at:
// the content route 404s, while the metadata route and the list keep answering
// for the grace window, and DELETE still removes the row immediately.
func TestExpiredFileLifecycle(t *testing.T) {
	s := newTestServer(t)
	oct := "application/octet-stream"

	// A downloadable file is the one that can show the content route changing
	// its mind: 200 before, 404 after. An upload (downloadable=false) shows the
	// other half — see the ordering assertion below.
	genID := "file_0000000000000000000000mk"
	content := []byte("generated bytes")
	if err := s.blobs.Put(context.Background(), blob.FilesKey(genID),
		bytes.NewReader(content), int64(len(content)), "text/plain"); err != nil {
		t.Fatalf("seed blob: %v", err)
	}
	if _, err := s.pool.Exec(context.Background(),
		`INSERT INTO files (id, filename, mime_type, size_bytes, downloadable)
		 VALUES ($1,'out.txt','text/plain',$2,true)`, genID, len(content)); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	res := s.doRaw("GET", "/v1/files/"+genID+"/content", nil, map[string]string{"x-api-key": testKey})
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("download before expiry = %d, want 200", res.StatusCode)
	}
	expire(t, s, genID)
	res = s.doRaw("GET", "/v1/files/"+genID+"/content", nil, map[string]string{"x-api-key": testKey})
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("download after expiry = %d, want 404", res.StatusCode)
	}

	// The metadata route still answers, with the instant in the past — "remains
	// readable for up to 30 days, with expires_at in the past".
	status, obj := s.do("GET", "/v1/files/"+genID, nil)
	if status != http.StatusOK {
		t.Fatalf("metadata after expiry = %d, want 200 (the grace window): %v", status, obj)
	}
	if ts := wantTime(t, obj, "expires_at"); !ts.Before(time.Now()) {
		t.Errorf("expires_at = %v, want an instant in the past", ts)
	}

	// And it still appears in the list — "It continues to appear in list
	// responses during that window".
	status, body := s.do("GET", "/v1/files?ids="+genID, nil)
	if status != http.StatusOK {
		t.Fatalf("list after expiry = %d: %v", status, body)
	}
	entries := listData(t, body)
	if got := pageIDs(entries); len(got) != 1 || got[0] != genID {
		t.Errorf("list after expiry = %v, want [%s]: an expired file is not filtered out", got, genID)
	}
	// ...carrying the instant, not just the id: the docs tell clients to
	// "compare expires_at to the current time to filter expired files", which
	// they can only do if the list entry reports it.
	if len(entries) == 1 {
		if ts := wantTime(t, entries[0], "expires_at"); !ts.Before(time.Now()) {
			t.Errorf("list entry expires_at = %v, want the past instant", ts)
		}
	}

	// An expired UPLOAD answers 404 where an unexpired one answers the
	// not-downloadable 400. That difference is the whole of the ordering claim:
	// move the expiry check below the downloadable gate and this arm reads 400.
	live := s.uploadFile(t, "live.bin", &oct, "bytes")
	liveID := live["id"].(string)
	res = s.doRaw("GET", "/v1/files/"+liveID+"/content", nil, map[string]string{"x-api-key": testKey})
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("download of a live upload = %d, want the not-downloadable 400", res.StatusCode)
	}
	expire(t, s, liveID)
	res = s.doRaw("GET", "/v1/files/"+liveID+"/content", nil, map[string]string{"x-api-key": testKey})
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("download of an expired upload = %d, want 404 (expiry is answered before the downloadable gate)", res.StatusCode)
	}

	// "Deleting an expired file removes its metadata immediately instead of
	// waiting for the 30-day window to elapse."
	status, obj = s.do("DELETE", "/v1/files/"+genID, nil)
	if status != http.StatusOK {
		t.Fatalf("delete of an expired file = %d, want 200: %v", status, obj)
	}
	if status, obj = s.do("GET", "/v1/files/"+genID, nil); status != http.StatusNotFound {
		t.Errorf("metadata after deleting an expired file = %d, want 404: %v", status, obj)
	}
}

// TestExpiredFileEnvironmentKeyLane: the worker's content lane skips the
// downloadable gate, so the expiry check is the only thing standing between a
// BYOC worker and bytes the wire contract calls gone.
func TestExpiredFileEnvironmentKeyLane(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	bearer := map[string]string{"Authorization": "Bearer " + issueKey(t, s.pool, envID, "expiry-lane")}
	oct := "application/octet-stream"

	mounted := s.uploadFile(t, "mounted.bin", &oct, "mounted secret")
	mountedID := mounted["id"].(string)
	createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{map[string]any{"type": "file", "file_id": mountedID}},
	})
	res := s.doRaw("GET", "/v1/files/"+mountedID+"/content", nil, bearer)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("env-key download before expiry = %d, want 200", res.StatusCode)
	}

	expire(t, s, mountedID)
	res = s.doRaw("GET", "/v1/files/"+mountedID+"/content", nil, bearer)
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("env-key download after expiry = %d, want 404", res.StatusCode)
	}
}

// TestExpiredFileNotMountable: a session cannot take an expired file as a
// resource. Mounting one would put a file in the sandbox that the content route
// refuses to serve — the local analogue of the docs' "A Messages request that
// references the file fails before inference".
func TestExpiredFileNotMountable(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	oct := "application/octet-stream"

	f := s.uploadFile(t, "doomed.bin", &oct, "bytes")
	id := f["id"].(string)
	expire(t, s, id)

	status, obj := s.do("POST", "/v1/sessions", map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{map[string]any{"type": "file", "file_id": id}},
	})
	// not_found_error is the wire type; file_not_found_error is the internal
	// classification fileMustExist attaches for the outcome, and never reaches
	// the client.
	wantErr(t, status, obj, http.StatusNotFound, "not_found_error")
}
