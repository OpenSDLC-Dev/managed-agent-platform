package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

// mcp_oauth_validate (plan 12 D8): a live probe needing no MCP client —
// attempt the refresh exchange when a refresh block exists, then probe the
// MCP server with a streamable-HTTP initialize under the (possibly refreshed)
// token, and map the outcome per the public docs: invalid = the grant is gone
// (an OAuth/HTTP 4xx), unknown = transient (5xx/429/network). A successful
// refresh persists the rotated tokens (our decision, recorded as INFERRED).

// validationJSON is the BetaManagedAgentsCredentialValidation wire shape.
type validationJSON struct {
	Type            string           `json:"type"`
	CredentialID    string           `json:"credential_id"`
	VaultID         string           `json:"vault_id"`
	ValidatedAt     time.Time        `json:"validated_at"`
	HasRefreshToken bool             `json:"has_refresh_token"`
	Status          string           `json:"status"`
	MCPProbe        mcpProbeJSON     `json:"mcp_probe"`
	Refresh         refreshProbeJSON `json:"refresh"`
}

type mcpProbeJSON struct {
	Method       string            `json:"method"`
	HTTPResponse *httpResponseJSON `json:"http_response"`
}

type refreshProbeJSON struct {
	Status       string            `json:"status"`
	HTTPResponse *httpResponseJSON `json:"http_response"`
}

// httpResponseJSON is the captured probe response: body truncated and
// scrubbed of secret values before it is rendered. Nullable at both use
// sites (the docs render null for no_refresh_token and connect errors).
type httpResponseJSON struct {
	StatusCode    int64  `json:"status_code"`
	ContentType   string `json:"content_type"`
	Body          string `json:"body"`
	BodyTruncated bool   `json:"body_truncated"`
}

const (
	validateBodyMax     = 4096 // capture cap; the docs promise truncation, not a size
	validateCallTimeout = 10 * time.Second
)

// The validate probe dials credential-supplied URLs from the control plane and
// returns their response bodies, so it is a full-response SSRF vector. The
// guard blocks the addresses that are never a legitimate MCP server or OAuth
// endpoint but are prime exfiltration targets — loopback (the control plane's
// own surfaces), link-local (cloud metadata, 169.254.169.254 / fe80::/10),
// the unspecified address, and multicast — checked on the *resolved* IP at
// connect time (net.Dialer.Control), so it holds across DNS rebinding and HTTP
// redirects alike. RFC 1918 private ranges are deliberately allowed: this
// platform's premise is on-prem / in-VPC operation (CLAUDE.md), where MCP
// servers and token endpoints legitimately live on the operator's own private
// network. A blocked target surfaces as a connection failure — connect_error
// on refresh, a null http_response on the probe — never revealing whether the
// internal host exists.
var probeIPAllowed = productionProbeIPAllowed

func productionProbeIPAllowed(ip net.IP) error {
	// An IPv6 transition address forwards to an embedded IPv4 target through a
	// translator on the deployment path; check that real target, not the v6
	// wrapper, so 64:ff9b::7f00:1 cannot smuggle 127.0.0.1 past the guard.
	target := ip
	if v4 := embeddedIPv4(ip); v4 != nil {
		target = v4
	}
	switch {
	case target.IsLoopback(), target.IsLinkLocalUnicast(), target.IsLinkLocalMulticast(),
		target.IsUnspecified(), target.IsMulticast():
		return fmt.Errorf("probe target %s is a disallowed address", ip)
	default:
		return nil
	}
}

// embeddedIPv4 returns the IPv4 address wrapped by an IPv6 transition form —
// NAT64 (the whole 64:ff9b::/32, covering both the 64:ff9b::/96 well-known and
// 64:ff9b:1::/48 local prefixes; v4 in the low 32 bits), 6to4 (2002::/16, v4 in
// bytes 2–5), and Teredo (2001:0::/32, client v4 in the inverted low 32 bits) —
// so the guard re-checks the target a translator would actually reach. The
// NAT64 match is deliberately broad and assumes /96-style low-32 embedding: a
// mis-decode can only add a refusal, never an admit, so it stays fail-safe.
// Returns nil for a plain address.
func embeddedIPv4(ip net.IP) net.IP {
	b := ip.To16()
	if b == nil || ip.To4() != nil {
		return nil
	}
	switch {
	case b[0] == 0x20 && b[1] == 0x02:
		return net.IPv4(b[2], b[3], b[4], b[5]).To4()
	case b[0] == 0x20 && b[1] == 0x01 && b[2] == 0x00 && b[3] == 0x00:
		return net.IPv4(^b[12], ^b[13], ^b[14], ^b[15]).To4()
	case b[0] == 0x00 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b:
		return net.IPv4(b[12], b[13], b[14], b[15]).To4()
	}
	return nil
}

