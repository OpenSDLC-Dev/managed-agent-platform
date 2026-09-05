// Package gate is the per-session egress gate: a forward proxy the sandbox
// reaches through HTTP_PROXY / HTTPS_PROXY. It is the enforcement point for the
// two-level gate (docs/plan/12_vaults-credentials.md, D3): the environment's
// networking policy decides which hosts a request may reach at all, and — for
// plain HTTP, where the platform holds the request plaintext — egress
// substitution rewrites vault placeholders into their secrets on admitted
// requests (internal/egress). HTTPS rides through as an opaque CONNECT tunnel,
// admitted or refused on the target host but never inspected, so in-sandbox TLS
// bodies keep their placeholders until the TLS-terminating phase (#166).
//
// The package is transport-only: it holds resolved credentials for the life of
// one session's gate and opens sockets, but reads no store and emits no events.
// A credential the request host is not allowed to use is left as its literal
// placeholder (never the secret) — documented reference behavior, not an error —
// and surfaced through the diagnostic OnUnreachable seam. The wire-visible
// credential_host_unreachable_error is a *configuration conflict* (a
// credential's allowed_hosts vs the environment's networking policy), detected
// and emitted controlplane-side when the gate config is rendered — never here.
package gate

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/dialguard"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
)

// defaultMaxBodyBytes bounds a plain-HTTP request body the gate buffers for
// substitution when Config.MaxBodyBytes is unset.
const defaultMaxBodyBytes = 10 << 20 // 10 MiB

// defaultTunnelIdleTimeout closes a CONNECT tunnel that has moved no bytes in
// either direction for the window when Config.TunnelIdleTimeout is unset. Wide
// enough for the quiet stretches of a long-lived interactive TLS session; an
// abandoned tunnel stops holding its goroutines and sockets once it elapses.
const defaultTunnelIdleTimeout = 5 * time.Minute

// defaultResponseHeaderTimeout bounds a stalled origin's hold on the serving
// goroutine when Config.ResponseHeaderTimeout is unset.
const defaultResponseHeaderTimeout = 60 * time.Second

// dialTimeout bounds the connect phase, which neither TLSHandshakeTimeout nor
// ResponseHeaderTimeout covers. Without it a dial to an address that blackholes
// packets is bounded only by the request context — which the sandbox controls —
// so concurrent stalled dials would hold serving goroutines and sockets for the
// OS connect timeout. It is a constant rather than a Config field because the
// gate owns its dialer outright (see Config).
const dialTimeout = 10 * time.Second

// errBodyTooLarge signals a request body over the substitution size limit; the
// handler maps it to 413 rather than the generic read-failure 502.
var errBodyTooLarge = errors.New("request body exceeds the gate substitution limit")

// Config constructs a Gate. Networking is the environment's request-level
// policy; Credentials are the session's resolved env-var credentials for
// substitution (nil is a valid gate that only host-filters). OnUnreachable, when
// set, is called with the request host and the placeholders of credentials whose
// allowed_hosts did not admit it — never a secret.
//
// There is deliberately no seam for the dialer or the transport. The gate opens
// every socket through one dialer of its own so the address floor below cannot
// be substituted away: a caller who replaced either would have kept IPAllowed —
// which the type would still accept — and silently lost the floor on the path
// they replaced, with a missing refusal as the only symptom.
type Config struct {
	Networking domain.Networking
	// MCPServerEndpoints are the `host:port` endpoints the session's agent
	// declares MCP servers at. They widen a `limited` policy that sets
	// allow_mcp_servers and nothing else — see newPolicy.
	MCPServerEndpoints []string
	// IPAllowed is the address floor a dial admitted only by a widening flag —
	// MCPServerEndpoints, or the package registries Networking opens — is held
	// to, run on the resolved address. Nil selects dialguard.IPAllowed, which is
	// what the platform's own MCP client uses on the same declarations; a test
	// overrides it to reach a loopback server.
	IPAllowed     func(net.IP) error
	Credentials   []egress.Credential
	OnUnreachable func(host string, placeholders []string)
	// ResponseHeaderTimeout bounds a stalled origin's hold on the serving
	// goroutine for a plain-HTTP request. It caps time-to-response-headers only,
	// so a slow-streaming body is unaffected. Zero selects
	// defaultResponseHeaderTimeout.
	ResponseHeaderTimeout time.Duration
	// MaxBodyBytes bounds a plain-HTTP request body the gate buffers for
	// substitution; a larger body is refused with 413 rather than read into
	// memory, since the sandbox controls its size. Zero selects
	// defaultMaxBodyBytes.
	MaxBodyBytes int64
	// TunnelIdleTimeout closes a CONNECT tunnel after no bytes have moved in
	// either direction for the window. Activity on one side alone keeps the
	// tunnel alive — a long download is silent upstream-ward — so this cuts
	// only tunnels with no end-to-end byte movement for the whole window
	// (liveness is measured on successful reads; a tunnel both of whose ends
	// stop draining for the entire window counts as abandoned). Zero selects
	// defaultTunnelIdleTimeout.
	TunnelIdleTimeout time.Duration
}

