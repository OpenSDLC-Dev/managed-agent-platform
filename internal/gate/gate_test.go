package gate_test

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gate"
)

// echoOrigin reflects the request's Authorization header, body, and the
// Content-Length it received so a test can see what the gate actually forwarded.
func echoOrigin(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"authorization":  r.Header.Get("Authorization"),
			"body":           string(body),
			"content_length": strconv.FormatInt(r.ContentLength, 10),
		})
	}))
}

// rawStatusLine dials the gate directly, writes a raw HTTP request, and returns
// its status line. A proxying http.Client masks the gate's own response — a
// CONNECT refusal surfaces as a generic tunnel error (indistinguishable from a
// TLS failure), and a normalized Host header is invisible — so tests that must
// observe the gate's wire response dial it raw.
func rawStatusLine(t *testing.T, gateURL, raw string) string {
	t.Helper()
	u, err := url.Parse(gateURL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, raw); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(line, "\r\n")
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

	// The secret is deliberately a different length from its placeholder, so a
	// forwarded request whose Content-Length was not recomputed after substitution
	// would reach the origin truncated — making the length reset load-bearing.
	g := gate.New(gate.Config{
		Networking:  domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{host}},
		Credentials: []egress.Credential{cred("vltph_tok", "sk-live-super-secret", []string{host})},
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
	if echoed["authorization"] != "Bearer sk-live-super-secret" {
		t.Errorf("origin saw Authorization %q, want the substituted secret", echoed["authorization"])
	}
	wantBody := "payload=sk-live-super-secret"
	if echoed["body"] != wantBody {
		t.Errorf("origin saw body %q, want %q", echoed["body"], wantBody)
	}
	// The Content-Length must be recomputed to the substituted length, or the
	// origin receives a truncated (or over-read) body.
	if echoed["content_length"] != strconv.Itoa(len(wantBody)) {
		t.Errorf("origin saw Content-Length %q, want %d", echoed["content_length"], len(wantBody))
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

	client := proxyClient(t, gsrv.URL, origin)
	// The tunnel now closes only when both directions do; close the pooled
	// tunnel connection so the handler goroutine unwinds at test end.
	defer client.CloseIdleConnections()
	resp, err := client.Get(origin.URL)
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
	g := gate.New(gate.Config{
		Networking: domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{"allowed.example"}},
	})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()

	// Assert the gate answers a forbidden-host CONNECT with 403 on the wire. A
	// proxying http.Client only surfaces a generic tunnel error, which also fires
	// on an unrelated TLS failure — so it cannot prove the host-filter ran (a
	// deleted CONNECT admit check would still leave such a test green). Raw-dial
	// so the 403 (vs. the 502 an unfiltered dial to the refused host would give)
	// is observed directly.
	status := rawStatusLine(t, gsrv.URL, "CONNECT 127.0.0.1:1 HTTP/1.1\r\nHost: 127.0.0.1:1\r\n\r\n")
	if !strings.HasPrefix(status, "HTTP/1.1 403") {
		t.Errorf("CONNECT status line = %q, want HTTP/1.1 403 for a host outside the networking policy", status)
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
	u, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	target := u.Host
	origin.Close() // admitted, but the CONNECT dial will fail

	g := gate.New(gate.Config{Networking: domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{u.Hostname()}}})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()

	// The dial to an admitted-but-unreachable CONNECT target fails before hijack,
	// so the gate answers 502 on the wire (asserted directly — a proxying client
	// would only surface a generic tunnel error, which a 200-then-close would
	// also produce).
	status := rawStatusLine(t, gsrv.URL, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n")
	if !strings.HasPrefix(status, "HTTP/1.1 502") {
		t.Errorf("CONNECT status line = %q, want HTTP/1.1 502 when the target is unreachable", status)
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

// allow_mcp_servers widens `limited` by the hosts the agent declares MCP
// servers at — the sandbox half of a flag the executor already honors for the
// platform's own dial. Driven end to end through the proxy, so what is asserted
// is a request that arrived rather than a predicate that returned true.
func TestGateAdmitsAnMCPServerHostOnlyUnderItsFlag(t *testing.T) {
	origin := echoOrigin(t)
	defer origin.Close()
	// A name the request never targets. Two httptest servers would not do: both
	// listen on 127.0.0.1 and differ only by port, which a host set does not see.
	const elsewhere = "mcp.example.com"

	for name, tc := range map[string]struct {
		net   domain.Networking
		hosts []string
		want  int
	}{
		"the flag admits the declared host": {
			domain.Networking{Type: domain.NetLimited, AllowMCPServers: true},
			[]string{hostOf(t, origin.URL)}, http.StatusOK,
		},
		// The gate is never sent the hosts without the flag; if one reaches it
		// anyway, the flag is still what decides.
		"without it the same host is refused": {
			domain.Networking{Type: domain.NetLimited},
			[]string{hostOf(t, origin.URL)}, http.StatusForbidden,
		},
		"and a host nobody declared, either way": {
			domain.Networking{Type: domain.NetLimited, AllowMCPServers: true},
			[]string{elsewhere}, http.StatusForbidden,
		},
		// The flag widens; it does not replace. An operator's own list still
		// admits what it always did.
		"allowed_hosts still admits its own": {
			domain.Networking{
				Type: domain.NetLimited, AllowMCPServers: true,
				AllowedHosts: []string{hostOf(t, origin.URL)},
			},
			[]string{elsewhere}, http.StatusOK,
		},
		// An unrecognized policy admits nothing, and a widening flag beside it
		// does not make it recognized.
		"an unknown policy stays closed": {
			domain.Networking{Type: "bogus", AllowMCPServers: true},
			[]string{hostOf(t, origin.URL)}, http.StatusForbidden,
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := gate.New(gate.Config{Networking: tc.net, MCPServerHosts: tc.hosts})
			gsrv := httptest.NewServer(g)
			defer gsrv.Close()

			resp, err := proxyClient(t, gsrv.URL, nil).Get(origin.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

// The declared hosts are read, never written to: a policy that appended them
// onto the config's own AllowedHosts array would hand the next reader of that
// slice a list it never configured.
func TestGateDoesNotWriteTheMCPHostsIntoTheConfiguredList(t *testing.T) {
	configured := make([]string, 1, 4) // spare capacity is what makes append destructive
	configured[0] = "api.example.com"
	net := domain.Networking{Type: domain.NetLimited, AllowMCPServers: true, AllowedHosts: configured}

	gate.New(gate.Config{Networking: net, MCPServerHosts: []string{"mcp.example.com"}})

	if got := configured[:cap(configured)]; got[1] != "" {
		t.Errorf("the configured list was written past its length: %q", got)
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

// rawResponseOrigin serves one fixed raw HTTP/1.1 response per connection, so a
// test can craft response headers Go's http.Server would not emit (e.g. a
// Connection-named hop-by-hop header). hostOf its address is the admitted host.
func rawResponseOrigin(t *testing.T, rawResponse string) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					line, err := br.ReadString('\n')
					if err != nil || line == "\r\n" {
						break
					}
				}
				_, _ = io.WriteString(c, rawResponse)
			}(c)
		}
	}()
	return ln
}

func TestGatePlainHTTPStripsResponseConnectionHeaders(t *testing.T) {
	// Two Connection field lines (RFC 7230 §6.1 permits repeats) name two
	// hop-by-hop headers; both must be stripped, X-Keep must survive.
	ln := rawResponseOrigin(t, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: X-Hop\r\nConnection: X-Hop2\r\nX-Hop: leaked\r\nX-Hop2: leaked2\r\nX-Keep: kept\r\n\r\n")
	defer ln.Close()
	host, _, _ := net.SplitHostPort(ln.Addr().String())

	g := gate.New(gate.Config{Networking: domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{host}}})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()

	resp, err := proxyClient(t, gsrv.URL, nil).Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// X-Hop / X-Hop2 are named by the response's Connection field lines, so they
	// are hop-by-hop and must not reach the sandbox; X-Keep is a normal
	// end-to-end header. The transport preserves all three, so the gate must
	// strip the hop-by-hop ones itself — including the one named by the second
	// Connection line.
	if got := resp.Header.Get("X-Hop"); got != "" {
		t.Errorf("X-Hop = %q, want it stripped as a Connection-named response header", got)
	}
	if got := resp.Header.Get("X-Hop2"); got != "" {
		t.Errorf("X-Hop2 = %q, want it stripped — named by a second Connection line", got)
	}
	if got := resp.Header.Get("X-Keep"); got != "kept" {
		t.Errorf("X-Keep = %q, want it forwarded end-to-end", got)
	}
}

func TestGatePlainHTTPBoundsRequestBody(t *testing.T) {
	origin := echoOrigin(t)
	defer origin.Close()
	host := hostOf(t, origin.URL)

	g := gate.New(gate.Config{
		Networking:   domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{host}},
		MaxBodyBytes: 16,
	})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()
	client := proxyClient(t, gsrv.URL, nil)

	// A body at the limit is forwarded normally.
	atLimit, err := client.Post(origin.URL, "text/plain", strings.NewReader(strings.Repeat("A", 16)))
	if err != nil {
		t.Fatal(err)
	}
	atLimit.Body.Close()
	if atLimit.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 for a body at MaxBodyBytes", atLimit.StatusCode)
	}

	// One byte over the limit is refused with 413 rather than buffered unbounded.
	over, err := client.Post(origin.URL, "text/plain", strings.NewReader(strings.Repeat("A", 17)))
	if err != nil {
		t.Fatal(err)
	}
	over.Body.Close()
	if over.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413 for a body over MaxBodyBytes", over.StatusCode)
	}
}

func TestGatePlainHTTPForwardsAuthorizedHostHeader(t *testing.T) {
	// An adversarial sandbox sends an absolute-form proxy request whose Host
	// header names a different vhost than the request-target. The gate authorizes
	// and forwards on the URL authority only, so the spoofed Host must never reach
	// the origin — otherwise a co-hosted forbidden vhost could receive a
	// substituted secret. Go's server derives r.Host from the absolute URI; this
	// test locks that in through the real serving path.
	gotHost := make(chan string, 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost <- r.Host
	}))
	defer origin.Close()
	u, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}

	g := gate.New(gate.Config{Networking: domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{u.Hostname()}}})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()

	status := rawStatusLine(t, gsrv.URL,
		"GET "+origin.URL+"/ HTTP/1.1\r\nHost: forbidden.example\r\nConnection: close\r\n\r\n")
	if !strings.HasPrefix(status, "HTTP/1.1 200") {
		t.Fatalf("status line = %q, want HTTP/1.1 200", status)
	}
	// The origin must see the authorized URL authority, not the spoofed Host.
	if h := <-gotHost; h != u.Host {
		t.Errorf("origin saw Host %q, want the authorized authority %q (spoofed Host must not survive)", h, u.Host)
	}
}

