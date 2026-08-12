// Package identitytest is a fake OpenID Provider for the identity verifier's
// tests, and for the API layer's real-token tests in later slices.
//
// It deliberately does not import internal/identity: with no cycle, that
// package's own in-package test files may use it, and so may any consumer. For
// the same reason there is no fake verifier here — a consumer that wants one
// declares the interface it needs in its own test files, where a Go consumer's
// interface belongs.
package identitytest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// keyPool pre-generates the expensive keys once per test binary. RSA generation
// costs tens of milliseconds, and a suite that starts an IdP per subtest would
// otherwise pay it every time.
//
// The pool is a CACHE, never a ring. An earlier version took i%len, which made
// the pool's size a silent correctness bound: NewIdP consumes index 0, so the
// third rotation wrapped and published a brand-new kid over key material
// byte-identical to the first key's. Every rotation and retirement test — the
// ones this fixture exists for — then asserted "this key stopped verifying"
// against a key that was still in the set under another name, and a regression
// that fell back to trying every key would have passed. Growing on demand costs
// one keygen the first time an index is reached and nothing after.
var keyPool = struct {
	mu  sync.Mutex
	rsa []*rsa.PrivateKey
	ec  []*ecdsa.PrivateKey
}{}

func pooledRSA(i int) *rsa.PrivateKey {
	keyPool.mu.Lock()
	defer keyPool.mu.Unlock()
	for len(keyPool.rsa) <= i {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic("identitytest: generate RSA key: " + err.Error())
		}
		keyPool.rsa = append(keyPool.rsa, k)
	}
	return keyPool.rsa[i]
}

func pooledEC(i int) *ecdsa.PrivateKey {
	keyPool.mu.Lock()
	defer keyPool.mu.Unlock()
	for len(keyPool.ec) <= i {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			panic("identitytest: generate EC key: " + err.Error())
		}
		keyPool.ec = append(keyPool.ec, k)
	}
	return keyPool.ec[i]
}

// signingKey is one key the fake provider can sign with and publish.
type signingKey struct {
	kid  string
	alg  string
	priv any
	pub  any
}

// IdP is a fake OpenID Provider on httptest: a discovery document, a JWK Set,
// and token minting, with per-test overrides for the failure shapes a verifier
// must survive.
type IdP struct {
	server *httptest.Server

	fetches     atomic.Int64
	discoveries atomic.Int64

	mu        sync.RWMutex
	keys      []signingKey
	active    string
	published map[string]bool
	// One cursor per pool. A single shared counter would make an EC key's index
	// skip an RSA one, and pooledRSA grows to whatever index it is handed — so a
	// suite that added a few EC keys and then one RSA key would generate every
	// intermediate 2048-bit RSA key it never uses, which is the cost the pool
	// exists to avoid.
	nextRSA int
	nextEC  int

	jwksBody     []byte
	discoveryDoc map[string]any
	jwksStatus   int
	redirectTo   string
	redirectCode int
	block        <-chan struct{}
}

// NewIdP starts a provider holding one RS256 key. The server is closed by
// t.Cleanup.
func NewIdP(t *testing.T) *IdP {
	t.Helper()
	p := &IdP{published: map[string]bool{}}
	priv := pooledRSA(0)
	p.keys = []signingKey{{kid: "rsa-1", alg: "RS256", priv: priv, pub: &priv.PublicKey}}
	p.active = "rsa-1"
	p.published["rsa-1"] = true

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", p.serveDiscovery)
	mux.HandleFunc("/jwks", p.serveJWKS)
	p.server = httptest.NewServer(mux)
	t.Cleanup(p.server.Close)
	return p
}

// Issuer is the provider's base URL, equal to the discovery document's issuer.
func (p *IdP) Issuer() string { return p.server.URL }

// JWKSURL is where the key set is published.
func (p *IdP) JWKSURL() string { return p.server.URL + "/jwks" }

// DiscoveryURL is the well-known metadata URL.
func (p *IdP) DiscoveryURL() string { return p.server.URL + "/.well-known/openid-configuration" }

// ActiveKID is the key Mint signs with.
func (p *IdP) ActiveKID() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.active
}

// Client reaches the provider on loopback. Supply it as the verifier's HTTP
// client: the production client's dial guard refuses loopback by design.
func (p *IdP) Client() *http.Client { return p.server.Client() }

// Fetches counts key-set requests served.
func (p *IdP) Fetches() int { return int(p.fetches.Load()) }

