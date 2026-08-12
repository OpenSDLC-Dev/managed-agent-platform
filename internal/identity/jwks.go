package identity

import (
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"slices"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// verifyKey is one usable public key from the key set.
type verifyKey struct {
	alg string // the JWK's declared alg, or "" when it declares none
	pub any    // *rsa.PublicKey | *ecdsa.PublicKey
}

// keySet is one URL's kid-indexed public keys, with a bounded lifetime.
type keySet struct {
	url      string
	client   *http.Client
	algs     map[string]bool
	now      func() time.Time
	ttl      time.Duration
	cooldown time.Duration
	// timeout is the per-fetch deadline. A field rather than the constant so a
	// test can drive the deadline branch in milliseconds.
	timeout time.Duration

	mu        sync.Mutex
	keys      map[string]verifyKey
	fetchedAt time.Time     // when keys was last REPLACED (success only)
	lastTry   time.Time     // when a fetch was last ATTEMPTED (any outcome)
	inflight  chan struct{} // non-nil while a fetch runs; closed on completion
}

// newKeySet builds an empty, cold key set for one URL.
func newKeySet(url string, client *http.Client, algs []string, now func() time.Time) *keySet {
	allowed := make(map[string]bool, len(algs))
	for _, a := range algs {
		allowed[a] = true
	}
	return &keySet{
		url:      url,
		client:   client,
		algs:     allowed,
		now:      now,
		ttl:      keySetTTL,
		cooldown: refreshCooldown,
		timeout:  fetchTimeout,
		keys:     map[string]verifyKey{},
	}
}

// get returns the key for kid, refreshing at most once.
//
// The invariant: the rate limit may suppress a FETCH; it may never extend a
// KEY'S LIFE.
//
// Ordering matters in three places:
//
//  1. Freshness is checked before the map lookup, so a stale set never serves a
//     hit. That is the whole TTL guarantee.
//  2. inflight is checked before the cooldown. Checking the cooldown first would
//     refuse a request arriving a millisecond after the leader stamped lastTry,
//     instead of letting it join the in-flight fetch about to produce its key —
//     spurious 401s on legitimate traffic during every key rotation.
//  3. The cooldown is evaluated on the stale path too, not only when the set is
//     fresh. Otherwise, past the TTL — where a failed fetch deliberately leaves
//     fetchedAt untouched, so the set stays stale — every request would lead a
//     fresh flight, making this process an outbound amplifier during an IdP
//     outage.
//
// Straight-line, no loop: at most one wait-or-fetch per call, then one final
// lookup, so a persistently failing IdP cannot livelock a request.
func (k *keySet) get(ctx context.Context, kid string) (verifyKey, error) {
	k.mu.Lock()
	// The unlock is deferred rather than written on each path because leadFlight
	// runs the fetch — arbitrary code, including a third-party JSON decoder. A
	// panic there unwinds through this function, and net/http recovers a handler
	// panic per connection: the process would survive with this mutex held
	// forever, which is every later authentication deadlocked. The two paths that
	// release the lock re-acquire it before returning, so the defer is always
	// balanced.
	defer k.mu.Unlock()

	if key, ok := k.lookupLocked(kid); ok {
		return key, nil
	}
	switch {
	case k.inflight != nil:
		ch := k.inflight
		k.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			// This caller abandons; the shared fetch continues for the others.
			k.mu.Lock()
			return verifyKey{}, reject("key set unavailable")
		}
		k.mu.Lock()
	case k.now().Sub(k.lastTry) < k.cooldown:
		// Rate-limited: fall through to the final lookup with the cache as it
		// stands. A stale cache yields nothing, which is the fail-closed answer.
		//
		// The kid is logged truncated and only here: it is the one diagnostic
		// that answers "which key did the provider rotate to" during a failed
		// rotation, and it is attacker-supplied, so it is bounded (slog escapes
		// control bytes but does not bound length) and confined to Debug.
		slog.DebugContext(ctx, "identity: key-set refresh rate-limited",
			"url", redactURL(k.url), "kid", truncate(kid, maxLoggedKID))
	default:
		k.leadFlight(ctx) // releases and re-acquires k.mu
	}
	key, ok := k.lookupLocked(kid)
	if !ok {
		return verifyKey{}, reject("unknown kid")
	}
	return key, nil
}

// lookupLocked returns the key only while the set is within its TTL. Freshness
// before the map lookup is what makes the bounded lifetime real. Caller holds
// k.mu.
func (k *keySet) lookupLocked(kid string) (verifyKey, bool) {
	if k.now().Sub(k.fetchedAt) >= k.ttl {
		return verifyKey{}, false
	}
	key, ok := k.keys[kid]
	return key, ok
}

