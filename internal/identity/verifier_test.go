package identity_test

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity/identitytest"
)

// This file drives Verify end to end against the fake provider. Every helper in
// it carries the verifierX prefix so it cannot collide with another test file in
// this package.
//
// Two rules hold throughout. Time is the frozen identitytest.Clock passed as
// Config.Now, never the wall clock, so no assertion waits for anything. And the
// provider is reached through idp.Client(), because the production client's dial
// guard refuses loopback by design — that guard is proven separately, in
// fetch_internal_test.go.

const (
	// verifierXAudience is the audience every token in this file is minted for
	// unless a test is about the audience itself.
	verifierXAudience = "console"
	// verifierXFailed is the constant Error() the package promises. It is one of
	// the few literals this suite pins: an oracle would show up here first.
	verifierXFailed = "authentication failed"

	// The gcp-iap preset's values, written out rather than read from the package,
	// because the point of the IAP tests is to drive the shipped shape.
	verifierXIAPHeader = "x-goog-iap-jwt-assertion"
	verifierXIAPIssuer = "https://cloud.google.com/iap"
	// The audience is the whole tenant boundary; see
	// TestGCPIAPAudienceIsTheTenantBoundary.
	verifierXIAPAudience = "/projects/1/global/backendServices/2"
	verifierXIAPOther    = "/projects/9/global/backendServices/7"
)

// verifierXBase is the instant every clock in this file starts at. Whole
// seconds, because a NumericDate is rounded to one and an off-by-one there would
// make the leeway boundaries ambiguous.
var verifierXBase = time.Date(2026, 3, 4, 15, 4, 5, 0, time.UTC)

// verifierXRoleMap covers all three roles, so a test can assert the strongest
// one wins rather than only that something mapped.
func verifierXRoleMap() map[string]identity.Role {
	return map[string]identity.Role{
		"platform-admins": identity.RoleAdmin,
		"devs":            identity.RoleDeveloper,
		"everyone":        identity.RoleViewer,
	}
}

// verifierXIdP starts a provider and a frozen clock.
func verifierXIdP(t *testing.T) (*identitytest.IdP, *identitytest.Clock) {
	t.Helper()
	return identitytest.NewIdP(t), identitytest.NewClock(verifierXBase)
}

// verifierXConfig is an oidc-mode Config over the provider, with the key URL
// pinned so New skips discovery. adjust may reshape it. Separate from
// verifierXNew because a test that expects New to REFUSE a config needs the
// config without the t.Fatalf.
func verifierXConfig(idp *identitytest.IdP, clock *identitytest.Clock, adjust func(*identity.Config)) identity.Config {
	cfg := identity.Config{
		Mode:       identity.ModeOIDC,
		Issuer:     idp.Issuer(),
		Audience:   verifierXAudience,
		JWKSURL:    idp.JWKSURL(),
		RoleMap:    verifierXRoleMap(),
		HTTPClient: idp.Client(),
		Now:        clock.Now,
	}
	if adjust != nil {
		adjust(&cfg)
	}
	return cfg
}

// verifierXNew builds a verifier from that config and fails the test if New does.
func verifierXNew(t *testing.T, idp *identitytest.IdP, clock *identitytest.Clock, adjust func(*identity.Config)) *identity.Verifier {
	t.Helper()
	v, err := identity.New(context.Background(), verifierXConfig(idp, clock, adjust))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return v
}

// verifierXFixture is the common case: a provider holding its default RS256 key,
// a frozen clock, and a verifier already warmed against it.
func verifierXFixture(t *testing.T) (*identitytest.IdP, *identitytest.Clock, *identity.Verifier) {
	t.Helper()
	idp, clock := verifierXIdP(t)
	return idp, clock, verifierXNew(t, idp, clock, nil)
}

// verifierXClaims is a minimally valid claim set plus the profile claims a fully
// mapped Identity needs. A test mutates or deletes fields to build its case.
func verifierXClaims(idp *identitytest.IdP, clock *identitytest.Clock) map[string]any {
	c := idp.Claims(verifierXAudience, clock.Now())
	c["email"] = "ada@example.test"
	c["name"] = "Ada Lovelace"
	c["roles"] = []any{"platform-admins"}
	return c
}

// verifierXWant is the Identity verifierXClaims maps to.
func verifierXWant(idp *identitytest.IdP) identity.Identity {
	return identity.Identity{
		Issuer:      idp.Issuer(),
		Subject:     "user-1",
		Email:       "ada@example.test",
		DisplayName: "Ada Lovelace",
		Role:        identity.RoleAdmin,
	}
}

// verifierXAccepted asserts a verification succeeded and produced want.
func verifierXAccepted(t *testing.T, what string, got identity.Identity, err error, want identity.Identity) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: Verify: %v", what, err)
	}
	if got != want {
		t.Errorf("%s: Identity = %+v, want %+v", what, got, want)
	}
}

// verifierXRejected asserts a verification failed the one way this package is
// allowed to fail: the sentinel class, the constant rendered message, and the
// zero Identity. It returns the concrete error so a caller can inspect Reason().
func verifierXRejected(t *testing.T, what string, id identity.Identity, err error) *identity.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: Verify accepted the token and returned %+v, want a rejection", what, id)
	}
	if !errors.Is(err, identity.ErrUnauthenticated) {
		t.Errorf("%s: errors.Is(err, ErrUnauthenticated) = false for %v", what, err)
	}
	if got := err.Error(); got != verifierXFailed {
		t.Errorf("%s: Error() = %q, want the constant %q", what, got, verifierXFailed)
	}
	if id != (identity.Identity{}) {
		t.Errorf("%s: a rejection returned %+v, want the zero Identity", what, id)
	}
	var ie *identity.Error
	if !errors.As(err, &ie) {
		t.Fatalf("%s: errors.As(err, **identity.Error) = false for %v", what, err)
	}
	return ie
}

// verifierXOwnKey is one RSA key generated once per test binary, for the tokens
// identitytest deliberately cannot mint: a genuinely valid off-allowlist
// signature, a payload that is not a JSON object, a crit header, and a JWK whose
// declared alg differs from the header. Those need the private half of a key the
// verifier trusts, which a test gets by publishing this key through
// SetJWKSBody.
func verifierXOwnKey() *rsa.PrivateKey {
	verifierXKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic("identity_test: generate RSA key: " + err.Error())
		}
		verifierXKey = key
	})
	return verifierXKey
}

var (
	verifierXKeyOnce sync.Once
	verifierXKey     *rsa.PrivateKey
)

// verifierXKeyed is a provider whose published key set is one key this file
// holds the private half of.
type verifierXKeyed struct {
	idp   *identitytest.IdP
	clock *identitytest.Clock
	kid   string
	priv  *rsa.PrivateKey
}

// verifierXNewKeyed publishes this file's own key as the provider's whole key
// set, declaring declaredAlg (pass "" to publish no alg at all).
func verifierXNewKeyed(t *testing.T, declaredAlg string) *verifierXKeyed {
	t.Helper()
	idp, clock := verifierXIdP(t)
	priv := verifierXOwnKey()
	const kid = "own-1"
	body, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &priv.PublicKey, KeyID: kid, Algorithm: declaredAlg, Use: "sig",
	}}})
	if err != nil {
		t.Fatalf("marshal key set: %v", err)
	}
	idp.SetJWKSBody(body)
	return &verifierXKeyed{idp: idp, clock: clock, kid: kid, priv: priv}
}

// verifier builds a verifier warmed against the published own-key set.
func (k *verifierXKeyed) verifier(t *testing.T) *identity.Verifier {
	t.Helper()
	return verifierXNew(t, k.idp, k.clock, nil)
}

// claims is the standard claim set for this provider.
func (k *verifierXKeyed) claims() map[string]any {
	return verifierXClaims(k.idp, k.clock)
}

// sign serializes payload as a compact JWS under alg, with any extra protected
// headers. The payload is arbitrary bytes, so a test can sign something that is
// not a JSON object at all.
func (k *verifierXKeyed) sign(t *testing.T, alg jose.SignatureAlgorithm, payload []byte, extra map[jose.HeaderKey]any) string {
	t.Helper()
	opts := (&jose.SignerOptions{}).WithType("JWT")
	for name, value := range extra {
		opts = opts.WithHeader(name, value)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: alg, Key: jose.JSONWebKey{Key: k.priv, KeyID: k.kid}}, opts)
	if err != nil {
		t.Fatalf("new %s signer: %v", alg, err)
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatalf("sign %s: %v", alg, err)
	}
	token, err := jws.CompactSerialize()
	if err != nil {
		t.Fatalf("serialize %s: %v", alg, err)
	}
	return token
}

// signClaims is sign over a marshalled claim set.
func (k *verifierXKeyed) signClaims(t *testing.T, alg jose.SignatureAlgorithm, claims map[string]any, extra map[jose.HeaderKey]any) string {
	t.Helper()
	return k.sign(t, alg, verifierXJSON(t, claims), extra)
}

// verifierXJSON marshals a test value or fails the test.
func verifierXJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// verifierXRS256 is a real RS256 signature over the compact signing input, for
// the MintRaw shapes that need a valid signature under an attacker's key.
func verifierXRS256(t *testing.T, priv *rsa.PrivateKey) func([]byte) []byte {
	t.Helper()
	return func(in []byte) []byte {
		sum := sha256.Sum256(in)
		sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, sum[:])
		if err != nil {
			t.Errorf("sign RS256: %v", err)
			return nil
		}
		return sig
	}
}

// verifierXPastCooldown moves the clock just past the refresh cooldown, so that
// a key lookup for an unknown kid would lead a real fetch. Tests asserting that
// something happens BEFORE key lookup need that, or the fetch they claim did not
// happen was suppressed by the rate limit rather than by the pipeline order. It
// stays well inside the key-set TTL, so the cached keys remain fresh.
func verifierXPastCooldown(t *testing.T, clock *identitytest.Clock) {
	t.Helper()
	clock.Advance(identity.RefreshCooldownForTest + time.Second)
}

