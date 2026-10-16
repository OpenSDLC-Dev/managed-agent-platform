package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// A clone's HTTP transport records what it met (plan 66, #940), because
// go-git keeps too little of some of the host's failures to name them. It
// takes an empty or cut pack response for "already up to date"
// (transport.ErrEmptyUploadPackRequest), and the checkout then misses the
// commit, which reads as the caller's ref. It formats a body read that failed
// with %s, which leaves text no type matches.

// cloneRecord is what one clone's transport met: the first request that
// failed, the first read of a response body that failed, or a pack response
// that ended before its first byte (errEmptyPack).
type cloneRecord struct {
	mu     sync.Mutex
	failed error
}

// errHostTransport marks a clone failure whose transport recorded a failure
// of the host's (cloneFailed).
var errHostTransport = errors.New("the transport to the git host failed")

// errEmptyPack is a pack response that ended before its first byte. A clone
// always asks for objects, so a host that sends none failed.
var errEmptyPack = errors.New("the git host's pack response ended before its first byte")

// note keeps err as the record's failure unless one came before it, or ctx
// is done: then a failure is the deadline's or the cancellation's, which
// cloneReason reads from go-git's own error. A failure of this executor's own
// (isLocalError) takes the place of a host's that came before it, which
// go-git read past: go-git formats the local one with %s while it decodes the
// response, so only the record keeps its type (cloneFailed).
func (r *cloneRecord) note(ctx context.Context, err error) {
	if r == nil || ctx.Err() != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed == nil || (isLocalError(err) && !isLocalError(r.failed)) {
		r.failed = err
	}
}

// failure is the record's failure, nil where nothing failed or there is no
// record.
func (r *cloneRecord) failure() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failed
}

type cloneRecordKey struct{}

// withCloneRecord is ctx carrying a new record, for a clone to run under.
// go-git passes the clone's context to each of its requests, so the record is
// the clone's alone, though the transport is the process's.
func withCloneRecord(ctx context.Context) (context.Context, *cloneRecord) {
	rec := &cloneRecord{}
	return context.WithValue(ctx, cloneRecordKey{}, rec), rec
}

// cloneRecordFrom is the record ctx carries, or nil.
func cloneRecordFrom(ctx context.Context) *cloneRecord {
	rec, _ := ctx.Value(cloneRecordKey{}).(*cloneRecord)
	return rec
}

// go-git's HTTP client, for http and https alike, is the one it uses by
// default with its transport recording (recordingTransport). go-git keeps one
// client per protocol for the process, in a map it does not lock, so the
// client is installed as the package initializes, before any clone can read
// the map. Every go-git clone in a binary that imports this package goes
// through it, the evals suite's among them. go-git asks a client's transport
// to be an *http.Transport only for an endpoint that names its own proxy, CA
// bundle, client certificate or InsecureSkipTLS, and refuses such an endpoint
// with this one. None of those clones names one; the system's trust store
// and the proxy environment variables, which http.DefaultTransport reads,
// apply as before.
func init() {
	c := githttp.NewClient(&http.Client{Transport: recordingTransport{base: http.DefaultTransport}})
	client.InstallProtocol("http", c)
	client.InstallProtocol("https", c)
}

// recordingTransport is base, noting each failure into the record its
// request's context carries. A request with no record passes through as
// base's.
type recordingTransport struct{ base http.RoundTripper }

func (t recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := t.base.RoundTrip(req)
	ctx := req.Context()
	rec := cloneRecordFrom(ctx)
	if rec == nil {
		return res, err
	}
	if err != nil {
		rec.note(ctx, err)
		return res, err
	}
	// Only a success's body is what go-git reads the host's answer from. The
	// client drains a redirect's body and ignores a failure there, and go-git
	// reads a refusal by its status.
	if res.StatusCode/100 != 2 {
		return res, nil
	}
	pack := req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/"+transport.UploadPackServiceName)
	res.Body = &recordingBody{ReadCloser: res.Body, ctx: ctx, rec: rec, pack: pack}
	return res, nil
}

// recordingBody is a response body noting a read that failed, and a pack
// response that ended before its first byte.
type recordingBody struct {
	io.ReadCloser
	ctx  context.Context
	rec  *cloneRecord
	pack bool
	read int64
}

func (b *recordingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.read += int64(n)
	switch {
	case err == nil:
	case errors.Is(err, io.EOF):
		if b.pack && b.read == 0 {
			b.rec.note(b.ctx, errEmptyPack)
		}
	default:
		b.rec.note(b.ctx, err)
	}
	return n, err
}
