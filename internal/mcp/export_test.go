package mcp

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ListToolsWithinForTest runs a listing under a caller-chosen whole-listing
// budget, which is what ListTools does with ListTimeout.
//
// The budget is two minutes in production, and no suite waits that out — so with
// it read directly inside the loop, replacing the deadline with a plain cancel
// left every test green and the published bound rested on review alone.
func ListToolsWithinForTest(c *Conn, ctx context.Context, budget time.Duration) ([]Tool, error) {
	return c.listTools(ctx, budget)
}

// LimitedBodyForTest wraps body with a total budget of limit bytes, exactly as a
// connection's response bodies are wrapped, and returns it along with the shared
// counter so a test can add a second body to the same budget.
//
// It exists for the boundary that decides whether the bound means "at most" or
// "fewer than": a body ending exactly on the budget. Reaching that through
// Connect would mean serving an HTTP response of an exact total byte length,
// framing included, and the framing is the SDK's and the server's rather than
// the test's — so the test would be asserting something about net/http's
// chunking rather than about this reader.
func LimitedBodyForTest(body io.ReadCloser, limit int64) (io.ReadCloser, *Budget) {
	t := newLimitedTransport(nil, limit)
	return &limitedBody{ReadCloser: body, transport: t}, &Budget{t}
}

// RequestDeadlineForTest reports the deadline the transport gave an outbound
// request, as observed by the round-tripper underneath it, and whether there was
// one at all. Pass a context to make the request with.
//
// The path it exercises is only reachable from outside through go-sdk's own
// connection teardown, which detaches its context deliberately, so driving the
// transport directly is the only way to assert the fallback both fires when a
// request carries no deadline and stays out of the way when it does.
func RequestDeadlineForTest(ctx context.Context) (time.Duration, bool) {
	spy := &deadlineSpy{}
	tr := newLimitedTransport(spy, 1<<20)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, "http://example.invalid/rpc", nil)
	if err != nil {
		panic(err)
	}
	if _, err := tr.RoundTrip(req); err != nil {
		panic(err)
	}
	return spy.left, spy.had
}

// RoundTripBodyForTest sends a real request through the limiting transport and
// hands the body back unread, so a test can read it after RoundTrip has already
// returned.
//
// That ordering is the point: the fallback deadline has to outlive RoundTrip,
// because the body is streamed after it returns. Cancelling on return instead
// would pass every fixture whose body is small enough to arrive whole inside the
// round trip, and fail only against a server that streams — which is every real
// one.
func RoundTripBodyForTest(ctx context.Context, url string, limit int64) (io.ReadCloser, error) {
	// A transport of its own for the same reason loopbackClient has one: passing
	// nil here falls through to http.DefaultTransport, which every httptest
	// server in the suite clears the idle connections of when it closes.
	tr := newLimitedTransport(&http.Transport{}, limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

type deadlineSpy struct {
	had  bool
	left time.Duration
}

func (s *deadlineSpy) RoundTrip(req *http.Request) (*http.Response, error) {
	var dl time.Time
	dl, s.had = req.Context().Deadline()
	if s.had {
		s.left = time.Until(dl)
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
}

// BearerAttachesForTest reports whether a bearer transport scoped to endpoint
// would put its credential on a request for target. It drives the real
// RoundTrip and reads back the header rather than re-deciding, so the test
// cannot pass by agreeing with a copy of the rule.
//
// The decision is two comparisons, scheme and host, and only the host half is
// reachable through a redirect a test client will actually follow: expressing a
// scheme downgrade — an https endpoint answering with a redirect to http on the
// same host, which is the case that would put the credential on the wire in
// cleartext — needs the origins named directly.
func BearerAttachesForTest(endpoint, target string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		panic(err)
	}
	// Through withBearer rather than by constructing a bearerTransport here: a
	// seam that rebuilds production wiring is a seam a mutant of that wiring
	// walks straight past, which this package has already been bitten by twice.
	spy := &headerSpy{}
	client := withBearer(&http.Client{Transport: spy}, "probe", u)
	req, err := http.NewRequest(http.MethodPost, target, nil)
	if err != nil {
		panic(err)
	}
	if _, err := client.Transport.RoundTrip(req); err != nil {
		panic(err)
	}
	return spy.auth != ""
}

type headerSpy struct{ auth string }

func (s *headerSpy) RoundTrip(req *http.Request) (*http.Response, error) {
	s.auth = req.Header.Get("Authorization")
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
}

// LimitedTransportForTest wraps base with a budget of limit bytes, exactly as a
// connection's transport is wrapped, so a test can drive a budget small enough
// that the header charge alone refuses a response.
//
// It exists for the refusal path's Close: the line executes on every refusal, so
// statement coverage is satisfied whether or not it is there, and deleting it
// left the whole suite green while leaking a socket and its reader goroutine per
// refused response. What separates the two is not reachable from the assertions
// a fixture normally makes — only from the connection underneath.
func LimitedTransportForTest(base http.RoundTripper, limit int64) http.RoundTripper {
	return newLimitedTransport(base, limit)
}

// HeaderBoundsForTest reports the raw cap on one header block and the slack
// net/http adds to it on the way to HTTP/2's SETTINGS_MAX_HEADER_LIST_SIZE.
//
// Both are exposed rather than only the first because the fixtures that matter
// sit 128 bytes below the sum of the two and 64 above it. Rounder margins pass
// under any overhead value and so assert nothing about net/http; these tie the
// constants to the framer's real boundary.
func HeaderBoundsForTest() (headerBytes, h2Overhead int64) {
	return maxHeaderBytesPerResponse, http2HeaderListOverhead
}

// Budget is a handle on one connection's shared byte budget.
type Budget struct{ t *limitedTransport }

// Wrap adds another body drawing on the same budget, which is what makes the
// bound cumulative across a paginated listing rather than per response.
func (b *Budget) Wrap(body io.ReadCloser) io.ReadCloser {
	return &limitedBody{ReadCloser: body, transport: b.t}
}
