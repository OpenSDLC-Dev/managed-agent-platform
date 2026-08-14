// Package gateconfig is a session egress gate's client for the control plane's
// internal gate-config endpoint (GET /internal/v1/gate/config), and the wire
// contract the two share. The gate presents its per-session gtk_ token
// (internal/gatetoken) and receives the two inputs internal/gate needs: the
// environment's request-level networking policy, and the session's resolved,
// decrypted vault credentials for egress substitution. The token is fixed for
// the session's life, so the gate holds one Client and re-fetches periodically
// (docs/plan/12_vaults-credentials.md slice 4).
//
// Neither this client nor the endpoint is on the public /v1 wire — the gate is a
// platform-internal process, recorded as a deliberate divergence (DIVERGENCES).
// Secrets in a Config live only in the gate process's memory and the response
// body: never logged, never stored.
package gateconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// Path is the endpoint's fixed request path (the auth lane keys on it exactly).
const Path = "/internal/v1/gate/config"

// Config is one session's gate configuration: the environment's request-level
// networking policy (which hosts a request may reach at all), the `host:port`
// endpoints the session's agent declares MCP servers at, and the resolved
// credentials for egress substitution.
//
// The MCP endpoints ride beside the policy rather than folded into its
// AllowedHosts, so the gate is told what an operator configured and what the
// agent declared as two separate facts — which is what lets it treat them
// differently, matching the port on one and holding its dial to the platform's
// address floor. The control plane sends them only for the policy that can use
// them (`limited` with `allow_mcp_servers`), and the gate admits them only
// under the same condition — two ends of one rule, since a widening the agent
// spec controls is worth failing closed twice.
type Config struct {
	Networking         domain.Networking `json:"networking"`
	MCPServerEndpoints []string          `json:"mcp_server_endpoints,omitempty"`
	Credentials        []Credential      `json:"credentials"`
}

// Credential is one resolved environment_variable vault credential as the gate
// needs it: the sandbox-visible Placeholder, the plaintext Secret it stands for,
// the credential's own networking arm (which hosts the secret may be used
// against), and the injection locations it is enabled for. CredentialID is
// non-secret — it names the credential in the substitution span's
// credential_id attribute.
type Credential struct {
	CredentialID      string               `json:"credential_id"`
	Placeholder       string               `json:"placeholder"`
	Secret            string               `json:"secret"`
	Networking        CredentialNetworking `json:"networking"`
	InjectionLocation InjectionLocation    `json:"injection_location"`
}

// CredentialNetworking is a credential's own egress arm: unrestricted (the
// secret may be used against any host) or limited to AllowedHosts. It carries no
// environment-level widening flags — those belong to the environment policy, not
// a credential.
type CredentialNetworking struct {
	Type         domain.NetworkingType `json:"type"`
	AllowedHosts []string              `json:"allowed_hosts,omitempty"`
}

// InjectionLocation is where in an outbound request a credential's secret may be
// substituted for its placeholder.
type InjectionLocation struct {
	Header bool `json:"header"`
	Body   bool `json:"body"`
}

// ErrUnauthorized is returned by Fetch when the endpoint answers 401 — the gate
// token is revoked or its session archived. It is unambiguous (fail-closed: the
// gate must stop serving), distinct from a transient/network error where the
// gate keeps its last-known-good config.
var ErrUnauthorized = errors.New("gateconfig: gate token unauthorized")

// Client fetches gate configuration from the control plane over one session's
// fixed-lifetime gate token.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewClient builds a Client for baseURL (the control plane's scheme://host[:port],
// no trailing path) authenticating with the session's gtk_ token. A nil hc uses
// http.DefaultClient; callers govern timeouts through the Fetch context.
func NewClient(baseURL, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{baseURL: baseURL, token: token, http: hc}
}

// Fetch retrieves the current gate configuration. It returns ErrUnauthorized on
// a 401 (revoked/archived — fail-closed), and a plain error on any other non-200
// or on a transport/decode failure (transient — the caller keeps last-known-good).
// A non-200 body is never echoed into the error, so a proxy error page cannot
// leak into the gate's logs.
func (c *Client) Fetch(ctx context.Context) (*Config, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+Path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, ErrUnauthorized
	default:
		return nil, fmt.Errorf("gateconfig: unexpected status %d", resp.StatusCode)
	}
	var cfg Config
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("gateconfig: decode config: %w", err)
	}
	return &cfg, nil
}
