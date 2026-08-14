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
	"sort"
	"strings"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/dialguard"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/oauthrefresh"
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
// returns their response bodies, so it is a full-response SSRF vector.
// internal/dialguard holds the address guard and the reasoning behind what it
// refuses; this var is the seam a test overrides to reach an httptest server on
// loopback. A blocked target surfaces as a connection failure — connect_error
// on refresh, a null http_response on the probe — never revealing whether the
// internal host exists.
var probeIPAllowed = dialguard.IPAllowed

// probeClient is the SSRF-guarded client for both outbound validate calls. The
// Control hook reads probeIPAllowed on every dial (so a test override takes
// effect). Redirects are never followed: neither an OAuth token exchange nor an
// MCP initialize legitimately redirects, and following a 307/308 would replay
// the POST body — the refresh_token and a client_secret_post secret — to the
// redirect target, an exfiltration the per-dial IP guard cannot see (it vets
// the hop's address, not that a hop should happen at all). ErrUseLastResponse
// pins the 3xx as the final response, so it is captured as a failure instead.
var probeClient = &http.Client{
	Timeout: validateCallTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: validateCallTimeout,
			Control: dialguard.Control(func(ip net.IP) error { return probeIPAllowed(ip) }),
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
				return nil, resealFailed(err)
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
//
// The grant's wire shape lives in internal/oauthrefresh, shared with the
// executor's dial-time refresh; everything here is the probe's own half — the
// guarded client, the scrubbed capture, and the three documented statuses.
func (s *server) refreshExchange(ctx context.Context, doc *mcpOAuthAuthJSON,
	secrets map[string]string, scrub *scrubber) (refreshProbeJSON, bool) {
	params := mcpRefreshParams(doc, secrets)
	// The Basic arm's header composite is not any single secret value, so it
	// needs a needle of its own; see [oauthrefresh.Params.BasicNeedle].
	basicNeedle := params.BasicNeedle()
	scrub.add(basicNeedle)

	callCtx, cancel := context.WithTimeout(ctx, validateCallTimeout)
	defer cancel()
	req, err := params.NewRequest(callCtx)
	if err != nil {
		return refreshProbeJSON{Status: "connect_error"}, false
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		return refreshProbeJSON{Status: "connect_error"}, false
	}
	defer resp.Body.Close()
	raw := readProbeBody(resp, scrub)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return refreshProbeJSON{Status: "failed", HTTPResponse: captureResponse(resp, raw, scrub)}, false
	}
	grant, ok := oauthrefresh.ParseGrant(raw)
	if !ok {
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

// mcpRefreshParams reads a credential's refresh configuration out of its stored
// auth document and its sealed secrets. Only reached with doc.Refresh non-nil.
func mcpRefreshParams(doc *mcpOAuthAuthJSON, secrets map[string]string) oauthrefresh.Params {
	return oauthrefresh.Params{
		ClientID:          doc.Refresh.ClientID,
		TokenEndpoint:     doc.Refresh.TokenEndpoint,
		TokenEndpointAuth: doc.Refresh.TokenEndpointAuth.Type,
		Resource:          doc.Refresh.Resource,
		Scope:             doc.Refresh.Scope,
		RefreshToken:      secrets["refresh_token"],
		ClientSecret:      secrets["client_secret"],
	}
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
	needles  []string
	maxLen_  int
	prepared bool
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

// prepare sorts the needles by descending length and de-dupes them, so that
// when one secret is a substring of another (an access token that prefixes a
// refresh token, say) the longer value is redacted first — replacing the
// shorter one first would leave the longer secret's suffix exposed.
func (s *scrubber) prepare() {
	if s.prepared {
		return
	}
	sort.Slice(s.needles, func(i, j int) bool { return len(s.needles[i]) > len(s.needles[j]) })
	out := s.needles[:0]
	seen := map[string]bool{}
	for _, n := range s.needles {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	s.needles = out
	s.prepared = true
}

func (s *scrubber) clean(text string) string {
	s.prepare()
	for _, n := range s.needles {
		text = strings.ReplaceAll(text, n, "[redacted]")
	}
	return sensitiveJSONValue.ReplaceAllString(text, "${1}[redacted]")
}