// Gate is one session's forward proxy. It implements http.Handler: an
// http.Server{Handler: gate} serving on the address injected as the sandbox's
// HTTP(S)_PROXY is the running gate.
type Gate struct {
	policy        *policy
	engine        *egress.Engine
	onUnreachable func(host string, placeholders []string)
	dial          func(ctx context.Context, network, addr string) (net.Conn, error)
	transport     http.RoundTripper
	maxBody       int64
	tunnelIdle    time.Duration
}

// admissionKey carries how the policy admitted a request down to the dialer. It
// rides the context because both things done with it happen below the handler:
// the address floor has to run on the *resolved* address — a name that resolves
// into a refused class, or differently on the second lookup, is exactly what a
// pre-dial check misses — and the name is rooted at the moment it is dialled,
// after the Host header and any TLS ServerName are already fixed from the
// spelling the sandbox sent.
type admissionKey struct{}

// withAdmission marks ctx with how the request was admitted.
func withAdmission(ctx context.Context, a admission) context.Context {
	return context.WithValue(ctx, admissionKey{}, a)
}

// admissionOf reads it back. An unmarked context reads as admitNone, which is
// floored and unrooted: a dial that reached the dialler without passing admit
// is the last one to hand an unfloored socket, and it carries no name anyone
// vouched for to root.
func admissionOf(ctx context.Context) admission {
	a, _ := ctx.Value(admissionKey{}).(admission)
	return a
}

// rootedName appends the trailing dot that makes a resolver answer a name
// absolutely, skipping the `search` list and the `ndots` rule it would otherwise
// try first (#596). It is applied to the dial address and nowhere else, so
// nothing the origin sees changes.
//
// Everything that is not a resolvable multi-label DNS name is returned
// untouched, because for those a trailing dot can only break:
//   - what net.SplitHostPort refuses is left as it came. That is not a
//     hypothetical branch: addrWithPort double-brackets an already-bracketed
//     host, so a CONNECT to `[::1]` hands this `[[::1]]:443`, which
//     SplitHostPort rejects and which the dial will fail on for its own
//     reasons. Rooting must not be what turns that into a different failure.
//   - an address literal has no name to resolve. A colon is the whole test for
//     the IPv6 spellings — SplitHostPort has taken the brackets off by here, and
//     a bracket cannot survive it — and it catches a zone-scoped `fe80::1%eth0`,
//     which net.ParseIP rejects. What is left carrying a dot and no colon is
//     IPv4, which ParseIP does answer for.
//   - a name with no dot is left alone, which covers both a single label —
//     where one resolves at all the search answer *is* the intended answer, so
//     rooting it resolves nothing — and the empty host of an address like
//     `:443`, which needs no test of its own.
//   - a name already rooted is left alone, so this is idempotent.
func rootedName(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if strings.HasSuffix(host, ".") || strings.Contains(host, ":") {
		return addr
	}
	if !strings.Contains(host, ".") || net.ParseIP(host) != nil {
		return addr
	}
	return net.JoinHostPort(host+".", port)
}

// rootedDial wraps the gate's one dialer so a dial the registry set alone
// admitted resolves its name absolutely. It wraps the dialer rather than living
// in the handlers because the transport under handlePlain dials on its own —
// one wrapper is what covers both paths.
func rootedDial(base func(ctx context.Context, network, addr string) (net.Conn, error),
) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if admissionOf(ctx).rooted() {
			addr = rootedName(addr)
		}
		return base(ctx, network, addr)
	}
}

