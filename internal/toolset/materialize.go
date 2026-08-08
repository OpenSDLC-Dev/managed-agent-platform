package toolset

import (
	"encoding/json"
	"fmt"
	"slices"

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
// Every check walks the raw JSON rather than decoding into Go types. A typed
// unmarshal would answer the leaf questions ("is enabled a boolean") for free,
// but its error text is a dump of the receiving struct — unexported field names
// and all — and this package's errors are the message of a 400, so they name
// the field's path instead. A raw walk is also the only way to tell an explicit
// null from an absent key, which a pointer field cannot.
func ValidateMCPToolset(raw json.RawMessage) error {
	top, ok := jsonObject(raw)
	if !ok {
		return fmt.Errorf("%s: entry must be an object", mcpToolsetType)
	}
	if err := rejectUnknownToolsetKeys(mcpToolsetType, raw); err != nil {
		return err
	}
	if dc, set := present(top["default_config"]); set {
		obj, ok := jsonObject(dc)
		if !ok {
			return fmt.Errorf("%s: default_config must be an object", mcpToolsetType)
		}
		if err := checkMCPConfigFields(obj, "default_config"); err != nil {
			return err
		}
	}
	cfgs, set := present(top["configs"])
	if !set {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(cfgs, &items); err != nil {
		return fmt.Errorf("%s: configs must be an array", mcpToolsetType)
	}
	for i, item := range items {
		path := fmt.Sprintf("configs[%d]", i)
		c, ok := jsonObject(item)
		if !ok {
			return fmt.Errorf("%s: %s must be an object", mcpToolsetType, path)
		}
		// name identifies the tool the entry configures and is required on the
		// response type, so an entry without one both configures nothing and
		// would render an echo the wire schema rejects.
		var name string
		if err := json.Unmarshal(c["name"], &name); err != nil || name == "" {
			return fmt.Errorf("%s: %s requires a non-empty name", mcpToolsetType, path)
		}
		if err := checkMCPConfigFields(c, path); err != nil {
			return err
		}
	}
	return nil
}

// checkMCPConfigFields validates the two settings a default_config or configs[]
// entry carries. A permission_policy must name a policy this platform can
// evaluate, and neither setting may be an explicit null: the wire's unions have
// no null arm, and null is indistinguishable from an omission once decoded — so
// accepting it would let `"permission_policy": null` silently inherit a
// permissive default where the author wrote a gate. Absent keys are fine; that
// is what inheritance is for.
func checkMCPConfigFields(obj map[string]json.RawMessage, path string) error {
	for _, field := range []string{"enabled", "permission_policy"} {
		if raw, ok := obj[field]; ok && string(raw) == "null" {
			return fmt.Errorf("%s: %s.%s must not be null (omit it to inherit)",
				mcpToolsetType, path, field)
		}
	}
	if raw, set := present(obj["enabled"]); set {
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return fmt.Errorf("%s: %s.enabled must be a boolean", mcpToolsetType, path)
		}
	}
	if raw, set := present(obj["permission_policy"]); set {
		pp, ok := jsonObject(raw)
		if !ok {
			return fmt.Errorf("%s: %s.permission_policy must be an object", mcpToolsetType, path)
		}
		var typ string
		_ = json.Unmarshal(pp["type"], &typ)
		if _, err := policyType(mcpToolsetType, typ); err != nil {
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
// echoed as stored, malformed or not. Rejecting malformed input is the API
// boundary's job (Validate, ValidateMCPToolset), and a read of an older row
// must not fail because of what a write once let through. One thing does not
// survive: an unknown *key* nested inside default_config or a resolved
// configs[] entry is dropped, because those objects are rebuilt from the three
// fields the schema defines. Only a row written before that validation existed
// can carry one, an unknown key at the toolset object's own level is preserved,
// and the drop is toward the safe reading — a stored `permission_polciy`
// disappears from the echo and the tool renders with the default policy it
// actually resolves to. A configs[] entry that resolves to no tool at all is
// not rebuilt, so it keeps every key it was stored with (materializeConfigs).
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
	top["configs"] = materializeConfigs(kind, top["configs"], defEnabled, defPolicy)
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
//
// Only an entry that configures a tool this toolset actually has is resolved
// (configurable). Anything else — a non-object element, an entry with no name,
// a name no built-in answers to — is echoed exactly as supplied, in place. Both
// arms of that rule matter. Resolving such an entry would state something
// untrue: filling in `enabled` and `permission_policy` presents the entry as
// effective configuration when resolveToolset ignores it entirely, and merging
// every nameless entry under one "" key would collapse distinct entries into a
// single fabricated one. Dropping it would be worse still — a client that GETs
// an agent and re-POSTs the tools it was handed would silently lose entries the
// API accepted. Echoing it unchanged is what this code did before the entry was
// resolved at all, and it keeps every configs[] element accounted for.
func materializeConfigs(kind string, raw, defEnabled, defPolicy json.RawMessage) json.RawMessage {
	type merged struct {
		name            json.RawMessage
		enabled, policy json.RawMessage
	}
	// A slot is one output element: either an entry echoed as supplied, or the
	// name of a merge bucket resolved once the whole array has been read.
	type slot struct {
		verbatim json.RawMessage
		name     string
	}
	var slots []slot
	byName := map[string]*merged{}
	for _, item := range jsonArray(raw) {
		obj, ok := jsonObject(item)
		if !ok {
			slots = append(slots, slot{verbatim: item})
			continue
		}
		var name string
		_ = json.Unmarshal(obj["name"], &name)
		if !configurable(kind, name) {
			slots = append(slots, slot{verbatim: item})
			continue
		}
		m := byName[name]
		if m == nil {
			m = &merged{name: obj["name"]}
			byName[name] = m
			slots = append(slots, slot{name: name})
		}
		if v, set := present(obj["enabled"]); set {
			m.enabled = v
		}
		if v, set := present(obj["permission_policy"]); set {
			m.policy = v
		}
	}
	out := make([]json.RawMessage, 0, len(slots))
	for _, s := range slots {
		if s.verbatim != nil {
			out = append(out, s.verbatim)
			continue
		}
		m := byName[s.name]
		entry := map[string]json.RawMessage{
			"name": m.name, "enabled": defEnabled, "permission_policy": defPolicy,
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

// configurable reports whether a configs[] entry naming this tool resolves to
// anything. The built-in toolset configures exactly its own definitions, so a
// name outside them overrides nothing; an MCP server reports its own tools, so
// any name it carries can be real and only an empty one is meaningless.
func configurable(kind, name string) bool {
	if name == "" {
		return false
	}
	if kind != agentToolsetType {
		return true
	}
	return slices.ContainsFunc(definitions, func(d toolDef) bool { return d.name == name })
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
