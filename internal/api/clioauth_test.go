package api_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway"
	gatewayadmin "github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/admin"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/catalog"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	gatewaystore "github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/local"
)

const antClientID = "41077d10-94b8-4194-be48-d251e9eb21b4"

func TestCLIOAuthConsentAndCodeExchange(t *testing.T) {
	s := newTestServer(t)
	const scope = "user:profile user:inference user:developer"
	const state = "7fZ-WEzEOC5E9uuW0M36dq3aAjhDXj22cK1zX2K0USg"
	verifier := strings.Repeat("a", 64)
	digest := sha256.Sum256([]byte(verifier))
	redirect := nativeOrigin + "/oauth/code/callback?app=anthropic-cli"
	params := url.Values{
		"client_id": {antClientID}, "response_type": {"code"}, "redirect_uri": {redirect},
		"scope": {scope}, "state": {state}, "code_challenge_method": {"S256"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(digest[:])},
	}

	authorize := "/oauth/authorize?" + params.Encode()
	request := s.doRaw(http.MethodGet, authorize, nil, map[string]string{"Cookie": s.cookie})
	html, err := io.ReadAll(request.Body)
	request.Body.Close()
	if err != nil || request.StatusCode != http.StatusOK || !strings.Contains(string(html), "Authorize CLI") {
		t.Fatalf("consent page: HTTP %d, err %v", request.StatusCode, err)
	}

	params.Set("csrf_token", s.csrf)
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Cookie": s.cookie, "Origin": nativeOrigin}
	response := s.doRaw(http.MethodPost, "/oauth/authorize", params.Encode(), form)
	html, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("consent submit: HTTP %d, err %v", response.StatusCode, err)
	}
	_, start, ok := strings.Cut(string(html), "id=\"authorization-code\">")
	code, _, found := strings.Cut(start, "#"+state+"</code>")
	if !ok || !found || code == "" {
		t.Fatal("consent did not show the CLI copy code and state")
	}
	exchange := url.Values{
		"grant_type": {"authorization_code"}, "client_id": {antClientID},
		"code": {code}, "state": {state}, "redirect_uri": {redirect},
		"code_verifier": {strings.Repeat("b", 64)},
	}
	headers := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	status, body := s.decode(s.doRaw(http.MethodPost, "/v1/oauth/token", exchange.Encode(), headers))
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("incorrect verifier: HTTP %d", status)
	}
	exchange.Set("code_verifier", verifier)
	for _, tc := range []struct{ field, bad, good string }{
		{"state", "different-state", state},
		{"redirect_uri", "http://localhost:58423/callback", redirect},
	} {
		exchange.Set(tc.field, tc.bad)
		status, body = s.decode(s.doRaw(http.MethodPost, "/v1/oauth/token", exchange.Encode(), headers))
		if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
			t.Fatalf("incorrect %s: HTTP %d", tc.field, status)
		}
		exchange.Set(tc.field, tc.good)
	}
	status, body = s.decode(s.doRaw(http.MethodPost, "/v1/oauth/token", exchange.Encode(), headers))
	token, _ := body["access_token"].(string)
	if status != http.StatusOK || token == "" || body["scope"] != scope || body["token_type"] != "Bearer" {
		t.Fatalf("token exchange: HTTP %d", status)
	}
	if len(body) != 10 {
		t.Fatalf("token response has %d fields, want 10", len(body))
	}
	for _, field := range []string{"refresh_token", "token_uuid"} {
		if value, ok := body[field].(string); !ok || value == "" {
			t.Errorf("%s must be a nonempty string", field)
		}
	}
	for _, field := range []string{"expires_in", "refresh_token_expires_in"} {
		if seconds, ok := body[field].(float64); !ok || seconds <= 0 {
			t.Errorf("%s must be a positive number", field)
		}
	}
	for field, keys := range map[string][2]string{
		"organization": {"uuid", "name"},
		"account":      {"uuid", "email_address"},
		"workspace":    {"id", "name"},
	} {
		object, _ := body[field].(map[string]any)
		_, first := object[keys[0]].(string)
		_, second := object[keys[1]].(string)
		if len(object) != 2 || !first || !second {
			t.Errorf("%s must contain the recorded string fields %v", field, keys)
		}
	}
	status, _ = s.decode(s.doRaw(http.MethodGet, "/v1/agents", nil, map[string]string{"Authorization": "Bearer " + token}))
	if status != http.StatusOK {
		t.Fatalf("OAuth token on management list: HTTP %d", status)
	}
	status, body = s.decode(s.doRaw(http.MethodPost, "/v1/oauth/token", exchange.Encode(), headers))
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("reused code: HTTP %d", status)
	}
}

