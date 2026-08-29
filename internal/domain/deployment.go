package domain

import (
	"encoding/json"
	"time"
)

// AgentReference is an agent pinned to a concrete version — "a resolved agent
// reference with a concrete version". A deployment stores one rather than a
// floating "latest" so that publishing a new agent version cannot change what
// tonight's scheduled fire runs; repinning is an explicit update.
type AgentReference struct {
	Type    string `json:"type"` // "agent"
	ID      ID     `json:"id"`
	Version int    `json:"version"`
}

// DeploymentStatus is the two-value computed status. It is never stored: see
// Deployment.Status.
type DeploymentStatus string

const (
	DeploymentActive DeploymentStatus = "active"
	DeploymentPaused DeploymentStatus = "paused"
)

// PausedKind discriminates the two ways a deployment comes to be paused. It is
// the stored half of paused_reason, whose wire shape carries no timestamp —
// so the column pair, not the rendering, is what the platform writes.
type PausedKind string

const (
	// PausedManual: "the caller invoked the pause endpoint on the deployment".
	PausedManual PausedKind = "manual"
	// PausedError: "a scheduled fire recorded a failed run whose error
	// auto-pauses the deployment".
	PausedError PausedKind = "error"
)

// ScheduleCron is the only schedule type the reference defines. It is spelled
// out rather than assumed because the wire field is a discriminator: a second
// kind would arrive as a new value here, not as a new schema.
const ScheduleCron = "cron"

// Schedule is a deployment's recurring cron schedule together with the two
// timestamps the platform computes for it. Presence enables scheduled
// execution; a nil Schedule means manual-only.
//
// Expression and Timezone are the stored pair — always both or neither.
// LastRunAt and UpcomingRunsAt are derived at read time from deployment_runs
// and internal/cron respectively, and are never stored: a stored copy would be
// a second answer to a question the run history already answers, free to drift
// from it.
type Schedule struct {
	Type       string `json:"type"` // ScheduleCron
	Expression string `json:"expression"`
	Timezone   string `json:"timezone"`

	// LastRunAt is "time the most recent scheduled run actually started. Null
	// until one completes; preserved after the deployment is archived. Manual
	// runs do not update this."
	LastRunAt *time.Time `json:"last_run_at"`
	// UpcomingRunsAt is "up to 5 timestamps of upcoming cron occurrences" —
	// non-empty for active and paused deployments, empty once archived.
	UpcomingRunsAt []time.Time `json:"upcoming_runs_at"`
}

// MarshalJSON renders an unset UpcomingRunsAt as [] rather than null — the
// member is an array whenever it is present, and a client reading "is anything
// scheduled?" indexes into it — and stamps both timestamps in UTC.
//
// The zone is not cosmetic. LastRunAt arrives from the database in whatever
// zone the connection is set to, and rendering it as +08:00 rather than Z is a
// different string for the same instant: a client diffing two reads, or an
// acceptance test comparing against a recorded response, sees a change that
// did not happen.
func (s Schedule) MarshalJSON() ([]byte, error) {
	type alias Schedule
	if s.UpcomingRunsAt == nil {
		s.UpcomingRunsAt = []time.Time{}
	}
	utc := make([]time.Time, len(s.UpcomingRunsAt))
	for i, t := range s.UpcomingRunsAt {
		utc[i] = t.UTC()
	}
	s.UpcomingRunsAt = utc
	if s.LastRunAt != nil {
		at := s.LastRunAt.UTC()
		s.LastRunAt = &at
	}
	return json.Marshal(alias(s))
}

// PausedReasonError names why an auto-pause happened. It carries a type and
// nothing else — no message — so the run row remains the only place the
// failure's detail survives.
type PausedReasonError struct {
	Type string `json:"type"`
}

// PausedReason is the rendering of a deployment's three pause columns:
// {"type":"manual"}, or {"type":"error","error":{"type":…}}.
type PausedReason struct {
	Type  PausedKind         `json:"type"`
	Error *PausedReasonError `json:"error,omitempty"`
}

