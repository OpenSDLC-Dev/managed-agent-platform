package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	neturl "net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/mcp/mcptest"
)

// attachVaultWithMCPCredential attaches a vault to the session carrying one
// static_bearer credential for serverURL, sealed through the harness's own
// cipher — the shape the API writes, so the executor's read path is exercised
// end to end rather than against a hand-placed plaintext.
func (h *harness) attachVaultWithMCPCredential(t *testing.T, serverURL, token string) {
	t.Helper()
	ctx := context.Background()
	vaultID := domain.NewID("vlt").String()
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO vaults (id, display_name) VALUES ($1, 'test vault')`, vaultID); err != nil {
		t.Fatalf("insert vault: %v", err)
	}
	sealed, err := json.Marshal(map[string]string{"token": token})
	if err != nil {
		t.Fatal(err)
	}
	ct, keyID, err := h.cipher.Encrypt(ctx, sealed)
	if err != nil {
		t.Fatalf("seal token: %v", err)
	}
	auth, err := json.Marshal(map[string]string{
		"type": "static_bearer", "mcp_server_url": serverURL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO vault_credentials (id, vault_id, auth_type, auth, secret_ciphertext, secret_key_id, cred_key)
		 VALUES ($1, $2, 'static_bearer', $3::jsonb, $4, $5, $6)`,
		domain.NewID("vcrd").String(), vaultID, auth, ct, keyID, "url:"+serverURL); err != nil {
		t.Fatalf("insert mcp credential: %v", err)
	}
	if _, err := h.pool.Exec(ctx,
		`UPDATE sessions SET vault_ids = $2 WHERE id = $1`,
		h.sid.String(), []string{vaultID}); err != nil {
		t.Fatalf("attach vault: %v", err)
	}
}