// dialCONNECT opens a raw CONNECT tunnel through the gate to target and returns
// the client conn plus a reader positioned just past the 200 response, ready to
// read tunnelled bytes.
func dialCONNECT(t *testing.T, gateURL, target string) (net.Conn, *bufio.Reader) {
	t.Helper()
	u, err := url.Parse(gateURL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(status, "HTTP/1.1 200") {
		t.Fatalf("CONNECT status = %q, want 200", status)
	}
	for { // consume the rest of the CONNECT response headers
		line, err := br.ReadString('\n')
		if err != nil || line == "\r\n" {
			break
		}
	}
	return conn, br
}

func TestGateConnectHalfCloseDeliversResponse(t *testing.T) {
	// A tunneled client that finishes sending, half-closes its write side, and
	// waits for the origin's reply must still receive the full response — the
	// gate must propagate the half-close as EOF, not tear down both directions on
	// the first pump's EOF.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.ReadAll(c) // drain until the client half-closes its write side
		_, _ = io.WriteString(c, "RESPONSE-AFTER-EOF")
	}()
	host, _, _ := net.SplitHostPort(ln.Addr().String())

	g := gate.New(gate.Config{Networking: domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{host}}})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()

	conn, br := dialCONNECT(t, gsrv.URL, ln.Addr().String())
	defer conn.Close()
	if _, err := io.WriteString(conn, "PING"); err != nil {
		t.Fatal(err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	// A deadline so a regression that stops propagating the half-close (the
	// origin then never sees EOF and never replies) fails cleanly here instead
	// of hanging to the global test timeout.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	reply, err := io.ReadAll(br)
	if err != nil {
		t.Fatal(err)
	}
	if string(reply) != "RESPONSE-AFTER-EOF" {
		t.Errorf("tunneled reply = %q, want RESPONSE-AFTER-EOF (half-close truncated the response?)", reply)
	}
}

func TestGateConnectForwardsPipelinedClientBytes(t *testing.T) {
	// A client may write payload bytes in the same segment as the CONNECT
	// request, before reading the 200. Those bytes land in the server's hijack
	// buffer; the gate must forward them from that buffer, not drop them.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, 4)
		n, _ := io.ReadFull(c, buf)
		// Echo what arrived — empty on a regression that drops the pipelined
		// bytes, so the assertion fails cleanly instead of hanging.
		_, _ = c.Write(buf[:n])
	}()
	host, _, _ := net.SplitHostPort(ln.Addr().String())

	g := gate.New(gate.Config{Networking: domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{host}}})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()

	gu, err := url.Parse(gsrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", gu.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// The CONNECT request and the pipelined payload go out in one write, before
	// reading the 200 — so the payload lands in the server's hijack buffer.
	if _, err := io.WriteString(conn, "CONNECT "+ln.Addr().String()+" HTTP/1.1\r\nHost: "+ln.Addr().String()+"\r\n\r\nPIPE"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(status, "HTTP/1.1 200") {
		t.Fatalf("CONNECT status = %q, want 200", status)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil || line == "\r\n" {
			break
		}
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	echo := make([]byte, 4)
	n, _ := io.ReadFull(br, echo)
	if string(echo[:n]) != "PIPE" {
		t.Errorf("origin echoed %q, want PIPE (pipelined CONNECT bytes were dropped?)", echo[:n])
	}
}

func TestGateConnectIdleTunnelClosedAfterDeadline(t *testing.T) {
	// An established tunnel that moves no bytes in either direction for the
	// idle window must be torn down by the gate — both the client conn and the
	// origin conn — rather than holding its goroutines and sockets forever.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	originClosed := make(chan struct{})
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		// This deadline must sit far beyond the select's 3s wait below: were it
		// shorter, ReadAll would return from the origin's OWN deadline and fire
		// originClosed regardless of the gate, making the origin-side assertion
		// vacuous. At 30s, originClosed can only fire from gate-driven teardown
		// — the watchdog's close, or the half-close cascade it triggers.
		_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
		_, _ = io.ReadAll(c) // returns when the gate closes the upstream side
		close(originClosed)
	}()
	host, _, _ := net.SplitHostPort(ln.Addr().String())

	g := gate.New(gate.Config{
		Networking:        domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{host}},
		TunnelIdleTimeout: 150 * time.Millisecond,
	})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()

	conn, br := dialCONNECT(t, gsrv.URL, ln.Addr().String())
	defer conn.Close()

	// Go silent. The guard deadline only bounds a regression: if the gate never
	// cuts the idle tunnel, the read times out instead of hanging the test.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := br.ReadByte(); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("idle tunnel client read = %v, want a closed connection within the idle window", err)
	}
	select {
	case <-originClosed:
	case <-time.After(3 * time.Second):
		t.Error("idle tunnel origin conn never closed")
	}
}

