package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
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
// host's side; isProtocolError reads it by its words, which go-git's %w
// carries and its %s keeps.
var errHostTransport = errors.New("the git host's response broke off")

// errEmptyPack is a pack response that ended before its first byte. A clone
// always asks for objects, so a host that sends none failed.
var errEmptyPack = fmt.Errorf("%w: its pack response ended before its first byte", errHostTransport)

// errOwnRead marks a read of the git host's response that this executor's
// machine failed, out of buffers or memory; isLocalError reads it by its
// words, as isProtocolError reads errHostTransport's.
var errOwnRead = errors.New("this executor could not read the git host's response")

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
// among them (errEmptyPack), or this executor's (errOwnRead). A read that
// failed once the clone's context was done is the deadline's or the
// cancellation's, which cloneReason reads from go-git's own error, so it
// keeps its error. A pack response that ended with no bytes is the host's
// either way: the client returns such a body's end whatever the context.
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
	case b.ctx.Err() != nil:
		return n, err
	case isLocalError(err):
		return n, fmt.Errorf("%w: %w", errOwnRead, err)
	default:
		return n, fmt.Errorf("%w: %w", errHostTransport, err)
	}
}
