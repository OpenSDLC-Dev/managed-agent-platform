package identity

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// The tests below drive parseKeySet over hand-written JWK Set bytes rather than
// over a server. Every entry a provider is entitled to publish — including the
// ones no Go library can build — is a JSON literal here, which is the only way to
// express an unknown kty, an X25519 encryption key or a 16384-bit modulus at all:
// a fixture that mints its fixtures through a signing library can only produce
// the keys that library already accepts.

// jwksXSetURL stands in for the key-set URL parseKeySet names in its errors and
// its skip log. Nothing dials it.
const jwksXSetURL = "https://idp.example/jwks"

// jwksXKeys is the key material these fixtures are built from, generated once per
// test binary: RSA generation costs tens of milliseconds and every row below
// would otherwise pay it again.
var jwksXKeys struct {
	once sync.Once
	rsa  *rsa.PublicKey
	ec   map[string]*ecdsa.PublicKey
}

// jwksXECAlg is the JWS algorithm each curve is published under.
var jwksXECAlg = map[string]string{"P-256": "ES256", "P-384": "ES384", "P-521": "ES512"}

func jwksXGenerate() {
	rk, err := rsa.GenerateKey(rand.Reader, minRSABits)
	if err != nil {
		panic("identity: generate RSA fixture key: " + err.Error())
	}
	jwksXKeys.rsa = &rk.PublicKey
	jwksXKeys.ec = map[string]*ecdsa.PublicKey{}
	for crv, curve := range map[string]elliptic.Curve{
		"P-256": elliptic.P256(),
		"P-384": elliptic.P384(),
		"P-521": elliptic.P521(),
	} {
		ek, err := ecdsa.GenerateKey(curve, rand.Reader)
		if err != nil {
			panic("identity: generate EC fixture key: " + err.Error())
		}
		jwksXKeys.ec[crv] = &ek.PublicKey
	}
}

// jwksXB64 encodes as unpadded base64url, the only encoding go-jose accepts for a
// JWK's numeric fields.
func jwksXB64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// jwksXPad left-pads to n bytes. An EC coordinate must be exactly the curve's
// full width or go-jose refuses the entry, and a big.Int drops leading zeros.
func jwksXPad(b []byte, n int) []byte {
	if len(b) >= n {
		return b
	}
	out := make([]byte, n)
	copy(out[n-len(b):], b)
	return out
}

// jwksXModulus returns an odd integer of exactly bits bits — a modulus shaped
// only well enough for the size rule to read, which is all parseKeySet inspects.
// Real 1024-bit and 16384-bit keys would cost a keygen each for no extra pinning.
func jwksXModulus(bits int) *big.Int {
	n := new(big.Int).Lsh(big.NewInt(1), uint(bits-1))
	return n.Or(n, big.NewInt(1))
}

// jwksXRSAModulus builds an RSA JWK entry from an explicit modulus and exponent.
// An empty kid omits the member entirely, which is how a real kid-less entry
// arrives.
func jwksXRSAModulus(kid string, n *big.Int, e int) map[string]any {
	entry := map[string]any{
		"kty": "RSA",
		"n":   jwksXB64(n.Bytes()),
		"e":   jwksXB64(big.NewInt(int64(e)).Bytes()),
		"alg": "RS256",
		"use": "sig",
	}
	if kid != "" {
		entry["kid"] = kid
	}
	return entry
}

// jwksXRSA builds a fully usable RS256 entry over the shared key. Callers mutate
// or delete one member to isolate the single rule their row is about.
func jwksXRSA(kid string) map[string]any {
	jwksXKeys.once.Do(jwksXGenerate)
	return jwksXRSAModulus(kid, jwksXKeys.rsa.N, jwksXKeys.rsa.E)
}

// jwksXEC builds a fully usable EC entry on the named curve.
func jwksXEC(kid, crv string) map[string]any {
	jwksXKeys.once.Do(jwksXGenerate)
	pub := jwksXKeys.ec[crv]
	width := (pub.Curve.Params().BitSize + 7) / 8
	return map[string]any{
		"kty": "EC",
		"crv": crv,
		"x":   jwksXB64(jwksXPad(pub.X.Bytes(), width)),
		"y":   jwksXB64(jwksXPad(pub.Y.Bytes(), width)),
		"alg": jwksXECAlg[crv],
		"use": "sig",
		"kid": kid,
	}
}

// jwksXSet marshals entries into a JWK Set body.
func jwksXSet(t *testing.T, entries ...map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"keys": entries})
	if err != nil {
		t.Fatalf("marshal fixture key set: %v", err)
	}
	return body
}