// verifierXSegments splits a compact token into its three segments. It refuses
// an empty one, because a caller searching a rejection for a leaked segment
// would otherwise be searching for the empty string, which every string
// contains.
func verifierXSegments(t *testing.T, token string) (header, payload, sig string) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3", len(parts))
	}
	for i, part := range parts {
		if part == "" {
			t.Fatalf("token segment %d is empty", i)
		}
	}
	return parts[0], parts[1], parts[2]
}

// verifierXFlip changes one byte of a base64url segment to another byte of the
// same alphabet, so the token still parses and only the bytes differ.
func verifierXFlip(t *testing.T, segment string) string {
	t.Helper()
	if segment == "" {
		t.Fatal("cannot flip a byte of an empty segment")
	}
	i := len(segment) / 2
	replacement := byte('A')
	if segment[i] == 'A' {
		replacement = 'B'
	}
	return segment[:i] + string(replacement) + segment[i+1:]
}

// TestVerifyRS256 is the whole pipeline in one call: a token the fake provider
// minted with its default key maps to the expected principal, roles included.
func TestVerifyRS256(t *testing.T) {
	t.Parallel()
	idp, clock, v := verifierXFixture(t)

	got, err := v.Verify(context.Background(), idp.Mint(t, verifierXClaims(idp, clock)))
	verifierXAccepted(t, "RS256", got, err, verifierXWant(idp))
}

// TestVerifyES256 is the same round trip through the other key family, because
// key-type handling and algorithm binding are where a JOSE integration usually
// diverges between RSA and EC.
func TestVerifyES256(t *testing.T) {
	t.Parallel()
	idp, clock := verifierXIdP(t)
	kid := idp.AddECKey(t)
	v := verifierXNew(t, idp, clock, nil)

	got, err := v.Verify(context.Background(), idp.MintWith(t, kid, verifierXClaims(idp, clock)))
	verifierXAccepted(t, "ES256", got, err, verifierXWant(idp))
}

// TestVerifyAllAllowedAlgorithms walks the settled five. It is the positive half
// of the allowlist: TestVerifyRejectsOffAllowlistAlgorithm is the negative half,
// and between them the list is pinned in both directions.
func TestVerifyAllAllowedAlgorithms(t *testing.T) {
	t.Parallel()
	idp, clock := verifierXIdP(t)
	kids := map[string]string{"RS256": idp.ActiveKID()}
	for _, alg := range []string{"RS512", "ES256", "ES384", "ES512"} {
		kids[alg] = idp.AddKey(t, alg)
	}
	v := verifierXNew(t, idp, clock, nil)

	for _, alg := range []string{"RS256", "RS512", "ES256", "ES384", "ES512"} {
		t.Run(alg, func(t *testing.T) {
			got, err := v.Verify(context.Background(), idp.MintWith(t, kids[alg], verifierXClaims(idp, clock)))
			verifierXAccepted(t, alg, got, err, verifierXWant(idp))
		})
	}
}

// TestVerifyRejectsNoneAlgorithm is the unsigned forgery. No signing library
// will build it, which is why identitytest.MintRaw exists: the header claims
// alg:none and the third segment is empty, and the allowlist parameter of
// ParseSigned refuses it before a key is ever looked up.
func TestVerifyRejectsNoneAlgorithm(t *testing.T) {
	t.Parallel()
	idp, clock, v := verifierXFixture(t)
	claims := verifierXClaims(idp, clock)

	for _, tc := range []struct {
		name   string
		alg    string
		signer func([]byte) []byte
	}{
		{name: "none with an empty signature", alg: "none"},
		{name: "none with a junk signature", alg: "none", signer: func([]byte) []byte { return []byte("junk") }},
		{name: "capitalised none", alg: "None"},
		{name: "empty alg", alg: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := idp.MintRaw(t,
				map[string]any{"alg": tc.alg, "typ": "JWT", "kid": idp.ActiveKID()}, claims, tc.signer)
			got, err := v.Verify(context.Background(), token)
			verifierXRejected(t, tc.name, got, err)
		})
	}
}

// TestVerifyRejectsHS256 is algorithm confusion in its plain form: a symmetric
// MAC presented where an asymmetric signature belongs.
//
// The second subtest is the sharper claim — that the refusal happens at the
// parser, before any key lookup. It gives the token a kid the provider has never
// published and moves the clock past the refresh cooldown first, so a key lookup
// would lead a real fetch; the fetch counter staying put is what proves the
// pipeline never got that far.
func TestVerifyRejectsHS256(t *testing.T) {
	t.Parallel()
	idp, clock, v := verifierXFixture(t)
	claims := verifierXClaims(idp, clock)
	mac := func(in []byte) []byte {
		m := hmac.New(sha256.New, []byte("shared-secret"))
		m.Write(in)
		return m.Sum(nil)
	}

	t.Run("with a published kid", func(t *testing.T) {
		token := idp.MintRaw(t,
			map[string]any{"alg": "HS256", "typ": "JWT", "kid": idp.ActiveKID()}, claims, mac)
		got, err := v.Verify(context.Background(), token)
		verifierXRejected(t, "HS256", got, err)
	})

	t.Run("never reaches key lookup", func(t *testing.T) {
		verifierXPastCooldown(t, clock)
		before := idp.Fetches()
		token := idp.MintRaw(t,
			map[string]any{"alg": "HS256", "typ": "JWT", "kid": "never-published"}, claims, mac)
		got, err := v.Verify(context.Background(), token)
		verifierXRejected(t, "HS256 with an unknown kid", got, err)
		if after := idp.Fetches(); after != before {
			t.Errorf("key set fetched %d times, want %d: an HS256 token reached key lookup", after, before)
		}
	})
}

// TestVerifyRejectsHS256WithJWKSPublicKey is the key-confusion classic: the
// attacker MACs the token with the very bytes the provider publishes, so a
// verifier that picks its primitive from the header's alg rather than from the
// key's type authenticates anybody who can read a public document.
func TestVerifyRejectsHS256WithJWKSPublicKey(t *testing.T) {
	t.Parallel()
	idp, clock, v := verifierXFixture(t)

	kid := idp.ActiveKID()
	der := idp.PublicKeyDER(t, kid)
	token := idp.MintRaw(t,
		map[string]any{"alg": "HS256", "typ": "JWT", "kid": kid},
		verifierXClaims(idp, clock),
		func(in []byte) []byte {
			m := hmac.New(sha256.New, der)
			m.Write(in)
			return m.Sum(nil)
		})

	got, err := v.Verify(context.Background(), token)
	verifierXRejected(t, "HS256 MACed with the published public key", got, err)
}

// TestVerifyRejectsOffAllowlistAlgorithm covers the two ways an algorithm can be
// off the list.
//
// The PS256 token is genuinely valid — signed with the private half of the key
// the provider publishes, under a kid the verifier holds — and the control shows
// the same key verifying an RS256 token, so the only thing refusing PS256 is the
// allowlist. The second subtest narrows the list to one algorithm and presents a
// token that would pass the default configuration.
func TestVerifyRejectsOffAllowlistAlgorithm(t *testing.T) {
	t.Parallel()

	t.Run("a genuinely valid PS256 signature", func(t *testing.T) {
		t.Parallel()
		// The published key declares no alg, so nothing but the parser's
		// allowlist can object to the header.
		keyed := verifierXNewKeyed(t, "")
		v := keyed.verifier(t)
		claims := keyed.claims()

		got, err := v.Verify(context.Background(), keyed.signClaims(t, jose.PS256, claims, nil))
		verifierXRejected(t, "PS256", got, err)

		got, err = v.Verify(context.Background(), keyed.signClaims(t, jose.RS256, claims, nil))
		verifierXAccepted(t, "RS256 with the same key", got, err, verifierXWant(keyed.idp))
	})

	t.Run("RS256 against an ES256-only configuration", func(t *testing.T) {
		t.Parallel()
		idp, clock := verifierXIdP(t)
		ec := idp.AddECKey(t)
		v := verifierXNew(t, idp, clock, func(cfg *identity.Config) {
			cfg.Algorithms = []string{"ES256"}
		})
		claims := verifierXClaims(idp, clock)

		got, err := v.Verify(context.Background(), idp.Mint(t, claims))
		verifierXRejected(t, "RS256 against an ES256-only allowlist", got, err)

		got, err = v.Verify(context.Background(), idp.MintWith(t, ec, claims))
		verifierXAccepted(t, "ES256 against an ES256-only allowlist", got, err, verifierXWant(idp))
	})
}

// TestVerifyRequiresKID pins the operator-visible constraint the package doc
// states: key selection is this package's and it is indexed by kid, so a token
// without one is refused rather than matched against the only key in the set.
//
// The clock is moved past the cooldown first for the same reason as in
// TestVerifyRejectsHS256: without that, an unchanged fetch counter would prove
// only that the rate limit held.
func TestVerifyRequiresKID(t *testing.T) {
	t.Parallel()
	idp, clock, v := verifierXFixture(t)
	claims := verifierXClaims(idp, clock)
	verifierXPastCooldown(t, clock)

	for _, tc := range []struct {
		name   string
		header map[string]any
	}{
		{name: "no kid at all", header: map[string]any{"alg": "RS256", "typ": "JWT"}},
		{name: "an empty kid", header: map[string]any{"alg": "RS256", "typ": "JWT", "kid": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := idp.Fetches()
			got, err := v.Verify(context.Background(), idp.MintRaw(t, tc.header, claims, nil))
			verifierXRejected(t, tc.name, got, err)
			if after := idp.Fetches(); after != before {
				t.Errorf("key set fetched %d times, want %d: a kid-less token reached key lookup", after, before)
			}
		})
	}
}