// serveRequiringBearer is an MCP server behind a gateway that answers 401 to any
// request not carrying the expected token — the shape every authenticating MCP
// endpoint has, and the one that proves the header actually left this process.
func serveRequiringBearer(t *testing.T, want string, tool mcptest.Tool) (url string, seen func() string) {
	t.Helper()
	inner := mcptest.Server(t, tool)
	target, err := neturl.Parse(inner)
	if err != nil {
		t.Fatal(err)
	}
	var got atomic.Pointer[string]
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		got.Store(&auth)
		if auth != "Bearer "+want {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts.URL, func() string {
		if p := got.Load(); p != nil {
			return *p
		}
		return ""
	}
}

// The whole point of the slice: a session whose vault registers a token for the
// server it is about to call dials with that token, and the call succeeds where
// an unauthenticated one would not.
func TestMCPCallCarriesTheVaultsBearerToken(t *testing.T) {
	h := mcpHarness(t)
	url, seen := serveRequiringBearer(t, "lin_api_secret",
		mcptest.Tool{Name: "ask", Blocks: []mcptest.Block{{Type: "text", Text: "answered"}}})
	h.attachVaultWithMCPCredential(t, url, "lin_api_secret")
	h.declareListedMCPServers(t, [2]string{"docs", url})
	useID := h.appendMCPToolUse(t, "docs", "ask", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	if got := seen(); got != "Bearer lin_api_secret" {
		t.Fatalf("the server saw Authorization %q, want the vault's token", got)
	}
	results := h.mcpResults(t)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	blocks := blocksOf(t, results[0])
	if txt, _ := blocks[0]["text"].(string); len(blocks) != 1 || !strings.Contains(txt, "answered") {
		t.Errorf("result = %v, want the tool's answer", blocks)
	}
	if errs := h.sessionErrors(t); len(errs) != 0 {
		t.Errorf("an authenticated call emitted %d session errors, want 0", len(errs))
	}
	_ = useID
}

// A server that requires a credential the session's vaults do not register is an
// authentication failure, not a connection that could not be made — the
// reference names "required authentication when no matching credential was
// configured" as one of the three the auth error covers.
func TestMCPCallWithNoMatchingCredentialReportsAuthenticationFailure(t *testing.T) {
	h := mcpHarness(t)
	url, _ := serveRequiringBearer(t, "the-token-nobody-registered",
		mcptest.Tool{Name: "ask", Blocks: []mcptest.Block{{Type: "text", Text: "unreachable"}}})
	h.declareListedMCPServers(t, [2]string{"docs", url})
	h.appendMCPToolUse(t, "docs", "ask", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	if got := mcpErrorType(t, h); got != "mcp_authentication_failed_error" {
		t.Errorf("session error type = %q, want mcp_authentication_failed_error", got)
	}
	// The turn still continues: the model gets an is_error result either way.
	if results := h.mcpResults(t); len(results) != 1 {
		t.Fatalf("got %d results, want the call answered so the turn can carry on", len(results))
	}
}

// A wrong token is the other of those three, and reads the same way to this
// platform — the server answers 401 whether one was sent or not.
func TestMCPCallWithARejectedCredentialReportsAuthenticationFailure(t *testing.T) {
	h := mcpHarness(t)
	url, _ := serveRequiringBearer(t, "the-right-token",
		mcptest.Tool{Name: "ask", Blocks: []mcptest.Block{{Type: "text", Text: "unreachable"}}})
	h.attachVaultWithMCPCredential(t, url, "the-wrong-token")
	h.declareListedMCPServers(t, [2]string{"docs", url})
	h.appendMCPToolUse(t, "docs", "ask", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	if got := mcpErrorType(t, h); got != "mcp_authentication_failed_error" {
		t.Errorf("session error type = %q, want mcp_authentication_failed_error", got)
	}
}

// A server that is simply not there is still a connection failure: the split is
// by cause, and this one never got far enough to be refused.
func TestMCPCallToAnUnreachableServerStaysAConnectionFailure(t *testing.T) {
	h := mcpHarness(t)
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer dead.Close()
	h.declareListedMCPServers(t, [2]string{"docs", dead.URL})
	h.appendMCPToolUse(t, "docs", "ask", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	if got := mcpErrorType(t, h); got != "mcp_connection_failed_error" {
		t.Errorf("session error type = %q, want mcp_connection_failed_error", got)
	}
}

// Discovery dials with the same credential: a listing is as authenticated as a
// call, and a server that only publishes its tools to a known client would
// otherwise never yield a catalog row.
func TestMCPDiscoveryCarriesTheVaultsBearerToken(t *testing.T) {
	h := mcpHarness(t)
	url, seen := serveRequiringBearer(t, "discovery-token",
		mcptest.Tool{Name: "ask", Blocks: []mcptest.Block{{Type: "text", Text: "ok"}}})
	h.attachVaultWithMCPCredential(t, url, "discovery-token")
	h.declareMCPServers(t, [2]string{"docs", url})
	h.enqueueMCP(t)

	h.stepOnce(t)

	if got := seen(); got != "Bearer discovery-token" {
		t.Fatalf("the server saw Authorization %q during discovery, want the vault's token", got)
	}
	if row := h.catalog(t)["docs"]; row.status != "ready" {
		t.Errorf("catalog row = %q (%s), want ready", row.status, row.reason)
	}
}

// And a listing the server refuses says so, so an operator reading the row is
// pointed at the credential rather than at the network.
func TestMCPDiscoveryNamesAnAuthenticationFailureInTheRow(t *testing.T) {
	h := mcpHarness(t)
	url, _ := serveRequiringBearer(t, "the-right-token",
		mcptest.Tool{Name: "ask", Blocks: []mcptest.Block{{Type: "text", Text: "ok"}}})
	h.attachVaultWithMCPCredential(t, url, "the-wrong-token")
	h.declareMCPServers(t, [2]string{"docs", url})
	h.enqueueMCP(t)

	h.stepOnce(t)

	row := h.catalog(t)["docs"]
	if row.status != "failed" {
		t.Fatalf("catalog row = %q, want failed", row.status)
	}
	if !strings.HasPrefix(row.reason, "authentication failed:") {
		t.Errorf("row reason = %q, want it to name an authentication failure", row.reason)
	}
	if strings.Contains(row.reason, "the-wrong-token") {
		t.Errorf("row reason quotes the credential: %q", row.reason)
	}
}

// sessionErrors reads the session's session.error events in log order.
func (h *harness) sessionErrors(t *testing.T) []domain.Event {
	t.Helper()
	evs, err := h.log.List(context.Background(), h.sid, events.ListQuery{
		Types: []string{string(domain.EventSessionError)}})
	if err != nil {
		t.Fatalf("list session errors: %v", err)
	}
	return evs
}

// mcpErrorType reads the single session.error's error type — which of the wire's
// two MCP failures the pass decided this was.
func mcpErrorType(t *testing.T, h *harness) string {
	t.Helper()
	errs := h.sessionErrors(t)
	if len(errs) != 1 {
		t.Fatalf("got %d session errors, want 1", len(errs))
	}
	var body struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(errs[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	return body.Error.Type
}

// serveRefusingTheCall lets the handshake through and answers 401 to the tool
// call itself — a token that expires between connecting and calling, or a server
// that authenticates the work rather than the connection. It is the only shape
// that reaches the call-time arm of the split; a gateway refusing everything
// fails at the handshake and never gets there.
func serveRefusingTheCall(t *testing.T, tool mcptest.Tool) string {
	t.Helper()
	inner := mcptest.Server(t, tool)
	target, err := neturl.Parse(inner)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		var msg struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &msg)
		if msg.Method == "tools/call" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

// A refusal that arrives on the call rather than the handshake is the same
// authentication failure. It must also not be read as ErrServerAnswered, which a
// refused call can carry and which would report nothing to the operator at all.
func TestMCPCallRefusedAtCallTimeReportsAuthenticationFailure(t *testing.T) {
	h := mcpHarness(t)
	url := serveRefusingTheCall(t,
		mcptest.Tool{Name: "ask", Blocks: []mcptest.Block{{Type: "text", Text: "unreachable"}}})
	h.declareListedMCPServers(t, [2]string{"docs", url})
	h.appendMCPToolUse(t, "docs", "ask", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	if got := mcpErrorType(t, h); got != "mcp_authentication_failed_error" {
		t.Errorf("session error type = %q, want mcp_authentication_failed_error", got)
	}
	if results := h.mcpResults(t); len(results) != 1 {
		t.Fatalf("got %d results, want the call answered so the turn can carry on", len(results))
	}
}

// A credential that matches and cannot be opened never reaches the server, and
// must not quietly become an anonymous dial: the operator configured a
// credential, and a server that then accepts an unauthenticated request would
// hide the failure entirely.
func TestMCPCallWithAnUnopenableCredentialDoesNotDialAnonymously(t *testing.T) {
	h := mcpHarness(t)
	var reached atomic.Bool
	inner := mcptest.Server(t,
		mcptest.Tool{Name: "ask", Blocks: []mcptest.Block{{Type: "text", Text: "served anonymously"}}})
	target, err := neturl.Parse(inner)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Store(true)
		proxy.ServeHTTP(w, r)
	}))
	defer ts.Close()

	// A credential whose sealed bytes this cipher cannot open — the shape a
	// rotated or misconfigured key leaves behind.
	ctx := context.Background()
	vaultID := domain.NewID("vlt").String()
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO vaults (id, display_name) VALUES ($1, 'test vault')`, vaultID); err != nil {
		t.Fatalf("insert vault: %v", err)
	}
	auth, err := json.Marshal(map[string]string{"type": "static_bearer", "mcp_server_url": ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO vault_credentials (id, vault_id, auth_type, auth, secret_ciphertext, secret_key_id, cred_key)
		 VALUES ($1, $2, 'static_bearer', $3::jsonb, $4, $5, $6)`,
		domain.NewID("vcrd").String(), vaultID, auth,
		[]byte("not a ciphertext this key produced"), "test-1", "url:"+ts.URL); err != nil {
		t.Fatalf("insert mcp credential: %v", err)
	}
	if _, err := h.pool.Exec(ctx,
		`UPDATE sessions SET vault_ids = $2 WHERE id = $1`, h.sid.String(), []string{vaultID}); err != nil {
		t.Fatalf("attach vault: %v", err)
	}

	h.declareListedMCPServers(t, [2]string{"docs", ts.URL})
	h.appendMCPToolUse(t, "docs", "ask", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	if reached.Load() {
		t.Error("the dial went out anyway, unauthenticated, with a credential configured")
	}
	if got := mcpErrorType(t, h); got != "mcp_authentication_failed_error" {
		t.Errorf("session error type = %q, want mcp_authentication_failed_error", got)
	}
}
