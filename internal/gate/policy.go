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
// second. "limited" admits the configured allowed_hosts, plus the hosts the
// agent declares MCP servers at when the policy sets allow_mcp_servers;
// "unrestricted" admits every host — the reference's safety blocklist for
// unrestricted is unpublished and its enforcement is deferred to a later sub-PR
// (recorded INFERRED in DIVERGENCES), so this phase does not narrow it. An
// unknown type fails closed (admits nothing).
type policy struct {
	admitAll bool
	allowed  *egress.HostSet
}

// newPolicy builds the policy. mcpHosts are the agent's declared MCP server
// hosts, which only `limited` can widen by and only under its own flag: an
// unrecognized type must stay closed to them like everything else, and
// `unrestricted` already admits them along with every other host.
func newPolicy(net domain.Networking, mcpHosts []string) *policy {
	switch net.Type {
	case domain.NetUnrestricted:
		return &policy{admitAll: true}
	case domain.NetLimited:
		allowed := net.AllowedHosts
		if net.AllowMCPServers {
			// A fresh slice: appending onto the config's own array would write
			// past it into whatever else the decode put there.
			allowed = append(append(make([]string, 0, len(allowed)+len(mcpHosts)), allowed...), mcpHosts...)
		}
		return &policy{allowed: egress.NewHostSet(allowed)}
	default:
		return &policy{} // fail closed: neither admitAll nor an allow-list
	}
}

func (p *policy) admit(host string) bool {
	if p.admitAll {
		return true
	}
	return p.allowed.Match(host)
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
