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
	"sync/atomic"
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
// fixturePageSize is small on purpose. The SDK server's default is 1,000
// (mcp.DefaultPageSize), so a fixture that leaves it alone answers every
// realistic listing in one page — and a client that fetched only the first page
// would pass a test named for following pagination. Every fixture pages, so the
// cursor loop is exercised by the whole suite rather than by one test that has
// to remember to ask for it.
const fixturePageSize = 7

func serveMCP(t *testing.T, tools ...*sdk.Tool) string {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "test-server", Version: "1"},
		&sdk.ServerOptions{PageSize: fixturePageSize})
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

// serveToolsList starts a server whose handshake is a real SDK server's but
// whose tools/list result is written by hand, and counts the listing requests.
//
// The suite otherwise runs against SDK servers on purpose, and this is the
// deliberate exception rather than a retreat from it: what these tests are
// about is what a *non-SDK* server can put on the wire — a null entry in the
// tools array, a schema that is not an object, a cursor that never advances.
// An SDK server cannot produce any of it, so a fixture that only ever speaks
// through one cannot reach the code that survives it. Everything except the
// listing still goes through the real handler, so the connection under test is
// a real negotiated one.
func serveToolsList(t *testing.T, result func(cursor string) map[string]any) (url string, calls *atomic.Int32) {
	t.Helper()
	calls = &atomic.Int32{}
	inner := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server {
		return sdk.NewServer(&sdk.Implementation{Name: "raw-server", Version: "1"}, nil)
	}, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read", http.StatusInternalServerError)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Cursor string `json:"cursor"`
			} `json:"params"`
		}
		if json.Unmarshal(body, &req) != nil || req.Method != "tools/list" {
			inner.ServeHTTP(w, r)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  result(req.Params.Cursor),
		})
	}))
	t.Cleanup(ts.Close)
	return ts.URL, calls
}

func TestListToolsContainsAPanicInsideTheClientLibrary(t *testing.T) {
	t.Parallel()
	// `"tools": [null]` is legal JSON that go-sdk v1.7.0 decodes into a nil
	// element and then dereferences without checking (filterValidTools →
	// validateParamHeaderAnnotations), so the client panics on a response a
	// customer-named server chose to send. The caller is an executor shared by
	// every session on the host and a Go panic is not confined to its
	// goroutine, so an unhandled one here is not a failed work item — it is
	// every concurrent tool call on that executor.
	//
	// If a later SDK release stops panicking and skips the nil element instead,
	// this test goes *red* rather than quietly green: the assertion below wants
	// an error, and a clean empty listing is not one. That is deliberate — the
	// failure is the notice to rewrite it as a skip assertion, where silence
	// would let the recover outlive the bug it exists for. What it must never
	// do is crash.
	url, _ := serveToolsList(t, func(string) map[string]any {
		return map[string]any{"tools": []any{nil}}
	})

	conn, err := mcp.Connect(context.Background(), mcp.Config{URL: url, HTTPClient: loopbackClient()})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()

	// Reaching the next line at all is most of the assertion: an uncontained
	// panic ends the test binary rather than failing this test.
	if _, err := conn.ListTools(context.Background()); err == nil {
		t.Fatal("ListTools reported success on a response the client library cannot parse")
	}
}