// TestVerifyDeclaredKeyAlgMustMatchHeader closes a gap go-jose leaves open: it
// binds the key TYPE to the header's alg, but never the alg the JWK itself
// declares. A key published for RS512 must not verify an RS256 header, and the
// control shows the same key verifying the algorithm it declares.
func TestVerifyDeclaredKeyAlgMustMatchHeader(t *testing.T) {
	t.Parallel()
	keyed := verifierXNewKeyed(t, "RS512")
	v := keyed.verifier(t)
	claims := keyed.claims()

	got, err := v.Verify(context.Background(), keyed.signClaims(t, jose.RS256, claims, nil))
	verifierXRejected(t, "RS256 header against an RS512 key", got, err)

	got, err = v.Verify(context.Background(), keyed.signClaims(t, jose.RS512, claims, nil))
	verifierXAccepted(t, "RS512 header against an RS512 key", got, err, verifierXWant(keyed.idp))
}

// TestVerifyRejectsCritHeader pins that a critical header is refused rather than
// ignored — ANY crit, not only one naming an extension go-jose fails on. The
// token is genuinely signed by a key the verifier holds, so nothing but the crit
// check can be what rejects it, and the control, the same signer without the
// header, verifies.
//
// The bare {"b64"} row is the one that matters and the one a weaker test misses:
// go-jose lists b64 in its own supportedCritical set, so leaning on its check
// alone accepts it. RFC 7797 §7 says a JWT MUST NOT use b64, and an unencoded
// payload is exactly the kind of thing two parsers read differently. Paired with
// {"b64","exp"} below, which go-jose rejects on the "exp" member, the two rows
// separate our rule from the library's.
func TestVerifyRejectsCritHeader(t *testing.T) {
	t.Parallel()
	keyed := verifierXNewKeyed(t, "RS256")
	v := keyed.verifier(t)
	claims := keyed.claims()

	for _, crit := range [][]string{{"exp"}, {"http://example.test/ext"}, {"b64"}, {"b64", "exp"}, {}} {
		token := keyed.signClaims(t, jose.RS256, claims, map[jose.HeaderKey]any{"crit": crit})
		got, err := v.Verify(context.Background(), token)
		ie := verifierXRejected(t, "crit "+strings.Join(crit, ","), got, err)
		if want := "crit or b64 header present"; ie.Reason() != want {
			t.Errorf("crit %v: Reason() = %q, want %q — the rejection must be the crit rule, "+
				"not a later step", crit, ie.Reason(), want)
		}
	}

	// crit whose value is not an array of strings at all. The presence test does
	// not care what the value is, which is what makes the rule "any crit" rather
	// than "any crit go-jose could parse".
	for _, odd := range []any{"b64", 7, map[string]any{"b64": true}, []any{}} {
		token := keyed.signClaims(t, jose.RS256, claims, map[jose.HeaderKey]any{"crit": odd})
		got, err := v.Verify(context.Background(), token)
		verifierXRejected(t, fmt.Sprintf("crit %#v", odd), got, err)
	}

	// The one exception, asserted rather than left to be discovered: "crit":null
	// is ACCEPTED, because go-jose never surfaces it. rawHeader decodes a JSON
	// null to a nil *RawMessage and sanitized() skips those (shared.go:416), so it
	// never reaches ExtraHeaders.
	//
	// That is safe, and the reason is not "go-jose hides it" — it is that a null
	// confers nothing to hide. go-jose's own getCritical returns no names for it
	// (shared.go:340), and getB64 returns the default true for a null b64, so
	// neither member can declare an extension or change how the payload is read.
	// A null crit is semantically the member being absent.
	//
	// This row exists so that if go-jose ever starts surfacing nulls, or starts
	// reading meaning into one, the change fails here and someone re-reads this
	// paragraph instead of finding out in production.
	nullCrit := keyed.signClaims(t, jose.RS256, claims, map[jose.HeaderKey]any{"crit": nil})
	got, err := v.Verify(context.Background(), nullCrit)
	verifierXAccepted(t, `crit:null is indistinguishable from absent`, got, err, verifierXWant(keyed.idp))

	// b64 WITHOUT crit, which the crit rule alone would let through while go-jose
	// still honours it: computeAuthData reads b64 from the protected header with
	// no reference to crit and verifies over the raw payload when it is false.
	// Unreachable for an attacker — the protected header is signed — but a
	// provider that minted one would hand us a token other verifiers read
	// differently.
	for _, b64 := range []any{false, true} {
		token := keyed.signClaims(t, jose.RS256, claims, map[jose.HeaderKey]any{"b64": b64})
		got, err := v.Verify(context.Background(), token)
		ie := verifierXRejected(t, fmt.Sprintf("bare b64:%v", b64), got, err)
		if want := "crit or b64 header present"; ie.Reason() != want {
			t.Errorf("bare b64:%v: Reason() = %q, want %q", b64, ie.Reason(), want)
		}
	}

	got, err = v.Verify(context.Background(), keyed.signClaims(t, jose.RS256, claims, nil))
	verifierXAccepted(t, "the same signer with no crit header", got, err, verifierXWant(keyed.idp))
}

// TestHeaderKeyMaterialIsNeverRead is the SSRF-and-trust test. The token carries
// an embedded jwk holding an attacker key, plus jku and x5u pointing at a second
// server that would happily serve a key set binding the provider's own kid to
// that attacker key. Header-supplied key material is never trusted and never
// fetched: the token is refused, and the second server is never contacted.
//
// The second subtest gives the token an unpublished kid and moves past the
// cooldown, so a refresh really does happen — and it goes to the configured key
// URL, not to the jku the token asked for.
func TestHeaderKeyMaterialIsNeverRead(t *testing.T) {
	t.Parallel()
	idp, clock, v := verifierXFixture(t)
	attacker := verifierXOwnKey()

	// The attacker's key set binds the PROVIDER's kid to the attacker's key, so
	// fetching it would make the forged signature verify.
	attackerSet := verifierXJSON(t, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &attacker.PublicKey, KeyID: idp.ActiveKID(), Algorithm: "RS256", Use: "sig",
	}}})
	attackerJWK := verifierXJSON(t, jose.JSONWebKey{
		Key: &attacker.PublicKey, KeyID: idp.ActiveKID(), Algorithm: "RS256", Use: "sig",
	})

	var hits atomic.Int64
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(attackerSet)
	}))
	t.Cleanup(second.Close)

	header := func(kid string) map[string]any {
		return map[string]any{
			"alg": "RS256",
			"typ": "JWT",
			"kid": kid,
			"jwk": json.RawMessage(attackerJWK),
			"jku": second.URL + "/jwks",
			"x5u": second.URL + "/cert",
		}
	}
	sign := verifierXRS256(t, attacker)
	claims := verifierXClaims(idp, clock)

	t.Run("with the provider's kid", func(t *testing.T) {
		got, err := v.Verify(context.Background(), idp.MintRaw(t, header(idp.ActiveKID()), claims, sign))
		verifierXRejected(t, "embedded jwk", got, err)
	})

	t.Run("with an unpublished kid", func(t *testing.T) {
		verifierXPastCooldown(t, clock)
		before := idp.Fetches()
		got, err := v.Verify(context.Background(), idp.MintRaw(t, header("never-published"), claims, sign))
		verifierXRejected(t, "jku-supplied kid", got, err)
		if after := idp.Fetches(); after <= before {
			t.Errorf("configured key URL fetched %d times, want more than %d: the refresh went somewhere else",
				after, before)
		}
	})

	if n := hits.Load(); n != 0 {
		t.Errorf("the jku/x5u server was contacted %d times, want 0: header-supplied key URLs must never be fetched", n)
	}
}

// TestVerifyRejectsMalformedTokens walks the shapes that are not a compact JWS
// at all, and the ones that are but whose bytes were touched. Segment counts,
// the base64url alphabet and padding are all the parser's job; the flipped and
// swapped signatures are the cryptography's.
func TestVerifyRejectsMalformedTokens(t *testing.T) {
	t.Parallel()
	idp, clock, v := verifierXFixture(t)

	first := idp.Mint(t, verifierXClaims(idp, clock))
	other := verifierXClaims(idp, clock)
	other["sub"] = "user-2"
	second := idp.Mint(t, other)

	h1, p1, s1 := verifierXSegments(t, first)
	_, _, s2 := verifierXSegments(t, second)

	for _, tc := range []struct {
		name  string
		token string
	}{
		{name: "empty", token: ""},
		{name: "one segment", token: h1},
		{name: "two segments", token: h1 + "." + p1},
		{name: "four segments", token: first + ".extra"},
		{name: "five segments", token: first + ".extra.more"},
		{name: "empty header segment", token: "." + p1 + "." + s1},
		{name: "empty payload segment", token: h1 + ".." + s1},
		{name: "empty signature segment", token: h1 + "." + p1 + "."},
		{name: "only dots", token: ".."},
		{name: "base64 padding on the signature", token: h1 + "." + p1 + "." + s1 + "="},
		{name: "a non-alphabet byte in the payload", token: h1 + "." + p1 + "!." + s1},
		{name: "a flipped payload byte", token: h1 + "." + verifierXFlip(t, p1) + "." + s1},
		{name: "a flipped signature byte", token: h1 + "." + p1 + "." + verifierXFlip(t, s1)},
		{name: "another token's signature", token: h1 + "." + p1 + "." + s2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := v.Verify(context.Background(), tc.token)
			verifierXRejected(t, tc.name, got, err)
		})
	}
}

