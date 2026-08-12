package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	identity "github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity/identitytest"
)

// discoveryXWellKnown is the path an OpenID Provider publishes its metadata at,
// spelled out here rather than shared with discovery.go: it is a wire constant
// every provider in the compatibility set serves, so a change to it has to be a
// deliberate edit on both sides.
const discoveryXWellKnown = "/.well-known/openid-configuration"

const discoveryXAudience = "console"

// discoveryXBase is the instant every clock here starts at. Nothing in this file
// depends on the wall clock: New's failures are structural, and the one token
// each success case verifies is minted against the same clock the verifier reads.
var discoveryXBase = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// discoveryXConfig is a minimal oidc Config pointed at issuer, with JWKSURL left
// empty so discovery runs. A test that pins the key URL sets it afterwards.
//
// client is not optional. The production client's dial guard refuses loopback by
// design, so Config.HTTPClient is the single seam through which any test reaches
// an httptest server at all.
func discoveryXConfig(issuer string, client *http.Client, now func() time.Time) identity.Config {
	return identity.Config{
		Mode:       identity.ModeOIDC,
		Issuer:     issuer,
		Audience:   discoveryXAudience,
		RoleMap:    map[string]identity.Role{"platform-admins": identity.RoleAdmin},
		HTTPClient: client,
		Now:        now,
	}
}

// discoveryXStub starts a provider whose metadata endpoint runs h. It is a
// ServeMux with that one route, so a request for anything else is a 404 rather
// than a document.
func discoveryXStub(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(discoveryXWellKnown, h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// discoveryXPathLog records the paths a stub provider was asked for.
type discoveryXPathLog struct {
	mu    sync.Mutex
	paths []string
}

func (l *discoveryXPathLog) add(p string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.paths = append(l.paths, p)
}

func (l *discoveryXPathLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.paths...)
}

// discoveryXRecordingIssuer starts a provider whose metadata document is the
// server's whole handler — no ServeMux, so nothing cleans a path or redirects,
// and the log holds the URL exactly as this package built it. The document's
// issuer is the request's own host plus suffix, and its jwks_uri points at
// keysURL, so the recorded issuer can be this stub while the keys stay a
// fixture's.
//
// The handler reads the host from the request rather than from the server value,
// which does not exist until httptest.NewServer returns: everything it closes
// over is written before the server's goroutines exist.
func discoveryXRecordingIssuer(t *testing.T, suffix, keysURL string) (*httptest.Server, *discoveryXPathLog) {
	t.Helper()
	log := &discoveryXPathLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.add(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":   "http://" + r.Host + suffix,
			"jwks_uri": keysURL,
		})
	}))
	t.Cleanup(srv.Close)
	return srv, log
}

// discoveryXWantBootError fails unless New refused to build.
//
// A boot error is a plain descriptive error an operator reads on a console —
// deliberately not the uniform rejection Verify produces, which is why the
// sentinel is asserted absent rather than present. And a refused New returns no
// verifier: a caller that ignored the error must not be handed something that
// would authenticate anyone.
func discoveryXWantBootError(t *testing.T, v *identity.Verifier, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("New succeeded; want a boot error")
	}
	if v != nil {
		t.Error("New returned a verifier alongside its error")
	}
	if errors.Is(err, identity.ErrUnauthenticated) {
		t.Errorf("boot error %v classes as ErrUnauthenticated; a startup failure carries no rejection sentinel", err)
	}
}

func TestNewDiscovers(t *testing.T) {
	t.Parallel()
	p := identitytest.NewIdP(t)
	clock := identitytest.NewClock(discoveryXBase)
	ctx := context.Background()

	v, err := identity.New(ctx, discoveryXConfig(p.Issuer(), p.Client(), clock.Now))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Both network calls happen in New, before any human's first request: an
	// unreachable issuer or an unusable key source is a boot failure, not a 401
	// storm later.
	if got := p.Discoveries(); got != 1 {
		t.Errorf("Discoveries() = %d, want 1", got)
	}
	if got := p.Fetches(); got != 1 {
		t.Errorf("Fetches() = %d, want 1 — New warms the key set", got)
	}

	id, err := v.Verify(ctx, p.Mint(t, p.Claims(discoveryXAudience, clock.Now())))
	if err != nil {
		t.Fatalf("Verify a token from the discovered key set: %v", err)
	}
	if id.Issuer != p.Issuer() {
		t.Errorf("Identity.Issuer = %q, want %q", id.Issuer, p.Issuer())
	}
	if id.Subject != "user-1" {
		t.Errorf("Identity.Subject = %q, want %q", id.Subject, "user-1")
	}
	if got := p.Fetches(); got != 1 {
		t.Errorf("Fetches() = %d after a verify, want 1 — the warmed set serves it", got)
	}
}