func TestListToolsSkipsEntriesThatCannotBeATool(t *testing.T) {
	t.Parallel()
	// Entries a server is free to send and the SDK decodes without complaint.
	// Two of them are tools; the rest are not, and none may take those down
	// with them — the reference treats an unresolvable tool as a warning, so a
	// malformed entry must not deny an agent the rest of a server's catalog.
	//
	// The boundary rows are the point of several of these. A 128-byte name is
	// legal and a 129-byte one is not, so both are here: a guard written with
	// >= would drop a name the rule allows, and the suite would never say so.
	// Likewise "empty_schema" and "array_type" are the two ways a schema can
	// fail the root-type rule without being obviously junk — {} carries no
	// type at all and {"type":"array"} carries a well-formed wrong one — and a
	// check that only rejects a *present* wrong type admits the first.
	maxName := strings.Repeat("n", 128)
	url, _ := serveToolsList(t, func(string) map[string]any {
		return map[string]any{"tools": []any{
			map[string]any{"name": "", "inputSchema": map[string]any{"type": "object"}},
			map[string]any{"name": "bad name!", "inputSchema": map[string]any{"type": "object"}},
			map[string]any{"name": strings.Repeat("x", 129), "inputSchema": map[string]any{"type": "object"}},
			map[string]any{"name": "not_a_schema", "inputSchema": 42},
			map[string]any{"name": "null_schema", "inputSchema": nil},
			map[string]any{"name": "no_schema", "description": "takes nothing"},
			map[string]any{"name": "wrong_type", "inputSchema": map[string]any{"type": 42}},
			map[string]any{"name": "array_type", "inputSchema": map[string]any{"type": "array"}},
			map[string]any{"name": "empty_schema", "inputSchema": map[string]any{}},
			map[string]any{"name": maxName, "inputSchema": map[string]any{"type": "object"}},
			map[string]any{"name": "good", "inputSchema": map[string]any{
				"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}},
			}},
		}}
	})

	conn, err := mcp.Connect(context.Background(), mcp.Config{URL: url, HTTPClient: loopbackClient()})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()

	tools, err := conn.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var names []string
	byName := map[string]mcp.Tool{}
	for _, tl := range tools {
		names = append(names, tl.Name)
		byName[tl.Name] = tl
	}
	// "good" and the 128-byte name survive; nothing else does. In particular
	// "no_schema", "null_schema" and "empty_schema" do not: substituting
	// {"type":"object"} for a schema the server never sent — or reading {} as
	// though it had said so — publishes "this tool takes no arguments" on the
	// server's behalf, and a tool that in fact requires arguments would then be
	// called with none. A fabricated contract is worse than a dropped tool.
	if len(tools) != 2 || byName["good"].Name == "" || byName[maxName].Name == "" {
		t.Fatalf("got tools %v, want exactly [%s good]", names, "<128-byte name>")
	}
	if !strings.Contains(string(byName["good"].InputSchema), `"properties"`) {
		t.Errorf("good input_schema = %s, want the server's own schema", byName["good"].InputSchema)
	}
}

func TestListToolsCannotTellAnAbsentToolsFieldFromAnEmptyOne(t *testing.T) {
	t.Parallel()
	// MCP requires a successful tools/list result to carry a `tools`
	// collection, so a result of `{}` is malformed and arguably ought to be a
	// retryable error rather than the durable fact "this server has no tools".
	// It is not treated that way, and the reason is not a judgment call: the
	// SDK erases the difference before this package sees it. ListTools ends
	// with `result.Tools = filterValidTools(...)`, and filterValidTools builds
	// its result with `make([]*Tool, 0, len(tools))` — so a nil `tools` and an
	// empty one both arrive here as a non-nil empty slice, and no check at this
	// layer can separate them.
	//
	// This test exists to pin that, because "distinguish absent from empty" is
	// a natural thing for a reviewer to ask for and it cannot be built without
	// abandoning the SDK's typed call. If a later SDK release stops normalizing
	// it, this test goes red and the choice becomes a real one.
	url, _ := serveToolsList(t, func(string) map[string]any {
		return map[string]any{} // no "tools" key at all
	})

	conn, err := mcp.Connect(context.Background(), mcp.Config{URL: url, HTTPClient: loopbackClient()})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()

	tools, err := conn.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v — the SDK now distinguishes absent from empty, "+
			"so this package can too; see docs/DIVERGENCES.md", err)
	}
	if len(tools) != 0 {
		t.Fatalf("got %d tools from a result with no tools field", len(tools))
	}
}

