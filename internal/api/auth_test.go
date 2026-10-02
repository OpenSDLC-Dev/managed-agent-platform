package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
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
		// No key at all is refused in the reference's words (2026-09-19
		// self-hosted-docker worker-network idx 0, a credential-less GET; #540).
		if name == "missing key" {
			wantErrMsg(t, res.StatusCode, body, http.StatusUnauthorized, "authentication_error", "x-api-key header is required")
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

	if _, err := s.pool.Exec(ctx, "UPDATE api_keys SET status = 'archived' WHERE name = 'second'"); err != nil {
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

// TestUnknownRouteAndMethodReturnErrorEnvelope pins the fallbacks' envelopes
// and, since #540, the reference's own words: "Not found" for an unknown path
// (2026-09-05-dreams batch1 idx 9 `rec91.control.nonsense-path`, idx 20
// `/v1/dreamz`; 2026-09-03 batch2 idx 270 `/v1/outcomes`), "Not Found" for one
// under the deployment and dream routes (2026-09-12-console-141 api-fixtures
// idx 22 `deployment.final-runs`; 2026-09-05-dreams batch1 idx 21
// `rec91.control.dreams.extra-segment`), and "Method Not Allowed" for a known
// path's unsupported method (2026-09-05-dreams batch1 idx 23
// `rec91.control.dreams.delete`).
func TestUnknownRouteAndMethodReturnErrorEnvelope(t *testing.T) {
	s := newTestServer(t)

	for path, want := range map[string]string{
		"/v1/nope":                          "Not found",
		"/v1/nonsense_route_does_not_exist": "Not found",
		"/v1/dreamz":                        "Not found",
		"/v1/outcomes":                      "Not found",
		"/v1/deployments/depl_x/runs":       "Not Found",
		"/v1/dreams/drm_x/bogus":            "Not Found",
	} {
		status, body := s.do(http.MethodGet, path, nil)
		wantErrMsg(t, status, body, http.StatusNotFound, "not_found_error", want)
	}

	status, body := s.do(http.MethodPut, "/v1/agents", nil)
	wantErrMsg(t, status, body, http.StatusMethodNotAllowed, "invalid_request_error", "Method Not Allowed")
	status, body = s.do(http.MethodDelete, "/v1/dreams/drm_x", nil)
	wantErrMsg(t, status, body, http.StatusMethodNotAllowed, "invalid_request_error", "Method Not Allowed")
}

// TestEveryRefusalIsLogged pins the one Info line writeError writes for a
// 4xx: method, path without its query, status, error type and request id —
// what an operator needs once a refusal's wire message names nothing (#540).
// A success writes none.
func TestEveryRefusalIsLogged(t *testing.T) {
	s := newTestServer(t)
	logs := captureLogs(t, slog.LevelInfo)

	status, body := s.do(http.MethodGet, "/v1/agents/agent_0000000000000000000000000?cursor=secret-looking", nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
	status, put := s.do(http.MethodPut, "/v1/agents", nil)
	wantErr(t, status, put, http.StatusMethodNotAllowed, "invalid_request_error")
	if status, _ := s.do(http.MethodGet, "/v1/agents", nil); status != http.StatusOK {
		t.Fatalf("list agents: %d", status)
	}

	var refused []string
	for _, l := range strings.Split(logs(), "\n") {
		if strings.Contains(l, `msg="request refused"`) {
			refused = append(refused, l)
		}
	}
	if len(refused) != 2 {
		t.Fatalf("refusal lines = %d, want 2 (one per 4xx, none for the 200):\n%s", len(refused), strings.Join(refused, "\n"))
	}
	for i, want := range [][]string{
		{"method=GET", "path=/v1/agents/agent_0000000000000000000000000", "status=404", "error_type=not_found_error",
			"request_id=" + body["request_id"].(string)},
		{"method=PUT", "path=/v1/agents ", "status=405", "error_type=invalid_request_error",
			"request_id=" + put["request_id"].(string)},
	} {
		for _, w := range want {
			if !strings.Contains(refused[i]+" ", w) {
				t.Errorf("refusal line %q lacks %q", refused[i], w)
			}
		}
	}
	if strings.Contains(logs(), "secret-looking") {
		t.Error("a refusal line carries the query string")
	}
}

func TestEnsureAPIKeyIsIdempotentAndStoresOnlyHashes(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)

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