func TestNewFailsOnUnreachableIssuer(t *testing.T) {
	t.Parallel()
	srv := discoveryXStub(t, func(http.ResponseWriter, *http.Request) {
		t.Error("a closed provider served a discovery request")
	})
	issuer, client := srv.URL, srv.Client()
	srv.Close() // the shape of an IdP that is down when the process boots

	v, err := identity.New(context.Background(),
		discoveryXConfig(issuer, client, identitytest.NewClock(discoveryXBase).Now))
	discoveryXWantBootError(t, v, err)
}

func TestNewFailsOnIssuerMismatch(t *testing.T) {
	t.Parallel()
	const other = "https://attacker.example"
	p := identitytest.NewIdP(t)
	// OIDC Discovery §4.3's mix-up defence: a document that names a different
	// issuer is not this issuer's document, whoever served it.
	p.SetDiscovery(map[string]any{"issuer": other, "jwks_uri": p.JWKSURL()})

	v, err := identity.New(context.Background(),
		discoveryXConfig(p.Issuer(), p.Client(), identitytest.NewClock(discoveryXBase).Now))
	discoveryXWantBootError(t, v, err)
	// A boot error names the defect — that is what an operator has to act on.
	if err != nil && !strings.Contains(err.Error(), other) {
		t.Errorf("error %q does not name the document's issuer %q", err, other)
	}
	if got := p.Fetches(); got != 0 {
		t.Errorf("Fetches() = %d, want 0 — the mismatch aborts before any key is fetched", got)
	}
}

func TestNewFailsOnMissingJWKSURI(t *testing.T) {
	t.Parallel()
	p := identitytest.NewIdP(t)
	p.SetDiscovery(map[string]any{"issuer": p.Issuer()})

	v, err := identity.New(context.Background(),
		discoveryXConfig(p.Issuer(), p.Client(), identitytest.NewClock(discoveryXBase).Now))
	discoveryXWantBootError(t, v, err)
	if got := p.Fetches(); got != 0 {
		t.Errorf("Fetches() = %d, want 0 — there is no key URL to fetch", got)
	}
}

func TestNewFailsOnNonHTTPSJWKSURI(t *testing.T) {
	t.Parallel()
	// The jwks_uri is the one URL this package fetches that a remote party
	// supplies, so the scheme rule applies to it exactly as it applies to
	// operator configuration.
	for _, tc := range []struct {
		name string
		uri  string
	}{
		{name: "http to a named host", uri: "http://evil.example/keys"},
		{name: "another scheme", uri: "ftp://evil.example/keys"},
		{name: "credentials in the URL", uri: "https://user:pass@evil.example/keys"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := identitytest.NewIdP(t)
			p.SetDiscovery(map[string]any{"issuer": p.Issuer(), "jwks_uri": tc.uri})

			v, err := identity.New(context.Background(),
				discoveryXConfig(p.Issuer(), p.Client(), identitytest.NewClock(discoveryXBase).Now))
			discoveryXWantBootError(t, v, err)
			if got := p.Fetches(); got != 0 {
				t.Errorf("Fetches() = %d, want 0 — the URL is refused before it is dialed", got)
			}
		})
	}
}

