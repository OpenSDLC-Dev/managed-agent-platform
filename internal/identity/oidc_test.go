package identity_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity/identitytest"
)

const (
	oidcRedirect = "https://console.example/api/auth/callback"
	// RFC 7636 Appendix B's independent S256 vector.
	oidcVerifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
)

func oidcConfig(p *identitytest.IdP) identity.Config {
	return identity.Config{Mode: identity.ModeOIDC, Issuer: p.Issuer(), Audience: "console", HTTPClient: p.Client()}
}

// Removing a PKCE/redirect parameter, or authenticating a public client with a
// secret, must break this real HTTP exchange against the signed-token fixture.
func TestOIDCCodeFlow(t *testing.T) {
	for _, auth := range []string{"public", "basic", "post"} {
		t.Run(auth, func(t *testing.T) {
			p := identitytest.NewIdP(t)
			cfg := oidcConfig(p)
			cfg.RoleMap = map[string]identity.Role{"devs": identity.RoleDeveloper}
			secret := ""
			if auth != "public" {
				secret = "client secret:+&"
			}
			if auth == "post" {
				p.SetDiscovery(map[string]any{
					"issuer": p.Issuer(), "jwks_uri": p.JWKSURL(),
					"authorization_endpoint":                p.Issuer() + "/authorize?tenant=kept",
					"token_endpoint":                        p.Issuer() + "/token",
					"token_endpoint_auth_methods_supported": []string{"client_secret_post"},
				})
			}
			claims := p.Claims("console", time.Now())
			claims["nonce"], claims["email"], claims["name"], claims["roles"] = "nonce-value", "ada@example.test", "Ada", []string{"devs"}
			token := p.Mint(t, claims)
			var exchanges atomic.Int64
			p.SetTokenHandler(func(w http.ResponseWriter, r *http.Request) {
				exchanges.Add(1)
				if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.ParseForm() != nil {
					t.Error("exchange must be a form POST")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				want := url.Values{"grant_type": {"authorization_code"}, "code": {"code-value"}, "redirect_uri": {oidcRedirect}, "code_verifier": {oidcVerifier}}
				if auth != "basic" {
					want.Set("client_id", "console")
				}
				if auth == "post" {
					want.Set("client_secret", secret)
				}
				if !reflect.DeepEqual(r.PostForm, want) {
					t.Errorf("exchange form fields mismatch: got keys %v", reflect.ValueOf(r.PostForm).MapKeys())
				}
				user, password, basic := r.BasicAuth()
				if auth == "basic" {
					if !basic || user != "console" || password != url.QueryEscape(secret) {
						t.Error("confidential client did not use OAuth client_secret_basic encoding")
					}
				} else if r.Header.Get("Authorization") != "" {
					t.Error("unexpected client authorization header")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"id_token": token, "access_token": "discard-access", "refresh_token": "discard-refresh", "token_type": "Bearer"})
			})
			rp, err := identity.NewRelyingParty(context.Background(), cfg, "console", secret, oidcRedirect)
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(rp.AuthorizationURL("state-value", "nonce-value", oidcVerifier))
			if err != nil {
				t.Fatal(err)
			}
			want := url.Values{
				"response_type": {"code"}, "client_id": {"console"}, "scope": {"openid profile email"},
				"redirect_uri": {oidcRedirect}, "state": {"state-value"}, "nonce": {"nonce-value"},
				"code_challenge_method": {"S256"}, "code_challenge": {"E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"},
			}
			if auth == "post" {
				want.Set("tenant", "kept")
			}
			if u.Scheme+"://"+u.Host+u.Path != p.Issuer()+"/authorize" || !reflect.DeepEqual(u.Query(), want) {
				t.Fatalf("authorization URL does not carry exact code/nonce/PKCE contract: %s", u)
			}
			id, err := rp.Exchange(context.Background(), "code-value", oidcVerifier, "nonce-value")
			if err != nil {
				t.Fatal(err)
			}
			if id != (identity.Identity{Issuer: p.Issuer(), Subject: "user-1", Email: "ada@example.test", DisplayName: "Ada", Role: identity.RoleDeveloper}) || exchanges.Load() != 1 {
				t.Fatalf("exchange did not return the verified identity: %+v, calls %d", id, exchanges.Load())
			}
			if p.Discoveries() != 1 || p.Fetches() != 1 {
				t.Fatalf("RP did not discover and warm exactly once: %d/%d", p.Discoveries(), p.Fetches())
			}
		})
	}
}

