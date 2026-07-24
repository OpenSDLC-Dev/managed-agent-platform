package api_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
)

// The vault wire surface (plan 12 slice 2): shapes per the pinned SDK's
// BetaManagedAgentsVault / BetaManagedAgentsCredential, limits per the public
// docs (D7).

func createVault(t *testing.T, s *tserver, name string) string {
	t.Helper()
	status, body := s.do("POST", "/v1/vaults", map[string]any{"display_name": name})
	if status != http.StatusOK {
		t.Fatalf("create vault: status %d (%v)", status, body)
	}
	return body["id"].(string)
}

func TestVaultCRUD(t *testing.T) {
	s := newTestServer(t)

	status, body := s.do("POST", "/v1/vaults", map[string]any{
		"display_name": "Prod secrets", "metadata": map[string]string{"team": "infra"},
	})
	if status != http.StatusOK {
		t.Fatalf("create: status %d (%v)", status, body)
	}
	id, _ := body["id"].(string)
	if !strings.HasPrefix(id, "vlt_") {
		t.Fatalf("id %q lacks the vlt_ prefix", id)
	}
	if body["type"] != "vault" || body["display_name"] != "Prod secrets" {
		t.Fatalf("unexpected create body: %v", body)
	}
	if body["archived_at"] != nil {
		t.Fatalf("archived_at should render null, got %v", body["archived_at"])
	}
	if md := body["metadata"].(map[string]any); md["team"] != "infra" {
		t.Fatalf("metadata not round-tripped: %v", md)
	}

	// Get returns the same shape; unknown and malformed ids 404/400.
	if status, got := s.do("GET", "/v1/vaults/"+id, nil); status != http.StatusOK || got["id"] != id {
		t.Fatalf("get: status %d (%v)", status, got)
	}
	if status, _ := s.do("GET", "/v1/vaults/vlt_missing0000000000000000", nil); status != http.StatusNotFound {
		t.Fatalf("get missing: status %d", status)
	}
	// A malformed or wrong-prefix id is indistinguishable from a missing one
	// (the checkID rule, #135).
	if status, _ := s.do("GET", "/v1/vaults/env_wrongprefix", nil); status != http.StatusNotFound {
		t.Fatalf("get with a wrong prefix: status %d", status)
	}

	// Update: display_name replace + metadata patch (null deletes, empty
	// string is a VALUE here — the empty-deletes rule is environments-only).
	status, body = s.do("POST", "/v1/vaults/"+id, map[string]any{
		"display_name": "Renamed",
		"metadata":     map[string]any{"team": nil, "tier": "", "env": "prod"},
	})
	if status != http.StatusOK {
		t.Fatalf("update: status %d (%v)", status, body)
	}
	md := body["metadata"].(map[string]any)
	if _, ok := md["team"]; ok {
		t.Fatalf("null should delete the key: %v", md)
	}
	if md["tier"] != "" || md["env"] != "prod" {
		t.Fatalf("patch semantics wrong: %v", md)
	}
	if body["display_name"] != "Renamed" {
		t.Fatalf("display_name not updated: %v", body)
	}

	// Archive is idempotent and returns the full vault; an archived vault
	// rejects updates.
	status, body = s.do("POST", "/v1/vaults/"+id+"/archive", nil)
	if status != http.StatusOK || body["archived_at"] == nil {
		t.Fatalf("archive: status %d (%v)", status, body)
	}
	first := body["archived_at"]
	if _, body = s.do("POST", "/v1/vaults/"+id+"/archive", nil); body["archived_at"] != first {
		t.Fatalf("archive not idempotent: %v vs %v", body["archived_at"], first)
	}
	if status, _ = s.do("POST", "/v1/vaults/"+id, map[string]any{"display_name": "X"}); status != http.StatusBadRequest {
		t.Fatalf("update archived: status %d", status)
	}

	// Delete is a tombstone; a second delete 404s.
	status, body = s.do("DELETE", "/v1/vaults/"+id, nil)
	if status != http.StatusOK || body["type"] != "vault_deleted" || body["id"] != id {
		t.Fatalf("delete: status %d (%v)", status, body)
	}
	if status, _ = s.do("DELETE", "/v1/vaults/"+id, nil); status != http.StatusNotFound {
		t.Fatalf("second delete: status %d", status)
	}
}

func TestVaultValidation(t *testing.T) {
	s := newTestServer(t)

	for name, body := range map[string]map[string]any{
		"missing display_name": {},
		"empty display_name":   {"display_name": ""},
		"long display_name":    {"display_name": strings.Repeat("n", 256)},
		"unknown key":          {"display_name": "v", "surprise": true},
		"long metadata key":    {"display_name": "v", "metadata": map[string]string{strings.Repeat("k", 65): "v"}},
		"long metadata value":  {"display_name": "v", "metadata": map[string]string{"k": strings.Repeat("v", 513)}},
	} {
		if status, resp := s.do("POST", "/v1/vaults", body); status != http.StatusBadRequest {
			t.Fatalf("%s: status %d (%v)", name, status, resp)
		}
	}
	tooMany := map[string]string{}
	for i := 0; i < 17; i++ {
		tooMany[fmt.Sprintf("k%d", i)] = "v"
	}
	if status, _ := s.do("POST", "/v1/vaults", map[string]any{"display_name": "v", "metadata": tooMany}); status != http.StatusBadRequest {
		t.Fatal("17 metadata pairs must be rejected")
	}
	// The caps also hold across an update patch.
	id := createVault(t, s, "capped")
	sixteen := map[string]string{}
	for i := 0; i < 16; i++ {
		sixteen[fmt.Sprintf("k%d", i)] = "v"
	}
	if status, _ := s.do("POST", "/v1/vaults/"+id, map[string]any{"metadata": sixteen}); status != http.StatusOK {
		t.Fatal("16 pairs must be accepted")
	}
	if status, _ := s.do("POST", "/v1/vaults/"+id, map[string]any{"metadata": map[string]string{"one-more": "v"}}); status != http.StatusBadRequest {
		t.Fatal("a patch growing metadata past 16 pairs must be rejected")
	}
}

