package queue

import (
	"context"
	"fmt"
	"sync/atomic"
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
	// start is the monotonic base last is measured from; storing an instant
	// instead would compare a wall clock against the ticker's monotonic one —
	// provider.StallGuard measures the model endpoint the same way.
	start time.Time
	last  atomic.Int64 // nanoseconds since start of the last reported progress
}

// KeepLease starts a keeper that extends item's lease to ttl at every ttl/3 until
// Close, and returns a child context cancelled when a renewal fails (the lease is
// lost). Run the work under the returned context and call Close when it finishes;
// Close reports the first renewal failure, if any.
//
// stall bounds how long the holder may report no progress before the keeper gives
// up on it: the work is cancelled and, deliberately, the lease is left to lapse,
// so the item becomes reclaimable exactly as it would if the process had died.
// That is the containment #383 was missing — a wedged sandbox call leaves the row
// untouched, so renewal succeeds forever and the documented crash recovery never
// fires, because nothing crashed. A stall of zero or less keeps a keeper that
// renews for as long as its holder lives, which is what the brain's turn loop
// wants: its silence is bounded a layer down, by provider.StallGuard on the model
// stream itself.
//
// Progress is reported by the holder (a tool finished, a mount landed), never
// inferred here, because only the holder can tell a long step from a stuck one.
// Detection costs up to one tick on top of the budget, since both live on the
// same ticker — and the tick is the shorter of ttl/3 and the budget itself, so a
// lease much longer than the budget cannot stretch that cost with it. One case
// costs more, deliberately: the check and the renewal share this goroutine, so a
// renewal blocked on a stalled database delays the next check by however long it
// blocks. That is bounded by what the lease has left (the renewal's own budget)
// and self-limiting — the blocked attempt either times out, which cancels the
// holder anyway, or returns to a tick that is already buffered and checks at
// once. A second goroutine would close the gap and is not worth its
// synchronisation: the outcome in that window is a cancelled holder either way,
// and only the error naming it differs.
func (q *Queue) KeepLease(ctx context.Context, item *Item, ttl, stall time.Duration) (context.Context, *LeaseKeeper) {
	kctx, cancel := context.WithCancel(ctx)
	k := &LeaseKeeper{cancel: cancel, quit: make(chan struct{}), done: make(chan struct{}), start: time.Now()}
	// Renew at a third of the lease. Guard the degenerate case: a sub-3ns TTL
	// (operator misconfiguration — a lease that short is unusable anyway) would
	// otherwise make the interval zero and panic time.NewTicker.
	interval := ttl / 3
	if interval <= 0 {
		interval = ttl
	}
	// The stall check rides this same tick, so a lease far longer than the stall
	// budget would push detection out with it: at a three-hour TTL the tick is an
	// hour, and a wedge that the budget says costs half an hour would cost an hour
	// and a half — during which this consumer, which processes one item at a time,
	// runs nothing else. The two knobs are independently tunable and neither
	// binary can validate a relation the queue is free to change, so the tick
	// takes the shorter of the two instead. Renewing more often than a third of a
	// lease is only extra UPDATEs; detecting a wedge later than the budget says is
	// the defect (#383).
	if stall > 0 && stall < interval {
		interval = stall
	}
	// When the lease currently held was bought, on this process's monotonic clock.
	// Stamped here rather than inside the goroutine so that scheduling delay is not
	// added to the lease as though it were lease time; the residue is Claim's own
	// round trip, which no anchor on this side of the wire can remove. That leaves
	// this *later* than the instant the database bought the lease — later and never
	// earlier, the same deliberate direction as the renewal anchor below, so the
	// budget can outlast the real lease but never fall short of it. Nothing of a
	// lost lease commits either way, and it takes both guards to say so: Extend's
	// `lease_expires_at = $2` fails a renewal once another claimant has taken the
	// item, and the settling Complete/Requeue carry the same proof, which is what
	// covers a reclaim landing after the keeper has closed healthy. What the
	// overstatement does cost is a holder working past the point that could
	// happen, so the gap is worth keeping small rather than calling it free.
	bought := time.Now()
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
				// Checked before renewing, so a wedged item's lease is never
				// bought another interval it cannot use. Written as
				// elapsed-minus-last rather than last-plus-stall: the left side
				// cannot underflow (last is an elapsed time this keeper stored,
				// so never ahead of now), while the right side would wrap
				// negative — and stall instantly — for a budget near the
				// duration ceiling, which time.ParseDuration accepts.
				if stall > 0 && time.Since(k.start)-time.Duration(k.last.Load()) > stall {
					k.failed = ErrWorkStalled
					k.cancel()
					return
				}
				// Bounded: Close waits for this goroutine, so an Extend blocked on
				// an exhausted pool or a stalled database would otherwise hang the
				// holder forever. The budget is what is left of the lease the last
				// successful renewal bought, so an attempt that overruns it has
				// outlived that lease, and abandoning the turn is the safe reaction
				// rather than a guess.
				//
				// Safe, not certain: an UPDATE that commits while this client's
				// deadline fires renews a lease the caller never gets to see, so a
				// timeout here can still discard a turn whose row is in fact leased.
				// Closing that window needs the keeper to resynchronise the lease
				// value it holds, not a wider budget, and it is unobserved — #400.
				//
				// Measured from that renewal rather than assumed to be `ttl-ttl/3`,
				// and #392's review is why. A Ticker buffers a tick and drops the
				// rest, so a renewal that takes longer than one interval is followed
				// by a tick that is *already waiting*: the next attempt starts at
				// once, not an interval later. With a fixed `ttl-ttl/3` its deadline
				// then fell well inside a lease that renewal had just extended, and
				// a timeout there abandoned a turn this brain still owned — exactly
				// the false positive #392 was filed about, reachable by construction
				// on any host slow enough to make one renewal outlast an interval.
				// Subtracting the elapsed time restores the invariant in both cases:
				// punctual ticks still get `ttl-ttl/3`, and a late one gets only what
				// the lease has left.
				//
				// A duration, not the lease timestamp: the deadline must never
				// depend on the database clock and this process's agreeing on the
				// time of day. Measuring elapsed time still asks them to agree on
				// the rate of a second, which is a far weaker assumption and the
				// only one this can be built on.
				budget := ttl - time.Since(bought)
				if budget <= 0 {
					// A tick this late means the goroutine was starved for a whole
					// lease; there is nothing left to renew.
					k.failed = fmt.Errorf("queue: keep lease %s: %w", item.ID, ErrLeaseLost)
					k.cancel()
					return
				}
				ectx, ecancel := context.WithTimeout(kctx, budget)
				err := q.Extend(ectx, item, ttl)
				ecancel()
				if err != nil {
					k.failed = err
					k.cancel() // aborts the in-flight tool run or provider stream
					return
				}
				// Stamped on return, not before the call, and the asymmetry is
				// deliberate. The database bought this lease partway through the
				// round trip, so a timestamp taken here is *later* than the real
				// purchase and the budget it feeds is therefore never short of
				// what the lease has left. Anchoring before the call would invert
				// that: the budget would understate a slow round trip, and an
				// attempt could time out while the lease was still live — which is
				// the false positive this whole change exists to remove. The cost
				// of the direction chosen is that a holder can go on working past
				// an expiry it cannot see. What that cannot do is commit the turn:
				// settlement runs only once Close reports the keeper healthy, and
				// Complete/Requeue carry the lease as their proof. The overdue
				// Extend itself may well succeed — the guard matches the timestamp
				// it is replacing, not the wall clock, so an item nobody reclaimed
				// is simply re-extended. It is a reclaim, never the clock, that
				// turns a renewal into ErrLeaseLost.
				bought = time.Now()
			}
		}
	}()
	return kctx, k
}

// Progress reports that the work has moved: another tool answered, another mount
// landed. It is what keeps a long but healthy item alive, so it belongs at the
// steps a wedge would stop — never on a timer, which would report progress a
// wedged holder is not making. Safe from any goroutine. A keeper started with no
// stall budget ignores what it records, but the store still happens, so a caller
// that reports on a hot path pays for it whether or not anything reads it.
func (k *LeaseKeeper) Progress() { k.last.Store(int64(time.Since(k.start))) }

// Close stops the keeper and reports the first extension failure. The goroutine
// has exited when Close returns, so the item's lease value is stable again for
// the settling append to use as its ownership proof.
func (k *LeaseKeeper) Close() error {
	close(k.quit)
	<-k.done
	k.cancel()
	return k.failed
}
