package identitytest

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// fixtureXNow is the instant every test in this file mints at. A fixed value, so
// a claim assertion pins an exact number rather than a window around wall time.
var fixtureXNow = time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

// fixtureXSegments splits a compact JWS and fails unless it has three segments.
func fixtureXSegments(t *testing.T, token string) []string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3: %q", len(parts), token)
	}
	return parts
}

// fixtureXDecodeSegment base64url-decodes one JWS segment as a JSON object.
func fixtureXDecodeSegment(t *testing.T, seg string) map[string]any {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatalf("decode segment %q: %v", seg, err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal segment %q: %v", raw, err)
	}
	return out
}

// fixtureXThroughJSON is a claim set as a decoder sees it, so a comparison
// against a decoded payload is not defeated by int64 arriving back as float64.
func fixtureXThroughJSON(t *testing.T, claims map[string]any) map[string]any {
	t.Helper()
	b, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	return out
}

// fixtureXPublicKey parses the DER the provider exports for a kid.
func fixtureXPublicKey(t *testing.T, p *IdP, kid string) any {
	t.Helper()
	pub, err := x509.ParsePKIXPublicKey(p.PublicKeyDER(t, kid))
	if err != nil {
		t.Fatalf("parse public key for %q: %v", kid, err)
	}
	return pub
}

// fixtureXGetJSON fetches a provider endpoint over the loopback-capable client.
func fixtureXGetJSON(t *testing.T, p *IdP, url string, v any) {
	t.Helper()
	resp, err := p.Client().Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d, want 200", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

// fixtureXPublishedKIDs fetches the key set and returns its kids, sorted. It
// decodes through jose.JSONWebKeySet rather than a bespoke struct, so the set
// the fixture publishes is asserted to be one the verifier's own library loads.
func fixtureXPublishedKIDs(t *testing.T, p *IdP) []string {
	t.Helper()
	var set jose.JSONWebKeySet
	fixtureXGetJSON(t, p, p.JWKSURL(), &set)
	kids := make([]string, 0, len(set.Keys))
	for _, k := range set.Keys {
		if !k.Valid() || !k.IsPublic() {
			t.Errorf("published key %q is invalid or not public", k.KeyID)
		}
		if k.Use != "sig" {
			t.Errorf("published key %q has use %q, want \"sig\"", k.KeyID, k.Use)
		}
		kids = append(kids, k.KeyID)
	}
	slices.Sort(kids)
	return kids
}

func TestMintRoundTrip(t *testing.T) {
	t.Parallel()
	p := NewIdP(t)

	t.Run("active key", func(t *testing.T) {
		claims := p.Claims("console", fixtureXNow)
		claims["custom"] = "kept verbatim"
		token := p.Mint(t, claims)

		parts := fixtureXSegments(t, token)
		hdr := fixtureXDecodeSegment(t, parts[0])
		if hdr["alg"] != "RS256" {
			t.Errorf("header alg = %v, want RS256", hdr["alg"])
		}
		if want := p.ActiveKID(); hdr["kid"] != want {
			t.Errorf("header kid = %v, want %q", hdr["kid"], want)
		}
		if hdr["typ"] != "JWT" {
			t.Errorf("header typ = %v, want JWT", hdr["typ"])
		}

		want := fixtureXThroughJSON(t, claims)
		if got := fixtureXDecodeSegment(t, parts[1]); !reflect.DeepEqual(got, want) {
			t.Errorf("payload = %v, want %v", got, want)
		}

		// The signature verifies under the key the provider publishes, which is
		// what makes this a round trip rather than three decoded segments.
		tok, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.RS256})
		if err != nil {
			t.Fatalf("ParseSigned: %v", err)
		}
		var verified map[string]any
		if err := tok.Claims(fixtureXPublicKey(t, p, p.ActiveKID()), &verified); err != nil {
			t.Fatalf("verify with the published key: %v", err)
		}
		if !reflect.DeepEqual(verified, want) {
			t.Errorf("verified claims = %v, want %v", verified, want)
		}
	})

	t.Run("named key signs with its own algorithm", func(t *testing.T) {
		kid := p.AddECKey(t)
		token := p.MintWith(t, kid, p.Claims("console", fixtureXNow))

		hdr := fixtureXDecodeSegment(t, fixtureXSegments(t, token)[0])
		if hdr["alg"] != "ES256" {
			t.Errorf("header alg = %v, want ES256", hdr["alg"])
		}
		if hdr["kid"] != kid {
			t.Errorf("header kid = %v, want %q", hdr["kid"], kid)
		}
		if active := p.ActiveKID(); active == kid {
			t.Errorf("AddECKey made %q active; it must leave the active key alone", kid)
		}

		tok, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.ES256})
		if err != nil {
			t.Fatalf("ParseSigned: %v", err)
		}
		var verified map[string]any
		if err := tok.Claims(fixtureXPublicKey(t, p, kid), &verified); err != nil {
			t.Fatalf("verify with the published key: %v", err)
		}
	})
}

