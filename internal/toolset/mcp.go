package toolset

import (
	"encoding/json"
	"fmt"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// MCPTool is one tool as an MCP server reported it, in the shape the session's
// catalog row stores. It is this package's own type rather than internal/mcp's
// on purpose: the go-sdk that package wraps has no business in the brain's
// request-assembly path, and the listing is three fields either way.
type MCPTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// MCPResolved is one enabled MCP tool: what the server reported, plus the
// permission policy the mcp_toolset entry resolves for it.
type MCPResolved struct {
	MCPTool
	Policy domain.PermissionPolicyType
}

// ResolveMCP applies an mcp_toolset entry's default_config and configs[] onto
// the tools one server reported, returning the enabled ones in the order the
// server listed them and the configs[] names that named no reported tool.
//
// It is resolveToolset for a list this package does not know: enable and policy
// resolve independently, a per-tool config overrides default_config, and
// default_config overrides the toolset default — on, and
// DefaultMCPToolsetPolicy. Three things differ, and each is the wire's doing.
//
// The unknown names are returned rather than rejected. An MCP server's tool
// list is dynamic and unknowable when the agent is written, which is why the
// docs make an unrecognised configs[] name a warning and not an error; the
// caller says so where a human will see it.
//
// A repeated tool name is dropped after its first listing. Nothing downstream
// could tell the two apart — a result names a tool, not a position — and two
// definitions under one name is a request the endpoint rejects, which would
// cost the whole turn rather than the duplicate.
//
// Unknown keys are not rejected here, where Tools and Policies do reject them
// for the built-in kind. The API boundary (ValidateMCPToolset) is where an
// mcp_toolset's shape is checked, and a row stored before that validation
// existed must not turn a turn that used to expand to nothing into a turn that
// fails. An unevaluable *policy* is the exception, for the reason it always is:
// defaulting it would run an unconfirmed tool (#26), so it is an error wherever
// a live tool carries one.
func ResolveMCP(raw json.RawMessage, tools []MCPTool) ([]MCPResolved, []string, error) {
	var e entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", mcpToolsetType, err)
	}

	enabled := true
	var defaultPolicy *policyConfig
	if e.DefaultConfig != nil {
		if e.DefaultConfig.Enabled != nil {
			enabled = *e.DefaultConfig.Enabled
		}
		defaultPolicy = e.DefaultConfig.PermissionPolicy
	}

	type override struct {
		enabled *bool
		policy  *policyConfig
	}
	overrides := make(map[string]override, len(e.Configs))
	var configured []string // first-appearance order, for the unknown report
	for _, c := range e.Configs {
		if c.Name == "" {
			continue
		}
		o, seen := overrides[c.Name]
		if !seen {
			configured = append(configured, c.Name)
		}
		if c.Enabled != nil {
			o.enabled = c.Enabled
		}
		if c.PermissionPolicy != nil {
			o.policy = c.PermissionPolicy
		}
		overrides[c.Name] = o
	}

	var out []MCPResolved
	reported := make(map[string]bool, len(tools))
	for _, t := range tools {
		if reported[t.Name] {
			continue
		}
		reported[t.Name] = true

		o := overrides[t.Name]
		on := enabled
		if o.enabled != nil {
			on = *o.enabled
		}
		if !on {
			continue
		}
		pc := defaultPolicy
		if o.policy != nil {
			pc = o.policy
		}
		policy := DefaultMCPToolsetPolicy
		if pc != nil {
			p, err := policyType(mcpToolsetType, pc.Type)
			if err != nil {
				return nil, nil, err
			}
			policy = p
		}
		out = append(out, MCPResolved{MCPTool: t, Policy: policy})
	}

	var unknown []string
	for _, name := range configured {
		if !reported[name] {
			unknown = append(unknown, name)
		}
	}
	return out, unknown, nil
}