// probeClient is the SSRF-guarded client for both outbound validate calls. The
// Control hook reads probeIPAllowed on every dial (so a test override takes
// effect), and default redirect following is safe because each hop re-dials
// through the same hook.
var probeClient = &http.Client{
	Timeout: validateCallTimeout,
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: validateCallTimeout,
			Control: func(_, address string, _ syscall.RawConn) error {
				host, _, err := net.SplitHostPort(address)
				if err != nil {
					return err
				}
				ip := net.ParseIP(host)
				if ip == nil {
					return fmt.Errorf("probe address %q did not resolve to an IP", address)
				}
				return probeIPAllowed(ip)
			},
		}).DialContext,
	},
}

func (s *server) validateVaultCredential(r *http.Request) (any, error) {
	ctx := r.Context()
	vaultID, credID, err := s.credentialPathIDs(r)
	if err != nil {
		return nil, err
	}
	row := &credentialRow{}
	err = s.pool.QueryRow(ctx,
		`SELECT vault_id, auth_type, auth, secret_ciphertext, secret_key_id, archived_at
		 FROM vault_credentials WHERE id = $1`, credID).
		Scan(&row.vaultID, &row.authType, &row.authDoc, &row.ciphertext, &row.keyID, &row.archivedAt)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && row.vaultID != vaultID) {
		return nil, errNotFound("credential %s not found in vault %s", credID, vaultID)
	}
	if err != nil {
		return nil, err
	}
	if row.authType != authMCPOAuth {
		return nil, errInvalid("mcp_oauth_validate requires an mcp_oauth credential; %s is %s", credID, row.authType)
	}
	if row.archivedAt != nil {
		return nil, errInvalid("credential %s is archived; its secrets were purged", credID)
	}
	if s.cipher == nil {
		return nil, errSecretsUnavailable
	}
	plain, err := s.cipher.Decrypt(ctx, row.ciphertext, deref(row.keyID))
	if err != nil {
		return nil, fmt.Errorf("unseal credential secrets: %w", err)
	}
	secrets := map[string]string{}
	if err := json.Unmarshal(plain, &secrets); err != nil {
		return nil, err
	}
	var doc mcpOAuthAuthJSON
	if err := json.Unmarshal(row.authDoc, &doc); err != nil {
		return nil, err
	}

	out := validationJSON{
		Type: "vault_credential_validation", CredentialID: credID, VaultID: vaultID,
		ValidatedAt: time.Now().UTC(), MCPProbe: mcpProbeJSON{Method: "initialize"},
	}
	scrubber := newScrubber(secrets)

	// Phase 1: the refresh exchange, when the credential can attempt one.
	refreshed := false
	if doc.Refresh == nil || secrets["refresh_token"] == "" {
		out.Refresh.Status = "no_refresh_token"
	} else {
		out.Refresh, refreshed = s.refreshExchange(ctx, &doc, secrets, scrubber)
		if refreshed {
			// Persist the rotated tokens so the next resolution uses them.
			scrubber = newScrubber(secrets)
			sealed, err := json.Marshal(secrets)
			if err != nil {
				return nil, err
			}
			ciphertext, keyID, err := s.cipher.Encrypt(ctx, sealed)
			if err != nil {
				return nil, fmt.Errorf("seal refreshed secrets: %w", err)
			}
			newAuthDoc, err := json.Marshal(doc)
			if err != nil {
				return nil, err
			}
			// Compare-and-set on the ciphertext this validate read: the exchange
			// did seconds of network I/O, and a concurrent credential update or
			// archive may have landed meanwhile. Writing only when the row is
			// still active and unchanged means this best-effort persist never
			// clobbers a newer write or resurrects an archived credential; if it
			// affects no row, the probe result still returns (a point-in-time
			// snapshot, exactly the reference's re-resolution semantics).
			if _, err := s.pool.Exec(ctx,
				`UPDATE vault_credentials SET auth = $2, secret_ciphertext = $3, secret_key_id = $4,
				   updated_at = now()
				 WHERE id = $1 AND archived_at IS NULL AND secret_ciphertext = $5`,
				credID, newAuthDoc, ciphertext, keyID, row.ciphertext); err != nil {
				return nil, err
			}
		}
	}
	out.HasRefreshToken = secrets["refresh_token"] != ""

	// Phase 2: the MCP initialize probe under the (possibly refreshed) token.
	probeResp := s.mcpInitializeProbe(ctx, doc.MCPServerURL, secrets["access_token"], scrubber)
	out.MCPProbe.HTTPResponse = probeResp

	out.Status = validationStatus(out.Refresh, probeResp)
	return out, nil
}

