package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/mcp"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// legacyRequest is one request a legacy fixture received: the HTTP method, the
// JSON-RPC method a POST carried, whether it was sent on an open session (the
// stream's message endpoint) and its Authorization header.
type legacyRequest struct {
	method, rpc string
	onSession   bool
	auth        string
}

// serveLegacySSE starts a server that hosts only the HTTP+SSE transport of
// 2024-11-05, with one tool named "echo" that answers "legacy:" and its "q"
// argument, and records every request it receives, in order.
//
// The transport is the SDK's own handler, but the SDK's server is a modern one
// and would answer `server/discover` on the stream, which a 2024-11-05 server
// does not know. So the fixture refuses that method as one would, with "method
// not found", and the handshake goes through `initialize` as it did in the
// recording.
//
// probe decides how the server answers the modern client's first POSTs — the
// ones carrying no sessionid. Nil leaves them to the SDK handler, which answers
// 400 "sessionid must be provided" in plain text; the others write a status of
// their own, so the suite covers all three statuses the spec names as the
// signal.
func serveLegacySSE(t *testing.T, probe http.HandlerFunc) (url string, seen func() []legacyRequest) {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "legacy-server", Version: "1"}, nil)
	server.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			if method == "server/discover" {
				return nil, &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "Method not found"}
			}
			return next(ctx, method, req)
		}
	})
	server.AddTool(tool("echo", "echoes its input", "q"),
		func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			var args struct{ Q string }
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return nil, err
			}
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "legacy:" + args.Q}}}, nil
		})
	inner := sdk.NewSSEHandler(func(*http.Request) *sdk.Server { return server }, nil)
	var mu sync.Mutex
	var log []legacyRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read", http.StatusInternalServerError)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var rpc struct{ Method string }
		_ = json.Unmarshal(body, &rpc)
		onSession := r.URL.Query().Get("sessionid") != ""
		mu.Lock()
		log = append(log, legacyRequest{r.Method, rpc.Method, onSession, r.Header.Get("Authorization")})
		mu.Unlock()
		if probe != nil && r.Method == http.MethodPost && !onSession {
			probe(w, r)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts.URL, func() []legacyRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]legacyRequest(nil), log...)
	}
}

// The reference documents the fallback ("servers that only support the
// deprecated SSE transport still work through an automatic fallback") and a
// 2026-09-23 recording shows it: POST answered 405, then a GET that opens the
// stream, then the handshake, the listing and a call over it. The spec names
// three statuses as the signal, so each is driven here.
func TestConnectFallsBackToTheDeprecatedSSETransport(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name  string
		probe http.HandlerFunc
	}{
		{name: "400 from the SDK's own legacy handler"},
		{name: "404 with a plain body", probe: func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "404 page not found", http.StatusNotFound)
		}},
		// The recorded fixture's own answer: a JSON body that is not a JSON-RPC
		// message, which is not "a recognized modern JSON-RPC error" either.
		{name: "405 with a non-JSON-RPC JSON body", probe: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Allow", "GET")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = w.Write([]byte(`{"error":"method not allowed"}`))
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			url, seen := serveLegacySSE(t, row.probe)
			conn, err := mcp.Connect(context.Background(), mcp.Config{
				URL: url, BearerToken: "s3cret", HTTPClient: loopbackClient()})
			if err != nil {
				t.Fatalf("Connect to a legacy-only server: %v", err)
			}
			defer conn.Close()

			tools, err := conn.ListTools(context.Background())
			if err != nil {
				t.Fatalf("ListTools over the legacy transport: %v", err)
			}
			if len(tools) != 1 || tools[0].Name != "echo" {
				t.Fatalf("tools = %+v, want the server's echo", tools)
			}
			res, err := conn.CallTool(context.Background(), "echo", json.RawMessage(`{"q":"hi"}`))
			if err != nil {
				t.Fatalf("CallTool over the legacy transport: %v", err)
			}
			if len(res.Content) != 1 || res.Content[0].Text != "legacy:hi" {
				t.Errorf("content = %+v, want the tool's answer", res.Content)
			}

			got := seen()
			var gets int
			var stream []string
			for i, r := range got {
				if r.method == http.MethodGet {
					gets++
				}
				if r.onSession {
					stream = append(stream, r.rpc)
				}
				// Every request of both attempts carries the credential:
				// the message endpoint is on the server's own origin, which
				// is where the bearer is scoped to.
				if r.auth != "Bearer s3cret" {
					t.Errorf("request %d (%+v) does not carry the bearer", i, r)
				}
			}
			if gets != 1 {
				t.Errorf("server saw %d GET streams, want the one the fallback opens: %+v", gets, got)
			}
			// The handshake on the stream: the SDK's discovery probe, refused
			// as a 2024-11-05 server refuses it, then the legacy initialize.
			want := []string{"server/discover", "initialize", "notifications/initialized", "tools/list", "tools/call"}
			if strings.Join(stream, ",") != strings.Join(want, ",") {
				t.Errorf("messages on the stream = %v, want %v", stream, want)
			}
		})
	}
}