// TestVerifyIssuerExact pins that iss is compared exactly as configured. No
// normalisation, no prefix matching, no case folding — a near miss is a
// different issuer, and a non-string one is not an issuer at all.
func TestVerifyIssuerExact(t *testing.T) {
	t.Parallel()
	idp, clock, v := verifierXFixture(t)
	issuer := idp.Issuer()

	for _, tc := range []struct {
		name string
		iss  any
		want bool
	}{
		{name: "exactly as configured", iss: issuer, want: true},
		{name: "a trailing slash", iss: issuer + "/"},
		{name: "a different case", iss: strings.ToUpper(issuer)},
		{name: "a prefix", iss: issuer[:len(issuer)-1]},
		{name: "a suffix", iss: issuer + "x"},
		{name: "a leading space", iss: " " + issuer},
		{name: "a number", iss: 123},
		{name: "an array", iss: []any{issuer}},
		{name: "absent", iss: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			claims := verifierXClaims(idp, clock)
			if tc.iss == nil {
				delete(claims, "iss")
			} else {
				claims["iss"] = tc.iss
			}
			got, err := v.Verify(context.Background(), idp.Mint(t, claims))
			if tc.want {
				verifierXAccepted(t, tc.name, got, err, verifierXWant(idp))
				return
			}
			verifierXRejected(t, tc.name, got, err)
		})
	}
}

// TestVerifyAudienceShapes walks every JSON shape an aud claim arrives in. The
// configured audience must APPEAR in it, whatever the shape.
//
// One row diverges from this file's design note, deliberately and in the
// fail-closed direction: an array mixing a matching string with a non-string
// element is REFUSED, because go-jose's Audience.UnmarshalJSON (jwt/claims.go)
// returns ErrUnmarshalAudience for any non-string element rather than skipping
// it, so the claim set never decodes. Refusing a token whose aud we cannot fully
// decode is the safe reading, and it is what the code does.
func TestVerifyAudienceShapes(t *testing.T) {
	t.Parallel()
	idp, clock, v := verifierXFixture(t)

	for _, tc := range []struct {
		name string
		aud  any
		want bool
	}{
		{name: "a scalar string", aud: verifierXAudience, want: true},
		{name: "a one-element array", aud: []any{verifierXAudience}, want: true},
		{name: "one of several", aud: []any{"other", verifierXAudience}, want: true},
		{name: "an empty array", aud: []any{}},
		{name: "an array without ours", aud: []any{"other"}},
		{name: "null", aud: nil},
		{name: "a number", aud: 123},
		{name: "an object", aud: map[string]any{"aud": verifierXAudience}},
		{name: "a mixed array", aud: []any{verifierXAudience, 1}},
		{name: "a substring", aud: verifierXAudience[:len(verifierXAudience)-1]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			claims := verifierXClaims(idp, clock)
			claims["aud"] = tc.aud
			// Multi-audience needs azp; that rule has its own tests.
			claims["azp"] = verifierXAudience
			got, err := v.Verify(context.Background(), idp.Mint(t, claims))
			if tc.want {
				verifierXAccepted(t, tc.name, got, err, verifierXWant(idp))
				return
			}
			verifierXRejected(t, tc.name, got, err)
		})
	}

	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		claims := verifierXClaims(idp, clock)
		delete(claims, "aud")
		got, err := v.Verify(context.Background(), idp.Mint(t, claims))
		verifierXRejected(t, "absent aud", got, err)
	})
}

// TestMultiAudienceRequiresAZP pins the settled rule: once a token names more
// than one audience, the authorized party must be this deployment. Otherwise a
// token a user legitimately holds for another relying party, which happens to
// also list us, would authenticate them here.
func TestMultiAudienceRequiresAZP(t *testing.T) {
	t.Parallel()
	idp, clock, v := verifierXFixture(t)

	for _, tc := range []struct {
		name string
		azp  any
		want bool
	}{
		{name: "azp names this deployment", azp: verifierXAudience, want: true},
		{name: "azp names the other party", azp: "other"},
		{name: "azp is absent", azp: nil},
		{name: "azp is empty", azp: ""},
		{name: "azp is not a string", azp: []any{verifierXAudience}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			claims := verifierXClaims(idp, clock)
			claims["aud"] = []any{"other", verifierXAudience}
			if tc.azp != nil {
				claims["azp"] = tc.azp
			}
			got, err := v.Verify(context.Background(), idp.Mint(t, claims))
			if tc.want {
				verifierXAccepted(t, tc.name, got, err, verifierXWant(idp))
				return
			}
			verifierXRejected(t, tc.name, got, err)
		})
	}
}

// TestSingleAudienceIgnoresAZP is the other half of that rule, and the row worth
// stating out loud: a single-audience token naming us is accepted even when its
// azp names somebody else. There is no other party to be confused with, and
// several providers emit an azp that is not the audience. The stricter "check
// azp whenever present" reading was considered and lost.
func TestSingleAudienceIgnoresAZP(t *testing.T) {
	t.Parallel()
	idp, clock, v := verifierXFixture(t)

	for _, tc := range []struct {
		name string
		azp  any
	}{
		{name: "no azp at all"},
		{name: "an azp naming somebody else", azp: "some-other-client"},
		{name: "an empty azp", azp: ""},
		{name: "an azp that is not a string", azp: 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			claims := verifierXClaims(idp, clock)
			if tc.azp != nil {
				claims["azp"] = tc.azp
			}
			got, err := v.Verify(context.Background(), idp.Mint(t, claims))
			verifierXAccepted(t, tc.name, got, err, verifierXWant(idp))
		})
	}
}

// TestVerifyRequiresSubAndExp covers the two checks go-jose does not make. A
// principal with no subject cannot be a principal, and a token with no expiry
// never stops being valid — go-jose's ValidateWithLeeway skips exp when it is
// absent, so without this check a token minted once would authenticate forever.
func TestVerifyRequiresSubAndExp(t *testing.T) {
	t.Parallel()
	idp, clock, v := verifierXFixture(t)

	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		want   bool
	}{
		{name: "a subject and an expiry", mutate: func(map[string]any) {}, want: true},
		{name: "no sub", mutate: func(c map[string]any) { delete(c, "sub") }},
		{name: "an empty sub", mutate: func(c map[string]any) { c["sub"] = "" }},
		{name: "a sub that is not a string", mutate: func(c map[string]any) { c["sub"] = 123 }},
		{name: "no exp", mutate: func(c map[string]any) { delete(c, "exp") }},
		{name: "an exp that is not a number", mutate: func(c map[string]any) { c["exp"] = "soon" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			claims := verifierXClaims(idp, clock)
			tc.mutate(claims)
			got, err := v.Verify(context.Background(), idp.Mint(t, claims))
			if tc.want {
				verifierXAccepted(t, tc.name, got, err, verifierXWant(idp))
				return
			}
			verifierXRejected(t, tc.name, got, err)
		})
	}
}

// TestVerifyLeewayBoundary drives both sides of the clock-skew allowance a
// second at a time. The allowance is a real one — a token a minute past its
// expiry still verifies — so its far edge has to be exact, or the allowance is
// whatever the implementation happens to do.
//
// Every instant is derived from Config.Now and the exported bound, so the test
// never waits and never hard-codes the number.
func TestVerifyLeewayBoundary(t *testing.T) {
	t.Parallel()
	idp, clock, v := verifierXFixture(t)
	now := clock.Now()
	leeway := identity.ClockSkewLeewayForTest

	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		want   bool
	}{
		{
			name:   "expired, one second inside the leeway",
			mutate: func(c map[string]any) { c["exp"] = now.Add(-leeway + time.Second).Unix() },
			want:   true,
		},
		{
			name:   "expired, one second outside the leeway",
			mutate: func(c map[string]any) { c["exp"] = now.Add(-leeway - time.Second).Unix() },
		},
		{
			name:   "not yet valid, one second inside the leeway",
			mutate: func(c map[string]any) { c["nbf"] = now.Add(leeway - time.Second).Unix() },
			want:   true,
		},
		{
			name:   "not yet valid, one second outside the leeway",
			mutate: func(c map[string]any) { c["nbf"] = now.Add(leeway + time.Second).Unix() },
		},
		// The edge itself, where "one second either side" cannot say which way the
		// comparison goes. Inclusive is the answer go-jose gives (its check is
		// expiry.Before(now.Add(-leeway))), and pinning it means a library change
		// that silently made the allowance exclusive would fail here rather than
		// start rejecting real tokens at exactly the skew bound.
		{
			name:   "expired by exactly the leeway",
			mutate: func(c map[string]any) { c["exp"] = now.Add(-leeway).Unix() },
			want:   true,
		},
		{
			name:   "not valid until exactly the leeway from now",
			mutate: func(c map[string]any) { c["nbf"] = now.Add(leeway).Unix() },
			want:   true,
		},
		{
			name:   "issued one second inside the leeway",
			mutate: func(c map[string]any) { c["iat"] = now.Add(leeway - time.Second).Unix() },
			want:   true,
		},
		{
			name:   "issued two minutes in the future",
			mutate: func(c map[string]any) { c["iat"] = now.Add(2 * time.Minute).Unix() },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			claims := verifierXClaims(idp, clock)
			tc.mutate(claims)
			got, err := v.Verify(context.Background(), idp.Mint(t, claims))
			if tc.want {
				verifierXAccepted(t, tc.name, got, err, verifierXWant(idp))
				return
			}
			verifierXRejected(t, tc.name, got, err)
		})
	}
}

// TestVerifyTokenSizeCap pins the bound that comes before any decode. The second
// case is the one that matters: a token that would otherwise verify exactly as
// TestVerifyRS256's does, refused for its size alone.
func TestVerifyTokenSizeCap(t *testing.T) {
	t.Parallel()
	idp, clock, v := verifierXFixture(t)

	got, err := v.Verify(context.Background(), strings.Repeat("a", identity.MaxTokenBytesForTest+1))
	verifierXRejected(t, "a string one byte over the cap", got, err)

	claims := verifierXClaims(idp, clock)
	claims["padding"] = strings.Repeat("p", identity.MaxTokenBytesForTest)
	oversize := idp.Mint(t, claims)
	if len(oversize) <= identity.MaxTokenBytesForTest {
		t.Fatalf("the padded token is %d bytes, want more than the %d-byte cap",
			len(oversize), identity.MaxTokenBytesForTest)
	}
	got, err = v.Verify(context.Background(), oversize)
	verifierXRejected(t, "an otherwise valid token over the cap", got, err)
}

