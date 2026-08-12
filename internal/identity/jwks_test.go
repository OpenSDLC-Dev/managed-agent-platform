package identity_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	identity "github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity/identitytest"
)

// This file is the key set's bounded lifetime, driven end to end through Verify:
// the TTL that stops a removed key verifying, the cooldown that stops an
// attacker-chosen kid amplifying into the provider, and the single flight that
// collapses a rotation's concurrent refreshes into one fetch.
//
// Time is identitytest.Clock and nothing else, and the one blocking primitive is
// identitytest.BlockJWKS with a channel these tests close. No assertion here
// waits on the wall clock.

// jwksXAudience is the audience every verifier and token in this file agrees on.
// Audience policy is verifier_test.go's subject, not this file's.
const jwksXAudience = "console"

// jwksXStart is the fake clock's origin. Every advance below is expressed as
// identity.KeySetTTLForTest or identity.RefreshCooldownForTest, so a test moves
// time by exactly the bound production uses rather than by a number copied out of
// the source.
var jwksXStart = time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)

// jwksXVerifier builds a verifier pinned at the fixture's key set — a set JWKSURL
// skips discovery entirely — with clock driving token expiry, the key-set TTL and
// the refresh cooldown from one place.
func jwksXVerifier(t *testing.T, idp *identitytest.IdP, clock *identitytest.Clock) *identity.Verifier {
	t.Helper()
	return jwksXVerifierWithClient(t, idp, clock, idp.Client())
}

// jwksXVerifierWithClient is jwksXVerifier with the HTTP client spelled out, for
// the one test that needs a client carrying a production property the fixture's
// own does not.
func jwksXVerifierWithClient(t *testing.T, idp *identitytest.IdP, clock *identitytest.Clock, client *http.Client) *identity.Verifier {
	t.Helper()
	v, err := identity.New(context.Background(), identity.Config{
		Mode:     identity.ModeOIDC,
		Issuer:   idp.Issuer(),
		Audience: jwksXAudience,
		JWKSURL:  idp.JWKSURL(),
		RoleMap:  map[string]identity.Role{"eng": identity.RoleDeveloper},
		// The production client's dial guard refuses loopback by design, so
		// anything reaching the fixture supplies its own client.
		HTTPClient: client,
		Now:        clock.Now,
	})
	if err != nil {
		t.Fatalf("identity.New: %v", err)
	}
	return v
}

// jwksXToken mints a valid token under the provider's active key.
func jwksXToken(t *testing.T, idp *identitytest.IdP, clock *identitytest.Clock) string {
	t.Helper()
	return idp.Mint(t, idp.Claims(jwksXAudience, clock.Now()))
}

// jwksXTokenUnderRetiredKey publishes a key, mints under it, then unpublishes it.
// The token is genuinely signed and syntactically perfect, so key lookup is the
// only thing that can reject it: its kid is absent from every key set the
// verifier can fetch from here on.
func jwksXTokenUnderRetiredKey(t *testing.T, idp *identitytest.IdP, clock *identitytest.Clock) string {
	t.Helper()
	kid := idp.AddKey(t, "RS256")
	token := idp.MintWith(t, kid, idp.Claims(jwksXAudience, clock.Now()))
	idp.Retire(t, kid)
	return token
}

// jwksXVerifyAccepted fails the test unless the token verifies.
func jwksXVerifyAccepted(t *testing.T, v *identity.Verifier, token, what string) {
	t.Helper()
	if _, err := v.Verify(context.Background(), token); err != nil {
		t.Fatalf("%s: Verify: %v", what, err)
	}
}

// jwksXVerifyRejected fails the test unless the token is refused as
// unauthenticated, with no identity leaking out alongside the error.
func jwksXVerifyRejected(t *testing.T, v *identity.Verifier, token, what string) {
	t.Helper()
	id, err := v.Verify(context.Background(), token)
	if !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("%s: Verify err = %v, want ErrUnauthenticated", what, err)
	}
	if id != (identity.Identity{}) {
		t.Errorf("%s: Verify returned identity %+v on a rejection, want the zero value", what, id)
	}
}

