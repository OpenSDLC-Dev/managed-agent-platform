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
// The flag answers for the connection's most recent exchange, within an
// operation. A failure surfaces at whichever exchange the SDK gave up on, which
// need not be the refused one, so the flag cannot be per request; but a refusal
// the SDK recovered from must not speak for what failed afterwards, so each
// response replaces the last and each operation clears the flag before it
// begins (see [authWatch.reset]).
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
	// A transport error and a response can arrive together, so the status is
	// read whenever there is one rather than only when err is nil.
	//
	// The latest answer replaces the one before it rather than joining it. An
	// operation is several exchanges and the SDK recovers from some of them — it
	// opens with `server/discover` and falls back to the legacy `initialize` on
	// any error there — so a refused probe followed by a 500 is a connection
	// that failed, not a credential that was refused, and a flag that only ever
	// rose would report the wrong one.
	//
	// An exchange with no response at all replaces it too, with nothing: a
	// transport failure after a refused probe is a connection that failed, and
	// leaving the refusal standing would send the operator after a credential
	// that is fine.
	//
	// A DELETE is not one of those exchanges. It is the streamable transport's
	// session teardown, sent after the operation has already failed, and letting
	// it answer for the operation would erase the refusal that caused the
	// teardown — which is exactly what it did.
	if req.Method != http.MethodDelete {
		w.seen.Store(resp != nil && (resp.StatusCode == http.StatusUnauthorized ||
			resp.StatusCode == http.StatusForbidden))
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
