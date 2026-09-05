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

// admit reports which of the policy's sets let a request to host:port out, or
// admitNone if none did. The class is the answer rather than a bare yes because
// what happens below the handler depends on which set it was — the address floor
// and, for one of them, the rooted lookup.
//
// A host can sit in more than one set, so the order is a precedence rule and
// both steps of it are load-bearing. The operator's own list is consulted
// first, so a registry or endpoint an operator also listed is dialled as the
// operator's host, unfloored: putting it in allowed_hosts is the vouching the
// floor stands in for. A `*.` entry counts as that vouching too — an operator
// who opens a family has opened its members, so `*.org` carries `pypi.org` out
// of the registry class and into the operator's own, floor and rooting alike.
// The registry set is then consulted before the MCP set, so a curated host an
// agent *also* declares an MCP server at keeps the rooted lookup. The two
// classes agree on the floor and differ only there, so reading a host as the
// registry's can only harden its dial — while the other order let an agent
// author switch #596's fix off on a grant that is the operator's, by declaring
// an endpoint at a host the operator's own flag had opened.
func (p *policy) admit(host, port string) admission {
	if p.admitAll {
		return admitUnrestricted
	}
	if p.allowed.Match(host) {
		return admitOperator
	}
	if p.packages.Match(host) {
		return admitRegistry
	}
	if _, declared := p.mcp[endpointKey(host, port)]; declared {
		return admitMCP
	}
	return admitNone
}

// admission is which of the policy's sets let a request out. It is one value
// rather than a bool because the gate asks two different questions of one
// lookup, and they do not have the same answer.
type admission uint8

const (
	admitNone         admission = iota // refused
	admitUnrestricted                  // every host, under `unrestricted`
	admitOperator                      // the operator's own allowed_hosts
	admitMCP                           // an endpoint the session's agent declared
	admitRegistry                      // a package registry this platform curates
)

// floored reports whether the dial is held to the platform's address floor.
// Both widening flags admit a name no operator vouched for, so both are.
//
// It is written as the two exemptions rather than as the members so that the
// floor is what a class gets unless someone writes down why it should not. Two
// classes of caller depend on that direction: an admitting set added later is
// floored until its author says otherwise, and so is the zero value — a
// context no handler marked, which is to say a dial that reached the dialler
// without passing admit at all, and therefore the last one to trust with an
// unfloored socket.
func (a admission) floored() bool { return a != admitUnrestricted && a != admitOperator }

// rooted reports whether the gate resolves the name absolutely — appending the
// trailing dot that makes a resolver skip its `search` list (#596).
//
// Only the registry class. The distinction is not fussiness: it is where the
// vulnerability actually lives. `allow_package_managers` admits a *fixed list
// this platform authors*, so a search-domain collision — `pypi.org` answering
// as `pypi.org.svc.cluster.local` on an RFC 1918 address the floor admits by
// design — is the one way that flag can be turned into reach the list never
// granted. Rooting closes it outright, and cannot cost anything: every entry is
// a public multi-label FQDN.
//
// The other two classes are left resolving as they always did. For
// `allow_mcp_servers` that is a narrowing rather than a clean answer, and it is
// #601's to settle. The host it admits is one an agent author *names*, so a
// search answer wins that author nothing they could not have by naming the
// internal host outright — but a *credential* is a different matter, and that
// is the part the narrowing leaves open. Two paths carry one, by two different
// rules, and both match on a **name** while the socket goes to an address: the
// gate substitutes on plain HTTP when the request host matches a credential's
// own allowed_hosts (internal/egress; a CONNECT tunnel is opaque, so HTTPS
// keeps its placeholder), and the executor dials the declared URL itself under
// a bearer matched by the credential's mcp_server_url
// (internal/vaultresolve). Either way the secret is chosen for the declared
// host and then delivered wherever the search list sends the dial — and
// declaring the internal host outright would not have obtained it, because
// neither matching rule would have matched. What it can also cross is an
// operator who reads a declaration's public-looking spelling as where the
// session will connect.
//
// The reason to leave it here rather than root it anyway is that rooting breaks
// a configuration this platform permits — not the single-label declaration, which
// rootedName leaves alone whatever the class, but the *relative* multi-label
// name only a resolver's search list completes, such as the Kubernetes
// `<service>.<namespace>` spelling `nexus.infra:8080` that
// egress.ValidateHostEntry accepts like any other entry. Telling that apart
// from a public name is what `ndots` exists to guess at, so it is a policy
// decision and not a predicate.
//
// And `allowed_hosts` is the operator's own list resolving through the
// operator's own resolver: second-guessing it narrows reach, buys no floor, and
// would be a plan 12 policy decision rather than this fix.
func (a admission) rooted() bool { return a == admitRegistry }

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
