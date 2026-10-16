package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// A clone's HTTP transport marks the host's failures where go-git would keep
// too little of them (plan 66, #940). go-git takes a pack response that is
// empty, or cut before its first byte, for "already up to date"
// (transport.ErrEmptyUploadPackRequest), and its checkout then misses the
// commit, which reads as the caller's ref. It formats a body read that failed
// with %s while it decodes the response, which leaves text no type matches. So
// a clone's response bodies answer such a read with an error of their own:
// go-git returns it as it is, its words outlast go-git's %s, and it travels
// with the error the clone ends with.

// errHostTransport marks a read of the git host's response that failed on the
// host's side; isProtocolError reads it (transportMark).
var errHostTransport = errors.New("the git host's response broke off")

// errEmptyPack is a pack response that ended before its first byte. A clone
// always asks for objects, so a host that sends none failed.
var errEmptyPack = fmt.Errorf("%w: its pack response ended before its first byte", errHostTransport)

// errOwnRead marks a read of the git host's response that this executor's
// machine failed, out of buffers or memory; isLocalError reads it
// (transportMark).
var errOwnRead = errors.New("this executor could not read the git host's response")

// errContextRead marks a read of the git host's response that the clone's
// deadline or cancellation ended, and wraps the context's own error;
// cloneFailed reads it (transportMark).
var errContextRead = errors.New("the clone's deadline or cancellation ended the read of the git host's response")

// transportMark is the mark the clone's transport put on err,
// errHostTransport, errOwnRead or errContextRead: whichever mark's words come
// first in err's text. go-git keeps a mark's type only where it does not
// format it with %s, and its words either way. A mark's own text begins with
// its words, and what follows them is the error it wraps, which can quote the
// host's words, a mark's among them. The transport marks only a success
// body's reads that failed, so none of these carries a mark, whatever its text
// quotes: a failed request (a *url.Error, inside go-git's UnexpectedError
// too), the host's words that go-git read and kept in a type (hostWords), and
// the 401, 403 or 404 that go-git refuses with a sentinel and the body's text.
// It is nil where err carries none.
func transportMark(err error) error {
	var urlErr *url.Error
	if err == nil || hostWords(err) || errors.As(withUnexpected(err), &urlErr) ||
		errors.Is(err, transport.ErrAuthenticationRequired) || errors.Is(err, transport.ErrAuthorizationFailed) ||
		errors.Is(err, transport.ErrRepositoryNotFound) {
		return nil
	}
	msg := err.Error()
	var first error
	at := -1
	for _, mark := range []error{errHostTransport, errOwnRead, errContextRead} {
		if i := strings.Index(msg, mark.Error()); i >= 0 && (at < 0 || i < at) {
			first, at = mark, i
		}
	}
	return first
}

type cloneMarksKey struct{}

// withCloneMarks is ctx for a clone whose responses mark the host's failures
// (hostTransport). go-git passes the clone's context to each of its requests.
func withCloneMarks(ctx context.Context) context.Context {
	return context.WithValue(ctx, cloneMarksKey{}, true)
}

// go-git's HTTP client, for http and https alike, is the one it uses by
// default with the host's failures marked (hostTransport). go-git keeps one
// client per protocol for the process, in a map it does not lock, so the
// client is installed as the package initializes, before any clone can read
// the map. Every go-git clone in a binary that imports this package goes
// through it, the evals suite's among them, and only a clone whose context
// withCloneMarks made has its responses marked. go-git asks a client's
// transport to be an *http.Transport only for an endpoint that names its own
// proxy, CA bundle, client certificate or InsecureSkipTLS, and refuses such an
// endpoint with this one. None of those clones names one; the system's trust
// store and the proxy environment variables, which http.DefaultTransport
// reads, apply as before.
func init() {
	c := githttp.NewClient(&http.Client{Transport: hostTransport{base: http.DefaultTransport}})
	client.InstallProtocol("http", c)
	client.InstallProtocol("https", c)
}

// hostTransport is base, whose success bodies mark a read that failed, for a
// request whose context withCloneMarks made. Only a success's body is what
// go-git reads the host's answer from: the client drains a redirect's body
// and ignores a failure there, and go-git reads a refusal by its status. Every
// other request and response passes through as base's.
type hostTransport struct{ base http.RoundTripper }

func (t hostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := t.base.RoundTrip(req)
	if err != nil || res.StatusCode/100 != 2 || req.Context().Value(cloneMarksKey{}) == nil {
		return res, err
	}
	pack := req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/"+transport.UploadPackServiceName)
	res.Body = &markedBody{ReadCloser: res.Body, ctx: req.Context(), pack: pack}
	return res, nil
}

// markedBody is a response body whose failed read says whose it was: the
// host's (errHostTransport), a pack response that ended before its first byte
// among them (errEmptyPack), this executor's (errOwnRead), or the clone's
// deadline's or cancellation's (errContextRead), which net/http reports as
// the context's own error: its cause (context.Cause) over HTTP/1.1, its Err
// over HTTP/2. A read's own error carries no mark, which only this body puts
// on, so a mark's words in it are the host's: an HTTP/2 GOAWAY quotes the
// host's debug text.
type markedBody struct {
	io.ReadCloser
	ctx  context.Context
	pack bool
	read bool
}

func (b *markedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.read = b.read || n > 0
	switch {
	case err == nil:
		return n, err
	case err == io.EOF:
		if b.pack && !b.read {
			return n, errEmptyPack
		}
		return n, err
	case transportMark(err) == nil && isLocalError(err):
		return n, fmt.Errorf("%w: %w", errOwnRead, err)
	case b.ctx.Err() != nil && (errors.Is(err, b.ctx.Err()) || errors.Is(err, context.Cause(b.ctx))):
		return n, fmt.Errorf("%w: %w", errContextRead, err)
	default:
		return n, fmt.Errorf("%w: %w", errHostTransport, err)
	}
}