func TestNewFailsOnDiscoveryTransport(t *testing.T) {
	t.Parallel()
	// A document oversize enough to trip the body cap, and valid JSON naming the
	// right issuer, so the row fails for its size and for nothing else.
	oversize := func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":   base,
			"jwks_uri": base + "/jwks",
			"pad":      strings.Repeat("a", identity.MaxIdPBytesForTest),
		})
	}

	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{name: "not found", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}},
		{name: "server error", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}},
		{name: "malformed body", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("<html>not json</html>"))
		}},
		{name: "oversize body", handler: oversize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := discoveryXStub(t, tc.handler)
			v, err := identity.New(context.Background(),
				discoveryXConfig(srv.URL, srv.Client(), identitytest.NewClock(discoveryXBase).Now))
			discoveryXWantBootError(t, v, err)
		})
	}
}

func TestNewSkipsDiscoveryWithPinnedJWKSURL(t *testing.T) {
	t.Parallel()
	p := identitytest.NewIdP(t)
	clock := identitytest.NewClock(discoveryXBase)
	ctx := context.Background()
	// The document is poisoned twice over: its issuer is someone else's, and its
	// jwks_uri fails the scheme rule. Either defect alone is a boot error, so a
	// New that consulted this document at all could not succeed — and the
	// Discoveries() count below says it was never asked for, rather than asked for
	// and forgiven.
	p.SetDiscovery(map[string]any{
		"issuer":   "https://attacker.example",
		"jwks_uri": "http://attacker.example/keys",
	})

	cfg := discoveryXConfig(p.Issuer(), p.Client(), clock.Now)
	cfg.JWKSURL = p.JWKSURL()
	v, err := identity.New(ctx, cfg)
	if err != nil {
		t.Fatalf("New with a pinned JWKS URL: %v", err)
	}
	if got := p.Discoveries(); got != 0 {
		t.Errorf("Discoveries() = %d, want 0 — a pinned key URL skips discovery outright", got)
	}
	if got := p.Fetches(); got != 1 {
		t.Errorf("Fetches() = %d, want 1 — the key set is still warmed", got)
	}
	if _, err := v.Verify(ctx, p.Mint(t, p.Claims(discoveryXAudience, clock.Now()))); err != nil {
		t.Errorf("Verify against the pinned key set: %v", err)
	}
	if got := p.Discoveries(); got != 0 {
		t.Errorf("Discoveries() = %d after a verify, want 0", got)
	}
}

func TestIssuerTrailingSlashConcatenation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		suffix string // appended to the provider's base URL to form the issuer
		other  string // the other spelling of the same issuer
	}{
		{name: "bare", suffix: "", other: "/"},
		{name: "trailing slash", suffix: "/", other: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := identitytest.NewIdP(t)
			clock := identitytest.NewClock(discoveryXBase)
			ctx := context.Background()
			// The metadata comes from a recording stub and the keys from the
			// fixture: a jwks_uri on another origin is ordinary, and it is what
			// lets the exact requested path be asserted.
			srv, log := discoveryXRecordingIssuer(t, tc.suffix, p.JWKSURL())
			issuer := srv.URL + tc.suffix

			v, err := identity.New(ctx, discoveryXConfig(issuer, p.Client(), clock.Now))
			if err != nil {
				t.Fatalf("New with issuer %q: %v", issuer, err)
			}
			// Both spellings resolve to the one well-known path, exactly once. A
			// naive concatenation would ask for //.well-known/… , which no
			// provider routes to its document.
			if got := log.snapshot(); len(got) != 1 || got[0] != discoveryXWellKnown {
				t.Errorf("issuer %q requested %q, want exactly one %q", issuer, got, discoveryXWellKnown)
			}

			claims := p.Claims(discoveryXAudience, clock.Now())
			claims["iss"] = issuer
			if _, err := v.Verify(ctx, p.Mint(t, claims)); err != nil {
				t.Errorf("Verify a token whose iss is the configured issuer %q: %v", issuer, err)
			}

			// iss is compared exactly as configured and never normalised, so the
			// other spelling of the same issuer is a different issuer.
			claims = p.Claims(discoveryXAudience, clock.Now())
			claims["iss"] = srv.URL + tc.other
			if _, err := v.Verify(ctx, p.Mint(t, claims)); !errors.Is(err, identity.ErrUnauthenticated) {
				t.Errorf("Verify with iss %q = %v, want ErrUnauthenticated", srv.URL+tc.other, err)
			}
		})
	}
}

