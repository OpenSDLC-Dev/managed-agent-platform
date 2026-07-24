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
// placeholder (never the secret) and surfaced through the OnUnreachable seam,
// which the deployment wiring turns into a credential_host_unreachable_error.
package gate

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
)

// defaultMaxBodyBytes bounds a plain-HTTP request body the gate buffers for
// substitution when Config.MaxBodyBytes is unset.
const defaultMaxBodyBytes = 10 << 20 // 10 MiB

// errBodyTooLarge signals a request body over the substitution size limit; the
// handler maps it to 413 rather than the generic read-failure 502.
var errBodyTooLarge = errors.New("request body exceeds the gate substitution limit")

// Config constructs a Gate. Networking is the environment's request-level
// policy; Credentials are the session's resolved env-var credentials for
// substitution (nil is a valid gate that only host-filters). OnUnreachable, when
// set, is called with the request host and the placeholders of credentials whose
// allowed_hosts did not admit it — never a secret. Dial and Transport default to
// a direct network dialer and transport; tests override them.
type Config struct {
	Networking    domain.Networking
	Credentials   []egress.Credential
	OnUnreachable func(host string, placeholders []string)
	// Dial reaches an origin for a CONNECT tunnel; Transport forwards a plain
	// HTTP request. Both default to direct, non-proxied network access.
	Dial      func(ctx context.Context, network, addr string) (net.Conn, error)
	Transport http.RoundTripper
	// MaxBodyBytes bounds a plain-HTTP request body the gate buffers for
	// substitution; a larger body is refused with 413 rather than read into
	// memory, since the sandbox controls its size. Zero selects
	// defaultMaxBodyBytes.
	MaxBodyBytes int64
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
}

// New builds a Gate from cfg.
func New(cfg Config) *Gate {
	dial := cfg.Dial
	if dial == nil {
		var d net.Dialer
		dial = d.DialContext
	}
	transport := cfg.Transport
	if transport == nil {
		transport = &http.Transport{
			DialContext:       dial,
			ForceAttemptHTTP2: false,
			MaxIdleConns:      32,
			IdleConnTimeout:   90 * time.Second,
			// A transparent proxy must not inject Accept-Encoding or auto-decompress:
			// the sandbox controls its own content negotiation, and the origin's
			// Content-Encoding/Content-Length must reach it unaltered.
			DisableCompression:  true,
			TLSHandshakeTimeout: 10 * time.Second,
			// Bound a stalled origin's hold on the serving goroutine — this caps
			// time-to-response-headers only, so a slow-streaming body is unaffected.
			// The deployment wiring (4c-2b) can override the whole transport.
			ResponseHeaderTimeout: 60 * time.Second,
			ExpectContinueTimeout: time.Second,
		}
	}
	maxBody := cfg.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = defaultMaxBodyBytes
	}
	return &Gate{
		policy:        newPolicy(cfg.Networking),
		engine:        egress.NewEngine(cfg.Credentials),
		onUnreachable: cfg.OnUnreachable,
		dial:          dial,
		transport:     transport,
		maxBody:       maxBody,
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
	host := hostOnly(r.Host)
	if !g.policy.admit(host) {
		http.Error(w, "host not permitted by the environment's networking policy", http.StatusForbidden)
		return
	}
	upstream, err := g.dial(r.Context(), "tcp", addrWithPort(r.Host, "443"))
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
	// Phase 1 sets no idle/overall tunnel deadline: an idle tunnel holds its two
	// goroutines and sockets until a peer closes. The blast radius is one
	// session's own disposable gate (self-inflicted), so an activity-based idle
	// timeout is deferred to the cmd/gate server wiring (4c-2b) rather than
	// hard-coded here, where a too-short cut would break long-lived TLS.
	done := make(chan struct{}, 2)
	cp := func(dst net.Conn, src io.Reader) {
		_, _ = io.Copy(dst, src)
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

// handlePlain admits or refuses a plain-HTTP request on its host, substitutes
// vault placeholders in the request it forwards, and streams the origin's
// response back.
func (g *Gate) handlePlain(w http.ResponseWriter, r *http.Request) {
	if r.URL == nil || !r.URL.IsAbs() {
		http.Error(w, "not a proxy request", http.StatusBadRequest)
		return
	}
	host := hostOnly(r.URL.Host)
	if !g.policy.admit(host) {
		http.Error(w, "host not permitted by the environment's networking policy", http.StatusForbidden)
		return
	}

	out := r.Clone(r.Context())
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