// newDialer is the one dialer the gate opens every socket through — both
// handlers, and the transport under handlePlain.
//
// The floor runs only for a dial a widening flag admitted: `allowed_hosts` is an
// operator's list and this proxy is the operator's own egress, so narrowing that
// half would be a plan 12 decision rather than this one. ControlContext rather
// than Control, because the marker is what tells the two apart and only the
// context carries it; Go calls it once per candidate address, so a dual-stack or
// multi-A name is judged on every address it is actually about to connect to.
func newDialer(ipAllowed func(net.IP) error) *net.Dialer {
	floor := dialguard.Control(ipAllowed)
	return &net.Dialer{
		Timeout: dialTimeout,
		ControlContext: func(ctx context.Context, network, address string, c syscall.RawConn) error {
			if !admissionOf(ctx).floored() {
				return nil
			}
			return floor(network, address, c)
		},
	}
}

// New builds a Gate from cfg.
func New(cfg Config) *Gate {
	ipAllowed := cfg.IPAllowed
	if ipAllowed == nil {
		ipAllowed = dialguard.IPAllowed
	}
	dial := rootedDial(newDialer(ipAllowed).DialContext)
	headerTimeout := cfg.ResponseHeaderTimeout
	if headerTimeout <= 0 {
		headerTimeout = defaultResponseHeaderTimeout
	}
	transport := &http.Transport{
		DialContext:       dial,
		ForceAttemptHTTP2: false,
		MaxIdleConns:      32,
		IdleConnTimeout:   90 * time.Second,
		// A transparent proxy must not inject Accept-Encoding or auto-decompress:
		// the sandbox controls its own content negotiation, and the origin's
		// Content-Encoding/Content-Length must reach it unaltered.
		DisableCompression:    true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: headerTimeout,
		ExpectContinueTimeout: time.Second,
	}
	maxBody := cfg.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = defaultMaxBodyBytes
	}
	tunnelIdle := cfg.TunnelIdleTimeout
	if tunnelIdle <= 0 {
		tunnelIdle = defaultTunnelIdleTimeout
	}
	return &Gate{
		policy:        newPolicy(cfg.Networking, cfg.MCPServerEndpoints),
		engine:        egress.NewEngine(cfg.Credentials),
		onUnreachable: cfg.OnUnreachable,
		dial:          dial,
		transport:     transport,
		maxBody:       maxBody,
		tunnelIdle:    tunnelIdle,
	}
}

// ServeHTTP dispatches a forward-proxy request: CONNECT is an opaque tunnel,
// everything else is a plain-HTTP request to substitute and forward.
func (g *Gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		g.handleConnect(w, r)
		return
	}
	g.handlePlain(w, r)
}

