package api_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
)

// mcp_oauth_validate (plan 12 D8): a live probe against real (test) OAuth and
// MCP endpoints — refresh exchange first, then a streamable-HTTP initialize
// under the (possibly refreshed) token.

// oauthMCPFixture runs a token endpoint and an MCP endpoint whose behavior
// each test scripts.
type oauthMCPFixture struct {
	token *httptest.Server
	mcp   *httptest.Server

	// scripted behavior
	refreshStatus int    // 0 = 200 with a fresh grant
	grantJSON     string // overrides the default grant body when set
	mcpStatus     int    // 0 = 200

	// observed requests
	lastRefreshToken string
	lastAuthz        string
	lastBearer       string
}

func newOAuthMCPFixture(t *testing.T) *oauthMCPFixture {
	// The httptest servers below listen on loopback; permit it for the probe
	// (link-local and friends stay blocked — see TestValidateSSRFGuard).
	t.Cleanup(api.AllowLoopbackProbeForTest())
	f := &oauthMCPFixture{}
	f.token = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.lastRefreshToken = r.PostFormValue("refresh_token")
		f.lastAuthz = r.Header.Get("Authorization")
		if f.refreshStatus != 0 {
			w.WriteHeader(f.refreshStatus)
			fmt.Fprintf(w, `{"error":"invalid_grant","echo":%q}`, f.lastRefreshToken)
			return
		}
		body := f.grantJSON
		if body == "" {
			body = `{"access_token":"at-new","refresh_token":"rt-new","expires_in":3600,"token_type":"Bearer"}`
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(f.token.Close)
	f.mcp = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.lastBearer = r.Header.Get("Authorization")
		if f.mcpStatus != 0 {
			w.WriteHeader(f.mcpStatus)
			fmt.Fprint(w, `{"error":"denied"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}`)
	}))
	t.Cleanup(f.mcp.Close)
	return f
}

func (f *oauthMCPFixture) createCredential(t *testing.T, s *tserver, vaultID string, withRefresh bool) string {
	t.Helper()
	auth := map[string]any{
		"type": "mcp_oauth", "mcp_server_url": f.mcp.URL, "access_token": "at-original",
	}
	if withRefresh {
		auth["refresh"] = map[string]any{
			"client_id": "client-1", "refresh_token": "rt-original",
			"token_endpoint":      f.token.URL,
			"token_endpoint_auth": map[string]any{"type": "client_secret_basic", "client_secret": "cs-secret"},
		}
	}
	return createCredential(t, s, vaultID, auth)["id"].(string)
}

func validatePath(vaultID, credID string) string {
	return "/v1/vaults/" + vaultID + "/credentials/" + credID + "/mcp_oauth_validate"
}

func TestValidateRefreshAndProbeSucceed(t *testing.T) {
	s := newTestServer(t)
	f := newOAuthMCPFixture(t)
	vaultID := createVault(t, s, "v")
	credID := f.createCredential(t, s, vaultID, true)

	status, body := s.do("POST", validatePath(vaultID, credID), nil)
	if status != http.StatusOK {
		t.Fatalf("validate: status %d (%v)", status, body)
	}
	if body["type"] != "vault_credential_validation" || body["status"] != "valid" {
		t.Fatalf("verdict: %v", body)
	}
	if body["credential_id"] != credID || body["vault_id"] != vaultID {
		t.Fatalf("ids: %v", body)
	}
	if body["has_refresh_token"] != true {
		t.Fatalf("has_refresh_token: %v", body)
	}
	refresh := body["refresh"].(map[string]any)
	if refresh["status"] != "succeeded" || refresh["http_response"] == nil {
		t.Fatalf("refresh: %v", refresh)
	}
	probe := body["mcp_probe"].(map[string]any)
	if probe["method"] != "initialize" {
		t.Fatalf("probe method: %v", probe)
	}
	if int(probe["http_response"].(map[string]any)["status_code"].(float64)) != 200 {
		t.Fatalf("probe response: %v", probe)
	}
	// The refresh used basic auth with form-urlencoded halves; the probe ran
	// under the freshly-rotated token.
	wantAuthz := "Basic " + base64.StdEncoding.EncodeToString([]byte("client-1:cs-secret"))
	if f.lastAuthz != wantAuthz {
		t.Fatalf("token-endpoint Authorization %q, want %q", f.lastAuthz, wantAuthz)
	}
	if f.lastBearer != "Bearer at-new" {
		t.Fatalf("probe ran under %q, want the rotated token", f.lastBearer)
	}
	// A successful refresh persists the rotated tokens: the next validate
	// presents the new refresh_token, and expires_at lands on the resource.
	s.do("POST", validatePath(vaultID, credID), nil)
	if f.lastRefreshToken != "rt-new" {
		t.Fatalf("second validate sent %q, want the persisted rt-new", f.lastRefreshToken)
	}
	_, cred := s.do("GET", "/v1/vaults/"+vaultID+"/credentials/"+credID, nil)
	if cred["auth"].(map[string]any)["expires_at"] == nil {
		t.Fatal("expires_at not persisted from expires_in")
	}
	// No secret value reaches the validation body or the stored row.
	rendered := fmt.Sprint(body)
	for _, secret := range []string{"at-original", "at-new", "rt-original", "rt-new", "cs-secret"} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("validation echoed secret %q: %s", secret, rendered)
		}
	}
}

