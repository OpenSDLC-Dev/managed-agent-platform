package identity_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity/identitytest"
)

// Context values alone do not put W3C fields on the network. The real provider
// must see the caller's trace on discovery, JWKS and token exchange alike.
func TestOIDCPropagatesTraceContext(t *testing.T) {
	for _, tc := range []struct {
		name    string
		context bool
		flags   trace.TraceFlags
		parent  string
	}{
		{"sampled", true, trace.FlagsSampled, "00-0102030405060708090a0b0c0d0e0f10-1112131415161718-01"},
		{"not sampled", true, 0, "00-0102030405060708090a0b0c0d0e0f10-1112131415161718-00"},
		{"no context", false, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := identitytest.NewIdP(t)
			ctx := context.Background()
			state, err := trace.ParseTraceState("provider=synthetic")
			if err != nil {
				t.Fatal(err)
			}
			if tc.context {
				ctx = trace.ContextWithSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
					TraceID: trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}, SpanID: trace.SpanID{17, 18, 19, 20, 21, 22, 23, 24}, TraceFlags: tc.flags, TraceState: state,
				}))
			}
			claims := p.Claims("console", time.Now())
			claims["nonce"] = "trace-nonce"
			p.SetTokenHandler(func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]string{"id_token": p.Mint(t, claims)})
			})
			type observed struct {
				path    string
				headers http.Header
			}
			seen := make(chan observed, 8)
			client := *p.Client()
			transport := client.Transport
			if transport == nil {
				transport = http.DefaultTransport
			}
			client.Transport = oidcRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				seen <- observed{r.Method + " " + r.URL.Path, r.Header.Clone()}
				return transport.RoundTrip(r)
			})
			cfg := oidcConfig(p)
			cfg.HTTPClient = &client
			rp, err := identity.NewRelyingParty(ctx, cfg, "console", "fixture-trace-secret", oidcRedirect)
			if err != nil {
				t.Fatal(err)
			}
			id, err := rp.Exchange(ctx, "trace-code", oidcVerifier, "trace-nonce")
			if err != nil || id.Subject != "user-1" {
				t.Fatalf("trace flow changed identity: %+v %v", id, err)
			}
			want := map[string]bool{"GET /.well-known/openid-configuration": false, "GET /jwks": false, "POST /token": false}
			if len(seen) != len(want) {
				t.Fatalf("observed requests=%d want=%d", len(seen), len(want))
			}
			for len(seen) > 0 {
				got := <-seen
				if _, ok := want[got.path]; !ok {
					t.Fatalf("unexpected provider request %s", got.path)
				}
				want[got.path] = true
				state := ""
				if tc.context {
					state = "provider=synthetic"
				}
				if got.headers.Get("traceparent") != tc.parent || got.headers.Get("tracestate") != state {
					t.Errorf("%s traceparent=%q tracestate=%q want=%q/%q", got.path, got.headers.Get("traceparent"), got.headers.Get("tracestate"), tc.parent, state)
				}
			}
			for path, found := range want {
				if !found {
					t.Errorf("missing provider boundary %s", path)
				}
			}
		})
	}
}