// jwksXWantFetches asserts the number of key-set requests the provider has served.
func jwksXWantFetches(t *testing.T, idp *identitytest.IdP, want int, why string) {
	t.Helper()
	if got := idp.Fetches(); got != want {
		t.Errorf("Fetches() = %d, want %d: %s", got, want, why)
	}
}

// jwksXAwaitFetch spins until the provider has served n key-set requests.
//
// The counter increments at the top of the handler, before BlockJWKS holds it
// open, so this observes that a flight is genuinely in progress rather than
// guessing at timing — and it never sleeps.
//
// It takes *testing.T and gives up rather than spinning forever. The tests that
// call it have a handler parked on a channel, so the shape a regression takes
// here is "the second fetch never happens": an unbounded spin would then hold
// the whole test binary until the package timeout, with no message naming the
// test. The deadline is wall-clock on purpose — it bounds a hang, and the frozen
// clock every other assertion uses cannot.
func jwksXAwaitFetch(t *testing.T, idp *identitytest.IdP, n int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for idp.Fetches() < n {
		if time.Now().After(deadline) {
			t.Fatalf("waited for %d key-set fetches, provider served %d", n, idp.Fetches())
		}
		runtime.Gosched()
	}
}

// jwksXRelease closes a handler-release channel exactly once, from the test body
// AND from cleanup.
//
// Both callers matter. httptest.Server.Close blocks until every outstanding
// request returns, and NewIdP registers that Close with t.Cleanup — so a t.Fatalf
// between BlockJWKS and the release would leave the handler parked forever and
// the cleanup blocked behind it, turning a failing assertion into a hung binary
// whose message is never printed.
func jwksXRelease(t *testing.T, release chan struct{}) func() {
	t.Helper()
	once := sync.OnceFunc(func() { close(release) })
	t.Cleanup(once)
	return once
}

func TestKeySetColdThenWarm(t *testing.T) {
	t.Parallel()
	idp := identitytest.NewIdP(t)
	clock := identitytest.NewClock(jwksXStart)
	v := jwksXVerifier(t, idp, clock)

	jwksXWantFetches(t, idp, 1, "New warms the set, so an unreachable provider is a boot error")

	token := jwksXToken(t, idp, clock)
	jwksXVerifyAccepted(t, v, token, "first verify")
	jwksXVerifyAccepted(t, v, token, "second verify")
	jwksXWantFetches(t, idp, 1, "a warm, fresh set serves hits without refetching")
}

func TestKeySetRotation(t *testing.T) {
	t.Parallel()
	idp := identitytest.NewIdP(t)
	clock := identitytest.NewClock(jwksXStart)
	v := jwksXVerifier(t, idp, clock)

	old := jwksXToken(t, idp, clock)
	idp.Rotate(t)
	fresh := jwksXToken(t, idp, clock)

	// New stamped lastTry, so the refresh a rotation needs waits out the cooldown.
	clock.Advance(identity.RefreshCooldownForTest)

	jwksXVerifyAccepted(t, v, fresh, "token under the rotated-in kid")
	jwksXWantFetches(t, idp, 2, "the unknown kid led exactly one refresh")

	// A rotation publishes both keys, so tokens already in circulation keep
	// working — and they come from the set the refresh just installed.
	jwksXVerifyAccepted(t, v, old, "token under the rotated-out kid")
	jwksXWantFetches(t, idp, 2, "both kids are in the refreshed set")
}

func TestKeySetUnknownKIDRefreshesOnce(t *testing.T) {
	t.Parallel()
	idp := identitytest.NewIdP(t)
	clock := identitytest.NewClock(jwksXStart)
	v := jwksXVerifier(t, idp, clock)

	token := jwksXTokenUnderRetiredKey(t, idp, clock)
	clock.Advance(identity.RefreshCooldownForTest)

	jwksXVerifyRejected(t, v, token, "kid the provider never published")
	jwksXWantFetches(t, idp, 2, "get refreshes at most once and then answers, never looping")
}