// TestVerifyMalformedPayload covers a signed payload that is not a claim set.
// The signatures are genuine — this file's own key, published as the provider's
// whole key set — so what is under test is the claim decoding rather than the
// cryptography, and the control at the end proves the same signer is trusted.
func TestVerifyMalformedPayload(t *testing.T) {
	t.Parallel()
	keyed := verifierXNewKeyed(t, "RS256")
	v := keyed.verifier(t)

	for _, tc := range []struct {
		name    string
		payload []byte
	}{
		{name: "an array", payload: []byte(`[1,2,3]`)},
		{name: "a string", payload: []byte(`"not a claim set"`)},
		{name: "a number", payload: []byte(`42`)},
		{name: "a boolean", payload: []byte(`true`)},
		{name: "null", payload: []byte(`null`)},
		{name: "not JSON at all", payload: []byte(`{`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := v.Verify(context.Background(), keyed.sign(t, jose.RS256, tc.payload, nil))
			verifierXRejected(t, tc.name, got, err)
		})
	}

	t.Run("exp as a string", func(t *testing.T) {
		claims := keyed.claims()
		claims["exp"] = "tomorrow"
		got, err := v.Verify(context.Background(), keyed.signClaims(t, jose.RS256, claims, nil))
		verifierXRejected(t, "exp as a string", got, err)
	})

	t.Run("a well-formed claim set from the same signer", func(t *testing.T) {
		got, err := v.Verify(context.Background(), keyed.signClaims(t, jose.RS256, keyed.claims(), nil))
		verifierXAccepted(t, "control", got, err, verifierXWant(keyed.idp))
	})
}

// TestVerifyReturnsOnlyUnauthenticatedErrors is the uniform-401 contract as one
// assertion over every rejection this file can produce: the sentinel class on
// all of them, and one byte-identical rendered message. A caller that wanted to
// distinguish expired from wrong-audience from bad-signature would have to reach
// for Reason() deliberately — which is the point, and the last check confirms
// Reason() really does carry the detail Error() withholds.
func TestVerifyReturnsOnlyUnauthenticatedErrors(t *testing.T) {
	t.Parallel()
	idp, clock, v := verifierXFixture(t)
	keyed := verifierXNewKeyed(t, "RS512")
	kv := keyed.verifier(t)
	ctx := context.Background()

	claims := func(mutate func(map[string]any)) map[string]any {
		c := verifierXClaims(idp, clock)
		mutate(c)
		return c
	}
	valid := idp.Mint(t, verifierXClaims(idp, clock))
	h, p, s := verifierXSegments(t, valid)

	cases := []struct {
		name string
		err  error
	}{
		{name: "over the size cap"},
		{name: "not a JWS"},
		{name: "alg none"},
		{name: "HS256"},
		{name: "HS256 with the published public key"},
		{name: "no kid"},
		{name: "unknown kid"},
		{name: "key algorithm mismatch"},
		{name: "crit header"},
		{name: "signature invalid"},
		{name: "issuer mismatch"},
		{name: "audience mismatch"},
		{name: "azp mismatch"},
		{name: "missing sub"},
		{name: "missing exp"},
		{name: "expired"},
		{name: "not yet valid"},
		{name: "issued in the future"},
		{name: "malformed claims"},
	}
	der := idp.PublicKeyDER(t, idp.ActiveKID())
	mac := func(in []byte) []byte {
		m := hmac.New(sha256.New, der)
		m.Write(in)
		return m.Sum(nil)
	}
	now := clock.Now()
	tokens := []string{
		strings.Repeat("a", identity.MaxTokenBytesForTest+1),
		"sk-map-env01-not-a-jwt",
		idp.MintRaw(t, map[string]any{"alg": "none", "kid": idp.ActiveKID()}, verifierXClaims(idp, clock), nil),
		idp.MintRaw(t, map[string]any{"alg": "HS256", "kid": idp.ActiveKID()}, verifierXClaims(idp, clock),
			func(in []byte) []byte { return in }),
		idp.MintRaw(t, map[string]any{"alg": "HS256", "kid": idp.ActiveKID()}, verifierXClaims(idp, clock), mac),
		idp.MintRaw(t, map[string]any{"alg": "RS256"}, verifierXClaims(idp, clock), nil),
		idp.MintRaw(t, map[string]any{"alg": "RS256", "kid": "never-published"}, verifierXClaims(idp, clock), nil),
		"", // filled in below: the key-algorithm mismatch belongs to the other verifier
		"",
		h + "." + p + "." + verifierXFlip(t, s),
		idp.Mint(t, claims(func(c map[string]any) { c["iss"] = idp.Issuer() + "/" })),
		idp.Mint(t, claims(func(c map[string]any) { c["aud"] = "somebody-else" })),
		idp.Mint(t, claims(func(c map[string]any) { c["aud"] = []any{"other", verifierXAudience} })),
		idp.Mint(t, claims(func(c map[string]any) { delete(c, "sub") })),
		idp.Mint(t, claims(func(c map[string]any) { delete(c, "exp") })),
		idp.Mint(t, claims(func(c map[string]any) { c["exp"] = now.Add(-time.Hour).Unix() })),
		idp.Mint(t, claims(func(c map[string]any) { c["nbf"] = now.Add(time.Hour).Unix() })),
		idp.Mint(t, claims(func(c map[string]any) { c["iat"] = now.Add(time.Hour).Unix() })),
		idp.Mint(t, claims(func(c map[string]any) { c["exp"] = "tomorrow" })),
	}

	rendered := map[string]bool{}
	reasons := map[string]bool{}
	for i, tc := range cases {
		var id identity.Identity
		var err error
		switch tc.name {
		case "key algorithm mismatch":
			id, err = kv.Verify(ctx, keyed.signClaims(t, jose.RS256, keyed.claims(), nil))
		case "crit header":
			id, err = kv.Verify(ctx, keyed.signClaims(t, jose.RS512, keyed.claims(),
				map[jose.HeaderKey]any{"crit": []string{"exp"}}))
		default:
			id, err = v.Verify(ctx, tokens[i])
		}
		ie := verifierXRejected(t, tc.name, id, err)
		if ie.Reason() == "" {
			t.Errorf("%s: Reason() is empty; the detail must be carried, just not rendered", tc.name)
		}
		rendered[err.Error()] = true
		reasons[ie.Reason()] = true
	}

	if len(rendered) != 1 {
		t.Errorf("%d distinct rendered messages across %d rejections, want exactly 1 — that is an oracle: %v",
			len(rendered), len(cases), rendered)
	}
	if len(reasons) < 2 {
		t.Errorf("%d distinct reasons across %d rejections; Reason() is meant to carry the detail Error() withholds",
			len(reasons), len(cases))
	}
}

// TestRejectionLeaksNoTokenBytes plants a canary in every attacker-influenced
// value a rejection could be tempted to quote — iss, aud, sub, azp, kid, email,
// name and each roles element — and asserts none of them, nor the token or any
// of its three segments, reaches Error() or Reason().
//
// The rule this enforces is stronger than "never the token": a reason is built
// from fixed strings only, which is what makes the uniform-401 property
// mechanically checkable rather than a habit.
func TestRejectionLeaksNoTokenBytes(t *testing.T) {
	t.Parallel()
	idp, clock, v := verifierXFixture(t)

	const (
		canaryIssuer = "https://canary-issuer.example.test"
		canaryAud    = "canary-audience"
		canarySub    = "canary-subject"
		canaryAZP    = "canary-azp"
		canaryKID    = "canary-kid"
		canaryEmail  = "canary@example.test"
		canaryName   = "Canary Name"
		canaryRole   = "canary-role"
	)
	canaries := []string{
		canaryIssuer, canaryAud, canarySub, canaryAZP, canaryKID, canaryEmail, canaryName, canaryRole,
	}

	poisoned := func() map[string]any {
		c := idp.Claims(verifierXAudience, clock.Now())
		c["sub"] = canarySub
		c["azp"] = canaryAZP
		c["email"] = canaryEmail
		c["name"] = canaryName
		c["roles"] = []any{canaryRole, canaryRole + "-2"}
		return c
	}

	// Signed with a key of this file's own, so the signature segment is a real
	// one rather than empty — a rejection cannot be searched for an empty
	// segment, since every string contains it.
	unknownKID := idp.MintRaw(t,
		map[string]any{"alg": "RS256", "typ": "JWT", "kid": canaryKID},
		poisoned(), verifierXRS256(t, verifierXOwnKey()))

	wrongIssuer := poisoned()
	wrongIssuer["iss"] = canaryIssuer

	wrongAudience := poisoned()
	wrongAudience["aud"] = canaryAud

	multiAudience := poisoned()
	multiAudience["aud"] = []any{canaryAud, verifierXAudience}

	for _, tc := range []struct {
		name  string
		token string
	}{
		{name: "an unknown kid", token: unknownKID},
		{name: "an issuer mismatch", token: idp.Mint(t, wrongIssuer)},
		{name: "an audience mismatch", token: idp.Mint(t, wrongAudience)},
		{name: "an azp mismatch", token: idp.Mint(t, multiAudience)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := v.Verify(context.Background(), tc.token)
			ie := verifierXRejected(t, tc.name, got, err)

			h, p, s := verifierXSegments(t, tc.token)
			for label, text := range map[string]string{"Error()": err.Error(), "Reason()": ie.Reason()} {
				for _, canary := range canaries {
					if strings.Contains(text, canary) {
						t.Errorf("%s: %s = %q leaks the claim value %q", tc.name, label, text, canary)
					}
				}
				for segment, value := range map[string]string{
					"the whole token": tc.token, "the header segment": h,
					"the payload segment": p, "the signature segment": s,
				} {
					if strings.Contains(text, value) {
						t.Errorf("%s: %s = %q leaks %s", tc.name, label, text, segment)
					}
				}
			}
		})
	}
}