// jwksXAlgs builds an allowlist. No names means the shipped default.
func jwksXAlgs(names ...string) map[string]bool {
	if len(names) == 0 {
		names = defaultAlgorithms
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// jwksXParse parses body and fails the test on an error.
func jwksXParse(t *testing.T, body []byte, algs map[string]bool) map[string]verifyKey {
	t.Helper()
	keys, err := parseKeySet(body, algs, jwksXSetURL)
	if err != nil {
		t.Fatalf("parseKeySet: %v", err)
	}
	return keys
}

// jwksXWantKIDs fails unless exactly these kids survived.
func jwksXWantKIDs(t *testing.T, keys map[string]verifyKey, want ...string) {
	t.Helper()
	got := make([]string, 0, len(keys))
	for kid := range keys {
		got = append(got, kid)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("usable kids = %v, want %v", got, want)
	}
}

// TestParseKeySetSkipsUnusableEntries is the regression test for the decoder's
// whole reason to exist: one entry a provider is entitled to publish must not
// take the working signing keys down with it.
//
// Each unusable entry below is unusable for exactly one reason, so a rule that
// stopped firing would surface as that entry appearing rather than as a vague
// count change.
func TestParseKeySetSkipsUnusableEntries(t *testing.T) {
	t.Parallel()

	// kty nobody can build: go-jose's JSONWebKey.UnmarshalJSON returns
	// ErrUnsupportedKeyType, which is the error that fails a set-level decode.
	unknownKty := map[string]any{"kty": "NTRU", "kid": "unknown-kty", "use": "sig"}

	// OKP with a curve go-jose only handles for Ed25519. An X25519 encryption key
	// beside the signing keys is an ordinary thing for a provider to publish.
	okp := map[string]any{
		"kty": "OKP",
		"crv": "X25519",
		"kid": "okp-x25519",
		"x":   jwksXB64(make([]byte, 32)),
	}

	// Ed25519: the OKP curve go-jose DOES build, so the entry decodes into a real
	// ed25519.PublicKey that Valid and IsPublic both accept. Only the type switch's
	// default arm stops it, and it carries no alg precisely so the declared-alg
	// filter cannot be the rule that drops it — EdDSA is off the allowlist by
	// decision, and this is the assertion that the decision reaches key material
	// and not only algorithm names.
	edPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate Ed25519 fixture key: %v", err)
	}
	ed := map[string]any{
		"kty": "OKP",
		"crv": "Ed25519",
		"kid": "ed25519-1",
		"use": "sig",
		"x":   jwksXB64(edPub),
	}

	// Symmetric material: go-jose decodes it happily into a []byte, and only the
	// Valid/IsPublic rule keeps it from ever verifying anything. Deliberately
	// carries no alg, so no other rule can be the one that drops it.
	oct := map[string]any{"kty": "oct", "kid": "oct-1", "k": jwksXB64(make([]byte, 32)), "use": "sig"}

	// Valid RS256 in every respect but its declared use.
	encOnly := jwksXRSA("enc-only")
	encOnly["use"] = "enc"

	// Valid RS256 with no kid: unaddressable, because selection is kid-indexed.
	kidless := jwksXRSA("")

	// Valid RSA key advertising a MAC algorithm — the shape that must never reach
	// a verifier at all.
	hmacAlg := jwksXRSA("hs256")
	hmacAlg["alg"] = "HS256"

	body := jwksXSet(t, unknownKty, okp, ed, oct, encOnly, kidless, hmacAlg, jwksXRSA("good"))

	keys := jwksXParse(t, body, jwksXAlgs())
	jwksXWantKIDs(t, keys, "good")
	if got := keys["good"].alg; got != "RS256" {
		t.Errorf("surviving key alg = %q, want RS256", got)
	}
	if _, ok := keys["good"].pub.(*rsa.PublicKey); !ok {
		t.Errorf("surviving key is %T, want *rsa.PublicKey", keys["good"].pub)
	}

	// The set-level decode go-jose offers fails on this very body: JSONWebKeySet
	// is a bare struct with no UnmarshalJSON of its own, so the unknown-kty entry
	// fails the whole slice and takes the good RS256 key with it — a boot failure,
	// and after the TTL a permanently stale set. This assertion is what fails if
	// parseKeySet is ever "simplified" back to it.
	var whole jose.JSONWebKeySet
	if err := json.Unmarshal(body, &whole); err == nil {
		t.Fatalf("set-level decode accepted %d keys; the per-entry decode's premise no longer holds", len(whole.Keys))
	}
}

// TestParseKeySetRejectsWeakRSAKeys pins the bounds go-jose does not apply: it
// builds a bare rsa.PublicKey from n and e without bounding either.
func TestParseKeySetRejectsWeakRSAKeys(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		entry  map[string]any
		usable bool
	}{
		{name: "RSA-1024", entry: jwksXRSAModulus("rsa-1024", jwksXModulus(1024), 65537)},
		{name: "one bit below the floor", entry: jwksXRSAModulus("under", jwksXModulus(minRSABits-1), 65537)},
		{name: "exactly the floor", entry: jwksXRSAModulus("floor", jwksXModulus(minRSABits), 65537), usable: true},
		{name: "exactly the ceiling", entry: jwksXRSAModulus("ceiling", jwksXModulus(maxRSABits), 65537), usable: true},
		{name: "one bit above the ceiling", entry: jwksXRSAModulus("over", jwksXModulus(maxRSABits+1), 65537)},
		{name: "RSA-16384", entry: jwksXRSAModulus("rsa-16384", jwksXModulus(16384), 65537)},
		// Go bounds the exponent's magnitude at verify time but never requires it
		// odd, and an even or unit exponent is not an RSA public exponent at all.
		{name: "even exponent", entry: jwksXRSAModulus("even-e", jwksXModulus(minRSABits), 4)},
		{name: "unit exponent", entry: jwksXRSAModulus("unit-e", jwksXModulus(minRSABits), 1)},
		{name: "exponent three", entry: jwksXRSAModulus("three-e", jwksXModulus(minRSABits), 3), usable: true},
		// crypto/rsa's own ceiling, applied at parse time. Above it go-jose's
		// int(big.Int.Int64()) decode would keep some low-order remnant of the
		// published exponent, so the entry must go rather than become another key.
		{name: "exactly the exponent ceiling", usable: true,
			entry: jwksXRSAModulus("max-e", jwksXModulus(minRSABits), maxRSAExponent)},
		{name: "above the exponent ceiling",
			entry: jwksXRSAModulus("huge-e", jwksXModulus(minRSABits), maxRSAExponent+2)},
		// A modulus is a product of odd primes, so an even one is not a modulus.
		{name: "even modulus",
			entry: jwksXRSAModulus("even-n", new(big.Int).Lsh(big.NewInt(1), minRSABits-1), 65537)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kid, _ := tc.entry["kid"].(string)
			keys := jwksXParse(t, jwksXSet(t, tc.entry, jwksXRSA("good")), jwksXAlgs())
			want := []string{"good"}
			if tc.usable {
				want = append(want, kid)
			}
			jwksXWantKIDs(t, keys, want...)
		})
	}
}