func TestGateConnectActiveTunnelSurvivesIdleWindow(t *testing.T) {
	// Activity in ONE direction must keep the tunnel alive past the idle window:
	// a long download is silent upstream-ward while active downstream-ward, so a
	// per-direction read deadline would wrongly cut it. The origin streams bytes
	// for twice the idle window, then closes; the client must receive them all.
	const (
		idle     = 200 * time.Millisecond
		interval = 25 * time.Millisecond
		chunks   = 16 // 16×25ms = 400ms of one-directional traffic > 2× idle
	)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		for range chunks {
			if _, err := io.WriteString(c, "x"); err != nil {
				return
			}
			time.Sleep(interval)
		}
		_, _ = io.WriteString(c, "END")
	}()
	host, _, _ := net.SplitHostPort(ln.Addr().String())

	g := gate.New(gate.Config{
		Networking:        domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{host}},
		TunnelIdleTimeout: idle,
	})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()

	conn, br := dialCONNECT(t, gsrv.URL, ln.Addr().String())
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("reading tunneled stream: %v (idle deadline cut an active tunnel?)", err)
	}
	want := strings.Repeat("x", chunks) + "END"
	if string(got) != want {
		t.Errorf("tunneled stream = %q, want %q", got, want)
	}
}

func TestGatePlainHTTPDoesNotDecompressResponse(t *testing.T) {
	// A transparent proxy must not auto-decompress: the origin's gzipped body and
	// its Content-Encoding must reach the sandbox unaltered, even when the sandbox
	// sent no Accept-Encoding — which would otherwise make Go's transport request
	// gzip and silently decode the response.
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write([]byte("compressed-payload")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	compressed := gz.Bytes()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(compressed)
	}))
	defer origin.Close()
	host := hostOf(t, origin.URL)

	g := gate.New(gate.Config{Networking: domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{host}}})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()

	// Disable the *test client's* auto-decompression too, so we observe exactly
	// the bytes the gate forwarded rather than the client's own decoding.
	client := proxyClient(t, gsrv.URL, nil)
	client.Transport.(*http.Transport).DisableCompression = true
	resp, err := client.Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ce := resp.Header.Get("Content-Encoding"); ce != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip preserved (the gate must not auto-decompress)", ce)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, compressed) {
		t.Errorf("forwarded body was altered in transit; want the original gzipped bytes")
	}
}

