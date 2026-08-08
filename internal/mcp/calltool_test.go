package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/trace"
)

// serveTool starts a server with one real SDK-hosted tool named "echo", whose
// handler is the test's. Its schema declares a required "q" so a call that
// reaches the server is one the server itself agreed to run.
func serveTool(t *testing.T, handle sdk.ToolHandler) string {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "call-server", Version: "1"}, nil)
	server.AddTool(tool("echo", "echoes its input", "q"), handle)
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts.URL
}

// serveToolCall starts a server whose handshake is a real SDK server's but
// whose tools/call result is written by hand, and records the params of the
// last call it answered.
//
// The suite runs against SDK servers wherever it can, for the reason serveMCP
// gives; this is the same deliberate exception serveToolsList is. What these
// tests are about is what a server that is not this SDK can put in a result —
// an input-required answer, a structured-only answer, a block type a tool
// result has no business carrying — and an SDK server produces none of it.
func serveToolCall(t *testing.T, result func(params json.RawMessage) map[string]any) (url string, seen *atomic.Pointer[json.RawMessage]) {
	t.Helper()
	seen = &atomic.Pointer[json.RawMessage]{}
	inner := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server {
		return sdk.NewServer(&sdk.Implementation{Name: "raw-call-server", Version: "1"}, nil)
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
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(body, &req) != nil || req.Method != "tools/call" {
			inner.ServeHTTP(w, r)
			return
		}
		params := req.Params
		seen.Store(&params)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  result(req.Params),
		})
	}))
	t.Cleanup(ts.Close)
	return ts.URL, seen
}

