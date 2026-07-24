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
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
)

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
			DialContext:           dial,
			ForceAttemptHTTP2:     false,
			MaxIdleConns:          32,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		}
	}
	return &Gate{
		policy:        newPolicy(cfg.Networking),
		engine:        egress.NewEngine(cfg.Credentials),
		onUnreachable: cfg.OnUnreachable,
		dial:          dial,
		transport:     transport,
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
	client, _, err := hj.Hijack()
	if err != nil {
		return
	}
	defer client.Close()

	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	// Tunnel until either side closes; the sandbox's own TLS rides inside.
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go cp(upstream, client)
	go cp(client, upstream)
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

	respHeader := w.Header()
	for k, vs := range resp.Header {
		if isHopByHop(k) {
			continue
		}
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
// string for substitution; a streaming pass is a later refinement.
func (g *Gate) substituteBody(host string, r *http.Request, unreachable map[string]struct{}) error {
	if r.Body == nil {
		return nil
	}
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		return err
	}
	out, unr := g.engine.Substitute(host, egress.LocationBody, string(body))
	for _, c := range unr {
		unreachable[c.Placeholder] = struct{}{}
	}
	sub := []byte(out)
	r.Body = io.NopCloser(strings.NewReader(string(sub)))
	r.ContentLength = int64(len(sub))
	r.Header.Set("Content-Length", fmt.Sprintf("%d", len(sub)))
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