func TestMintRawBuildsUnsignableTokens(t *testing.T) {
	t.Parallel()
	p := NewIdP(t)
	kid := p.ActiveKID()

	t.Run("alg none", func(t *testing.T) {
		claims := p.Claims("console", fixtureXNow)
		token := p.MintRaw(t, map[string]any{"alg": "none", "typ": "JWT"}, claims, nil)

		parts := fixtureXSegments(t, token)
		if parts[2] != "" {
			t.Errorf("signature segment = %q, want empty", parts[2])
		}
		if hdr := fixtureXDecodeSegment(t, parts[0]); hdr["alg"] != "none" {
			t.Errorf("header alg = %v, want none", hdr["alg"])
		}
		want := fixtureXThroughJSON(t, claims)
		if got := fixtureXDecodeSegment(t, parts[1]); !reflect.DeepEqual(got, want) {
			t.Errorf("payload = %v, want %v", got, want)
		}
	})

	t.Run("HS256 MACed with the published public key", func(t *testing.T) {
		secret := p.PublicKeyDER(t, kid)
		if len(secret) == 0 {
			t.Fatal("PublicKeyDER returned no bytes")
		}

		var inputs []string
		token := p.MintRaw(t,
			map[string]any{"alg": "HS256", "kid": kid, "typ": "JWT"},
			p.Claims("console", fixtureXNow),
			func(signingInput []byte) []byte {
				inputs = append(inputs, string(signingInput))
				mac := hmac.New(sha256.New, secret)
				mac.Write(signingInput)
				return mac.Sum(nil)
			})

		parts := fixtureXSegments(t, token)
		if len(inputs) != 1 {
			t.Fatalf("sign called %d times, want 1", len(inputs))
		}
		// The signing input is header.claims and nothing else — a signer that
		// received anything else could not produce a token a verifier accepts.
		signingInput := parts[0] + "." + parts[1]
		if inputs[0] != signingInput {
			t.Errorf("signing input = %q, want %q", inputs[0], signingInput)
		}
		hdr := fixtureXDecodeSegment(t, parts[0])
		if hdr["alg"] != "HS256" {
			t.Errorf("header alg = %v, want HS256", hdr["alg"])
		}
		if hdr["kid"] != kid {
			t.Errorf("header kid = %v, want %q", hdr["kid"], kid)
		}
		sig, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			t.Fatalf("decode signature %q: %v", parts[2], err)
		}
		mac := hmac.New(sha256.New, secret)
		mac.Write([]byte(signingInput))
		if !hmac.Equal(sig, mac.Sum(nil)) {
			t.Error("signature segment is not the HMAC of the signing input under the public key's DER")
		}
	})
}

// TestEveryKeyIsDistinct guards the fixture's own foundation. The key pool was
// once a ring — pooledRSA took i%len over three keys, and NewIdP consumes index 0
// — so the third rotation published a NEW kid over key material byte-identical to
// the first key's. Every "this key stopped verifying" assertion in the rotation
// and retirement tests would then have been vacuous, and a verifier regression
// that fell back to trying every key in the set would still have passed them.
//
// Nothing else in the suite would notice, which is why this is asserted directly
// rather than inferred.
func TestEveryKeyIsDistinct(t *testing.T) {
	t.Parallel()
	p := NewIdP(t)

	kids := []string{p.ActiveKID()}
	for range 5 {
		kids = append(kids, p.Rotate(t))
	}
	kids = append(kids, p.AddKey(t, "RS512"), p.AddECKey(t), p.AddECKey(t), p.AddECKey(t), p.AddECKey(t))

	if len(slices.Compact(slices.Sorted(slices.Values(kids)))) != len(kids) {
		t.Fatalf("kids are not unique: %v", kids)
	}

	// Key MATERIAL, not just the label: the collision this guards against gave
	// two different kids the same key.
	seen := map[string]string{}
	for _, kid := range kids {
		key, ok := p.key(kid)
		if !ok {
			t.Fatalf("no key %q", kid)
		}
		der, err := x509.MarshalPKIXPublicKey(key.pub)
		if err != nil {
			t.Fatalf("marshal %q: %v", kid, err)
		}
		fingerprint := string(der)
		if prev, dup := seen[fingerprint]; dup {
			t.Fatalf("kids %q and %q publish identical key material", prev, kid)
		}
		seen[fingerprint] = kid
	}
}

