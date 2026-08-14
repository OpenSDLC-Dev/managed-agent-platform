package mcp_test

import (
	"context"
	"errors"
	"net"
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

// urlInText finds a URL anywhere in a message. Deliberately not stopping at a
// quote: a URL is allowed to contain one, and a pattern that ended the match
// there would cut a query in half and leave its tail in the log.
var urlInText = regexp.MustCompile(`(?i)https?://[^\s<>]+`)

// scrub keeps the configured endpoint's secrets out of an error whose text
// something else chose. By value first and then by shape, which is the order
// and the reason the executor's storableReason uses.
//
// By value, because redacting by shape alone leaves whatever the pattern cannot
// bound — a query holding a space or a quote ends the match early. By shape as
// well, because redacting by value alone misses the spellings the declared bytes
// never take: net/http masks the password before it writes the URL into the
// *url.Error it returns, and the client writes url.URL.Redacted's.
//
// The token is separate again: it is not in the URL, and a server may quote back
// the header it received in any text it likes.
func scrub(err error, endpoint, token string) string {
	out := err.Error()
	for _, form := range endpointForms(endpoint) {
		// An empty form would match at every character boundary, which is what
		// replacing "" means. The token below is guarded for the same reason.
		if form != "" {
			out = strings.ReplaceAll(out, form, safeEndpoint(endpoint))
		}
	}
	out = urlInText.ReplaceAllStringFunc(out, redactURL)
	if token != "" {
		out = strings.ReplaceAll(out, token, "[redacted]")
	}
	return out
}

// endpointForms lists the spellings the configured endpoint appears in. Three
// writers put it into a message and none agree: whatever echoes the
// configuration writes the declared bytes, the MCP client writes
// url.URL.Redacted's (password "xxxxx"), and net/http writes its own ("***").
func endpointForms(endpoint string) []string {
	forms := []string{endpoint}
	u, err := url.Parse(endpoint)
	if err != nil || u.User == nil {
		return forms
	}
	if _, ok := u.User.Password(); !ok {
		return forms
	}
	redacted := u.Redacted()
	return append(forms, redacted, strings.Replace(redacted, ":xxxxx@", ":***@", 1))
}

// redactURL cuts one matched URL to scheme://host, handing back trailing
// punctuation the greedy match swallowed. The give-back is one character at a
// time until what is left parses, because two of those characters also end a
// URL — "]" closes an IPv6 literal and ":" precedes a port.
func redactURL(match string) string {
	var trailing string
	for len(match) > 0 && strings.ContainsRune(`.,:;!?)]}"'`, rune(match[len(match)-1])) {
		trailing, match = match[len(match)-1:]+trailing, match[:len(match)-1]
	}
	u, err := url.Parse(match)
	for (err != nil || u.Host == "") && trailing != "" {
		match, trailing = match+trailing[:1], trailing[1:]
		u, err = url.Parse(match)
	}
	if err != nil || u.Host == "" {
		return "[redacted url]" + trailing
	}
	return u.Scheme + "://" + u.Host + trailing
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

	// Errors a real failing dial produces, not ones built from the declared
	// string: net/http rewrites the password to *** on its way into url.Error,
	// so a fixture written by hand from the configuration tests a shape this
	// path never emits and passes a scrub that would leak.
	//
	// The queries carry characters that can end a URL match — an apostrophe, a
	// raw space, a double quote — because bounding the match wrongly is how a
	// shape rule fails, and a query is where the credential is.
	//
	// The port is one this test opened and closed, so it is known shut rather
	// than assumed so, and the client carries a deadline: this runs on every
	// `make verify`, where a host that answered on a guessed port or a firewall
	// that dropped the packet would fail or hang the merge gate.
	closed := closedPort(t)
	client := &http.Client{Timeout: 10 * time.Second}
	for _, query := range []string{
		"api_key=sekrit",
		"note='&api_key=sekrit",
		"agent=my agent&api_key=sekrit",
		`note="x"&api_key=sekrit`,
	} {
		dialled := "http://user:pw@" + closed + "/mcp?" + query
		_, err := client.Get(dialled)
		if err == nil {
			t.Fatalf("a dial to a closed port should fail: ?%s", query)
		}
		// A refusal, not the client's deadline. Something listening on that
		// address would leave the request in an accept backlog until the
		// timeout, and an error that arrived that way says nothing about the
		// shape net/http writes for a failed dial.
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			t.Fatalf("the dial to %s timed out rather than being refused, so the "+
				"port is not closed and this fixture proves nothing", closed)
		}
		if !strings.Contains(err.Error(), "sekrit") {
			t.Fatalf("this fixture no longer carries the query credential, so it "+
				"proves nothing about scrubbing one: %v", err)
		}
		out := scrub(err, dialled, token)
		for _, secret := range []string{"sekrit", "api_key", "user:"} {
			if strings.Contains(out, secret) {
				t.Errorf("scrub kept %q from ?%s: %s", secret, query, out)
			}
		}
		if !strings.Contains(out, "http://"+closed) {
			t.Errorf("scrub dropped the host, which the message needs to be "+
				"readable: %s", out)
		}
	}

	// A URL that is not the configured endpoint — a redirect target a server
	// named, say — has no declared form to match, so the shape pass is what has
	// to catch it.
	other := scrub(errFor(`redirected to "https://elsewhere.test/x?api_key=sekrit"`),
		endpoint, token)
	if strings.Contains(other, "sekrit") {
		t.Errorf("scrub kept a secret from a URL it was not configured with: %s", other)
	}

	out := scrub(errFor("failed to reach "+endpoint), endpoint, token)
	for _, secret := range []string{"sekrit", "api_key", "user:"} {
		if strings.Contains(out, secret) {
			t.Errorf("scrub kept %q: %s", secret, out)
		}
	}
	if !strings.Contains(out, where) {
		t.Errorf("scrub dropped the host, which the message needs to be readable: %s", out)
	}

	// The token rides a header, not the URL, so it survives the URL rewrite and
	// needs its own replacement.
	quoted := scrub(errFor("server said: bad bearer "+token), endpoint, token)
	if strings.Contains(quoted, token) {
		t.Errorf("scrub kept the token: %s", quoted)
	}
	// An empty token is the anonymous dial this tier admits; replacing "" would
	// rewrite every character boundary in the message.
	if got := scrub(errFor("plain failure"), "", ""); got != "plain failure" {
		t.Errorf("scrub with no configuration rewrote the message: %q", got)
	}
}

// closedPort returns a loopback address nothing is listening on, by taking one
// and giving it back. Guessing a low port instead would rest on nothing running
// there, which is not this test's to assume on a machine it does not own.
func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return addr
}

func errFor(msg string) error { return &staticError{msg} }

type staticError struct{ msg string }

func (e *staticError) Error() string { return e.msg }
