package mcp_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/mcp"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/mcp/mcptest"
)

// TestLiveServerListsItsTools is the tier every other test in this package
// cannot be: a handshake and a listing against a server this repository did not
// write.
//
// Everything else here speaks to a fixture built from the same go-sdk the client
// is built from, so both ends share one understanding of the protocol and agree
// even where that understanding is wrong — a negotiated version this platform
// mis-sends, a header a real implementation requires, an initialize the SDK
// happens to accept from itself. Only a third party's server can find that out.
//
// It asserts little on purpose. What a public server offers is its business and
// will change; that the platform can reach one, complete the handshake, and read
// back a listing it can store is the whole claim.
func TestLiveServerListsItsTools(t *testing.T) {
	url, token := mcptest.LiveServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	conn, err := mcp.Connect(ctx, mcp.Config{URL: url, BearerToken: token})
	if err != nil {
		t.Fatalf("connect to the live server: %v", err)
	}
	defer func() { _ = conn.Close() }()

	tools, err := conn.ListTools(ctx)
	if err != nil {
		t.Fatalf("list the live server's tools: %v", err)
	}
	if len(tools) == 0 {
		t.Fatal("the live server listed no tools: nothing here proves a listing round-trips")
	}
	// The shape the platform stores and later hands the model. A server that
	// answered with a nameless tool, or one with no schema, would be written
	// into a catalog the brain then offers as a tool the model cannot call.
	for _, tool := range tools {
		if strings.TrimSpace(tool.Name) == "" {
			t.Errorf("a listed tool has no name: %+v", tool)
		}
		if len(tool.InputSchema) == 0 {
			t.Errorf("tool %q has no input schema", tool.Name)
		}
	}
	// No test client is passed in, so this went out through DefaultClient — the
	// guarded one. Reaching a third-party endpoint through it is the other half
	// of what this tier proves: the address guard admits the public internet.
	t.Logf("live server %s listed %d tool(s)", url, len(tools))
}