func TestCLIOAuthConsentBoundaries(t *testing.T) {
	s := newTestServer(t)
	h := sha256.Sum256([]byte(strings.Repeat("x", 64)))
	q := url.Values{
		"client_id": {antClientID}, "response_type": {"code"}, "scope": {"user:profile"}, "state": {"login-state"},
		"redirect_uri": {"http://localhost:58423/callback"}, "code_challenge_method": {"S256"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(h[:])},
	}
	uri := "/oauth/authorize"
	res := s.doRaw(http.MethodGet, uri+"?"+q.Encode(), nil, nil)
	html, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || res.StatusCode != http.StatusOK || !strings.Contains(string(html), `href="/login"`) ||
		strings.Contains(string(html), `name="csrf_token"`) {
		t.Fatalf("signed-out consent: HTTP %d, err %v", res.StatusCode, err)
	}
	for _, tc := range []struct{ name, field, value string }{
		{"foreign host", "redirect_uri", "http://example.invalid:58423/callback"},
		{"userinfo", "redirect_uri", "http://localhost:58423@example.invalid/callback"},
		{"different path", "redirect_uri", "http://localhost:58423/other"},
		{"manual query change", "redirect_uri", nativeOrigin + "/oauth/code/callback?app=other"},
		{"plain PKCE", "code_challenge_method", "plain"},
		{"unsupported scope", "scope", "workspace:developer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			form, _ := url.ParseQuery(q.Encode())
			form.Set(tc.field, tc.value)
			status, body := s.decode(s.doRaw(http.MethodGet, uri+"?"+form.Encode(), nil, map[string]string{"Cookie": s.cookie}))
			wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
		})
	}
	headers := map[string]string{"Cookie": s.cookie, "Origin": nativeOrigin, "Content-Type": "application/x-www-form-urlencoded"}
	post := func() int {
		status, _ := s.decode(s.doRaw(http.MethodPost, uri, q.Encode(), headers))
		return status
	}
	if status := post(); status != http.StatusForbidden {
		t.Fatalf("missing CSRF token: HTTP %d", status)
	}
	q.Set("csrf_token", s.csrf)
	headers["Origin"] = "https://other.example.invalid"
	if status := post(); status != http.StatusForbidden {
		t.Fatalf("other origin: HTTP %d", status)
	}
}

func TestCLIOAuthUsesCurrentRoleAndRevokesOnRemoval(t *testing.T) {
	s := newTestServer(t)
	issued := issueCLITokens(t, s, "user:developer")
	token := issued["access_token"].(string)
	list := func() int {
		status, _ := s.decode(s.doRaw(http.MethodGet, "/v1/agents", nil, map[string]string{"Authorization": "Bearer " + token}))
		return status
	}
	var owner string
	if err := s.pool.QueryRow(t.Context(), `SELECT id FROM principals WHERE role='admin'`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		sql  string
		want int
	}{
		{`UPDATE principals SET role='user' WHERE id=$1`, http.StatusForbidden},
		{`UPDATE principals SET role='admin' WHERE id=$1`, http.StatusOK},
		{`UPDATE principals SET status='deactivated', deactivated_at=clock_timestamp() WHERE id=$1`, http.StatusUnauthorized},
		{`UPDATE principals SET status='active', deactivated_at=NULL WHERE id=$1`, http.StatusUnauthorized},
	} {
		if _, err := s.pool.Exec(t.Context(), tc.sql, owner); err != nil {
			t.Fatal(err)
		}
		if status := list(); status != tc.want {
			t.Fatalf("after %s: HTTP %d, want %d", tc.sql, status, tc.want)
		}
	}
}

