package gate

import (
	"net/http"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
)

// policy is the environment's request-level networking gate — which hosts a
// sandbox may reach at all, independent of any credential. It is the first of
// the two-level gate; a credential's own allowed_hosts (egress.Engine) is the
// second. "limited" admits the configured allowed_hosts, plus the *endpoints*
// the agent declares MCP servers at when the policy sets allow_mcp_servers;
// "unrestricted" admits every host — the reference's safety blocklist for
// unrestricted is unpublished and its enforcement is deferred to a later sub-PR
// (recorded INFERRED in DIVERGENCES), so this phase does not narrow it. An
// unknown type fails closed (admits nothing).
//
// The two admitting sets are kept apart because they are not equally trusted.
// `allowed_hosts` is an operator's list, entered through a validated grammar in
// which `*.example.com` is a deliberate suffix rule. The MCP set comes from the
// agent spec, so it is matched on host **and port** — the reference widens by
// "MCP server endpoints configured on the agent", and an endpoint is what an
// agent declares — and a request admitted by it alone is held to the platform's
// address floor at the dial (gate.go), which is what the platform's own MCP
// client does with the same declarations.
type policy struct {
	admitAll bool
	allowed  *egress.HostSet
	mcp      map[string]struct{} // "host:port", lowercased
}

// newPolicy builds the policy. mcpEndpoints are the agent's declared MCP
// endpoints as `host:port`, which only `limited` can widen by and only under its
// own flag: an unrecognized type must stay closed to them like everything else,
// and `unrestricted` already admits them along with every other host.
func newPolicy(net domain.Networking, mcpEndpoints []string) *policy {
	switch net.Type {
	case domain.NetUnrestricted:
		return &policy{admitAll: true}
	case domain.NetLimited:
		p := &policy{allowed: egress.NewHostSet(net.AllowedHosts)}
		if net.AllowMCPServers {
			p.mcp = make(map[string]struct{}, len(mcpEndpoints))
			for _, e := range mcpEndpoints {
				if host, port, ok := strings.Cut(strings.TrimSpace(e), ":"); ok {
					p.mcp[normalizeHost(host)+":"+port] = struct{}{}
				}
			}
		}
		return p
	default:
		return &policy{} // fail closed: neither admitAll nor an allow-list
	}
}

// admit reports whether a request to host:port may go out at all, and whether
// the agent's own declarations are the only reason it may — which is what puts
// the dial under the address floor.
func (p *policy) admit(host, port string) (ok, mcpOnly bool) {
	if p.admitAll {
		return true, false
	}
	if p.allowed.Match(host) {
		return true, false
	}
	if _, declared := p.mcp[normalizeHost(host)+":"+port]; declared {
		return true, true
	}
	return false, false
}

// normalizeHost is egress.HostSet's own host normalization, applied to the MCP
// set so the two lists answer a host the same way: `MCP.Example.com.` and
// `mcp.example.com` are one name, and an operator's list already treats them as
// one. The set stays an exact-match map rather than a second HostSet, because a
// HostSet would read a `*.`-prefixed entry as a suffix rule and this list comes
// from the agent spec — mcpEndpoint refuses a wildcard, and a map cannot become
// one however that changes.
func normalizeHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}

// hopByHop are the connection-scoped headers a forwarding proxy must not pass
// on (RFC 7230 §6.1, plus the proxy-specific Proxy-Connection).
var hopByHop = map[string]struct{}{
	"connection": {}, "proxy-connection": {}, "keep-alive": {},
	"proxy-authenticate": {}, "proxy-authorization": {}, "te": {},
	"trailer": {}, "transfer-encoding": {}, "upgrade": {},
}

func isHopByHop(k string) bool {
	_, ok := hopByHop[strings.ToLower(k)]
	return ok
}

// removeHopByHop strips the hop-by-hop headers, including any named in a
// Connection header, from a request or response the proxy is about to forward.
// RFC 7230 §6.1 permits Connection to appear as several field lines, so every
// value is consulted, not just the first.
func removeHopByHop(h http.Header) {
	for _, conn := range h.Values("Connection") {
		for _, name := range strings.Split(conn, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for k := range h {
		if isHopByHop(k) {
			h.Del(k)
		}
	}
}
