package gate

import (
	"net/http"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/dialguard"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
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

// UsePackageRegistries points the curated allow-set at hosts a test can serve,
// for the duration of the test. The real set names public registries, which a
// proxy test can neither resolve nor dial, and the arm worth driving through the
// proxy is the one where the request goes through — so the seam exists, but only
// under `go test`: this is a _test.go file, and the shipped gate has no way to
// reach it. Install it before gate.New, which is where newPolicy reads the set.
//
// Swapping a package-level variable is safe only while this package's tests run
// sequentially, which today they all do. A test that calls t.Parallel() and one
// that calls this would race, and the loser would be judged against the other's
// set — so give such a test its own policy through newPolicy instead.
func UsePackageRegistries(t *testing.T, hosts ...string) {
	t.Helper()
	saved := packageRegistries
	packageRegistries = egress.NewHostSet(hosts)
	t.Cleanup(func() { packageRegistries = saved })
}

// allow_package_managers opens the curated package-registry set beyond the
// operator's own allowed_hosts. Driven at the policy for the same reason the MCP
// set's normalization is — the set names real public registries, which a proxy
// test can neither resolve nor dial. What the proxy does with the answer is
// TestGateAdmitsAPackageRegistryOnlyUnderItsFlag's.
//
// The second return value is asserted everywhere, not just the first: it is what
// puts the dial under the address floor, and a set that admitted a registry
// without marking it would open a hole no status code shows.
func TestThePackageRegistrySetOpensOnlyUnderItsFlag(t *testing.T) {
	limited := func(flag bool) domain.Networking {
		return domain.Networking{Type: domain.NetLimited, AllowPackageManagers: flag}
	}
	for name, tc := range map[string]struct {
		net                 domain.Networking
		host, port          string
		wantOK, wantWidened bool
	}{
		"the flag opens the index":             {limited(true), "pypi.org", "443", true, true},
		"and the wheel CDN beside it":          {limited(true), "files.pythonhosted.org", "443", true, true},
		"without the flag the index is shut":   {limited(false), "pypi.org", "443", false, false},
		"nor is the CDN open without it":       {limited(false), "files.pythonhosted.org", "443", false, false},
		"a control host is refused either way": {limited(true), "example.com", "443", false, false},
		// Host-shaped, not endpoint-shaped: the recording's probes say which
		// hosts the flag opens and nothing about a port, and an operator's own
		// entry opens every port on its host. The MCP set is the port-scoped one,
		// because an agent author declares an endpoint.
		"the registry is open on any port": {limited(true), "pypi.org", "8080", true, true},
		// Exact hosts, no suffix rule: `files.pythonhosted.org` is an entry, not
		// evidence that `*.pythonhosted.org` is one.
		"a subdomain of a registry is not the registry": {limited(true), "evil.pypi.org", "443", false, false},
		"nor is the CDN's parent domain":                {limited(true), "pythonhosted.org", "443", false, false},
		// A widening flag cannot make an unrecognized policy recognized, and
		// `unrestricted` already admits every host without the floor.
		"an unknown policy stays closed": {
			domain.Networking{Type: "bogus", AllowPackageManagers: true}, "pypi.org", "443", false, false},
		"unrestricted admits it unfloored": {
			domain.Networking{Type: domain.NetUnrestricted, AllowPackageManagers: true}, "pypi.org", "443", true, false},
		// The operator's own list is consulted first, so a registry an operator
		// also listed is dialled as the operator's host — unfloored, because
		// listing it is the vouching the floor stands in for.
		"an operator's own entry wins, and is not floored": {
			domain.Networking{Type: domain.NetLimited, AllowPackageManagers: true,
				AllowedHosts: []string{"pypi.org"}}, "pypi.org", "443", true, false},
	} {
		t.Run(name, func(t *testing.T) {
			ok, widened := newPolicy(tc.net, nil).admit(tc.host, tc.port)
			if ok != tc.wantOK || widened != tc.wantWidened {
				t.Errorf("admit(%q, %q) = (%v, %v), want (%v, %v)",
					tc.host, tc.port, ok, widened, tc.wantOK, tc.wantWidened)
			}
		})
	}
}

// The set is what a recording sized, not what the reference's prose implies. The
// pinned SDK says "public package registries (PyPI, npm, etc.)" and the public
// docs say "such as PyPI and npm", but the only probe of the flag tried three
// URLs — two Python hosts and a control — so every other ecosystem's registry is
// unevidenced and stays shut (#594). A guessed entry would widen a `limited`
// sandbox past the reference, which is the one direction this gate must not err
// in, so the absences are asserted rather than left to the list's own reading.
func TestThePackageRegistrySetAdmitsNoEcosystemNobodyRecorded(t *testing.T) {
	p := newPolicy(domain.Networking{Type: domain.NetLimited, AllowPackageManagers: true}, nil)
	for _, host := range []string{
		"registry.npmjs.org", "registry.yarnpkg.com", // npm
		"crates.io", "index.crates.io", "static.crates.io", // cargo
		"rubygems.org", "index.rubygems.org", // gem
		"proxy.golang.org", "sum.golang.org", // go
		"archive.ubuntu.com", "security.ubuntu.com", "deb.debian.org", // apt
	} {
		if ok, _ := p.admit(host, "443"); ok {
			t.Errorf("admit(%q) = true, but no recording sizes it — the flag must fail closed on it (#594)", host)
		}
	}
}
