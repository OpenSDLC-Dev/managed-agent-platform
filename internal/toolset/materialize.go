package toolset

import (
	"encoding/json"
	"fmt"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// ValidateMCPToolset checks an mcp_toolset entry's shape the way Validate
// checks an agent_toolset_20260401 one: unknown keys anywhere in the
// default_config / configs / permission_policy nest, and permission policy
// types this platform cannot evaluate, are rejected at the API boundary rather
// than stored. Without it a misspelled `permission_polciy` would be dropped by
// encoding/json and the tool would silently resolve to the toolset default —
// the fail-open at the confirmation boundary that #26 closed for the other
// kind. It differs from Validate in two ways the wire forces: mcp_server_name
// is an accepted key here, and no tool *name* is checked against a known set,
// because an MCP server reports its own.
//
// Unlike Validate it is eager about policies rather than lazy: an MCP entry has
// no enumerable tool list, so "is this tool actually enabled" cannot be decided
// here, and a policy that cannot be evaluated is a defect wherever it sits.
func ValidateMCPToolset(raw json.RawMessage) error {
	var e entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return fmt.Errorf("%s: %w", mcpToolsetType, err)
	}
	if err := rejectUnknownToolsetKeys(mcpToolsetType, raw); err != nil {
		return err
	}
	policies := make([]*policyConfig, 0, len(e.Configs)+1)
	if e.DefaultConfig != nil {
		policies = append(policies, e.DefaultConfig.PermissionPolicy)
	}
	for _, c := range e.Configs {
		policies = append(policies, c.PermissionPolicy)
	}
	for _, pc := range policies {
		if pc == nil {
			continue
		}
		if _, err := policyType(mcpToolsetType, pc.Type); err != nil {
			return err
		}
	}
	return nil
}

// MaterializeTools returns a copy of an agent's tools[] with every toolset
// entry's configuration resolved (see Materialize). It never writes through
// its argument: the render funnels call it on the spec they are about to echo,
// while the bytes the store holds — and the update paths merge — stay exactly
// as the client sent them. A nil list stays nil so Normalize keeps deciding
// how an absent list renders.
func MaterializeTools(tools []json.RawMessage) []json.RawMessage {
	if tools == nil {
		return nil
	}
	out := make([]json.RawMessage, len(tools))
	for i, raw := range tools {
		out[i] = Materialize(raw)
	}
	return out
}

// Materialize resolves one tools[] entry for the wire's response shape: both
// toolset kinds come back carrying `configs` and `default_config`, and every
// `enabled` / `permission_policy` inside them carries a concrete value, because
// the reference's response types mark all of them required
// (BetaManagedAgentsAgentToolset20260401 and BetaManagedAgentsMCPToolset, and
// their AgentToolConfig / MCPToolConfig / *DefaultConfig members). Entries of
// any other type — custom tools, a type this build does not know — pass through
// byte for byte, as does anything that is not a JSON object.
//
// Two readings are ours and recorded in docs/DIVERGENCES.md. First, `configs`
// echoes the entries the client supplied, each resolved, rather than one entry
// per tool: an MCP server's tool names are unknowable when the agent is
// written, so listing every tool is impossible for that kind, and one rule that
// holds for both beats two that diverge. Second, resolution happens here, at
// render, rather than at write — so a stored row keeps the client's bytes, a
// row written before this code echoes resolved all the same, and the default
// policies stay constants to flip (DefaultAgentToolsetPolicy, #59) instead of
// values frozen into old rows.
//
// It fills in what was omitted and validates nothing: a supplied value is
// echoed exactly as stored, malformed or not. Rejecting malformed input is the
// API boundary's job (Validate, ValidateMCPToolset), and a read of an older row
// must not fail because of what a write once let through.
func Materialize(raw json.RawMessage) json.RawMessage {
	top, ok := jsonObject(raw)
	if !ok {
		return raw
	}
	var kind string
	if err := json.Unmarshal(top["type"], &kind); err != nil {
		return raw
	}
	var fallback domain.PermissionPolicyType
	switch kind {
	case agentToolsetType:
		fallback = DefaultAgentToolsetPolicy
	case mcpToolsetType:
		fallback = DefaultMCPToolsetPolicy
	default:
		return raw
	}

	defEnabled, defPolicy := resolveDefaultConfig(top["default_config"], fallback)
	top["default_config"] = mustMarshal(map[string]json.RawMessage{
		"enabled": defEnabled, "permission_policy": defPolicy,
	})
	top["configs"] = materializeConfigs(top["configs"], defEnabled, defPolicy)
	return mustMarshal(top)
}

// resolveDefaultConfig reads a default_config object into the pair every entry
// below it inherits: `enabled`, true when unset (the reference's documented
// default — "tools are enabled by default"), and `permission_policy`, the
// kind's default when unset. A supplied value is carried through raw.
func resolveDefaultConfig(raw json.RawMessage, fallback domain.PermissionPolicyType) (enabled, policy json.RawMessage) {
	enabled = json.RawMessage(`true`)
	policy = mustMarshal(domain.PermissionPolicy{Type: fallback})
	obj, ok := jsonObject(raw)
	if !ok {
		return enabled, policy
	}
	if v, set := present(obj["enabled"]); set {
		enabled = v
	}
	if v, set := present(obj["permission_policy"]); set {
		policy = v
	}
	return enabled, policy
}

// materializeConfigs resolves each supplied configs[] entry against the
// resolved default_config. Entries naming the same tool merge field by field,
// last value winning — the same rule resolveToolset's override map applies — and
// the merged result is emitted once, in first-appearance order: the echo
// describes the configuration in effect, not the shape of the request that set
// it. A configs[] that is absent, null, or not an array renders as [], which is
// what the response type requires of it.
func materializeConfigs(raw, defEnabled, defPolicy json.RawMessage) json.RawMessage {
	type merged struct {
		name            json.RawMessage
		enabled, policy json.RawMessage
	}
	var order []string
	byName := map[string]*merged{}
	for _, item := range jsonArray(raw) {
		obj, ok := jsonObject(item)
		if !ok {
			continue
		}
		// Group by the name as written: an unnamed entry configures nothing in
		// particular, and grouping every one of them together under "" keeps
		// the merge total rather than special-casing them out of the echo.
		var name string
		_ = json.Unmarshal(obj["name"], &name)
		m := byName[name]
		if m == nil {
			m = &merged{name: obj["name"]}
			byName[name] = m
			order = append(order, name)
		}
		if v, set := present(obj["enabled"]); set {
			m.enabled = v
		}
		if v, set := present(obj["permission_policy"]); set {
			m.policy = v
		}
	}
	out := make([]json.RawMessage, 0, len(order))
	for _, name := range order {
		m := byName[name]
		entry := map[string]json.RawMessage{
			"enabled": defEnabled, "permission_policy": defPolicy,
		}
		if m.name != nil {
			entry["name"] = m.name
		}
		if m.enabled != nil {
			entry["enabled"] = m.enabled
		}
		if m.policy != nil {
			entry["permission_policy"] = m.policy
		}
		out = append(out, mustMarshal(entry))
	}
	return mustMarshal(out)
}

// present reports whether a key carried a value worth echoing: an absent key
// and an explicit null both mean "unset", so both inherit.
func present(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	return raw, true
}

// mustMarshal marshals values this package built from already-valid JSON —
// maps, slices, and a two-field struct of them — so the error branch is
// unreachable. An impossible failure renders as JSON null rather than
// panicking: an echo is not worth a crashed request.
func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return b
}