// Discoveries counts well-known requests served.
func (p *IdP) Discoveries() int { return int(p.discoveries.Load()) }

// Claims returns a minimally valid claim set: iss, aud, sub, iat, nbf and an exp
// one hour out, and nothing else. A test mutates or deletes fields to build its
// case; Mint fills in nothing, so no later reader has to reverse-engineer a
// hidden default.
func (p *IdP) Claims(aud string, now time.Time) map[string]any {
	return map[string]any{
		"iss": p.Issuer(),
		"aud": aud,
		"sub": "user-1",
		"iat": now.Unix(),
		"nbf": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}
}

// Mint signs claims with the active key.
func (p *IdP) Mint(t *testing.T, claims map[string]any) string {
	t.Helper()
	return p.MintWith(t, p.ActiveKID(), claims)
}

// MintWith signs claims with a named key, using that key's algorithm.
func (p *IdP) MintWith(t *testing.T, kid string, claims map[string]any) string {
	t.Helper()
	key, ok := p.key(kid)
	if !ok {
		t.Fatalf("identitytest: no key %q", kid)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.SignatureAlgorithm(key.alg), Key: jose.JSONWebKey{Key: key.priv, KeyID: key.kid}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatalf("identitytest: new signer: %v", err)
	}
	token, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatalf("identitytest: sign: %v", err)
	}
	return token
}

// MintRaw assembles b64(header).b64(claims).b64(sign(signingInput)) by hand.
//
// It exists because go-jose will not sign alg:none and will not HMAC with an RSA
// public key — and those two tokens, the none forgery and the classic
// key-confusion token MACed with the published public key, are exactly the ones a
// verifier must refuse. Neither can be built through any signing library.
func (p *IdP) MintRaw(t *testing.T, header, claims map[string]any, sign func(signingInput []byte) []byte) string {
	t.Helper()
	enc := func(v map[string]any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("identitytest: marshal: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	input := enc(header) + "." + enc(claims)
	sig := ""
	if sign != nil {
		sig = base64.RawURLEncoding.EncodeToString(sign([]byte(input)))
	}
	return input + "." + sig
}

// PublicKeyDER returns a key's public half in DER — the "secret" a key-confusion
// token is MACed with.
func (p *IdP) PublicKeyDER(t *testing.T, kid string) []byte {
	t.Helper()
	key, ok := p.key(kid)
	if !ok {
		t.Fatalf("identitytest: no key %q", kid)
	}
	der, err := x509.MarshalPKIXPublicKey(key.pub)
	if err != nil {
		t.Fatalf("identitytest: marshal public key: %v", err)
	}
	return der
}

// AddKey publishes an additional key for one of the five allowed algorithms and
// returns its kid. The active key is unchanged.
func (p *IdP) AddKey(t *testing.T, alg string) string {
	t.Helper()
	var priv, pub any
	switch alg {
	case "RS256", "RS512":
		k := pooledRSA(p.nextRSAIndex())
		priv, pub = k, &k.PublicKey
	case "ES256":
		k := pooledEC(p.nextECIndex())
		priv, pub = k, &k.PublicKey
	case "ES384", "ES512":
		curve := elliptic.P384()
		if alg == "ES512" {
			curve = elliptic.P521()
		}
		k, err := ecdsa.GenerateKey(curve, rand.Reader)
		if err != nil {
			t.Fatalf("identitytest: generate %s key: %v", alg, err)
		}
		priv, pub = k, &k.PublicKey
	default:
		t.Fatalf("identitytest: unsupported algorithm %q", alg)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// A decimal index, not 'a'+n: past 26 keys that ran into punctuation and then
	// non-ASCII. Uniqueness held either way — len(p.keys) only grows, under this
	// lock — but a kid appears in test failures and in the truncated kid the
	// verifier logs, and both should stay readable.
	kid := fmt.Sprintf("%s-%d", alg, len(p.keys))
	p.keys = append(p.keys, signingKey{kid: kid, alg: alg, priv: priv, pub: pub})
	p.published[kid] = true
	return kid
}

// AddECKey publishes an ES256 key alongside the RS256 default.
func (p *IdP) AddECKey(t *testing.T) string {
	t.Helper()
	return p.AddKey(t, "ES256")
}

// Rotate publishes a new RS256 key, makes it active, and keeps the old one
// published — the shape a real rotation takes.
func (p *IdP) Rotate(t *testing.T) string {
	t.Helper()
	kid := p.AddKey(t, "RS256")
	p.mu.Lock()
	p.active = kid
	p.mu.Unlock()
	return kid
}

// Retire removes a key from the published set. Tokens it signed stay valid until
// the verifier's key set expires, which is the property under test.
func (p *IdP) Retire(t *testing.T, kid string) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.published[kid] {
		t.Fatalf("identitytest: key %q is not published", kid)
	}
	delete(p.published, kid)
}

// SetJWKSBody replaces the key-set response with arbitrary bytes: malformed,
// oversize, or holding entries no library can build.
func (p *IdP) SetJWKSBody(body []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.jwksBody = body
}

// SetDiscovery replaces the metadata document: a wrong issuer, a missing or
// non-https jwks_uri.
func (p *IdP) SetDiscovery(doc map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.discoveryDoc = doc
}

// FailJWKS makes the key-set endpoint answer with a status.
func (p *IdP) FailJWKS(status int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.jwksStatus = status
}

// RedirectJWKS makes the key-set endpoint redirect.
func (p *IdP) RedirectJWKS(status int, to string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.redirectCode, p.redirectTo = status, to
}

// BlockJWKS holds the key-set handler open until release is closed, so a test can
// drive the single-flight and deadline paths without sleeping.
func (p *IdP) BlockJWKS(release <-chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.block = release
}

// Restore clears every override.
func (p *IdP) Restore() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.jwksBody, p.discoveryDoc = nil, nil
	p.jwksStatus, p.redirectCode, p.redirectTo = 0, 0, ""
	p.block = nil
}

func (p *IdP) key(kid string) (signingKey, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, k := range p.keys {
		if k.kid == kid {
			return k, true
		}
	}
	return signingKey{}, false
}

// nextRSAIndex and nextECIndex are the per-IdP, per-pool cursors for picking
// pooled keys. Index 0 of each pool belongs to NewIdP's default key.
func (p *IdP) nextRSAIndex() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextRSA++
	return p.nextRSA
}

