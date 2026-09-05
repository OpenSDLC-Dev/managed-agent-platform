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
// second. "limited" admits the configured allowed_hosts, plus — each under its
// own flag — the *endpoints* the agent declares MCP servers at
// (allow_mcp_servers) and the curated package registries
// (allow_package_managers); "unrestricted" admits every host — the reference's
// safety blocklist for unrestricted is unpublished and its enforcement is
// deferred to a later sub-PR (recorded INFERRED in DIVERGENCES), so this phase
// does not narrow it. An unknown type fails closed (admits nothing).
//
// The three admitting sets are kept apart because they are not equally trusted.
// `allowed_hosts` is an operator's list, entered through a validated grammar in
// which `*.example.com` is a deliberate suffix rule. The MCP set comes from the
// agent spec, so it is matched on host **and port** — the reference widens by
// "MCP server endpoints configured on the agent", and an endpoint is what an
// agent declares. The package-registry set is neither author's: it is this
// platform's own (egress.PackageRegistryHosts), matched by host like an
// operator's entry rather than by endpoint. That contrast is the reference's
// own wording rather than an inference from the probes' silence about ports:
// beside the endpoint sentence above, it widens by "public package registries …
// beyond those listed in the `allowed_hosts` array", and that array's own unit
// is the domain ("Specifies domains the container can reach").
//
// A request admitted by either flag alone is held to the platform's address
// floor at the dial (gate.go); one admitted by `allowed_hosts` is not. The
// dividing line is not who typed the hostname but whether an operator did:
// these two sets widen an operator's own egress by a rule the operator did not
// write, so the name they admit is one nobody here vouched for, and a poisoned
// or rebound answer for it must not reach a link-local or loopback address.
type policy struct {
	admitAll bool
	allowed  *egress.HostSet
	mcp      map[string]struct{} // endpointKey values
	packages *egress.HostSet     // nil unless allow_package_managers is set
}

// packageRegistries is the curated set, built once and shared by every policy
// that opens it — it is immutable after construction and only ever matched
// against.
//
// It is a package-level variable rather than a call inside newPolicy so that an
// internal test can point it at an origin a test can actually serve: the real
// set names public registries, which a proxy test can neither resolve nor dial,
// and the arm worth proving end to end is the one where the request goes
// through. The seam exists only under `go test` (helpers_internal_test.go);
// production has no way to reach it, which is the same reason the gate offers
// no seam for its dialer.
var packageRegistries = egress.NewHostSet(egress.PackageRegistryHosts())

// newPolicy builds the policy. mcpEndpoints are the agent's declared MCP
// endpoints as `host:port`. Both widenings are `limited`'s alone and each is
// its own flag's: an unrecognized type must stay closed to them like everything
// else, and `unrestricted` already admits them along with every other host.
//
// The package-registry set is opened by the flag alone, never by what
// `config.packages` lists — the recorded environment that opened `pypi.org`
// declared no packages at all (its stored block was the six empty lists the
// reference synthesizes), so the flag is the whole condition.
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
					p.mcp[endpointKey(host, port)] = struct{}{}
				}
			}
		}
		if net.AllowPackageManagers {
			p.packages = packageRegistries
		}
		return p
	default:
		return &policy{} // fail closed: neither admitAll nor an allow-list
	}
}

// admit reports whether a request to host:port may go out at all, and whether a
// widening flag is the only reason it may — which is what puts the dial under
// the address floor.
//
// The operator's own list is consulted first, so a registry or endpoint an
// operator also listed is dialled as the operator's host, unfloored: naming it
// in allowed_hosts is the vouching the floor stands in for.
func (p *policy) admit(host, port string) (ok, widenedOnly bool) {
	if p.admitAll {
		return true, false
	}
	if p.allowed.Match(host) {
		return true, false
	}
	if _, declared := p.mcp[endpointKey(host, port)]; declared {
		return true, true
	}
	if p.packages.Match(host) {
		return true, true
	}
	return false, false
}

// endpointKey is how one host and port are spelled in the MCP set, on the way in
// and on the way out, so a declaration and the request that uses it need not be
// spelled identically.
//
// The host goes through egress.NormalizeHost — the very function an operator's
// allowed_hosts are matched by, not a copy of it — so the two lists cannot drift
// apart on what makes two names one. The port drops leading zeros because that
// is what the connection will do: a URL may carry `:0443`, and Go's dialer
// resolves it to 443, so keying on the digits as written would refuse a request
// on the strength of a spelling that changes nothing about where it goes.
//
// The set stays an exact-match map rather than a second HostSet: a HostSet reads
// a `*.`-prefixed entry as a suffix rule, and this list comes from the agent
// spec. mcpEndpoint refuses a wildcard today, and a map cannot become one
// however that changes.
func endpointKey(host, port string) string {
	return egress.NormalizeHost(host) + ":" + strings.TrimLeft(port, "0")
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
