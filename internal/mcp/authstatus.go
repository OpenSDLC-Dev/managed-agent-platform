package mcp

import (
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
)

// ErrUnauthorized marks an error raised on an exchange the server answered 401
// to — the credential was refused, or the server required one and this dial
// carried none. 401 alone: a 2026-09-03 recording dialled five statuses in one
// turn and only that one came back an authentication failure, 403 answering
// `mcp_connection_failed_error` ("access forbidden") beside 407, 500 and 502
// (#572). A forbidden request is a decision about the request, not about
// whether the credential was accepted.
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
// not carry it: for a 401 the go-sdk wraps no sentinel of its own — only a
// JSON-RPC error it finds in the body, when the server sent one — and renders
// the status as `http.StatusText(code)` inside a formatted message, since 401
// is not among the statuses it treats as transient (checked against go-sdk
// v1.7.0 — mcp/streamable.go streamableClientConn.checkResponse and
// isTransientHTTPStatus). Matching that message would be matching prose that a
// version bump may reword; watching the response is exact, and this package
// already owns the whole transport chain.
//
// Distinct from [ErrServerAnswered], which marks a call the server answered *and
// refused*: that is a working server reporting a working failure, and the model
// is told to stop calling the tool. An authentication failure is the operator's
// to fix, so a caller checks this one first.
var ErrUnauthorized = errors.New("the server refused the credential")

// authWatch is the innermost RoundTripper of a connection's chain: it records
// the status of the most recent exchange, so an error raised anywhere downstream
// of it can be classified by what the wire said — a 401 marks an authentication
// failure, and only a 2xx lets a JSON-RPC error read as the server's answer (see
// [authWatch.delivered]).
//
// The status answers for the connection's most recent exchange, within an
// operation. A failure surfaces at whichever exchange the SDK gave up on, which
// need not be the refused one, so the status cannot be per request; but a
// refusal the SDK recovered from must not speak for what failed afterwards, so
// each response replaces the last and each operation clears the status before
// it begins (see [authWatch.reset]).
type authWatch struct {
	base http.RoundTripper
	// status is the most recent exchange's HTTP status, and 0 when it produced
	// no response or when this operation has made none yet.
	status atomic.Int32
}

// withAuthWatch returns a shallow copy of client whose transport records each
// exchange's status, and the watch to read it back from — the refusal and the
// server-answered question both.
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
		var status int32
		if resp != nil {
			status = int32(resp.StatusCode)
		}
		w.status.Store(status)
	}
	return resp, err
}

// mark wraps err with ErrUnauthorized when this connection was refused. A nil
// watch marks nothing.
func (w *authWatch) mark(err error) error {
	if err == nil || !w.refused() {
		return err
	}
	return fmt.Errorf("%w: %w", ErrUnauthorized, err)
}

// refused reports whether this operation was answered 401. A nil watch has seen
// nothing.
func (w *authWatch) refused() bool {
	return w != nil && w.status.Load() == http.StatusUnauthorized
}

// delivered reports whether this operation's most recent exchange came back
// 2xx. MCP's streamable transport carries a JSON-RPC error on a 2xx, so that is
// the only status on which an error can be the server's own refusal; anything
// else, or no response at all, is the HTTP layer failing (#641).
//
// The most recent exchange is not always the call's own. A reply the client
// posts to a server's request mid-call is one too, and a transient failure of
// that reply, which the go-sdk shrugs off, leaves a call the server then refused
// reading as a connection failure: the operator hears of an HTTP failure that
// did happen, and the model's result is the same either way. Reading only the
// call's own exchange would err the other way, which is worse. A non-transient
// refusal of the reply ends the connection (checked against go-sdk v1.7.0 —
// mcp/streamable.go streamableClientConn.Write), and the call then fails
// carrying the refusal's JSON-RPC error while its own exchange was a 200 — a
// connection failure the operator would never hear of. The two differ only in
// which statuses the SDK counts as transient.
//
// A nil watch has seen no exchange and vouches for none, so a Conn built without
// one reports no error as the server's answer: without the status nothing shows
// the error rode a 2xx, and reporting a failure that was not one costs less
// than hiding one that was.
func (w *authWatch) delivered() bool {
	if w == nil {
		return false
	}
	status := w.status.Load()
	return status >= 200 && status < 300
}

// reset starts a fresh operation, so each operation is judged on its own
// exchanges alone: one that fails before making any is judged on nothing, not
// on whatever the previous operation's last exchange said.
func (w *authWatch) reset() {
	if w != nil {
		w.status.Store(0)
	}
}
