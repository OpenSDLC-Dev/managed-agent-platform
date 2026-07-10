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
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testKey = "map-test-key-0123456789"

// tserver is a running control-plane handler over a fresh database.
type tserver struct {
	t    *testing.T
	url  string
	pool *pgxpool.Pool
}

func newTestServer(t *testing.T) *tserver {
	t.Helper()
	ctx := context.Background()
	pool, err := store.Open(ctx, freshDB(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := api.EnsureAPIKey(ctx, pool, "test", testKey); err != nil {
		t.Fatalf("EnsureAPIKey: %v", err)
	}
	srv := httptest.NewServer(api.NewHandler(pool))
	t.Cleanup(srv.Close)
	return &tserver{t: t, url: srv.URL, pool: pool}
}

// do issues a request with the test API key. body may be nil, a raw string
// (sent verbatim), or any JSON-marshalable value. It returns the status code
// and the decoded JSON response object (nil if the body is not a JSON object).
func (s *tserver) do(method, path string, body any) (int, map[string]any) {
	s.t.Helper()
	res := s.doRaw(method, path, body, map[string]string{"x-api-key": testKey})
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		s.t.Fatalf("read response body: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		obj = nil
	}
	return res.StatusCode, obj
}

// doRaw issues a request with explicit headers and returns the raw response.
func (s *tserver) doRaw(method, path string, body any, headers map[string]string) *http.Response {
	s.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = bytes.NewBufferString(b)
	default:
		buf, err := json.Marshal(b)
		if err != nil {
			s.t.Fatalf("marshal request body: %v", err)
		}
		rd = bytes.NewBuffer(buf)
	}
	req, err := http.NewRequest(method, s.url+path, rd)
	if err != nil {
		s.t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	return res
}

// wantErr asserts the Anthropic error envelope:
// {"type":"error","request_id":…,"error":{"type":…,"message":…}}.
func wantErr(t *testing.T, status int, body map[string]any, wantStatus int, wantType string) {
	t.Helper()
	if status != wantStatus {
		t.Fatalf("status = %d, want %d (body %v)", status, wantStatus, body)
	}
	if body["type"] != "error" {
		t.Errorf(`envelope type = %v, want "error"`, body["type"])
	}
	if id, _ := body["request_id"].(string); id == "" {
		t.Errorf("request_id missing from error envelope: %v", body)
	}
	inner, _ := body["error"].(map[string]any)
	if inner == nil {
		t.Fatalf("error object missing: %v", body)
	}
	if inner["type"] != wantType {
		t.Errorf("error.type = %v, want %q (message %v)", inner["type"], wantType, inner["message"])
	}
	if msg, _ := inner["message"].(string); msg == "" {
		t.Errorf("error.message missing: %v", body)
	}
}

// wantFields asserts that every named key is present in the object — the wire
// schema marks these api:"required", so they must appear even when empty/null.
func wantFields(t *testing.T, obj map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := obj[k]; !ok {
			t.Errorf("required wire field %q missing from %v", k, obj)
		}
	}
}

// listData pulls the "data" array out of a list response.
func listData(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["data"].([]any)
	if !ok {
		t.Fatalf(`list response missing "data" array: %v`, body)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("list entry is not an object: %v", e)
		}
		out = append(out, m)
	}
	return out
}

// nextPage returns the next_page cursor, asserting the field is present
// (nullable but required on the wire).
func nextPage(t *testing.T, body map[string]any) string {
	t.Helper()
	v, ok := body["next_page"]
	if !ok {
		t.Fatalf(`list response missing "next_page": %v`, body)
	}
	s, _ := v.(string)
	return s
}
