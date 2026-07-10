package events

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/puddle/v2"
)

// Broker fans Postgres NOTIFY traffic out to in-process stream subscribers.
// One listening connection per process serves every subscriber (multi-replica
// control planes each run their own), and it is only held while subscribers
// exist: the listener starts with the first Subscribe and stops with the last
// Close, so idle processes and finished tests don't pin a connection.
type Broker struct {
	pool *pgxpool.Pool

	mu           sync.Mutex
	subs         map[domain.ID]map[*Subscription]struct{}
	listenCancel context.CancelFunc // non-nil while the listener goroutine runs
	ready        chan struct{}      // closed while a LISTEN is active
}

func NewBroker(pool *pgxpool.Pool) *Broker {
	return &Broker{
		pool:  pool,
		subs:  make(map[domain.ID]map[*Subscription]struct{}),
		ready: make(chan struct{}),
	}
}

// Ready blocks until the shared listener holds an active LISTEN. Subscribers
// that snapshot their starting log position after Ready returns cannot miss a
// wake for anything committed after the snapshot; any later coverage lapse
// re-wakes everyone on reconnect.
func (b *Broker) Ready(ctx context.Context) error {
	b.mu.Lock()
	ch := b.ready
	b.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Subscription delivers two kinds of traffic for one session: Wake, a
// coalesced "new committed events may exist, re-read the log" signal, and
// Frames, ephemeral broadcast frames (previews, session.deleted) in arrival
// order. Frames are best-effort by contract — a subscriber that can't keep
// up loses frames, never log events.
type Subscription struct {
	broker    *Broker
	sessionID domain.ID
	wake      chan struct{}
	frames    chan json.RawMessage
	closeOnce sync.Once
	// lossyDeltas is set (under the broker mutex) when an event_delta had
	// to be dropped: from then until the next event_start, further deltas
	// are suppressed so the subscriber sees a clean prefix that simply
	// stopped early — the wire contract's "best effort" — never a text
	// with an interior hole.
	lossyDeltas bool
}

func (s *Subscription) Wake() <-chan struct{}          { return s.wake }
func (s *Subscription) Frames() <-chan json.RawMessage { return s.frames }

func (s *Subscription) Close() {
	s.closeOnce.Do(func() {
		b := s.broker
		b.mu.Lock()
		defer b.mu.Unlock()
		if set := b.subs[s.sessionID]; set != nil {
			delete(set, s)
			if len(set) == 0 {
				delete(b.subs, s.sessionID)
			}
		}
		if len(b.subs) == 0 && b.listenCancel != nil {
			b.listenCancel()
			b.listenCancel = nil
		}
	})
}

// Subscribe registers for one session's live traffic, starting the shared
// listener if it isn't running. Callers must Close the subscription.
func (b *Broker) Subscribe(sessionID domain.ID) *Subscription {
	s := &Subscription{
		broker:    b,
		sessionID: sessionID,
		wake:      make(chan struct{}, 1),
		frames:    make(chan json.RawMessage, 256),
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs[sessionID] == nil {
		b.subs[sessionID] = make(map[*Subscription]struct{})
	}
	b.subs[sessionID][s] = struct{}{}
	if b.listenCancel == nil {
		// A previous listener may have exited with ready still closed;
		// fresh coverage starts unconfirmed.
		select {
		case <-b.ready:
			b.ready = make(chan struct{})
		default:
		}
		ctx, cancel := context.WithCancel(context.Background())
		b.listenCancel = cancel
		go b.listen(ctx)
	}
	return s
}

// listen holds one dedicated connection on LISTEN and dispatches
// notifications until the last subscriber leaves or the pool closes. On any
// connection error it reconnects and wakes every subscriber, so a gap in
// LISTEN coverage can only ever delay a wake, never lose a log event.
func (b *Broker) listen(ctx context.Context) {
	for ctx.Err() == nil {
		b.setReady(ctx, false)
		conn, err := b.pool.Acquire(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, puddle.ErrClosedPool) {
				return
			}
			time.Sleep(200 * time.Millisecond)
			continue
		}
		_, err = conn.Exec(ctx, "LISTEN "+channelEvents+"; LISTEN "+channelFrames)
		if err != nil {
			conn.Release()
			// Same pacing as the Acquire failure path: LISTEN can fail
			// persistently (e.g. a transaction-pooling proxy), and a
			// backoff-free retry would spin a core against the database.
			time.Sleep(200 * time.Millisecond)
			continue
		}
		b.setReady(ctx, true)
		// Heal the window between Subscribe/Append and LISTEN becoming
		// active: everyone re-reads the log once.
		b.wakeAll()
		for {
			n, err := conn.Conn().WaitForNotification(ctx)
			if err != nil {
				break
			}
			b.dispatch(n.Channel, n.Payload)
		}
		// The wait ended by cancellation or a broken connection; destroy
		// the conn rather than returning LISTEN state to the pool.
		_ = conn.Conn().Close(context.Background())
		conn.Release()
	}
}

// setReady flips the coverage gate. A canceled listener never touches it —
// the next generation (see Subscribe) owns it from then on.
func (b *Broker) setReady(ctx context.Context, ready bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ctx.Err() != nil {
		return
	}
	closed := true
	select {
	case <-b.ready:
	default:
		closed = false
	}
	if ready && !closed {
		close(b.ready)
	} else if !ready && closed {
		b.ready = make(chan struct{})
	}
}

func (b *Broker) wakeAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, set := range b.subs {
		for s := range set {
			select {
			case s.wake <- struct{}{}:
			default:
			}
		}
	}
}

func (b *Broker) dispatch(channel, payload string) {
	switch channel {
	case channelEvents:
		var m struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal([]byte(payload), &m) != nil {
			return
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		for s := range b.subs[domain.ID(m.SessionID)] {
			select {
			case s.wake <- struct{}{}:
			default: // already signalled; wakes coalesce
			}
		}
	case channelFrames:
		var m struct {
			SessionID string          `json:"session_id"`
			Frame     json.RawMessage `json:"frame"`
		}
		if json.Unmarshal([]byte(payload), &m) != nil {
			return
		}
		var ft struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(m.Frame, &ft)
		b.mu.Lock()
		defer b.mu.Unlock()
		for s := range b.subs[domain.ID(m.SessionID)] {
			// A fresh preview generation clears the lossy state: chunk
			// suppression is per preview, not per connection.
			if ft.Type == "event_start" {
				s.lossyDeltas = false
			}
			if ft.Type == "event_delta" && s.lossyDeltas {
				continue
			}
			select {
			case s.frames <- m.Frame:
			default:
				// Best-effort: a stalled subscriber drops frames — but a
				// dropped delta poisons the rest of its preview (see
				// lossyDeltas) so partial text is a prefix, never holed.
				if ft.Type == "event_delta" {
					s.lossyDeltas = true
				}
			}
		}
	}
}