// A refresh success body may carry tokens the credential never stored — an
// OIDC id_token most notably — which value-based scrubbing cannot catch. The
// key-based redaction blanks them from the captured body.
func TestValidateScrubsIDTokenByKey(t *testing.T) {
	s := newTestServer(t)
	f := newOAuthMCPFixture(t)
	const idTok = "eyJhbGciOiJSUzI1NiJ9.payload-with-pii.signature"
	f.grantJSON = fmt.Sprintf(
		`{"access_token":"at-new","refresh_token":"rt-new","id_token":%q,"token_type":"Bearer"}`, idTok)
	vaultID := createVault(t, s, "v")
	credID := f.createCredential(t, s, vaultID, true)

	_, body := s.do("POST", validatePath(vaultID, credID), nil)
	refreshBody := body["refresh"].(map[string]any)["http_response"].(map[string]any)["body"].(string)
	if strings.Contains(refreshBody, idTok) {
		t.Fatalf("id_token leaked into the refresh body: %s", refreshBody)
	}
	if !strings.Contains(refreshBody, `"id_token":"[redacted]"`) {
		t.Fatalf("id_token not redacted by key: %s", refreshBody)
	}
}

// A secret echoed JSON-escaped (a `"`/`\` in the value), or one placed in the
// third-party Content-Type header, must still be scrubbed.
func TestValidateScrubsJSONEscapedAndContentType(t *testing.T) {
	t.Cleanup(api.AllowLoopbackProbeForTest())
	const refreshTok = `rt"with\quote`
	const accessTok = "ACCESS-header-secret-9x"

	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.WriteHeader(http.StatusBadRequest)
		// json.Marshal escapes the `"`/`\` — the form value-based needle for the
		// raw secret would not match this escaped spelling.
		echo, _ := json.Marshal(map[string]string{"error": "invalid_grant", "echo": r.PostFormValue("refresh_token")})
		w.Write(echo)
	}))
	defer token.Close()
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/"+accessTok) // secret smuggled into a header
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer mcp.Close()

	s := newTestServer(t)
	vaultID := createVault(t, s, "scrub2")
	credID := createCredential(t, s, vaultID, map[string]any{
		"type": "mcp_oauth", "mcp_server_url": mcp.URL, "access_token": accessTok,
		"refresh": map[string]any{"client_id": "c", "refresh_token": refreshTok,
			"token_endpoint": token.URL, "token_endpoint_auth": map[string]any{"type": "none"}},
	})["id"].(string)

	_, body := s.do("POST", validatePath(vaultID, credID), nil)
	rendered := fmt.Sprint(body)
	escaped := string(mustJSON(t, refreshTok))
	escaped = escaped[1 : len(escaped)-1] // strip the surrounding quotes
	for _, needle := range []string{refreshTok, escaped, accessTok} {
		if strings.Contains(rendered, needle) {
			t.Fatalf("captured output leaked %q: %s", needle, rendered)
		}
	}
	// The MCP probe's captured Content-Type must be redacted, not verbatim.
	ct := body["mcp_probe"].(map[string]any)["http_response"].(map[string]any)["content_type"].(string)
	if strings.Contains(ct, accessTok) {
		t.Fatalf("content_type leaked the access token: %q", ct)
	}
}