// TestModeAndAssertionHeader pins the two accessors the API layer's lane
// dispatch reads: which mode is in force, and which header carries the assertion
// in it. In oidc mode there is no such header, and the empty string is what says
// so.
func TestModeAndAssertionHeader(t *testing.T) {
	t.Parallel()
	idp, clock := verifierXIdP(t)
	idp.AddECKey(t)

	oidc := verifierXNew(t, idp, clock, nil)
	if got := oidc.Mode(); got != identity.ModeOIDC {
		t.Errorf("oidc Mode() = %q, want %q", string(got), string(identity.ModeOIDC))
	}
	if got := oidc.AssertionHeader(); got != "" {
		t.Errorf("oidc AssertionHeader() = %q, want the empty string", got)
	}

	proxy := verifierXNew(t, idp, clock, func(cfg *identity.Config) {
		cfg.Mode = identity.ModeTrustedProxy
		cfg.AssertionHeader = verifierXIAPHeader
	})
	if got := proxy.Mode(); got != identity.ModeTrustedProxy {
		t.Errorf("trusted_proxy Mode() = %q, want %q", string(got), string(identity.ModeTrustedProxy))
	}
	if got := proxy.AssertionHeader(); got != verifierXIAPHeader {
		t.Errorf("trusted_proxy AssertionHeader() = %q, want %q", got, verifierXIAPHeader)
	}
}

// verifierXIAPVerifier builds the gcp-iap shape as a literal Config: the
// preset's header, issuer and single algorithm, a per-deployment audience, and a
// key URL pointing at the fixture.
//
// This is why Config is exported at all. The preset's real key URL is Google's
// global gstatic endpoint, which no test can point at a fixture, so an
// IAP-shaped end-to-end test is only possible by building the Config directly.
func verifierXIAPVerifier(t *testing.T, idp *identitytest.IdP, clock *identitytest.Clock, audience string) *identity.Verifier {
	t.Helper()
	v, err := identity.New(context.Background(), identity.Config{
		Mode:            identity.ModeTrustedProxy,
		AssertionHeader: verifierXIAPHeader,
		Issuer:          verifierXIAPIssuer,
		Audience:        audience,
		JWKSURL:         idp.JWKSURL(),
		Algorithms:      []string{"ES256"},
		RoleMap:         verifierXRoleMap(),
		HTTPClient:      idp.Client(),
		Now:             clock.Now,
	})
	if err != nil {
		t.Fatalf("New(gcp-iap shape): %v", err)
	}
	return v
}

// verifierXIAPClaims is an IAP-shaped assertion: the preset's issuer, the
// backend-service audience, and the profile claims a role maps from.
func verifierXIAPClaims(clock *identitytest.Clock, audience string) map[string]any {
	now := clock.Now()
	return map[string]any{
		"iss":   verifierXIAPIssuer,
		"aud":   audience,
		"sub":   "accounts.google.com:1234567890",
		"email": "ada@example.test",
		"name":  "Ada Lovelace",
		"roles": []any{"platform-admins"},
		"iat":   now.Unix(),
		"nbf":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
}

// TestGCPIAPShapeVerifies drives the shipped trusted-proxy preset end to end: an
// ES256 assertion carrying IAP's issuer and this deployment's backend-service
// audience maps to the expected principal.
func TestGCPIAPShapeVerifies(t *testing.T) {
	t.Parallel()
	idp, clock := verifierXIdP(t)
	kid := idp.AddECKey(t)
	v := verifierXIAPVerifier(t, idp, clock, verifierXIAPAudience)

	got, err := v.Verify(context.Background(),
		idp.MintWith(t, kid, verifierXIAPClaims(clock, verifierXIAPAudience)))
	verifierXAccepted(t, "an IAP assertion", got, err, identity.Identity{
		Issuer:      verifierXIAPIssuer,
		Subject:     "accounts.google.com:1234567890",
		Email:       "ada@example.test",
		DisplayName: "Ada Lovelace",
		Role:        identity.RoleAdmin,
	})

	if got := v.AssertionHeader(); got != verifierXIAPHeader {
		t.Errorf("AssertionHeader() = %q, want %q", got, verifierXIAPHeader)
	}
	if got := v.Mode(); got != identity.ModeTrustedProxy {
		t.Errorf("Mode() = %q, want %q", string(got), string(identity.ModeTrustedProxy))
	}
}

// TestGCPIAPAudienceIsTheTenantBoundary is the reason the preset ships no
// audience of its own.
//
// The preset's key source, https://www.gstatic.com/iap/verify/public_key-jwk, is
// Google's GLOBAL key set: every Google Cloud customer's IAP assertion is signed
// by a key this deployment would trust. The signature therefore separates
// nothing, and the backend-service audience is the entire tenant boundary — an
// empty or wrong IDENTITY_PROXY_AUDIENCE is not a misconfiguration, it is
// cross-customer authentication. Here the same verifier, the same key, and an
// otherwise perfectly valid assertion minted for another project's backend
// service is refused.
func TestGCPIAPAudienceIsTheTenantBoundary(t *testing.T) {
	t.Parallel()
	idp, clock := verifierXIdP(t)
	kid := idp.AddECKey(t)
	v := verifierXIAPVerifier(t, idp, clock, verifierXIAPAudience)

	got, err := v.Verify(context.Background(),
		idp.MintWith(t, kid, verifierXIAPClaims(clock, verifierXIAPOther)))
	verifierXRejected(t, "another project's IAP audience", got, err)

	// The same assertion for this deployment's audience verifies, so the
	// audience is demonstrably the only thing that separated them.
	got, err = v.Verify(context.Background(),
		idp.MintWith(t, kid, verifierXIAPClaims(clock, verifierXIAPAudience)))
	if err != nil {
		t.Fatalf("this deployment's audience: %v", err)
	}
	if got.Subject == "" {
		t.Error("this deployment's audience verified to an empty subject")
	}
}

// TestCrossModeSubstitution presents each mode's token to the other mode's
// verifier. Both verifiers here share one provider and one signing key, so the
// signature is valid in both directions and nothing but the issuer and audience
// policy stands between them — which is exactly the property under test.
func TestCrossModeSubstitution(t *testing.T) {
	t.Parallel()
	idp, clock := verifierXIdP(t)
	oidc := verifierXNew(t, idp, clock, nil)
	proxy := verifierXNew(t, idp, clock, func(cfg *identity.Config) {
		cfg.Mode = identity.ModeTrustedProxy
		cfg.AssertionHeader = verifierXIAPHeader
		cfg.Issuer = verifierXIAPIssuer
		cfg.Audience = verifierXIAPAudience
	})

	oidcToken := idp.Mint(t, verifierXClaims(idp, clock))
	proxyClaims := verifierXIAPClaims(clock, verifierXIAPAudience)
	proxyToken := idp.Mint(t, proxyClaims)

	ctx := context.Background()

	got, err := proxy.Verify(ctx, oidcToken)
	verifierXRejected(t, "an oidc token against the proxy verifier", got, err)

	got, err = oidc.Verify(ctx, proxyToken)
	verifierXRejected(t, "a proxy assertion against the oidc verifier", got, err)

	got, err = oidc.Verify(ctx, oidcToken)
	verifierXAccepted(t, "the oidc token against its own verifier", got, err, verifierXWant(idp))

	got, err = proxy.Verify(ctx, proxyToken)
	if err != nil {
		t.Fatalf("the proxy assertion against its own verifier: %v", err)
	}
	if got.Issuer != verifierXIAPIssuer {
		t.Errorf("proxy Identity.Issuer = %q, want %q", got.Issuer, verifierXIAPIssuer)
	}
}

// TestClaimMappingEndToEnd runs the two real provider shapes through the whole
// pipeline rather than through claimAt alone: Casdoor's flat roles array, and
// Keycloak's nested realm_access.roles reached by a dotted claim name.
func TestClaimMappingEndToEnd(t *testing.T) {
	t.Parallel()
	idp, clock := verifierXIdP(t)
	casdoor := verifierXNew(t, idp, clock, nil)
	keycloak := verifierXNew(t, idp, clock, func(cfg *identity.Config) {
		cfg.RolesClaim = "realm_access.roles"
		cfg.NameClaim = "preferred_username"
	})
	ctx := context.Background()

	flat := idp.Claims(verifierXAudience, clock.Now())
	flat["email"] = "ada@example.test"
	flat["name"] = "Ada Lovelace"
	flat["roles"] = []any{"platform-admins"}
	got, err := casdoor.Verify(ctx, idp.Mint(t, flat))
	verifierXAccepted(t, "a Casdoor-shaped token", got, err, identity.Identity{
		Issuer:      idp.Issuer(),
		Subject:     "user-1",
		Email:       "ada@example.test",
		DisplayName: "Ada Lovelace",
		Role:        identity.RoleAdmin,
	})

	nested := idp.Claims(verifierXAudience, clock.Now())
	nested["email"] = "grace@example.test"
	nested["preferred_username"] = "grace"
	nested["realm_access"] = map[string]any{"roles": []any{"offline_access", "devs"}}
	got, err = keycloak.Verify(ctx, idp.Mint(t, nested))
	verifierXAccepted(t, "a Keycloak-shaped token", got, err, identity.Identity{
		Issuer:      idp.Issuer(),
		Subject:     "user-1",
		Email:       "grace@example.test",
		DisplayName: "grace",
		Role:        identity.RoleDeveloper,
	})

	// The nested claim is reached only as a path: a flat claim literally named
	// "realm_access.roles" must not outrank it, or a user who can set one of
	// their own attributes maps their own role.
	escalation := idp.Claims(verifierXAudience, clock.Now())
	escalation["realm_access.roles"] = []any{"platform-admins"}
	got, err = keycloak.Verify(ctx, idp.Mint(t, escalation))
	if err != nil {
		t.Fatalf("a flat claim named like the path: %v", err)
	}
	if got.Role != identity.RoleNone {
		t.Errorf("a flat claim literally named %q mapped %q; a dotted claim name is a path only",
			"realm_access.roles", string(got.Role))
	}
}

// TestNoMappedRoleYieldsRoleNone pins that authentication and authority are
// separate outcomes. A verified human whose roles map to nothing is
// authenticated with no authority — not an error here; a role-gated route is
// what refuses them.
func TestNoMappedRoleYieldsRoleNone(t *testing.T) {
	t.Parallel()
	idp, clock, v := verifierXFixture(t)

	claims := verifierXClaims(idp, clock)
	claims["roles"] = []any{"nobody-in-particular", "another-unmapped-group"}

	got, err := v.Verify(context.Background(), idp.Mint(t, claims))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	want := verifierXWant(idp)
	want.Role = identity.RoleNone
	if got != want {
		t.Errorf("Identity = %+v, want %+v", got, want)
	}
	if got.Role.AtLeast(identity.RoleViewer) {
		t.Error("an unmapped principal satisfies the viewer minimum; RoleNone must satisfy nothing")
	}
}

// TestVerifyRoleClaimTypes walks the JSON shapes a roles claim arrives in, all
// the way through Verify. The strongest mapped value wins whatever the shape,
// and a claim that is not a string or an array of them maps nothing rather than
// failing the verification.
func TestVerifyRoleClaimTypes(t *testing.T) {
	t.Parallel()
	idp, clock, v := verifierXFixture(t)

	for _, tc := range []struct {
		name  string
		roles any
		want  identity.Role
	}{
		{name: "a scalar string", roles: "platform-admins", want: identity.RoleAdmin},
		{name: "an array", roles: []any{"devs"}, want: identity.RoleDeveloper},
		{name: "the strongest of several", roles: []any{"everyone", "platform-admins", "devs"}, want: identity.RoleAdmin},
		{name: "the strongest, other way round", roles: []any{"platform-admins", "everyone"}, want: identity.RoleAdmin},
		{name: "a mixed-type array", roles: []any{7, "devs", true, nil}, want: identity.RoleDeveloper},
		{name: "a space-delimited scalar", roles: "everyone platform-admins", want: identity.RoleNone},
		{name: "an empty array", roles: []any{}, want: identity.RoleNone},
		{name: "null", roles: nil, want: identity.RoleNone},
		{name: "a number", roles: 7, want: identity.RoleNone},
		{name: "an object", roles: map[string]any{"roles": "platform-admins"}, want: identity.RoleNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			claims := verifierXClaims(idp, clock)
			claims["roles"] = tc.roles
			got, err := v.Verify(context.Background(), idp.Mint(t, claims))
			want := verifierXWant(idp)
			want.Role = tc.want
			verifierXAccepted(t, tc.name, got, err, want)
		})
	}

	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		claims := verifierXClaims(idp, clock)
		delete(claims, "roles")
		got, err := v.Verify(context.Background(), idp.Mint(t, claims))
		want := verifierXWant(idp)
		want.Role = identity.RoleNone
		verifierXAccepted(t, "absent roles", got, err, want)
	})
}