// Checking an unsigned nonce, skipping signature verification, or accepting a
// token for another client must fail even when the supplied nonce matches.
func TestOIDCVerifiedNonce(t *testing.T) {
	p, clock, v := verifierXFixture(t)
	for _, tc := range []struct {
		name     string
		mutate   func(map[string]any)
		expected string
		forge    bool
		wantOK   bool
	}{
		{name: "valid", expected: "bound", wantOK: true},
		{name: "multiple audiences authorized", expected: "bound", wantOK: true, mutate: func(c map[string]any) { c["aud"], c["azp"] = []string{"console", "other"}, "console" }},
		{name: "wrong nonce", expected: "other"},
		{name: "empty expected nonce"},
		{name: "missing nonce", expected: "bound", mutate: func(c map[string]any) { delete(c, "nonce") }},
		{name: "non-string nonce", expected: "bound", mutate: func(c map[string]any) { c["nonce"] = []string{"bound"} }},
		{name: "wrong issuer", expected: "bound", mutate: func(c map[string]any) { c["iss"] = "https://other.example" }},
		{name: "wrong audience", expected: "bound", mutate: func(c map[string]any) { c["aud"] = "other" }},
		{name: "multiple audiences missing azp", expected: "bound", mutate: func(c map[string]any) { c["aud"] = []string{"console", "other"} }},
		{name: "wrong azp", expected: "bound", mutate: func(c map[string]any) { c["aud"], c["azp"] = []string{"console", "other"}, "other" }},
		{name: "single audience wrong azp", expected: "bound", mutate: func(c map[string]any) { c["azp"] = "other" }},
		{name: "null azp", expected: "bound", mutate: func(c map[string]any) { c["azp"] = nil }},
		{name: "expired", expected: "bound", mutate: func(c map[string]any) { c["exp"] = clock.Now().Add(-2 * time.Minute).Unix() }},
		{name: "unsigned matching nonce", expected: "bound", forge: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := verifierXClaims(p, clock)
			claims["nonce"] = "bound"
			if tc.mutate != nil {
				tc.mutate(claims)
			}
			token := p.Mint(t, claims)
			if tc.forge {
				token = p.MintRaw(t, map[string]any{"alg": "RS256", "kid": p.ActiveKID()}, claims, func([]byte) []byte { return []byte("forged") })
			}
			id, err := v.VerifyNonce(context.Background(), token, tc.expected)
			if tc.wantOK {
				verifierXAccepted(t, tc.name, id, err, verifierXWant(p))
			} else {
				verifierXRejected(t, tc.name, id, err)
			}
		})
	}
	t.Run("cancelled with warm keys", func(t *testing.T) {
		claims := verifierXClaims(p, clock)
		claims["nonce"] = "bound"
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		id, err := v.VerifyNonce(ctx, p.Mint(t, claims), "bound")
		verifierXRejected(t, "cancelled verification", id, err)
	})
	// Verifier-only users still have no nonce requirement.
	if _, err := v.Verify(context.Background(), p.Mint(t, verifierXClaims(p, clock))); err != nil {
		t.Fatalf("legacy Verify acquired a nonce requirement: %v", err)
	}
}

