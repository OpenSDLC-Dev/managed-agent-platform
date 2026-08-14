package mcp_test

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"net/url"

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
// back a listing it can store is the whole claim. Nothing here checks the shape
// of a listed tool: ListTools already drops anything without a usable name or an
// object input schema, so an assertion about those would be about the client's
// filter and not about the server.
func TestLiveServerListsItsTools(t *testing.T) {
	endpoint, token := mcptest.LiveServer(t)
	// Cut to scheme://host before it is quoted anywhere: this endpoint is
	// operator-supplied and may carry a credential in its userinfo or its
	// query, and a test log is a place those end up in CI output.
	where := safeEndpoint(endpoint)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	conn, err := mcp.Connect(ctx, mcp.Config{URL: endpoint, BearerToken: token})
	if err != nil {
		t.Fatalf("connect to the live server %s: %v", where, scrub(err, token))
	}
	defer func() { _ = conn.Close() }()

	tools, err := conn.ListTools(ctx)
	if err != nil {
		t.Fatalf("list the live server %s's tools: %v", where, scrub(err, token))
	}
	if len(tools) == 0 {
		t.Fatal("the live server listed no tools: nothing here proves a listing round-trips")
	}
	// No test client is passed in, so this went out through DefaultClient — the
	// guarded one. Reaching a third-party endpoint through it is the other half
	// of what this tier proves: the address guard admits the public internet.
	t.Logf("live server %s listed %d tool(s)", where, len(tools))
}

// safeEndpoint renders a URL for a log: scheme and host, nothing else.
// url.URL.Redacted would keep the query, which is one of the two places a
// credential rides.
func safeEndpoint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(unparseable endpoint)"
	}
	return u.Scheme + "://" + u.Host
}

// urlInText finds a URL anywhere in a message. The closing set excludes the
// quote %q wraps a url.Error's URL in, so the match ends where the URL does.
var urlInText = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s"'<>]+`)

// scrub keeps the configured endpoint's secrets out of an error whose text
// something else chose.
//
// Redacting by the configured string alone does not work, and fails on exactly
// the shape it would be written for: net/http masks the password before it
// writes the URL into the *url.Error it returns, so the declared bytes are not
// what appears and a substring replacement matches nothing — leaving the query,
// where the other half of endpoint credentials ride, in the log. So every URL in
// the message is cut to scheme://host whatever its spelling, which is the same
// thing the executor's storableReason does with the reasons it stores.
//
// The token is separate: it is not in the URL, and a server may quote back the
// header it received in any text it likes.
func scrub(err error, token string) string {
	out := urlInText.ReplaceAllStringFunc(err.Error(), func(match string) string {
		return safeEndpoint(strings.TrimRight(match, `.,;:)]}`))
	})
	if token != "" {
		out = strings.ReplaceAll(out, token, "[redacted]")
	}
	return out
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
	if got := safeEndpoint("://not a url"); got != "(unparseable endpoint)" {
		t.Errorf("an unparseable endpoint rendered as %q", got)
	}

	// The error a real failing dial produces, not one built from the declared
	// string: net/http rewrites the password to *** on its way into url.Error,
	// so a fixture written by hand from the configuration tests a shape this
	// path never emits and passes a scrub that would leak.
	_, err := http.Get("http://user:pw@127.0.0.1:1/mcp?api_key=" + "sekrit")
	if err == nil {
		t.Fatal("a dial to a closed port should fail")
	}
	if !strings.Contains(err.Error(), "sekrit") {
		t.Fatalf("this fixture no longer carries the query credential, so it proves "+
			"nothing about scrubbing one: %v", err)
	}
	out := scrub(err, token)
	for _, secret := range []string{"sekrit", "api_key", "user:"} {
		if strings.Contains(out, secret) {
			t.Errorf("scrub kept %q: %s", secret, out)
		}
	}
	if !strings.Contains(out, "http://127.0.0.1:1") {
		t.Errorf("scrub dropped the host, which the message needs to be readable: %s", out)
	}

	// The token rides a header, not the URL, so it survives the URL rewrite and
	// needs its own replacement.
	quoted := scrub(errFor("server said: bad bearer "+token), token)
	if strings.Contains(quoted, token) {
		t.Errorf("scrub kept the token: %s", quoted)
	}
	// An empty token is the anonymous dial this tier admits; replacing "" would
	// rewrite every character boundary in the message.
	if got := scrub(errFor("plain failure"), ""); got != "plain failure" {
		t.Errorf("scrub with no token rewrote the message: %q", got)
	}
}

func errFor(msg string) error { return &staticError{msg} }

type staticError struct{ msg string }

func (e *staticError) Error() string { return e.msg }
