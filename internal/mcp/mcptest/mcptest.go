// Package mcptest serves in-process MCP servers to tests in other packages.
//
// It exists so that the official go-sdk stays a dependency of internal/mcp and
// its own test support alone (CLAUDE.md): a package whose tests need something
// to speak MCP to imports this rather than building a server out of SDK types
// itself. The servers are real ones built from that SDK and served over real
// HTTP by httptest — not hand-rolled fakes, which would encode this platform's
// understanding of the protocol and pass whether or not it is right.
package mcptest

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tool is one tool a fixture server offers. Result is the text every call to it
// answers with — the common case, equivalent to a single Block of type "text".
// Blocks answers with those blocks instead when set, IsError marks the answer as
// a tool that ran and failed, and Fail makes the call itself fail (a JSON-RPC
// error rather than a result), which is a different thing entirely.
// While runs on the server's goroutine before the answer is built, for a test
// that needs the world to change during a call rather than around it.
type Tool struct {
	Name        string
	Description string
	Result      string
	Blocks      []Block
	IsError     bool
	Fail        string
	While       func()
}

// Block is one content block a fixture tool answers with, in this package's own
// shape so the go-sdk stays inside internal/mcp (CLAUDE.md). It mirrors what the
// protocol admits in a tool result: Type is "text", "image", "audio",
// "resource" or "resource_link", and each type fills only the fields that mean
// something for it.
type Block struct {
	Type     string
	Text     string
	Data     []byte
	MIMEType string
	URI      string
}

func sdkContent(b Block) sdk.Content {
	switch b.Type {
	case "image":
		return &sdk.ImageContent{Data: b.Data, MIMEType: b.MIMEType}
	case "audio":
		return &sdk.AudioContent{Data: b.Data, MIMEType: b.MIMEType}
	case "resource":
		res := &sdk.ResourceContents{URI: b.URI, MIMEType: b.MIMEType}
		if b.Data != nil {
			res.Blob = b.Data
		} else {
			res.Text = b.Text
		}
		return &sdk.EmbeddedResource{Resource: res}
	case "resource_link":
		return &sdk.ResourceLink{URI: b.URI, MIMEType: b.MIMEType}
	default:
		return &sdk.TextContent{Text: b.Text}
	}
}

// Server starts an MCP server offering tools and returns its URL. The server is
// shut down when the test ends.
func Server(t *testing.T, tools ...Tool) string {
	t.Helper()
	ts := httptest.NewServer(handler(build(tools)))
	t.Cleanup(ts.Close)
	return ts.URL
}

// ServeAt is Server listening somewhere other than loopback, and returns the
// same thing: the endpoint to point an MCP client at.
//
// It exists for the callers the platform reaches with its *own* dialer rather
// than a test client. The eval suite runs a real executor, whose MCP dials go
// through internal/dialguard — and that guard refuses loopback, which is exactly
// where httptest listens. Such a caller has nowhere to pass Client() in, so the
// fixture moves instead.
//
// host is an address of this machine the guard admits: a private-range interface
// address. The port is chosen free, and the URL names the host as given, so a
// caller that resolved an interface gets back a URL the executor can dial.
func ServeAt(t *testing.T, host string, tools ...Tool) string {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatalf("mcptest: listen on %s: %v", host, err)
	}
	srv := &http.Server{Handler: handler(build(tools))}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return "http://" + ln.Addr().String()
}

// shutdownGrace bounds the wait for in-flight requests at teardown. A fixture
// whose client walked away from a response would otherwise hold the test binary
// open for as long as that connection stays warm.
const shutdownGrace = 5 * time.Second

func handler(server *sdk.Server) http.Handler {
	return sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil)
}

// build is the fixture server itself, shared by both ways of serving it.
func build(tools []Tool) *sdk.Server {
	server := sdk.NewServer(&sdk.Implementation{Name: "mcptest", Version: "1"}, nil)
	for _, tool := range tools {
		tool := tool
		server.AddTool(
			&sdk.Tool{
				Name:        tool.Name,
				Description: tool.Description,
				InputSchema: map[string]any{"type": "object"},
			},
			func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				if tool.While != nil {
					tool.While()
				}
				if tool.Fail != "" {
					return nil, errors.New(tool.Fail)
				}
				blocks := tool.Blocks
				if blocks == nil {
					blocks = []Block{{Type: "text", Text: tool.Result}}
				}
				content := make([]sdk.Content, 0, len(blocks))
				for _, b := range blocks {
					content = append(content, sdkContent(b))
				}
				return &sdk.CallToolResult{Content: content, IsError: tool.IsError}, nil
			})
	}
	return server
}

// Client is an HTTP client a fixture server can be reached through. The
// production client carries a dial-address guard that refuses loopback — which
// is exactly where httptest listens, and the reason a caller cannot simply let
// the MCP config default.
func Client() *http.Client {
	return &http.Client{Transport: &http.Transport{}}
}