// TestVerifyDistinguishesADecodeFailureFromABadSignature pins the two apart in
// the reason vocabulary. go-jose's Claims does both jobs in one call — verify,
// then unmarshal into every destination — so a genuinely IdP-signed token whose
// payload will not decode arrives as an error from the same line as a forgery.
// Collapsing them sends an operator hunting a key rotation for what is a provider
// emitting the wrong claim type, and the reason is their whole diagnostic
// surface. Both still fail, and both still return the same constant Error() to
// the caller — only Reason() differs.
func TestVerifyDistinguishesADecodeFailureFromABadSignature(t *testing.T) {
	t.Parallel()
	keyed := verifierXNewKeyed(t, "RS256")
	v := keyed.verifier(t)

	t.Run("payload that will not decode", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name    string
			payload []byte
		}{
			{name: "aud is a number", payload: verifierXJSON(t, map[string]any{
				"iss": keyed.idp.Issuer(), "sub": "user-1", "aud": 5,
				"exp": keyed.clock.Now().Add(time.Hour).Unix(),
			})},
			{name: "exp is a string", payload: verifierXJSON(t, map[string]any{
				"iss": keyed.idp.Issuer(), "sub": "user-1", "aud": verifierXAudience,
				"exp": "1800000000",
			})},
			{name: "payload is a JSON array", payload: []byte(`["not","an","object"]`)},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				token := keyed.sign(t, jose.RS256, tc.payload, nil)
				got, err := v.Verify(context.Background(), token)
				ie := verifierXRejected(t, tc.name, got, err)
				if want := "claims did not decode"; ie.Reason() != want {
					t.Errorf("Reason() = %q, want %q — the signature is genuine here", ie.Reason(), want)
				}
			})
		}
	})

	t.Run("tampered signature", func(t *testing.T) {
		t.Parallel()
		// The control. Same key, same claims, one byte of the signature changed —
		// so the ONLY difference from an accepted token is the signature itself.
		token := keyed.signClaims(t, jose.RS256, keyed.claims(), nil)
		parts := strings.Split(token, ".")
		if len(parts) != 3 {
			t.Fatalf("minted token has %d segments, want 3", len(parts))
		}
		flipped := []byte(parts[2])
		if flipped[0] == 'A' {
			flipped[0] = 'B'
		} else {
			flipped[0] = 'A'
		}
		tampered := parts[0] + "." + parts[1] + "." + string(flipped)

		got, err := v.Verify(context.Background(), tampered)
		ie := verifierXRejected(t, "tampered signature", got, err)
		if want := "signature invalid"; ie.Reason() != want {
			t.Errorf("Reason() = %q, want %q", ie.Reason(), want)
		}
	})
}

// TestVerifyBoundsTheProfileFields pins the cap on the two fields whose length
// the token alone decides and a later slice persists. Neither carries authority,
// so truncating keeps the login working rather than turning a valid one into an
// insert failure against a bounded column.
func TestVerifyBoundsTheProfileFields(t *testing.T) {
	t.Parallel()
	idp, clock, v := verifierXFixture(t)

	claims := verifierXClaims(idp, clock)
	claims["email"] = strings.Repeat("a", identity.MaxProfileBytesForTest+500)
	claims["name"] = strings.Repeat("b", identity.MaxProfileBytesForTest+1)

	got, err := v.Verify(context.Background(), idp.Mint(t, claims))
	if err != nil {
		t.Fatalf("Verify: %v — an oversized descriptive claim is not an authentication failure", err)
	}
	if len(got.Email) != identity.MaxProfileBytesForTest {
		t.Errorf("len(Email) = %d, want the cap %d", len(got.Email), identity.MaxProfileBytesForTest)
	}
	if len(got.DisplayName) != identity.MaxProfileBytesForTest {
		t.Errorf("len(DisplayName) = %d, want the cap %d", len(got.DisplayName), identity.MaxProfileBytesForTest)
	}
	if got.Role != identity.RoleAdmin {
		t.Errorf("Role = %q, want %q — bounding a descriptive field must not touch authority",
			got.Role, identity.RoleAdmin)
	}

	// A normal-length address is returned whole, so the cap is a cap and not a
	// truncation everything hits.
	claims["email"], claims["name"] = "ada@example.test", "Ada Lovelace"
	got, err = v.Verify(context.Background(), idp.Mint(t, claims))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Email != "ada@example.test" || got.DisplayName != "Ada Lovelace" {
		t.Errorf("Email/DisplayName = %q/%q, want them untouched", got.Email, got.DisplayName)
	}
}

// TestVerifyBoundsTheProfileFieldsOnARuneBoundary is the other half of that cap,
// and the half that decides whether it works at all. The bound exists so a long
// claim cannot turn a valid login into an insert failure against a bounded
// column; a cut through a multi-byte sequence leaves bytes a UTF8 PostgreSQL
// database refuses, which would reintroduce that failure for every non-ASCII name
// unlucky in its length.
func TestVerifyBoundsTheProfileFieldsOnARuneBoundary(t *testing.T) {
	t.Parallel()
	idp, clock, v := verifierXFixture(t)

	// Three-byte runes over a cap that is not a multiple of three, so the cut
	// lands inside one whatever the cap's exact value.
	const rune3 = "世"
	claims := verifierXClaims(idp, clock)
	claims["name"] = strings.Repeat(rune3, identity.MaxProfileBytesForTest)
	claims["email"] = "a" + strings.Repeat(rune3, identity.MaxProfileBytesForTest)

	got, err := v.Verify(context.Background(), idp.Mint(t, claims))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	for _, f := range []struct{ what, value string }{
		{"DisplayName", got.DisplayName}, {"Email", got.Email},
	} {
		if !utf8.ValidString(f.value) {
			t.Errorf("%s is not valid UTF-8 after truncation: %q", f.what, f.value)
		}
		if len(f.value) > identity.MaxProfileBytesForTest {
			t.Errorf("len(%s) = %d, want at most the cap %d", f.what, len(f.value),
				identity.MaxProfileBytesForTest)
		}
		// At most one rune's worth of bytes may be given up to reach the boundary,
		// so the bound stays a bound rather than a licence to return anything
		// shorter.
		if len(f.value) < identity.MaxProfileBytesForTest-utf8.UTFMax {
			t.Errorf("len(%s) = %d, want within %d bytes of the cap %d", f.what,
				len(f.value), utf8.UTFMax, identity.MaxProfileBytesForTest)
		}
	}
}