func TestListToolsRefusesAResponseTooLargeToRead(t *testing.T) {
	t.Parallel()
	// go-sdk v1.7.0 reads a response with io.ReadAll before decoding anything,
	// so an unbounded body is an unbounded allocation in an executor shared by
	// every session on the host. Neither the request timeout nor the page bound
	// helps — both count requests, not bytes — and no recover catches an OOM.
	//
	// The fixture streams past the limit without ever finishing, and without a
	// Content-Length, which is the shape that defeats any check made before the
	// body is read.
	var served atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		chunk := bytes.Repeat([]byte("x"), 1<<16)
		for served.Load() < 4*mcp.MaxResponseBytes {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			served.Add(int64(len(chunk)))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := mcp.Connect(ctx, mcp.Config{URL: ts.URL, HTTPClient: loopbackClient()})
	if err == nil {
		defer conn.Close()
		if _, err = conn.ListTools(ctx); err == nil {
			t.Fatal("a response larger than the limit was accepted")
		}
	}
	// The assertion that matters is that the read stopped: without the bound
	// the handler runs until it has written everything it means to.
	if got := served.Load(); got > 2*mcp.MaxResponseBytes {
		t.Errorf("server streamed %d bytes past a %d-byte limit — the body was not bounded",
			got, mcp.MaxResponseBytes)
	}
}

func TestConnectClosesTheConnectionItCannotHandBack(t *testing.T) {
	t.Parallel()
	// The SDK returns unsupportedProtocolVersionError without closing the
	// session it just built, and hands back no session for the caller to close
	// — so this package keeps the transport's Connection in order to close it
	// itself. A server that answers this way on every attempt would otherwise
	// leak a reader goroutine and its connection per attempt.
	//
	// Asserted through the protocol rather than by counting goroutines: closing
	// a streamable connection that has a session id sends an HTTP DELETE to the
	// endpoint, so the fixture assigns one and then watches for the DELETE.
	var initialized, deleted atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read", http.StatusInternalServerError)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(body, &req) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Mcp-Session-Id", "sesn-under-test")
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		if req.Method == "initialize" {
			initialized.Add(1)
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"protocolVersion": "2099-01-01",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "from-the-future", "version": "1"},
			}})
			return
		}
		_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{
			"code": -32601, "message": "method not found",
		}})
	}))
	defer ts.Close()

	conn, err := mcp.Connect(context.Background(), mcp.Config{URL: ts.URL, HTTPClient: loopbackClient()})
	if err == nil {
		conn.Close()
		t.Fatal("Connect accepted a protocol version no revision defines")
	}
	if initialized.Load() == 0 {
		t.Fatal("the server was never asked to initialize, so there was nothing to leak")
	}
	if deleted.Load() == 0 {
		t.Error("a failed Connect left the SDK's connection open — no DELETE reached the server")
	}
}

func TestListToolsRefusesACursorThatNeverAdvances(t *testing.T) {
	t.Parallel()
	// Pagination is server-driven: the loop ends when a server omits the next
	// cursor, so a server that returns the same cursor forever never ends it.
	// This is the shape a broken cursor implementation actually takes, and the
	// work item doing the listing holds a queue lease while it spins.
	url, calls := serveToolsList(t, func(string) map[string]any {
		return map[string]any{"tools": []any{}, "nextCursor": "stuck"}
	})

	conn, err := mcp.Connect(context.Background(), mcp.Config{URL: url, HTTPClient: loopbackClient()})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()

	tools, err := conn.ListTools(context.Background())
	if err == nil {
		t.Fatalf("ListTools returned %d tools against a stuck cursor, want an error", len(tools))
	}
	if !strings.Contains(err.Error(), "cursor") {
		t.Errorf("error %q does not say the cursor is the problem", err)
	}
	// Caught on the request that proves it, not after a hundred round trips.
	if got := calls.Load(); got != 2 {
		t.Errorf("server saw %d listing requests, want 2", got)
	}
}

