package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultStallTimeout is how long a model endpoint may say nothing at all
// before its turn is abandoned, when a route configures no stall_timeout of its
// own. It is the anthropic-sdk-go's own judgment for the same hazard — that
// SDK's defaultResponseHeaderTimeout — because the worst legitimate silence is
// the same wait it bounds: an endpoint that queues a request sends no response
// header until it starts generating. Sized to never end a healthy turn;
// operators who know their endpoint answers faster tighten it per route.
const DefaultStallTimeout = 10 * time.Minute

// ErrStalled reports that an endpoint accepted the request and then went silent
// for its whole stall budget. It is a model-side failure like any other — the
// brain settles it as a session.error rather than abandoning the turn to its
// lease — and it exists as a sentinel so a caller can tell an endpoint that
// stopped answering from a context its own caller cancelled, which are the same
// cancellation on the wire.
var ErrStalled = errors.New("model endpoint stalled")

// StallGuard bounds a turn by the endpoint's silence rather than by its
// duration: it cancels the request context once nothing has arrived for the
// budget, and every sign of life on the wire buys another budget. Duration
// alone cannot be the bound — a model streaming a large answer legitimately
// holds one request open for many minutes, while an endpoint that completes the
// handshake and then never speaks holds it open forever (#121). Both halves of
// that hang are one silence: the wait for response headers, which the anthropic
// SDK's own timeout covers only when the caller lets it install its HTTP
// client, and a stream that dies mid-SSE, which nothing covered at all.
//
// It guards the request context rather than the HTTP client, deliberately.
// Registered on a client instead, a header timeout surfaces as a transport
// error the SDK retries — three wedged attempts, three budgets — while a
// per-route client would give every provider instance its own connection pool,
// which the registry's per-turn construction cannot afford (see Registry).
//
// The shape mirrors queue.KeepLease: the constructor returns the context to run
// the work under, the work reports progress, and Stop releases the watcher.
//
// Three limits are worth naming, because what this measures is strictly "no
// response byte was read for the budget". A consumer that stops reading charges
// its own slowness to the endpoint's budget (between chunks the brain does one
// event append, so only a database stall of a whole budget could reach it — at
// the default it cannot, at a tightened per-route one it could, and the turn
// would then blame the model for Postgres). A request body still being uploaded
// counts as silence too, which is the right reading: a peer too slow to receive
// the request is the wedged peer this bounds. And a peer that dribbles one byte
// per budget is never bounded at all, by construction — every byte buys another
// budget. Bounding that one needs a total deadline, which would have to be sized
// for the longest healthy turn and so would not bound anything worth bounding.
type StallGuard struct {
	d time.Duration
	// start is the monotonic base last is measured from; storing an instant
	// instead would compare a wall clock against the timer's monotonic one.
	start   time.Time
	last    atomic.Int64 // nanoseconds since start of the last sign of life
	cancel  context.CancelFunc
	tripped atomic.Bool
	stop    chan struct{}
	once    sync.Once
}

// NewStallGuard returns a child context cancelled once the endpoint has been
// silent for d, and the guard watching it. A d of zero or less takes
// DefaultStallTimeout. Run the request under the returned context, hand its
// response body to ProgressBody, call Stop when the stream is finished with,
// and pass any error the stream reports through Cause so a stall is named as
// one.
func NewStallGuard(ctx context.Context, d time.Duration) (context.Context, *StallGuard) {
	if d <= 0 {
		d = DefaultStallTimeout
	}
	gctx, cancel := context.WithCancel(ctx)
	g := &StallGuard{d: d, start: time.Now(), cancel: cancel, stop: make(chan struct{})}
	go g.watch(gctx)
	// The guard rides the context so a response body can be wrapped wherever
	// the request is actually executed. The anthropic adapter never touches its
	// own body — the SDK owns it — and reaches it only from a middleware, which
	// is handed nothing but the request.
	return context.WithValue(gctx, guardKey{}, g), g
}