// A token endpoint that reflects the request Authorization header must not leak
// the Basic-auth client_secret — the header carries base64(client_id:secret),
// which the secret's own value-needles do not cover when the base64 alignment
// breaks (client_id "client-12" -> a 10-byte "client-12:" prefix, mod 3 != 0).
func TestValidateScrubsReflectedBasicAuth(t *testing.T) {
	t.Cleanup(api.AllowLoopbackProbeForTest())
	const clientID = "client-12"
	const clientSecret = "cs-secret-value"

	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"error":"invalid_client","seen_authorization":%q}`, r.Header.Get("Authorization"))
	}))
	defer token.Close()
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer mcp.Close()

	s := newTestServer(t)
	vaultID := createVault(t, s, "basic")
	credID := createCredential(t, s, vaultID, map[string]any{
		"type": "mcp_oauth", "mcp_server_url": mcp.URL, "access_token": "at",
		"refresh": map[string]any{"client_id": clientID, "refresh_token": "rt",
			"token_endpoint":      token.URL,
			"token_endpoint_auth": map[string]any{"type": "client_secret_basic", "client_secret": clientSecret}},
	})["id"].(string)

	_, body := s.do("POST", validatePath(vaultID, credID), nil)
	rendered := fmt.Sprint(body)
	composite := base64.StdEncoding.EncodeToString([]byte(clientID + ":" + clientSecret))
	for _, needle := range []string{clientSecret, composite} {
		if strings.Contains(rendered, needle) {
			t.Fatalf("reflected Basic auth leaked %q: %s", needle, rendered)
		}
	}
}

// When one secret is a prefix of another, the scrubber must redact the longer
// value first — redacting the shorter one first would leave the longer secret's
// suffix exposed. The needles are added shortest-first to prove the ordering is
// by length, not insertion.
func TestScrubberRedactsLongestFirst(t *testing.T) {
	got := api.ScrubberCleanForTest([]string{"abc", "abcXYZsecret"}, "value=abcXYZsecret")
	if strings.Contains(got, "XYZsecret") || !strings.Contains(got, "[redacted]") {
		t.Fatalf("overlapping needle left a secret suffix exposed: %q", got)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestValidateProbeRejected(t *testing.T) {
	s := newTestServer(t)
	f := newOAuthMCPFixture(t)
	f.mcpStatus = http.StatusUnauthorized
	vaultID := createVault(t, s, "v")
	credID := f.createCredential(t, s, vaultID, true)

	_, body := s.do("POST", validatePath(vaultID, credID), nil)
	if body["status"] != "invalid" {
		t.Fatalf("a 401 probe must map to invalid: %v", body)
	}
	probe := body["mcp_probe"].(map[string]any)["http_response"].(map[string]any)
	if int(probe["status_code"].(float64)) != 401 {
		t.Fatalf("probe response: %v", probe)
	}
}

func TestValidateRefreshRejected(t *testing.T) {
	s := newTestServer(t)
	f := newOAuthMCPFixture(t)
	f.refreshStatus = http.StatusBadRequest // invalid_grant — the grant is gone
	vaultID := createVault(t, s, "v")
	credID := f.createCredential(t, s, vaultID, true)

	_, body := s.do("POST", validatePath(vaultID, credID), nil)
	if body["status"] != "invalid" {
		t.Fatalf("an OAuth 4xx must map to invalid: %v", body)
	}
	refresh := body["refresh"].(map[string]any)
	if refresh["status"] != "failed" {
		t.Fatalf("refresh: %v", refresh)
	}
	// The token endpoint echoed the refresh token back; the captured body
	// must carry the scrubbed form, never the value.
	captured := refresh["http_response"].(map[string]any)["body"].(string)
	if strings.Contains(captured, "rt-original") {
		t.Fatalf("captured body leaked the refresh token: %s", captured)
	}
	if !strings.Contains(captured, "[redacted]") {
		t.Fatalf("expected a scrub marker in the captured body: %s", captured)
	}
}

func TestValidateTransientAndNoRefresh(t *testing.T) {
	s := newTestServer(t)
	f := newOAuthMCPFixture(t)
	vaultID := createVault(t, s, "v")

	// A 5xx MCP answer is transient → unknown.
	f.mcpStatus = http.StatusBadGateway
	credID := f.createCredential(t, s, vaultID, false)
	_, body := s.do("POST", validatePath(vaultID, credID), nil)
	if body["status"] != "unknown" {
		t.Fatalf("a 502 probe must map to unknown: %v", body)
	}
	refresh := body["refresh"].(map[string]any)
	if refresh["status"] != "no_refresh_token" || refresh["http_response"] != nil {
		t.Fatalf("refresh without a block: %v", refresh)
	}
	if body["has_refresh_token"] != false {
		t.Fatalf("has_refresh_token: %v", body)
	}

	// An unreachable MCP server is a null http_response → unknown.
	unreachable := createCredential(t, s, vaultID, map[string]any{
		"type": "mcp_oauth", "mcp_server_url": "http://127.0.0.1:1", "access_token": "at",
	})["id"].(string)
	_, body = s.do("POST", validatePath(vaultID, unreachable), nil)
	if body["status"] != "unknown" {
		t.Fatalf("a connect failure must map to unknown: %v", body)
	}
	if body["mcp_probe"].(map[string]any)["http_response"] != nil {
		t.Fatalf("probe response must render null on a connect failure: %v", body)
	}
}

func TestValidatePreconditions(t *testing.T) {
	s := newTestServer(t)
	vaultID := createVault(t, s, "v")

	envID := createCredential(t, s, vaultID, envVarAuth("K"))["id"].(string)
	if status, _ := s.do("POST", validatePath(vaultID, envID), nil); status != http.StatusBadRequest {
		t.Fatal("validate on a non-mcp_oauth credential must 400")
	}

	f := newOAuthMCPFixture(t)
	credID := f.createCredential(t, s, vaultID, false)
	s.do("POST", "/v1/vaults/"+vaultID+"/credentials/"+credID+"/archive", nil)
	if status, _ := s.do("POST", validatePath(vaultID, credID), nil); status != http.StatusBadRequest {
		t.Fatal("validate on an archived credential must 400")
	}

	other := createVault(t, s, "other")
	live := f.createCredential(t, s, vaultID, false)
	if status, _ := s.do("POST", validatePath(other, live), nil); status != http.StatusNotFound {
		t.Fatal("validate under the wrong vault must 404")
	}
}

// The SSRF guard refuses the exfiltration-target address classes and admits
// ordinary/private ones (the on-prem case), and a credential URL with a bad
// scheme is rejected at create time.
func TestValidateSSRFGuard(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "::1", "169.254.169.254", "fe80::1", "0.0.0.0", "224.0.0.1",
		// IPv6 transition forms that a translator would rewrite to a blocked v4:
		"64:ff9b::7f00:1",    // NAT64 -> 127.0.0.1
		"64:ff9b::a9fe:a9fe", // NAT64 -> 169.254.169.254 (metadata)
		"64:ff9b:1::7f00:1",  // NAT64 local prefix -> 127.0.0.1
		"2002:7f00:1::",      // 6to4 -> 127.0.0.1
		"2002:a9fe:a9fe::",   // 6to4 -> 169.254.169.254
		"2001::5601:5601"} {  // Teredo -> ^(5601:5601) = 169.254.169.254
		if api.ProbeIPAllowedForTest(net.ParseIP(ip)) == nil {
			t.Errorf("probe must refuse %s", ip)
		}
	}
	for _, ip := range []string{"93.184.216.34", "10.1.2.3", "192.168.5.5", "172.16.9.9",
		"64:ff9b::5db8:d822"} { // NAT64 -> 93.184.216.34, a public target, still allowed
		if err := api.ProbeIPAllowedForTest(net.ParseIP(ip)); err != nil {
			t.Errorf("probe must admit %s (on-prem/public): %v", ip, err)
		}
	}

	s := newTestServer(t)
	vaultID := createVault(t, s, "ssrf")
	for _, bad := range []string{"ftp://x.example.com", "file:///etc/passwd", "not-a-url", "http://"} {
		status, _ := s.do("POST", "/v1/vaults/"+vaultID+"/credentials", map[string]any{
			"auth": map[string]any{"type": "static_bearer", "mcp_server_url": bad, "token": "t"}})
		if status != http.StatusBadRequest {
			t.Errorf("mcp_server_url %q must be rejected at create, got %d", bad, status)
		}
	}

	// A live probe at a link-local target is refused at dial → unknown, never
	// revealing the host. (The fixture's loopback relaxation does not cover
	// link-local.)
	f := newOAuthMCPFixture(t)
	blocked := createCredential(t, s, vaultID, map[string]any{
		"type": "mcp_oauth", "mcp_server_url": "http://169.254.169.254/latest/meta-data/", "access_token": "at"})["id"].(string)
	_, body := s.do("POST", validatePath(vaultID, blocked), nil)
	if body["status"] != "unknown" || body["mcp_probe"].(map[string]any)["http_response"] != nil {
		t.Fatalf("a link-local probe must be refused (unknown, null response): %v", body)
	}
	_ = f
}

// A secret echoed in an encoded form, or one straddling the truncation
// boundary, must still be scrubbed from the captured body.
func TestValidateScrubsEncodedAndBoundarySecrets(t *testing.T) {
	t.Cleanup(api.AllowLoopbackProbeForTest())
	const refreshTok = "tok+en/with=special&chars-abc123"
	const accessTok = "ACCESS-TOKEN-0123456789-boundary-secret!"

	// Token endpoint echoes the raw (url-encoded) request form in a 400 body.
	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"error":"invalid_grant","echo":%q}`, string(raw))
	}))
	defer token.Close()
	// MCP endpoint returns the access token straddling byte 4096.
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pad := strings.Repeat("x", 4096-10)
		fmt.Fprint(w, pad+accessTok+strings.Repeat("y", 200))
	}))
	defer mcp.Close()

	s := newTestServer(t)
	vaultID := createVault(t, s, "scrub")
	credID := createCredential(t, s, vaultID, map[string]any{
		"type": "mcp_oauth", "mcp_server_url": mcp.URL, "access_token": accessTok,
		"refresh": map[string]any{"client_id": "c", "refresh_token": refreshTok,
			"token_endpoint": token.URL, "token_endpoint_auth": map[string]any{"type": "none"}},
	})["id"].(string)

	_, body := s.do("POST", validatePath(vaultID, credID), nil)
	rendered := fmt.Sprint(body)
	// Neither the raw secrets, nor the url-encoded refresh token, may appear.
	for _, needle := range []string{refreshTok, url.QueryEscape(refreshTok), accessTok} {
		if strings.Contains(rendered, needle) {
			t.Fatalf("captured body leaked %q: %s", needle, rendered)
		}
	}
	probe := body["mcp_probe"].(map[string]any)["http_response"].(map[string]any)
	if probe["body_truncated"] != true {
		t.Fatalf("expected the oversized MCP body to be truncated: %v", probe)
	}
	// The teeth of "scrub before truncate": the pad is 4096-10 bytes, so the
	// token's first 10 bytes are the part that survives a naive truncate-first.
	// Asserting the FULL token is absent proves nothing (truncation drops it
	// regardless) — assert the boundary-surviving PREFIX is gone, and that the
	// redaction marker (exactly 10 chars) lands where the token was cut.
	mcpBody := probe["body"].(string)
	if strings.Contains(mcpBody, accessTok[:10]) {
		t.Fatalf("boundary-straddling secret prefix leaked: ...%q", mcpBody[max(0, len(mcpBody)-24):])
	}
	if !strings.HasSuffix(mcpBody, "[redacted]") {
		t.Fatalf("expected the redaction marker at the truncated boundary, got ...%q", mcpBody[max(0, len(mcpBody)-24):])
	}
}

// The wire renders every field of the captured http_response.
func TestValidateHTTPResponseShape(t *testing.T) {
	s := newTestServer(t)
	f := newOAuthMCPFixture(t)
	vaultID := createVault(t, s, "v")
	credID := f.createCredential(t, s, vaultID, false)

	_, body := s.do("POST", validatePath(vaultID, credID), nil)
	raw, _ := json.Marshal(body["mcp_probe"].(map[string]any)["http_response"])
	var resp struct {
		StatusCode    *int64  `json:"status_code"`
		ContentType   *string `json:"content_type"`
		Body          *string `json:"body"`
		BodyTruncated *bool   `json:"body_truncated"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.StatusCode == nil || resp.ContentType == nil || resp.Body == nil || resp.BodyTruncated == nil {
		t.Fatalf("http_response must carry all four fields: %s", raw)
	}
	if *resp.ContentType != "application/json" || *resp.BodyTruncated {
		t.Fatalf("http_response: %s", raw)
	}
}