func TestListToolsStopsOnAServerThatPaginatesForever(t *testing.T) {
	t.Parallel()
	// The cursor advances every time, so nothing about any single response is
	// wrong — only the sequence is. Without a bound this call never returns,
	// so the assertion that matters is that it returns at all.
	url, calls := serveToolsList(t, func(cursor string) map[string]any {
		return map[string]any{"tools": []any{}, "nextCursor": cursor + "x"}
	})

	conn, err := mcp.Connect(context.Background(), mcp.Config{URL: url, HTTPClient: loopbackClient()})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()

	done := make(chan error, 1)
	go func() {
		_, err := conn.ListTools(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("ListTools returned no error against a server that never stops paginating")
		}
		if !strings.Contains(err.Error(), "paginating") {
			t.Errorf("error %q does not say pagination is the problem", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("ListTools did not return — the page bound is not bounding")
	}
	if got := calls.Load(); got < 2 {
		t.Errorf("server saw %d listing requests, want the loop to have actually run", got)
	}
}

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

	// The schema's meaning is carried through, under the Anthropic field name,
	// so a catalog row needs no second translation. Meaning rather than bytes:
	// the SDK hands over a decoded value, so re-marshaling sorts keys — which
	// is why this reads the fields rather than comparing the JSON text.
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
	// Several times one page: the SDK's server pages its own listing, so this
	// exercises the cursor loop rather than simulating one.
	var tools []*sdk.Tool
	for i := range 30 {
		tools = append(tools, tool(fmt.Sprintf("tool_%02d", i), "d"))
	}
	if len(tools) <= fixturePageSize {
		t.Fatalf("the fixture fits in one page of %d — nothing here paginates", fixturePageSize)
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
	// By position, not as a set: a set assertion passes on a listing whose
	// pages came back interleaved or reversed, and the order a server lists
	// its tools in is the order they are offered to the model.
	for i, tl := range got {
		if want := fmt.Sprintf("tool_%02d", i); tl.Name != want {
			t.Fatalf("tools[%d] = %q, want %q — pagination reordered the listing", i, tl.Name, want)
		}
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

func TestBearerTokenDoesNotFollowARedirectToAnotherServer(t *testing.T) {
	t.Parallel()
	// net/http strips Authorization when a redirect changes origin — but only
	// from headers set on the outbound request. A header a RoundTripper adds is
	// invisible to that logic, so the stripping has nothing to strip and the
	// wrapper simply runs again against the new host. The caller's client is
	// what decides whether redirects are followed, and the executor supplies
	// its own; the credential must not depend on that choice.
	var mu sync.Mutex
	var elsewhereSaw []string
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		elsewhereSaw = append(elsewhereSaw, r.Header.Get("Authorization"))
		mu.Unlock()
		http.Error(w, "not the server the token was resolved for", http.StatusOK)
	}))
	defer elsewhere.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	// A caller-supplied client that follows redirects, which is the default and
	// therefore the case a caller falls into without choosing it.
	conn, err := mcp.Connect(context.Background(), mcp.Config{
		URL:         redirector.URL,
		BearerToken: "s3cret",
		HTTPClient:  &http.Client{Timeout: mcp.DialTimeout},
	})
	if err == nil {
		// Connecting through a redirector is not expected to work; what matters
		// is where the credential went, asserted below either way.
		defer conn.Close()
		_, _ = conn.ListTools(context.Background())
	}

	mu.Lock()
	defer mu.Unlock()
	// The redirect is followed — the client under test is one that follows
	// them — so this is not the vacuous "nothing happened, therefore nothing
	// leaked". The other server was reached; it must not have been given the
	// credential.
	if len(elsewhereSaw) == 0 {
		t.Fatal("the redirect target was never reached, so this test asserted nothing")
	}
	for i, h := range elsewhereSaw {
		if h != "" {
			t.Errorf("redirect target request %d carried %q — the credential "+
				"resolved for one server reached another", i, h)
		}
	}
}

func TestConnectFailsOnAnUnsupportedProtocolVersion(t *testing.T) {
	t.Parallel()
	// A server that refuses server/discover and then answers the legacy
	// initialize with a version no revision defines. The client must reject it
	// rather than proceed against a protocol it does not implement.
	//
	// This is the path that reaches the upstream leak: go-sdk v1.7.0 returns
	// `unsupportedProtocolVersionError` without closing the session it just
	// built (mcp/client.go, the `!slices.Contains(supportedProtocolVersions,
	// ...)` branch — the adjacent initialize and initialized failure paths both
	// call `cs.Close()`), and Connect hands back no session for a caller to
	// close. Connect works around it by capturing the transport's Connection,
	// so a server answering this way on every attempt does not leak per
	// attempt; TestConnectClosesTheConnectionItCannotHandBack is what asserts
	// that, by watching for the DELETE. The upstream bug is still upstream's.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read", http.StatusInternalServerError)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(body, &req) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		switch req.Method {
		case "initialize":
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"protocolVersion": "2099-01-01",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "from-the-future", "version": "1"},
			}})
		default: // server/discover and anything else
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{
				"code": -32601, "message": "method not found",
			}})
		}
	}))
	defer ts.Close()

	conn, err := mcp.Connect(context.Background(), mcp.Config{URL: ts.URL, HTTPClient: loopbackClient()})
	if err == nil {
		conn.Close()
		t.Fatal("Connect accepted a protocol version no revision defines")
	}
	if !strings.Contains(err.Error(), ts.URL) {
		t.Errorf("error %q does not name the server URL", err)
	}
}