func connect(t *testing.T, url string) *mcp.Conn {
	t.Helper()
	conn, err := mcp.Connect(context.Background(), mcp.Config{URL: url, HTTPClient: loopbackClient()})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestCallToolCarriesEveryBlockKindAToolResultAdmits pins the translation from
// the SDK's content interface to this package's flat block. All five types the
// protocol admits in a tool result are here, including both embedded-resource
// shapes, because each fills a different subset of the struct and a converter
// that dropped one field would still pass a test carrying only text.
func TestCallToolCarriesEveryBlockKindAToolResultAdmits(t *testing.T) {
	t.Parallel()
	url := serveTool(t, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{
			&sdk.TextContent{Text: "the answer"},
			&sdk.ImageContent{Data: []byte{0x89, 'P', 'N', 'G'}, MIMEType: "image/png"},
			&sdk.AudioContent{Data: []byte{'R', 'I', 'F', 'F'}, MIMEType: "audio/wav"},
			&sdk.EmbeddedResource{Resource: &sdk.ResourceContents{
				URI: "file:///notes.txt", MIMEType: "text/plain", Text: "a text resource",
			}},
			&sdk.EmbeddedResource{Resource: &sdk.ResourceContents{
				URI: "file:///blob.bin", MIMEType: "application/octet-stream", Blob: []byte{0x00, 0xff},
			}},
			&sdk.ResourceLink{URI: "https://example.test/doc", MIMEType: "text/html"},
		}}, nil
	})

	res, err := connect(t, url).CallTool(context.Background(), "echo", json.RawMessage(`{"q":"x"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Error("IsError set on a result the server did not mark as one")
	}
	want := []mcp.Content{
		{Type: "text", Text: "the answer"},
		{Type: "image", Data: []byte{0x89, 'P', 'N', 'G'}, MIMEType: "image/png"},
		{Type: "audio", Data: []byte{'R', 'I', 'F', 'F'}, MIMEType: "audio/wav"},
		{Type: "resource", URI: "file:///notes.txt", MIMEType: "text/plain", Text: "a text resource"},
		{Type: "resource", URI: "file:///blob.bin", MIMEType: "application/octet-stream", Data: []byte{0x00, 0xff}},
		{Type: "resource_link", URI: "https://example.test/doc", MIMEType: "text/html"},
	}
	if len(res.Content) != len(want) {
		t.Fatalf("content = %+v, want %d blocks", res.Content, len(want))
	}
	for i, w := range want {
		if got := res.Content[i]; got.Type != w.Type || got.Text != w.Text ||
			got.MIMEType != w.MIMEType || got.URI != w.URI || !bytes.Equal(got.Data, w.Data) {
			t.Errorf("block %d = %+v, want %+v", i, got, w)
		}
	}
}

// TestCallToolReportsAFailedToolAsAResultNotAnError pins the line MCP draws and
// this package keeps: a tool that ran and failed is the model's to read and
// retry, so it comes back as a result with IsError, never as a Go error. The
// error text rides the content, which is where the model can see it.
func TestCallToolReportsAFailedToolAsAResultNotAnError(t *testing.T) {
	t.Parallel()
	url := serveTool(t, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{
			IsError: true,
			Content: []sdk.Content{&sdk.TextContent{Text: "the repository does not exist"}},
		}, nil
	})

	res, err := connect(t, url).CallTool(context.Background(), "echo", json.RawMessage(`{"q":"x"}`))
	if err != nil {
		t.Fatalf("CallTool turned a failed tool into a transport failure: %v", err)
	}
	if !res.IsError {
		t.Error("IsError not set on a result the server marked as an error")
	}
	if len(res.Content) != 1 || res.Content[0].Text != "the repository does not exist" {
		t.Errorf("content = %+v, want the tool's error text", res.Content)
	}
}

// TestCallToolReportsAProtocolFailureAsAnError is the other half of that line.
// A tool the server does not have is not a tool that failed — nothing ran — so
// the server answers with a JSON-RPC error and the platform, not the model, is
// the one told.
func TestCallToolReportsAProtocolFailureAsAnError(t *testing.T) {
	t.Parallel()
	url := serveTool(t, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{}, nil
	})

	res, err := connect(t, url).CallTool(context.Background(), "no_such_tool", json.RawMessage(`{"q":"x"}`))
	if err == nil {
		t.Fatalf("CallTool reported success for a tool the server does not offer: %+v", res)
	}
	if !strings.Contains(err.Error(), "no_such_tool") {
		t.Errorf("error %q does not name the tool", err)
	}
}

// TestCallToolSendsTheModelsArgumentBytesUnaltered pins that the arguments
// reach the server as written. A map round trip would sort the keys and render
// 1e3 as 1000 — both legal JSON and neither what the model sent.
func TestCallToolSendsTheModelsArgumentBytesUnaltered(t *testing.T) {
	t.Parallel()
	url, seen := serveToolCall(t, func(json.RawMessage) map[string]any {
		return map[string]any{"content": []any{}}
	})

	const args = `{"zebra":1e3,"alpha":"a"}`
	if _, err := connect(t, url).CallTool(context.Background(), "echo", json.RawMessage(args)); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	params := seen.Load()
	if params == nil {
		t.Fatal("the server was never asked to run the tool")
	}
	var got struct {
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(*params, &got); err != nil {
		t.Fatalf("params %s: %v", *params, err)
	}
	if string(got.Arguments) != args {
		t.Errorf("arguments on the wire = %s, want %s", got.Arguments, args)
	}
}

// TestCallToolIsBoundedAgainstAServerThatKeepsAskingForInput pins what a
// multi round-trip answer costs and how it ends (MCP 2026-07-28, SEP-2322).
//
// A server may answer `resultType: "input_required"` rather than run the tool,
// asking for input to be supplied and the call retried. go-sdk v1.7.0 answers
// those requests in its own client middleware and re-sends the call, so a server
// that never stops asking is a loop the caller cannot see — and the two things
// worth pinning are that it terminates, and that it terminates as a failure. An
// empty answer instead would reach the model as a tool that returned nothing,
// with nothing to say why.
//
// The request is a `roots/list`, one of the three methods the SDK decodes (with
// `elicitation/create` and `sampling/createMessage`); it refuses any other while
// decoding, which is a fine outcome but a different one, and a fixture built on
// an invented method would be testing that refusal instead of this bound.
func TestCallToolIsBoundedAgainstAServerThatKeepsAskingForInput(t *testing.T) {
	t.Parallel()
	asked := &atomic.Int32{}
	url, _ := serveToolCall(t, func(json.RawMessage) map[string]any {
		asked.Add(1)
		return map[string]any{
			"content":    []any{},
			"resultType": "input_required",
			"inputRequests": map[string]any{
				"r1": map[string]any{"method": "roots/list", "params": map[string]any{}},
			},
			"requestState": "opaque",
		}
	})

	res, err := connect(t, url).CallTool(context.Background(), "echo", json.RawMessage(`{"q":"x"}`))
	if err == nil {
		t.Fatalf("CallTool read an input-required answer as a completed call: %+v", res)
	}
	// The bound is the SDK's (maxMultiRoundTripRetries, 10). Asserting a
	// ceiling rather than the number keeps this a test of "it stops" — but a
	// ceiling alone would also pass if the call never retried at all, so the
	// floor is here too.
	if got := asked.Load(); got < 2 || got > 10 {
		t.Errorf("the server was asked %d times, want a bounded retry between 2 and 10", got)
	}
}

// TestCallToolFallsBackToTheStructuredAnswer covers the server that takes the
// spec at its word: structuredContent is what a tool with an outputSchema
// returns, and duplicating it as text is only a SHOULD. Without the fallback the
// model receives a result with no content at all.
func TestCallToolFallsBackToTheStructuredAnswer(t *testing.T) {
	t.Parallel()
	url, _ := serveToolCall(t, func(json.RawMessage) map[string]any {
		return map[string]any{
			"content":           []any{},
			"structuredContent": map[string]any{"stars": 42},
		}
	})

	res, err := connect(t, url).CallTool(context.Background(), "echo", json.RawMessage(`{"q":"x"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(res.Content) != 1 || res.Content[0].Type != "text" || res.Content[0].Text != `{"stars":42}` {
		t.Errorf("content = %+v, want the structured answer as one text block", res.Content)
	}
}

// TestCallToolKeepsTheServersOwnTextOverTheStructuredAnswer is the other side of
// that fallback: a server that follows the recommendation sends both, and
// appending the structured value as well would show the model the same answer
// twice.
//
// The server's text deliberately is not the structured value's JSON. The
// recommendation is that a server send both, not that the text be a rendering
// of the structure — and with the two identical, a fallback that fired
// unconditionally and *replaced* the text would be indistinguishable from one
// that never fired at all.
func TestCallToolKeepsTheServersOwnTextOverTheStructuredAnswer(t *testing.T) {
	t.Parallel()
	url, _ := serveToolCall(t, func(json.RawMessage) map[string]any {
		return map[string]any{
			"content":           []any{map[string]any{"type": "text", "text": "42 stars"}},
			"structuredContent": map[string]any{"stars": 42},
		}
	})

	res, err := connect(t, url).CallTool(context.Background(), "echo", json.RawMessage(`{"q":"x"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("content = %+v, want the server's one block and no duplicate", res.Content)
	}
	if got := res.Content[0]; got.Type != "text" || got.Text != "42 stars" {
		t.Errorf("content = %+v, want the server's own text rather than the structured value", got)
	}
}

// TestCallToolDropsBlocksAToolResultCannotCarry covers the two content types the
// SDK's decoder accepts here but the protocol admits only in sampling messages.
// They reach the client because CallToolResult decodes its content with no
// allow-list at all (protocol.go, contentsFromWire(_, nil)); this platform has
// nowhere to put them, and a block guessed into a text answer would be a
// fabrication. The blocks around them must survive.
func TestCallToolDropsBlocksAToolResultCannotCarry(t *testing.T) {
	t.Parallel()
	url, _ := serveToolCall(t, func(json.RawMessage) map[string]any {
		return map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "before"},
			map[string]any{"type": "tool_use", "id": "t1", "name": "nested"},
			// An embedded resource with no resource: the block is the
			// resource, so there is nothing to carry on.
			map[string]any{"type": "resource"},
			map[string]any{"type": "text", "text": "after"},
		}}
	})

	res, err := connect(t, url).CallTool(context.Background(), "echo", json.RawMessage(`{"q":"x"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(res.Content) != 2 || res.Content[0].Text != "before" || res.Content[1].Text != "after" {
		t.Errorf("content = %+v, want the two text blocks alone", res.Content)
	}
}

// TestCallToolFailsWhenEveryBlockOfTheAnswerIsDropped is the case dropping
// blocks quietly cannot cover: with nothing left, the caller cannot tell an
// answer this platform could not read from a tool that legitimately returned
// nothing, and only one of those two is something the model should act on.
func TestCallToolFailsWhenEveryBlockOfTheAnswerIsDropped(t *testing.T) {
	t.Parallel()
	url, _ := serveToolCall(t, func(json.RawMessage) map[string]any {
		return map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "nested"},
			map[string]any{"type": "resource"},
		}}
	})

	res, err := connect(t, url).CallTool(context.Background(), "echo", json.RawMessage(`{"q":"x"}`))
	if err == nil {
		t.Fatalf("CallTool reported success for an answer it could not read: %+v", res)
	}
	if !strings.Contains(err.Error(), "cannot carry") {
		t.Errorf("error %q does not say the blocks were untranslatable", err)
	}
}

// TestCallToolStillSucceedsWhenATrulyEmptyAnswerComesBack is the other side of
// that line, and the reason the check counts the server's blocks rather than
// this package's: a tool is allowed to return nothing, and a call that failed
// whenever Content came back empty would fail every one of those.
func TestCallToolStillSucceedsWhenATrulyEmptyAnswerComesBack(t *testing.T) {
	t.Parallel()
	url, _ := serveToolCall(t, func(json.RawMessage) map[string]any {
		return map[string]any{"content": []any{}}
	})

	res, err := connect(t, url).CallTool(context.Background(), "echo", json.RawMessage(`{"q":"x"}`))
	if err != nil {
		t.Fatalf("CallTool failed a tool that returned nothing: %v", err)
	}
	if len(res.Content) != 0 || res.IsError {
		t.Errorf("result = %+v, want an empty successful answer", res)
	}
}

// TestCallToolRefusesAnInputRequiredAnswerItCannotFulfil pins the one multi
// round-trip shape that reaches this package. The SDK's client middleware
// drives its retry loop off a non-nil `inputRequests` map, so an answer that
// omits the key entirely is handed back untouched — carrying no output, from a
// tool that never ran. Left alone it is a successful empty result, which is the
// one reading of it the model can neither detect nor recover from.
func TestCallToolRefusesAnInputRequiredAnswerItCannotFulfil(t *testing.T) {
	t.Parallel()
	url, _ := serveToolCall(t, func(json.RawMessage) map[string]any {
		return map[string]any{
			"content":      []any{},
			"resultType":   "input_required",
			"requestState": "opaque",
		}
	})

	res, err := connect(t, url).CallTool(context.Background(), "echo", json.RawMessage(`{"q":"x"}`))
	if err == nil {
		t.Fatalf("CallTool reported success for a tool that never ran: %+v", res)
	}
	if !strings.Contains(err.Error(), "further input") {
		t.Errorf("error %q does not say why the call could not complete", err)
	}
}

// TestCallToolOnAConnectionThatWasNeverOpened pins that this package's own
// misuse is reported as such rather than as a nil dereference in an executor
// shared by every session on the host.
func TestCallToolOnAConnectionThatWasNeverOpened(t *testing.T) {
	t.Parallel()
	var conn *mcp.Conn
	if _, err := conn.CallTool(context.Background(), "echo", nil); err == nil {
		t.Fatal("CallTool on a nil connection reported success")
	}
	if _, err := new(mcp.Conn).CallTool(context.Background(), "echo", nil); err == nil {
		t.Fatal("CallTool on an unopened connection reported success")
	}
}

// TestConnectFailureRedactsCredentialsInTheURL pins that a failed connection
// does not carry the endpoint's password. An `mcp_servers` entry's url is
// customer-supplied and may hold userinfo, and the executor's discovery pass
// stores this error text in a database column — so the wrapper naming the
// endpoint is the one place a secret could enter it. net/http already redacts
// its own half of the message; this is ours.
//
// Both halves are asserted: a message that dropped the endpoint entirely would
// pass a check for the password's absence while leaving an operator unable to
// tell which server failed. The second assertion is on this package's own
// prefix rather than on the host appearing somewhere in the string, because the
// SDK error being wrapped names the host too — a substring check would be
// satisfied by that alone and would say nothing about what this package wrote.
func TestConnectFailureRedactsCredentialsInTheURL(t *testing.T) {
	t.Parallel()
	// Port 1 on loopback: nothing listens, so the connection is refused and the
	// error is the one a discovery pass would record.
	const raw = "http://alice:s3cr3t-token@127.0.0.1:1/mcp"
	_, err := mcp.Connect(context.Background(), mcp.Config{URL: raw, HTTPClient: loopbackClient()})
	if err == nil {
		t.Fatal("Connect reported success against a port nothing listens on")
	}
	if strings.Contains(err.Error(), "s3cr3t-token") {
		t.Errorf("error carries the URL's password: %v", err)
	}
	endpoint, perr := url.Parse(raw)
	if perr != nil {
		t.Fatal(perr)
	}
	if want := "mcp: connect to " + endpoint.Redacted() + ":"; !strings.HasPrefix(err.Error(), want) {
		t.Errorf("error %q does not open by naming the endpoint it failed to reach (%q)", err, want)
	}
}

// TestRequestsCarryW3CTraceContext pins the propagation MCP 2026-07-28
// documents (SEP-414): the trace context rides `_meta` under the bare W3C key
// names, not the protocol's namespaced ones, so a trace that starts in this
// platform continues inside the server. Both request kinds carry it — a listing
// that lost it would break the discovery half of the same trace.
//
// It also pins the boundary of that claim, which is the reason to connect here
// under the traced context rather than through the shared helper: the handshake
// is the SDK's to build and takes no caller metadata, so a traced Connect
// reaches the server untraced. A trace covers this client from its first
// request, not from its first packet. Pinned because it is the kind of gap a
// reader assumes away, and because an SDK that later grew the hook should
// surface here as a failing test rather than as a quietly better wire.
func TestRequestsCarryW3CTraceContext(t *testing.T) {
	t.Parallel()
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x80, 0x81, 0x82, 0x83, 0x84, 0x85, 0x86, 0x87, 0x88, 0x89, 0x8a, 0x8b, 0x8c, 0x8d, 0x8e, 0x8f},
		SpanID:     trace.SpanID{0x91, 0x92, 0x93, 0x94, 0x95, 0x96, 0x97, 0x98},
		TraceFlags: trace.FlagsSampled,
	})
	want := fmt.Sprintf("00-%s-%s-01", sc.TraceID(), sc.SpanID())

	listed, called := &atomic.Pointer[json.RawMessage]{}, &atomic.Pointer[json.RawMessage]{}
	var handshakeMu sync.Mutex
	handshake := map[string]json.RawMessage{}
	inner := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server {
		return sdk.NewServer(&sdk.Implementation{Name: "meta-server", Version: "1"}, nil)
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
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(body, &req) != nil {
			inner.ServeHTTP(w, r)
			return
		}
		params := req.Params
		var result map[string]any
		switch req.Method {
		case "tools/list":
			listed.Store(&params)
			result = map[string]any{"tools": []any{}}
		case "tools/call":
			called.Store(&params)
			result = map[string]any{"content": []any{}}
		default:
			if req.Method != "" {
				handshakeMu.Lock()
				handshake[req.Method] = params
				handshakeMu.Unlock()
			}
			inner.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(ts.Close)

	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	conn, err := mcp.Connect(ctx, mcp.Config{URL: ts.URL, HTTPClient: loopbackClient()})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.ListTools(ctx); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if _, err := conn.CallTool(ctx, "echo", json.RawMessage(`{"q":"x"}`)); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	for name, got := range map[string]*atomic.Pointer[json.RawMessage]{"tools/list": listed, "tools/call": called} {
		params := got.Load()
		if params == nil {
			t.Fatalf("%s never reached the server", name)
		}
		var decoded struct {
			Meta map[string]any `json:"_meta"`
		}
		if err := json.Unmarshal(*params, &decoded); err != nil {
			t.Fatalf("%s params %s: %v", name, *params, err)
		}
		if decoded.Meta["traceparent"] != want {
			t.Errorf("%s _meta.traceparent = %v, want %q (in %s)", name, decoded.Meta["traceparent"], want, *params)
		}
	}

	handshakeMu.Lock()
	defer handshakeMu.Unlock()
	if len(handshake) == 0 {
		t.Fatal("no handshake request reached the server, so the gap this asserts was never exercised")
	}
	for method, params := range handshake {
		var decoded struct {
			Meta map[string]any `json:"_meta"`
		}
		if len(params) > 0 && json.Unmarshal(params, &decoded) != nil {
			continue // a handshake params shape this test does not model
		}
		if _, ok := decoded.Meta["traceparent"]; ok {
			t.Errorf("%s now carries a traceparent (%s) — the SDK grew the hook this package works around; "+
				"drop the caveat from the docs and pass the context through", method, params)
		}
	}
}

// TestRequestsWithoutATraceCarryNoTraceparent pins the other direction: with no
// span in the context, nothing is invented to fill the field with. What it does
// not pin is the shape of the `_meta` that goes out — the field is `omitempty`,
// so an empty map and a nil one put identical bytes on the wire.
func TestRequestsWithoutATraceCarryNoTraceparent(t *testing.T) {
	t.Parallel()
	url, seen := serveToolCall(t, func(json.RawMessage) map[string]any {
		return map[string]any{"content": []any{}}
	})

	if _, err := connect(t, url).CallTool(context.Background(), "echo", json.RawMessage(`{"q":"x"}`)); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	var decoded struct {
		Meta map[string]any `json:"_meta"`
	}
	if err := json.Unmarshal(*seen.Load(), &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded.Meta["traceparent"]; ok {
		t.Errorf("_meta carries a traceparent with no active span: %v", decoded.Meta)
	}
}
