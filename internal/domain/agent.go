package domain

import (
	"encoding/json"
	"fmt"
	"time"
)

// Model is an agent's model selection. On the wire it is either a bare string
// ("claude-opus-4-8") or an object ({"id":…, "speed":…, "effort":…, "inference_geo":…}); we
// normalize to this struct and round-trip both forms.
type Model struct {
	ID     string      `json:"id"`
	Speed  string      `json:"speed,omitempty"` // "standard" | "fast"
	Effort ModelEffort `json:"effort,omitempty"`
	// InferenceGeo is compatibility metadata only; it does not constrain routing.
	// Checked against anthropic-sdk-go v1.70.1 — betaagent.go
	// BetaManagedAgentsModelConfigParams.InferenceGeo and BetaManagedAgentsModelConfig.InferenceGeo.
	InferenceGeo ModelInferenceGeo `json:"inference_geo,omitempty"`
}

// UnmarshalJSON accepts either a bare string or the object form.
func (m *Model) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*m = Model{ID: s}
		return nil
	}
	type alias Model
	var decoded alias
	if err := json.Unmarshal(b, &decoded); err != nil {
		return err
	}
	*m = Model(decoded)
	return nil
}

// ModelInferenceGeo preserves the optional geo selection, without enforcing it.
// The supported values come from the public agent guide:
// https://platform.claude.com/docs/en/managed-agents/agent-setup#pin-the-inference-geo
type ModelInferenceGeo string

func (g *ModelInferenceGeo) UnmarshalJSON(b []byte) error {
	var geo *string
	if err := json.Unmarshal(b, &geo); err != nil {
		return fmt.Errorf("model.inference_geo must be a string or null")
	}
	if geo == nil {
		*g = ""
		return nil
	}
	if *geo != "us" && *geo != "global" {
		return fmt.Errorf(`model.inference_geo must be "us" or "global"`)
	}
	*g = ModelInferenceGeo(*geo)
	return nil
}

// ModelEffort accepts the input union and renders the response's object form.
// Checked against anthropic-sdk-go v1.70.1 — betaagent.go
// BetaManagedAgentsModelConfigParams.Effort and BetaManagedAgentsModelConfigEffortUnion.
// The zero value leaves the choice to the configured upstream (DIVERGENCES).
type ModelEffort string

func (e *ModelEffort) UnmarshalJSON(b []byte) error {
	var level string
	if err := json.Unmarshal(b, &level); err != nil {
		var obj struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(b, &obj); err != nil {
			return fmt.Errorf("model.effort must be a level string or a {type: level} object")
		}
		level = obj.Type
	}
	switch level {
	case "low", "medium", "high", "xhigh", "max":
		*e = ModelEffort(level)
		return nil
	default:
		return fmt.Errorf("model.effort must be low, medium, high, xhigh, or max")
	}
}

func (e ModelEffort) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type string `json:"type"`
	}{Type: string(e)})
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
	// Multiagent is the coordinator roster, null for a single agent: on the
	// stored agent, pinned {id, type, version} references; on a session's
	// resolved agent, full member definitions (plan 35; internal/api/roster.go).
	Multiagent json.RawMessage `json:"multiagent"`
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
// reference the base agent. Stored verbatim in sessions.resolved_agent;
// rendering is a passthrough except for the toolset configuration inside
// tools[] — the agent's own and each multiagent member's — which the API
// resolves for the echo.
type ResolvedAgent struct {
	Type    string `json:"type"` // "agent"
	ID      ID     `json:"id"`
	Version int64  `json:"version"`
	Name    string `json:"name"`

	AgentSpec
}