func TestOIDCRelyingPartyMetadata(t *testing.T) {
	for _, fault := range []string{"issuer", "missing authorization", "missing token", "insecure authorization", "insecure token", "credentials", "fragment", "oversize", "client auth"} {
		t.Run(fault, func(t *testing.T) {
			p := identitytest.NewIdP(t)
			doc := map[string]any{"issuer": p.Issuer(), "jwks_uri": p.JWKSURL(), "authorization_endpoint": p.Issuer() + "/authorize", "token_endpoint": p.Issuer() + "/token"}
			switch fault {
			case "issuer":
				doc["issuer"] = "https://other.example"
			case "missing authorization":
				delete(doc, "authorization_endpoint")
			case "missing token":
				delete(doc, "token_endpoint")
			case "insecure authorization":
				doc["authorization_endpoint"] = "http://idp.example/authorize"
			case "insecure token":
				doc["token_endpoint"] = "http://169.254.169.254/token"
			case "credentials":
				doc["token_endpoint"] = "https://user:secret@idp.example/token"
			case "fragment":
				doc["token_endpoint"] = "https://idp.example/token#hidden"
			case "oversize":
				doc["pad"] = strings.Repeat("x", 128<<10)
			case "client auth":
				doc["token_endpoint_auth_methods_supported"] = []string{"private_key_jwt"}
			}
			p.SetDiscovery(doc)
			cfg := oidcConfig(p)
			cfg.JWKSURL = p.JWKSURL() // Pinning keys must not skip RP metadata validation.
			rp, err := identity.NewRelyingParty(context.Background(), cfg, "console", "secret", oidcRedirect)
			if err == nil || rp != nil {
				t.Fatalf("RP accepted bad %s metadata", fault)
			}
		})
	}
	for _, fault := range []string{"mode", "client ID", "audience", "redirect", "redirect fragment"} {
		t.Run(fault, func(t *testing.T) {
			p := identitytest.NewIdP(t)
			cfg, clientID, redirect := oidcConfig(p), "console", oidcRedirect
			switch fault {
			case "mode":
				cfg.Mode = identity.ModeDisabled
			case "client ID":
				clientID = ""
			case "audience":
				clientID = "other"
			case "redirect":
				redirect = "http://console.example/callback"
			case "redirect fragment":
				redirect += "#fragment"
			}
			if rp, err := identity.NewRelyingParty(context.Background(), cfg, clientID, "", redirect); err == nil || rp != nil {
				t.Fatalf("accepted invalid %s config", fault)
			}
			if p.Discoveries() != 0 || p.Fetches() != 0 {
				t.Fatal("invalid local config reached provider")
			}
		})
	}
}

func TestOIDCVerifierOnlyDiscoveryIsUnchanged(t *testing.T) {
	p := identitytest.NewIdP(t)
	p.SetDiscovery(map[string]any{
		"issuer": p.Issuer(), "jwks_uri": p.JWKSURL(),
		"authorization_endpoint": 42, "token_endpoint": false,
		"token_endpoint_auth_methods_supported": "not-an-array",
	})
	if _, err := identity.New(context.Background(), oidcConfig(p)); err != nil {
		t.Fatalf("verifier-only discovery started validating RP-only metadata: %v", err)
	}
	// A pinned JWKS skips discovery outright, including a poisoned issuer.
	p.SetDiscovery(map[string]any{"issuer": "https://other.example"})
	cfg := oidcConfig(p)
	cfg.JWKSURL = p.JWKSURL()
	if _, err := identity.New(context.Background(), cfg); err != nil || p.Discoveries() != 1 {
		t.Fatalf("pinned verifier unexpectedly consulted discovery: %v", err)
	}
	// A relying party must discover endpoints, but still use the configured key
	// source instead of the metadata's valid-looking, unreachable JWKS location.
	p.SetDiscovery(map[string]any{
		"issuer": p.Issuer(), "jwks_uri": p.Issuer() + "/missing-keys",
		"authorization_endpoint": p.Issuer() + "/authorize", "token_endpoint": p.Issuer() + "/token",
	})
	if _, err := identity.NewRelyingParty(context.Background(), cfg, "console", "", oidcRedirect); err != nil {
		t.Fatalf("RP ignored pinned JWKS: %v", err)
	}
	if p.Discoveries() != 2 || p.Fetches() != 3 {
		t.Fatal("RP did not retain endpoint discovery and pinned key warming")
	}
}