func TestKeySetUnknownKIDRateLimited(t *testing.T) {
	t.Parallel()
	idp := identitytest.NewIdP(t)
	clock := identitytest.NewClock(jwksXStart)
	v := jwksXVerifier(t, idp, clock)

	first := jwksXTokenUnderRetiredKey(t, idp, clock)
	second := jwksXTokenUnderRetiredKey(t, idp, clock)
	clock.Advance(identity.RefreshCooldownForTest)

	jwksXVerifyRejected(t, v, first, "first unknown kid")
	jwksXWantFetches(t, idp, 2, "the first unknown kid is worth one refresh")

	// The kid is attacker-supplied. Without this, a stream of tokens carrying
	// random kids would turn the control plane into an outbound amplifier.
	jwksXVerifyRejected(t, v, second, "second unknown kid inside the cooldown")
	jwksXWantFetches(t, idp, 2, "the cooldown suppresses the second refresh")
}

func TestKeySetUnknownKIDAfterCooldown(t *testing.T) {
	t.Parallel()
	idp := identitytest.NewIdP(t)
	clock := identitytest.NewClock(jwksXStart)
	v := jwksXVerifier(t, idp, clock)

	first := jwksXTokenUnderRetiredKey(t, idp, clock)
	second := jwksXTokenUnderRetiredKey(t, idp, clock)
	third := jwksXTokenUnderRetiredKey(t, idp, clock)
	clock.Advance(identity.RefreshCooldownForTest)

	jwksXVerifyRejected(t, v, first, "first unknown kid")
	jwksXWantFetches(t, idp, 2, "past the cooldown a miss leads a flight")

	jwksXVerifyRejected(t, v, second, "second unknown kid inside the cooldown")
	jwksXWantFetches(t, idp, 2, "rate-limited")

	// The limit is a delay, not a permanent refusal: a real rotation landing
	// seconds after a probe must still be picked up.
	clock.Advance(identity.RefreshCooldownForTest)
	jwksXVerifyRejected(t, v, third, "third unknown kid past a second cooldown")
	jwksXWantFetches(t, idp, 3, "the cooldown elapsed, so a refresh is allowed again")
}

func TestRotationAfterCooldownVerifies(t *testing.T) {
	t.Parallel()
	idp := identitytest.NewIdP(t)
	clock := identitytest.NewClock(jwksXStart)
	v := jwksXVerifier(t, idp, clock)

	idp.Rotate(t)
	token := jwksXToken(t, idp, clock)

	// Inside the cooldown New itself stamped, a rotation costs a rejection and no
	// fetch — the documented price of rotating within the process's first 30s.
	jwksXVerifyRejected(t, v, token, "rotated-in kid inside the cooldown")
	jwksXWantFetches(t, idp, 1, "the cooldown suppressed the refresh")

	clock.Advance(identity.RefreshCooldownForTest)
	jwksXVerifyAccepted(t, v, token, "rotated-in kid past the cooldown")
	jwksXWantFetches(t, idp, 2, "one refresh picked the rotated-in key up")
}

// TestRetiredKeyStopsVerifyingAtTTL is the bounded-key-lifetime requirement as one
// assertion: a key the provider has removed keeps verifying only until the cached
// set expires, and not one request longer.
func TestRetiredKeyStopsVerifyingAtTTL(t *testing.T) {
	t.Parallel()
	idp := identitytest.NewIdP(t)
	clock := identitytest.NewClock(jwksXStart)
	v := jwksXVerifier(t, idp, clock)

	retiring := idp.ActiveKID()
	token := jwksXToken(t, idp, clock)
	jwksXVerifyAccepted(t, v, token, "before the key was retired")

	idp.Rotate(t)           // a successor, so the published set never empties
	idp.Retire(t, retiring) // the provider revokes the signing key

	// Revocation at the provider is invisible while the cached set is fresh. That
	// window is the TTL, which is exactly why the cache is not "refresh on an
	// unknown kid" alone — that design never expires anything.
	jwksXVerifyAccepted(t, v, token, "retired key while the cached set is fresh")
	jwksXWantFetches(t, idp, 1, "a fresh set is served without a round trip")

	clock.Advance(identity.KeySetTTLForTest)
	jwksXVerifyRejected(t, v, token, "retired key at the TTL")
	jwksXWantFetches(t, idp, 2, "an expired set is refetched, never served")

	// And it stays rejected: the refreshed set simply does not hold the key.
	jwksXVerifyRejected(t, v, token, "retired key after the refresh")
	jwksXWantFetches(t, idp, 2, "the refreshed set answers without another fetch")
}

