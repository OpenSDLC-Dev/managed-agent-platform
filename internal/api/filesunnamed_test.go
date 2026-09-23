package api_test

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

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
		// The quoted-string escapes each "\", as the SDK's encoder does.
		{"windows path", `form-data; name="file"; filename="C:\\Users\\me\\report.pdf"`, ptr("application/octet-stream"), "report.pdf", "application/pdf"},
		{"trailing backslash", `form-data; name="file"; filename="dir\\"`, ptr("application/pdf"), "unnamed.pdf", "application/pdf"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ct, body := dispositionForm(t, tc.disposition, tc.contentType, "x")
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
	// The rest of the documented rule applies to what the cut leaves, and the
	// refusal names only characters that can still be refused.
	t.Run("forbidden character after the cut", func(t *testing.T) {
		ct, body := dispositionForm(t, `form-data; name="file"; filename="dir/a:b.txt"`, ptr("text/plain"), "x")
		status, obj := s.doForm("POST", "/v1/files", ct, body)
		wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")
		errObj, _ := obj["error"].(map[string]any)
		if msg, _ := errObj["message"].(string); strings.ContainsAny(msg, `/\`) {
			t.Errorf("message %q lists a separator the cut has already removed", msg)
		}
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

// windowsFile stands in for an *os.File opened by a Windows path: the SDK
// names a reader by path.Base of its Name(), which cuts only at "/", so the
// whole backslash path goes out as the filename (checked against
// anthropic-sdk-go v1.70.1 — internal/apiform/encoder.go
// encoder.newReaderTypeEncoder).
type windowsFile struct{ *bytes.Reader }

func (windowsFile) Name() string { return `C:\Users\me\report.pdf` }

// TestFileUploadWindowsPathThroughTheSDK is the client that sends a "\": the
// pinned SDK on Windows, given an open file.
func TestFileUploadWindowsPathThroughTheSDK(t *testing.T) {
	s := newTestServer(t)
	client := sdk.NewClient(option.WithoutEnvironmentDefaults(), option.WithBaseURL(s.url), option.WithAPIKey(testKey))
	got, err := client.Beta.Files.Upload(context.Background(), sdk.BetaFileUploadParams{
		File: windowsFile{bytes.NewReader([]byte("%PDF-1.7"))},
	})
	if err != nil {
		t.Fatalf("upload through the SDK: %v", err)
	}
	if got.Filename != "report.pdf" || got.MimeType != "application/pdf" {
		t.Fatalf("filename, mime_type = %q, %q; want report.pdf, application/pdf", got.Filename, got.MimeType)
	}
}