func TestNewFailsOnEmptyKeySet(t *testing.T) {
	t.Parallel()
	p := identitytest.NewIdP(t)
	// Discovery succeeds and the warming fetch is what refuses: a key set with
	// nothing usable in it can verify nobody, so it fails the process rather than
	// installing an empty cache that 401s every human.
	p.SetJWKSBody([]byte(`{"keys":[]}`))

	v, err := identity.New(context.Background(),
		discoveryXConfig(p.Issuer(), p.Client(), identitytest.NewClock(discoveryXBase).Now))
	discoveryXWantBootError(t, v, err)
	if got := p.Discoveries(); got != 1 {
		t.Errorf("Discoveries() = %d, want 1", got)
	}
	if got := p.Fetches(); got != 1 {
		t.Errorf("Fetches() = %d, want 1 — the warming fetch is what refused", got)
	}
}

// TestNewValidatesTheRoleMap covers the boundary ConfigFromEnv does not: a
// Config built literally, which is how every test and every later slice
// constructs one. An empty claim value is the dangerous shape — a roles claim
// carrying "" would then be granted whatever it maps to — and a role string
// outside the three is a typo that would silently grant nothing while looking
// configured.
func TestNewValidatesTheRoleMap(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		m    map[string]identity.Role
	}{
		{name: "empty claim value", m: map[string]identity.Role{"": identity.RoleAdmin}},
		{name: "empty value beside a good one", m: map[string]identity.Role{
			"platform-admins": identity.RoleAdmin, "": identity.RoleViewer}},
		{name: "unknown role", m: map[string]identity.Role{"eng": identity.Role("superuser")}},
		{name: "RoleNone as a target", m: map[string]identity.Role{"eng": identity.RoleNone}},
		{name: "role differing only in case", m: map[string]identity.Role{"eng": identity.Role("Admin")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := identitytest.NewIdP(t)
			cfg := discoveryXConfig(p.Issuer(), p.Client(), identitytest.NewClock(discoveryXBase).Now)
			cfg.RoleMap = tc.m

			v, err := identity.New(context.Background(), cfg)
			discoveryXWantBootError(t, v, err)
			if got, fetches := p.Discoveries(), p.Fetches(); got != 0 || fetches != 0 {
				t.Errorf("Discoveries() = %d, Fetches() = %d, want 0 and 0 — the role map is refused before any network call", got, fetches)
			}
		})
	}
}

// TestNewCopiesTheRoleMap pins that authority stops being editable once New
// returns. The map is the whole role policy; keeping the caller's would make
// every live Verify race a caller that still holds it, and would let a mutation
// anywhere in the process silently change who is an admin.
func TestNewCopiesTheRoleMap(t *testing.T) {
	t.Parallel()
	p := identitytest.NewIdP(t)
	clock := identitytest.NewClock(discoveryXBase)
	cfg := discoveryXConfig(p.Issuer(), p.Client(), clock.Now)
	caller := map[string]identity.Role{"platform-admins": identity.RoleAdmin}
	cfg.RoleMap = caller

	v, err := identity.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// The caller escalates its own map after construction, exactly as a config
	// reload or a careless test helper would.
	caller["everyone"] = identity.RoleAdmin
	delete(caller, "platform-admins")

	claims := p.Claims(discoveryXAudience, clock.Now())
	claims["roles"] = []any{"everyone"}
	got, err := v.Verify(context.Background(), p.Mint(t, claims))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Role != identity.RoleNone {
		t.Errorf("Role = %q for a value added to the caller's map after New; want %q",
			got.Role, identity.RoleNone)
	}

	claims["roles"] = []any{"platform-admins"}
	got, err = v.Verify(context.Background(), p.Mint(t, claims))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Role != identity.RoleAdmin {
		t.Errorf("Role = %q for a value DELETED from the caller's map after New; want %q — "+
			"the verifier keeps the policy it was built with", got.Role, identity.RoleAdmin)
	}
}

