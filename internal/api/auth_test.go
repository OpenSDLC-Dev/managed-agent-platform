package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gateconfig"
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
// 4xx answered to an authenticated request: method, path without its query
// and capped, status, error type, the wire message and request id — what an
// operator needs once a refusal's wire message names nothing (#540). A
// success writes none, and neither does a request no credential
// authenticated.
func TestEveryRefusalIsLogged(t *testing.T) {
	s := newTestServer(t)
	logs := captureLogs(t, slog.LevelInfo)

	status, body := s.do(http.MethodGet, "/v1/agents/agent_0000000000000000000000000?cursor=secret-looking", nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
	status, put := s.do(http.MethodPut, "/v1/agents", nil)
	wantErr(t, status, put, http.StatusMethodNotAllowed, "invalid_request_error")
	longPath := "/v1/agents/" + strings.Repeat("a", 600)
	status, long := s.do(http.MethodGet, longPath, nil)
	wantErr(t, status, long, http.StatusBadRequest, "invalid_request_error") // a malformed agent id (#841)
	if status, _ := s.do(http.MethodGet, "/v1/agents", nil); status != http.StatusOK {
		t.Fatalf("list agents: %d", status)
	}
	res := s.doRaw(http.MethodGet, "/v1/agents/unauthenticated-probe", nil, map[string]string{})
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key: %d, want 401", res.StatusCode)
	}
	// A 5xx answered to an authenticated request is no refusal: a server with
	// no secrets cipher answers a repository-bearing create 500.
	cipherless := newTestServerWithCipher(t, nil)
	agentID, envID := fixture(t, cipherless)
	status, fault := cipherless.do(http.MethodPost, "/v1/sessions", map[string]any{
		"agent": agentID, "environment_id": envID, "resources": []any{repoBody("ghp_x", nil)}})
	wantErr(t, status, fault, http.StatusInternalServerError, "api_error")

	var refused []string
	for _, l := range strings.Split(logs(), "\n") {
		if strings.Contains(l, `msg="request refused"`) {
			refused = append(refused, l)
		}
	}
	if len(refused) != 3 {
		t.Fatalf("refusal lines = %d, want 3 (one per authenticated 4xx, none for the 200, the 401 or the 500):\n%s",
			len(refused), strings.Join(refused, "\n"))
	}
	message := func(res map[string]any) string {
		return "message=" + strconv.Quote(res["error"].(map[string]any)["message"].(string))
	}
	for i, want := range [][]string{
		{"method=GET", "path=/v1/agents/agent_0000000000000000000000000 ", "status=404", "error_type=not_found_error",
			message(body), "request_id=" + body["request_id"].(string)},
		{"method=PUT", "path=/v1/agents ", "status=405", "error_type=invalid_request_error",
			`message="Method Not Allowed"`, "request_id=" + put["request_id"].(string)},
		{"path=" + longPath[:512-len("…[truncated]")] + "…[truncated] ", "status=400", "request_id=" + long["request_id"].(string)},
	} {
		for _, w := range want {
			if !strings.Contains(refused[i], w) {
				t.Errorf("refusal line %q lacks %q", refused[i], w)
			}
		}
	}
	if l := logs(); strings.Contains(l, "secret-looking") || strings.Contains(l, "unauthenticated-probe") {
		t.Error("a refusal line carries the query string, or an unauthenticated request was logged")
	}
	for _, l := range refused {
		if strings.Contains(l, "request_id="+fault["request_id"].(string)) {
			t.Errorf("a 5xx was logged as a refusal: %s", l)
		}
	}
}

// TestARefusalAfterAuthenticationIsLoggedOnEveryLane pins the refusal line on
// the requests a lane refuses after their credential verified but before the
// lane puts anything of it on the context — the environment key's session and
// file-download lanes and the sessions token's route checks — and its absence
// on every lane for a credential that never verified. The management key, a
// human's credential and the gate token refuse nothing between the two.
func TestARefusalAfterAuthenticationIsLoggedOnEveryLane(t *testing.T) {
	s := newTestServer(t)
	logs := captureLogs(t, slog.LevelInfo)

	selfEnv, _, selfKey := selfHostedWorker(t, s, "lanes-self")
	cloudEnv := createEnvironment(t, s, map[string]any{"name": "lanes-cloud"})["id"].(string)
	cloudKey := issueKey(t, s.pool, cloudEnv, "lanes-cloud")
	_, storeEnv, _, _, storeKey := storeWorker(t, s, "lanes-token")
	_, _, wtk := pollItem(t, s, storeEnv, storeKey)
	if wtk == "" {
		t.Fatal("the poll minted no sessions token")
	}
	ghostSession := "/v1/sessions/sesn_" + strings.Repeat("0", 25)

	requestID := func(t *testing.T, method, path string, headers map[string]string, wantStatus int) string {
		t.Helper()
		res := s.doRaw(method, path, nil, headers)
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != wantStatus {
			t.Fatalf("%s %s = %d, want %d", method, path, res.StatusCode, wantStatus)
		}
		return res.Header.Get("request-id")
	}
	logged := map[string]string{
		"self_hosted key, a session not its own": requestID(t, http.MethodGet, ghostSession, asBearer(selfKey), http.StatusNotFound),
		"cloud key, a session route":             requestID(t, http.MethodGet, ghostSession+"/events", asBearer(cloudKey), http.StatusNotFound),
		"cloud key, a file download": requestID(t, http.MethodGet, "/v1/files/file_"+strings.Repeat("0", 25)+"/content",
			asBearer(cloudKey), http.StatusNotFound),
		"sessions token, outside its routes": requestID(t, http.MethodGet, "/v1/environments/"+storeEnv+"/work",
			asBearer(wtk), http.StatusUnauthorized),
		"sessions token, another session": requestID(t, http.MethodGet, ghostSession+"/events", asBearer(wtk), http.StatusNotFound),
	}
	silent := map[string]string{
		"no credential":           requestID(t, http.MethodGet, "/v1/agents", map[string]string{}, http.StatusUnauthorized),
		"unknown management key":  requestID(t, http.MethodGet, "/v1/agents", map[string]string{"x-api-key": "not-a-key"}, http.StatusUnauthorized),
		"unknown environment key": requestID(t, http.MethodGet, "/v1/environments/"+selfEnv+"/work/poll", asBearer("sk-map-env01-bogus"), http.StatusUnauthorized),
		"unknown sessions token":  requestID(t, http.MethodGet, "/v1/environments/"+storeEnv+"/work", asBearer("wtk_bogus"), http.StatusUnauthorized),
		"unknown gate token":      requestID(t, http.MethodGet, gateconfig.Path, asBearer("gtk_bogus"), http.StatusUnauthorized),
	}

	lines := strings.Split(logs(), "\n")
	lineFor := func(rid string) string {
		for _, l := range lines {
			if strings.Contains(l, `msg="request refused"`) && strings.Contains(l, "request_id="+rid) {
				return l
			}
		}
		return ""
	}
	for name, rid := range logged {
		if lineFor(rid) == "" {
			t.Errorf("%s: no refusal line for request %s", name, rid)
		}
	}
	for name, rid := range silent {
		if l := lineFor(rid); l != "" {
			t.Errorf("%s: an unauthenticated request was logged: %s", name, l)
		}
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
