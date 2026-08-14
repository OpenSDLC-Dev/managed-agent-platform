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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/mcp/mcptest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
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

// attachVaultWithAnUnopenableCredential is attachVaultWithMCPCredential's
// unusable twin: the sealed bytes are not this cipher's, the shape a rotated or
// misconfigured key leaves behind.
func (h *harness) attachVaultWithAnUnopenableCredential(t *testing.T, serverURL string) {
	t.Helper()
	ctx := context.Background()
	vaultID := domain.NewID("vlt").String()
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO vaults (id, display_name) VALUES ($1, 'test vault')`, vaultID); err != nil {
		t.Fatalf("insert vault: %v", err)
	}
	auth, err := json.Marshal(map[string]string{"type": "static_bearer", "mcp_server_url": serverURL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO vault_credentials (id, vault_id, auth_type, auth, secret_ciphertext, secret_key_id, cred_key)
		 VALUES ($1, $2, 'static_bearer', $3::jsonb, $4, $5, $6)`,
		domain.NewID("vcrd").String(), vaultID, auth,
		[]byte("not a ciphertext this key produced"), "test-1", "url:"+serverURL); err != nil {
		t.Fatalf("insert mcp credential: %v", err)
	}
	if _, err := h.pool.Exec(ctx,
		`UPDATE sessions SET vault_ids = $2 WHERE id = $1`, h.sid.String(), []string{vaultID}); err != nil {
		t.Fatalf("attach vault: %v", err)
	}
}

