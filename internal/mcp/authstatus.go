package mcp

import (
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
)

// ErrUnauthorized marks an error raised on an exchange the server answered 401
// or 403 to — the credential was refused, or the server required one and this
// dial carried none.
//
// It exists because the two failures the wire distinguishes cannot otherwise be
// told apart here. The reference splits them by cause:
// `mcp_connection_failed_error` is "the MCP server could not be reached (network
// error, timeout, or non-authentication HTTP failure)", while
// `mcp_authentication_failed_error` is "the server rejected the credential from
// the attached vault, required authentication when no matching credential was
// configured, or an OAuth token refresh failed". A refused credential is
// therefore not a connection that failed — the connection worked well enough to
// be refused.
//
// The status is observed here rather than read off the SDK's error, which does
// not carry it: go-sdk v1.7.0 renders a non-2xx as `http.StatusText(code)` inside
// a formatted message and wraps no sentinel (mcp/streamable.go, checkResponse),
// and 401/403 are not among the statuses it treats as transient. Matching that
// message would be matching prose that a version bump may reword; watching the
// response is exact, and this package already owns the whole transport chain.
//
// Distinct from [ErrServerAnswered], which marks a call the server answered *and
// refused*: that is a working server reporting a working failure, and the model
// is told to stop calling the tool. An authentication failure is the operator's
// to fix, so a caller checks this one first.
var ErrUnauthorized = errors.New("the server refused the credential")

// authWatch is the innermost RoundTripper of a connection's chain: it records
// whether any exchange came back 401 or 403, so an error raised anywhere
// downstream of one can be marked as an authentication failure.
//
// The flag spans an operation, not a request and not the connection. A single
// operation is several exchanges and a failure surfaces at whichever of them the
// SDK gave up on, which need not be the refused one — so it cannot be per
// request. But a 401 the SDK recovered from would otherwise answer for every
// later error on the same connection, so each operation clears it before it
// begins (see [authWatch.reset]) and the flag then answers only for that one.
type authWatch struct {
	base http.RoundTripper
	seen atomic.Bool
}

// withAuthWatch returns a shallow copy of client whose transport records
// refusals, and the recorder to read them back from.
func withAuthWatch(client *http.Client) (*http.Client, *authWatch) {
	w := &authWatch{base: client.Transport}
	copied := *client
	copied.Transport = w
	return &copied, w
}

func (w *authWatch) RoundTrip(req *http.Request) (*http.Response, error) {
	base := w.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	// A transport error and a response can arrive together; the status is worth
	// recording whenever there is one to read.
	if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
		w.seen.Store(true)
	}
	return resp, err
}

// mark wraps err with ErrUnauthorized when this connection was refused. A nil
// watch marks nothing, so a Conn built without one (a test's, or a future
// caller's) behaves exactly as it did before.
func (w *authWatch) mark(err error) error {
	if err == nil || w == nil || !w.seen.Load() {
		return err
	}
	return fmt.Errorf("%w: %w", ErrUnauthorized, err)
}

// refused reports whether this operation was answered 401 or 403. A nil watch
// has seen nothing.
func (w *authWatch) refused() bool { return w != nil && w.seen.Load() }

// reset starts a fresh operation. Clearing on the way in rather than on any
// non-401 response is what keeps the flag readable under the SDK's standalone
// SSE stream, whose exchanges are not this operation's and could otherwise clear
// a refusal between the refusal and the read of it.
func (w *authWatch) reset() {
	if w != nil {
		w.seen.Store(false)
	}
}