func TestBearerIsScopedToSchemeAsWellAsHost(t *testing.T) {
	t.Parallel()
	// The origin check is two comparisons and both are load-bearing. The host
	// half is what the redirect test covers; this covers the scheme half, whose
	// failure mode is worse — an https endpoint answering with a redirect to
	// http on the same host would put the credential on the wire in cleartext,
	// and a host-only check would attach it happily.
	for _, tc := range []struct {
		name           string
		endpoint, want string
		attach         bool
	}{
		{name: "same origin", endpoint: "https://mcp.example:8443/rpc", want: "https://mcp.example:8443/rpc", attach: true},
		{name: "host case folds", endpoint: "https://MCP.Example:8443/rpc", want: "https://mcp.example:8443/rpc", attach: true},
		{name: "scheme downgraded on the same host", endpoint: "https://mcp.example:8443/rpc", want: "http://mcp.example:8443/rpc"},
		{name: "scheme upgraded on the same host", endpoint: "http://mcp.example/rpc", want: "https://mcp.example/rpc"},
		{name: "different host", endpoint: "https://mcp.example/rpc", want: "https://evil.example/rpc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := mcp.BearerAttachesForTest(tc.endpoint, tc.want); got != tc.attach {
				verb := "withheld from"
				if got {
					verb = "sent to"
				}
				t.Errorf("a credential scoped to %s was %s %s", tc.endpoint, verb, tc.want)
			}
		})
	}
}

func TestBearerOriginComparison(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		a, b  string
		same  bool
		wrong string
	}{
		{name: "identical", a: "example.com:8080", b: "example.com:8080", same: true},
		{name: "DNS names fold", a: "Example.COM:8080", b: "example.com:8080", same: true,
			wrong: "a host name's case is not significant, so this withholds the credential from its own server"},
		{name: "different host", a: "example.com:8080", b: "evil.example:8080"},
		{name: "explicit port differs textually", a: "example.com", b: "example.com:443",
			wrong: "fails closed: the credential is withheld and the request 401s rather than leaking"},

		// A zone identifier names a local interface, and two interfaces can
		// differ only in case. Folding it would call two different scoped
		// addresses the same origin — the one direction that leaks.
		{name: "same zone", a: "[fe80::1%eth0]:8080", b: "[fe80::1%eth0]:8080", same: true},
		{name: "zone differing only in case", a: "[fe80::1%eth0]:8080", b: "[fe80::1%ETH0]:8080"},
		{name: "zone against no zone", a: "[fe80::1%eth0]:8080", b: "[fe80::1]:8080"},
		{name: "address folds, zone does not", a: "[FE80::1%eth0]:8080", b: "[fe80::1%eth0]:8080", same: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := mcp.SameHostForTest(tc.a, tc.b); got != tc.same {
				t.Errorf("sameHost(%q, %q) = %v, want %v — %s", tc.a, tc.b, got, tc.same, tc.wrong)
			}
		})
	}
}