func TestGatePlainHTTPBadGatewayWhenOriginStalls(t *testing.T) {
	// An origin that accepts the connection but never sends response headers must
	// not pin the serving goroutine: a short ResponseHeaderTimeout bounds the wait
	// and the gate answers 502.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c) // hold open, never respond
			mu.Unlock()
		}
	}()
	defer func() {
		mu.Lock()
		for _, c := range held {
			_ = c.Close()
		}
		mu.Unlock()
	}()
	host, _, _ := net.SplitHostPort(ln.Addr().String())

	g := gate.New(gate.Config{
		Networking: domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{host}},
		Transport:  &http.Transport{ResponseHeaderTimeout: 200 * time.Millisecond},
	})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()

	resp, err := proxyClient(t, gsrv.URL, nil).Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 when the origin stalls past ResponseHeaderTimeout", resp.StatusCode)
	}
}

func TestGatePlainHTTPBadGatewayWhenBodyReadFails(t *testing.T) {
	origin := echoOrigin(t)
	defer origin.Close()
	u, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}

	g := gate.New(gate.Config{Networking: domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{u.Hostname()}}})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()

	// Declare more body than we send, then half-close: the gate's body read hits
	// an unexpected EOF and answers 502 — the read-failure arm, distinct from the
	// 413 over-limit arm.
	gu, err := url.Parse(gsrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", gu.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	raw := "POST " + origin.URL + "/ HTTP/1.1\r\nHost: " + u.Host + "\r\nContent-Length: 100\r\n\r\nshort"
	if _, err := io.WriteString(conn, raw); err != nil {
		t.Fatal(err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "HTTP/1.1 502") {
		t.Errorf("status line = %q, want HTTP/1.1 502 when the request body read fails", line)
	}
}