type guardKey struct{}

// ProgressBody wraps a response body so that every byte it delivers is a sign
// of life for the stall guard ctx carries, returning it unchanged when there is
// none. Liveness is measured in bytes rather than in protocol frames because
// the frames that prove a quiet endpoint is alive never reach an adapter: the
// SDK's stream decoder swallows Anthropic's ping events, and an SSE comment is
// not an event at all. A guard fed only by content would kill a model that is
// still thinking.
func ProgressBody(ctx context.Context, body io.ReadCloser) io.ReadCloser {
	g, _ := ctx.Value(guardKey{}).(*StallGuard)
	if g == nil || body == nil {
		return body
	}
	// The response reaching here is itself a sign of life — its headers arrived.
	// Without this the two waits share one budget: an endpoint that spends most
	// of it on headers and then thinks for the rest is cancelled although
	// neither silence lasted a whole budget. It also re-arms on each of the
	// SDK's retry attempts, since the middleware wraps every one of them.
	g.Progress()
	return &progressBody{ReadCloser: body, guard: g}
}

type progressBody struct {
	io.ReadCloser
	guard *StallGuard
}

func (b *progressBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.guard.Progress()
	}
	return n, err
}

func (g *StallGuard) watch(ctx context.Context) {
	t := time.NewTimer(g.d)
	defer t.Stop()
	for {
		select {
		case <-g.stop:
			return
		case <-ctx.Done():
			// The parent ended the turn (a lost lease, a shutdown) or Stop
			// cancelled ours; either way there is nothing left to guard.
			return
		case <-t.C:
			// Measured from the last sign of life, not from when the timer was
			// armed: progress that arrived while it ran reschedules it for the
			// remaining budget instead of tripping on a stale deadline. Without
			// that, a stream delivering steadily could still be killed by a
			// timer armed a whole budget ago.
			//
			// The now-then-last sampling order is deliberate. Descheduled between
			// the two reads, this over-states the remaining budget and trips late
			// by that interval; reading last first would under-state it and trip
			// early on progress that had just arrived. Late is the safe direction:
			// it delays a wedged endpoint's failure by a scheduling gap, where
			// early would end a healthy turn.
			if idle := time.Since(g.start) - time.Duration(g.last.Load()); idle < g.d {
				t.Reset(g.d - idle)
				continue
			}
			// Order matters: Cause is read by whoever the cancellation wakes, so
			// the verdict must be visible before the wait it ends.
			g.tripped.Store(true)
			g.cancel()
			return
		}
	}
}

// Progress records a sign of life from the endpoint. It is deliberately cheap
// and lock-free: ProgressBody calls it on every read that delivered bytes, so
// it runs far more often than once per turn.
func (g *StallGuard) Progress() { g.last.Store(int64(time.Since(g.start))) }

// Stop releases the guard and the context it returned. It is idempotent, so a
// stream may be closed more than once.
func (g *StallGuard) Stop() {
	g.once.Do(func() { close(g.stop) })
	g.cancel()
}

// Cause names a stall as one, and passes anything else through. A tripped guard
// replaces the error outright rather than wrapping it: what the aborted read
// actually reports is "context canceled", which says nothing about who
// cancelled or why and would leave the session.error indistinguishable from a
// shutdown.
//
// One case it labels wrongly, knowingly: the anthropic SDK honors an upstream
// Retry-After verbatim and uncapped, and sleeps it out under this context. A
// backoff longer than the budget is therefore cut short — a good bound, since an
// uncapped Retry-After is a hang of its own — but reported as a stall, and the
// endpoint's 429 is lost with it. Nothing distinguishes the SDK's own sleep from
// the endpoint's silence from outside the SDK.
func (g *StallGuard) Cause(err error) error {
	if g.tripped.Load() {
		return fmt.Errorf("%w: no data for %s", ErrStalled, g.d)
	}
	return err
}
