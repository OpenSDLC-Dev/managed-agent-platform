package gate

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
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
		if got := p.admit(host, "8443"); got != admitMCP {
			t.Errorf("admit(%q, 8443) = %v, want %v", host, got, admitMCP)
		}
	}
	// The port is canonical on both sides for the same reason: Go's dialer
	// resolves `08443` to 8443, so a request written that way goes exactly where
	// the declaration says.
	if got := p.admit("mcp.example.com", "08443"); got != admitMCP {
		t.Errorf("a leading-zero spelling of the declared port gave %v", got)
	}
	// The port is still exact, and a neighbouring name is still not the name.
	if got := p.admit("mcp.example.com", "22"); got != admitNone {
		t.Errorf("a second port on the declared host gave %v", got)
	}
	if got := p.admit("evil.mcp.example.com", "8443"); got != admitNone {
		t.Errorf("a subdomain of the declared host gave %v", got)
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
// The whole classification is asserted, never just that the host got out: which
// set admitted it is what decides the address floor and the rooted lookup below,
// so a registry admitted as though an operator had listed it would silently lose
// both, with no status code to show it.
func TestThePackageRegistrySetOpensOnlyUnderItsFlag(t *testing.T) {
	limited := func(flag bool) domain.Networking {
		return domain.Networking{Type: domain.NetLimited, AllowPackageManagers: flag}
	}
	for name, tc := range map[string]struct {
		net        domain.Networking
		host, port string
		want       admission
	}{
		"the flag opens the index":             {limited(true), "pypi.org", "443", admitRegistry},
		"and the wheel CDN beside it":          {limited(true), "files.pythonhosted.org", "443", admitRegistry},
		"without the flag the index is shut":   {limited(false), "pypi.org", "443", admitNone},
		"nor is the CDN open without it":       {limited(false), "files.pythonhosted.org", "443", admitNone},
		"a control host is refused either way": {limited(true), "example.com", "443", admitNone},
		// Host-shaped, not endpoint-shaped: the recording's probes say which
		// hosts the flag opens and nothing about a port, and an operator's own
		// entry opens every port on its host. The MCP set is the port-scoped one,
		// because an agent author declares an endpoint.
		"the registry is open on any port": {limited(true), "pypi.org", "8080", admitRegistry},
		// Exact hosts, no suffix rule: `files.pythonhosted.org` is an entry, not
		// evidence that `*.pythonhosted.org` is one.
		"a subdomain of a registry is not the registry": {limited(true), "evil.pypi.org", "443", admitNone},
		"nor is the CDN's parent domain":                {limited(true), "pythonhosted.org", "443", admitNone},
		// The set reaches past package registries, and the two groups that do
		// are classified like the rest — floored and resolved absolutely, not
		// waved through because they are famous.
		"a source forge is the registry class too": {limited(true), "github.com", "443", admitRegistry},
		"and so is a container registry":           {limited(true), "ghcr.io", "443", admitRegistry},
		// A widening flag cannot make an unrecognized policy recognized, and
		// `unrestricted` already admits every host without the floor.
		"an unknown policy stays closed": {
			domain.Networking{Type: "bogus", AllowPackageManagers: true}, "pypi.org", "443", admitNone},
		"unrestricted admits it unfloored": {
			domain.Networking{Type: domain.NetUnrestricted, AllowPackageManagers: true}, "pypi.org", "443", admitUnrestricted},
		// The operator's own list is consulted first, so a registry an operator
		// also listed is dialled as the operator's host — unfloored, because
		// listing it is the vouching the floor stands in for.
		"an operator's own entry wins, and is not floored": {
			domain.Networking{Type: domain.NetLimited, AllowPackageManagers: true,
				AllowedHosts: []string{"pypi.org"}}, "pypi.org", "443", admitOperator},
	} {
		t.Run(name, func(t *testing.T) {
			if got := newPolicy(tc.net, nil).admit(tc.host, tc.port); got != tc.want {
				t.Errorf("admit(%q, %q) = %v, want %v", tc.host, tc.port, got, tc.want)
			}
		})
	}
}

// The golden list in internal/egress compares strings, and the table above spot-
// checks four of the thirty. Neither would notice an entry HostSet can never
// match — an empty label, a stray leading dot — added to the list and to the
// golden literal alike: it would be silently dead, admitting nothing, while both
// tests stayed green and the refusal test never looked at it. So every entry is
// driven through the policy that actually consults it.
func TestEveryPackageRegistryEntryReallyAdmits(t *testing.T) {
	p := newPolicy(domain.Networking{Type: domain.NetLimited, AllowPackageManagers: true}, nil)
	for _, host := range egress.PackageRegistryHosts() {
		if got := p.admit(host, "443"); got != admitRegistry {
			t.Errorf("admit(%q) = %v, want admitRegistry — the entry is in the list but matches nothing", host, got)
		}
	}
}

// Every host below was probed in the recording that sized the set and observed
// gate-refused — most of them one label away from an admitted sibling. They are
// asserted rather than left to the list's own reading because each is exactly
// what a later contributor would add for symmetry: the apex under an admitted
// pair, the test instance beside the real one, the other distribution's mirror,
// the ecosystem whose neighbours are all open. A guessed entry widens a
// `limited` sandbox past the reference, which is the one direction this gate
// must not err in (#594).
func TestThePackageRegistrySetStillRefusesWhatTheRecordingSawRefused(t *testing.T) {
	p := newPolicy(domain.Networking{Type: domain.NetLimited, AllowPackageManagers: true}, nil)
	for _, host := range []string{
		// Apexes and test instances beside an admitted name.
		"test.pypi.org", "test-files.pythonhosted.org", "pypi.python.org",
		"npmjs.org", "www.npmjs.com", "registry.npmjs.com", "yarnpkg.com",
		"golang.org",
		// Siblings on a shared domain: raw. and objects. are open, these are not.
		"gist.githubusercontent.com", "pkg-containers.githubusercontent.com",
		// The alias a client is redirected away from, where the target is open.
		"index.docker.io",
		// apt is Ubuntu's alone, and not all of Ubuntu's.
		"deb.debian.org", "ftp.debian.org", "cdn-aws.deb.debian.org",
		"ports.ubuntu.com", "azure.archive.ubuntu.com", "esm.ubuntu.com",
		"changelogs.ubuntu.com", "keyserver.ubuntu.com",
		// Whole ecosystems whose neighbours are open.
		"api.nuget.org", "pub.dev", "hex.pm", "cdn.cocoapods.org", "deno.land",
		"cran.r-project.org", "metacpan.org", "dl-cdn.alpinelinux.org",
		"conda.anaconda.org", "clojars.org", "search.maven.org", "oss.sonatype.org",
		"cdn.jsdelivr.net", "unpkg.com", "sourceforge.net",
	} {
		if got := p.admit(host, "443"); got != admitNone {
			t.Errorf("admit(%q) = %v, but the recording saw it refused — the flag must fail closed on it (#594)", host, got)
		}
	}
}

// Admission is the step that gates everything after it, which is what made the
// Unicode fold worth fixing before this set shipped rather than after. A request
// to an IDN alias of a curated host normalized onto that host, so the gate
// admitted it — and only an admitted request reaches the substitution engine,
// where a credential scoped to the ASCII name would then be handed to a dial Go
// resolves, via IDNA, to a wholly different and registerable domain. The alias
// must be refused outright (#606).
func TestAnIDNAliasOfACuratedHostIsNotAdmitted(t *testing.T) {
	p := newPolicy(domain.Networking{Type: domain.NetLimited, AllowPackageManagers: true}, nil)
	for _, alias := range []string{
		"g\u0130thub.com", // U+0130 folds to "i" under Unicode, not under DNS
		"pyp\u0130.org",
		"crates.\u0130o",
	} {
		if got := p.admit(alias, "443"); got != admitNone {
			t.Errorf("admit(%q) = %v, want admitNone — it is not the ASCII host it folds to (#606)", alias, got)
		}
	}

	// The ASCII spellings, upper-cased and dotted, still resolve to the entry.
	for _, real := range []string{"GitHub.com", "PyPI.org.", "crates.io"} {
		if got := p.admit(real, "443"); got != admitRegistry {
			t.Errorf("admit(%q) = %v, want admitRegistry — ASCII folding must still work", real, got)
		}
	}
}

// String names the class in a failure message. It lives in the test file because
// only a test ever formats an admission: the gate switches on it, never prints
// it, and a Stringer no production path can reach would be dead weight in
// policy.go.
func (a admission) String() string {
	switch a {
	case admitNone:
		return "admitNone"
	case admitUnrestricted:
		return "admitUnrestricted"
	case admitOperator:
		return "admitOperator"
	case admitMCP:
		return "admitMCP"
	case admitRegistry:
		return "admitRegistry"
	}
	return "admission(" + strconv.Itoa(int(a)) + ")"
}

// floored and rooted are the two questions the dialer asks of one admission, and
// they deliberately do not have the same answer: both widening flags admit a
// name no operator vouched for, so both are floored — but only the registry
// class is a list this platform authors rather than reads, so only it is
// resolved absolutely (#596).
func TestWhatEachAdmissionClassAsksOfTheDial(t *testing.T) {
	for _, tc := range []struct {
		how             admission
		floored, rooted bool
	}{
		// The zero value, which is also what a context no handler marked reads
		// as. It floors: a dial that never passed admit is not one to hand an
		// unfloored socket.
		{admitNone, true, false},
		{admitUnrestricted, false, false},
		{admitOperator, false, false},
		{admitMCP, true, false},
		{admitRegistry, true, true},
	} {
		if got := tc.how.floored(); got != tc.floored {
			t.Errorf("%v.floored() = %v, want %v", tc.how, got, tc.floored)
		}
		if got := tc.how.rooted(); got != tc.rooted {
			t.Errorf("%v.rooted() = %v, want %v", tc.how, got, tc.rooted)
		}
	}
}

// rootedName is applied to a dial address and to nothing else, so every address
// shape that is not a resolvable multi-label DNS name has to come back byte for
// byte — for those a trailing dot can only break the dial, and the gate opens
// every socket through this one wrapper.
func TestRootedNameLeavesEverythingThatIsNotADNSNameAlone(t *testing.T) {
	for _, tc := range []struct{ addr, want string }{
		// The one shape that changes: a multi-label name, which is all the
		// curated set holds.
		{"pypi.org:443", "pypi.org.:443"},
		{"files.pythonhosted.org:80", "files.pythonhosted.org.:80"},
		// A spelling the host set admits (NormalizeHost lowercases) and the
		// resolver answers case-insensitively, so it must root like any other.
		{"PYPI.ORG:443", "PYPI.ORG.:443"},
		// Already absolute — so this is idempotent, and a request the sandbox
		// spelled with the dot itself is not double-rooted.
		{"pypi.org.:443", "pypi.org.:443"},
		// An address literal has no name to resolve: a colon catches the IPv6
		// spellings, including a zone-scoped one net.ParseIP rejects, and ParseIP
		// catches the IPv4 one that a dot would otherwise send through.
		{"127.0.0.1:443", "127.0.0.1:443"},
		{"[::1]:443", "[::1]:443"},
		{"[fe80::1%eth0]:443", "[fe80::1%eth0]:443"},
		// Not host:port at all: the double-bracketed form addrWithPort makes of an
		// already-bracketed host, and an address carrying no port.
		{"[[::1]]:443", "[[::1]]:443"},
		{"no-port", "no-port"},
		// No host at all — caught by the same no-dot rule as a single label.
		{":443", ":443"},
		// A single label has no global identity — the search answer *is* the
		// intended answer — which is why an in-cluster MCP declaration like
		// `http://nexus:8080` must never be rooted.
		{"nexus:8080", "nexus:8080"},
	} {
		if got := rootedName(tc.addr); got != tc.want {
			t.Errorf("rootedName(%q) = %q, want %q", tc.addr, got, tc.want)
		}
	}
}

// errSpyDial is what the spy dialler below answers with; it opens no socket, so
// the rooting is observed on the address it was handed rather than on a
// connection.
var errSpyDial = errors.New("the spy dialler opens no sockets")

// The rooting is asked for by the admission and by nothing else. A dial the
// operator's own list, an MCP declaration or `unrestricted` admitted reaches the
// base dialler with the address the handler built, byte for byte — and so does
// an unmarked context, which is every caller that never went through a handler.
func TestOnlyARegistryDialIsRooted(t *testing.T) {
	var saw string
	dial := rootedDial(func(_ context.Context, _, addr string) (net.Conn, error) {
		saw = addr
		return nil, errSpyDial
	})
	marked := func(a admission) context.Context {
		return withAdmission(context.Background(), a)
	}

	for name, tc := range map[string]struct {
		ctx  context.Context
		want string
	}{
		"a registry dial is rooted":                {marked(admitRegistry), "pypi.org.:443"},
		"an MCP dial is not":                       {marked(admitMCP), "pypi.org:443"},
		"nor is an operator's own host":            {marked(admitOperator), "pypi.org:443"},
		"nor is unrestricted":                      {marked(admitUnrestricted), "pypi.org:443"},
		"and an unmarked context asks for nothing": {context.Background(), "pypi.org:443"},
	} {
		t.Run(name, func(t *testing.T) {
			saw = ""
			if _, err := dial(tc.ctx, "tcp", "pypi.org:443"); !errors.Is(err, errSpyDial) {
				t.Fatalf("dial error = %v, want the spy's", err)
			}
			if saw != tc.want {
				t.Errorf("the base dialler saw %q, want %q", saw, tc.want)
			}
		})
	}
}

// The wiring rung: the gate has two dial paths — handleConnect calls g.dial and
// handlePlain goes through the transport — and the rooting has to be on both.
// One wrapper covers them because they are the same function value, which is
// asserted here as well as exercised: a transport handed the unwrapped dialler
// would still serve every existing test.
//
// The first label is one byte past the 63 a DNS label may hold, so no query
// can encode it: Go's own resolver refuses the name before sending one, and a C
// resolver's compressor fails the same way, which is what keeps this off the
// network and off a build machine's DNS. What comes back is the resolver naming
// what it was asked to look up, and that is the rooted spelling if and only if
// the wrapper ran.
//
// The pointer check beside it identifies the function, not the closure, which
// is all that is needed: an unwrapped net.Dialer.DialContext in either field is
// a different function and shows up here.
func TestBothDialPathsCarryTheRooting(t *testing.T) {
	g := New(Config{})
	if reflect.ValueOf(g.dial).Pointer() !=
		reflect.ValueOf(g.transport.(*http.Transport).DialContext).Pointer() {
		t.Error("the transport dials through a different function than handleConnect does")
	}

	name := strings.Repeat("a", 64) + ".invalid"
	ctx := withAdmission(context.Background(), admitRegistry)
	_, err := g.dial(ctx, "tcp", name+":80")
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) {
		t.Fatalf("dial error = %v (%T), want a *net.DNSError naming what was looked up", err, err)
	}
	if dnsErr.Name != name+"." {
		t.Errorf("the resolver was asked for %q, want %q", dnsErr.Name, name+".")
	}
}

