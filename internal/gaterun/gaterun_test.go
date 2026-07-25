package gaterun_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gate"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gateconfig"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gaterun"
)

// unreachableHost is guaranteed to fail DNS resolution (RFC 2606 .invalid), so an
// admitted request always ends in a 502 dial failure — distinct from the 403 a
// refused host gets before any dial. That 403-vs-502 split is how a test tells a
// deny-all gate from a permissive one without a stub transport.
const unreachableHost = "origin.invalid"

// proxyStatus sends a plain-HTTP GET for host through the gate server at srvURL
// (used as a forward proxy) and returns the response status.
func proxyStatus(t *testing.T, srvURL, host string) int {
	t.Helper()
	proxyURL, err := url.Parse(srvURL)
	if err != nil {
		t.Fatalf("parse proxy url: %v", err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	resp, err := client.Get("http://" + host + "/")
	if err != nil {
		t.Fatalf("proxied GET %s: %v", host, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestConvert(t *testing.T) {
	cfg := &gateconfig.Config{
		Networking: domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{"api.example.com"}},
		Credentials: []gateconfig.Credential{
			{
				CredentialID: "vcrd_1", Placeholder: "vltph_a", Secret: "S1",
				Networking:        gateconfig.CredentialNetworking{Type: domain.NetLimited, AllowedHosts: []string{"api.example.com"}},
				InjectionLocation: gateconfig.InjectionLocation{Header: true},
			},
			{
				CredentialID: "vcrd_2", Placeholder: "vltph_b", Secret: "S2",
				Networking:        gateconfig.CredentialNetworking{Type: domain.NetUnrestricted},
				InjectionLocation: gateconfig.InjectionLocation{Body: true},
			},
		},
	}
	gc := gaterun.Convert(cfg)

	if gc.Networking.Type != domain.NetLimited || len(gc.Networking.AllowedHosts) != 1 {
		t.Errorf("networking not passed through: %+v", gc.Networking)
	}
	if len(gc.Credentials) != 2 {
		t.Fatalf("credentials = %d, want 2", len(gc.Credentials))
	}

	limited := gc.Credentials[0]
	if limited.Placeholder != "vltph_a" || limited.Secret != "S1" ||
		limited.Unrestricted || !limited.Header || limited.Body {
		t.Errorf("limited credential mismapped: %+v", limited)
	}
	if !limited.Hosts.Match("api.example.com") || limited.Hosts.Match("evil.test") {
		t.Error("limited credential's host set does not gate on allowed_hosts")
	}

	unrestricted := gc.Credentials[1]
	if !unrestricted.Unrestricted || unrestricted.Secret != "S2" || !unrestricted.Body || unrestricted.Header {
		t.Errorf("unrestricted credential mismapped: %+v", unrestricted)
	}
}

func TestSwappableHandler(t *testing.T) {
	// A deny-all gate (empty networking admits nothing) refuses every host: 403
	// before any dial.
	h := gaterun.NewSwappableHandler(gate.New(gate.Config{}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	if got := proxyStatus(t, srv.URL, unreachableHost); got != http.StatusForbidden {
		t.Errorf("deny-all gate: status = %d, want 403", got)
	}

	// Swap in an unrestricted gate: the host is now admitted, so the gate dials
	// the (unresolvable) origin and returns 502 — the 403->502 flip proves the
	// swap is what the next request uses.
	h.Swap(gate.New(gate.Config{Networking: domain.Networking{Type: domain.NetUnrestricted}}))
	if got := proxyStatus(t, srv.URL, unreachableHost); got != http.StatusBadGateway {
		t.Errorf("after swap to unrestricted: status = %d, want 502", got)
	}
}