// install replaces the key set and stamps both clocks from ONE reading. The two
// timestamps are one instant by design — the TTL and the cooldown must not start
// from two different ones — and this is the only writer, so New's warm-up cannot
// drift from what a refresh does. Takes k.mu itself.
func (k *keySet) install(keys map[string]verifyKey) {
	stamp := k.now()
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys, k.fetchedAt, k.lastTry = keys, stamp, stamp
}

// leadFlight performs one fetch as the flight leader. The caller holds k.mu on
// entry; leadFlight releases it for the duration of the fetch and holds it again
// on return.
//
// Completion is deferred so the flight always finishes: a panicking fetch would
// otherwise leave inflight non-nil and block every later caller on a channel
// nobody closes. stamp is read BEFORE the deferred cleanup rather than inside it
// for the same reason — k.now() is caller-supplied, and a clock that panics
// during cleanup would strand inflight non-nil with its channel never closed,
// which is the exact failure the defer exists to prevent. ok is set on the
// success line alone, so a panic can neither install a nil key set nor stamp
// fetchedAt.
func (k *keySet) leadFlight(ctx context.Context) {
	ch := make(chan struct{})
	k.inflight, k.lastTry = ch, k.now()
	k.mu.Unlock()

	var fetched map[string]verifyKey
	var stamp time.Time
	ok := false
	defer func() {
		k.mu.Lock()
		if ok {
			k.keys, k.fetchedAt = fetched, stamp
		}
		k.inflight = nil
		close(ch)
	}()

	var err error
	// The fetch is detached from this caller's cancellation (its trace context
	// still rides along) because it is shared: the request that happened to lead
	// the flight hanging up must not fail every other caller waiting on it.
	// Waiters honour their own context on the flight channel instead. Startup
	// calls fetch directly and so keeps its deadline.
	if fetched, err = k.fetch(context.WithoutCancel(ctx)); err != nil {
		slog.WarnContext(ctx, "identity: key set fetch failed", "url", redactURL(k.url), "err", err)
		return
	}
	stamp = k.now()
	slog.InfoContext(ctx, "identity: key set refreshed", "url", redactURL(k.url), "keys", len(fetched))
	ok = true
}

// fetch performs one bounded round trip and decodes the result.
func (k *keySet) fetch(ctx context.Context) (map[string]verifyKey, error) {
	var raw json.RawMessage
	if err := getJSON(ctx, k.client, k.url, k.timeout, &raw); err != nil {
		return nil, err
	}
	return parseKeySet(raw, k.algs, k.url)
}

// rawKeySet decodes the set one entry at a time.
//
// This is deliberately not json.Unmarshal(body, &jose.JSONWebKeySet{}), and the
// difference is an availability bug rather than a style preference.
// jose.JSONWebKeySet is a bare struct with no set-level UnmarshalJSON, and
// JSONWebKey.UnmarshalJSON returns an error for any kty it cannot build —
// including OKP with a curve other than Ed25519. So one entry a provider is
// entitled to publish (an X25519 encryption key, a future kty) would fail the
// ENTIRE set and take every RS256 key with it: a boot failure, and after the TTL
// a permanently stale set — uniform 401s for every human until the provider
// removes the key. RFC 7517 §5 says implementations should ignore unusable
// entries.
type rawKeySet struct {
	Keys []json.RawMessage `json:"keys"`
}

// rawKeyMeta carries the three fields jose.JSONWebKey cannot report usefully:
// key_ops (absent from its raw struct entirely), use, and the RSA exponent as
// PUBLISHED. E has to come from here because by the time go-jose hands over an
// rsa.PublicKey the exponent is already int(big.Int.Int64()) (encoding.go), and
// Int64 on an oversized value yields its low 64 bits — so a published exponent
// of 2^64+65537 arrives as a perfectly ordinary 65537 and no check applied
// afterwards can tell the difference.
type rawKeyMeta struct {
	Use    string   `json:"use"`
	KeyOps []string `json:"key_ops"`
	E      string   `json:"e"`
}