func TestVaultListPagination(t *testing.T) {
	s := newTestServer(t)
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, createVault(t, s, fmt.Sprintf("v%d", i)))
	}
	s.do("POST", "/v1/vaults/"+ids[0]+"/archive", nil)

	// Default list hides archived vaults.
	status, body := s.do("GET", "/v1/vaults", nil)
	if status != http.StatusOK {
		t.Fatalf("list: status %d", status)
	}
	if n := len(body["data"].([]any)); n != 2 {
		t.Fatalf("expected 2 active vaults, got %d", n)
	}
	if _, ok := body["next_page"]; !ok {
		t.Fatal("next_page must be present (null) in the page envelope")
	}
	status, body = s.do("GET", "/v1/vaults?include_archived=true", nil)
	if n := len(body["data"].([]any)); status != http.StatusOK || n != 3 {
		t.Fatalf("include_archived: status %d, %d rows", status, n)
	}

	// Keyset pagination walks newest-first without overlap.
	status, body = s.do("GET", "/v1/vaults?include_archived=true&limit=2", nil)
	if n := len(body["data"].([]any)); status != http.StatusOK || n != 2 || body["next_page"] == nil {
		t.Fatalf("page 1: status %d, %d rows, next %v", status, n, body["next_page"])
	}
	next := body["next_page"].(string)
	status, body = s.do("GET", "/v1/vaults?include_archived=true&limit=2&page="+next, nil)
	if n := len(body["data"].([]any)); status != http.StatusOK || n != 1 || body["next_page"] != nil {
		t.Fatalf("page 2: status %d, %d rows, next %v", status, n, body["next_page"])
	}
}

// --- credentials ---

func envVarAuth(name string) map[string]any {
	return map[string]any{
		"type": "environment_variable", "secret_name": name, "secret_value": "s3cr3t-" + name,
		"networking": map[string]any{"type": "limited", "allowed_hosts": []string{"api.example.com"}},
	}
}

func createCredential(t *testing.T, s *tserver, vaultID string, auth map[string]any) map[string]any {
	t.Helper()
	status, body := s.do("POST", "/v1/vaults/"+vaultID+"/credentials", map[string]any{"auth": auth})
	if status != http.StatusOK {
		t.Fatalf("create credential: status %d (%v)", status, body)
	}
	return body
}

