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
	// IPAllowed is the address floor a dial admitted only by MCPServerEndpoints
	// is held to, run on the resolved address. Nil selects dialguard.IPAllowed,
	// which is what the platform's own MCP client uses on the same declarations;
	// a test overrides it to reach a loopback server.
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

// mcpGuardKey marks a request whose destination only the agent's own MCP
// declarations admitted. It rides the context because the address floor has to
// run on the *resolved* address, which is the dialer's business and not the
// handler's — a name that resolves into a refused class, or one that resolves
// differently on the second lookup, is exactly what a pre-dial check misses.
type mcpGuardKey struct{}

// guardTheDial marks ctx so the dialer under it holds the connection to the
// address floor.
func guardTheDial(ctx context.Context) context.Context {
	return context.WithValue(ctx, mcpGuardKey{}, struct{}{})
}

// newDialer is the one dialer the gate opens every socket through — both
// handlers, and the transport under handlePlain.
//
// The floor runs only for a dial the agent's own declarations admitted:
// `allowed_hosts` is an operator's list and this proxy is the operator's own
// egress, so narrowing that half would be a plan 12 decision rather than this
// one. ControlContext rather than Control, because the marker is what tells the
// two apart and only the context carries it; Go calls it once per candidate
// address, so a dual-stack or multi-A name is judged on every address it is
// actually about to connect to.
func newDialer(ipAllowed func(net.IP) error) *net.Dialer {
	floor := dialguard.Control(ipAllowed)
	return &net.Dialer{
		Timeout: dialTimeout,
		ControlContext: func(ctx context.Context, network, address string, c syscall.RawConn) error {
			if ctx.Value(mcpGuardKey{}) == nil {
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
	dial := newDialer(ipAllowed).DialContext
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
	ok, mcpOnly := g.policy.admit(host, port)
	if !ok {
		http.Error(w, "host not permitted by the environment's networking policy", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	if mcpOnly {
		ctx = guardTheDial(ctx)
	}
	upstream, err := g.dial(ctx, "tcp", target)
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
	ok, mcpOnly := g.policy.admit(host, port)
	if !ok {
		http.Error(w, "host not permitted by the environment's networking policy", http.StatusForbidden)
		return
	}

	ctx := r.Context()
	if mcpOnly {
		ctx = guardTheDial(ctx)
	}
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
