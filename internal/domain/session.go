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

// Usage accumulates token counts over the session — the session-resource
// wire shape, with cache-creation tokens nested per TTL bucket (unlike the
// flat event-level ModelUsage on span.model_request_end). Stored verbatim in
// sessions.usage.
type Usage struct {
	InputTokens          int64         `json:"input_tokens"`
	OutputTokens         int64         `json:"output_tokens"`
	CacheReadInputTokens int64         `json:"cache_read_input_tokens"`
	CacheCreation        CacheCreation `json:"cache_creation"`
}

// CacheCreation splits cache-creation input tokens by cache TTL.
type CacheCreation struct {
	Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
	Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
}

// Add folds one model turn's usage into the session totals. The flat
// cache_creation_input_tokens counter lands in the 5-minute bucket: providers
// don't report a TTL split, and 5m is the protocol's default cache TTL.
func (u *Usage) Add(m ModelUsage) {
	u.InputTokens += m.InputTokens
	u.OutputTokens += m.OutputTokens
	u.CacheReadInputTokens += m.CacheReadInputTokens
	u.CacheCreation.Ephemeral5m += m.CacheCreationInputTokens
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
