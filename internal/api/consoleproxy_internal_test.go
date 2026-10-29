package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity/identitytest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"go.opentelemetry.io/otel/trace"
)

// Rebuilding a request instead of cloning it loses raw input, verified actor or trace context.
func TestPrivateProxyForwardingContext(t *testing.T) {
	idp := identitytest.NewIdP(t)
	v, err := identity.New(context.Background(), identity.Config{Mode: identity.ModeTrustedProxy,
		Issuer: idp.Issuer(), Audience: "proxy-unit", JWKSURL: idp.JWKSURL(), AssertionHeader: "X-Edge-Assertion", HTTPClient: idp.Client()})
	if err != nil {
		t.Fatal(err)
	}
	s := &server{login: login.New(nil, login.Config{Mode: login.ModeOIDC, PublicOrigin: "https://console.example"}), verifier: v}
	session := login.Session{PrincipalID: "principal_proxy", Role: identity.RoleDeveloper, ID: "user_proxy", SignedIn: true}
	ctx := context.WithValue(context.Background(), cookieContextKey{}, session)
	ctx = context.WithValue(ctx, ctxKeyIdentity, identityPrincipal{ID: session.PrincipalID, Role: session.Role})
	ctx = context.WithValue(ctx, ctxKeyRequestID, "request-proxy-fixture")
	tid, _ := trace.TraceIDFromHex("0af7651916cd43dd8448eb211c80319c")
	sid, _ := trace.SpanIDFromHex("b7ad6b7169203331")
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled})
	ctx = trace.ContextWithSpanContext(ctx, sc)
	const body = "\x00\xff\r\nnot parsed + %"
	const query = "x=%2F&x=a+b&raw=%ff&beta=true"
	r := httptest.NewRequest("POST", "/%61pi/organizations/%64efault/%70roxy/%761/%61gents/agent_unit?"+query, strings.NewReader(body)).WithContext(ctx)
	r.SetPathValue("org", "default")
	r.Header.Set("Cookie", "map_session=fixture-only")
	r.Header.Set("x-api-key", "must-not-forward")
	r.Header.Set("Authorization", "Bearer must-not-forward")
	r.Header.Set("X-Edge-Assertion", "must-not-replay")
	r.Header.Set("Content-Type", "multipart/form-data; boundary=fixture-boundary")
	r.Header.Set("Accept", "application/binary")
	mux := http.NewServeMux()
	var reached bool
	mux.HandleFunc("POST /v1/agents/{id}", roleGate(identity.RoleDeveloper, func(w http.ResponseWriter, forwarded *http.Request) {
		reached = true
		raw, err := io.ReadAll(forwarded.Body)
		actor, ok := identityFrom(forwarded.Context())
		browser, hasCookie := cookieSessionFrom(forwarded.Context())
		if err != nil || string(raw) != body || forwarded.URL.RawQuery != query || forwarded.RequestURI != "/%761/%61gents/agent_unit?"+query || forwarded.ContentLength != int64(len(body)) || forwarded.PathValue("id") != "agent_unit" {
			t.Errorf("raw forwarding body=%q url=%s requestURI=%s length=%d id=%s err=%v", raw, forwarded.URL, forwarded.RequestURI, forwarded.ContentLength, forwarded.PathValue("id"), err)
		}
		if !ok || actor.ID != session.PrincipalID || actor.Role != identity.RoleDeveloper || !hasCookie || browser != session || principalFrom(forwarded.Context()) != session.PrincipalID || requestIDFrom(forwarded.Context()) != "request-proxy-fixture" || !trace.SpanContextFromContext(forwarded.Context()).Equal(sc) {
			t.Error("forwarding lost verified session, actor, audit, request ID or trace context")
		}
		for _, field := range []string{"X-Api-Key", "Authorization", "X-Edge-Assertion"} {
			if len(forwarded.Header.Values(field)) != 0 {
				t.Errorf("forwarded another credential: %s", field)
			}
		}
		if forwarded.Header.Get("Content-Type") != "multipart/form-data; boundary=fixture-boundary" || forwarded.Header.Get("Accept") != "application/binary" {
			t.Error("forwarding changed content negotiation/upload headers")
		}
		w.WriteHeader(204)
	}))
	w := httptest.NewRecorder()
	s.serveConsoleProxy(dispatchAuth(nil, v, mux))(w, r)
	if !reached || w.Code != 204 {
		t.Fatalf("resource dispatch reached=%t status=%d body=%s", reached, w.Code, w.Body.String())
	}
	if r.URL.EscapedPath() != "/%61pi/organizations/%64efault/%70roxy/%761/%61gents/agent_unit" || r.Header.Get("x-api-key") != "must-not-forward" {
		t.Error("proxy mutated the caller's original request")
	}
	for _, human := range []bool{false, true} {
		ctx := context.Background()
		if human {
			ctx = context.WithValue(ctx, ctxKeyIdentity, identityPrincipal{ID: "principal_not_cookie", Role: identity.RoleAdmin})
		}
		missing := httptest.NewRequest("GET", "/api/organizations/default/proxy/v1/agents", nil).WithContext(ctx)
		w := httptest.NewRecorder()
		s.serveConsoleProxy(mux)(w, missing)
		if w.Code != 401 {
			t.Fatalf("proxy accepted non-cookie provenance: %d", w.Code)
		}
	}
}