func TestOIDCTokenResponseAtBodyLimit(t *testing.T) {
	p := identitytest.NewIdP(t)
	claims := p.Claims("console", time.Now())
	claims["nonce"] = "bound"
	body := `{"id_token":"` + p.Mint(t, claims) + `"}`
	body += strings.Repeat(" ", (128<<10)-len(body))
	p.SetTokenHandler(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
	rp, err := identity.NewRelyingParty(context.Background(), oidcConfig(p), "console", "", oidcRedirect)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := rp.Exchange(context.Background(), "code", oidcVerifier, "bound"); err != nil || id.Subject != "user-1" {
		t.Fatalf("response exactly at the body limit must remain valid: %v", err)
	}
}

// Provider error bodies and transport errors are untrusted, credential-bearing
// input. Neither Error, Reason nor an ordinary log of the error may quote them.
func TestOIDCErrorsDoNotExposeCredentials(t *testing.T) {
	const canary = "OIDC-CREDENTIAL-CANARY"
	p := identitytest.NewIdP(t)
	rp, err := identity.NewRelyingParty(context.Background(), oidcConfig(p), "console", canary, oidcRedirect)
	if err != nil {
		t.Fatal(err)
	}
	claims := p.Claims("console", time.Now())
	claims["nonce"] = "bound"
	token := p.Mint(t, claims)
	validBody := `{"id_token":"` + token + `"}`
	for _, tc := range []struct {
		name   string
		status int
		body   string
		nonce  string
	}{
		{"provider error", 400, `{"error":"invalid_grant","error_description":"` + canary + `"}`, "bound"},
		{"non-OK valid token", 502, validBody, "bound"},
		{"error with ID token", 200, `{"error":"` + canary + `","id_token":"` + token + `"}`, "bound"},
		{"missing token", 200, `{"access_token":"` + canary + `","refresh_token":"` + canary + `"}`, "bound"},
		{"malformed", 200, `{"id_token":` + canary, "bound"},
		{"wrong nonce", 200, validBody, "other"},
		{"oversize token response", 200, validBody + strings.Repeat(" ", (128<<10)+1-len(validBody)), "bound"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p.SetTokenHandler(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			id, err := rp.Exchange(context.Background(), canary, oidcVerifier, tc.nonce)
			rejected := verifierXRejected(t, tc.name, id, err)
			var logs bytes.Buffer
			slog.New(slog.NewJSONHandler(&logs, nil)).Error("exchange", "error", err, "reason", rejected.Reason())
			for _, output := range []string{err.Error(), rejected.Reason(), fmt.Sprintf("%+v", err), logs.String()} {
				if strings.Contains(output, canary) || strings.Contains(output, token) {
					t.Fatal("exchange leaked credential bytes")
				}
			}
		})
	}
	// Whole-value formatting must not accidentally expose the held client secret.
	for _, output := range []string{fmt.Sprint(rp), fmt.Sprintf("%+v", rp), fmt.Sprintf("%#v", rp)} {
		if strings.Contains(output, canary) {
			t.Fatal("RP formatting disclosed client secret")
		}
	}
	client := p.Client()
	client.Transport = oidcRoundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New(canary) })
	_, err = rp.Exchange(context.Background(), canary, oidcVerifier, "bound")
	if err == nil || strings.Contains(fmt.Sprintf("%+v", err), canary) {
		t.Fatal("transport error leaked credential")
	}
}

type oidcRoundTripFunc func(*http.Request) (*http.Response, error)

func (f oidcRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOIDCExchangeUsesVerifiedNonce(t *testing.T) {
	p := identitytest.NewIdP(t)
	rp, err := identity.NewRelyingParty(context.Background(), oidcConfig(p), "console", "", oidcRedirect)
	if err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"missing nonce", "wrong issuer", "wrong audience", "wrong azp", "signature"} {
		t.Run(fault, func(t *testing.T) {
			claims := p.Claims("console", time.Now())
			claims["nonce"] = "bound"
			switch fault {
			case "missing nonce":
				delete(claims, "nonce")
			case "wrong issuer":
				claims["iss"] = "https://other.example"
			case "wrong audience":
				claims["aud"] = "other"
			case "wrong azp":
				claims["azp"] = "other"
			}
			token := p.Mint(t, claims)
			if fault == "signature" {
				token = p.MintRaw(t, map[string]any{"alg": "RS256", "kid": p.ActiveKID()}, claims, func([]byte) []byte { return []byte("forged") })
			}
			p.SetTokenHandler(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]string{"id_token": token})
			})
			id, err := rp.Exchange(context.Background(), "code", oidcVerifier, "bound")
			verifierXRejected(t, fault, id, err)
		})
	}
}

