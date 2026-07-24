package gate_test

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gate"
)

// echoOrigin reflects the request's Authorization header and body so a test can
// see what the gate actually forwarded.
func echoOrigin(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"authorization": r.Header.Get("Authorization"),
			"body":          string(body),
		})
	}))
}

// proxyClient is an http.Client that routes through the gate served at gateURL.
// trustedTLS, when non-nil, is an httptest TLS origin whose self-signed
// certificate the client trusts — so the CONNECT tunnel test verifies the origin
// properly rather than skipping verification.
func proxyClient(t *testing.T, gateURL string, trustedTLS *httptest.Server) *http.Client {
	t.Helper()
	u, err := url.Parse(gateURL)
	if err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{Proxy: http.ProxyURL(u)}
	if trustedTLS != nil {
		pool := x509.NewCertPool()
		pool.AddCert(trustedTLS.Certificate())
		tr.TLSClientConfig = &tls.Config{RootCAs: pool}
	}
	return &http.Client{Transport: tr}
}

// hostOf returns the bare host of a server URL (an IP literal for httptest).
func hostOf(t *testing.T, serverURL string) string {
	t.Helper()
	u, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Hostname()
}

func cred(placeholder, secret string, hosts []string) egress.Credential {
	return egress.Credential{
		Placeholder: placeholder, Secret: secret,
		Hosts: egress.NewHostSet(hosts), Header: true, Body: true,
	}
}

func TestGatePlainHTTPSubstitutesForAdmittedHost(t *testing.T) {
	origin := echoOrigin(t)
	defer origin.Close()
	host := hostOf(t, origin.URL)

	g := gate.New(gate.Config{
		Networking:  domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{host}},
		Credentials: []egress.Credential{cred("vltph_tok", "sk-secret", []string{host})},
	})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()

	client := proxyClient(t, gsrv.URL, nil)
	req, _ := http.NewRequest("POST", origin.URL, strings.NewReader("payload=vltph_tok"))
	req.Header.Set("Authorization", "Bearer vltph_tok")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var echoed map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&echoed); err != nil {
		t.Fatal(err)
	}
	// The origin — a stand-in third party — must have received the real secret in
	// both the header and the body, never the placeholder.
	if echoed["authorization"] != "Bearer sk-secret" {
		t.Errorf("origin saw Authorization %q, want the substituted secret", echoed["authorization"])
	}
	if echoed["body"] != "payload=sk-secret" {
		t.Errorf("origin saw body %q, want the substituted secret", echoed["body"])
	}
}

func TestGatePlainHTTPRefusesDisallowedHost(t *testing.T) {
	origin := echoOrigin(t)
	defer origin.Close()

	// limited networking whose allow-list does NOT include the origin's host.
	g := gate.New(gate.Config{
		Networking: domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{"allowed.example"}},
	})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()

	resp, err := proxyClient(t, gsrv.URL, nil).Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a host outside the networking policy", resp.StatusCode)
	}
}

func TestGatePlainHTTPUnreachableCredentialLeftLiteral(t *testing.T) {
	origin := echoOrigin(t)
	defer origin.Close()
	host := hostOf(t, origin.URL)

	var mu sync.Mutex
	var reported [][]string
	g := gate.New(gate.Config{
		// The environment admits the host, but the credential's own allowed_hosts
		// do not — so the request leaves, but with the placeholder literal.
		Networking:  domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{host}},
		Credentials: []egress.Credential{cred("vltph_tok", "sk-secret", []string{"other.example"})},
		OnUnreachable: func(_ string, placeholders []string) {
			mu.Lock()
			defer mu.Unlock()
			reported = append(reported, placeholders)
		},
	})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()

	req, _ := http.NewRequest("GET", origin.URL, nil)
	req.Header.Set("Authorization", "Bearer vltph_tok")
	resp, err := proxyClient(t, gsrv.URL, nil).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var echoed map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&echoed)
	if strings.Contains(echoed["authorization"], "sk-secret") {
		t.Fatalf("secret leaked to a host the credential does not allow: %q", echoed["authorization"])
	}
	if echoed["authorization"] != "Bearer vltph_tok" {
		t.Errorf("Authorization = %q, want the literal placeholder", echoed["authorization"])
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reported) != 1 || len(reported[0]) != 1 || reported[0][0] != "vltph_tok" {
		t.Errorf("OnUnreachable reported %v, want one call with [vltph_tok]", reported)
	}
}

func TestGateConnectTunnelsAdmittedHTTPS(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "secure-ok")
	}))
	defer origin.Close()
	host := hostOf(t, origin.URL)

	g := gate.New(gate.Config{
		Networking: domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{host}},
	})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()

	resp, err := proxyClient(t, gsrv.URL, origin).Get(origin.URL)
	if err != nil {
		t.Fatalf("CONNECT to an admitted host failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "secure-ok" {
		t.Errorf("tunneled response = %q, want secure-ok", body)
	}
}

func TestGateConnectRefusesDisallowedHTTPS(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer origin.Close()

	g := gate.New(gate.Config{
		Networking: domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{"allowed.example"}},
	})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()

	// The CONNECT is refused (403), so the client's tunnel is never established
	// and the HTTPS request errors rather than reaching the origin.
	if _, err := proxyClient(t, gsrv.URL, nil).Get(origin.URL); err == nil {
		t.Error("expected an error reaching a host the networking policy forbids")
	}
}

func TestGateRejectsNonProxyRequest(t *testing.T) {
	g := gate.New(gate.Config{Networking: domain.Networking{Type: domain.NetUnrestricted}})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()

	// A direct (origin-form) request to the gate has a relative URL — it is not a
	// proxy request and is rejected rather than forwarded.
	resp, err := http.Get(gsrv.URL + "/not-a-proxy-request")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a non-proxy request", resp.StatusCode)
	}
}

func TestGatePlainHTTPBadGatewayWhenOriginUnreachable(t *testing.T) {
	origin := echoOrigin(t)
	host := hostOf(t, origin.URL)
	originURL := origin.URL
	origin.Close() // the host is admitted but nothing is listening

	g := gate.New(gate.Config{Networking: domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{host}}})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()

	resp, err := proxyClient(t, gsrv.URL, nil).Get(originURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 when the origin is unreachable", resp.StatusCode)
	}
}

func TestGateConnectBadGatewayWhenOriginUnreachable(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	host := hostOf(t, origin.URL)
	originURL := origin.URL
	origin.Close() // admitted, but the CONNECT dial will fail

	g := gate.New(gate.Config{Networking: domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{host}}})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()

	// The gate answers CONNECT with 502; the client cannot establish the tunnel.
	if _, err := proxyClient(t, gsrv.URL, nil).Get(originURL); err == nil {
		t.Error("expected an error when the CONNECT target is unreachable")
	}
}

func TestGateUnknownNetworkingFailsClosed(t *testing.T) {
	origin := echoOrigin(t)
	defer origin.Close()

	// An unrecognized networking type admits nothing rather than everything.
	g := gate.New(gate.Config{Networking: domain.Networking{Type: "bogus"}})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()

	resp, err := proxyClient(t, gsrv.URL, nil).Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (fail closed) for an unknown networking type", resp.StatusCode)
	}
}

func TestGateUnrestrictedAdmitsAnyHost(t *testing.T) {
	origin := echoOrigin(t)
	defer origin.Close()

	g := gate.New(gate.Config{Networking: domain.Networking{Type: domain.NetUnrestricted}})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()

	resp, err := proxyClient(t, gsrv.URL, nil).Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("unrestricted networking should admit any host, got %d", resp.StatusCode)
	}
}
