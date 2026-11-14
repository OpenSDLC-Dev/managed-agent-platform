package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Sanitize a separate diagnostic copy, never the response used by assertions.
// An unexpected success can carry a freshly issued key. Unparseable bodies are
// omitted because they may contain a truncated secret.
func managementDiagnosticBody(raw []byte) string {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "[response omitted: not a JSON object]"
	}
	if _, ok := obj["raw_key"]; ok {
		obj["raw_key"] = json.RawMessage(`"[REDACTED]"`)
	}
	safe, _ := json.Marshal(obj) // Each RawMessage came from valid JSON.
	return string(safe)
}

// Capture the actual Fatalf arguments without failing the enclosing test. Return
// so the test can also check the original response bytes used by assertions.
type managementDiagnosticRecorder struct {
	message string
	calls   int
}

func (*managementDiagnosticRecorder) Helper() {}
func (r *managementDiagnosticRecorder) Fatalf(format string, args ...any) {
	r.calls++
	r.message = fmt.Sprintf(format, args...)
}

// Bypassing sanitization at the logging call must fail, even when the response
// unexpectedly succeeds. The sentinel is not a credential and is never issued.
func TestManagementFailureDiagnostic(t *testing.T) {
	const sentinel = "NOT-A-CREDENTIAL-DIAGNOSTIC-SENTINEL"
	for _, tc := range []struct {
		name, body, detail string
	}{
		{"issued-key", `{"type":"api_key","raw_key":"` + sentinel + `","id":"apikey_fixture"}`, "apikey_fixture"},
		{"malformed", `{"raw_key":"` + sentinel, ""},
		{"error", `{"error":{"type":"invalid_request_error","message":"denied"}}`, "invalid_request_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.body)
			}))
			t.Cleanup(srv.Close)
			s := &nativeServer{tserver: &tserver{t: t, url: srv.URL}}
			var diagnostic managementDiagnosticRecorder
			raw := expectManagement(&diagnostic, s, "POST", consoleAPIKeysPath, "{}", nil, 403)
			if diagnostic.calls != 1 || !strings.Contains(diagnostic.message, "status=200 want=403") || !strings.Contains(diagnostic.message, "POST "+consoleAPIKeysPath) {
				t.Fatalf("missing status failure diagnostic: %q", diagnostic.message)
			}
			if strings.Contains(diagnostic.message, sentinel) {
				t.Errorf("failure diagnostic exposed the raw_key sentinel: %s", diagnostic.message)
			}
			if tc.detail != "" && !strings.Contains(diagnostic.message, tc.detail) {
				t.Error("failure diagnostic lost non-secret response detail")
			}
			if string(raw) != tc.body {
				t.Error("diagnostic sanitization changed the original assertion bytes")
			}
		})
	}
}
