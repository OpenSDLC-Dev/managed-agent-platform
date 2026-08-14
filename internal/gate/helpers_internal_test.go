package gate

import (
	"net/http"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/dialguard"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

func TestDefaultTransportConfig(t *testing.T) {
	g := New(Config{})
	tr, ok := g.transport.(*http.Transport)
	if !ok {
		t.Fatalf("default transport is %T, want *http.Transport", g.transport)
	}
	// A transparent forward proxy must not negotiate/undo compression itself, and
	// must bound a stalled origin's time-to-response-headers.
	if !tr.DisableCompression {
		t.Error("default transport must set DisableCompression so responses forward verbatim")
	}
	if tr.ResponseHeaderTimeout != 60*time.Second {
		t.Errorf("default ResponseHeaderTimeout = %v, want 60s", tr.ResponseHeaderTimeout)
	}
}

// The connect phase has a bound of its own. The transport's timeouts start
// after it, and the gate owns its dialer outright, so nothing else can supply
// one: a dial to an address that blackholes packets would otherwise be bounded
// only by the request context, which the sandbox controls.
func TestTheGatesDialerBoundsTheConnectPhase(t *testing.T) {
	d := newDialer(dialguard.IPAllowed)
	if d.Timeout <= 0 {
		t.Errorf("dialer Timeout = %v, want a finite bound on the connect phase", d.Timeout)
	}
	if d.ControlContext == nil {
		t.Error("dialer has no ControlContext, so the address floor never runs")
	}
}

func TestDefaultTunnelIdleTimeout(t *testing.T) {
	g := New(Config{})
	if g.tunnelIdle != defaultTunnelIdleTimeout {
		t.Errorf("default tunnel idle timeout = %v, want %v", g.tunnelIdle, defaultTunnelIdleTimeout)
	}
	// The default must tolerate quiet stretches of a long-lived interactive TLS
	// session; a sub-minute cut would sever them.
	if defaultTunnelIdleTimeout < time.Minute {
		t.Errorf("defaultTunnelIdleTimeout = %v is too aggressive for long-lived TLS", defaultTunnelIdleTimeout)
	}
}

func TestHostOnly(t *testing.T) {
	cases := map[string]string{
		"example.com:443": "example.com",
		"example.com":     "example.com", // no port
		"10.1.2.3:8080":   "10.1.2.3",
	}
	for in, want := range cases {
		if got := hostOnly(in); got != want {
			t.Errorf("hostOnly(%q) = %q, want %q", in, got, want)
		}
	}
}

// The port a plain-HTTP request is admitted on when its absolute-form url
// carries none. handleConnect hard-codes 443, so https here is reachable only
// through an absolute-form `GET https://host/` — which is why it has a test of
// its own rather than riding one of the proxy-client tests.
func TestDefaultPort(t *testing.T) {
	cases := map[string]string{"https": "443", "HTTPS": "443", "http": "80", "": "80"}
	for scheme, want := range cases {
		if got := defaultPort(scheme); got != want {
			t.Errorf("defaultPort(%q) = %q, want %q", scheme, got, want)
		}
	}
}

func TestPortOnly(t *testing.T) {
	cases := map[string]string{
		"example.com:443": "443",
		// A url written `http://h:/` reaches the handler with an empty port that
		// SplitHostPort accepts, so this is a request shape and not an
		// impossible state: it must not read as some default.
		"example.com:": "",
		"example.com":  "",
	}
	for in, want := range cases {
		if got := portOnly(in); got != want {
			t.Errorf("portOnly(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAddrWithPort(t *testing.T) {
	if got := addrWithPort("example.com", "443"); got != "example.com:443" {
		t.Errorf("addrWithPort default port = %q", got)
	}
	if got := addrWithPort("example.com:8443", "443"); got != "example.com:8443" {
		t.Errorf("addrWithPort existing port = %q, want it kept", got)
	}
}

func TestRemoveHopByHop(t *testing.T) {
	h := http.Header{}
	h.Set("Connection", "X-Custom, Keep-Alive")
	h.Set("X-Custom", "drop-me")      // named in Connection → hop-by-hop for this hop
	h.Set("Proxy-Connection", "1")    // always hop-by-hop
	h.Set("Authorization", "keep-me") // end-to-end
	removeHopByHop(h)
	for _, gone := range []string{"Connection", "X-Custom", "Proxy-Connection"} {
		if h.Get(gone) != "" {
			t.Errorf("%s should have been stripped", gone)
		}
	}
	if h.Get("Authorization") != "keep-me" {
		t.Error("Authorization is end-to-end and must survive forwarding")
	}
}

// The MCP set answers a host the way an operator's allowed_hosts does — case and
// a trailing dot are not what tells two names apart — so a declaration and the
// request that uses it need not be spelled identically. Driven at the policy,
// because a proxy test reaches an httptest origin by address and an address has
// neither case nor a trailing dot.
func TestTheMCPSetNormalizesAHostLikeTheOperatorsList(t *testing.T) {
	p := newPolicy(
		domain.Networking{Type: domain.NetLimited, AllowMCPServers: true},
		[]string{"MCP.Example.com.:8443"})

	for _, host := range []string{"mcp.example.com", "MCP.Example.com", "mcp.example.com."} {
		if ok, mcpOnly := p.admit(host, "8443"); !ok || !mcpOnly {
			t.Errorf("admit(%q, 8443) = (%v, %v), want the declaration to admit it", host, ok, mcpOnly)
		}
	}
	// The port is canonical on both sides for the same reason: Go's dialer
	// resolves `08443` to 8443, so a request written that way goes exactly where
	// the declaration says.
	if ok, _ := p.admit("mcp.example.com", "08443"); !ok {
		t.Error("a leading-zero spelling of the declared port was refused")
	}
	// The port is still exact, and a neighbouring name is still not the name.
	if ok, _ := p.admit("mcp.example.com", "22"); ok {
		t.Error("a second port on the declared host was admitted")
	}
	if ok, _ := p.admit("evil.mcp.example.com", "8443"); ok {
		t.Error("a subdomain of the declared host was admitted")
	}
}