func TestTTLForcesRefetchEvenOnAHit(t *testing.T) {
	t.Parallel()
	idp := identitytest.NewIdP(t)
	clock := identitytest.NewClock(jwksXStart)
	v := jwksXVerifier(t, idp, clock)

	token := jwksXToken(t, idp, clock)
	jwksXVerifyAccepted(t, v, token, "fresh set")
	jwksXWantFetches(t, idp, 1, "a fresh hit costs nothing")

	// Freshness is checked before the map lookup, so a kid that is still in the
	// cache does not get to skip the expiry — a stale set never serves a hit.
	clock.Advance(identity.KeySetTTLForTest)
	jwksXVerifyAccepted(t, v, token, "known kid past the TTL")
	jwksXWantFetches(t, idp, 2, "the expired set was refetched even though the kid was cached")
}

// TestRateLimitNeverExtendsTTL is the other half of the bounded-lifetime
// invariant: the rate limit may suppress a FETCH; it may never extend a KEY'S
// LIFE. A stale set that could not be refreshed answers nothing.
func TestRateLimitNeverExtendsTTL(t *testing.T) {
	t.Parallel()
	idp := identitytest.NewIdP(t)
	clock := identitytest.NewClock(jwksXStart)
	v := jwksXVerifier(t, idp, clock)

	retiring := idp.ActiveKID()
	token := jwksXToken(t, idp, clock)
	idp.Rotate(t)
	idp.Retire(t, retiring)

	clock.Advance(identity.KeySetTTLForTest)
	idp.FailJWKS(http.StatusInternalServerError)

	// The refresh is attempted and fails, so fetchedAt is left untouched and the
	// set stays stale.
	jwksXVerifyRejected(t, v, token, "retired key past the TTL with the provider down")
	jwksXWantFetches(t, idp, 2, "the expired set was refetched, unsuccessfully")

	// Now inside the cooldown the failed attempt stamped, so no fetch happens at
	// all. The retired key must still be refused: were the cooldown evaluated
	// only on the fresh path, or freshness checked after the map lookup, this is
	// the request that would serve it.
	jwksXVerifyRejected(t, v, token, "retired key with the refresh rate-limited")
	jwksXWantFetches(t, idp, 2, "the cooldown suppressed the fetch")
}

func TestExpiredSetWithDownIdPRejects(t *testing.T) {
	t.Parallel()
	idp := identitytest.NewIdP(t)
	clock := identitytest.NewClock(jwksXStart)
	v := jwksXVerifier(t, idp, clock)

	token := jwksXToken(t, idp, clock)
	clock.Advance(identity.KeySetTTLForTest)
	idp.FailJWKS(http.StatusInternalServerError)

	// A key-set outage is a uniform rejection, never stale acceptance.
	jwksXVerifyRejected(t, v, token, "expired set, provider down")
	jwksXWantFetches(t, idp, 2, "one attempted refresh")

	jwksXVerifyRejected(t, v, token, "expired set, refresh rate-limited")
	jwksXWantFetches(t, idp, 2, "an outage must not be hammered once the TTL has passed")

	// The failure is not sticky: the cache was never poisoned with an empty set.
	idp.Restore()
	clock.Advance(identity.RefreshCooldownForTest)
	jwksXVerifyAccepted(t, v, token, "after the provider recovered")
	jwksXWantFetches(t, idp, 3, "the next allowed attempt succeeded")
}

