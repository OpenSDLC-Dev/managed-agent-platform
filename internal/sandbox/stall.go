package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultStallTimeout is how long a sandbox transport call may move zero bytes
// before it is abandoned when a provider configures no StallTimeout of its own.
// It sits well below the 30-minute work-item stall budget (#383) while still
// clearing silent platform steps such as extracting a 2 GiB checkpoint tar or
// unpacking a large container image layer (#395).
const DefaultStallTimeout = 5 * time.Minute

// ErrStalled reports that a sandbox transport call made no progress for its
// whole stall budget. It is a sentinel — replacing raw cancellation or socket
// timeout errors in StallGuard.Cause — so callers can tell a wedged transport
// apart from a context they cancelled themselves (notably the BYOC worker's
// reclaimFault, which treats context.Canceled as routine teardown rather than a
// fault).
var ErrStalled = errors.New("sandbox: call stalled")

// ParseStallTimeout parses an optional environment variable holding a sandbox
// call stall budget. An empty value returns 0 (the caller uses
// DefaultStallTimeout); a malformed or non-positive value fails startup loudly
// rather than silently falling back to the default.
func ParseStallTimeout(name, value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a Go duration: %w", name, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be a positive Go duration", name)
	}
	return d, nil
}

// StallGuard bounds one sandbox transport call by silence rather than by total
// duration: it cancels the child context once neither side has moved bytes for
// the budget, and every byte moved buys another budget. A 500 MB streaming mount
// or a long-running untimed command that keeps printing runs as long as it
// needs; a peer that stops answering fails within the budget with ErrStalled.
//
// Shaped like provider.StallGuard, with Disarm added for timed Exec calls whose
// command is legitimately silent up to ExecRequest.Timeout once the transport
// handshake has succeeded: Disarm stops the silence watcher without cancelling
// the child context, which net/http still holds open for the response body.
type StallGuard struct {
	d        time.Duration
	start    time.Time
	last     atomic.Int64 // nanoseconds since start of the last sign of life
	mu       sync.Mutex
	disarmed bool
	cancel   context.CancelFunc
	tripped  atomic.Bool
	stop     chan struct{}
	stopOnce sync.Once
}

// NewStallGuard returns a child context cancelled once no progress has been
// reported for d, and the guard watching it. A d of zero or less takes
// DefaultStallTimeout. Call Stop when the guarded operation finishes, and pass
// any error through Cause so a stall is reported as ErrStalled.
func NewStallGuard(ctx context.Context, d time.Duration) (context.Context, *StallGuard) {
	if d <= 0 {
		d = DefaultStallTimeout
	}
	gctx, cancel := context.WithCancel(ctx)
	g := &StallGuard{
		d:      d,
		start:  time.Now(),
		cancel: cancel,
		stop:   make(chan struct{}),
	}
	go g.watch(gctx)
	return gctx, g
}

// Budget returns the effective silence budget this guard enforces.
func (g *StallGuard) Budget() time.Duration { return g.d }

// Progress records a sign of life on the transport. Lock-free and monotonic so
// concurrent Read/Write goroutines cannot let an older sample overwrite a newer
// progress timestamp.
func (g *StallGuard) Progress() {
	now := int64(time.Since(g.start))
	for {
		prev := g.last.Load()
		if now <= prev || g.last.CompareAndSwap(prev, now) {
			return
		}
	}
}

// Disarm stops the silence watcher without cancelling the guarded context,
// returning true if it disarmed before the guard tripped and false if the guard
// had already tripped. Callers must still call Stop when the operation ends to
// release the child context.
func (g *StallGuard) Disarm() bool {
	g.mu.Lock()
	if g.tripped.Load() {
		g.mu.Unlock()
		return false
	}
	g.disarmed = true
	g.stopOnce.Do(func() { close(g.stop) })
	g.mu.Unlock()
	return true
}

// Stop releases the watcher goroutine and cancels the child context. It is
// idempotent and serialized with the watcher so a call that has already ended
// cannot later flip to ErrStalled.
func (g *StallGuard) Stop() {
	g.mu.Lock()
	g.disarmed = true
	g.stopOnce.Do(func() { close(g.stop) })
	g.mu.Unlock()
	g.cancel()
}

// Trip marks the guard as tripped when an underlying connection deadline (such
// as the raw net.Conn deadline during a Kubernetes SPDY HTTP 101 upgrade)
// fires ahead of the watcher goroutine. If the guard was already disarmed,
// Trip is a no-op.
func (g *StallGuard) Trip() {
	g.mu.Lock()
	tripped := !g.disarmed
	if tripped {
		g.tripped.Store(true)
		g.stopOnce.Do(func() { close(g.stop) })
	}
	g.mu.Unlock()
	if tripped {
		g.cancel()
	}
}

// Cause names a stall as ErrStalled and passes every other error through. When
// the guard tripped, Cause replaces err outright rather than wrapping
// context.Canceled or a socket timeout, so callers never mistake a transport
// stall for caller cancellation.
func (g *StallGuard) Cause(err error) error {
	if g.tripped.Load() {
		return fmt.Errorf("%w: no data for %s", ErrStalled, g.d)
	}
	return err
}