func TestListToolsOnAConnectionThatWasNeverOpened(t *testing.T) {
	t.Parallel()
	// Conn is exported and its zero value is constructible, so this package's
	// own misuse reaches the same code path as a server response. It must not
	// come back as "the client library panicked on this server's response",
	// which would send someone debugging a nil pointer to look at a server.
	tools, err := new(mcp.Conn).ListTools(context.Background())
	if err == nil {
		t.Fatalf("ListTools on a zero Conn returned %d tools, want an error", len(tools))
	}
	if strings.Contains(err.Error(), "panicked") {
		t.Errorf("error %q blames the server for this package's own state", err)
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
		// Not tolerated: the fixture hands every non-listing request to a real
		// SDK server, so Connect must succeed. It never issues tools/list — the
		// SDK's only toolsCache.put is inside ListTools itself — so a Connect
		// error here means the listing was never reached and the assertion below
		// would pass without testing anything.
		t.Fatalf("Connect: %v (the fixture only fails tools/list)", err)
	}
	defer conn.Close()
	if tools, err := conn.ListTools(context.Background()); err == nil {
		t.Fatalf("ListTools returned %d tools against a failing server, want an error — "+
			"an empty catalog is recorded as \"this server has no tools\"", len(tools))
	}
}

func TestResponseBudgetAllowsExactlyTheLimit(t *testing.T) {
	t.Parallel()
	// The bound must mean "at most MaxResponseBytes", not "fewer than" and not
	// "one more than". Whether a body is accepted must also not depend on how
	// the server chunked it: a reader may hand back its final bytes with io.EOF
	// attached or return them and only report EOF on the read after, and both
	// are legal. bytes.Reader only ever does the second, so testing against it
	// alone proved nothing about the first — and net/http does the first, which
	// makes it the shape that matters in production rather than the exotic one.
	// Both are exercised here, at four read sizes.
	const limit = 64
	for _, shape := range []struct {
		name string
		open func(int) io.ReadCloser
	}{
		{name: "EOF on the read after the last bytes", open: func(n int) io.ReadCloser {
			return io.NopCloser(bytes.NewReader(make([]byte, n)))
		}},
		{name: "EOF attached to the last bytes", open: func(n int) io.ReadCloser {
			return io.NopCloser(&eofAttachingReader{left: n})
		}},
	} {
		for _, bufSize := range []int{1, 7, limit, limit * 2} {
			for _, size := range []int{limit - 1, limit, limit + 1} {
				body, _ := mcp.LimitedBodyForTest(shape.open(size), limit)
				got, err := readAllWith(body, bufSize)
				switch {
				case size <= limit && err != nil:
					t.Errorf("%s: body of %d read %d at a time: %v, want it accepted at a %d limit",
						shape.name, size, bufSize, err, limit)
				case size <= limit && got != size:
					t.Errorf("%s: body of %d read %d at a time: got %d bytes, want %d",
						shape.name, size, bufSize, got, size)
				case size > limit && err == nil:
					t.Errorf("%s: body of %d read %d at a time was accepted at a %d limit",
						shape.name, size, bufSize, limit)
				case size > limit && got > limit:
					t.Errorf("%s: body of %d read %d at a time delivered %d bytes past a %d limit",
						shape.name, size, bufSize, got, limit)
				}
			}
		}
	}
}

// eofAttachingReader returns io.EOF alongside its final bytes rather than on the
// read after them — what net/http does, and what bytes.Reader never does.
type eofAttachingReader struct{ left int }

func (r *eofAttachingReader) Read(p []byte) (int, error) {
	if r.left == 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > r.left {
		n = r.left
	}
	r.left -= n
	if r.left == 0 {
		return n, io.EOF
	}
	return n, nil
}

func TestResponseBudgetIgnoresAZeroLengthRead(t *testing.T) {
	t.Parallel()
	// A read asking for nothing must not consult the budget. take(0) returns 0,
	// which is indistinguishable from an exhausted budget, so without the guard
	// a zero-length read latches the body refused for good — and io.Copy and
	// friends are entitled to make one.
	body, _ := mcp.LimitedBodyForTest(io.NopCloser(strings.NewReader("hello")), 64)
	if n, err := body.Read(nil); n != 0 || err != nil {
		t.Fatalf("Read(nil) = (%d, %v), want (0, nil)", n, err)
	}
	got, err := io.ReadAll(body)
	if err != nil || string(got) != "hello" {
		t.Errorf("after a zero-length read: %q, %v; want \"hello\" and no error", got, err)
	}
}