// validationStatus maps the two probe outcomes onto the documented statuses:
// any definitive rejection (an HTTP 4xx from either exchange, except 429) is
// invalid; otherwise any transient signal (5xx, 429, network error) is
// unknown; a clean 2xx probe is valid.
func validationStatus(refresh refreshProbeJSON, probe *httpResponseJSON) string {
	invalid := func(r *httpResponseJSON) bool {
		return r != nil && r.StatusCode >= 400 && r.StatusCode < 500 && r.StatusCode != 429
	}
	if invalid(probe) || (refresh.Status == "failed" && invalid(refresh.HTTPResponse)) {
		return "invalid"
	}
	if probe != nil && probe.StatusCode >= 200 && probe.StatusCode < 300 &&
		(refresh.Status == "succeeded" || refresh.Status == "no_refresh_token") {
		return "valid"
	}
	return "unknown"
}

// refreshExchange performs the OAuth refresh-token grant against the stored
// token endpoint. On success it mutates secrets and doc with the rotated
// tokens and expiry and reports refreshed=true.
func (s *server) refreshExchange(ctx context.Context, doc *mcpOAuthAuthJSON,
	secrets map[string]string, scrub *scrubber) (refreshProbeJSON, bool) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {secrets["refresh_token"]},
	}
	if doc.Refresh.Scope != nil {
		form.Set("scope", *doc.Refresh.Scope)
	}
	if doc.Refresh.Resource != nil {
		form.Set("resource", *doc.Refresh.Resource)
	}
	// The Basic-auth arm carries the secret as base64(escaped client_id ':'
	// escaped client_secret) in a header. That composite is not any single
	// secret value, so register it as its own needle — a token endpoint that
	// reflects the request Authorization header would otherwise leak it.
	basicNeedle := ""
	if doc.Refresh.TokenEndpointAuth.Type == "client_secret_basic" {
		basicNeedle = base64.StdEncoding.EncodeToString(
			[]byte(url.QueryEscape(doc.Refresh.ClientID) + ":" + url.QueryEscape(secrets["client_secret"])))
		scrub.add(basicNeedle)
	}
	switch doc.Refresh.TokenEndpointAuth.Type {
	case "client_secret_basic":
		// Credentials ride the Authorization header, added below.
	case "client_secret_post":
		form.Set("client_id", doc.Refresh.ClientID)
		form.Set("client_secret", secrets["client_secret"])
	default: // "none": a public client sends its client_id in the body
		form.Set("client_id", doc.Refresh.ClientID)
	}
	callCtx, cancel := context.WithTimeout(ctx, validateCallTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, doc.Refresh.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return refreshProbeJSON{Status: "connect_error"}, false
	}
	if doc.Refresh.TokenEndpointAuth.Type == "client_secret_basic" {
		// RFC 6749 §2.3.1: both halves are form-urlencoded before basic auth.
		req.SetBasicAuth(url.QueryEscape(doc.Refresh.ClientID), url.QueryEscape(secrets["client_secret"]))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := probeClient.Do(req)
	if err != nil {
		return refreshProbeJSON{Status: "connect_error"}, false
	}
	defer resp.Body.Close()
	raw := readProbeBody(resp, scrub)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return refreshProbeJSON{Status: "failed", HTTPResponse: captureResponse(resp, raw, scrub)}, false
	}
	var grant struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &grant); err != nil || grant.AccessToken == "" {
		return refreshProbeJSON{Status: "failed", HTTPResponse: captureResponse(resp, raw, scrub)}, false
	}
	secrets["access_token"] = grant.AccessToken
	if grant.RefreshToken != "" {
		secrets["refresh_token"] = grant.RefreshToken
	}
	if grant.ExpiresIn > 0 {
		t := time.Now().UTC().Add(time.Duration(grant.ExpiresIn) * time.Second)
		doc.ExpiresAt = &t
	}
	// The grant body carries the freshly-rotated tokens; capture with a scrubber
	// that includes them, on the full pre-truncation window, so a rotated token
	// straddling the display cap is scrubbed whole rather than as a leaked prefix.
	succScrub := newScrubber(secrets)
	succScrub.add(basicNeedle)
	return refreshProbeJSON{Status: "succeeded", HTTPResponse: captureResponse(resp, raw, succScrub)}, true
}

