package mcp

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SameHostForTest exposes the origin comparison the bearer-token transport
// uses. It is reachable through Connect only via a redirect between two hosts
// the caller's client will actually dial, which cannot express the case that
// matters most here — a scoped IPv6 zone identifier, whose case is locally
// significant and is the one way the comparison could match two origins that
// are not the same one.
var SameHostForTest = sameHost

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
	spy := &headerSpy{}
	tr := &bearerTransport{base: spy, token: "probe", scheme: u.Scheme, host: u.Host}
	req, err := http.NewRequest(http.MethodPost, target, nil)
	if err != nil {
		panic(err)
	}
	if _, err := tr.RoundTrip(req); err != nil {
		panic(err)
	}
	return spy.auth != ""
}

type headerSpy struct{ auth string }

func (s *headerSpy) RoundTrip(req *http.Request) (*http.Response, error) {
	s.auth = req.Header.Get("Authorization")
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
}

// Budget is a handle on one connection's shared byte budget.
type Budget struct{ t *limitedTransport }

// Wrap adds another body drawing on the same budget, which is what makes the
// bound cumulative across a paginated listing rather than per response.
func (b *Budget) Wrap(body io.ReadCloser) io.ReadCloser {
	return &limitedBody{ReadCloser: body, transport: b.t}
}
