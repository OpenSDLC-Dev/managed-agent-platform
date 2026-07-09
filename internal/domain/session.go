package domain

import "time"

// SessionStatus is the session state-machine value, wire-compatible with
// Anthropic Managed Agents.
type SessionStatus string

const (
	SessionIdle         SessionStatus = "idle"         // waiting for input; new sessions start here
	SessionRunning      SessionStatus = "running"      // executing
	SessionRescheduling SessionStatus = "rescheduling" // transient error, auto-retrying
	SessionTerminated   SessionStatus = "terminated"   // unrecoverable, ended
)

// Usage accumulates token counts over the session, mirroring the wire shape.
type Usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// Scope is the reserved multi-tenant scoping carried by every core resource.
// v1 is single-tenant and fills these with default values; the columns exist
// from day 1 so multi-tenancy can land without a migration of meaning.
//
// NOTE: scoping is org/workspace/project — NEVER user. Sessions are not bound
// to an end-user (a deliberate divergence from adk's AppName+UserID). End-user
// ↔ session ownership is an application-layer concern.
type Scope struct {
	OrgID       string `json:"-"`
	WorkspaceID string `json:"-"`
	ProjectID   string `json:"-"`
}

// Session is a running instance of an agent in an environment. It references a
// resolved snapshot of the agent config (so agent updates don't retroactively
// change a live session) and an environment.
type Session struct {
	Scope

	ID            ID            `json:"id"` // sesn_…
	Type          string        `json:"type"`
	Title         string        `json:"title,omitempty"`
	Status        SessionStatus `json:"status"`
	EnvironmentID ID            `json:"environment_id"`

	// Agent is the resolved agent snapshot applied to this session (after any
	// per-session overrides). Its embedded id/version still point at the base
	// agent for traceability.
	Agent ResolvedAgent `json:"agent"`

	VaultIDs  []ID              `json:"vault_ids,omitempty"`
	Resources []SessionResource `json:"resources,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Usage     Usage             `json:"usage"`

	// CreatedBy is audit-only: which API key / principal created the session.
	// It does NOT participate in isolation/partitioning and is not part of the
	// wire schema — it exists purely for on-prem audit. Nil when unknown.
	CreatedBy *string `json:"-"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SessionResource is a mounted resource (github_repository, file, …). Modeled
// as a raw envelope in v1; typed variants land with the resource-mount slice.
type SessionResource struct {
	ID   ID     `json:"id"` // sesrsc_…
	Type string `json:"type"`
	Raw  []byte `json:"-"`
}