func TestCLIOAuthRefreshRotation(t *testing.T) {
	s := newTestServer(t)
	token := issueCLITokens(t, s, "user:developer")
	refresh := func(value string) (int, map[string]any) {
		return s.decode(s.doRaw(http.MethodPost, "/v1/oauth/token", map[string]string{
			"grant_type": "refresh_token", "client_id": antClientID, "refresh_token": value,
		}, map[string]string{"Content-Type": "application/json", "anthropic-beta": "oauth-2025-04-20"}))
	}
	list := func(value string) int {
		status, _ := s.decode(s.doRaw(http.MethodGet, "/v1/agents", nil, map[string]string{"Authorization": "Bearer " + value}))
		return status
	}
	status, rotated := refresh(token["refresh_token"].(string))
	if status != http.StatusOK || rotated["scope"] != "user:developer" || rotated["refresh_token"] == token["refresh_token"] {
		t.Fatalf("refresh rotation: HTTP %d", status)
	}
	if list(token["access_token"].(string)) != http.StatusUnauthorized || list(rotated["access_token"].(string)) != http.StatusOK {
		t.Fatal("refresh did not replace the current access token")
	}
	status, body := refresh(token["refresh_token"].(string))
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("spent refresh: HTTP %d", status)
	}
	if list(rotated["access_token"].(string)) != http.StatusUnauthorized {
		t.Fatal("refresh replay did not revoke the grant")
	}
	status, body = refresh(rotated["refresh_token"].(string))
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("refresh of revoked grant: HTTP %d", status)
	}
}

func TestCLIOAuthScopeMatrix(t *testing.T) {
	s := newTestServer(t)
	gateway := cliGatewayFixture(t, s)
	for _, tc := range []struct {
		name, scope                              string
		management, files, inference, workspaces int
	}{
		{"default", "user:profile user:inference user:developer", 200, 200, 200, 403},
		{"admin", "user:profile user:inference user:developer org:admin", 200, 200, 200, 200},
		{"inference", "user:inference", 401, 403, 200, 403},
		{"profile", "user:profile", 401, 403, 403, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issued := issueCLITokens(t, s, tc.scope)
			token := issued["access_token"].(string)
			headers := map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/json", "anthropic-version": "2026-10-21"}
			for _, probe := range []struct {
				method, path, message string
				want                  int
				body                  any
			}{
				{"GET", "/v1/agents?beta=true", "OAuth token lacks a scope this endpoint accepts", tc.management, nil},
				{"POST", "/v1/agents?beta=true", "OAuth token lacks a scope this endpoint accepts", tc.management, map[string]string{"name": "oauth-agent", "model": "claude-opus-4-8"}},
				{"GET", "/v1/sessions?beta=true", "OAuth token lacks a scope this endpoint accepts", tc.management, nil},
				{"GET", "/v1/files", "OAuth token does not meet scope requirement any_of(org:file_upload, user:developer, user:file_upload, user:managed_agents, workspace:developer)", tc.files, nil},
				{"GET", "/v1/skills?beta=true", "OAuth token does not meet scope requirement any_of(org:skills, user:developer, user:managed_agents, user:skills, workspace:developer, workspace:skills)", tc.files, nil},
				{"GET", "/v1/organizations/workspaces", "Authentication method not allowed for this endpoint.", tc.workspaces, nil},
				{"GET", "/api/auth/session", "a browser session cookie is required", http.StatusUnauthorized, nil},
			} {
				status, body := s.decode(s.doRaw(probe.method, probe.path, probe.body, headers))
				if status != probe.want || status >= 400 && errMessage(body) != probe.message {
					t.Errorf("%s %s: HTTP %d %v, want %d", probe.method, probe.path, status, body, probe.want)
				}
				if probe.path == "/v1/organizations/workspaces" && status == http.StatusOK {
					data, array := body["data"].([]any)
					_, first := body["first_id"]
					_, last := body["last_id"]
					if len(body) != 4 || !array || len(data) != 0 || body["has_more"] != false ||
						!first || !last || body["first_id"] != nil || body["last_id"] != nil {
						t.Errorf("workspaces page: %v, want the recorded empty page", body)
					}
				}
			}
			for _, probe := range []struct{ method, path, body, message string }{
				{"GET", "/v1/models", "", "OAuth token does not meet scope requirement any_of(user:ccr_inference, user:developer, user:inference, workspace:developer, workspace:inference)"},
				{"POST", "/v1/messages", `{"model":"cli-model","max_tokens":1,"messages":[{"role":"user","content":"test"}]}`, "OAuth token does not meet scope requirement any_of(org:service_key_inference, user:ccr_inference, user:developer, user:inference, user:voice, workspace:developer, workspace:inference, workspace:messages_create)"},
				{"POST", "/v1/messages/count_tokens", `{"model":"cli-model","messages":[{"role":"user","content":"test"}]}`, "OAuth token does not meet scope requirement; one of [user:inference, user:developer, user:ccr_inference, org:service_key_inference, workspace:inference, workspace:messages_create, workspace:developer] is required"},
			} {
				status, body := cliGatewayCall(t, gateway, probe.method, probe.path, probe.body, token)
				if status != tc.inference || status >= 400 && errMessage(body) != probe.message {
					t.Errorf("%s: HTTP %d %v, want %d", probe.path, status, body, tc.inference)
				}
			}
			if status, _ := cliGatewayCall(t, gateway, "GET", "/admin/v1/providers", "", token); status != http.StatusUnauthorized {
				t.Errorf("gateway admin accepted CLI bearer: HTTP %d", status)
			}
		})
	}
}

