package domain

import (
	"encoding/json"
	"time"
)

// Model is an agent's model selection. On the wire it is either a bare string
// ("claude-opus-4-8") or an object ({"id":…,"speed":"standard|fast"}); we
// normalize to this struct and round-trip both forms.
type Model struct {
	ID    string `json:"id"`
	Speed string `json:"speed,omitempty"` // "standard" | "fast"
}

// UnmarshalJSON accepts either a bare string or the object form.
func (m *Model) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		m.ID, m.Speed = s, ""
		return nil
	}
	type alias Model
	return json.Unmarshal(b, (*alias)(m))
}

// PermissionPolicyType controls whether a tool runs automatically or pauses for
// confirmation. Defaults per the reference: agent toolset = always_allow,
// mcp toolset = always_ask.
type PermissionPolicyType string

const (
	PolicyAlwaysAllow PermissionPolicyType = "always_allow"
	PolicyAlwaysAsk   PermissionPolicyType = "always_ask"
)

// PermissionPolicy wraps the policy type in the wire's {"type":…} object.
type PermissionPolicy struct {
	Type PermissionPolicyType `json:"type"`
}

// EvaluatedPermission is the resolved permission the brain stamps on an
// agent.tool_use / agent.mcp_tool_use event: the platform ran the tool
// automatically (allow), paused it for human confirmation (ask), or blocked it
// (deny — reserved; no configurable permission_policy produces it yet).
type EvaluatedPermission string

const (
	EvalPermAllow EvaluatedPermission = "allow"
	EvalPermAsk   EvaluatedPermission = "ask"
	EvalPermDeny  EvaluatedPermission = "deny"
)

// AgentSpec is the mutable configuration of an agent, shared by the stored
// Agent resource and the per-session ResolvedAgent snapshot. This is the wire
// shape: every field is always present (the surface is api:"required"), and
// tools/mcp_servers/skills entries stay raw wire JSON so they pass through
// byte-for-byte — validation happens at the API boundary.
type AgentSpec struct {
	Model       Model             `json:"model"`
	System      string            `json:"system"`
	Description string            `json:"description"`
	Tools       []json.RawMessage `json:"tools"`
	MCPServers  []json.RawMessage `json:"mcp_servers"`
	Skills      []json.RawMessage `json:"skills"`
	Multiagent  json.RawMessage   `json:"multiagent"` // reserved seam: always null in v1
}

// Normalize guarantees non-nil collections so JSON renders [] rather than null.
func (s *AgentSpec) Normalize() {
	if s.Tools == nil {
		s.Tools = []json.RawMessage{}
	}
	if s.MCPServers == nil {
		s.MCPServers = []json.RawMessage{}
	}
	if s.Skills == nil {
		s.Skills = []json.RawMessage{}
	}
}

// Agent is a versioned, reusable configuration. Updates use optimistic locking
// on Version (mismatch → 409); each change bumps Version and snapshots an
// AgentVersion.
type Agent struct {
	Scope
	AgentSpec

	ID         ID                `json:"id"` // agent_…
	Type       string            `json:"type"`
	Name       string            `json:"name"`
	Version    int               `json:"version"` // starts at 1
	Metadata   map[string]string `json:"metadata,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
	ArchivedAt *time.Time        `json:"archived_at"`
}

// ResolvedAgent is the agent config actually applied to a session, after any
// per-session overrides (BetaManagedAgentsSessionAgent). ID/Version still
// reference the base agent. Stored verbatim in sessions.resolved_agent, so
// rendering is a passthrough.
type ResolvedAgent struct {
	Type    string `json:"type"` // "agent"
	ID      ID     `json:"id"`
	Version int64  `json:"version"`
	Name    string `json:"name"`

	AgentSpec
}