// A JSON-RPC error from a server reached over the deprecated transport is still
// the server's own answer: the POST that carried the call came back 202 and the
// error arrived on the stream, so a caller is told the connection worked.
func TestALegacyServersRefusalIsTheServersAnswer(t *testing.T) {
	t.Parallel()
	url, _ := serveLegacySSE(t, nil)
	res, err := connect(t, url).CallTool(context.Background(), "no_such_tool", json.RawMessage(`{"q":"x"}`))
	if err == nil {
		t.Fatalf("CallTool reported success for a tool the server does not offer: %+v", res)
	}
	if !errors.Is(err, mcp.ErrServerAnswered) {
		t.Errorf("error %q is not marked ErrServerAnswered; the connection was fine", err)
	}
}

// serveCountingStatus answers every POST with status and body, and counts the
// GETs a client sends it.
func serveCountingStatus(t *testing.T, status int, body func(http.ResponseWriter, *http.Request)) (url string, gets *atomic.Int32) {
	t.Helper()
	gets = &atomic.Int32{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets.Add(1)
			http.Error(w, "no stream here", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != nil {
			body(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts.URL, gets
}

// The signal is a status *and* a body. A modern server answers 400 with a
// JSON-RPC error for an unsupported version or a malformed header, and the
// spec's answer to that is to correct the request rather than fall back — so
// no GET goes out.
func TestConnectDoesNotFallBackOnAModernJSONRPCError(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Parallel()
			url, gets := serveCountingStatus(t, status, func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					ID json.RawMessage `json:"id"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				jsonRPCError(w, req.ID)
			})
			conn, err := mcp.Connect(context.Background(), mcp.Config{URL: url, HTTPClient: loopbackClient()})
			if err == nil {
				_ = conn.Close()
				t.Fatal("Connect succeeded against a server that refused every request")
			}
			if n := gets.Load(); n != 0 {
				t.Errorf("a %d carrying a JSON-RPC error opened %d GET streams, want none", status, n)
			}
			// Reading the body to decide must leave it for the SDK, which
			// decodes a 400's or a 405's JSON-RPC error into the message (a
			// 404's it never reads).
			if status != http.StatusNotFound && !strings.Contains(err.Error(), "policy says no") {
				t.Errorf("the server's JSON-RPC error did not reach the SDK: %v", err)
			}
		})
	}
}

// Only the latest POST signals. A refused server/discover is how an older
// server that hosts the modern endpoint answers too, so when the initialize
// after it fails for some other reason, that reason is the answer — here a
// refused credential, which must not turn into a GET to the same gate.
func TestARefusedProbeDoesNotSignalForTheRequestAfterIt(t *testing.T) {
	t.Parallel()
	var gets atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets.Add(1)
			http.Error(w, "nope", http.StatusUnauthorized)
			return
		}
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "server/discover" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer ts.Close()
	conn, err := mcp.Connect(context.Background(), mcp.Config{
		URL: ts.URL, BearerToken: "tok", HTTPClient: loopbackClient()})
	if err == nil {
		_ = conn.Close()
		t.Fatal("Connect succeeded against a server that refused the credential")
	}
	if n := gets.Load(); n != 0 {
		t.Errorf("a refused probe followed by a 401 opened %d GET streams, want none", n)
	}
	if !errors.Is(err, mcp.ErrUnauthorized) {
		t.Errorf("a 401 on initialize was not marked a refused credential: %v", err)
	}
}

// Any other status is not the signal at all. 401 in particular stays a refused
// credential, rather than becoming whatever a GET to the same gate says.
func TestConnectDoesNotFallBackOnAnyOtherStatus(t *testing.T) {
	t.Parallel()
	for _, status := range []int{
		http.StatusUnauthorized, http.StatusForbidden, http.StatusNotAcceptable,
		http.StatusUnsupportedMediaType, http.StatusInternalServerError, http.StatusBadGateway,
	} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Parallel()
			url, gets := serveCountingStatus(t, status, nil)
			conn, err := mcp.Connect(context.Background(), mcp.Config{
				URL: url, BearerToken: "tok", HTTPClient: loopbackClient()})
			if err == nil {
				_ = conn.Close()
				t.Fatal("Connect succeeded against a server that refused every request")
			}
			if n := gets.Load(); n != 0 {
				t.Errorf("a %d opened %d GET streams, want none", status, n)
			}
			if got, want := errors.Is(err, mcp.ErrUnauthorized), status == http.StatusUnauthorized; got != want {
				t.Errorf("errors.Is(err, ErrUnauthorized) = %v, want %v (err: %v)", got, want, err)
			}
		})
	}
}

// A server that refuses the stream's credential is a refused credential like any
// other, whichever transport's request it refused.
func TestARefusedLegacyStreamIsARefusedCredential(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.Error(w, "nope", http.StatusUnauthorized)
			return
		}
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}))
	defer ts.Close()
	conn, err := mcp.Connect(context.Background(), mcp.Config{
		URL: ts.URL, BearerToken: "tok", HTTPClient: loopbackClient()})
	if err == nil {
		_ = conn.Close()
		t.Fatal("Connect succeeded against a server that refused the stream")
	}
	if !errors.Is(err, mcp.ErrUnauthorized) {
		t.Errorf("a 401 on the legacy stream was not marked a refused credential: %v", err)
	}
}

// A refusal whose body never arrives whole is a failure to report, not a reason
// to make another request: what it would have said is unknown.
func TestARefusalWhoseBodyCannotBeReadDoesNotSignal(t *testing.T) {
	t.Parallel()
	var gets atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets.Add(1)
			http.Error(w, "no stream here", http.StatusNotFound)
			return
		}
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 100\r\n\r\npartial")
		_ = buf.Flush()
	}))
	defer ts.Close()
	conn, err := mcp.Connect(context.Background(), mcp.Config{URL: ts.URL, HTTPClient: loopbackClient()})
	if err == nil {
		_ = conn.Close()
		t.Fatal("Connect succeeded against a server that cut every answer short")
	}
	if n := gets.Load(); n != 0 {
		t.Errorf("a truncated refusal opened %d GET streams, want none", n)
	}
}

// When both transports fail, the error says so: an operator reading "Not
// Found" alone would not know the deprecated transport was tried too. It still
// names the server by scheme and host and nothing more.
func TestAFailedFallbackReportsBothAttempts(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no MCP here", http.StatusNotFound)
	}))
	defer ts.Close()
	_, err := mcp.Connect(context.Background(), mcp.Config{
		URL: ts.URL + "/mcp?api_key=SECRET-QUERY", HTTPClient: loopbackClient()})
	if err == nil {
		t.Fatal("Connect succeeded against a server with no MCP endpoint")
	}
	msg := err.Error()
	if !strings.Contains(msg, "mcp: connect to "+ts.URL+": ") {
		t.Errorf("the error no longer names the server it failed against: %v", err)
	}
	if !strings.Contains(msg, "HTTP+SSE") {
		t.Errorf("the error does not say the deprecated transport was tried: %v", err)
	}
	if strings.Contains(msg, "SECRET-QUERY") {
		t.Errorf("the error quotes the endpoint's query: %v", err)
	}
}

// The deprecated transport lets the server name where every later request goes:
// the stream's first event carries the message endpoint, and the SDK resolves
// it against the server URL, so an absolute URL may name any host at all. The
// client sends nothing to another origin — the server URL is the only
// destination anyone configured.
func TestTheFallbackRefusesAMessageEndpointOnAnotherOrigin(t *testing.T) {
	t.Parallel()
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhere.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer other.Close()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: endpoint\ndata: %s/messages?sessionid=x\n\n", other.URL)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer ts.Close()

	conn, err := mcp.Connect(context.Background(), mcp.Config{
		URL: ts.URL, BearerToken: "tok", HTTPClient: loopbackClient()})
	if err == nil {
		_ = conn.Close()
		t.Fatal("Connect succeeded through a message endpoint on another origin")
	}
	if n := elsewhere.Load(); n != 0 {
		t.Errorf("the other origin received %d requests, want none", n)
	}
}

// Only the host half of that rule is reachable with two loopback servers; the
// scheme half — an https server naming an http endpoint on its own host, which
// would put the session's messages on the wire in cleartext — needs the
// origins named directly.
func TestTheLegacyOriginComparison(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		endpoint, target string
		allowed          bool
	}{
		{"https://mcp.example.com/sse", "https://mcp.example.com/messages?sessionId=1", true},
		{"https://mcp.example.com/sse", "https://MCP.example.com/messages", true},
		{"https://mcp.example.com/sse", "http://mcp.example.com/messages", false},
		{"https://mcp.example.com/sse", "https://other.example.com/messages", false},
		{"https://mcp.example.com/sse", "https://mcp.example.com:8443/messages", false},
		// Fails closed: the same origin spelled two ways is refused rather than
		// normalized, the direction bearer scoping errs in too.
		{"https://mcp.example.com/sse", "https://mcp.example.com:443/messages", false},
	} {
		if got := mcp.LegacyRequestAllowedForTest(row.endpoint, row.target); got != row.allowed {
			t.Errorf("endpoint %s, request %s: allowed = %v, want %v", row.endpoint, row.target, got, row.allowed)
		}
	}
}