// TestParseKeySetAcceptsValidEC pins that EC needs no extra rule here — go-jose
// already checks the curve name, both coordinate widths and the point itself —
// and that its refusals skip the entry rather than the set.
func TestParseKeySetAcceptsValidEC(t *testing.T) {
	t.Parallel()

	t.Run("every allowed curve", func(t *testing.T) {
		t.Parallel()
		body := jwksXSet(t, jwksXEC("ec-256", "P-256"), jwksXEC("ec-384", "P-384"), jwksXEC("ec-521", "P-521"))
		keys := jwksXParse(t, body, jwksXAlgs())
		jwksXWantKIDs(t, keys, "ec-256", "ec-384", "ec-521")
		for kid, key := range keys {
			if _, ok := key.pub.(*ecdsa.PublicKey); !ok {
				t.Errorf("key %q is %T, want *ecdsa.PublicKey", kid, key.pub)
			}
		}
	})

	// x = y = 0 is off every NIST curve — at x = 0 the curve equation demands
	// y² = b, and b is nonzero — and Go's IsOnCurve documents that it rejects the
	// conventional (0, 0) outright. Deterministic, unlike flipping a bit of a
	// generated coordinate.
	t.Run("off-curve point", func(t *testing.T) {
		t.Parallel()
		offCurve := jwksXEC("ec-off-curve", "P-256")
		offCurve["x"], offCurve["y"] = jwksXB64(make([]byte, 32)), jwksXB64(make([]byte, 32))
		keys := jwksXParse(t, jwksXSet(t, offCurve, jwksXRSA("good")), jwksXAlgs())
		jwksXWantKIDs(t, keys, "good")
	})

	// A coordinate must be the curve's full width; a short one is a different
	// point, not the same one written compactly.
	t.Run("short x coordinate", func(t *testing.T) {
		t.Parallel()
		short := jwksXEC("ec-short-x", "P-256")
		short["x"] = jwksXB64(make([]byte, 31))
		keys := jwksXParse(t, jwksXSet(t, short, jwksXRSA("good")), jwksXAlgs())
		jwksXWantKIDs(t, keys, "good")
	})
}