// A host can sit in two sets at once, and the precedence decides which
// treatment it gets. A curated registry an agent also declares an MCP server at
// must stay the registry's: the two classes agree on the address floor and
// differ only on the rooted lookup, so the other order let an agent author turn
// #596's fix off on a host the operator's own flag had opened.
func TestARegistryAnAgentAlsoDeclaresIsStillTheRegistrys(t *testing.T) {
	p := newPolicy(
		domain.Networking{Type: domain.NetLimited, AllowMCPServers: true, AllowPackageManagers: true},
		[]string{"pypi.org:443", "mcp.example.com:443"})

	if got := p.admit("pypi.org", "443"); got != admitRegistry {
		t.Errorf("a declared registry host admitted as %v, want %v", got, admitRegistry)
	}
	// And the precedence takes nothing from the MCP set: a host only it names is
	// still its own.
	if got := p.admit("mcp.example.com", "443"); got != admitMCP {
		t.Errorf("a declared endpoint admitted as %v, want %v", got, admitMCP)
	}
}

// roundTripperFunc lets a test stand in for the gate's transport, which is the
// only way to watch what handlePlain hands its dial: that path never calls
// Gate.dial itself.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Each handler has to mark the context with the class the policy actually
// returned, and the tests above cannot see that: they drive rootedDial with a
// context a test wrote. A handler that marked every request admitMCP would keep
// the address floor, so every floor test would stay green, and would silently
// lose the rooting — so this drives real requests through both handlers and
// reads the class back off the dial each one reaches.
func TestEachHandlerMarksTheAdmissionItFound(t *testing.T) {
	UsePackageRegistries(t, "pypi.org")
	g := New(Config{
		Networking: domain.Networking{
			Type:                 domain.NetLimited,
			AllowedHosts:         []string{"listed.example.com"},
			AllowMCPServers:      true,
			AllowPackageManagers: true,
		},
		MCPServerEndpoints: []string{"mcp.example.com:443"},
	})

	var saw admission
	g.dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		saw = admissionOf(ctx)
		return nil, errSpyDial
	}
	g.transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		saw = admissionOf(r.Context())
		return nil, errSpyDial
	})

	// One port throughout: the MCP set is the port-scoped one, and 443 is what
	// the declaration above names on both paths.
	paths := map[string]struct {
		serve   func(http.ResponseWriter, *http.Request)
		request func(host string) *http.Request
	}{
		"CONNECT": {g.handleConnect, func(h string) *http.Request {
			// handleConnect reads the authority off r.Host, and httptest's CONNECT
			// form wants an authority rather than a URL — so set it outright
			// instead of spelling the request line two ways.
			r := httptest.NewRequest(http.MethodGet, "http://"+h+":443/", nil)
			r.Method, r.Host = http.MethodConnect, h+":443"
			return r
		}},
		"plain": {g.handlePlain, func(h string) *http.Request {
			return httptest.NewRequest(http.MethodGet, "http://"+h+":443/", nil)
		}},
	}
	for name, tc := range map[string]struct {
		host string
		want admission
	}{
		"the operator's own host":    {"listed.example.com", admitOperator},
		"an agent's MCP endpoint":    {"mcp.example.com", admitMCP},
		"a curated package registry": {"pypi.org", admitRegistry},
	} {
		t.Run(name, func(t *testing.T) {
			for path, p := range paths {
				saw = admitNone
				p.serve(httptest.NewRecorder(), p.request(tc.host))
				if saw != tc.want {
					t.Errorf("the %s dial carried %v, want %v", path, saw, tc.want)
				}
			}
		})
	}
}