func TestResponseBudgetToleratesAProbeThatYieldsNothing(t *testing.T) {
	t.Parallel()
	// io.Reader may legally return (0, nil) — "nothing yet, ask again". The
	// probe that decides whether a body ended on the budget or ran past it must
	// read that as neither answer and let the caller retry, rather than as EOF
	// (accepting a body that may still have bytes) or as a byte (refusing one
	// that does not).
	body, _ := mcp.LimitedBodyForTest(io.NopCloser(&stallingReader{left: 8}), 8)
	got, err := readAllWith(body, 4)
	if err != nil || got != 8 {
		t.Errorf("body of 8 at an 8-byte limit: got %d bytes, %v; want 8 and no error", got, err)
	}
}

// stallingReader delivers its bytes, then returns (0, nil) once before EOF.
type stallingReader struct {
	left    int
	stalled bool
}

func (r *stallingReader) Read(p []byte) (int, error) {
	if r.left > 0 {
		n := len(p)
		if n > r.left {
			n = r.left
		}
		r.left -= n
		return n, nil
	}
	if !r.stalled {
		r.stalled = true
		return 0, nil
	}
	return 0, io.EOF
}

func TestResponseBudgetStaysRefusedOnceExceeded(t *testing.T) {
	t.Parallel()
	// The refusal is sticky. A caller that reads past the error must not see the
	// budget reopen for bytes the probe has already proven are over the limit.
	//
	// The body is exactly limit+1, and that is the whole point of the fixture
	// rather than an arbitrary size. With a longer body every read past the
	// refusal finds another byte and errors again, so the latch is invisible and
	// a test built on one passes whether the latch is there or not. At limit+1
	// the probe consumed the only excess byte, so without the latch the next
	// read reaches EOF and the refusal turns back into a clean end of body —
	// which is precisely what this test says cannot happen.
	const limit = 4
	body, _ := mcp.LimitedBodyForTest(io.NopCloser(strings.NewReader("01234")), limit)
	if _, err := readAllWith(body, 3); err == nil {
		t.Fatalf("a %d-byte body was accepted at a %d-byte limit", limit+1, limit)
	}
	// Assert the refusal specifically, not merely "an error". Without the latch
	// this read returns io.EOF — the probe already consumed the excess byte, so
	// the body really is finished — and a test content with any non-nil error
	// would call that a pass while the refusal had silently turned into a clean
	// end of body, which is the failure it exists to catch.
	n, err := body.Read(make([]byte, 3))
	if err == nil || !strings.Contains(err.Error(), "exceed") {
		t.Errorf("reading again after the refusal returned (%d, %v), want the refusal again", n, err)
	}
}

func TestTheFallbackDeadlineOutlivesTheRoundTrip(t *testing.T) {
	t.Parallel()
	// The deadline the transport supplies has to survive past RoundTrip, because
	// the body is streamed after it returns. Cancelling on return instead — the
	// obvious shape — passes against any fixture whose body arrives whole inside
	// the round trip, and fails against a server that streams, which is every
	// real one. So this fixture deliberately streams: headers and a first chunk,
	// then a pause, then the rest.
	const first, second = "the first chunk", "and the rest of it"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("fixture cannot stream")
			return
		}
		_, _ = io.WriteString(w, first)
		fl.Flush()
		time.Sleep(40 * time.Millisecond)
		_, _ = io.WriteString(w, second)
	}))
	defer ts.Close()

	// context.Background() carries no deadline, so the transport supplies one —
	// the same shape as go-sdk's detached teardown context.
	body, err := mcp.RoundTripBodyForTest(context.Background(), ts.URL, 1<<20)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer body.Close()
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("reading a streamed body after RoundTrip returned: %v "+
			"(a deadline cancelled when RoundTrip returned would read exactly like this)", err)
	}
	if string(got) != first+second {
		t.Errorf("body = %q, want %q", got, first+second)
	}
}

