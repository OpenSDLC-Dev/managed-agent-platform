package gate

import "sync"

const (
	// defaultMaxConcurrentRequests bounds in-flight plain-HTTP proxy requests.
	defaultMaxConcurrentRequests = 64

	// defaultMaxConcurrentTunnels bounds active CONNECT tunnels independently
	// of plain-HTTP requests so idle tunnels cannot starve plain-HTTP traffic.
	defaultMaxConcurrentTunnels = 64

	// defaultMaxTotalBodyBytes bounds aggregate plain-HTTP request body bytes
	// buffered across all concurrent requests at once. Sized well below the
	// gate container's 256 MiB memory ceiling to leave headroom for substitution
	// copies and runtime overhead.
	defaultMaxTotalBodyBytes int64 = 32 << 20 // 32 MiB
)

// LimitConfig configures a Limiter. Non-positive fields select the package
// defaults so a zero value always enforces bounded work and buffering.
type LimitConfig struct {
	MaxConcurrentRequests int
	MaxConcurrentTunnels  int
	MaxTotalBodyBytes     int64
}

// Limiter bounds concurrent in-flight proxy work and aggregate plain-HTTP body
// buffering. A single Limiter is meant to be shared across policy swaps (see
// gaterun.SwappableHandler) so periodic config refreshes never reset or double
// the active resource budgets (plan 70 slice 2).
type Limiter struct {
	maxRequests   int
	maxTunnels    int
	maxTotalBytes int64

	mu            sync.Mutex
	inFlightReqs  int
	inFlightTuns  int
	bufferedBytes int64
}

// NewLimiter returns a Limiter enforcing cfg, applying package defaults to any
// non-positive field.
func NewLimiter(cfg LimitConfig) *Limiter {
	maxReq := cfg.MaxConcurrentRequests
	if maxReq <= 0 {
		maxReq = defaultMaxConcurrentRequests
	}
	maxTun := cfg.MaxConcurrentTunnels
	if maxTun <= 0 {
		maxTun = defaultMaxConcurrentTunnels
	}
	maxBytes := cfg.MaxTotalBodyBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxTotalBodyBytes
	}
	return &Limiter{
		maxRequests:   maxReq,
		maxTunnels:    maxTun,
		maxTotalBytes: maxBytes,
	}
}

func (l *Limiter) acquireRequest() (release func(), ok bool) {
	l.mu.Lock()
	if l.inFlightReqs >= l.maxRequests {
		l.mu.Unlock()
		return nil, false
	}
	l.inFlightReqs++
	l.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.inFlightReqs--
			l.mu.Unlock()
		})
	}, true
}

func (l *Limiter) acquireTunnel() (release func(), ok bool) {
	l.mu.Lock()
	if l.inFlightTuns >= l.maxTunnels {
		l.mu.Unlock()
		return nil, false
	}
	l.inFlightTuns++
	l.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.inFlightTuns--
			l.mu.Unlock()
		})
	}, true
}

func (l *Limiter) hasBodyCapacity(n int64) bool {
	if n <= 0 {
		return true
	}
	l.mu.Lock()
	ok := l.bufferedBytes+n <= l.maxTotalBytes
	l.mu.Unlock()
	return ok
}

func (l *Limiter) acquireBodyBytes(n int64) bool {
	if n <= 0 {
		return true
	}
	l.mu.Lock()
	if l.bufferedBytes+n > l.maxTotalBytes {
		l.mu.Unlock()
		return false
	}
	l.bufferedBytes += n
	l.mu.Unlock()
	return true
}

func (l *Limiter) releaseBodyBytes(n int64) {
	if n <= 0 {
		return
	}
	l.mu.Lock()
	l.bufferedBytes -= n
	if l.bufferedBytes < 0 {
		l.bufferedBytes = 0
	}
	l.mu.Unlock()
}
