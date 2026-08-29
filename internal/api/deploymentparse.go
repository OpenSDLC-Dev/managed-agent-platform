package api

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/cron"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// scheduleExpressionMax is the expression's published ceiling.
const scheduleExpressionMax = 256

// rejectUnknownNested applies to a sub-object the additionalProperties: false
// the reference declares on it, which rejectUnknownKeys only enforces at the
// top level. The dotted name says which object refused the key, so a typo
// reads as `unknown field "schedule.timezome"` rather than being dropped and
// firing a schedule in the wrong zone for months.
//
// A raw that is not an object is left alone: the caller's own unmarshal has
// already reported that shape error in its own words.
func rejectUnknownNested(raw json.RawMessage, prefix string, allowed ...string) error {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	for key := range obj {
		if !slices.Contains(allowed, key) {
			return errInvalid("unknown field %q", prefix+"."+key)
		}
	}
	return nil
}

// parseDeploymentSchedule reads the create/update schedule union. It reports
// whether the key was present and whether it was an explicit null, because
// update needs all three cases apart: absent preserves, null clears, an object
// replaces.
//
// The expression is validated by compiling it, and one with no occurrence
// inside internal/cron's search bound is refused rather than stored.
// "0 0 31 2 *" parses, every field is in range, and February never has 31
// days — so upcoming_runs_at would be [] on an active deployment, which is the
// shape the wire reserves for an archived one (plan 37 §8.1 entry 27).
func parseDeploymentSchedule(obj map[string]json.RawMessage) (expr, tz string, set, null bool, err error) {
	raw, ok := obj["schedule"]
	if !ok {
		return "", "", false, false, nil
	}
	if isNull(raw) {
		return "", "", true, true, nil
	}
	var body struct {
		Type       *string `json:"type"`
		Expression *string `json:"expression"`
		Timezone   *string `json:"timezone"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", "", true, false, errInvalid("schedule must be an object")
	}
	if err := rejectUnknownNested(raw, "schedule", "type", "expression", "timezone"); err != nil {
		return "", "", true, false, err
	}
	// All three are required, and type is the union's discriminator rather
	// than a default we may invent: accepting its absence would echo back a
	// "cron" the caller never sent.
	if body.Type == nil {
		return "", "", true, false, errInvalid("schedule.type is required")
	}
	if *body.Type != domain.ScheduleCron {
		return "", "", true, false, errInvalid(
			"schedule.type %q is not supported; the only schedule type is %q", *body.Type, domain.ScheduleCron)
	}
	if body.Expression == nil || *body.Expression == "" {
		return "", "", true, false, errInvalid("schedule.expression is required")
	}
	if body.Timezone == nil || *body.Timezone == "" {
		return "", "", true, false, errInvalid("schedule.timezone is required")
	}
	// The published ceiling. A 5-field expression can exceed it through long
	// comma lists, and the cron parser has no length opinion of its own.
	if len([]rune(*body.Expression)) > scheduleExpressionMax {
		return "", "", true, false, errInvalid(
			"schedule.expression cannot exceed %d characters", scheduleExpressionMax)
	}
	expr, tz = *body.Expression, *body.Timezone

	next, err := cron.Upcoming(expr, tz, time.Now().UTC(), 1)
	switch {
	case errors.Is(err, cron.ErrExpression):
		return "", "", true, false, errInvalid(
			"schedule.expression is not a valid 5-field POSIX cron expression: %s", err)
	case errors.Is(err, cron.ErrTimezone):
		return "", "", true, false, errInvalid(
			"schedule.timezone is not an IANA timezone identifier: %s", err)
	case err != nil:
		return "", "", true, false, err
	}
	if len(next) == 0 {
		return "", "", true, false, errInvalid(
			"schedule.expression %q has no occurrence in the next %d years, so it would never fire",
			expr, cron.SearchYears)
	}
	return expr, tz, true, false, nil
}

// resolveDeploymentAgent resolves the agent union — a bare id string, which
// pins the latest version, or {"type":"agent","id":…,"version":…}. It is
// narrower than a session's by one arm: the reference gives a deployment no
// agent_with_overrides variant, so a per-deployment override is a 400 rather
// than a silently dropped key.
//
// FOR SHARE on the agent row, so a concurrent archive cannot land between this
// check and the write. The refusal that lock exists for is still owed: slice 1
// has yet to make the agent-archive route take FOR UPDATE and decline while a
// live deployment pins the agent (plan 37 decision 7). Taking the shared lock
// here now is what will make that refusal race-free rather than merely usually
// right — without it, an update could repin an agent the archive just cleared.
func resolveDeploymentAgent(ctx context.Context, db querier, raw json.RawMessage) (domain.AgentReference, error) {
	var ref domain.AgentReference

	var id string
	var version *int
	if err := json.Unmarshal(raw, &id); err != nil {
		var obj struct {
			Type    *string `json:"type"`
			ID      string  `json:"id"`
			Version *int    `json:"version"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			return ref, errInvalid("agent must be an agent id string or an agent reference object")
		}
		if err := rejectUnknownNested(raw, "agent", "type", "id", "version"); err != nil {
			return ref, err
		}
		// type is required and is what tells the two object arms apart, so its
		// absence cannot be read as "agent" — that would let an override
		// through as a reference with its extra keys dropped.
		if obj.Type == nil {
			return ref, errInvalid("agent.type is required on an agent reference object")
		}
		if *obj.Type != "agent" {
			return ref, errInvalid(
				"agent.type %q is not supported; a deployment takes an agent reference, not an override", *obj.Type)
		}
		if obj.ID == "" {
			return ref, errInvalid("agent.id is required")
		}
		// "Must be at least 1 if specified" — resolveAgent's answer for a
		// session, so that a zero or negative version is a 400 about the input
		// rather than a 404 about a version that could never exist.
		if obj.Version != nil && *obj.Version < 1 {
			return ref, errInvalid("agent.version must be a positive integer")
		}
		// version is optional on the object arm: BetaManagedAgentsAgentParams
		// requires only type and id, and its version says "Omit to use the
		// latest version". The endpoint prose ("an agent object with both id
		// and version specified") describes the common case and is outranked
		// by the schema it references — and by the SDK param a real client
		// builds, whose Version is a param.Opt. resolveAgent answers a
		// session's versionless reference the same way.
		id, version = obj.ID, obj.Version
	}
	if err := checkID(id, "agent"); err != nil {
		return ref, err
	}

	var latest int
	var archivedAt *time.Time
	err := db.QueryRow(ctx, `SELECT version, archived_at FROM agents WHERE id = $1 FOR SHARE`, id).
		Scan(&latest, &archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// 404, not 400: resolveAgent answers a session's missing agent the same
		// way, and checkID above has already spent a 404 on a malformed one, so
		// a well-formed id for an agent that is gone must not answer differently.
		return ref, errNotFound("agent %s not found", id)
	}
	if err != nil {
		return ref, err
	}
	if archivedAt != nil {
		return ref, errInvalid("agent %s is archived", id)
	}

	pinned := latest
	if version != nil {
		pinned = *version
		var exists bool
		err := db.QueryRow(ctx,
			`SELECT true FROM agent_versions WHERE agent_id = $1 AND version = $2`, id, pinned).Scan(&exists)
		if errors.Is(err, pgx.ErrNoRows) {
			return ref, errNotFound("agent %s version %d not found", id, pinned)
		}
		if err != nil {
			return ref, err
		}
	}
	return domain.AgentReference{Type: "agent", ID: domain.ID(id), Version: pinned}, nil
}

// validateDeploymentBounds applies the documented caps no column enforces.
// initial_events is the one collection with a floor as well as a ceiling —
// "at least 1, maximum 50" — and the ceiling is parseInitialEvents'.
func validateDeploymentBounds(name string, description *string, envID string,
	vaultIDs []string, initialEvents, resources int) error {
	switch {
	case name == "" || len([]rune(name)) > deploymentNameMax:
		return errInvalid("name must be 1-%d characters", deploymentNameMax)
	case description != nil && len([]rune(*description)) > deploymentDescriptionMax:
		return errInvalid("description cannot exceed %d characters", deploymentDescriptionMax)
	case envID == "" || len(envID) > deploymentEnvironmentMax:
		return errInvalid("environment_id must be 1-%d characters", deploymentEnvironmentMax)
	case len(vaultIDs) > deploymentVaultIDsMax:
		return errInvalid("vault_ids supports at most %d entries (got %d)", deploymentVaultIDsMax, len(vaultIDs))
	case initialEvents < 1:
		return errInvalid("initial_events must contain at least 1 event")
	case resources > deploymentResourcesMax:
		return errInvalid("resources supports at most %d entries (got %d)", deploymentResourcesMax, resources)
	}
	return nil
}
