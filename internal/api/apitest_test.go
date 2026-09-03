package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/blobtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/local"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testKey = "map-test-key-0123456789"

func TestMain(m *testing.M) {
	os.Exit(pgtest.Main(m))
}

// tserver is a running control-plane handler over a fresh database and an
// in-memory object store (the S3 backend has its own contract suite).
type tserver struct {
	t     *testing.T
	url   string
	pool  *pgxpool.Pool
	blobs *blobtest.MemStore
}

// newPoolWithKey is the database half of newTestServer, for tests that
// assemble their own handler.
func newPoolWithKey(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := pgtest.NewPool(t)
	if err := api.EnsureAPIKey(context.Background(), pool, "test", testKey); err != nil {
		t.Fatalf("EnsureAPIKey: %v", err)
	}
	return pool
}

// issueKey mints a live worker credential for an environment and returns the
// plaintext. Tests can no longer choose the value: the platform generates it and
// keeps only the hash, so the returned string is the single copy in existence.
func issueKey(t *testing.T, pool *pgxpool.Pool, envID, name string) string {
	t.Helper()
	key, err := api.IssueEnvironmentKey(context.Background(), pool, envID, name)
	if err != nil {
		t.Fatalf("IssueEnvironmentKey(%s): %v", name, err)
	}
	return key
}

func newTestServer(t *testing.T) *tserver {
	t.Helper()
	// A real (local AES-GCM) cipher under a fixed test key, so the vault
	// credential routes exercise the sealed-secret path end to end.
	cipher, err := local.New(local.Config{KeyID: "test-1", Key: bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	return newTestServerWithCipher(t, cipher)
}

// newTestServerWithCipher is newTestServer with the cipher chosen by the
// caller — for the one test that must show the SAME request answered
// differently by two ciphers behind the one interface.
func newTestServerWithCipher(t *testing.T, cipher secrets.Cipher) *tserver {
	t.Helper()
	pool := newPoolWithKey(t)
	blobs := blobtest.Mem()
	srv := httptest.NewServer(api.NewHandler(pool, blobs, cipher, nil))
	t.Cleanup(srv.Close)
	return &tserver{t: t, url: srv.URL, pool: pool, blobs: blobs}
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

// stampCreatedAt assigns the named rows the timestamps their listing orders on:
// a second apart, in the order named, oldest first, from one now() and in one
// statement.
//
// A list these tests assert positions in orders on `created_at`, with the row
// id as the tiebreak behind it; `created_at` defaults to now() and the id is
// 120 random bits — so a test naming rows by position holds only while the wall
// clock stays monotonic across the writes that made them. Measured here, those
// writes leave 1.5 to 6 ms between rows, and #411 recorded this hardware's
// database clock stepping 20 ms backwards: larger than every one of those gaps
// (#561). Where the order is the wire contract a test exists to pin it cannot
// be sorted away, so it is assigned instead — the remedy #551 settled on.
//
// The session event list is the one caller ordering on something else, `seq`.
// It takes a stamp for its created_at range filters alone, and says so there.
//
// One statement and one now(), never a loop: re-reading the clock per row lets
// a slow round trip hand an older row a later timestamp, reintroducing exactly
// what this removes. spreadCreatedAt says the same for environment keys, though
// it takes its rows newest first — the reverse of this one.
//
// The newest row lands a second in the past rather than at now(), so a row
// written after the stamp outranks all of them by a second instead of by the
// milliseconds a request leaves. Two tests depend on that: they insert a row
// mid-pagination and require it at the head.
//
// Only created_at moves: a stamped row's updated_at stays where it was, so a
// test comparing the two must not read a stamped row.
//
// The table is interpolated because a table name cannot be a bind parameter;
// every caller passes a literal.
func stampCreatedAt(t *testing.T, s *tserver, table string, oldestFirst ...string) {
	t.Helper()
	ages := make([]float64, len(oldestFirst))
	for i := range oldestFirst {
		ages[i] = float64(len(oldestFirst) - i)
	}
	tag, err := s.pool.Exec(t.Context(),
		fmt.Sprintf(`UPDATE %s r SET created_at = now() - make_interval(secs => n.age)
		               FROM unnest($1::text[], $2::float8[]) AS n(id, age)
		              WHERE r.id = n.id`, table),
		oldestFirst, ages)
	if err != nil {
		t.Fatalf("stamp %s %v: %v", table, oldestFirst, err)
	}
	if tag.RowsAffected() != int64(len(oldestFirst)) {
		t.Fatalf("stamped %d %s rows, want %d (ids %v)",
			tag.RowsAffected(), table, len(oldestFirst), oldestFirst)
	}
}
