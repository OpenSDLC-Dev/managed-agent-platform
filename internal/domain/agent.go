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

// Tool is a member of an agent's tools[] union. The concrete shape depends on
// Type ("agent_toolset_20260401" | "mcp_toolset" | "custom"); v1 keeps the
// full body as Raw and only lifts the discriminator fields it routes on.
type Tool struct {
	Type          string          `json:"type"`
	MCPServerName string          `json:"mcp_server_name,omitempty"` // mcp_toolset
	Name          string          `json:"name,omitempty"`            // custom
	Raw           json.RawMessage `json:"-"`                         // full original object
}

// MCPServer is an entry in an agent's mcp_servers[]. Type must be "url".
type MCPServer struct {
	Type string `json:"type"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Skill is an entry in an agent's skills[] (anthropic pre-built or custom).
type Skill struct {
	Type    string `json:"type"`     // "anthropic" | "custom"
	SkillID string `json:"skill_id"` // e.g. "xlsx" or "skill_…"
	Version string `json:"version,omitempty"`
}

// AgentSpec is the mutable configuration of an agent, shared by the stored
// Agent resource and the per-session ResolvedAgent snapshot.
type AgentSpec struct {
	Model       Model           `json:"model"`
	System      string          `json:"system,omitempty"`
	Tools       []Tool          `json:"tools,omitempty"`
	MCPServers  []MCPServer     `json:"mcp_servers,omitempty"`
	Skills      []Skill         `json:"skills,omitempty"`
	Multiagent  json.RawMessage `json:"multiagent,omitempty"`
	Description *string         `json:"description,omitempty"`
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
// per-session overrides. ID/Version still reference the base agent.
type ResolvedAgent struct {
	AgentSpec

	Type    string `json:"type"`
	ID      ID     `json:"id"`
	Version int    `json:"version"`
	Name    string `json:"name,omitempty"`
}
