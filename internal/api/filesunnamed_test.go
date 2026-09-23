package api_test

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// dispositionForm builds a one-part form with the Content-Disposition given
// verbatim, so a test can send filename="" as well as no filename at all.
func dispositionForm(t *testing.T, disposition string, contentType *string) (ct, body string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", disposition)
	if contentType != nil {
		h.Set("Content-Type", *contentType)
	}
	pw, err := w.CreatePart(h)
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	_, _ = pw.Write([]byte("x"))
	if err := w.Close(); err != nil {
		t.Fatalf("close form: %v", err)
	}
	return w.FormDataContentType(), buf.String()
}

// TestFileUploadFilenameRule pins the API reference's rule for the file part
// (checked against anthropic-sdk-go v1.70.1 — betafile.go
// BetaFileUploadParams.File): only the final path component of the filename
// is kept, and an absent or empty one becomes `unnamed` plus the extension for
// the stored mime_type, when known.
func TestFileUploadFilenameRule(t *testing.T) {
	s := newTestServer(t)
	ptr := func(v string) *string { return &v }
	cases := []struct {
		name, disposition string
		contentType       *string
		wantName, wantMT  string
	}{
		{"absent, pdf", `form-data; name="file"`, ptr("application/pdf"), "unnamed.pdf", "application/pdf"},
		{"empty, png", `form-data; name="file"; filename=""`, ptr("image/png"), "unnamed.png", "image/png"},
		{"absent, jpeg", `form-data; name="file"`, ptr("image/jpeg"), "unnamed.jpg", "image/jpeg"},
		{"absent, text with charset", `form-data; name="file"`, ptr("text/plain; charset=utf-8"), "unnamed.txt", "text/plain; charset=utf-8"},
		{"absent, no type", `form-data; name="file"`, nil, "unnamed.bin", "application/octet-stream"},
		{"absent, unlisted type", `form-data; name="file"`, ptr("application/x-custom"), "unnamed", "application/x-custom"},
		{"path-qualified", `form-data; name="file"; filename="dir/sub/report.pdf"`, ptr("application/octet-stream"), "report.pdf", "application/pdf"},
		{"trailing slash", `form-data; name="file"; filename="dir/"`, ptr("application/pdf"), "unnamed.pdf", "application/pdf"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ct, body := dispositionForm(t, tc.disposition, tc.contentType)
			status, obj := s.doForm("POST", "/v1/files", ct, body)
			if status != http.StatusOK {
				t.Fatalf("upload: status %d body %v", status, obj)
			}
			if obj["filename"] != tc.wantName || obj["mime_type"] != tc.wantMT {
				t.Fatalf("filename, mime_type = %v, %v; want %s, %s", obj["filename"], obj["mime_type"], tc.wantName, tc.wantMT)
			}
			id, _ := obj["id"].(string)
			if _, got := s.do("GET", "/v1/files/"+id, nil); got["filename"] != tc.wantName {
				t.Errorf("stored filename = %v, want %s", got["filename"], tc.wantName)
			}
		})
	}
	// "\" is not a separator: it stays one of the documented forbidden characters.
	t.Run("backslash", func(t *testing.T) {
		ct, body := dispositionForm(t, `form-data; name="file"; filename="dir\\report.pdf"`, ptr("application/pdf"))
		status, obj := s.doForm("POST", "/v1/files", ct, body)
		wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")
	})
}

// TestFileUploadWithoutANameThroughTheSDK is the path a real client takes to
// the rule: the pinned SDK's File helper given no name sends filename="".
func TestFileUploadWithoutANameThroughTheSDK(t *testing.T) {
	s := newTestServer(t)
	client := sdk.NewClient(option.WithoutEnvironmentDefaults(), option.WithBaseURL(s.url), option.WithAPIKey(testKey))
	got, err := client.Beta.Files.Upload(context.Background(), sdk.BetaFileUploadParams{
		File: sdk.File(bytes.NewReader([]byte("%PDF-1.7")), "", "application/pdf"),
	})
	if err != nil {
		t.Fatalf("upload through the SDK: %v", err)
	}
	if got.Filename != "unnamed.pdf" || got.MimeType != "application/pdf" {
		t.Fatalf("filename, mime_type = %q, %q; want unnamed.pdf, application/pdf", got.Filename, got.MimeType)
	}
}