func TestRotateRetireAndCounters(t *testing.T) {
	t.Parallel()
	p := NewIdP(t)
	first := p.ActiveKID()

	if got := p.Fetches(); got != 0 {
		t.Errorf("Fetches() = %d before any request, want 0", got)
	}
	if got := p.Discoveries(); got != 0 {
		t.Errorf("Discoveries() = %d before any request, want 0", got)
	}

	var doc map[string]any
	fixtureXGetJSON(t, p, p.DiscoveryURL(), &doc)
	if doc["issuer"] != p.Issuer() {
		t.Errorf("discovery issuer = %v, want %q", doc["issuer"], p.Issuer())
	}
	if doc["jwks_uri"] != p.JWKSURL() {
		t.Errorf("discovery jwks_uri = %v, want %q", doc["jwks_uri"], p.JWKSURL())
	}
	if got := p.Discoveries(); got != 1 {
		t.Errorf("Discoveries() = %d after one well-known request, want 1", got)
	}

	if got, want := fixtureXPublishedKIDs(t, p), []string{first}; !slices.Equal(got, want) {
		t.Errorf("published kids = %v, want %v", got, want)
	}

	second := p.Rotate(t)
	if second == first {
		t.Fatalf("Rotate returned the existing kid %q", second)
	}
	if got := p.ActiveKID(); got != second {
		t.Errorf("ActiveKID() = %q after Rotate, want %q", got, second)
	}
	want := []string{first, second}
	slices.Sort(want)
	if got := fixtureXPublishedKIDs(t, p); !slices.Equal(got, want) {
		t.Errorf("published kids after Rotate = %v, want both %v", got, want)
	}
	hdr := fixtureXDecodeSegment(t, fixtureXSegments(t, p.Mint(t, p.Claims("console", fixtureXNow)))[0])
	if hdr["kid"] != second {
		t.Errorf("Mint used kid %v after Rotate, want the new active key %q", hdr["kid"], second)
	}

	p.Retire(t, first)
	if got, want := fixtureXPublishedKIDs(t, p), []string{second}; !slices.Equal(got, want) {
		t.Errorf("published kids after Retire = %v, want %v", got, want)
	}
	// A retired key still mints: the token outliving its publication is the
	// property a key-set TTL test needs from this fixture.
	retired := fixtureXDecodeSegment(t, fixtureXSegments(t, p.MintWith(t, first, p.Claims("console", fixtureXNow)))[0])
	if retired["kid"] != first {
		t.Errorf("MintWith on the retired key used kid %v, want %q", retired["kid"], first)
	}

	if got := p.Fetches(); got != 3 {
		t.Errorf("Fetches() = %d after three key-set requests, want 3", got)
	}
	if got := p.Discoveries(); got != 1 {
		t.Errorf("Discoveries() = %d, want 1 — minting must not touch the server", got)
	}
}

func TestClaimsAreMinimallyValid(t *testing.T) {
	t.Parallel()
	p := NewIdP(t)
	claims := p.Claims("console", fixtureXNow)

	want := []string{"aud", "exp", "iat", "iss", "nbf", "sub"}
	if got := slices.Sorted(maps.Keys(claims)); !slices.Equal(got, want) {
		t.Fatalf("Claims keys = %v, want exactly %v", got, want)
	}
	if claims["iss"] != p.Issuer() {
		t.Errorf("iss = %v, want %q", claims["iss"], p.Issuer())
	}
	if claims["aud"] != "console" {
		t.Errorf("aud = %v, want the requested audience", claims["aud"])
	}
	if sub, _ := claims["sub"].(string); sub == "" {
		t.Errorf("sub = %v, want a non-empty subject", claims["sub"])
	}
	if claims["iat"] != fixtureXNow.Unix() {
		t.Errorf("iat = %v, want %d", claims["iat"], fixtureXNow.Unix())
	}
	if claims["nbf"] != fixtureXNow.Unix() {
		t.Errorf("nbf = %v, want %d", claims["nbf"], fixtureXNow.Unix())
	}
	if want := fixtureXNow.Add(time.Hour).Unix(); claims["exp"] != want {
		t.Errorf("exp = %v, want %d (one hour out)", claims["exp"], want)
	}

	// Every call hands back a fresh map, so one case's mutation cannot leak into
	// the next — building a case by deleting a field is the documented usage.
	delete(claims, "exp")
	claims["sub"] = "mutated"
	fresh := p.Claims("console", fixtureXNow)
	if fresh["sub"] == "mutated" {
		t.Error("Claims returned a shared map: a mutated sub survived into a later call")
	}
	if _, ok := fresh["exp"]; !ok {
		t.Error("Claims returned a shared map: a deleted exp stayed deleted")
	}
}

func TestClockIsRaceFree(t *testing.T) {
	t.Parallel()
	base := fixtureXNow
	c := NewClock(base)
	if got := c.Now(); !got.Equal(base) {
		t.Fatalf("Now() = %v at rest, want %v", got, base)
	}

	const (
		writers = 4
		readers = 4
		steps   = 500
	)
	total := time.Duration(writers*steps) * time.Millisecond

	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range steps {
				c.Advance(time.Millisecond)
			}
		}()
	}
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			prev := base
			for range steps {
				now := c.Now()
				if now.Before(prev) {
					t.Errorf("Now() went backwards: %v after %v", now, prev)
				}
				if now.After(base.Add(total)) {
					t.Errorf("Now() = %v, past the %v the writers can reach", now, base.Add(total))
				}
				prev = now
			}
		}()
	}
	wg.Wait()

	if got, want := c.Now(), base.Add(total); !got.Equal(want) {
		t.Errorf("Now() = %v after %d advances, want %v — an advance was lost", got, writers*steps, want)
	}
}
