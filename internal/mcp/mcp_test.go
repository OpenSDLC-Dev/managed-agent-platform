package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The contract suite runs against real MCP servers built from the same SDK,
// served over real HTTP by httptest — not against a hand-rolled fake. A fake
// would encode this package's own understanding of the protocol and pass
// whether or not that understanding is right; an SDK server answers the way a
// server actually does, so a wrong request shape fails here rather than against
// a customer's endpoint. No network, no money.

// serveMCP starts an httptest server hosting an MCP server with the given
// tools, and returns its URL.
func serveMCP(t *testing.T, tools ...*sdk.Tool) string {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "test-server", Version: "1"}, nil)
	for _, tool := range tools {
		server.AddTool(tool, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{}, nil
		})
	}
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts.URL
}

// tool builds a server-side tool definition with a schema that is not the
// SDK's inferred default, so a test can tell a preserved schema from a
// fabricated one.
func tool(name, description string, required ...string) *sdk.Tool {
	props := map[string]any{}
	for _, r := range required {
		props[r] = map[string]any{"type": "string"}
	}
	return &sdk.Tool{
		Name:        name,
		Description: description,
		InputSchema: map[string]any{"type": "object", "properties": props, "required": required},
	}
}

// loopbackClient is a client whose dial guard is absent, so it can reach the
// httptest servers above (which listen on 127.0.0.1 — exactly what the
// production guard refuses). Every test that needs to talk to a fixture uses
// it; TestDefaultClientRefusesLoopback covers the guard itself.
func loopbackClient() *http.Client { return &http.Client{Timeout: mcp.DialTimeout} }