// The two tests above cover disjoint halves of one chain: the handler test
// replaces both dial paths, so it sees a class and never an address, and
// TestBothDialPathsCarryTheRooting calls Gate.dial with a context a test wrote.
// Nothing yet watches an address leave a *handler* rooted — and the end-to-end
// registry rows in gate_test.go cannot, because an httptest origin is reached
// at 127.0.0.1 and rootedName leaves an address literal alone, so those rows
// pass identically with the rooting deleted.
//
// So this drives both handlers with a curated host that is a multi-label name,
// through the production wrapper around a dialler that opens nothing.
func TestARegistryRequestLeavesAHandlerRooted(t *testing.T) {
	const registry = "registry.example.com"
	UsePackageRegistries(t, registry)
	g := New(Config{Networking: domain.Networking{
		Type:                 domain.NetLimited,
		AllowedHosts:         []string{"listed.example.com"},
		AllowPackageManagers: true,
	}})

	var saw, forwarded string
	spy := rootedDial(func(_ context.Context, _, addr string) (net.Conn, error) {
		saw = addr
		return nil, errSpyDial
	})
	g.dial = spy
	// The production transport, under a recorder: the rooting has to reach the
	// socket without reaching the request, and both halves of that are only
	// visible if the real RoundTrip runs on the way to the real dial.
	plain := &http.Transport{DialContext: spy}
	g.transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		forwarded = r.Host + " " + r.URL.Host
		return plain.RoundTrip(r)
	})

	for name, tc := range map[string]struct{ host, want string }{
		"a curated registry is rooted on the way out": {registry, registry + ".:443"},
		"the operator's own host is not":              {"listed.example.com", "listed.example.com:443"},
	} {
		t.Run(name, func(t *testing.T) {
			connect := httptest.NewRequest(http.MethodGet, "http://"+tc.host+":443/", nil)
			connect.Method, connect.Host = http.MethodConnect, tc.host+":443"
			for path, req := range map[string]*http.Request{
				"CONNECT": connect,
				"plain":   httptest.NewRequest(http.MethodGet, "http://"+tc.host+":443/", nil),
			} {
				saw = ""
				if path == "CONNECT" {
					g.handleConnect(httptest.NewRecorder(), req)
				} else {
					g.handlePlain(httptest.NewRecorder(), req)
				}
				if saw != tc.want {
					t.Errorf("the %s dial reached the socket as %q, want %q", path, saw, tc.want)
				}
				// Nothing the origin sees may move: the rooted spelling belongs to
				// the dial alone, or a virtual-hosted origin answers the wrong site
				// and a certificate stops matching.
				if path == "plain" {
					if want := tc.host + ":443 " + tc.host + ":443"; forwarded != want {
						t.Errorf("the forwarded request carried %q, want %q", forwarded, want)
					}
				}
			}
		})
	}
}
