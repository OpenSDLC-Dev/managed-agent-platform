package queue

import (
	"context"
	"time"
)

// LeaseKeeper renews a claimed item's lease on a timer while its holder works, so
// a long tool run or model stream cannot let the lease lapse and hand the item to
// a second claimant mid-work. Each renewal is bounded by the lease it is racing,
// so a stalled database cannot hang the holder behind an unreturnable Extend, and
// losing the lease cancels the work context the holder runs under.
//
// One keeper serves both consumers — the brain's turn loop (a model can think
// far longer than any inter-chunk gap, e.g. a long time-to-first-token on a big
// replayed context) and the executor's item processing (a slow image pull or a
// long-running tool). The timing semantics below are subtle enough that they
// must exist once, not once per consumer.
type LeaseKeeper struct {
	cancel context.CancelFunc
	quit   chan struct{}
	done   chan struct{}
	failed error // written once by the goroutine before done closes
}

// KeepLease starts a keeper that extends item's lease to ttl at every ttl/3 until
// Close, and returns a child context cancelled when a renewal fails (the lease is
// lost). Run the work under the returned context and call Close when it finishes;
// Close reports the first renewal failure, if any.
func (q *Queue) KeepLease(ctx context.Context, item *Item, ttl time.Duration) (context.Context, *LeaseKeeper) {
	kctx, cancel := context.WithCancel(ctx)
	k := &LeaseKeeper{cancel: cancel, quit: make(chan struct{}), done: make(chan struct{})}
	// Renew at a third of the lease. Guard the degenerate case: a sub-3ns TTL
	// (operator misconfiguration — a lease that short is unusable anyway) would
	// otherwise make the interval zero and panic time.NewTicker.
	interval := ttl / 3
	if interval <= 0 {
		interval = ttl
	}
	go func() {
		defer close(k.done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-k.quit:
				return
			case <-kctx.Done():
				return
			case <-t.C:
				// Bounded: Close waits for this goroutine, so an Extend blocked
				// on an exhausted pool or a stalled database would otherwise hang
				// the holder forever. The budget is the lease the last renewal
				// bought minus the tick we waited — an Extend that overruns it has
				// let the lease lapse anyway. A duration, not the lease timestamp:
				// the deadline must not depend on agreement between the database
				// clock and this process's.
				ectx, ecancel := context.WithTimeout(kctx, ttl-ttl/3)
				err := q.Extend(ectx, item, ttl)
				ecancel()
				if err != nil {
					k.failed = err
					k.cancel() // aborts the in-flight tool run or provider stream
					return
				}
			}
		}
	}()
	return kctx, k
}

// Close stops the keeper and reports the first extension failure. The goroutine
// has exited when Close returns, so the item's lease value is stable again for
// the settling append to use as its ownership proof.
func (k *LeaseKeeper) Close() error {
	close(k.quit)
	<-k.done
	k.cancel()
	return k.failed
}