// mcpInitializeProbe issues a streamable-HTTP MCP initialize request under
// the bearer token; a network failure yields a null http_response.
func (s *server) mcpInitializeProbe(ctx context.Context, serverURL, accessToken string, scrub *scrubber) *httpResponseJSON {
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":` +
		`{"protocolVersion":"2025-06-18","capabilities":{},` +
		`"clientInfo":{"name":"managed-agent-platform","version":"validate-probe"}}}`
	callCtx, cancel := context.WithTimeout(ctx, validateCallTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, serverURL, strings.NewReader(body))
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := probeClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	return captureResponse(resp, readProbeBody(resp, scrub), scrub)
}

// captureResponse renders a probe response for storage. It scrubs the FULL
// read window before truncating to the display cap: truncating first could
// slice a secret at the boundary into a prefix the scrubber's whole-value
// needle no longer matches, leaking it. readProbeBody reads a margin past the
// cap so a secret straddling the boundary is present in full for scrubbing.
func captureResponse(resp *http.Response, raw []byte, scrub *scrubber) *httpResponseJSON {
	truncated := len(raw) > validateBodyMax
	body := scrub.clean(string(raw))
	if len(body) > validateBodyMax {
		body = body[:validateBodyMax]
	}
	return &httpResponseJSON{
		StatusCode: int64(resp.StatusCode),
		// The third-party Content-Type is attacker-controllable, so it is
		// scrubbed like the body — a server could otherwise echo a secret in it.
		ContentType:   scrub.clean(resp.Header.Get("Content-Type")),
		Body:          body,
		BodyTruncated: truncated,
	}
}

// readProbeBody reads the response body up to the display cap plus the longest
// secret, so captureResponse can scrub a boundary-straddling secret whole
// before truncating.
func readProbeBody(resp *http.Response, scrub *scrubber) []byte {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, int64(validateBodyMax+scrub.maxLen()+1)))
	return raw
}

// scrubber blanks secret values out of captured text — the literal value and
// the encodings a secret actually travels in: JSON-string escaping (a token or
// error endpoint most often echoes the secret in a JSON body, where a `"` or
// `\` in the value is backslash-escaped), form/URL escaping (the refresh
// exchange sends the token in an x-www-form-urlencoded body), and base64 (std
// and url).
type scrubber struct {
	needles []string
	maxLen_ int
}

func newScrubber(secrets map[string]string) *scrubber {
	s := &scrubber{}
	for _, v := range secrets {
		if v == "" {
			continue
		}
		// The value as it appears inside a JSON string (quotes/backslashes/
		// control chars escaped), minus the surrounding quotes. Marshaling a
		// string never errors.
		jsonInner := v
		if b, err := json.Marshal(v); err == nil && len(b) >= 2 {
			jsonInner = string(b[1 : len(b)-1])
		}
		for _, n := range []string{
			v,
			jsonInner,
			url.QueryEscape(v),
			url.PathEscape(v),
			base64.StdEncoding.EncodeToString([]byte(v)),
			base64.RawStdEncoding.EncodeToString([]byte(v)),
			base64.URLEncoding.EncodeToString([]byte(v)),
			base64.RawURLEncoding.EncodeToString([]byte(v)),
		} {
			s.needles = append(s.needles, n)
			if len(n) > s.maxLen_ {
				s.maxLen_ = len(n)
			}
		}
	}
	return s
}

// maxLen is the longest needle — the read margin captureResponse needs so a
// boundary-straddling secret is fully present before truncation.
func (s *scrubber) maxLen() int { return s.maxLen_ }

// add registers an extra literal needle — used for a secret-bearing string that
// is not one of the raw secret values, e.g. the base64 Basic-auth credential
// (client_id:client_secret) the refresh exchange sends in a header.
func (s *scrubber) add(needle string) {
	if needle == "" {
		return
	}
	s.needles = append(s.needles, needle)
	if len(needle) > s.maxLen_ {
		s.maxLen_ = len(needle)
	}
}

// sensitiveJSONValue matches the string value of a well-known token-bearing
// JSON key. A refresh success body can carry tokens the credential never stored
// — most notably an OIDC id_token — which value-based needles cannot catch;
// blanking by key name closes that gap for any probe body. The value class is
// `[^"]*` (OAuth/OIDC tokens carry no quotes), so a value truncated at the read
// window (no closing quote) is redacted to its end rather than leaking a prefix.
var sensitiveJSONValue = regexp.MustCompile(
	`("(?:id_token|access_token|refresh_token|client_secret|client_assertion)"\s*:\s*")[^"]*`)

func (s *scrubber) clean(text string) string {
	for _, n := range s.needles {
		text = strings.ReplaceAll(text, n, "[redacted]")
	}
	return sensitiveJSONValue.ReplaceAllString(text, "${1}[redacted]")
}