// TestNewRefusesAnIssuerWithAQueryOrFragment pins the issuer identifier's own
// rule (OIDC Discovery §2). It matters twice over: iss is compared as an exact
// string, and the discovery path is built by appending to the issuer, which a
// query silently breaks.
func TestNewRefusesAnIssuerWithAQueryOrFragment(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, suffix string }{
		{name: "query", suffix: "?tenant=acme"},
		{name: "empty query", suffix: "?"},
		{name: "fragment", suffix: "#frag"},
		{name: "both", suffix: "?tenant=acme#frag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := identitytest.NewIdP(t)
			cfg := discoveryXConfig(p.Issuer()+tc.suffix, p.Client(), identitytest.NewClock(discoveryXBase).Now)

			v, err := identity.New(context.Background(), cfg)
			discoveryXWantBootError(t, v, err)
			if got := p.Discoveries(); got != 0 {
				t.Errorf("Discoveries() = %d, want 0 — the issuer is refused before any network call", got)
			}
		})
	}
}

// TestNewValidatesTheAssertionHeaderAgainstTheMode pins the pairing the API
// layer's lane dispatch depends on. AssertionHeader() == "" is the documented
// signal for oidc mode, so the two mismatches are not cosmetic: a trusted_proxy
// verifier without a header has the dispatch read the header named "", which
// nothing ever matches, and an oidc verifier WITH one has it read a request
// header the client controls instead of Authorization.
func TestNewValidatesTheAssertionHeaderAgainstTheMode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mode   identity.Mode
		header string
	}{
		{name: "trusted_proxy without a header", mode: identity.ModeTrustedProxy},
		{name: "oidc with a header", mode: identity.ModeOIDC, header: "x-goog-iap-jwt-assertion"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := identitytest.NewIdP(t)
			cfg := discoveryXConfig(p.Issuer(), p.Client(), identitytest.NewClock(discoveryXBase).Now)
			cfg.Mode, cfg.AssertionHeader = tc.mode, tc.header

			v, err := identity.New(context.Background(), cfg)
			discoveryXWantBootError(t, v, err)
			if got := p.Discoveries(); got != 0 {
				t.Errorf("Discoveries() = %d, want 0 — the pairing is checked before any network call", got)
			}
		})
	}
}

// TestNewRefusesAZeroClock closes a silent trap rather than an attack.
// go-jose's jwt.Expected treats a zero Time as "use time.Now()"
// (jwt/validation.go), and Config.Now is exported precisely so later slices can
// drive expiry — so a fake clock starting at time.Time{}, the natural zero value,
// would validate exp/nbf/iat against the real wall clock while the key-set TTL
// ran against the fake one. Nothing would say so.
func TestNewRefusesAZeroClock(t *testing.T) {
	t.Parallel()
	p := identitytest.NewIdP(t)
	cfg := discoveryXConfig(p.Issuer(), p.Client(), func() time.Time { return time.Time{} })

	v, err := identity.New(context.Background(), cfg)
	discoveryXWantBootError(t, v, err)
	if got := p.Discoveries(); got != 0 {
		t.Errorf("Discoveries() = %d, want 0 — the clock is checked before any network call", got)
	}
}

func TestNewRefusesModeDisabled(t *testing.T) {
	t.Parallel()
	// Disabled is FromEnv's case, and it is the one state that must be
	// byte-for-byte today's platform: New must refuse it rather than build a
	// verifier nothing is meant to consult.
	for _, tc := range []struct {
		name string
		mode identity.Mode
	}{
		{name: "disabled", mode: identity.ModeDisabled},
		{name: "zero value", mode: identity.Mode("")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := identitytest.NewIdP(t)
			cfg := discoveryXConfig(p.Issuer(), p.Client(), identitytest.NewClock(discoveryXBase).Now)
			cfg.Mode = tc.mode

			v, err := identity.New(context.Background(), cfg)
			discoveryXWantBootError(t, v, err)
			// Otherwise valid configuration, so the mode is the whole reason — and
			// nothing was dialed on the way to finding that out.
			if got, fetches := p.Discoveries(), p.Fetches(); got != 0 || fetches != 0 {
				t.Errorf("Discoveries() = %d, Fetches() = %d, want 0 and 0 — the mode is refused before any network call", got, fetches)
			}
		})
	}
}
