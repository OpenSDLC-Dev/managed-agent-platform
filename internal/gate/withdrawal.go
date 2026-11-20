package gate

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
)

// errPolicyWithdrawn signals that a destination host or credential was
// withdrawn by a live Gate.Update before the outbound connection opened;
// refusedOrUnreachable maps it to 403 Forbidden.
var errPolicyWithdrawn = errors.New("host not permitted by the updated networking policy")

// snapshot holds one immutable generation of the gate's networking policy and
// credential substitution engine (plan 70 slice 3).
type snapshot struct {
	policy *policy
	engine *egress.Engine
}

func newSnapshot(cfg Config) *snapshot {
	creds := append([]egress.Credential(nil), cfg.Credentials...)
	return &snapshot{
		policy: newPolicy(cfg.Networking, cfg.MCPServerEndpoints),
		engine: egress.NewEngine(creds),
	}
}

func (s *snapshot) allowsUsed(host string, used []egress.UsedCredential) bool {
	for _, u := range used {
		if !s.engine.StillPermitted(host, u) {
			return false
		}
	}
	return true
}

// dialTargetKey carries the request's policy-evaluated host and port down to
// the gate's dialer so every outbound TCP dial re-evaluates the current
// snapshot immediately before opening a socket.
type dialTargetKey struct{}

type dialTarget struct {
	host      string
	port      string
	recordIP  func(net.IP)
	ipAllowed func(net.IP) error
}

func withDialTarget(ctx context.Context, dt dialTarget) context.Context {
	return context.WithValue(ctx, dialTargetKey{}, dt)
}

func dialTargetFrom(ctx context.Context) (dialTarget, bool) {
	dt, ok := ctx.Value(dialTargetKey{}).(dialTarget)
	return dt, ok
}

// activeTunnel tracks one live CONNECT tunnel so Gate.Update can immediately
// sever tunnels whose destination host is withdrawn or whose connected IP
// violates a newly-required address floor.
type activeTunnel struct {
	host      string
	port      string
	remoteIP  net.IP
	upstream  net.Conn
	client    net.Conn // populated under activeMu after Hijack
	withdrawn bool
}

// activeRequest tracks one live plain-HTTP request from outbound admission
// through response completion so Gate.Update can cancel in-flight requests
// whose destination host, address floor, or substituted credentials are
// withdrawn.
type activeRequest struct {
	host      string
	port      string
	used      []egress.UsedCredential
	cancel    context.CancelFunc
	ips       []net.IP // protected by Gate.activeMu
	withdrawn atomic.Bool
}

// Update atomically installs cfg's Networking, MCPServerEndpoints, and
// Credentials as the gate's live policy, drops idle pooled upstream sockets,
// and immediately terminates any active CONNECT tunnel or plain-HTTP request
// whose destination host, resolved address floor, or substituted credentials
// are no longer permitted. Active work that remains permitted continues
// uninterrupted (plan 70 slice 3).
func (g *Gate) Update(cfg Config) {
	next := newSnapshot(cfg)

	var closeConns []net.Conn
	var cancels []context.CancelFunc

	g.activeMu.Lock()
	g.snap.Store(next)

	for t := range g.tunnels {
		how := next.policy.admit(t.host, t.port)
		if how == admitNone || (how.floored() && t.remoteIP != nil && g.ipAllowed(t.remoteIP) != nil) {
			t.withdrawn = true
			if t.upstream != nil {
				closeConns = append(closeConns, t.upstream)
			}
			if t.client != nil {
				closeConns = append(closeConns, t.client)
			}
		}
	}

	for r := range g.requests {
		how := next.policy.admit(r.host, r.port)
		disallowed := how == admitNone || !next.allowsUsed(r.host, r.used)
		if !disallowed && how.floored() {
			for _, ip := range r.ips {
				if ip != nil && g.ipAllowed(ip) != nil {
					disallowed = true
					break
				}
			}
		}
		if disallowed {
			r.withdrawn.Store(true)
			cancels = append(cancels, r.cancel)
		}
	}
	g.activeMu.Unlock()

	if tr, ok := g.transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
	for _, c := range closeConns {
		_ = c.Close()
	}
	for _, cancel := range cancels {
		cancel()
	}
}

func (g *Gate) registerTunnel(host, port string, remoteIP net.IP, upstream net.Conn) (*activeTunnel, bool) {
	g.activeMu.Lock()
	defer g.activeMu.Unlock()
	how := g.snap.Load().policy.admit(host, port)
	if how == admitNone || (how.floored() && remoteIP != nil && g.ipAllowed(remoteIP) != nil) {
		return nil, false
	}
	t := &activeTunnel{
		host:     host,
		port:     port,
		remoteIP: remoteIP,
		upstream: upstream,
	}
	g.tunnels[t] = struct{}{}
	return t, true
}

func (g *Gate) attachTunnelClient(t *activeTunnel, client net.Conn) bool {
	g.activeMu.Lock()
	defer g.activeMu.Unlock()
	if t.withdrawn {
		return false
	}
	t.client = client
	return true
}

func (g *Gate) unregisterTunnel(t *activeTunnel) {
	g.activeMu.Lock()
	delete(g.tunnels, t)
	g.activeMu.Unlock()
}

func (g *Gate) registerRequest(host, port string, used []egress.UsedCredential, cancel context.CancelFunc) (*activeRequest, admission, bool) {
	g.activeMu.Lock()
	defer g.activeMu.Unlock()
	cur := g.snap.Load()
	how := cur.policy.admit(host, port)
	if how == admitNone || !cur.allowsUsed(host, used) {
		return nil, admitNone, false
	}
	ar := &activeRequest{
		host:   host,
		port:   port,
		used:   used,
		cancel: cancel,
	}
	g.requests[ar] = struct{}{}
	return ar, how, true
}

func (g *Gate) recordRequestIP(ar *activeRequest, ip net.IP) {
	if ip == nil {
		return
	}
	g.activeMu.Lock()
	defer g.activeMu.Unlock()
	ar.ips = append(ar.ips, ip)
	if _, live := g.requests[ar]; !live {
		return
	}
	how := g.snap.Load().policy.admit(ar.host, ar.port)
	if how == admitNone || (how.floored() && g.ipAllowed(ip) != nil) {
		ar.withdrawn.Store(true)
		ar.cancel()
	}
}

func (g *Gate) unregisterRequest(ar *activeRequest) {
	g.activeMu.Lock()
	delete(g.requests, ar)
	g.activeMu.Unlock()
}

func remoteIPOf(conn net.Conn) net.IP {
	if conn == nil {
		return nil
	}
	if ta, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		return ta.IP
	}
	return nil
}