func TestCredentialEnvVarLifecycle(t *testing.T) {
	s := newTestServer(t)
	vaultID := createVault(t, s, "creds")

	body := createCredential(t, s, vaultID, envVarAuth("API_KEY"))
	id := body["id"].(string)
	if !strings.HasPrefix(id, "vcrd_") {
		t.Fatalf("id %q lacks the vcrd_ prefix", id)
	}
	if body["type"] != "vault_credential" || body["vault_id"] != vaultID {
		t.Fatalf("unexpected body: %v", body)
	}
	if body["display_name"] != nil {
		t.Fatalf("display_name must render null when unset, got %v", body["display_name"])
	}
	auth := body["auth"].(map[string]any)
	if auth["type"] != "environment_variable" || auth["secret_name"] != "API_KEY" {
		t.Fatalf("auth not round-tripped: %v", auth)
	}
	if _, leaked := auth["secret_value"]; leaked {
		t.Fatal("secret_value leaked into the response")
	}
	// Omitting injection_location enables both locations.
	loc := auth["injection_location"].(map[string]any)
	if loc["body"] != true || loc["header"] != true {
		t.Fatalf("default injection_location must enable both: %v", loc)
	}
	nw := auth["networking"].(map[string]any)
	if nw["type"] != "limited" || nw["allowed_hosts"].([]any)[0] != "api.example.com" {
		t.Fatalf("networking not round-tripped: %v", nw)
	}

	// The secret never lands in the database in the clear.
	var authDoc, ciphertext []byte
	if err := s.pool.QueryRow(t.Context(),
		`SELECT auth, secret_ciphertext FROM vault_credentials WHERE id = $1`, id).
		Scan(&authDoc, &ciphertext); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if strings.Contains(string(authDoc), "s3cr3t-API_KEY") {
		t.Fatal("plaintext secret in the auth document")
	}
	if strings.Contains(string(ciphertext), "s3cr3t-API_KEY") {
		t.Fatal("plaintext secret in the ciphertext column")
	}
	if len(ciphertext) == 0 {
		t.Fatal("no ciphertext sealed")
	}

	// Update: secret_value rotates (ciphertext changes), networking is full
	// replacement, injection_location merges field-by-field.
	status, body := s.do("POST", "/v1/vaults/"+vaultID+"/credentials/"+id, map[string]any{
		"display_name": "api key",
		"auth": map[string]any{
			"type": "environment_variable", "secret_value": "rotated",
			"networking":         map[string]any{"type": "unrestricted"},
			"injection_location": map[string]any{"body": false},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("update: status %d (%v)", status, body)
	}
	if body["display_name"] != "api key" {
		t.Fatalf("display_name not set: %v", body)
	}
	auth = body["auth"].(map[string]any)
	if auth["networking"].(map[string]any)["type"] != "unrestricted" {
		t.Fatalf("networking not replaced: %v", auth)
	}
	loc = auth["injection_location"].(map[string]any)
	if loc["body"] != false || loc["header"] != true {
		t.Fatalf("injection_location must merge per field: %v", loc)
	}
	var rotated []byte
	if err := s.pool.QueryRow(t.Context(),
		`SELECT secret_ciphertext FROM vault_credentials WHERE id = $1`, id).Scan(&rotated); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if string(rotated) == string(ciphertext) {
		t.Fatal("rotating the secret must change the ciphertext")
	}

	// Archive purges the sealed secret, keeps the record, and is idempotent.
	status, body = s.do("POST", "/v1/vaults/"+vaultID+"/credentials/"+id+"/archive", nil)
	if status != http.StatusOK || body["archived_at"] == nil {
		t.Fatalf("archive: status %d (%v)", status, body)
	}
	var purged []byte
	if err := s.pool.QueryRow(t.Context(),
		`SELECT secret_ciphertext FROM vault_credentials WHERE id = $1`, id).Scan(&purged); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if purged != nil {
		t.Fatal("archive must purge the ciphertext")
	}
	if status, _ = s.do("POST", "/v1/vaults/"+vaultID+"/credentials/"+id, map[string]any{"display_name": "x"}); status != http.StatusBadRequest {
		t.Fatalf("update archived: status %d", status)
	}

	// Delete is a tombstone.
	status, body = s.do("DELETE", "/v1/vaults/"+vaultID+"/credentials/"+id, nil)
	if status != http.StatusOK || body["type"] != "vault_credential_deleted" {
		t.Fatalf("delete: status %d (%v)", status, body)
	}
}

func TestCredentialMCPOAuthShape(t *testing.T) {
	s := newTestServer(t)
	vaultID := createVault(t, s, "oauth")

	body := createCredential(t, s, vaultID, map[string]any{
		"type": "mcp_oauth", "mcp_server_url": "https://mcp.example.com/sse",
		"access_token": "at-secret",
		"refresh": map[string]any{
			"client_id": "client-1", "refresh_token": "rt-secret",
			"token_endpoint":      "https://auth.example.com/token",
			"token_endpoint_auth": map[string]any{"type": "client_secret_basic", "client_secret": "cs-secret"},
			"scope":               "mcp.read",
		},
	})
	auth := body["auth"].(map[string]any)
	if auth["type"] != "mcp_oauth" || auth["mcp_server_url"] != "https://mcp.example.com/sse" {
		t.Fatalf("auth shape: %v", auth)
	}
	if auth["expires_at"] != nil {
		t.Fatalf("expires_at must render null when unset: %v", auth)
	}
	refresh := auth["refresh"].(map[string]any)
	if refresh["client_id"] != "client-1" || refresh["token_endpoint"] != "https://auth.example.com/token" {
		t.Fatalf("refresh shape: %v", refresh)
	}
	if refresh["token_endpoint_auth"].(map[string]any)["type"] != "client_secret_basic" {
		t.Fatalf("token_endpoint_auth shape: %v", refresh)
	}
	if refresh["resource"] != nil || refresh["scope"] != "mcp.read" {
		t.Fatalf("resource/scope: %v", refresh)
	}
	for _, secret := range []string{"at-secret", "rt-secret", "cs-secret"} {
		if strings.Contains(fmt.Sprint(body), secret) {
			t.Fatalf("write-only value %q leaked into the response", secret)
		}
	}

	// static_bearer renders only its discriminator + server URL.
	body = createCredential(t, s, vaultID, map[string]any{
		"type": "static_bearer", "mcp_server_url": "https://other.example.com/sse", "token": "tok-secret",
	})
	auth = body["auth"].(map[string]any)
	if len(auth) != 2 || auth["type"] != "static_bearer" || auth["mcp_server_url"] != "https://other.example.com/sse" {
		t.Fatalf("static_bearer must render exactly type + mcp_server_url: %v", auth)
	}
}

func TestCredentialValidationRules(t *testing.T) {
	s := newTestServer(t)
	vaultID := createVault(t, s, "rules")

	cases := map[string]map[string]any{
		"missing secret_value": {"type": "environment_variable", "secret_name": "K",
			"networking": map[string]any{"type": "unrestricted"}},
		"missing networking": {"type": "environment_variable", "secret_name": "K", "secret_value": "v"},
		"null networking": {"type": "environment_variable", "secret_name": "K", "secret_value": "v",
			"networking": nil},
		"both locations disabled": {"type": "environment_variable", "secret_name": "K", "secret_value": "v",
			"networking":         map[string]any{"type": "unrestricted"},
			"injection_location": map[string]any{}},
		"null injection_location": {"type": "environment_variable", "secret_name": "K", "secret_value": "v",
			"networking":         map[string]any{"type": "unrestricted"},
			"injection_location": nil},
		"null injection field": {"type": "environment_variable", "secret_name": "K", "secret_value": "v",
			"networking":         map[string]any{"type": "unrestricted"},
			"injection_location": map[string]any{"body": nil}},
		"host with scheme": {"type": "environment_variable", "secret_name": "K", "secret_value": "v",
			"networking": map[string]any{"type": "limited", "allowed_hosts": []string{"https://x.com"}}},
		"host with port": {"type": "environment_variable", "secret_name": "K", "secret_value": "v",
			"networking": map[string]any{"type": "limited", "allowed_hosts": []string{"x.com:443"}}},
		"inner wildcard": {"type": "environment_variable", "secret_name": "K", "secret_value": "v",
			"networking": map[string]any{"type": "limited", "allowed_hosts": []string{"a.*.com"}}},
		"bare wildcard": {"type": "environment_variable", "secret_name": "K", "secret_value": "v",
			"networking": map[string]any{"type": "limited", "allowed_hosts": []string{"*"}}},
		"wildcard on IP": {"type": "environment_variable", "secret_name": "K", "secret_value": "v",
			"networking": map[string]any{"type": "limited", "allowed_hosts": []string{"*.10.0.0.1"}}},
		"malformed IPv4": {"type": "environment_variable", "secret_name": "K", "secret_value": "v",
			"networking": map[string]any{"type": "limited", "allowed_hosts": []string{"999.999.999.999"}}},
		"IPv6 literal": {"type": "environment_variable", "secret_name": "K", "secret_value": "v",
			"networking": map[string]any{"type": "limited", "allowed_hosts": []string{"::1"}}},
		"missing access_token":   {"type": "mcp_oauth", "mcp_server_url": "https://m.example.com"},
		"mcp missing server_url": {"type": "mcp_oauth", "access_token": "at"},
		"mcp bad server_url":     {"type": "mcp_oauth", "mcp_server_url": "notaurl", "access_token": "at"},
		"mcp bad expires_at": {"type": "mcp_oauth", "mcp_server_url": "https://m.example.com",
			"access_token": "at", "expires_at": "notatime"},
		"mcp unknown key": {"type": "mcp_oauth", "mcp_server_url": "https://m.example.com",
			"access_token": "at", "bogus": 1},
		"missing token":      {"type": "static_bearer", "mcp_server_url": "https://m.example.com"},
		"bearer no url":      {"type": "static_bearer", "token": "t"},
		"bearer bad url":     {"type": "static_bearer", "mcp_server_url": "ftp://x", "token": "t"},
		"bearer unknown key": {"type": "static_bearer", "mcp_server_url": "https://m.example.com", "token": "t", "bogus": 1},
		"refresh not an object": {"type": "mcp_oauth", "mcp_server_url": "https://m.example.com",
			"access_token": "at", "refresh": "nope"},
		"refresh missing client_id": {"type": "mcp_oauth", "mcp_server_url": "https://m.example.com",
			"access_token": "at", "refresh": map[string]any{"refresh_token": "r",
				"token_endpoint": "https://a.example.com/t", "token_endpoint_auth": map[string]any{"type": "none"}}},
		"refresh missing token_endpoint": {"type": "mcp_oauth", "mcp_server_url": "https://m.example.com",
			"access_token": "at", "refresh": map[string]any{"client_id": "c", "refresh_token": "r",
				"token_endpoint_auth": map[string]any{"type": "none"}}},
		"refresh bad token_endpoint": {"type": "mcp_oauth", "mcp_server_url": "https://m.example.com",
			"access_token": "at", "refresh": map[string]any{"client_id": "c", "refresh_token": "r",
				"token_endpoint": "notaurl", "token_endpoint_auth": map[string]any{"type": "none"}}},
		"refresh missing refresh_token": {"type": "mcp_oauth", "mcp_server_url": "https://m.example.com",
			"access_token": "at", "refresh": map[string]any{"client_id": "c",
				"token_endpoint": "https://a.example.com/t", "token_endpoint_auth": map[string]any{"type": "none"}}},
		"refresh unknown key": {"type": "mcp_oauth", "mcp_server_url": "https://m.example.com",
			"access_token": "at", "refresh": map[string]any{"client_id": "c", "refresh_token": "r",
				"token_endpoint": "https://a.example.com/t", "bogus": 1,
				"token_endpoint_auth": map[string]any{"type": "none"}}},
		"refresh bad resource": {"type": "mcp_oauth", "mcp_server_url": "https://m.example.com",
			"access_token": "at", "refresh": map[string]any{"client_id": "c", "refresh_token": "r",
				"token_endpoint": "https://a.example.com/t", "resource": 5,
				"token_endpoint_auth": map[string]any{"type": "none"}}},
		"tea missing": {"type": "mcp_oauth", "mcp_server_url": "https://m.example.com",
			"access_token": "at", "refresh": map[string]any{"client_id": "c", "refresh_token": "r",
				"token_endpoint": "https://a.example.com/t"}},
		"tea not object": {"type": "mcp_oauth", "mcp_server_url": "https://m.example.com",
			"access_token": "at", "refresh": map[string]any{"client_id": "c", "refresh_token": "r",
				"token_endpoint": "https://a.example.com/t", "token_endpoint_auth": "x"}},
		"tea bad type": {"type": "mcp_oauth", "mcp_server_url": "https://m.example.com",
			"access_token": "at", "refresh": map[string]any{"client_id": "c", "refresh_token": "r",
				"token_endpoint": "https://a.example.com/t", "token_endpoint_auth": map[string]any{"type": "weird"}}},
		"tea empty client_secret": {"type": "mcp_oauth", "mcp_server_url": "https://m.example.com",
			"access_token": "at", "refresh": map[string]any{"client_id": "c", "refresh_token": "r",
				"token_endpoint":      "https://a.example.com/t",
				"token_endpoint_auth": map[string]any{"type": "client_secret_basic", "client_secret": ""}}},
		"tea unknown key": {"type": "mcp_oauth", "mcp_server_url": "https://m.example.com",
			"access_token": "at", "refresh": map[string]any{"client_id": "c", "refresh_token": "r",
				"token_endpoint":      "https://a.example.com/t",
				"token_endpoint_auth": map[string]any{"type": "none", "bogus": 1}}},
		"refresh without token_endpoint_auth secret": {"type": "mcp_oauth",
			"mcp_server_url": "https://m.example.com", "access_token": "at",
			"refresh": map[string]any{"client_id": "c", "refresh_token": "r",
				"token_endpoint":      "https://a.example.com/t",
				"token_endpoint_auth": map[string]any{"type": "client_secret_post"}}},
		"unknown auth type": {"type": "weird"},
		"missing auth type": {"mcp_server_url": "https://m.example.com"},
	}
	for name, auth := range cases {
		if status, resp := s.do("POST", "/v1/vaults/"+vaultID+"/credentials", map[string]any{"auth": auth}); status != http.StatusBadRequest {
			t.Fatalf("%s: status %d (%v)", name, status, resp)
		}
	}

	// 16 allowed hosts pass; 17 fail. Wildcards and IPv4 literals pass.
	hosts := []string{"10.0.0.1", "*.example.com"}
	for i := len(hosts); i < 16; i++ {
		hosts = append(hosts, fmt.Sprintf("h%d.example.com", i))
	}
	auth := map[string]any{"type": "environment_variable", "secret_name": "OK", "secret_value": "v",
		"networking": map[string]any{"type": "limited", "allowed_hosts": hosts}}
	if status, resp := s.do("POST", "/v1/vaults/"+vaultID+"/credentials", map[string]any{"auth": auth}); status != http.StatusOK {
		t.Fatalf("16 hosts must pass: %d (%v)", status, resp)
	}
	auth["secret_name"] = "OK2"
	auth["networking"].(map[string]any)["allowed_hosts"] = append(hosts, "h17.example.com")
	if status, _ := s.do("POST", "/v1/vaults/"+vaultID+"/credentials", map[string]any{"auth": auth}); status != http.StatusBadRequest {
		t.Fatal("17 hosts must be rejected")
	}
}

func TestCredentialUniquenessAndCap(t *testing.T) {
	s := newTestServer(t)
	vaultID := createVault(t, s, "unique")

	createCredential(t, s, vaultID, envVarAuth("DUP"))
	if status, _ := s.do("POST", "/v1/vaults/"+vaultID+"/credentials", map[string]any{"auth": envVarAuth("DUP")}); status != http.StatusConflict {
		t.Fatalf("duplicate active secret_name: status %d, want 409", status)
	}
	// The mcp_server_url namespace is shared by the two MCP variants.
	createCredential(t, s, vaultID, map[string]any{
		"type": "static_bearer", "mcp_server_url": "https://mcp.example.com", "token": "t"})
	if status, _ := s.do("POST", "/v1/vaults/"+vaultID+"/credentials", map[string]any{"auth": map[string]any{
		"type": "mcp_oauth", "mcp_server_url": "https://mcp.example.com", "access_token": "a"}}); status != http.StatusConflict {
		t.Fatalf("duplicate active mcp_server_url across variants: status %d, want 409", status)
	}
	// Archiving frees the key.
	var id string
	if err := s.pool.QueryRow(t.Context(),
		`SELECT id FROM vault_credentials WHERE cred_key = 'name:DUP'`).Scan(&id); err != nil {
		t.Fatalf("find credential: %v", err)
	}
	s.do("POST", "/v1/vaults/"+vaultID+"/credentials/"+id+"/archive", nil)
	createCredential(t, s, vaultID, envVarAuth("DUP"))

	// A second vault has its own namespace.
	otherVault := createVault(t, s, "other")
	createCredential(t, s, otherVault, envVarAuth("DUP"))

	// The 20-active-credential cap.
	capVault := createVault(t, s, "cap")
	for i := 0; i < 20; i++ {
		createCredential(t, s, capVault, envVarAuth(fmt.Sprintf("K%d", i)))
	}
	if status, _ := s.do("POST", "/v1/vaults/"+capVault+"/credentials", map[string]any{"auth": envVarAuth("K20")}); status != http.StatusBadRequest {
		t.Fatalf("21st active credential: status %d, want 400", status)
	}
}

func TestCredentialPathScoping(t *testing.T) {
	s := newTestServer(t)
	vaultA := createVault(t, s, "a")
	vaultB := createVault(t, s, "b")
	id := createCredential(t, s, vaultA, envVarAuth("K"))["id"].(string)

	// The wrong vault segment 404s on every nested verb.
	for _, probe := range []struct{ method, path string }{
		{"GET", "/v1/vaults/" + vaultB + "/credentials/" + id},
		{"POST", "/v1/vaults/" + vaultB + "/credentials/" + id},
		{"DELETE", "/v1/vaults/" + vaultB + "/credentials/" + id},
		{"POST", "/v1/vaults/" + vaultB + "/credentials/" + id + "/archive"},
	} {
		var payload any
		if probe.method == "POST" && !strings.HasSuffix(probe.path, "/archive") {
			payload = map[string]any{"display_name": "x"}
		}
		if status, _ := s.do(probe.method, probe.path, payload); status != http.StatusNotFound {
			t.Fatalf("%s %s: status %d, want 404", probe.method, probe.path, status)
		}
	}
	// A wrong-vault archive must 404 WITHOUT destroying the credential — the
	// mutation is scoped to the path's vault, not checked after the fact.
	if status, _ := s.do("POST", "/v1/vaults/"+vaultB+"/credentials/"+id+"/archive", nil); status != http.StatusNotFound {
		t.Fatal("wrong-vault archive must 404")
	}
	var archived bool
	var ciphertext []byte
	if err := s.pool.QueryRow(t.Context(),
		`SELECT archived_at IS NOT NULL, secret_ciphertext FROM vault_credentials WHERE id = $1`, id).
		Scan(&archived, &ciphertext); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if archived || ciphertext == nil {
		t.Fatal("a wrong-vault archive destroyed the credential (archived or purged its ciphertext)")
	}
	// Listing a missing vault 404s rather than answering an empty page.
	if status, _ := s.do("GET", "/v1/vaults/vlt_missing0000000000000000/credentials", nil); status != http.StatusNotFound {
		t.Fatal("list on a missing vault must 404")
	}
	// Cascade: deleting the vault takes its credentials with it.
	s.do("DELETE", "/v1/vaults/"+vaultA, nil)
	if status, _ := s.do("GET", "/v1/vaults/"+vaultA+"/credentials/"+id, nil); status != http.StatusNotFound {
		t.Fatal("credential must not outlive its vault")
	}
}

func TestCredentialUpdateImmutability(t *testing.T) {
	s := newTestServer(t)
	vaultID := createVault(t, s, "frozen")
	envID := createCredential(t, s, vaultID, envVarAuth("K"))["id"].(string)
	oauthID := createCredential(t, s, vaultID, map[string]any{
		"type": "mcp_oauth", "mcp_server_url": "https://m.example.com", "access_token": "at",
		"refresh": map[string]any{"client_id": "c", "refresh_token": "rt",
			"token_endpoint":      "https://a.example.com/t",
			"token_endpoint_auth": map[string]any{"type": "none"}},
	})["id"].(string)
	bearerID := createCredential(t, s, vaultID, map[string]any{
		"type": "static_bearer", "mcp_server_url": "https://b.example.com", "token": "t"})["id"].(string)

	rejected := []struct {
		id   string
		auth map[string]any
	}{
		{envID, map[string]any{"type": "mcp_oauth"}},                                    // variant switch
		{envID, map[string]any{"type": "environment_variable", "secret_name": "OTHER"}}, // immutable anchor
		{oauthID, map[string]any{"type": "mcp_oauth", "mcp_server_url": "https://x.example.com"}},
		{bearerID, map[string]any{"type": "static_bearer", "mcp_server_url": "https://x.example.com"}},
		{oauthID, map[string]any{"type": "mcp_oauth", "refresh": map[string]any{"client_id": "new"}}},
		{oauthID, map[string]any{"type": "mcp_oauth", "refresh": map[string]any{"token_endpoint": "https://x"}}},
		{oauthID, map[string]any{"type": "mcp_oauth", "refresh": map[string]any{
			"token_endpoint_auth": map[string]any{"type": "none"}}}}, // update union drops none
	}
	for _, c := range rejected {
		if status, resp := s.do("POST", "/v1/vaults/"+vaultID+"/credentials/"+c.id, map[string]any{"auth": c.auth}); status != http.StatusBadRequest {
			t.Fatalf("auth %v: status %d (%v), want 400", c.auth, status, resp)
		}
	}

	// A refresh block cannot be introduced after create (its anchors are
	// create-only fields).
	noRefreshID := createCredential(t, s, vaultID, map[string]any{
		"type": "mcp_oauth", "mcp_server_url": "https://plain.example.com", "access_token": "at"})["id"].(string)
	if status, _ := s.do("POST", "/v1/vaults/"+vaultID+"/credentials/"+noRefreshID, map[string]any{
		"auth": map[string]any{"type": "mcp_oauth", "refresh": map[string]any{"refresh_token": "rt"}}}); status != http.StatusBadRequest {
		t.Fatal("adding a refresh block after create must be rejected")
	}

	// Switching token_endpoint_auth arms requires a client_secret; restating
	// the same arm with one succeeds.
	if status, _ := s.do("POST", "/v1/vaults/"+vaultID+"/credentials/"+oauthID, map[string]any{
		"auth": map[string]any{"type": "mcp_oauth", "refresh": map[string]any{
			"token_endpoint_auth": map[string]any{"type": "client_secret_post"}}}}); status != http.StatusBadRequest {
		t.Fatal("switching arms without a client_secret must be rejected")
	}
	if status, resp := s.do("POST", "/v1/vaults/"+vaultID+"/credentials/"+oauthID, map[string]any{
		"auth": map[string]any{"type": "mcp_oauth", "refresh": map[string]any{
			"token_endpoint_auth": map[string]any{"type": "client_secret_post", "client_secret": "cs"}}}}); status != http.StatusOK {
		t.Fatalf("arm switch with a client_secret: status %d (%v)", status, resp)
	}
}

// The mutable arms of each auth variant update in place, and the rendered auth
// reflects the change while write-only secrets stay absent.
func TestCredentialUpdateSuccess(t *testing.T) {
	s := newTestServer(t)
	vaultID := createVault(t, s, "mutable")

	// environment_variable: rotate the secret, replace networking, and merge
	// injection_location (create defaulted both true; disable body, keep header).
	envID := createCredential(t, s, vaultID, envVarAuth("K"))["id"].(string)
	status, body := s.do("POST", "/v1/vaults/"+vaultID+"/credentials/"+envID, map[string]any{
		"display_name": "rotated",
		"metadata":     map[string]string{"env": "prod"},
		"auth": map[string]any{"type": "environment_variable",
			"secret_value":       "new-secret",
			"networking":         map[string]any{"type": "unrestricted"},
			"injection_location": map[string]any{"body": false}},
	})
	if status != http.StatusOK {
		t.Fatalf("env update: status %d (%v)", status, body)
	}
	auth := body["auth"].(map[string]any)
	if auth["networking"].(map[string]any)["type"] != "unrestricted" {
		t.Fatalf("networking not replaced: %v", auth["networking"])
	}
	loc := auth["injection_location"].(map[string]any)
	if loc["body"] != false || loc["header"] != true {
		t.Fatalf("injection_location merge wrong: %v", loc)
	}
	if body["display_name"] != "rotated" {
		t.Fatalf("display_name not updated: %v", body["display_name"])
	}
	if strings.Contains(fmt.Sprint(body), "new-secret") {
		t.Fatalf("rotated secret leaked into the response: %v", body)
	}

	// mcp_oauth: rotate access_token, set then clear expires_at, and update the
	// refresh block (refresh_token, scope set then null, same-arm restated).
	oauthID := createCredential(t, s, vaultID, map[string]any{
		"type": "mcp_oauth", "mcp_server_url": "https://m.example.com", "access_token": "at",
		"refresh": map[string]any{"client_id": "c", "refresh_token": "rt",
			"token_endpoint":      "https://a.example.com/t",
			"token_endpoint_auth": map[string]any{"type": "client_secret_basic", "client_secret": "cs"}},
	})["id"].(string)
	status, body = s.do("POST", "/v1/vaults/"+vaultID+"/credentials/"+oauthID, map[string]any{
		"auth": map[string]any{"type": "mcp_oauth",
			"access_token": "at2", "expires_at": "2027-01-01T00:00:00Z",
			"refresh": map[string]any{"refresh_token": "rt2", "scope": "read",
				"token_endpoint_auth": map[string]any{"type": "client_secret_basic"}}},
	})
	if status != http.StatusOK {
		t.Fatalf("oauth update: status %d (%v)", status, body)
	}
	if body["auth"].(map[string]any)["expires_at"] != "2027-01-01T00:00:00Z" {
		t.Fatalf("expires_at not set: %v", body["auth"])
	}
	// Clearing expires_at and the scope with explicit null.
	status, body = s.do("POST", "/v1/vaults/"+vaultID+"/credentials/"+oauthID, map[string]any{
		"auth": map[string]any{"type": "mcp_oauth", "expires_at": nil,
			"refresh": map[string]any{"scope": nil}},
	})
	if status != http.StatusOK || body["auth"].(map[string]any)["expires_at"] != nil {
		t.Fatalf("expires_at not cleared: status %d (%v)", status, body)
	}

	// static_bearer: rotate the token.
	bearerID := createCredential(t, s, vaultID, map[string]any{
		"type": "static_bearer", "mcp_server_url": "https://b.example.com", "token": "t"})["id"].(string)
	if status, body := s.do("POST", "/v1/vaults/"+vaultID+"/credentials/"+bearerID, map[string]any{
		"auth": map[string]any{"type": "static_bearer", "token": "t2"}}); status != http.StatusOK {
		t.Fatalf("bearer update: status %d (%v)", status, body)
	}
}

// Error branches across the vault handlers: missing rows 404, bad input 400.
func TestVaultHandlerEdges(t *testing.T) {
	s := newTestServer(t)
	const missing = "vlt_missing0000000000000000"

	// update
	if status, _ := s.do("POST", "/v1/vaults/"+missing, map[string]any{"display_name": "x"}); status != http.StatusNotFound {
		t.Fatalf("update missing vault: %d", status)
	}
	id := createVault(t, s, "edges")
	for name, body := range map[string]map[string]any{
		"clear display_name": {"display_name": nil},
		"long display_name":  {"display_name": strings.Repeat("n", 256)},
		"unknown key":        {"surprise": true},
	} {
		if status, resp := s.do("POST", "/v1/vaults/"+id, body); status != http.StatusBadRequest {
			t.Fatalf("update %s: status %d (%v)", name, status, resp)
		}
	}

	// archive / delete missing
	if status, _ := s.do("POST", "/v1/vaults/"+missing+"/archive", nil); status != http.StatusNotFound {
		t.Fatalf("archive missing: %d", status)
	}
	if status, _ := s.do("DELETE", "/v1/vaults/bad_prefix", nil); status != http.StatusNotFound {
		t.Fatalf("delete wrong-prefix id: %d", status)
	}

	// list query-param validation (vaults and nested credentials)
	for _, path := range []string{"/v1/vaults?limit=abc", "/v1/vaults?include_archived=maybe",
		"/v1/vaults/" + id + "/credentials?limit=abc"} {
		if status, _ := s.do("GET", path, nil); status != http.StatusBadRequest {
			t.Fatalf("bad query %q: status %d", path, status)
		}
	}
}

// Error branches on the credential get/update paths.
func TestCredentialUpdateErrors(t *testing.T) {
	s := newTestServer(t)
	vaultID := createVault(t, s, "cred-errs")
	envID := createCredential(t, s, vaultID, envVarAuth("K"))["id"].(string)
	oauthID := createCredential(t, s, vaultID, map[string]any{
		"type": "mcp_oauth", "mcp_server_url": "https://m.example.com", "access_token": "at",
		"refresh": map[string]any{"client_id": "c", "refresh_token": "rt",
			"token_endpoint":      "https://a.example.com/t",
			"token_endpoint_auth": map[string]any{"type": "none"}},
	})["id"].(string)

	// get a missing credential, and one under the wrong vault segment.
	if status, _ := s.do("GET", "/v1/vaults/"+vaultID+"/credentials/vcrd_missing0000000000000000", nil); status != http.StatusNotFound {
		t.Fatalf("get missing credential: %d", status)
	}
	if status, _ := s.do("GET", "/v1/vaults/"+vaultID+"/credentials/bad_id", nil); status != http.StatusNotFound {
		t.Fatalf("get malformed credential id: %d", status)
	}

	bad := []struct {
		id   string
		auth map[string]any
	}{
		{envID, map[string]any{"type": "environment_variable", "secret_value": ""}},                            // clear secret
		{envID, map[string]any{"type": "environment_variable", "networking": map[string]any{"type": "weird"}}}, // bad networking
		{envID, map[string]any{"type": "environment_variable", "networking": map[string]any{"type": "limited", // bad host
			"allowed_hosts": []string{"https://x"}}}},
		{envID, map[string]any{"type": "environment_variable", "injection_location": map[string]any{"body": nil}}},                    // null field
		{envID, map[string]any{"type": "environment_variable", "injection_location": map[string]any{"body": false, "header": false}}}, // none enabled
		{envID, map[string]any{"type": "environment_variable", "bogus": 1}},                                                           // unknown key
		{oauthID, map[string]any{"type": "mcp_oauth", "access_token": ""}},                                                            // clear access_token
		{oauthID, map[string]any{"type": "mcp_oauth", "expires_at": "notatime"}},                                                      // bad time
		{oauthID, map[string]any{"type": "mcp_oauth", "refresh": map[string]any{"refresh_token": ""}}},                                // clear refresh_token
	}
	for _, c := range bad {
		if status, resp := s.do("POST", "/v1/vaults/"+vaultID+"/credentials/"+c.id, map[string]any{"auth": c.auth}); status != http.StatusBadRequest {
			t.Fatalf("auth %v: status %d (%v), want 400", c.auth, status, resp)
		}
	}
	// display_name too long on update.
	if status, _ := s.do("POST", "/v1/vaults/"+vaultID+"/credentials/"+envID, map[string]any{
		"display_name": strings.Repeat("n", 256)}); status != http.StatusBadRequest {
		t.Fatal("long display_name must be rejected")
	}
}

// The update lock-narrowing re-seals from an unlocked read; if the stored
// secret rotates before the locked write, the compare-and-set must refuse (409)
// rather than clobber the concurrent rotation.
func TestCredentialUpdateConcurrentRotation(t *testing.T) {
	s := newTestServer(t)
	vaultID := createVault(t, s, "cas")
	credID := createCredential(t, s, vaultID, envVarAuth("K"))["id"].(string)

	restore := api.SetUpdateCredentialResealHookForTest(func() {
		if _, err := s.pool.Exec(t.Context(),
			`UPDATE vault_credentials SET secret_ciphertext = $2 WHERE id = $1`, credID, []byte{0}); err != nil {
			t.Fatal(err)
		}
	})
	defer restore()

	status, body := s.do("POST", "/v1/vaults/"+vaultID+"/credentials/"+credID, map[string]any{
		"auth": map[string]any{"type": "environment_variable", "secret_value": "rotated"}})
	if status != http.StatusConflict {
		t.Fatalf("a concurrent secret rotation must 409, got %d (%v)", status, body)
	}
}

func TestVaultRoutesRequireManagementAuth(t *testing.T) {
	s := newTestServer(t)
	vaultID := createVault(t, s, "authed")
	for _, probe := range []struct{ method, path string }{
		{"POST", "/v1/vaults"},
		{"GET", "/v1/vaults"},
		{"GET", "/v1/vaults/" + vaultID},
		{"POST", "/v1/vaults/" + vaultID + "/credentials"},
		{"POST", "/v1/vaults/" + vaultID + "/credentials/vcrd_x/mcp_oauth_validate"},
	} {
		res := s.doRaw(probe.method, probe.path, nil, nil)
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s %s without a key: status %d, want 401", probe.method, probe.path, res.StatusCode)
		}
	}
}

// Without a configured cipher the secret-bearing paths fail closed with a
// configuration error; metadata CRUD stays available (plan 12 D1).
func TestCredentialPathsWithoutCipher(t *testing.T) {
	pool := newPoolWithKey(t)
	srv := httptest.NewServer(api.NewHandler(pool, nil, nil))
	t.Cleanup(srv.Close)
	s := &tserver{t: t, url: srv.URL, pool: pool}

	status, body := s.do("POST", "/v1/vaults", map[string]any{"display_name": "no cipher"})
	if status != http.StatusOK {
		t.Fatalf("vault create must not need the cipher: %d (%v)", status, body)
	}
	vaultID := body["id"].(string)
	status, body = s.do("POST", "/v1/vaults/"+vaultID+"/credentials", map[string]any{"auth": envVarAuth("K")})
	if status != http.StatusInternalServerError || !strings.Contains(fmt.Sprint(body), "cipher") {
		t.Fatalf("credential create without a cipher: %d (%v), want a configuration error", status, body)
	}
}

func TestCredentialList(t *testing.T) {
	s := newTestServer(t)
	vaultID := createVault(t, s, "list")
	a := createCredential(t, s, vaultID, envVarAuth("A"))["id"].(string)
	createCredential(t, s, vaultID, envVarAuth("B"))
	s.do("POST", "/v1/vaults/"+vaultID+"/credentials/"+a+"/archive", nil)

	status, body := s.do("GET", "/v1/vaults/"+vaultID+"/credentials", nil)
	if status != http.StatusOK || len(body["data"].([]any)) != 1 {
		t.Fatalf("default list: status %d (%v)", status, body)
	}
	status, body = s.do("GET", "/v1/vaults/"+vaultID+"/credentials?include_archived=true", nil)
	if status != http.StatusOK || len(body["data"].([]any)) != 2 {
		t.Fatalf("include_archived: status %d (%v)", status, body)
	}
}
