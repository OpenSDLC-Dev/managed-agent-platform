package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gateconfig"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gatetoken"
)

// decodeBody reads and JSON-decodes a raw response body into an object (nil if
// the body is not a JSON object), closing the body.
func decodeBody(t *testing.T, res *http.Response) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	return obj
}

// gatedSession creates an agent, a limited-networking environment, a vault with
// one header-injected environment_variable credential, and a session attached to
// that vault. It returns the session id and the environment's allowed host.
func gatedSession(t *testing.T, s *tserver) (sessionID, allowedHost, secret string) {
	t.Helper()
	allowedHost = "api.example.com"
	secret = "s3cr3t-value"

	agent := createAgent(t, s, map[string]any{"name": "task-agent", "model": "claude-opus-4-8", "system": "base"})
	env := createEnvironment(t, s, map[string]any{
		"name": "gated-env",
		"config": map[string]any{
			"type":       "cloud",
			"networking": map[string]any{"type": "limited", "allowed_hosts": []any{allowedHost}},
		},
	})
	vaultID := createVault(t, s, "creds")
	createCredential(t, s, vaultID, map[string]any{
		"type": "environment_variable", "secret_name": "API_KEY", "secret_value": secret,
		"networking":         map[string]any{"type": "limited", "allowed_hosts": []any{allowedHost}},
		"injection_location": map[string]any{"header": true, "body": false},
	})
	sess := createSession(t, s, map[string]any{
		"agent": agent["id"], "environment_id": env["id"], "vault_ids": []any{vaultID},
	})
	return sess["id"].(string), allowedHost, secret
}

// mintGateToken issues and stores a live gate token for the session.
func mintGateToken(t *testing.T, s *tserver, sessionID string) string {
	t.Helper()
	token := gatetoken.Mint()
	if err := gatetoken.Ensure(context.Background(), s.pool, sessionID, token); err != nil {
		t.Fatalf("Ensure gate token: %v", err)
	}
	return token
}

func TestGateConfigServesNetworkingAndCredentials(t *testing.T) {
	s := newTestServer(t)
	sessionID, allowedHost, secret := gatedSession(t, s)
	token := mintGateToken(t, s, sessionID)

	cfg, err := gateconfig.NewClient(s.url, token, nil).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// Environment (level-1) policy.
	if cfg.Networking.Type != "limited" ||
		len(cfg.Networking.AllowedHosts) != 1 || cfg.Networking.AllowedHosts[0] != allowedHost {
		t.Errorf("networking = %+v", cfg.Networking)
	}

	// The one resolved credential, decrypted, with its placeholder derived the
	// same way the executor injects it into the sandbox.
	if len(cfg.Credentials) != 1 {
		t.Fatalf("credentials = %d, want 1", len(cfg.Credentials))
	}
	cr := cfg.Credentials[0]
	if want := egress.Placeholder(sessionID, "API_KEY"); cr.Placeholder != want {
		t.Errorf("placeholder = %q, want %q", cr.Placeholder, want)
	}
	if cr.Secret != secret {
		t.Errorf("secret not decrypted: got %q, want %q", cr.Secret, secret)
	}
	if cr.CredentialID == "" {
		t.Error("credential_id is empty")
	}
	if cr.Networking.Type != "limited" ||
		len(cr.Networking.AllowedHosts) != 1 || cr.Networking.AllowedHosts[0] != allowedHost {
		t.Errorf("credential networking = %+v", cr.Networking)
	}
	if !cr.InjectionLocation.Header || cr.InjectionLocation.Body {
		t.Errorf("injection_location = %+v, want header-only", cr.InjectionLocation)
	}
}

func TestGateConfigUnrestrictedCredential(t *testing.T) {
	s := newTestServer(t)
	agent := createAgent(t, s, map[string]any{"name": "a", "model": "claude-opus-4-8", "system": "base"})
	env := createEnvironment(t, s, map[string]any{"name": "env"})
	vaultID := createVault(t, s, "creds")
	createCredential(t, s, vaultID, map[string]any{
		"type": "environment_variable", "secret_name": "TOKEN", "secret_value": "v",
		"networking":         map[string]any{"type": "unrestricted"},
		"injection_location": map[string]any{"header": true, "body": false},
	})
	sess := createSession(t, s, map[string]any{
		"agent": agent["id"], "environment_id": env["id"], "vault_ids": []any{vaultID},
	})
	token := mintGateToken(t, s, sess["id"].(string))

	cfg, err := gateconfig.NewClient(s.url, token, nil).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(cfg.Credentials) != 1 {
		t.Fatalf("credentials = %d, want 1", len(cfg.Credentials))
	}
	// The unrestricted arm of toGateCredentials must render type "unrestricted"
	// with no fabricated allowed_hosts — a secret usable against any host.
	cr := cfg.Credentials[0]
	if cr.Networking.Type != "unrestricted" {
		t.Errorf("credential networking type = %q, want unrestricted", cr.Networking.Type)
	}
	if len(cr.Networking.AllowedHosts) != 0 {
		t.Errorf("unrestricted credential carries allowed_hosts = %v, want none", cr.Networking.AllowedHosts)
	}
}