// Done signals that a follower reached the shared-flight wait, not just that its
// goroutine was launched. Its context never cancels the shared fetch.
type oidcWaitingContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *oidcWaitingContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

type oidcVerifyResult struct {
	id  identity.Identity
	err error
}

func TestOIDCCancelDuringRefreshKeepsFollower(t *testing.T) {
	p, clock, v := verifierXFixture(t)
	p.Rotate(t)
	clock.Advance(identity.RefreshCooldownForTest)
	claims := verifierXClaims(p, clock)
	claims["nonce"] = "bound"
	token := p.Mint(t, claims)
	release := make(chan struct{})
	releaseOnce := jwksXRelease(t, release)
	p.BlockJWKS(release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	leader := make(chan oidcVerifyResult, 1)
	go func() { id, err := v.VerifyNonce(ctx, token, "bound"); leader <- oidcVerifyResult{id, err} }()
	jwksXAwaitFetch(t, p, 2)

	followerCtx := &oidcWaitingContext{Context: context.Background(), waiting: make(chan struct{})}
	follower := make(chan oidcVerifyResult, 1)
	go func() { id, err := v.VerifyNonce(followerCtx, token, "bound"); follower <- oidcVerifyResult{id, err} }()
	select {
	case <-followerCtx.waiting:
	case <-time.After(time.Second):
		t.Fatal("follower did not join the held refresh")
	}
	cancel()
	var got oidcVerifyResult
	select {
	case got = <-leader:
	case <-time.After(time.Second):
		t.Error("cancelled leader did not abandon the observed JWKS refresh before provider release")
		releaseOnce()
		got = <-leader
	}
	verifierXRejected(t, "cancelled refresh leader", got.id, got.err)
	releaseOnce()
	select {
	case got = <-follower:
		verifierXAccepted(t, "follower after leader cancellation", got.id, got.err, verifierXWant(p))
	case <-time.After(2 * time.Second):
		t.Fatal("follower did not finish after provider release")
	}
	jwksXWantFetches(t, p, 2, "leader cancellation must not cancel or restart the shared refresh")
}

func TestOIDCExchangeDeadlineCoversRefresh(t *testing.T) {
	p := identitytest.NewIdP(t)
	clock := identitytest.NewClock(time.Now())
	cfg := oidcConfig(p)
	cfg.Now = clock.Now
	transport := cfg.HTTPClient.Transport
	deadlines := make(chan time.Time, 1)
	cfg.HTTPClient.Transport = oidcRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/token" {
			deadline, ok := r.Context().Deadline()
			if !ok {
				t.Error("exchange has no overall deadline")
			}
			deadlines <- deadline
		}
		return transport.RoundTrip(r)
	})
	rp, err := identity.NewRelyingParty(context.Background(), cfg, "console", "", oidcRedirect)
	if err != nil {
		t.Fatal(err)
	}
	p.Rotate(t)
	clock.Advance(identity.RefreshCooldownForTest)
	claims := p.Claims("console", clock.Now())
	claims["nonce"] = "bound"
	token := p.Mint(t, claims)
	releaseToken := make(chan struct{})
	releaseTokenOnce := jwksXRelease(t, releaseToken)
	p.SetTokenHandler(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-releaseToken:
			_ = json.NewEncoder(w).Encode(map[string]string{"id_token": token})
		case <-r.Context().Done():
		}
	})
	releaseKeys := make(chan struct{})
	releaseKeysOnce := jwksXRelease(t, releaseKeys)
	p.BlockJWKS(releaseKeys)
	done := make(chan oidcVerifyResult, 1)
	go func() {
		id, err := rp.Exchange(context.Background(), "code", oidcVerifier, "bound")
		done <- oidcVerifyResult{id, err}
	}()
	var deadline time.Time
	select {
	case deadline = <-deadlines:
	case <-time.After(time.Second):
		t.Fatal("token POST did not enter its transport")
	}
	// Spend part of the actual request budget at the token endpoint. The held
	// JWKS response must not receive a fresh five-second caller budget afterward.
	timer := time.NewTimer(time.Until(deadline.Add(-2 * time.Second)))
	defer timer.Stop()
	<-timer.C
	releaseTokenOnce()
	jwksXAwaitFetch(t, p, 2)
	t.Logf("rotated-key refresh entered with %s of overall exchange budget left", time.Until(deadline))
	limit := time.NewTimer(time.Until(deadline.Add(250 * time.Millisecond)))
	defer limit.Stop()
	var got oidcVerifyResult
	select {
	case got = <-done:
	case <-limit.C:
		t.Error("Exchange did not abandon the held JWKS refresh at its overall deadline")
		releaseKeysOnce()
		got = <-done
	}
	verifierXRejected(t, "overall exchange deadline", got.id, got.err)
	releaseKeysOnce()
}