// TestParseKeySetKeyOps pins the rule go-jose cannot supply: its raw JWK struct
// has no key_ops field at all, so a key restricted to encryption would otherwise
// verify signatures.
func TestParseKeySetKeyOps(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		ops    []string
		setOps bool
		usable bool
	}{
		{name: "encrypt only", ops: []string{"encrypt"}, setOps: true},
		{name: "wrapKey only", ops: []string{"wrapKey"}, setOps: true},
		{name: "verify", ops: []string{"verify"}, setOps: true, usable: true},
		{name: "sign and verify", ops: []string{"sign", "verify"}, setOps: true, usable: true},
		// Present but empty is len 0, so the rule does not apply — the same
		// outcome as absent, and it pins that the guard reads the length rather
		// than the member's presence.
		{name: "empty list", ops: []string{}, setOps: true, usable: true},
		{name: "absent", usable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			entry := jwksXRSA("ops")
			if tc.setOps {
				entry["key_ops"] = tc.ops
			}
			keys := jwksXParse(t, jwksXSet(t, entry, jwksXRSA("good")), jwksXAlgs())
			want := []string{"good"}
			if tc.usable {
				want = append(want, "ops")
			}
			jwksXWantKIDs(t, keys, want...)
		})
	}
}

// TestParseKeySetDeclaredAlgFiltered pins that a JWK's own alg is checked against
// the allowlist here, since go-jose never compares the two.
func TestParseKeySetDeclaredAlgFiltered(t *testing.T) {
	t.Parallel()

	ps256 := jwksXRSA("ps256")
	ps256["alg"] = "PS256" // a real algorithm, deliberately off the shipped list
	noAlg := jwksXRSA("no-alg")
	delete(noAlg, "alg")
	body := jwksXSet(t, ps256, noAlg, jwksXRSA("rs256"), jwksXEC("es256", "P-256"))

	t.Run("default allowlist", func(t *testing.T) {
		t.Parallel()
		keys := jwksXParse(t, body, jwksXAlgs())
		jwksXWantKIDs(t, keys, "no-alg", "rs256", "es256")
		if got := keys["rs256"].alg; got != "RS256" {
			t.Errorf("declared alg carried through as %q, want RS256", got)
		}
		if got := keys["no-alg"].alg; got != "" {
			t.Errorf("undeclared alg carried through as %q, want empty", got)
		}
	})

	// Narrowing the allowlist — the gcp-iap preset ships ["ES256"] — drops the
	// entries that declare something else. An entry declaring nothing survives on
	// purpose: the binding it is missing is the JWS header's, which Verify checks
	// against the key at use time.
	t.Run("narrowed allowlist", func(t *testing.T) {
		t.Parallel()
		keys := jwksXParse(t, body, jwksXAlgs("ES256"))
		jwksXWantKIDs(t, keys, "no-alg", "es256")
	})
}

// TestParseKeySetKeyCountCap pins the bound applied before the per-entry loop, so
// a hostile endpoint cannot make the decode itself the expensive part.
func TestParseKeySetKeyCountCap(t *testing.T) {
	t.Parallel()
	entries := func(n int) []map[string]any {
		out := make([]map[string]any, 0, n)
		for i := range n {
			out = append(out, jwksXRSA(fmt.Sprintf("kid-%02d", i)))
		}
		return out
	}

	keys := jwksXParse(t, jwksXSet(t, entries(maxKeys)...), jwksXAlgs())
	if len(keys) != maxKeys {
		t.Fatalf("a set of exactly %d keys yielded %d", maxKeys, len(keys))
	}

	// Every entry is usable, so only the count can be what refuses this set.
	keys, err := parseKeySet(jwksXSet(t, entries(maxKeys+1)...), jwksXAlgs(), jwksXSetURL)
	if err == nil {
		t.Fatalf("a set of %d usable keys parsed, want a refusal", maxKeys+1)
	}
	if keys != nil {
		t.Errorf("refused set returned %d keys, want none", len(keys))
	}
}

