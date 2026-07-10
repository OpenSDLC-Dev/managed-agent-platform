package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
)

func TestAuthRejectsMissingAndWrongKeys(t *testing.T) {
	s := newTestServer(t)

	for name, headers := range map[string]map[string]string{
		"missing key": {},
		"wrong key":   {"x-api-key": "not-the-key"},
		"bearer only": {"Authorization": "Bearer " + testKey}, // management auth is x-api-key
	} {
		res := s.doRaw(http.MethodGet, "/v1/agents", nil, headers)
		raw, _ := io.ReadAll(res.Body)
		res.Body.Close()
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		wantErr(t, res.StatusCode, body, http.StatusUnauthorized, "authentication_error")
		if name == "missing key" && res.Header.Get("request-id") == "" {
			t.Error("error responses must carry a request-id header")
		}
	}
}

func TestAuthRejectsRevokedKey(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	const second = "second-key-to-be-revoked"
	if err := api.EnsureAPIKey(ctx, s.pool, "second", second); err != nil {
		t.Fatalf("EnsureAPIKey: %v", err)
	}
	res := s.doRaw(http.MethodGet, "/v1/agents", nil, map[string]string{"x-api-key": second})
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("second key before revocation: %d, want 200", res.StatusCode)
	}

	if _, err := s.pool.Exec(ctx, "UPDATE api_keys SET revoked_at = now() WHERE name = 'second'"); err != nil {
		t.Fatalf("revoke key: %v", err)
	}
	res = s.doRaw(http.MethodGet, "/v1/agents", nil, map[string]string{"x-api-key": second})
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	wantErr(t, res.StatusCode, body, http.StatusUnauthorized, "authentication_error")

	// The original key on the same server still authenticates.
	status, _ := s.do(http.MethodGet, "/v1/agents", nil)
	if status != http.StatusOK {
		t.Fatalf("live key rejected: %d", status)
	}
}

func TestAuthAcceptsValidKeyAndIgnoresAnthropicHeaders(t *testing.T) {
	s := newTestServer(t)
	res := s.doRaw(http.MethodGet, "/v1/agents?beta=true", nil, map[string]string{
		"x-api-key":         testKey,
		"anthropic-version": "2023-06-01",
		"anthropic-beta":    "managed-agents-2026-04-01",
	})
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if res.Header.Get("request-id") == "" {
		t.Error("successful responses must carry a request-id header")
	}
}

func TestUnknownRouteAndMethodReturnErrorEnvelope(t *testing.T) {
	s := newTestServer(t)

	status, body := s.do(http.MethodGet, "/v1/nope", nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")

	status, body = s.do(http.MethodPut, "/v1/agents", nil)
	wantErr(t, status, body, http.StatusMethodNotAllowed, "invalid_request_error")
}

func TestEnsureAPIKeyIsIdempotentAndStoresOnlyHashes(t *testing.T) {
	ctx := context.Background()
	pool, err := store.Open(ctx, freshDB(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer pool.Close()

	if err := api.EnsureAPIKey(ctx, pool, "boot", "secret-key-value"); err != nil {
		t.Fatalf("first EnsureAPIKey: %v", err)
	}
	if err := api.EnsureAPIKey(ctx, pool, "boot", "secret-key-value"); err != nil {
		t.Fatalf("second EnsureAPIKey (idempotent): %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM api_keys").Scan(&n); err != nil {
		t.Fatalf("count api_keys: %v", err)
	}
	if n != 1 {
		t.Fatalf("api_keys rows = %d, want 1", n)
	}
	var hash string
	if err := pool.QueryRow(ctx, "SELECT key_hash FROM api_keys").Scan(&hash); err != nil {
		t.Fatalf("read key_hash: %v", err)
	}
	if hash == "secret-key-value" {
		t.Fatal("api key stored in plaintext")
	}
}