func TestOIDCCancelBeforeVerifiedReturn(t *testing.T) {
	p, clock := verifierXIdP(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var armed atomic.Bool
	var readings atomic.Int64
	cfg := verifierXConfig(p, clock, nil)
	cfg.Now = func() time.Time {
		// With warm keys, the first reading checks the cache and the second
		// supplies the authenticated claims' time check just before return.
		if armed.Load() && readings.Add(1) == 2 {
			cancel()
		}
		return clock.Now()
	}
	v, err := identity.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	claims := verifierXClaims(p, clock)
	claims["nonce"] = "bound"
	token := p.Mint(t, claims)
	armed.Store(true)
	id, err := v.VerifyNonce(ctx, token, "bound")
	if ctx.Err() != context.Canceled {
		t.Fatal("fixture did not cancel during the verified-claims time check")
	}
	verifierXRejected(t, "cancellation before identity return", id, err)
}

func TestOIDCDiagnosticFormatting(t *testing.T) {
	const secret = "FIX1-SYNTHETIC-RP-CLIENT-SECRET"
	p := identitytest.NewIdP(t)
	rp, err := identity.NewRelyingParty(context.Background(), oidcConfig(p), "console", secret, oidcRedirect)
	if err != nil {
		t.Fatal(err)
	}
	for _, form := range []struct {
		name  string
		value any
	}{
		{"pointer", rp}, {"value", *rp},
		{"nested pointer", struct{ RP *identity.RelyingParty }{rp}},
		{"nested value", struct{ RP identity.RelyingParty }{*rp}},
		{"slice", []any{rp, *rp}},
		{"map", map[string]any{"rp": rp, "value": *rp}},
	} {
		t.Run(form.name, func(t *testing.T) {
			formats := []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x", "%X", "%t", "%f"}
			if form.name == "pointer" {
				formats = append(formats, "%p")
			}
			for _, format := range formats {
				output := fmt.Sprintf(format, form.value)
				for _, canary := range []string{secret, fmt.Sprintf("%x", secret), fmt.Sprintf("%X", secret)} {
					if strings.Contains(output, canary) {
						t.Errorf("%s of %s disclosed the synthetic client secret", format, form.name)
					}
				}
			}
			encoded, err := json.Marshal(form.value)
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			slog.New(slog.NewJSONHandler(&logs, nil)).Info("relying party", "rp", form.value)
			if strings.Contains(string(encoded), secret) || strings.Contains(logs.String(), secret) {
				t.Error("JSON or log rendering disclosed the synthetic client secret")
			}
		})
	}
}

func TestOIDCTransportPolicy(t *testing.T) {
	t.Run("redirect", func(t *testing.T) {
		p := identitytest.NewIdP(t)
		var hits atomic.Int64
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1); _, _ = w.Write([]byte(`{}`)) }))
		t.Cleanup(target.Close)
		p.SetTokenHandler(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
		})
		cfg := oidcConfig(p)
		cfg.HTTPClient = &http.Client{Transport: p.Client().Transport, CheckRedirect: identity.ProductionClientForTest().CheckRedirect}
		rp, err := identity.NewRelyingParty(context.Background(), cfg, "console", "secret", oidcRedirect)
		if err != nil {
			t.Fatal(err)
		}
		id, err := rp.Exchange(context.Background(), "code", oidcVerifier, "bound")
		verifierXRejected(t, "redirect", id, err)
		if hits.Load() != 0 {
			t.Fatal("exchange followed a token redirect")
		}
	})
	t.Run("dial guard", func(t *testing.T) {
		p := identitytest.NewIdP(t)
		client := p.Client()
		rp, err := identity.NewRelyingParty(context.Background(), oidcConfig(p), "console", "", oidcRedirect)
		if err != nil {
			t.Fatal(err)
		}
		var hits atomic.Int64
		p.SetTokenHandler(func(http.ResponseWriter, *http.Request) { hits.Add(1) })
		// Keep the warmed RP, then use production's exact guarded transport.
		client.Transport = identity.ProductionClientForTest().Transport
		id, err := rp.Exchange(context.Background(), "code", oidcVerifier, "bound")
		verifierXRejected(t, "loopback dial", id, err)
		if hits.Load() != 0 {
			t.Fatal("production exchange dialed loopback")
		}
	})
	t.Run("header bound", func(t *testing.T) {
		p := identitytest.NewIdP(t)
		cfg := oidcConfig(p)
		transport := identity.ProductionClientForTest().Transport.(*http.Transport).Clone()
		transport.DialContext = (&net.Dialer{}).DialContext // only fixture loopback admission differs
		t.Cleanup(transport.CloseIdleConnections)
		cfg.HTTPClient = &http.Client{Transport: transport, CheckRedirect: identity.ProductionClientForTest().CheckRedirect}
		rp, err := identity.NewRelyingParty(context.Background(), cfg, "console", "", oidcRedirect)
		if err != nil {
			t.Fatal(err)
		}
		claims := p.Claims("console", time.Now())
		claims["nonce"] = "bound"
		token := p.Mint(t, claims)
		p.SetTokenHandler(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Pad", strings.Repeat("x", 65<<10))
			_ = json.NewEncoder(w).Encode(map[string]string{"id_token": token})
		})
		id, err := rp.Exchange(context.Background(), "code", oidcVerifier, "bound")
		verifierXRejected(t, "oversize headers", id, err)
	})
	t.Run("exchange deadline", func(t *testing.T) {
		p := identitytest.NewIdP(t)
		claims := p.Claims("console", time.Now())
		claims["nonce"] = "bound"
		token := p.Mint(t, claims)
		p.SetTokenHandler(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]string{"id_token": token})
		})
		cfg := oidcConfig(p)
		transport := cfg.HTTPClient.Transport
		cfg.HTTPClient.Transport = oidcRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/token" {
				deadline, ok := r.Context().Deadline()
				if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
					t.Error("exchange did not set its own five-second request deadline")
				}
			}
			return transport.RoundTrip(r)
		})
		rp, err := identity.NewRelyingParty(context.Background(), cfg, "console", "", oidcRedirect)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rp.Exchange(context.Background(), "code", oidcVerifier, "bound"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		p := identitytest.NewIdP(t)
		entered, release := make(chan struct{}), make(chan struct{})
		t.Cleanup(func() { close(release) })
		p.SetTokenHandler(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			select {
			case <-r.Context().Done():
			case <-release:
			}
		})
		rp, err := identity.NewRelyingParty(context.Background(), oidcConfig(p), "console", "", oidcRedirect)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { _, err := rp.Exchange(ctx, "code", oidcVerifier, "bound"); done <- err }()
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("exchange did not reach provider")
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, identity.ErrUnauthenticated) {
				t.Fatalf("cancelled exchange = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("exchange ignored cancellation")
		}
	})
}