func TestDetachedRequestsGetADeadlineOfTheirOwn(t *testing.T) {
	t.Parallel()
	// go-sdk detaches a connection's lifecycle context deliberately
	// (xcontext.Detach: no deadline, nil Done) and sends the session-ending
	// DELETE on it, so neither the caller's context nor ListTimeout can bound
	// that request. http.Client.Timeout would, but it belongs to the caller and
	// a supplied client may leave it zero — so the transport supplies a floor.
	if left, had := mcp.RequestDeadlineForTest(context.Background()); !had {
		t.Error("a request with no deadline reached the round-tripper still without one")
	} else if left <= 0 || left > mcp.DialTimeout {
		t.Errorf("fallback deadline left %v, want (0, %v]", left, mcp.DialTimeout)
	}

	// And it is a floor, not a cap: a request that brought its own deadline
	// keeps it, so this never shortens ListTimeout.
	own := mcp.DialTimeout + 10*time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), own)
	defer cancel()
	left, had := mcp.RequestDeadlineForTest(ctx)
	if !had {
		t.Fatal("a request that carried a deadline arrived without one")
	}
	if left <= mcp.DialTimeout {
		t.Errorf("a request carrying a %v deadline was cut to %v", own, left)
	}
}

func TestResponseBudgetIsSharedAcrossResponses(t *testing.T) {
	t.Parallel()
	// A per-response cap does not bound a listing: maxToolPages responses of
	// MaxResponseBytes each is 800 MiB, and both the SDK's per-cursor cache and
	// this package's own result hold it. So the budget is one counter for the
	// connection, and a later body draws on what the earlier ones left.
	const limit = 100
	first, budget := mcp.LimitedBodyForTest(io.NopCloser(bytes.NewReader(make([]byte, 60))), limit)
	if n, err := readAllWith(first, 16); err != nil || n != 60 {
		t.Fatalf("first body: got %d bytes, %v; want 60 and no error", n, err)
	}
	second := budget.Wrap(io.NopCloser(bytes.NewReader(make([]byte, 40))))
	if n, err := readAllWith(second, 16); err != nil || n != 40 {
		t.Fatalf("second body: got %d bytes, %v; want 40 and no error", n, err)
	}
	third := budget.Wrap(io.NopCloser(bytes.NewReader(make([]byte, 1))))
	if _, err := readAllWith(third, 16); err == nil {
		t.Error("a third body was accepted after the connection's budget was spent")
	}
}

// readAllWith drains r through a buffer of exactly bufSize, so a test can choose
// where the read boundaries land relative to the budget.
func readAllWith(r io.Reader, bufSize int) (int, error) {
	buf := make([]byte, bufSize)
	total := 0
	for {
		n, err := r.Read(buf)
		total += n
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

func TestListToolsRefusesPagesThatExceedTheBudgetTogether(t *testing.T) {
	t.Parallel()
	// Each page here is comfortably under MaxResponseBytes; together they are
	// not. A per-response cap accepts every one of them.
	const perPage = 5 << 20
	pages := &atomic.Int32{}
	url, _ := serveToolsList(t, func(string) map[string]any {
		n := pages.Add(1)
		return map[string]any{
			"tools": []any{map[string]any{
				"name":        fmt.Sprintf("t%d", n),
				"description": strings.Repeat("d", perPage),
				"inputSchema": map[string]any{"type": "object"},
			}},
			"nextCursor": fmt.Sprintf("c%d", n),
		}
	})

	conn, err := mcp.Connect(context.Background(), mcp.Config{URL: url, HTTPClient: loopbackClient()})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()

	tools, err := conn.ListTools(context.Background())
	if err == nil {
		t.Fatalf("ListTools accepted %d tools over %d pages of %d bytes, want a refusal past %d in total",
			len(tools), pages.Load(), perPage, mcp.MaxResponseBytes)
	}
	if !strings.Contains(err.Error(), "in total") {
		t.Errorf("error %q does not report the cumulative bound", err)
	}
	if got := pages.Load(); got > 3 {
		t.Errorf("server was asked for %d pages; the budget should have stopped it inside the second", got)
	}
}