// serveRequiringBearer is an MCP server behind a gateway that answers 401 to any
// request not carrying the expected token — the shape every authenticating MCP
// endpoint has, and the one that proves the header actually left this process.
//
// seen reports the Authorization header and whether a request arrived at all.
// The two are separate answers because the header is empty in both of the cases
// a fail-closed test has to tell apart — no dial, and a dial that went out
// unauthenticated.
func serveRequiringBearer(t *testing.T, want string, tool mcptest.Tool) (url string, seen func() (string, bool)) {
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
	return ts.URL, func() (string, bool) {
		if p := got.Load(); p != nil {
			return *p, true
		}
		return "", false
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

	if got, _ := seen(); got != "Bearer lin_api_secret" {
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

	if got, _ := seen(); got != "Bearer discovery-token" {
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

// breakTheCredentialQuery makes the credential lookup fail for a reason of its
// own — the table it reads is gone — leaving the session's vault attachment
// intact so the resolver still runs. It stands in for the transient failures a
// test cannot arrange: a pool that blinked, a statement cancelled server-side.
func (h *harness) breakTheCredentialQuery(t *testing.T) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(), `DROP TABLE vault_credentials`); err != nil {
		t.Fatalf("drop vault_credentials: %v", err)
	}
}

// stepExpectingFault runs one step and returns the error the item faulted with,
// failing the test if it settled instead. A faulted item keeps its lease and is
// reclaimed, so nothing it would have written is committed.
func (h *harness) stepExpectingFault(t *testing.T) error {
	t.Helper()
	var faulted error
	h.exec.onFault = func(_ *queue.Item, err error) { faulted = err }
	h.stepOnce(t)
	if faulted == nil {
		t.Fatal("the item settled; a failed lookup must fault it so it is retried")
	}
	return faulted
}

// A lookup that failed says nothing about the credential, so the call must not
// be answered: a result event commits and nothing re-runs it, which would settle
// a transient failure permanently and point the operator at a credential that is
// fine. Faulting the item retries the whole call instead.
func TestMCPCallWithAFailedCredentialLookupFaultsRatherThanAnswers(t *testing.T) {
	h := mcpHarness(t)
	url, seen := serveRequiringBearer(t, "never-resolved",
		mcptest.Tool{Name: "ask", Blocks: []mcptest.Block{{Type: "text", Text: "served anyway"}}})
	h.attachVaultWithMCPCredential(t, url, "never-resolved")
	h.declareListedMCPServers(t, [2]string{"docs", url})
	h.appendMCPToolUse(t, "docs", "ask", `{}`)
	h.enqueueMCP(t)
	h.breakTheCredentialQuery(t)

	faulted := h.stepExpectingFault(t)
	if !strings.Contains(faulted.Error(), "mcp credential for") {
		t.Errorf("the item faulted for another reason, so this pins nothing: %v", faulted)
	}
	if _, reached := seen(); reached {
		t.Error("the dial went out although the credential was never resolved")
	}
	if results := h.mcpResults(t); len(results) != 0 {
		t.Errorf("the call was answered %d times; a faulted item leaves it unanswered", len(results))
	}
	if errs := h.sessionErrors(t); len(errs) != 0 {
		t.Errorf("a failed lookup emitted %d session errors", len(errs))
	}
}

// A pass whose budget runs out inside the credential settles like one that ran
// out between servers — a notReached row — rather than faulting and throwing
// away the rows every server before it earned. Resolving a credential used to be
// a query and a decrypt; with the OAuth refresh it can be seconds of
// third-party I/O, which is where a discovery budget realistically lands.
//
// Driven at discoverServer rather than through a step, because the budget has to
// expire *inside* the credential: a pass whose budget is already spent when the
// loop reaches a server never gets that far.
func TestABudgetSpentInsideTheCredentialSettlesRatherThanFaulting(t *testing.T) {
	h := mcpHarness(t)
	url, _ := serveRequiringBearer(t, "never-resolved",
		mcptest.Tool{Name: "ask", Blocks: []mcptest.Block{{Type: "text", Text: "unreached"}}})
	h.attachVaultWithMCPCredential(t, url, "never-resolved")
	h.declareListedMCPServers(t, [2]string{"docs", url})
	h.enqueueMCP(t)
	h.breakTheCredentialQuery(t)

	item, err := h.queue.Claim(context.Background(), queue.MCPExec, time.Minute)
	if err != nil || item == nil {
		t.Fatalf("claim: %+v %v", item, err)
	}
	sess, live, err := h.exec.sessionForRun(context.Background(), item)
	if err != nil || !live {
		t.Fatalf("sessionForRun: live=%v err=%v", live, err)
	}
	ref := sess.mcpServers[0]

	// With time left, the lookup's own failure faults the item: it says nothing
	// about the credential, so the pass is worth retrying.
	if _, _, _, err := h.exec.prepareServer(context.Background(), sess.envConfig, sess.vaultIDs, ref); err == nil {
		t.Fatal("a failed lookup with budget left settled instead of faulting")
	}

	// With the budget spent, the same failure is this pass running out of time.
	spent, cancel := context.WithCancel(context.Background())
	cancel()
	row, _, _, err := h.exec.prepareServer(spent, sess.envConfig, sess.vaultIDs, ref)
	if err != nil {
		t.Fatalf("a spent budget faulted the pass: %v", err)
	}
	if !row.notReached || row.status != "failed" {
		t.Errorf("row = %+v, want a failed, not-reached row", row)
	}
	if !strings.Contains(row.reason, "ran out of time") {
		t.Errorf("row reason = %q, want it to say the pass ran out of time", row.reason)
	}
}

// The clock is read before the credential's own verdict, because a resolution
// cut short marks some of its failures against the credential — a decrypt that
// never returned reads exactly like one that was refused, and a cipher backend
// can be a network call. Blaming a healthy credential is a verdict the row keeps
// and the tool call commits, so the budget has to win the tie.
func TestASpentBudgetOutranksTheCredentialsOwnVerdict(t *testing.T) {
	h := mcpHarness(t)
	url, _ := serveRequiringBearer(t, "never-resolved",
		mcptest.Tool{Name: "ask", Blocks: []mcptest.Block{{Type: "text", Text: "unreached"}}})
	// A credential whose sealed bytes this cipher cannot open: every failure it
	// produces is marked against the credential.
	h.attachVaultWithAnUnopenableCredential(t, url)
	h.declareListedMCPServers(t, [2]string{"docs", url})
	h.enqueueMCP(t)

	item, err := h.queue.Claim(context.Background(), queue.MCPExec, time.Minute)
	if err != nil || item == nil {
		t.Fatalf("claim: %+v %v", item, err)
	}
	sess, live, err := h.exec.sessionForRun(context.Background(), item)
	if err != nil || !live {
		t.Fatalf("sessionForRun: live=%v err=%v", live, err)
	}
	ref := sess.mcpServers[0]

	// With time left, the credential is what the row blames.
	row, _, _, err := h.exec.prepareServer(context.Background(), sess.envConfig, sess.vaultIDs, ref)
	if err != nil {
		t.Fatalf("an unusable credential faulted the pass: %v", err)
	}
	if row.notReached || !strings.Contains(row.reason, "authentication failed") {
		t.Errorf("row = %+v, want the credential blamed while there was time", row)
	}

	// With the budget spent, the clock is. Spent from inside the cipher and not
	// before the call, because a context already done fails the credential
	// *query* first — a plain error, not the credential's fault — which would
	// leave the two orderings indistinguishable and prove nothing.
	spent, cancel := context.WithCancel(context.Background())
	h.exec.cipher = cipherThatSpendsTheBudget{Cipher: h.cipher, spend: cancel}
	row, _, _, err = h.exec.prepareServer(spent, sess.envConfig, sess.vaultIDs, ref)
	if err != nil {
		t.Fatalf("a spent budget faulted the pass: %v", err)
	}
	if !row.notReached || !strings.Contains(row.reason, "ran out of time") {
		t.Errorf("row = %+v, want the clock blamed rather than the credential", row)
	}
}

// And the clock is read after a resolution that *succeeded*, not only after one
// that failed: a refresh can finish just as the budget runs out, and the dial
// below would then fail on a spent context and be stored as a server that was
// reached and refused this platform.
func TestABudgetSpentByASuccessfulCredentialSettlesBeforeTheDial(t *testing.T) {
	h := mcpHarness(t)
	url, seen := serveRequiringBearer(t, "tok",
		mcptest.Tool{Name: "ask", Blocks: []mcptest.Block{{Type: "text", Text: "unreached"}}})
	h.attachVaultWithMCPCredential(t, url, "tok")
	h.declareListedMCPServers(t, [2]string{"docs", url})
	h.enqueueMCP(t)

	item, err := h.queue.Claim(context.Background(), queue.MCPExec, time.Minute)
	if err != nil || item == nil {
		t.Fatalf("claim: %+v %v", item, err)
	}
	sess, live, err := h.exec.sessionForRun(context.Background(), item)
	if err != nil || !live {
		t.Fatalf("sessionForRun: live=%v err=%v", live, err)
	}

	spent, cancel := context.WithCancel(context.Background())
	h.exec.cipher = cipherThatSpendsTheBudget{Cipher: h.cipher, spend: cancel}
	row, _, dial, err := h.exec.prepareServer(spent, sess.envConfig, sess.vaultIDs, sess.mcpServers[0])
	if err != nil {
		t.Fatalf("a spent budget faulted the pass: %v", err)
	}
	if !row.notReached || !strings.Contains(row.reason, "ran out of time") {
		t.Errorf("row = %+v, want the clock blamed rather than the server", row)
	}
	// The dial is a separate phase now, and this is what keeps it from starting:
	// a server the serial phase already has a verdict on is not handed to it.
	if dial {
		t.Error("a spent budget still handed the server to the dialling phase")
	}
	if _, reached := seen(); reached {
		t.Error("the dial went out on a context that was already spent")
	}
}

// cipherThatSpendsTheBudget stands in for a cipher backend that is a network
// round trip of its own (OpenBao transit, Cloud KMS): the pass's budget runs out
// while it is in flight, and whatever it answers afterwards arrives too late.
type cipherThatSpendsTheBudget struct {
	secrets.Cipher
	spend func()
}

func (c cipherThatSpendsTheBudget) Decrypt(ctx context.Context, ciphertext []byte, keyID string) ([]byte, error) {
	c.spend()
	return c.Cipher.Decrypt(ctx, ciphertext, keyID)
}

// The other half of the same rule: a pass that already answered a call must not
// throw that answer away to fault. An MCP tool can have a side effect, and a
// discarded result is one the reclaim runs a second time.
func TestMCPPassCommitsWhatItAnsweredBeforeALookupFails(t *testing.T) {
	h := mcpHarness(t)
	var breakOnce func()
	url := serveInterceptingCalls(t,
		mcptest.Tool{Name: "ask", Blocks: []mcptest.Block{{Type: "text", Text: "answered"}}},
		func() { breakOnce() })
	h.attachVaultWithMCPCredential(t, url, "tok")
	// Armed only once the vault row exists, and fired on the first call — whose
	// own credential was resolved before this server was reached, so it is
	// answered and only the second call's lookup fails.
	var once sync.Once
	breakOnce = func() { once.Do(func() { h.breakTheCredentialQuery(t) }) }

	h.declareListedMCPServers(t, [2]string{"docs", url})
	h.appendMCPToolUse(t, "docs", "ask", `{}`)
	h.appendMCPToolUse(t, "docs", "ask", `{}`)
	h.enqueueMCP(t)

	var faulted error
	h.exec.onFault = func(_ *queue.Item, err error) { faulted = err }
	h.stepOnce(t)

	if faulted != nil {
		t.Errorf("the item faulted and discarded the answer it had: %v", faulted)
	}
	if results := h.mcpResults(t); len(results) != 1 {
		t.Fatalf("got %d results, want the one call that was answered committed", len(results))
	}
	if n := h.liveOf(t, queue.MCPExec); n != 1 {
		t.Errorf("live mcp_exec items = %d, want the item back for the call it could not make", n)
	}
}

// serveInterceptingCalls runs `before` on each tool call and then serves it, so
// a test can change the world between two calls of one pass.
//
// Before the answer rather than after it, which is what makes the order a fact:
// the driver is blocked on this response, so anything done here is done before
// it moves on. Run afterwards, the change would race the next call's own
// queries — and lose in CI, where the driver is not the slow half.
func serveInterceptingCalls(t *testing.T, tool mcptest.Tool, before func()) string {
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
			before()
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

// Discovery decides it the same way. A failed row would blame the credential in
// a stored column an operator reads later, for a failure that was never the
// credential's.
func TestMCPDiscoveryWithAFailedCredentialLookupFaultsRatherThanFailsTheRow(t *testing.T) {
	h := mcpHarness(t)
	url, seen := serveRequiringBearer(t, "never-resolved",
		mcptest.Tool{Name: "ask", Blocks: []mcptest.Block{{Type: "text", Text: "ok"}}})
	h.attachVaultWithMCPCredential(t, url, "never-resolved")
	h.declareMCPServers(t, [2]string{"docs", url})
	h.enqueueMCP(t)
	h.breakTheCredentialQuery(t)

	faulted := h.stepExpectingFault(t)
	if !strings.Contains(faulted.Error(), "mcp credential for") {
		t.Errorf("the item faulted for another reason, so this pins nothing: %v", faulted)
	}
	if _, reached := seen(); reached {
		t.Error("discovery dialled although the credential was never resolved")
	}
	if row, ok := h.catalog(t)["docs"]; ok {
		t.Errorf("a catalog row was written blaming the credential: %q (%s)", row.status, row.reason)
	}
}

// serveQuotingTheToken answers `refuse` with a 401 whose JSON-RPC error message
// contains the request's own Authorization header — a gateway naming the
// credential it turned away.
//
// That is the one route a secret takes into a stored reason that no rule over
// URLs can close, and it is not hypothetical: go-sdk decodes a non-2xx body,
// finds the JSON-RPC error and carries its message into the error it returns
// (mcp/streamable.go, checkResponse).
func serveQuotingTheToken(t *testing.T, refuse string, tool mcptest.Tool) string {
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
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &msg)
		if msg.Method == refuse {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": msg.ID,
				"error": map[string]any{"code": -32000,
					"message": "rejected " + r.Header.Get("Authorization")},
			})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

// A server's own error text reaches a stored column and, later, a model. The
// endpoint's renderings are redacted from it, but a vault's token is in none of
// them — and unlike the endpoint, it is nowhere else at rest in the clear.
func TestMCPCallDoesNotStoreATokenTheServerQuotedBack(t *testing.T) {
	for _, refuse := range []string{"initialize", "tools/call"} {
		t.Run(refuse, func(t *testing.T) {
			h := mcpHarness(t)
			url := serveQuotingTheToken(t, refuse,
				mcptest.Tool{Name: "ask", Blocks: []mcptest.Block{{Type: "text", Text: "ok"}}})
			h.attachVaultWithMCPCredential(t, url, "sk-quoted-back")
			h.declareListedMCPServers(t, [2]string{"docs", url})
			h.appendMCPToolUse(t, "docs", "ask", `{}`)
			h.enqueueMCP(t)

			h.stepOnce(t)

			msg := mcpErrorMessage(t, h)
			// The redacted form, not merely the absence of the token: without it
			// this would pass just as well on a reason the message never reached.
			if !strings.Contains(msg, "rejected Bearer ***") {
				t.Errorf("session error = %q, want the quoted credential replaced", msg)
			}
			for what, text := range map[string]string{
				"session error":   msg,
				"the tool result": string(mustJSON(t, h.mcpResults(t))),
			} {
				if strings.Contains(text, "sk-quoted-back") {
					t.Errorf("%s carries the vault's token: %s", what, text)
				}
			}
		})
	}
}

// The three redactions are ordered, and the secret goes last: the two before it
// substitute text *in*, so a token the endpoint pass happens to write back would
// survive a scrub that ran first.
func TestStorableReasonScrubsASecretItsOwnRedactionWouldPutBack(t *testing.T) {
	// Declared with an upper-case scheme, which url.Parse lowercases — so the
	// safe form the endpoint pass writes contains text the declared bytes do
	// not. A token scrubbed before that pass is a token the pass can put back.
	const endpoint = "HTTPS://mcp.example.com/rpc"
	// Absurd as a bearer token, and that is the point: the ordering has to hold
	// for any token rather than for the likely ones.
	const token = "https"

	got := storableReason("dialling "+endpoint+" failed", endpoint, token)

	if strings.Contains(got, token) {
		t.Errorf("reason = %q, still carries the token", got)
	}
}

// A server that succeeds can quote the token just as well as one that fails —
// in the answer it returns, and in the tool metadata it publishes. Both land in
// storage the failure path's redaction never runs on: the result event and its
// spill, and mcp_catalogs.tools, which is also what the model is offered.
func TestMCPScrubsATokenEchoedInASuccessfulAnswer(t *testing.T) {
	h := mcpHarness(t)
	url := serveEchoingTheToken(t)
	h.attachVaultWithMCPCredential(t, url, "sk-echoed-back")
	h.declareListedMCPServers(t, [2]string{"docs", url})
	h.appendMCPToolUse(t, "docs", "ask", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	results := h.mcpResults(t)
	if len(results) != 1 {
		t.Fatalf("got %d results, want the call answered", len(results))
	}
	rendered := string(mustJSON(t, results))
	// The redacted form, so this cannot pass on an answer that never carried it.
	if !strings.Contains(rendered, "you sent Bearer ***") {
		t.Errorf("result = %s, want the echoed credential replaced", rendered)
	}
	if strings.Contains(rendered, "sk-echoed-back") {
		t.Errorf("the tool result carries the vault's token: %s", rendered)
	}
}

// The listing half, stored in mcp_catalogs and handed to the model as the tool's
// own definition.
func TestMCPScrubsATokenEchoedInAToolDefinition(t *testing.T) {
	h := mcpHarness(t)
	url := serveEchoingTheToken(t)
	h.attachVaultWithMCPCredential(t, url, "sk-echoed-back")
	h.declareMCPServers(t, [2]string{"docs", url})
	h.enqueueMCP(t)

	h.stepOnce(t)

	row := h.catalog(t)["docs"]
	if row.status != "ready" {
		t.Fatalf("catalog row = %q (%s), want ready", row.status, row.reason)
	}
	stored := string(mustJSON(t, row.tools))
	if !strings.Contains(stored, "you sent Bearer ***") {
		t.Errorf("stored tools = %s, want the echoed credential replaced", stored)
	}
	if strings.Contains(stored, "sk-echoed-back") {
		t.Errorf("the stored listing carries the vault's token: %s", stored)
	}
}

// serveEchoingTheToken is an MCP server that quotes the Authorization header it
// received back into both halves of what it publishes: the description of the
// tool it lists, and the text of the answer it returns.
func serveEchoingTheToken(t *testing.T) string {
	t.Helper()
	inner := sdk.NewStreamableHTTPHandler(func(r *http.Request) *sdk.Server {
		echo := "you sent " + r.Header.Get("Authorization")
		s := sdk.NewServer(&sdk.Implementation{Name: "echoing-server", Version: "1"}, nil)
		sdk.AddTool(s, &sdk.Tool{Name: "ask", Description: echo},
			func(context.Context, *sdk.CallToolRequest, map[string]any) (
				*sdk.CallToolResult, map[string]any, error) {
				return &sdk.CallToolResult{
					Content: []sdk.Content{&sdk.TextContent{Text: echo}},
				}, nil, nil
			})
		return s
	}, nil)
	ts := httptest.NewServer(inner)
	t.Cleanup(ts.Close)
	return ts.URL
}

// The discovery path stores its reason in a catalog row rather than an event,
// and reaches redaction by a different line, so it is pinned separately.
func TestMCPDiscoveryDoesNotStoreATokenTheServerQuotedBack(t *testing.T) {
	h := mcpHarness(t)
	url := serveQuotingTheToken(t, "initialize",
		mcptest.Tool{Name: "ask", Blocks: []mcptest.Block{{Type: "text", Text: "ok"}}})
	h.attachVaultWithMCPCredential(t, url, "sk-quoted-back")
	h.declareMCPServers(t, [2]string{"docs", url})
	h.enqueueMCP(t)

	h.stepOnce(t)

	row := h.catalog(t)["docs"]
	if !strings.Contains(row.reason, "rejected Bearer ***") {
		t.Errorf("row reason = %q, want the quoted credential replaced", row.reason)
	}
	if strings.Contains(row.reason, "sk-quoted-back") {
		t.Errorf("row reason carries the vault's token: %q", row.reason)
	}
}

// mustJSON renders a value so a test can search everything it holds at once.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// mcpErrorMessage reads the single session.error's message.
func mcpErrorMessage(t *testing.T, h *harness) string {
	t.Helper()
	errs := h.sessionErrors(t)
	if len(errs) != 1 {
		t.Fatalf("got %d session errors, want 1", len(errs))
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(errs[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	return body.Error.Message
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

	h.attachVaultWithAnUnopenableCredential(t, ts.URL)

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

// A credential that cannot be opened is an authentication failure on the
// discovery path too, and its row says so: the two paths decide it with one
// predicate, so a reason that named neither the credential nor the failure would
// send an operator looking at the network.
func TestMCPDiscoveryWithAnUnopenableCredentialNamesTheCredential(t *testing.T) {
	h := mcpHarness(t)
	url, seen := serveRequiringBearer(t, "never-sent",
		mcptest.Tool{Name: "ask", Blocks: []mcptest.Block{{Type: "text", Text: "ok"}}})

	h.attachVaultWithAnUnopenableCredential(t, url)

	h.declareMCPServers(t, [2]string{"docs", url})
	h.enqueueMCP(t)

	h.stepOnce(t)

	if got, reached := seen(); reached {
		t.Errorf("discovery dialled anyway, with Authorization %q", got)
	}
	row := h.catalog(t)["docs"]
	if row.status != "failed" {
		t.Fatalf("catalog row = %q, want failed", row.status)
	}
	if !strings.HasPrefix(row.reason, "authentication failed:") {
		t.Errorf("row reason = %q, want it to name an authentication failure", row.reason)
	}
	if !strings.Contains(row.reason, "credential") {
		t.Errorf("row reason = %q, want it to name the credential", row.reason)
	}
}