// handleConnect admits or refuses an HTTPS tunnel on its target host, then
// copies bytes opaquely — no substitution, so a placeholder in a TLS body
// reaches the origin literally (the documented #166 gap).
func (g *Gate) handleConnect(w http.ResponseWriter, r *http.Request) {
	target := addrWithPort(r.Host, "443")
	host, port := hostOnly(target), portOnly(target)
	how := g.policy.admit(host, port)
	if how == admitNone {
		http.Error(w, "host not permitted by the environment's networking policy", http.StatusForbidden)
		return
	}
	ctx := withAdmission(r.Context(), how)
	// Dial the name that was admitted, not the one the sandbox typed. Admission
	// compares canonical names, so the two can differ by a whole name rather
	// than by case alone: the Kelvin sign U+212A before ".example" (not the
	// letter K, which the ASCII fold already merged with "k"),
	// "exa\u00admple.com" and "example\u3002com" all canonicalize onto their
	// ASCII spellings. Dialling the raw authority would be fail-closed rather
	// than unsafe — a resolver answers "no such host" for a U-label — but it
	// would make every widening this comparison buys a promise the connection
	// does not keep. A scoped address keeps its zone: CanonicalHost splits at
	// the "%" so an interface name is never case-folded. handlePlain needs no
	// equivalent — net/http canonicalizes the address itself before the gate's
	// dialer sees it.
	//
	// An empty port means the authority did not split into a host and a port,
	// and both shapes that produce one reach here: "example.com:", which
	// SplitHostPort accepts with an empty port, and "[::1]", which addrWithPort
	// double-brackets into "[[::1]]:443" and SplitHostPort then refuses, leaving
	// hostOnly holding the whole string. Rebuilding either would hand the dialer
	// an address more malformed than the one the sandbox wrote, and the dial
	// error and the address floor would both then report that instead. It goes
	// out as written, and fails as it does today.
	dialAddr := target
	if port != "" {
		dialAddr = net.JoinHostPort(egress.CanonicalHost(host), port)
	}
	upstream, err := g.dial(ctx, "tcp", dialAddr)
	if err != nil {
		http.Error(w, "cannot reach host", http.StatusBadGateway)
		return
	}
	defer upstream.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "proxy does not support hijacking", http.StatusInternalServerError)
		return
	}
	client, clientBuf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer client.Close()

	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	// Tunnel until BOTH directions close; the sandbox's own TLS rides inside.
	// The client half reads through the hijacked bufio.Reader so bytes the
	// server already buffered past the CONNECT request (a client may pipeline
	// the TLS ClientHello before reading the 200) are forwarded, not dropped.
	// Each direction's EOF is propagated to the peer as a half-close (CloseWrite)
	// so a client that shuts its write side and awaits the reply is not
	// truncated by the other pump being torn down first.
	//
	// An activity-based idle deadline bounds an abandoned tunnel: when no bytes
	// move in EITHER direction for tunnelIdle, the watchdog closes both conns
	// and the pumps unwind. The deadline is shared across directions — a
	// per-direction cut would sever a long download that is silent
	// upstream-ward — and is owned by this handler invocation, not the Gate:
	// the deployment swaps in a fresh Gate per config fetch while old tunnels
	// keep running on the gate they started with.
	activity := newTunnelActivity()
	stop := make(chan struct{})
	defer close(stop)
	go tunnelWatchdog(g.tunnelIdle, activity, stop, client, upstream)
	done := make(chan struct{}, 2)
	cp := func(dst net.Conn, src io.Reader) {
		_, _ = io.Copy(dst, &activityReader{r: src, activity: activity})
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(upstream, clientBuf)
	go cp(client, upstream)
	<-done
	<-done
}

// tunnelActivity is the last-activity instant shared between the two tunnel
// pumps and the watchdog, kept as a nanosecond offset from a monotonic base so
// the idle computation is immune to wall-clock steps — a forward NTP jump must
// not cut an active tunnel, nor a backward one extend an abandoned tunnel.
type tunnelActivity struct {
	base time.Time    // carries Go's monotonic reading
	last atomic.Int64 // last-activity offset from base, in nanoseconds
}

func newTunnelActivity() *tunnelActivity {
	return &tunnelActivity{base: time.Now()} // offset 0 = active at creation
}

func (a *tunnelActivity) bump() { a.last.Store(int64(time.Since(a.base))) }

func (a *tunnelActivity) quiet() time.Duration {
	return time.Since(a.base) - time.Duration(a.last.Load())
}

// activityReader bumps the shared last-activity instant on every successful
// read; both pumps read through one, so traffic in either direction counts.
type activityReader struct {
	r        io.Reader
	activity *tunnelActivity
}

func (a *activityReader) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if n > 0 {
		a.activity.bump()
	}
	return n, err
}

// tunnelWatchdog closes both tunnel conns once no byte has moved in either
// direction for idle; closing them unblocks both pumps, which then finish the
// handler's join. It re-arms from the last activity rather than polling, and
// exits when the tunnel closes on its own (stop).
func tunnelWatchdog(idle time.Duration, activity *tunnelActivity, stop <-chan struct{}, conns ...net.Conn) {
	timer := time.NewTimer(idle)
	defer timer.Stop()
	for {
		select {
		case <-stop:
			return
		case <-timer.C:
			quiet := activity.quiet()
			if quiet >= idle {
				for _, c := range conns {
					_ = c.Close()
				}
				return
			}
			timer.Reset(idle - quiet)
		}
	}
}

