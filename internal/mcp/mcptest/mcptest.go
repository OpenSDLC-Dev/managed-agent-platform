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
	"net/http"
	"net/http/httptest"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tool is one tool a fixture server offers. Result is the text every call to it
// answers with.
type Tool struct {
	Name        string
	Description string
	Result      string
}

// Server starts an MCP server offering tools and returns its URL. The server is
// shut down when the test ends.
func Server(t *testing.T, tools ...Tool) string {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "mcptest", Version: "1"}, nil)
	for _, tool := range tools {
		result := tool.Result
		server.AddTool(
			&sdk.Tool{
				Name:        tool.Name,
				Description: tool.Description,
				InputSchema: map[string]any{"type": "object"},
			},
			func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				return &sdk.CallToolResult{
					Content: []sdk.Content{&sdk.TextContent{Text: result}},
				}, nil
			})
	}
	ts := httptest.NewServer(sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return server }, nil))
	t.Cleanup(ts.Close)
	return ts.URL
}

// Client is an HTTP client a fixture server can be reached through. The
// production client carries a dial-address guard that refuses loopback — which
// is exactly where httptest listens, and the reason a caller cannot simply let
// the MCP config default.
func Client() *http.Client {
	return &http.Client{Transport: &http.Transport{}}
}