func TestListToolsReportsWhatTheServerOffers(t *testing.T) {
	t.Parallel()
	url := serveMCP(t,
		tool("get_issue", "Fetch one issue", "owner", "repo", "number"),
		tool("list_issues", "List issues", "owner", "repo"),
	)

	conn, err := mcp.Connect(context.Background(), mcp.Config{URL: url, HTTPClient: loopbackClient()})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()

	tools, err := conn.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("got %d tools, want 2: %+v", len(tools), tools)
	}
	if tools[0].Name != "get_issue" || tools[0].Description != "Fetch one issue" {
		t.Errorf("tools[0] = %+v", tools[0])
	}

	// The schema is carried through as the server wrote it, under the
	// Anthropic field name, so a catalog row needs no second translation.
	var schema struct {
		Type     string   `json:"type"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(tools[0].InputSchema, &schema); err != nil {
		t.Fatalf("input_schema %s: %v", tools[0].InputSchema, err)
	}
	if schema.Type != "object" || len(schema.Required) != 3 {
		t.Errorf("input_schema = %s, want the server's own object schema", tools[0].InputSchema)
	}

	// The wire name is input_schema, not MCP's inputSchema: a catalog row is
	// stored as-is and handed to the model later.
	encoded, err := json.Marshal(tools[0])
	if err != nil {
		t.Fatalf("marshal tool: %v", err)
	}
	if !strings.Contains(string(encoded), `"input_schema"`) {
		t.Errorf("tool JSON = %s, want an input_schema field", encoded)
	}
}

func TestListToolsOnAServerWithNoTools(t *testing.T) {
	t.Parallel()
	conn, err := mcp.Connect(context.Background(), mcp.Config{URL: serveMCP(t), HTTPClient: loopbackClient()})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()

	// Empty is a fact to record in the catalog, never an error: the reference
	// documents an unknown tool name as a warning, not a failure.
	tools, err := conn.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 0 {
		t.Fatalf("got %d tools, want none", len(tools))
	}
}

func TestListToolsFollowsPagination(t *testing.T) {
	t.Parallel()
	// More tools than one page: the SDK's server pages its own listing, so
	// this exercises the cursor loop rather than simulating one.
	var tools []*sdk.Tool
	for i := range 30 {
		tools = append(tools, tool(fmt.Sprintf("tool_%02d", i), "d"))
	}
	url := serveMCP(t, tools...)

	conn, err := mcp.Connect(context.Background(), mcp.Config{URL: url, HTTPClient: loopbackClient()})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()

	got, err := conn.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(got) != len(tools) {
		t.Fatalf("got %d tools, want %d — pagination dropped some", len(got), len(tools))
	}
	seen := map[string]bool{}
	for _, tl := range got {
		if seen[tl.Name] {
			t.Fatalf("tool %q listed twice", tl.Name)
		}
		seen[tl.Name] = true
	}
}

func TestBearerTokenIsSentAndDoesNotLeak(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var seen []string
	server := sdk.NewServer(&sdk.Implementation{Name: "test-server", Version: "1"}, nil)
	server.AddTool(tool("t", "d"), func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{}, nil
	})
	inner := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		inner.ServeHTTP(w, r)
	}))
	defer ts.Close()

	shared := loopbackClient()
	conn, err := mcp.Connect(context.Background(), mcp.Config{
		URL: ts.URL, BearerToken: "s3cret", HTTPClient: shared,
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := conn.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	conn.Close()

	mu.Lock()
	authed := append([]string(nil), seen...)
	seen = nil
	mu.Unlock()
	if len(authed) == 0 {
		t.Fatal("server saw no requests")
	}
	for i, h := range authed {
		if h != "Bearer s3cret" {
			t.Errorf("request %d Authorization = %q, want %q", i, h, "Bearer s3cret")
		}
	}

	// The credential must not stick to the caller's client: Config.HTTPClient
	// can be shared across servers (DefaultClient is), so a token injected for
	// one server reaching another would hand a customer's secret to a third
	// party. Reconnect over the same client with no token and assert none.
	plain, err := mcp.Connect(context.Background(), mcp.Config{URL: ts.URL, HTTPClient: shared})
	if err != nil {
		t.Fatalf("Connect (no token): %v", err)
	}
	if _, err := plain.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools (no token): %v", err)
	}
	plain.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("server saw no unauthenticated requests")
	}
	for i, h := range seen {
		if h != "" {
			t.Errorf("unauthenticated request %d carried Authorization %q — the token leaked "+
				"onto the shared client", i, h)
		}
	}
}

func TestConnectRequiresAURL(t *testing.T) {
	t.Parallel()
	if _, err := mcp.Connect(context.Background(), mcp.Config{}); err == nil {
		t.Fatal("Connect with no URL succeeded, want an error")
	}
}

func TestConnectFailsOnAnUnreachableServer(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no MCP here", http.StatusNotFound)
	}))
	defer ts.Close()

	conn, err := mcp.Connect(context.Background(), mcp.Config{URL: ts.URL, HTTPClient: loopbackClient()})
	if err == nil {
		conn.Close()
		t.Fatal("Connect to a non-MCP endpoint succeeded, want an error")
	}
	if !strings.Contains(err.Error(), ts.URL) {
		t.Errorf("error %q does not name the server URL — the driver reports which server failed", err)
	}
}

func TestDefaultClientRefusesLoopback(t *testing.T) {
	t.Parallel()
	// The production client is what a work item uses, and the httptest server
	// below is on 127.0.0.1 — the address class the guard exists to refuse.
	// This is the one test that does not swap the client out.
	url := serveMCP(t, tool("t", "d"))
	conn, err := mcp.Connect(context.Background(), mcp.Config{URL: url})
	if err == nil {
		conn.Close()
		t.Fatal("Connect reached a loopback address through the guarded client")
	}
}

func TestDefaultClientRefusesRedirects(t *testing.T) {
	t.Parallel()
	// Following a redirect would replay the request, Authorization header and
	// all, to a target the per-hop address guard vets but never approved as a
	// destination. Asserted on the client itself: its CheckRedirect is what
	// enforces this, and the guard above blocks reaching a fixture with it.
	req, err := http.NewRequest(http.MethodGet, "https://example.invalid/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if mcp.DefaultClient.CheckRedirect == nil {
		t.Fatal("DefaultClient follows redirects")
	}
	if err := mcp.DefaultClient.CheckRedirect(req, nil); err != http.ErrUseLastResponse {
		t.Errorf("CheckRedirect = %v, want ErrUseLastResponse", err)
	}
}

func TestDefaultClientGuardsEveryDial(t *testing.T) {
	t.Parallel()
	// The guard runs in the dialer's Control hook, so it sees the resolved
	// address of each dial rather than the name in the URL — a rebind that
	// answers with 127.0.0.1 on the second lookup is refused like the first.
	transport, ok := mcp.DefaultClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("DefaultClient.Transport = %T, want *http.Transport", mcp.DefaultClient.Transport)
	}
	if transport.DialContext == nil {
		t.Fatal("DefaultClient dials without a Control hook")
	}
	for _, addr := range []string{"127.0.0.1:443", "169.254.169.254:80", "[::1]:443", "[64:ff9b::7f00:1]:443"} {
		if _, err := transport.DialContext(context.Background(), "tcp", addr); err == nil {
			t.Errorf("dial to %s succeeded, want refusal", addr)
		}
	}
	// A routable address is not refused by the guard — it fails to connect for
	// its own reasons, which is a different error. 192.0.2.0/24 is TEST-NET-1
	// and is not answered, so the dial is bounded here rather than left to the
	// production 30s timeout: what is under test is which error comes back.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := transport.DialContext(ctx, "tcp", "192.0.2.1:9")
	if err != nil && strings.Contains(err.Error(), "disallowed address") {
		t.Errorf("guard refused a routable address: %v", err)
	}
}

func TestCloseIsSafeAfterAFailedCall(t *testing.T) {
	t.Parallel()
	url := serveMCP(t, tool("t", "d"))
	conn, err := mcp.Connect(context.Background(), mcp.Config{URL: url, HTTPClient: loopbackClient()})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	// A cancelled call is the work item's normal failure mode (a lease expires,
	// the session dies); the connection still has to close cleanly so the
	// driver's defer cannot wedge.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := conn.ListTools(ctx); err == nil {
		t.Error("ListTools with a cancelled context succeeded, want an error")
	}
	if err := conn.Close(); err != nil {
		t.Errorf("Close after a failed call: %v", err)
	}
}

func TestListToolsReportsATransportFailure(t *testing.T) {
	t.Parallel()
	// A server that accepts the connection and then stops answering: the
	// listing must surface an error rather than an empty catalog, since an
	// empty catalog is recorded as "this server has no tools".
	server := sdk.NewServer(&sdk.Implementation{Name: "test-server", Version: "1"}, nil)
	server.AddTool(tool("t", "d"), func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{}, nil
	})
	inner := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil)
	// Match on the JSON-RPC method in the body rather than on a header: the
	// mirrored Mcp-Method header is a 2026-07-28 addition, so keying off it
	// would silently stop matching whenever a negotiation lands on an earlier
	// revision — and a matcher that stops matching turns this into a test that
	// asserts nothing.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read", http.StatusInternalServerError)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if bytes.Contains(body, []byte(`"tools/list"`)) {
			http.Error(w, "gone", http.StatusBadGateway)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer ts.Close()

	conn, err := mcp.Connect(context.Background(), mcp.Config{URL: ts.URL, HTTPClient: loopbackClient()})
	if err != nil {
		// Some negotiations issue the listing during connect; either way the
		// failure must surface, which is what this test is about.
		return
	}
	defer conn.Close()
	if tools, err := conn.ListTools(context.Background()); err == nil {
		t.Fatalf("ListTools returned %d tools against a failing server, want an error — "+
			"an empty catalog is recorded as \"this server has no tools\"", len(tools))
	}
}