func TestFreshSetSurvivesFailedUnknownKIDRefresh(t *testing.T) {
	t.Parallel()
	idp := identitytest.NewIdP(t)
	clock := identitytest.NewClock(jwksXStart)
	v := jwksXVerifier(t, idp, clock)

	known := jwksXToken(t, idp, clock)
	unknown := jwksXTokenUnderRetiredKey(t, idp, clock)
	clock.Advance(identity.RefreshCooldownForTest)
	idp.FailJWKS(http.StatusInternalServerError)

	jwksXVerifyRejected(t, v, unknown, "unknown kid while the provider is down")
	jwksXWantFetches(t, idp, 2, "the unknown kid led one failed refresh")

	// A failed fetch leaves fetchedAt alone, so the still-fresh set keeps serving:
	// one token 401s and every other kid keeps working.
	jwksXVerifyAccepted(t, v, known, "known kid after the failed refresh")
	jwksXWantFetches(t, idp, 2, "the fresh cache answered without another attempt")
}

func TestKeySetSingleFlight(t *testing.T) {
	t.Parallel()
	idp := identitytest.NewIdP(t)
	clock := identitytest.NewClock(jwksXStart)
	v := jwksXVerifier(t, idp, clock)

	idp.Rotate(t)
	token := jwksXToken(t, idp, clock) // signed by the rotated-in, uncached kid
	clock.Advance(identity.RefreshCooldownForTest)

	release := make(chan struct{})
	releaseOnce := jwksXRelease(t, release)
	idp.BlockJWKS(release)

	const goroutines = 8
	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = v.Verify(context.Background(), token)
		}(i)
	}

	// One goroutine is inside the blocked handler and the rest are queued behind
	// its flight. Nothing can complete until this channel closes, so releasing
	// here is deterministic rather than a timing guess.
	jwksXAwaitFetch(t, idp, 2)
	releaseOnce()
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: Verify: %v", i, err)
		}
	}
	jwksXWantFetches(t, idp, 2, "eight concurrent refreshes must collapse into one flight")
}

func TestKeySetWaiterCancellationDoesNotKillFlight(t *testing.T) {
	t.Parallel()
	idp := identitytest.NewIdP(t)
	clock := identitytest.NewClock(jwksXStart)
	v := jwksXVerifier(t, idp, clock)

	idp.Rotate(t)
	token := jwksXToken(t, idp, clock)
	clock.Advance(identity.RefreshCooldownForTest)

	release := make(chan struct{})
	releaseOnce := jwksXRelease(t, release)
	idp.BlockJWKS(release)

	// The leader. Its fetch is detached from the caller's context, so it is the
	// goroutine that has to survive a waiter walking away.
	leader := make(chan error, 1)
	go func() {
		_, err := v.Verify(context.Background(), token)
		leader <- err
	}()
	jwksXAwaitFetch(t, idp, 2)

	// A waiter that abandons. The flight cannot have completed — the handler is
	// held open — so this deterministically takes the ctx.Done() branch.
	ctx, cancel := context.WithCancel(context.Background())
	waiter := make(chan error, 1)
	go func() {
		_, err := v.Verify(ctx, token)
		waiter <- err
	}()
	cancel()
	if err := <-waiter; !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("abandoning waiter: Verify err = %v, want ErrUnauthenticated", err)
	}

	releaseOnce()
	if err := <-leader; err != nil {
		t.Errorf("flight leader: Verify: %v", err)
	}
	// The abandoned flight still installed its result.
	jwksXVerifyAccepted(t, v, token, "after the flight completed")
	jwksXWantFetches(t, idp, 2, "a waiter's cancellation must not kill or restart the fetch")
}

func TestKeySetOversizeLeavesCacheIntact(t *testing.T) {
	t.Parallel()
	idp := identitytest.NewIdP(t)
	clock := identitytest.NewClock(jwksXStart)
	v := jwksXVerifier(t, idp, clock)

	token := jwksXToken(t, idp, clock)
	idp.SetJWKSBody(bytes.Repeat([]byte("a"), identity.MaxIdPBytesForTest+1))

	clock.Advance(identity.KeySetTTLForTest)
	jwksXVerifyRejected(t, v, token, "oversize key set past the TTL")
	jwksXWantFetches(t, idp, 2, "the body cap refused the response")

	// A refused response never replaces a working cache with an empty one, so a
	// recovered provider is all it takes to serve again.
	idp.Restore()
	clock.Advance(identity.RefreshCooldownForTest)
	jwksXVerifyAccepted(t, v, token, "after the provider stopped over-sizing its response")
	jwksXWantFetches(t, idp, 3, "the next allowed attempt succeeded")
}