// Only same-server management-resource Locations move; outside authorities and machine targets do not.
func TestPrivateProxyResourceRedirects(t *testing.T) {
	for _, tc := range []struct{ name, location, want string }{
		{"root-relative", "/v1/agents/agent_x?x=%2F&x=a+b#part", "/api/organizations/default/proxy/v1/agents/agent_x?x=%2F&x=a+b#part"},
		{"same-server", "http://example.com/v1/agents?x=%2F", "http://example.com/api/organizations/default/proxy/v1/agents?x=%2F"},
		{"same-server-network", "//example.com/v1/agents", "//example.com/api/organizations/default/proxy/v1/agents"},
		{"encoded-literal", "/%761/%61gents/agent_x", "/api/organizations/default/proxy/%761/%61gents/agent_x"},
		{"external", "https://outside.example/v1/agents", "https://outside.example/v1/agents"},
		{"userinfo", "http://other@example.com/v1/agents", "http://other@example.com/v1/agents"},
		{"machine", "/v1/environments/env_x/work", "/v1/environments/env_x/work"},
		{"inference", "/v1/messages", "/v1/messages"},
		{"relative", "agent_x?x=%2F", "agent_x?x=%2F"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &server{login: login.New(nil, login.Config{Mode: login.ModeOIDC, PublicOrigin: "https://console.example"})}
			ctx := context.WithValue(context.Background(), cookieContextKey{}, login.Session{PrincipalID: "principal_redirect", Role: identity.RoleAdmin})
			ctx = context.WithValue(ctx, ctxKeyIdentity, identityPrincipal{ID: "principal_redirect", Role: identity.RoleAdmin})
			r := httptest.NewRequest("GET", "http://example.com/api/organizations/default/proxy/v1/agents", nil).WithContext(ctx)
			r.SetPathValue("org", "default")
			mux := http.NewServeMux()
			mux.HandleFunc("GET /v1/agents", roleGate(identity.RoleDeveloper, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", tc.location)
				w.Header().Set("Content-Type", "application/binary")
				w.WriteHeader(307)
				io.WriteString(w, "raw-redirect-body")
			}))
			w := httptest.NewRecorder()
			s.serveConsoleProxy(dispatchAuth(nil, nil, mux))(w, r)
			if w.Code != 307 || w.Header().Get("Location") != tc.want || w.Body.String() != "raw-redirect-body" || w.Header().Get("Content-Type") != "application/binary" {
				t.Fatalf("redirect=%d location=%q body=%s headers=%v; want location=%q", w.Code, w.Header().Get("Location"), w.Body.String(), w.Header(), tc.want)
			}
		})
	}
}