func (g *StallGuard) watch(ctx context.Context) {
	t := time.NewTimer(g.d)
	defer t.Stop()
	for {
		select {
		case <-g.stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			g.mu.Lock()
			if g.disarmed || ctx.Err() != nil {
				g.mu.Unlock()
				return
			}
			// Now-then-last order is deliberate: a scheduling pause between the
			// two reads overstates remaining budget (trips slightly late) rather
			// than understating it and killing a live transfer early.
			if idle := time.Since(g.start) - time.Duration(g.last.Load()); idle < g.d {
				g.mu.Unlock()
				t.Reset(g.d - idle)
				continue
			}
			g.tripped.Store(true)
			g.mu.Unlock()
			g.cancel()
			return
		}
	}
}

// GuardReadCloser wraps a response stream so every byte delivered counts as
// progress, any error after the guard trips surfaces as ErrStalled (even an
// HTTP/1.0 close-delimited EOF triggered by closing the socket), and Close
// stops the guard.
func GuardReadCloser(rc io.ReadCloser, g *StallGuard) io.ReadCloser {
	if rc == nil || g == nil {
		return rc
	}
	return &guardedReadCloser{ReadCloser: rc, guard: g}
}

type guardedReadCloser struct {
	io.ReadCloser
	guard *StallGuard
}

func (r *guardedReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.guard.Progress()
	}
	if err != nil {
		return n, r.guard.Cause(err)
	}
	return n, nil
}

func (r *guardedReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.guard.Stop()
	return err
}

// ProgressReadCloser wraps a request body so every byte read by the transport
// counts as progress without stopping the guard on Close (the response still
// has to arrive after the upload finishes).
func ProgressReadCloser(rc io.ReadCloser, g *StallGuard) io.ReadCloser {
	if rc == nil || g == nil {
		return rc
	}
	return &progressReadCloser{ReadCloser: rc, guard: g}
}

// ProgressReadCloserWithCancel wraps rc like ProgressReadCloser and also closes
// rc if ctx cancels before Close runs. The cancellation callback stays armed
// until rc.Close runs rather than unregistering when response headers arrive,
// because net/http permits full-duplex response-before-upload-completion.
func ProgressReadCloserWithCancel(ctx context.Context, rc io.ReadCloser, g *StallGuard) io.ReadCloser {
	if rc == nil || g == nil {
		return rc
	}
	r := &progressReadCloser{ReadCloser: rc, guard: g}
	if ctx != nil {
		r.bindCancel(ctx)
	}
	return r
}

type progressReadCloser struct {
	io.ReadCloser
	guard     *StallGuard
	closeOnce sync.Once
	closeErr  error
	cancelMu  sync.Mutex
	closed    bool
	stop      func() bool
}

func (r *progressReadCloser) bindCancel(ctx context.Context) {
	stop := context.AfterFunc(ctx, func() { _ = r.Close() })
	r.cancelMu.Lock()
	if r.closed {
		r.cancelMu.Unlock()
		stop()
		return
	}
	r.stop = stop
	r.cancelMu.Unlock()
}

func (r *progressReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.guard.Progress()
	}
	return n, err
}

func (r *progressReadCloser) Close() error {
	r.closeOnce.Do(func() {
		r.closeErr = r.ReadCloser.Close()
		r.cancelMu.Lock()
		r.closed = true
		stop := r.stop
		r.stop = nil
		r.cancelMu.Unlock()
		if stop != nil {
			stop()
		}
	})
	return r.closeErr
}

// GuardRoundTripper wraps rt so every HTTP request and response stream is
// bounded by silence budget d (DefaultStallTimeout when d <= 0). Used by the
// Kubernetes REST client for Pods API calls; SPDY upgrades have their own
// connection-level guard because http.ReadResponse bypasses http.Transport.
func GuardRoundTripper(rt http.RoundTripper, d time.Duration) http.RoundTripper {
	if rt == nil {
		rt = http.DefaultTransport
	}
	return &stallRoundTripper{rt: rt, d: d}
}

type stallRoundTripper struct {
	rt http.RoundTripper
	d  time.Duration
}

func (s *stallRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	gctx, guard := NewStallGuard(req.Context(), s.d)
	r := req.Clone(gctx)
	var rb io.ReadCloser
	if r.Body != nil && r.Body != http.NoBody {
		rb = ProgressReadCloserWithCancel(gctx, r.Body, guard)
		r.Body = rb
	}
	if r.GetBody != nil {
		orig := r.GetBody
		r.GetBody = func() (io.ReadCloser, error) {
			rc, err := orig()
			if err != nil {
				return nil, err
			}
			return ProgressReadCloserWithCancel(gctx, rc, guard), nil
		}
	}
	resp, err := s.rt.RoundTrip(r)
	if err != nil {
		if rb != nil {
			_ = rb.Close()
		}
		guard.Stop()
		return nil, guard.Cause(err)
	}
	guard.Progress()
	if resp.Body == nil {
		guard.Stop()
		return resp, nil
	}
	resp.Body = GuardReadCloser(resp.Body, guard)
	return resp, nil
}