func TestGateConfigNoVaultsIsEmptyCredentials(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s) // unrestricted env, no vaults
	sess := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})
	token := mintGateToken(t, s, sess["id"].(string))

	cfg, err := gateconfig.NewClient(s.url, token, nil).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if cfg.Networking.Type != "unrestricted" {
		t.Errorf("networking = %+v, want unrestricted", cfg.Networking)
	}
	if len(cfg.Credentials) != 0 {
		t.Errorf("credentials = %d, want 0", len(cfg.Credentials))
	}
}

func TestGateConfigArchivedSessionFailsClosed(t *testing.T) {
	s := newTestServer(t)
	sessionID, _, _ := gatedSession(t, s)
	token := mintGateToken(t, s, sessionID)

	if status, _ := s.do(http.MethodPost, "/v1/sessions/"+sessionID+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive session: status %d", status)
	}
	// An archived session's gate must stop being served: 401, surfaced as the
	// client's unambiguous fail-closed sentinel.
	if _, err := gateconfig.NewClient(s.url, token, nil).Fetch(context.Background()); !errors.Is(err, gateconfig.ErrUnauthorized) {
		t.Fatalf("Fetch on archived session = %v, want ErrUnauthorized", err)
	}
}

func TestGateConfigAuthLane(t *testing.T) {
	s := newTestServer(t)

	// No Authorization header → 401 with the wire error envelope.
	res := s.doRaw(http.MethodGet, gateconfig.Path, nil, nil)
	body := decodeBody(t, res)
	wantErr(t, res.StatusCode, body, http.StatusUnauthorized, "authentication_error")

	// A management x-api-key does not open the gate lane (path-selected) → 401.
	res = s.doRaw(http.MethodGet, gateconfig.Path, nil, map[string]string{"x-api-key": testKey})
	body = decodeBody(t, res)
	wantErr(t, res.StatusCode, body, http.StatusUnauthorized, "authentication_error")

	// An unknown gate token → 401.
	res = s.doRaw(http.MethodGet, gateconfig.Path, nil, map[string]string{"Authorization": "Bearer " + gatetoken.Mint()})
	body = decodeBody(t, res)
	wantErr(t, res.StatusCode, body, http.StatusUnauthorized, "authentication_error")

	// A non-GET method under a valid token passes auth (auth precedes routing)
	// and hits the method-less fallback, keeping the wire error envelope (405).
	sessionID, _, _ := gatedSession(t, s)
	token := mintGateToken(t, s, sessionID)
	res = s.doRaw(http.MethodPost, gateconfig.Path, nil, map[string]string{"Authorization": "Bearer " + token})
	body = decodeBody(t, res)
	wantErr(t, res.StatusCode, body, http.StatusMethodNotAllowed, "invalid_request_error")

	// Lane isolation, both directions. A valid gate token must not authenticate
	// a management or a worker route (it is neither an x-api-key nor an
	// environment key), and a valid environment key must not open the gate lane.
	gateBearer := map[string]string{"Authorization": "Bearer " + token}
	res = s.doRaw(http.MethodGet, "/v1/agents", nil, gateBearer) // management route
	body = decodeBody(t, res)
	wantErr(t, res.StatusCode, body, http.StatusUnauthorized, "authentication_error")

	res = s.doRaw(http.MethodGet, "/v1/environments/env_x/work/poll", nil, gateBearer) // worker route
	body = decodeBody(t, res)
	wantErr(t, res.StatusCode, body, http.StatusUnauthorized, "authentication_error")

	env := createEnvironment(t, s, map[string]any{"name": "worker-env"})
	if err := api.EnsureEnvironmentKey(context.Background(), s.pool, env["id"].(string), "ek-reverse"); err != nil {
		t.Fatalf("EnsureEnvironmentKey: %v", err)
	}
	res = s.doRaw(http.MethodGet, gateconfig.Path, nil, map[string]string{"Authorization": "Bearer ek-reverse"})
	body = decodeBody(t, res)
	wantErr(t, res.StatusCode, body, http.StatusUnauthorized, "authentication_error")

	// A percent-encoded slash does not forge the gate path: it fails the exact
	// isGateConfigPath match, falls to management auth, and 401s under a gate
	// token (never routing to the handler).
	res = s.doRaw(http.MethodGet, "/internal/v1/gate%2Fconfig", nil, gateBearer)
	body = decodeBody(t, res)
	wantErr(t, res.StatusCode, body, http.StatusUnauthorized, "authentication_error")
}