// TestParseKeySetDuplicateKID pins first-wins by document order. RFC 7517 only
// says kids should be distinct, so the tie has to break somewhere; deterministic
// beats clever, and an attacker who can append an entry already owns the provider.
func TestParseKeySetDuplicateKID(t *testing.T) {
	t.Parallel()
	first := jwksXRSAModulus("dup", jwksXModulus(minRSABits), 65537)
	second := jwksXRSAModulus("dup", jwksXModulus(4096), 65537)

	keys := jwksXParse(t, jwksXSet(t, first, second, jwksXRSA("other")), jwksXAlgs())
	jwksXWantKIDs(t, keys, "dup", "other")

	pub, ok := keys["dup"].pub.(*rsa.PublicKey)
	if !ok {
		t.Fatalf("dup key is %T, want *rsa.PublicKey", keys["dup"].pub)
	}
	// The two entries differ only in modulus size, so the size says which won.
	if got := pub.N.BitLen(); got != minRSABits {
		t.Errorf("dup resolved to a %d-bit modulus, want the first entry's %d-bit one", got, minRSABits)
	}
}

// TestParseKeySetEmptyIsAnError pins that a set yielding nothing is an error
// rather than an empty map — a failed parse must never replace a working cache
// with one that verifies nothing.
func TestParseKeySetEmptyIsAnError(t *testing.T) {
	t.Parallel()

	kidless := jwksXRSA("")
	oct := map[string]any{"kty": "oct", "kid": "oct-1", "k": jwksXB64(make([]byte, 32))}

	for _, tc := range []struct {
		name string
		body []byte
	}{
		{name: "empty key array", body: []byte(`{"keys":[]}`)},
		{name: "no keys member", body: []byte(`{}`)},
		{name: "null keys", body: []byte(`{"keys":null}`)},
		{name: "keys is not an array", body: []byte(`{"keys":"x"}`)},
		{name: "only unusable entries", body: jwksXSet(t, kidless, oct)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			keys, err := parseKeySet(tc.body, jwksXAlgs(), jwksXSetURL)
			if err == nil {
				t.Fatalf("parseKeySet accepted %q with %d keys, want a refusal", tc.body, len(keys))
			}
			if keys != nil {
				t.Errorf("refused set returned %d keys, want none", len(keys))
			}
		})
	}
}

// TestLeadFlightOutlivesTheLeadersCancellation pins where the detachment lives.
// The refresh is SHARED: whichever request happened to lead it may hang up, and
// the fetch every other caller is parked on must still complete. getJSON honours
// its context (TestGetJSONHonoursCallerCancellation), so the insulation has to be
// applied here, at the point the fetch stops belonging to one caller.
func TestLeadFlightOutlivesTheLeadersCancellation(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	srv, client := fetchXServe(t, func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksXSet(t, jwksXRSA("k1")))
	})
	t.Cleanup(releaseOnce)

	k := newKeySet(srv.URL, client, defaultAlgorithms, time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		k.mu.Lock()
		k.leadFlight(ctx) // releases and re-acquires k.mu
		k.mu.Unlock()
	}()

	<-started // the fetch is in flight and the handler is holding it open
	cancel()
	releaseOnce()
	<-done

	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.keys["k1"]; !ok {
		t.Fatalf("the leader's cancellation killed the shared fetch: keys = %v", k.keys)
	}
	if k.fetchedAt.IsZero() {
		t.Error("fetchedAt was never stamped, so the flight did not succeed")
	}
}

// TestGetReleasesTheMutexWhenTheFetchPanics is a liveness test, not a
// correctness one. net/http recovers a panicking handler per connection, so a
// mutex left locked by a panic unwinding out of get would not crash the process —
// it would leave it running with every later authentication deadlocked on a lock
// nobody can release. The deferred unlock in get is what prevents that, and this
// is the only test that can tell the two apart.
func TestGetReleasesTheMutexWhenTheFetchPanics(t *testing.T) {
	t.Parallel()

	k := newKeySet("https://idp.example/jwks", panicClient(), defaultAlgorithms, time.Now)

	func() {
		defer func() {
			if recover() == nil {
				t.Error("the fixture transport did not panic, so this test asserted nothing")
			}
		}()
		_, _ = k.get(context.Background(), "k1")
	}()

	// TryLock is the whole assertion: it can only succeed if the panic path left
	// the mutex free. A plain Lock would hang the test binary on the bug instead
	// of failing it.
	if !k.mu.TryLock() {
		t.Fatal("get left the key set's mutex locked after the fetch panicked")
	}
	k.mu.Unlock()
}

// panicClient returns a client whose transport panics, standing in for any panic
// raised beneath leadFlight.
func panicClient() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		panic("identity test: transport panic")
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
