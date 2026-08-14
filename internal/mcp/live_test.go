package mcp_test

import (
	"context"
	"fmt"
	"net/url"
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
	endpoint, token := mcptest.LiveServer(t)
	// Cut to scheme://host before it is quoted anywhere, the way every stored
	// MCP reason is: this endpoint is operator-supplied and may carry a
	// credential in its userinfo or its query, and a test log is a place those
	// end up in CI output.
	where := safeEndpoint(endpoint)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	conn, err := mcp.Connect(ctx, mcp.Config{URL: endpoint, BearerToken: token})
	if err != nil {
		t.Fatalf("connect to the live server %s: %v", where, scrub(err, endpoint, token))
	}
	defer func() { _ = conn.Close() }()

	tools, err := conn.ListTools(ctx)
	if err != nil {
		t.Fatalf("list the live server %s's tools: %v", where, scrub(err, endpoint, token))
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
	t.Logf("live server %s listed %d tool(s)", where, len(tools))
}

// safeEndpoint renders a configured endpoint for a log: scheme and host, nothing
// else. url.URL.Redacted would keep the query, which is one of the two places a
// credential rides.
func safeEndpoint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(unparseable endpoint)"
	}
	return u.Scheme + "://" + u.Host
}

// TestTheLiveTierNeverQuotesItsConfiguration runs offline, unlike the tier whose
// helpers it covers: the two things a live run writes to a CI log are built from
// an operator-supplied endpoint and a token, and neither may reach it whole.
func TestTheLiveTierNeverQuotesItsConfiguration(t *testing.T) {
	const (
		endpoint = "https://user:pw@mcp.example.com/sse?api_key=sekrit"
		token    = "tok-abc123"
	)
	where := safeEndpoint(endpoint)
	if want := "https://mcp.example.com"; where != want {
		t.Errorf("safeEndpoint = %q, want %q", where, want)
	}
	for _, secret := range []string{"user:pw", "sekrit", "api_key"} {
		if strings.Contains(where, secret) {
			t.Errorf("safeEndpoint kept %q: %s", secret, where)
		}
	}
	if got := safeEndpoint("::not a url"); got != "(unparseable endpoint)" {
		t.Errorf("an unparseable endpoint rendered as %q", got)
	}

	// net/http names the URL it dialled, masking the password and nothing else,
	// so the error a failing dial wraps carries both the query and the token.
	err := fmt.Errorf(`Post %q: bad token %s`, endpoint, token)
	out := scrub(err, endpoint, token)
	for _, secret := range []string{"sekrit", token} {
		if strings.Contains(out, secret) {
			t.Errorf("scrub kept %q: %s", secret, out)
		}
	}
	if !strings.Contains(out, "[redacted]") {
		t.Errorf("scrub replaced nothing: %s", out)
	}
	// An empty token is the anonymous dial this tier admits; scrubbing "" would
	// rewrite every character boundary in the message.
	if got := scrub(fmt.Errorf("plain failure"), "", ""); got != "plain failure" {
		t.Errorf("scrub with no secrets rewrote the message: %q", got)
	}
}

// scrub keeps the configured values out of an error a server chose the text of.
// The MCP client redacts the endpoint in the prefix it writes, but the transport
// error it wraps is net/http's, which names the URL it dialled with only the
// password masked.
func scrub(err error, secrets ...string) string {
	out := err.Error()
	for _, secret := range secrets {
		if secret != "" {
			out = strings.ReplaceAll(out, secret, "[redacted]")
		}
	}
	return out
}