func cliGatewayCall(t *testing.T, base, method, path, body, token string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2026-10-21")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var result map[string]any
	if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, result
}

func cliGatewayFixture(t *testing.T, s *tserver) string {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/messages/count_tokens" {
			_, _ = io.WriteString(w, `{"input_tokens":1}`)
			return
		}
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"id":"msg_cli","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"cli-upstream","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(upstream.Close)
	ctx := t.Context()
	cipher, err := local.New(local.Config{KeyID: "cli", Key: bytes.Repeat([]byte{8}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	gwStore := gatewaystore.New(s.pool)
	p, err := gwStore.CreateProvider(ctx, gatewaystore.Provider{Name: "cli", Profile: "anthropic-generic", Enabled: true,
		Endpoints: map[profile.Protocol]string{profile.Anthropic: upstream.URL}})
	if err != nil {
		t.Fatal(err)
	}
	sealed, kid, err := cipher.Encrypt(ctx, []byte("fixture-vendor-key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gwStore.CreateCredential(ctx, gatewaystore.Credential{ProviderID: p.ID, Ciphertext: sealed, KeyID: kid, LastFour: "-key", Weight: 1, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	d, err := gwStore.CreateDeployment(ctx, gatewaystore.Deployment{ProviderID: p.ID, UpstreamModel: "cli-upstream", Kind: gatewaystore.KindChat, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gwStore.CreateAlias(ctx, gatewaystore.Alias{Name: "cli-model", Targets: []gatewaystore.Target{{DeploymentID: d.ID, Weight: 1}}}); err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.New(ctx, s.pool, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	manager := login.New(s.pool, login.Config{Mode: login.ModePassword, PublicOrigin: nativeOrigin, Development: true})
	cfg := modelgateway.Config{Catalog: cat, Store: gwStore, Keys: s.pool, Cipher: cipher}
	prober, err := modelgateway.NewProber(cfg)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := gatewayadmin.New(gatewayadmin.Config{Store: gwStore, Cipher: cipher, Login: manager, Prober: prober})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Admin = admin
	handler, err := modelgateway.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL
}

func issueCLITokens(t *testing.T, s *tserver, scope string) map[string]any {
	t.Helper()
	const state = "cli-probe-state"
	verifier := strings.Repeat("c", 64)
	pkce := sha256.Sum256([]byte(verifier))
	redirect := nativeOrigin + "/oauth/code/callback?app=anthropic-cli"
	q := url.Values{
		"client_id": {antClientID}, "response_type": {"code"}, "redirect_uri": {redirect},
		"scope": {scope}, "state": {state}, "code_challenge_method": {"S256"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(pkce[:])}, "csrf_token": {s.csrf},
	}
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Cookie": s.cookie, "Origin": nativeOrigin}
	res := s.doRaw(http.MethodPost, "/oauth/authorize", q.Encode(), form)
	defer res.Body.Close()
	html, err := io.ReadAll(res.Body)
	_, after, ok := strings.Cut(string(html), "id=\"authorization-code\">")
	code, _, cut := strings.Cut(after, "#"+state+"</code>")
	if err != nil || res.StatusCode != http.StatusOK || !ok || !cut || code == "" {
		t.Fatalf("consent: HTTP %d, err %v", res.StatusCode, err)
	}
	q = url.Values{"grant_type": {"authorization_code"}, "client_id": {antClientID}, "code": {code},
		"state": {state}, "redirect_uri": {redirect}, "code_verifier": {verifier}}
	status, body := s.decode(s.doRaw(http.MethodPost, "/v1/oauth/token", q.Encode(),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"}))
	if status != http.StatusOK || body["access_token"] == nil || body["refresh_token"] == nil {
		t.Fatalf("exchange: HTTP %d", status)
	}
	return body
}
