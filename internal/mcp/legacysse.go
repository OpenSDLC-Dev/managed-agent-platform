package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync/atomic"
)

// legacyWatch decides whether a failed Streamable HTTP handshake is the signal
// the spec gives for a server that hosts only the deprecated HTTP+SSE transport
// of 2024-11-05: a POST "fails with HTTP status code `400 Bad Request`, `404 Not
// Found`, or `405 Method Not Allowed` **and** the response body is not a
// recognized modern JSON-RPC error" (spec 2026-07-28,
// transports/streamable-http § "HTTP+SSE Transport (2024-11-05)"). The
// reference documents the fallback this enables — "servers that only support the
// deprecated SSE transport still work through an automatic fallback"
// (managed-agents/reference § "Supported MCP server types") — and a 2026-09-23
// recording shows it taken on a 405.
//
// The body is read here rather than off the SDK's error, for the same reason
// authWatch reads the status: the SDK's error does not reliably carry it. On a
// 404 the go-sdk never reads the body at all — it returns ErrSessionMissing,
// since 404 is also how a server ends a session — and its decoding of a 400 or
// 405 body into a JSON-RPC error can be switched off by MCPGODEBUG (checked
// against go-sdk v1.7.0 — mcp/streamable.go streamableClientConn.checkResponse).
// Reading the response here gives one answer for all three statuses, whatever
// the SDK does with it afterwards.
//
// It sits above the response limit, so what it reads is charged to the
// connection's budget like any other body, and it hands the SDK the same bytes
// it read. The whole body is read rather than a prefix: a prefix of a long
// JSON-RPC error does not decode, and would read as the signal.
//
// Only a POST's answer counts, and the latest one replaces the one before it: a
// refused `server/discover` followed by an `initialize` refused for another
// reason is not the signal. And a POST answered 2xx ends the question for the
// connection — "If it succeeds, the client can assume this is a server
// supporting the new Streamable HTTP transport" — so a POST refused after one,
// such as the `notifications/initialized` that follows a successful
// `initialize`, is that server failing, not a sign of the old transport.
type legacyWatch struct {
	base     http.RoundTripper
	signal   atomic.Bool
	answered atomic.Bool
}

// fallback reports whether the connection's failure is the signal: its latest
// POST was refused that way, and no POST before it was answered.
func (w *legacyWatch) fallback() bool {
	return w.signal.Load() && !w.answered.Load()
}

// withLegacyWatch returns a shallow copy of client whose transport records the
// signal, and the watch to read it back from.
func withLegacyWatch(client *http.Client) (*http.Client, *legacyWatch) {
	w := &legacyWatch{base: client.Transport}
	copied := *client
	copied.Transport = w
	return &copied, w
}

// RoundTrip needs no nil-base fallback, for the reason bearerTransport's does
// not: it is only ever applied to the client withResponseLimit has just
// returned.
func (w *legacyWatch) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := w.base.RoundTrip(req)
	if req.Method != http.MethodPost {
		return resp, err
	}
	signal := false
	if err == nil {
		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			w.answered.Store(true)
		case resp.StatusCode == http.StatusBadRequest, resp.StatusCode == http.StatusNotFound,
			resp.StatusCode == http.StatusMethodNotAllowed:
			signal = notJSONRPCError(resp)
		}
	}
	w.signal.Store(signal)
	return resp, err
}

// notJSONRPCError reads resp's body, puts the bytes back in front of whatever
// is left of it, and reports whether it was read cleanly and is not a JSON-RPC
// error response. A body that could not be read signals nothing: the budget
// refusing it, or the connection dropping, is a failure to report, not a reason
// to make another request.
//
// An error response is recognized by its version and its error code alone,
// whatever its id. The go-sdk's own decoder will not do here: it refuses a
// response without a valid id (checked against go-sdk v1.7.0 —
// internal/jsonrpc2/messages.go DecodeMessage), while JSON-RPC 2.0 answers a
// request whose id could not be read with `"id": null`, and the spec lets a
// refused POST carry "a JSON-RPC *error response* that has no `id`" (spec
// 2026-07-28, transports/streamable-http).
func notJSONRPCError(resp *http.Response) bool {
	body, err := io.ReadAll(resp.Body)
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(body), resp.Body), resp.Body}
	if err != nil {
		return false
	}
	var answer struct {
		JSONRPC string `json:"jsonrpc"`
		Error   *struct {
			Code *int64 `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &answer) != nil {
		return true
	}
	return answer.JSONRPC != "2.0" || answer.Error == nil || answer.Error.Code == nil
}

// errForeignOrigin is the refusal of a request the deprecated transport would
// send somewhere other than the server's own origin.
var errForeignOrigin = errors.New(
	"mcp: refused a request outside the server's origin; the HTTP+SSE stream named a message endpoint elsewhere")

// withSameOrigin returns a shallow copy of client that sends nothing outside
// endpoint's origin.
//
// The deprecated transport lets the server choose where every message goes: the
// stream's first event carries the message endpoint, and the go-sdk resolves it
// against the server URL with url.Parse, so an absolute URL in it names any
// host and scheme at all (checked against go-sdk v1.7.0 — mcp/sse.go
// SSEClientTransport.Connect). The bearer would not follow — withBearer scopes
// it to the configured origin — and the dial guard still vets the address, but
// the session's messages would. The configured URL is the only destination
// anyone approved, so a message endpoint elsewhere fails the connection.
//
// The comparison is bearerTransport's, and fails closed the same way: the same
// origin spelled two ways is refused rather than normalized. Whether the
// reference's client follows a cross-origin endpoint is unobserved.
func withSameOrigin(client *http.Client, endpoint *url.URL) *http.Client {
	copied := *client
	copied.Transport = &originTransport{base: client.Transport, scheme: endpoint.Scheme, host: endpoint.Host}
	return &copied
}

type originTransport struct {
	base   http.RoundTripper
	scheme string
	host   string
}

func (t *originTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != t.scheme || !sameHost(req.URL.Host, t.host) {
		// A RoundTripper closes the request body, including on errors.
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, errForeignOrigin
	}
	return t.base.RoundTrip(req)
}
