package egress

import (
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// MCPServerAdmitted reports whether the environment's networking policy admits
// an MCP server the agent declares at host. Two places ask, and must never
// disagree: session create, where the reference asks — a refusal there is its
// recorded 400 (#571) — and the executor before every dial, since mcp_servers
// is mid-session-mutable and a host can arrive after create.
//
// What decides is the policy's type, not which kind of environment carries it
// — and type rather than presence, because a value struct cannot tell an
// absent networking block from one that names no type: both decode to an
// empty type, and both read here as no policy. A self_hosted environment has no networking block by
// construction — its config normalizes to exactly {"type":"self_hosted"}, the
// REST surface rejecting every other field, and the reference documents
// networking on cloud environments only — so with no block there is nothing to
// apply and the server is admitted, as the reference admits it at create. A
// cloud environment always has one through the API. Either way a block that
// *is* present is a policy somebody meant, and one naming no recognized type is
// malformed rather than permissive: it admits nothing, which is
// `gate.newPolicy`'s shape and the safe direction.
//
// Reading a missing discriminator as "unrestricted" would make the malformed
// row the one that reaches everything: the schema constrains `config->>'type'`
// against the kind and nothing constrains `config->'networking'`, and a
// config-preserving update (packages, description) carries a stored value
// forward without revalidating it. Deciding on the kind first would do the same
// thing one arm at a time — a `self_hosted` row is exactly the one the API
// cannot have written a block onto, so a block found there came from an import,
// a restore or a hand-written UPDATE, and skipping it because of the kind
// would leave the refusal reachable on cloud rows alone.
//
// allow_mcp_servers "allows access to MCP server endpoints configured on the
// agent" on top of allowed_hosts, and this is only ever asked about a server the
// agent declared — both callers walk that array and nothing else — so under
// the flag the host is admitted without a second list to check it against.
func MCPServerAdmitted(cfg domain.EnvironmentConfig, host string) bool {
	if cfg.Type == domain.EnvSelfHosted && cfg.Networking.Type == "" {
		return true
	}
	switch cfg.Networking.Type {
	case domain.NetUnrestricted:
		return true
	case domain.NetLimited:
		if cfg.Networking.AllowMCPServers {
			return true
		}
		return NewHostSet(cfg.Networking.AllowedHosts).Match(host)
	default:
		return false
	}
}