func TestKeySetRefusesRedirect(t *testing.T) {
	t.Parallel()
	idp := identitytest.NewIdP(t)
	clock := identitytest.NewClock(jwksXStart)

	var redirectHits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"keys":[]}`))
	}))
	t.Cleanup(target.Close)

	// Config.HTTPClient replaces the guarded client wholesale, so the fixture's
	// client — which reaches the loopback the production dial guard refuses —
	// carries none of productionClient's other properties. This copy adds back
	// the one under test, its CheckRedirect, so what the assertions below pin is
	// the production rule rather than net/http's default of following a 3xx.
	client := *idp.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	v := jwksXVerifierWithClient(t, idp, clock, &client)

	known := jwksXToken(t, idp, clock)
	unknown := jwksXTokenUnderRetiredKey(t, idp, clock)
	clock.Advance(identity.RefreshCooldownForTest)

	idp.RedirectJWKS(http.StatusFound, target.URL)
	jwksXVerifyRejected(t, v, unknown, "unknown kid with the key set redirected away")
	if got := redirectHits.Load(); got != 0 {
		t.Errorf("the redirect target served %d requests, want 0: a 3xx is a failed fetch, never a followed one", got)
	}
	jwksXWantFetches(t, idp, 2, "the refresh was attempted and refused at the 302")

	// The refused refresh left the fresh cache exactly as it was.
	jwksXVerifyAccepted(t, v, known, "known kid after the refused redirect")
}

func TestKeySetFetchDeadline(t *testing.T) {
	t.Parallel()
	idp := identitytest.NewIdP(t)
	clock := identitytest.NewClock(jwksXStart)
	v := jwksXVerifier(t, idp, clock)

	known := jwksXToken(t, idp, clock)
	unknown := jwksXTokenUnderRetiredKey(t, idp, clock)
	clock.Advance(identity.RefreshCooldownForTest)

	// The one branch a fake clock cannot reach is a real context deadline, so it
	// is driven by shortening this verifier's own: 50ms instead of the five
	// seconds production allows.
	restore := identity.SetFetchTimeoutForTest(v, 50*time.Millisecond)
	defer restore()

	release := make(chan struct{})
	// Released before the fixture closes its server — which waits for outstanding
	// requests — because t.Cleanup runs last-registered-first.
	jwksXRelease(t, release)
	idp.BlockJWKS(release)

	jwksXVerifyRejected(t, v, unknown, "unknown kid with the key set hanging")
	jwksXWantFetches(t, idp, 2, "the refresh was attempted and hit its deadline")

	// The deadline belongs to the shared flight, not to the caller, and a flight
	// that timed out leaves the fresh cache it could not replace serving.
	jwksXVerifyAccepted(t, v, known, "known kid after the fetch deadline")
}

func TestKeySetConcurrentVerifyIsRaceFree(t *testing.T) {
	t.Parallel()
	idp := identitytest.NewIdP(t)
	clock := identitytest.NewClock(jwksXStart)
	v := jwksXVerifier(t, idp, clock)
	token := jwksXToken(t, idp, clock)

	const readers = 8
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// An expiring set may legitimately reject mid-rotation; nothing
				// may escape as an error of another class.
				if _, err := v.Verify(context.Background(), token); err != nil && !errors.Is(err, identity.ErrUnauthenticated) {
					t.Errorf("Verify: %v", err)
					return
				}
			}
		}()
	}

	// Rotation and expiry from this goroutine, verification from eight others:
	// under -race this is what says the one mutex really does guard keys,
	// fetchedAt, lastTry and inflight together.
	for i := 0; i < 4; i++ {
		idp.Rotate(t)
		clock.Advance(identity.KeySetTTLForTest)
	}
	close(stop)
	wg.Wait()
}