// handlePlain admits or refuses a plain-HTTP request on its host, substitutes
// vault placeholders in the request it forwards, and streams the origin's
// response back.
func (g *Gate) handlePlain(w http.ResponseWriter, r *http.Request) {
	if r.URL == nil || !r.URL.IsAbs() {
		http.Error(w, "not a proxy request", http.StatusBadRequest)
		return
	}
	// The port a plain-HTTP request names is its URL's, defaulted from the
	// scheme the way every client defaults it — an `mcp_servers` declaration
	// carries the same two halves and is normalized the same way.
	target := addrWithPort(r.URL.Host, defaultPort(r.URL.Scheme))
	host, port := hostOnly(target), portOnly(target)
	how := g.policy.admit(host, port)
	if how == admitNone {
		http.Error(w, "host not permitted by the environment's networking policy", http.StatusForbidden)
		return
	}

	ctx := withAdmission(r.Context(), how)
	out := r.Clone(ctx)
	out.RequestURI = "" // must be empty on a client request
	unreachable := map[string]struct{}{}
	g.substituteHeaders(host, out.Header, unreachable)
	if err := g.substituteBody(host, out, unreachable); err != nil {
		if errors.Is(err, errBodyTooLarge) {
			http.Error(w, "request body too large to substitute", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "cannot read request body", http.StatusBadGateway)
		return
	}
	removeHopByHop(out.Header)
	g.report(host, unreachable)

	resp, err := g.transport.RoundTrip(out)
	if err != nil {
		http.Error(w, "cannot reach host", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Strip hop-by-hop response headers — including any named in the response's
	// own Connection header, which the transport leaves intact — before copying
	// the end-to-end headers back to the sandbox.
	removeHopByHop(resp.Header)
	respHeader := w.Header()
	for k, vs := range resp.Header {
		for _, v := range vs {
			respHeader.Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// substituteHeaders rewrites every header value in the header location,
// collecting any host-unreachable credential's placeholder.
func (g *Gate) substituteHeaders(host string, h http.Header, unreachable map[string]struct{}) {
	for k, vs := range h {
		for i, v := range vs {
			out, unr := g.engine.Substitute(host, egress.LocationHeader, v)
			vs[i] = out
			for _, c := range unr {
				unreachable[c.Placeholder] = struct{}{}
			}
		}
		h[k] = vs
	}
}

// substituteBody rewrites the request body in the body location. Phase 1 buffers
// the whole body: the placeholder is a small token and the body must be a single
// string for substitution; a streaming pass is a later refinement. The read is
// bounded by maxBody — the sandbox controls the size, so an oversized body is
// refused (errBodyTooLarge) rather than buffered without limit.
func (g *Gate) substituteBody(host string, r *http.Request, unreachable map[string]struct{}) error {
	if r.Body == nil {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, g.maxBody+1))
	_ = r.Body.Close()
	if err != nil {
		return err
	}
	if int64(len(body)) > g.maxBody {
		return errBodyTooLarge
	}
	out, unr := g.engine.Substitute(host, egress.LocationBody, string(body))
	for _, c := range unr {
		unreachable[c.Placeholder] = struct{}{}
	}
	r.Body = io.NopCloser(strings.NewReader(out))
	r.ContentLength = int64(len(out))
	r.Header.Set("Content-Length", strconv.Itoa(len(out)))
	return nil
}

func (g *Gate) report(host string, unreachable map[string]struct{}) {
	if g.onUnreachable == nil || len(unreachable) == 0 {
		return
	}
	placeholders := make([]string, 0, len(unreachable))
	for p := range unreachable {
		placeholders = append(placeholders, p)
	}
	g.onUnreachable(host, placeholders)
}

// hostOnly strips a :port from a host[:port] and lowercases nothing (the
// HostSet normalizes). It tolerates a bare host.
func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// addrWithPort returns host:port, defaulting the port when host carries none.
func addrWithPort(hostport, defaultPort string) string {
	if _, _, err := net.SplitHostPort(hostport); err == nil {
		return hostport
	}
	return net.JoinHostPort(hostport, defaultPort)
}

// portOnly returns the port of an address that already carries one.
func portOnly(hostport string) string {
	if _, p, err := net.SplitHostPort(hostport); err == nil {
		return p
	}
	return ""
}

// defaultPort is the port a client assumes for a scheme it was given none for.
func defaultPort(scheme string) string {
	if strings.EqualFold(scheme, "https") {
		return "443"
	}
	return "80"
}