// Deployment binds an agent to everything needed to run it autonomously: an
// environment, credentials, initial events, and an optional schedule.
//
// Two of its sixteen required wire members are not fields here. `type` is a
// constant, and `status`/`paused_reason` are computed from the three pause
// columns by MarshalJSON — the invariant "paused_reason is non-null exactly
// when status is paused" is one a single renderer can hold and two
// independently-written columns cannot. `budget` is the seventeenth property,
// outside the required list, and this platform never emits it (§8.1 entry 2).
type Deployment struct {
	Scope

	ID   ID     `json:"id"` // depl_…
	Name string `json:"name"`
	// Description is nullable on the wire, and null is how the reference
	// spells "unset" — the key is required, so it is present either way.
	Description   *string           `json:"description"`
	Agent         AgentReference    `json:"agent"`
	EnvironmentID ID                `json:"environment_id"`
	VaultIDs      []ID              `json:"vault_ids"`
	InitialEvents []json.RawMessage `json:"initial_events"`
	// Resources "echoes the input minus write-only credentials" — raw wire
	// JSON, so it passes through byte-for-byte after the API has stripped
	// what must never be echoed.
	Resources []json.RawMessage `json:"resources"`
	Metadata  map[string]string `json:"metadata"`
	Schedule  *Schedule         `json:"schedule"`

	// The three stored pause columns. They are storage, not wire: emitting
	// them beside paused_reason would publish a second, unschema'd spelling of
	// the same fact.
	PausedAt        *time.Time `json:"-"`
	PausedKind      PausedKind `json:"-"`
	PausedErrorType string     `json:"-"`

	// CreatedBy is audit-only — which API key or principal created the
	// deployment — and never on the wire, sessions.created_by's rule. It
	// matters more here: a session a schedule fires has no creator, so this is
	// where the audit trail survives.
	CreatedBy *string `json:"-"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// ArchivedAt is set once and never cleared. Archiving is one-way: this
	// platform serves no unarchive and no DELETE /v1/deployments.
	ArchivedAt *time.Time `json:"archived_at"`
}

// Status is "active" or "paused", and an archived deployment reports "active"
// with archived_at set. Archive leaves the pause columns exactly as it found
// them, so without that last rule a deployment archived while paused would
// report paused forever on a row nothing can fire — and nothing unarchives, so
// nothing could observe it recover.
func (d Deployment) Status() DeploymentStatus {
	if d.ArchivedAt == nil && d.PausedAt != nil {
		return DeploymentPaused
	}
	return DeploymentActive
}

// Reason renders paused_reason: "non-null exactly when status is paused; null
// otherwise". It reads the same two columns Status does, so the two cannot
// disagree.
func (d Deployment) Reason() *PausedReason {
	if d.Status() != DeploymentPaused {
		return nil
	}
	r := &PausedReason{Type: d.PausedKind}
	if d.PausedKind == PausedError {
		r.Error = &PausedReasonError{Type: d.PausedErrorType}
	}
	return r
}

// MarshalJSON adds the three members nothing stores — the constant type and
// the computed status/paused_reason — and guarantees the collections render as
// [] and {} rather than null.
func (d Deployment) MarshalJSON() ([]byte, error) {
	if d.VaultIDs == nil {
		d.VaultIDs = []ID{}
	}
	if d.InitialEvents == nil {
		d.InitialEvents = []json.RawMessage{}
	}
	if d.Resources == nil {
		d.Resources = []json.RawMessage{}
	}
	if d.Metadata == nil {
		d.Metadata = map[string]string{}
	}
	// UTC for the same reason Schedule.MarshalJSON does it: the database hands
	// these back in the connection's zone, and the same instant rendered as
	// +08:00 is a different string from the same instant rendered as Z.
	d.CreatedAt, d.UpdatedAt = d.CreatedAt.UTC(), d.UpdatedAt.UTC()
	if d.ArchivedAt != nil {
		at := d.ArchivedAt.UTC()
		d.ArchivedAt = &at
	}

	type alias Deployment
	return json.Marshal(struct {
		Type string `json:"type"`
		alias
		Status       DeploymentStatus `json:"status"`
		PausedReason *PausedReason    `json:"paused_reason"`
	}{
		Type:         "deployment",
		alias:        alias(d),
		Status:       d.Status(),
		PausedReason: d.Reason(),
	})
}