func (p *IdP) nextECIndex() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextEC++
	return p.nextEC
}

func (p *IdP) serveDiscovery(w http.ResponseWriter, _ *http.Request) {
	p.discoveries.Add(1)
	p.mu.RLock()
	doc := p.discoveryDoc
	p.mu.RUnlock()
	if doc == nil {
		doc = map[string]any{"issuer": p.Issuer(), "jwks_uri": p.JWKSURL()}
	}
	// Marshalled before the status and Content-Type are committed. SetDiscovery
	// takes an arbitrary map, so a test can store something encoding/json refuses;
	// streaming it would send a 200 and then truncate, and the failure would
	// surface as a parse error at the verifier rather than at the fixture that
	// caused it.
	body, err := json.Marshal(doc)
	if err != nil {
		http.Error(w, "identitytest: marshal the discovery document: "+err.Error(),
			http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func (p *IdP) serveJWKS(w http.ResponseWriter, _ *http.Request) {
	p.fetches.Add(1)
	p.mu.RLock()
	block, status, code, to, body := p.block, p.jwksStatus, p.redirectCode, p.redirectTo, p.jwksBody
	keys := make([]signingKey, 0, len(p.keys))
	for _, k := range p.keys {
		if p.published[k.kid] {
			keys = append(keys, k)
		}
	}
	p.mu.RUnlock()

	if block != nil {
		<-block
	}
	if code != 0 {
		w.Header().Set("Location", to)
		w.WriteHeader(code)
		return
	}
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if body != nil {
		_, _ = w.Write(body)
		return
	}
	set := jose.JSONWebKeySet{Keys: make([]jose.JSONWebKey, 0, len(keys))}
	for _, k := range keys {
		set.Keys = append(set.Keys, jose.JSONWebKey{
			Key: k.pub, KeyID: k.kid, Algorithm: k.alg, Use: "sig",
		})
	}
	_ = json.NewEncoder(w).Encode(set)
}

// Clock is a race-safe test clock for a verifier's Now, over atomic nanoseconds,
// so a -race test may advance it while verifications run.
type Clock struct{ ns atomic.Int64 }

// NewClock starts a clock at t.
func NewClock(t time.Time) *Clock {
	c := &Clock{}
	c.ns.Store(t.UnixNano())
	return c
}

// Now reads the clock.
func (c *Clock) Now() time.Time { return time.Unix(0, c.ns.Load()) }

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) { c.ns.Add(int64(d)) }
