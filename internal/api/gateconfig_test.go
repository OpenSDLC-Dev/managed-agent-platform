package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

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

// The sandbox half of allow_mcp_servers: the gate is told the hosts the
// session's agent declares MCP servers at, and only when the policy can use
// them. A gate that is never sent a host cannot be made to admit it.
func TestGateConfigServesTheAgentsMCPHostsUnderTheFlag(t *testing.T) {
	for name, tc := range map[string]struct {
		networking map[string]any
		want       []string
	}{
		"limited with the flag": {
			map[string]any{"type": "limited", "allowed_hosts": []any{"api.example.com"},
				"allow_mcp_servers": true},
			[]string{"mcp.example"},
		},
		"limited without it": {
			map[string]any{"type": "limited", "allowed_hosts": []any{"api.example.com"}},
			nil,
		},
		// Nothing to widen: every host is admitted already.
		"unrestricted": {map[string]any{"type": "unrestricted"}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			s := newTestServer(t)
			agent := createAgent(t, s, map[string]any{
				"name": "task-agent", "model": "claude-opus-4-8", "system": "base",
				"mcp_servers": []any{mcpServer("docs")},
				"tools":       []any{mcpToolset("docs")},
			})
			env := createEnvironment(t, s, map[string]any{
				"name":   "gated-env",
				"config": map[string]any{"type": "cloud", "networking": tc.networking},
			})
			sess := createSession(t, s, map[string]any{
				"agent": agent["id"], "environment_id": env["id"],
			})
			sessionID := sess["id"].(string)

			cfg, err := gateconfig.NewClient(s.url, mintGateToken(t, s, sessionID), nil).
				Fetch(context.Background())
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if !slices.Equal(cfg.MCPServerHosts, tc.want) {
				t.Errorf("mcp_server_hosts = %v, want %v", cfg.MCPServerHosts, tc.want)
			}
			// Whatever the flag says, the operator's own list is served as written.
			if tc.networking["type"] == "limited" &&
				(len(cfg.Networking.AllowedHosts) != 1 || cfg.Networking.AllowedHosts[0] != "api.example.com") {
				t.Errorf("allowed_hosts = %v, want the configured list untouched", cfg.Networking.AllowedHosts)
			}
		})
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

// unreachableErrors lists the session's credential_host_unreachable_error
// session.error events as seen on the management events wire.
func unreachableErrors(t *testing.T, s *tserver, sessionID string) []map[string]any {
	t.Helper()
	status, res := s.do(http.MethodGet, "/v1/sessions/"+sessionID+"/events", nil)
	if status != http.StatusOK {
		t.Fatalf("list events: status %d", status)
	}
	var out []map[string]any
	for _, e := range res["data"].([]any) {
		ev := e.(map[string]any)
		if ev["type"] != "session.error" {
			continue
		}
		errObj, _ := ev["error"].(map[string]any)
		if errObj["type"] == "credential_host_unreachable_error" {
			out = append(out, ev)
		}
	}
	return out
}

// waitUnreachableErrors polls for the expected number of unreachable-error
// events — emission is asynchronous, detached from the config response — and
// returns them, failing the test if they do not arrive in time.
func waitUnreachableErrors(t *testing.T, s *tserver, sessionID string, want int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		evs := unreachableErrors(t, s, sessionID)
		if len(evs) == want {
			return evs
		}
		if time.Now().After(deadline) {
			t.Fatalf("unreachable-error events = %d, want %d", len(evs), want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// settleEmission gives the detached emission goroutine time to land (or not)
// before an assertion about its absence or its dedupe. A too-short settle can
// only false-pass an absence check, never flake a run red.
func settleEmission() { time.Sleep(500 * time.Millisecond) }

// TestGateConfigEmitsCredentialHostUnreachableError: a credential whose
// allowed_hosts includes a host the environment's networking policy does not
// permit (the SDK's documented trigger) surfaces a session.error carrying the
// credential_host_unreachable_error variant — best-effort once (deduped
// against the events table), not once per fetch. The dedupe and no-conflict
// branches are additionally pinned deterministically by the synchronous
// white-box tests in gateconfig_internal_test.go; these HTTP tests prove the
// async wiring.
func TestGateConfigEmitsCredentialHostUnreachableError(t *testing.T) {
	s := newTestServer(t)
	agent := createAgent(t, s, map[string]any{"name": "a", "model": "claude-opus-4-8", "system": "base"})
	env := createEnvironment(t, s, map[string]any{
		"name": "gated-env",
		"config": map[string]any{
			"type":       "cloud",
			"networking": map[string]any{"type": "limited", "allowed_hosts": []any{"env.example.com"}},
		},
	})
	vaultID := createVault(t, s, "creds")
	cred := createCredential(t, s, vaultID, map[string]any{
		"type": "environment_variable", "secret_name": "API_KEY", "secret_value": "v",
		// env.example.com is reachable; blocked.example.com is outside the
		// environment's policy — the credential can never be used there.
		"networking":         map[string]any{"type": "limited", "allowed_hosts": []any{"env.example.com", "blocked.example.com"}},
		"injection_location": map[string]any{"header": true, "body": false},
	})
	sess := createSession(t, s, map[string]any{
		"agent": agent["id"], "environment_id": env["id"], "vault_ids": []any{vaultID},
	})
	sessionID := sess["id"].(string)
	token := mintGateToken(t, s, sessionID)

	if _, err := gateconfig.NewClient(s.url, token, nil).Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	evs := waitUnreachableErrors(t, s, sessionID, 1)
	ev := evs[0]
	if id, _ := ev["id"].(string); len(id) < 6 || id[:5] != "sevt_" {
		t.Errorf("event id = %v, want an sevt_ id", ev["id"])
	}
	if ev["processed_at"] == nil {
		t.Error("event has no processed_at")
	}
	errObj := ev["error"].(map[string]any)
	if errObj["credential_id"] != cred["id"] {
		t.Errorf("credential_id = %v, want %v", errObj["credential_id"], cred["id"])
	}
	if errObj["vault_id"] != vaultID {
		t.Errorf("vault_id = %v, want %v", errObj["vault_id"], vaultID)
	}
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "blocked.example.com") {
		t.Errorf("message %q does not name the conflicting entry", msg)
	}
	rs, _ := errObj["retry_status"].(map[string]any)
	if rs["type"] != "retrying" {
		t.Errorf("retry_status = %v, want {type: retrying}", errObj["retry_status"])
	}

	// A second fetch re-detects the same conflict but must not re-emit.
	if _, err := gateconfig.NewClient(s.url, token, nil).Fetch(context.Background()); err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	settleEmission()
	if evs := unreachableErrors(t, s, sessionID); len(evs) != 1 {
		t.Errorf("after second fetch, unreachable-error events = %d, want still 1", len(evs))
	}
}

// TestGateConfigNoConflictEmitsNothing: a credential wholly inside the
// environment's policy — or on an unrestricted environment, or itself
// unrestricted — is not a conflict.
func TestGateConfigNoConflictEmitsNothing(t *testing.T) {
	s := newTestServer(t)

	// gatedSession: credential allowed_hosts == environment allowed_hosts.
	sessionID, _, _ := gatedSession(t, s)
	token := mintGateToken(t, s, sessionID)
	if _, err := gateconfig.NewClient(s.url, token, nil).Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	settleEmission()
	if evs := unreachableErrors(t, s, sessionID); len(evs) != 0 {
		t.Errorf("aligned credential emitted %d unreachable errors, want 0", len(evs))
	}

	// Unrestricted environment: everything is permitted, nothing conflicts.
	agent := createAgent(t, s, map[string]any{"name": "b", "model": "claude-opus-4-8", "system": "base"})
	env := createEnvironment(t, s, map[string]any{"name": "open-env"})
	vaultID := createVault(t, s, "creds2")
	createCredential(t, s, vaultID, map[string]any{
		"type": "environment_variable", "secret_name": "TOKEN", "secret_value": "v",
		"networking":         map[string]any{"type": "limited", "allowed_hosts": []any{"anywhere.example.com"}},
		"injection_location": map[string]any{"header": true, "body": false},
	})
	sess := createSession(t, s, map[string]any{
		"agent": agent["id"], "environment_id": env["id"], "vault_ids": []any{vaultID},
	})
	token2 := mintGateToken(t, s, sess["id"].(string))
	if _, err := gateconfig.NewClient(s.url, token2, nil).Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	settleEmission()
	if evs := unreachableErrors(t, s, sess["id"].(string)); len(evs) != 0 {
		t.Errorf("unrestricted environment emitted %d unreachable errors, want 0", len(evs))
	}

	// Unrestricted credential on a limited environment: it has no allowed_hosts
	// of its own, so the SDK's trigger sentence cannot apply — how far it
	// reaches is the environment policy's call, not a conflict.
	agent2 := createAgent(t, s, map[string]any{"name": "c", "model": "claude-opus-4-8", "system": "base"})
	env2 := createEnvironment(t, s, map[string]any{
		"name": "narrow-env",
		"config": map[string]any{
			"type":       "cloud",
			"networking": map[string]any{"type": "limited", "allowed_hosts": []any{"only.example.com"}},
		},
	})
	vaultID2 := createVault(t, s, "creds3")
	createCredential(t, s, vaultID2, map[string]any{
		"type": "environment_variable", "secret_name": "WIDE", "secret_value": "v",
		"networking":         map[string]any{"type": "unrestricted"},
		"injection_location": map[string]any{"header": true, "body": false},
	})
	sess2 := createSession(t, s, map[string]any{
		"agent": agent2["id"], "environment_id": env2["id"], "vault_ids": []any{vaultID2},
	})
	token3 := mintGateToken(t, s, sess2["id"].(string))
	if _, err := gateconfig.NewClient(s.url, token3, nil).Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	settleEmission()
	if evs := unreachableErrors(t, s, sess2["id"].(string)); len(evs) != 0 {
		t.Errorf("unrestricted credential emitted %d unreachable errors, want 0", len(evs))
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
	envKey := issueKey(t, s.pool, env["id"].(string), "ek-reverse")
	res = s.doRaw(http.MethodGet, gateconfig.Path, nil, map[string]string{"Authorization": "Bearer " + envKey})
	body = decodeBody(t, res)
	wantErr(t, res.StatusCode, body, http.StatusUnauthorized, "authentication_error")

	// A percent-encoded slash does not forge the gate path: it fails the exact
	// isGateConfigPath match, falls to management auth, and 401s under a gate
	// token (never routing to the handler).
	res = s.doRaw(http.MethodGet, "/internal/v1/gate%2Fconfig", nil, gateBearer)
	body = decodeBody(t, res)
	wantErr(t, res.StatusCode, body, http.StatusUnauthorized, "authentication_error")
}