// TestVerifyAcceptsGooglesLegacyIssuerForm covers the one OP in the compatibility
// set that mints two spellings of its issuer. Google documents an ID token's iss
// as "always https://accounts.google.com or accounts.google.com", and only the
// first is configurable here (the second is not a URL), so an exact comparison
// would 401 half of a working deployment's logins — after a startup that
// succeeded.
func TestVerifyAcceptsGooglesLegacyIssuerForm(t *testing.T) {
	t.Parallel()
	idp, clock := verifierXIdP(t)
	const google = "https://accounts.google.com"
	v := verifierXNew(t, idp, clock, func(c *identity.Config) { c.Issuer = google })

	for _, iss := range []string{google, "accounts.google.com"} {
		claims := verifierXClaims(idp, clock)
		claims["iss"] = iss
		got, err := v.Verify(context.Background(), idp.Mint(t, claims))
		if err != nil {
			t.Errorf("Verify with iss %q: %v — Google mints both forms", iss, err)
			continue
		}
		// Both spellings collapse to the CONFIGURED one. They are one issuer
		// identity, and a consumer keying on (issuer, subject) — internal/api's
		// principals row — would otherwise store this human twice and hand them a
		// different id depending on which spelling their token happened to carry.
		if got.Issuer != google {
			t.Errorf("Issuer = %q for a token carrying %q, want the configured %q",
				got.Issuer, iss, google)
		}
	}

	// The allowance is that one pair and nothing else: it is keyed on the exact
	// configured string, so no other deployment inherits a second accepted issuer,
	// and the scheme-less Google value is not accepted anywhere else either.
	other := verifierXNew(t, idp, clock, nil)
	for _, iss := range []string{"accounts.google.com", google} {
		claims := verifierXClaims(idp, clock)
		claims["iss"] = iss
		if _, err := other.Verify(context.Background(), idp.Mint(t, claims)); err == nil {
			t.Errorf("iss %q verified against a verifier configured for %q", iss, idp.Issuer())
		}
	}
}

// TestVerifyRefusesAnOversizedSubject pins OIDC Core §2's own bound, and pins
// that it REFUSES rather than truncates. The sub is half the key a principal row
// is stored under, so a shortened one would merge two humans sharing a prefix;
// an unbounded one rides inside the token cap and past PostgreSQL's B-tree tuple
// limit, turning every request for that identity into a 500 at provisioning.
func TestVerifyRefusesAnOversizedSubject(t *testing.T) {
	t.Parallel()
	idp, clock, v := verifierXFixture(t)

	claims := verifierXClaims(idp, clock)
	claims["sub"] = strings.Repeat("s", identity.MaxSubjectBytesForTest+1)
	if _, err := v.Verify(context.Background(), idp.Mint(t, claims)); err == nil {
		t.Error("Verify accepted a subject over the OIDC bound")
	}

	// Exactly at the bound is fine, and arrives whole — the refusal is a ceiling,
	// not a licence to shorten.
	at := strings.Repeat("s", identity.MaxSubjectBytesForTest)
	claims["sub"] = at
	got, err := v.Verify(context.Background(), idp.Mint(t, claims))
	if err != nil {
		t.Fatalf("Verify at the bound: %v", err)
	}
	if got.Subject != at {
		t.Errorf("Subject was altered: len %d, want %d", len(got.Subject), len(at))
	}
}

// TestNewRefusesAMachineCredentialsHeader pins trusted_proxy mode's discipline at
// construction. Naming Authorization as the assertion header would invert the
// mode's own rule — that Authorization is never a human credential — and hand a
// worker's Bearer environment key to the verifier.
func TestNewRefusesAMachineCredentialsHeader(t *testing.T) {
	t.Parallel()
	idp, clock := verifierXIdP(t)

	for _, header := range []string{"Authorization", "authorization", "X-Api-Key", "x-api-key"} {
		cfg := verifierXConfig(idp, clock, func(c *identity.Config) {
			c.Mode = identity.ModeTrustedProxy
			c.AssertionHeader = header
		})
		if _, err := identity.New(context.Background(), cfg); err == nil {
			t.Errorf("New accepted %q as the assertion header", header)
		}
	}

	// A real proxy header still works, so the check refuses only the two names.
	cfg := verifierXConfig(idp, clock, func(c *identity.Config) {
		c.Mode = identity.ModeTrustedProxy
		c.AssertionHeader = "x-goog-iap-jwt-assertion"
	})
	if _, err := identity.New(context.Background(), cfg); err != nil {
		t.Errorf("New refused a legitimate assertion header: %v", err)
	}
}

// TestNewRefusesAnOverDeepClaimName pins that a claim path claimAt will not walk
// is a BOOT error. Accepted instead, it resolves to nil on every request and the
// deployment maps every human to RoleNone — a control plane that denies everyone,
// with nothing in any log to say why.
func TestNewRefusesAnOverDeepClaimName(t *testing.T) {
	t.Parallel()
	idp, clock := verifierXIdP(t)

	deep := strings.Join(make([]string, identity.MaxClaimDepthForTest+2), ".") // depth+1 segments
	for _, tc := range []struct {
		what   string
		adjust func(*identity.Config)
	}{
		{"roles", func(c *identity.Config) { c.RolesClaim = deep }},
		{"email", func(c *identity.Config) { c.EmailClaim = deep }},
		{"name", func(c *identity.Config) { c.NameClaim = deep }},
	} {
		if _, err := identity.New(context.Background(), verifierXConfig(idp, clock, tc.adjust)); err == nil {
			t.Errorf("New accepted an over-deep %s claim name", tc.what)
		}
	}

	// A URI-shaped name is one flat key however many dots it carries, so the
	// namespaced convention Auth0 requires must not trip the depth check.
	namespaced := "https://corp.example/a.b.c.d.e.f.g.h.i.j"
	if _, err := identity.New(context.Background(),
		verifierXConfig(idp, clock, func(c *identity.Config) { c.RolesClaim = namespaced })); err != nil {
		t.Errorf("New refused the URI-shaped claim name %q: %v", namespaced, err)
	}
}

// syncBufferX is a concurrency-safe log sink. slog.SetDefault is process-wide, so
// the sink must tolerate concurrent writers, and the assertions built on it must
// not depend on which test wrote a line. The secret assertions are about the
// ABSENCE of a string unique to this test, which no other writer could produce;
// the presence assertions qualify each message with this fixture's own key-set
// host, so a line another verifier wrote can never satisfy them.
type syncBufferX struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBufferX) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBufferX) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestLogsCarryNoCredentials reads what this package actually writes, at Debug,
// through every logging path it has: startup, a successful refresh, a failed one,
// and the rate-limited branch. The claim that no secret reaches a log is
// otherwise a comment, and a comment is not evidence.
//
// The two secrets under test are the ones that really travel: a key-set URL can
// be a SIGNED url whose query string is the credential, and a token's payload and
// signature are bearer material for as long as the token lives.
//
// One thing is deliberately NOT asserted absent: the kid, which the rate-limited
// branch logs truncated and on purpose, because it is the diagnostic that answers
// "which key did the provider rotate to". It is attacker-supplied, so it is
// bounded; it is not a credential.
//
// Not parallel: it replaces the default logger for its duration.
func TestLogsCarryNoCredentials(t *testing.T) {
	const canary = "SUPER-SECRET-CANARY"

	sink := &syncBufferX{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	idp, clock := verifierXIdP(t)
	// The provider's key-set route ignores the query, so this stands in for a
	// signed key URL exactly as a real one behaves.
	signed := idp.JWKSURL() + "?access_token=" + canary
	v := verifierXNew(t, idp, clock, func(c *identity.Config) { c.JWKSURL = signed })

	// A live token, accepted: its payload and signature are bearer material.
	token := idp.Mint(t, verifierXClaims(idp, clock))
	if _, err := v.Verify(context.Background(), token); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	// Past the TTL with the provider failing: the fetch-failure path logs.
	idp.FailJWKS(http.StatusInternalServerError)
	clock.Advance(identity.KeySetTTLForTest + time.Second)
	if _, err := v.Verify(context.Background(), token); err == nil {
		t.Fatal("Verify succeeded against a failing provider past the TTL")
	}
	// Immediately again: inside the cooldown, so the rate-limited Debug line runs.
	if _, err := v.Verify(context.Background(), token); err == nil {
		t.Fatal("Verify succeeded against a failing provider past the TTL")
	}
	// And a recovery, so the success path logs its URL too.
	idp.Restore()
	clock.Advance(identity.RefreshCooldownForTest + time.Second)
	if _, err := v.Verify(context.Background(), token); err != nil {
		t.Fatalf("Verify after recovery: %v", err)
	}

	logged := sink.String()
	// Every presence check is qualified by THIS fixture's key-set host, which is a
	// port no other test holds. The four messages are constants any verifier in
	// this package emits, and the sink is process-wide, so an unqualified search
	// would be a guard another writer could satisfy — and a guard that proves the
	// canary checks below ran must not be satisfiable by anything but this test.
	// (`identity configured` carries the host in jwks_url, the three key-set lines
	// in url.)
	host, err := url.Parse(signed)
	if err != nil || host.Host == "" {
		t.Fatalf("read the fixture's key-set host from %q: %v", signed, err)
	}
	for _, want := range []string{
		"identity configured",
		"identity: key set fetch failed",
		"identity: key-set refresh rate-limited",
		"identity: key set refreshed",
	} {
		found := false
		for _, line := range strings.Split(logged, "\n") {
			if strings.Contains(line, want) && strings.Contains(line, host.Host) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("the %q path never logged for %s, so this test asserted nothing:\n%s",
				want, host.Host, logged)
		}
	}
	if strings.Contains(logged, canary) {
		t.Errorf("a log line quotes the key URL's query credential:\n%s", logged)
	}
	for i, segment := range strings.Split(token, ".") {
		if strings.Contains(logged, segment) {
			t.Errorf("a log line quotes token segment %d:\n%s", i, logged)
		}
	}
}