// parseKeySet decodes and filters, returning the kid-indexed usable keys.
//
// A set yielding zero usable keys is an error, so a failed parse never replaces a
// working cache with an empty one.
func parseKeySet(body []byte, algs map[string]bool, url string) (map[string]verifyKey, error) {
	safe := redactURL(url)
	var raw rawKeySet
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode key set %s: %w", safe, err)
	}
	if len(raw.Keys) > maxKeys {
		return nil, fmt.Errorf("key set %s has %d keys, limit %d", safe, len(raw.Keys), maxKeys)
	}
	out := make(map[string]verifyKey, len(raw.Keys))
	skipped := 0
	for _, entry := range raw.Keys {
		var jwk jose.JSONWebKey
		if err := json.Unmarshal(entry, &jwk); err != nil {
			skipped++
			continue
		}
		var meta rawKeyMeta
		if err := json.Unmarshal(entry, &meta); err != nil {
			skipped++
			continue
		}
		key, ok := usableKey(&jwk, meta, algs)
		if !ok {
			skipped++
			continue
		}
		if _, dup := out[jwk.KeyID]; dup {
			// First wins, by document order. RFC 7517 says kids should be
			// distinct; deterministic beats clever, and an attacker who can inject
			// a second entry already owns the provider.
			skipped++
			continue
		}
		out[jwk.KeyID] = key
	}
	if skipped > 0 {
		// Debug, not Warn: skipping entries is the healthy steady state, not an
		// incident. This function exists BECAUSE a compliant provider publishes
		// keys it cannot use — Keycloak and Entra both put encryption keys beside
		// signing keys — so at Warn every refresh of a correctly configured
		// deployment would log one, and an operator who has learned to filter it
		// is an operator who will filter the one that mattered. A set with
		// nothing usable left is still a hard error below.
		slog.Debug("identity: skipped unusable JWKs", "url", safe, "count", skipped)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("key set %s has no usable keys", safe)
	}
	return out, nil
}

// usableKey applies the checks go-jose does not, after go-jose has decoded the
// entry. Every failing rule skips the entry; none of them fails the set.
//
// go-jose already refuses an unknown curve, a wrong-length coordinate and an
// off-curve point, so EC needs nothing more here. It does not bound an RSA
// modulus, does not require an odd exponent, does not parse key_ops at all, and
// does not enforce a JWK's declared alg.
func usableKey(jwk *jose.JSONWebKey, meta rawKeyMeta, algs map[string]bool) (verifyKey, bool) {
	if jwk.KeyID == "" {
		return verifyKey{}, false // unaddressable: selection is kid-indexed
	}
	if !jwk.Valid() || !jwk.IsPublic() {
		return verifyKey{}, false // drops oct symmetric material and private keys
	}
	if meta.Use != "" && meta.Use != "sig" {
		return verifyKey{}, false // an encryption key must never verify
	}
	if len(meta.KeyOps) > 0 && !slices.Contains(meta.KeyOps, "verify") {
		return verifyKey{}, false
	}
	if jwk.Algorithm != "" && !algs[jwk.Algorithm] {
		return verifyKey{}, false
	}
	switch pub := jwk.Key.(type) {
	case *rsa.PublicKey:
		if bits := pub.N.BitLen(); bits < minRSABits || bits > maxRSABits {
			return verifyKey{}, false
		}
		// An RSA modulus is a product of odd primes, so an even one is not a
		// modulus at all — go-jose checks neither this nor the size above.
		if pub.N.Bit(0) == 0 {
			return verifyKey{}, false
		}
		// Go bounds the exponent's magnitude at verify time but does not require
		// it odd.
		if pub.E < 3 || pub.E%2 == 0 {
			return verifyKey{}, false
		}
		// The ceiling is applied to the PUBLISHED exponent, not to pub.E, and the
		// difference is the whole point: go-jose has already reduced an oversized
		// e to its low 64 bits, so 2^64+65537 reaches here indistinguishable from
		// an ordinary 65537 and would install a key the provider never published.
		// Reading the raw member is the only place the truncation is still
		// visible.
		if !exponentInRange(meta.E) {
			return verifyKey{}, false
		}
	case *ecdsa.PublicKey:
		// Fully validated by go-jose.
	default:
		// Includes ed25519.PublicKey, which IsPublic accepts: EdDSA is not on the
		// allowlist.
		return verifyKey{}, false
	}
	return verifyKey{alg: jwk.Algorithm, pub: jwk.Key}, true
}

// exponentInRange reports whether the JWK's published "e" is within crypto/rsa's
// own ceiling, read from the base64url member rather than from the decoded int.
//
// It is deliberately a bound on the PUBLISHED value: an entry whose exponent
// cannot survive the trip into an rsa.PublicKey intact is skipped, rather than
// installed as a key that verifies signatures the provider cannot make. An
// unparseable member is skipped too — go-jose would have failed the entry
// anyway, so this only decides which check gets there first.
func exponentInRange(raw string) bool {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(b) == 0 {
		return false
	}
	return new(big.Int).SetBytes(b).Cmp(big.NewInt(maxRSAExponent)) <= 0
}

// truncate bounds an attacker-controlled string before it reaches a log. slog
// escapes control bytes but does not bound length, so an unbounded kid would be a
// log-volume amplifier.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
