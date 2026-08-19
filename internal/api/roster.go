package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
	"github.com/jackc/pgx/v5"
)

// The multiagent roster (plan 35 decision 10). An agent's `multiagent` is a
// coordinator topology: the agents the primary thread may spawn as session
// threads. It is resolved at agent create/update and snapshotted at session
// create, in two shapes:
//
//   - stored on the agent (BetaManagedAgentsMultiagent): {type:"coordinator",
//     agents:[{id, type:"agent", version}]} — every entry pinned to a concrete
//     version, the eager pin the response type's plain int64 implies (a bare
//     id string and a versionless reference pin the member's *current* version;
//     the docs say the roster "never follows later member updates");
//     {type:"self"} resolves to the coordinator's own id and the version the
//     write produces.
//   - stored on the session (BetaManagedAgentsSessionMultiagentCoordinator):
//     {type:"coordinator", agents:[SessionThreadAgent…]} — full definitions
//     fetched from each member's pinned version, except the `self` member,
//     whose definition is the coordinator's own resolved spec for this session
//     (overrides applied, per the docs' "overrides apply to the coordinator and
//     its `self` copies") minus `multiagent`.
//
// The roster is untyped in domain.AgentSpec (a json.RawMessage) so one struct
// serves both shapes; these are the parsers and renderers around it.

// rosterMaxEntries is the documented roster size: "1–20 entries".
const rosterMaxEntries = 20

// rosterRef is one resolved roster entry as stored on the agent.
type rosterRef struct {
	ID      string `json:"id"`
	Type    string `json:"type"` // "agent"
	Version int64  `json:"version"`
}

// storedRoster is the agent-side shape.
type storedRoster struct {
	Type   string      `json:"type"` // "coordinator"
	Agents []rosterRef `json:"agents"`
}

// threadAgentJSON is BetaManagedAgentsSessionThreadAgent: a roster member's
// full definition as snapshotted into a session. Every field is required on
// the wire; the roster is deliberately not repeated inside a member.
type threadAgentJSON struct {
	ID          string            `json:"id"`
	Type        string            `json:"type"` // "agent"
	Version     int64             `json:"version"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Model       domain.Model      `json:"model"`
	System      string            `json:"system"`
	Tools       []json.RawMessage `json:"tools"`
	MCPServers  []json.RawMessage `json:"mcp_servers"`
	Skills      []json.RawMessage `json:"skills"`
}

// sessionRoster is the session-side shape.
type sessionRoster struct {
	Type   string            `json:"type"` // "coordinator"
	Agents []threadAgentJSON `json:"agents"`
}

func threadAgentOf(id string, version int64, name string, spec domain.AgentSpec) threadAgentJSON {
	spec.Normalize()
	return threadAgentJSON{
		ID: id, Type: "agent", Version: version, Name: name,
		Description: spec.Description, Model: spec.Model, System: spec.System,
		Tools: spec.Tools, MCPServers: spec.MCPServers, Skills: spec.Skills,
	}
}

// resolveRoster validates a `multiagent` request value and resolves it into
// the stored roster: selfID/selfVersion are the coordinator's own id and the
// version this write produces (1 on create, current+1 on update). It runs
// inside the caller's transaction and takes FOR SHARE on every referenced
// agent row, so a concurrent archive cannot slip between the check and the
// commit. The documented constraints (SDK betasession.go, the roster doc
// comment): 1–20 entries; a bare id string, {type:"agent",id,version?} or
// {type:"self"}; distinct agents after resolving `self` and string forms; at
// most one `self`; referenced agents exist, are not archived, and do not
// themselves carry `multiagent` (depth limit 1). `self` is exempt from the
// depth check — read literally the rule would forbid the documented feature —
// and the depth check reads the spec that gets pinned, since that is the
// definition a thread would run. Every rejection is a 400 naming the entry.
func resolveRoster(ctx context.Context, tx pgx.Tx, raw json.RawMessage, selfID string, selfVersion int64) (json.RawMessage, error) {
	var in struct {
		Type   *string           `json:"type"`
		Agents []json.RawMessage `json:"agents"`
	}
	obj, err := asObjectRaw(raw)
	if err != nil {
		return nil, errInvalid("multiagent must be an object")
	}
	if err := rejectUnknownKeys(obj, "type", "agents"); err != nil {
		return nil, errInvalid("multiagent: %s", err.Error())
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, errInvalid("multiagent must be a {type, agents} object")
	}
	if in.Type == nil || *in.Type != "coordinator" {
		return nil, errInvalid(`multiagent.type must be "coordinator"`)
	}
	if len(in.Agents) == 0 || len(in.Agents) > rosterMaxEntries {
		return nil, errInvalid("multiagent.agents must have between 1 and %d entries", rosterMaxEntries)
	}

	out := storedRoster{Type: "coordinator", Agents: make([]rosterRef, 0, len(in.Agents))}
	seen := map[string]bool{}
	selfSeen := false
	for i, entry := range in.Agents {
		id, version, isSelf, err := parseRosterEntry(entry)
		if err != nil {
			return nil, errInvalid("multiagent.agents[%d]: %s", i, err.Error())
		}
		if isSelf {
			if selfSeen {
				return nil, errInvalid("multiagent.agents[%d]: at most one self entry", i)
			}
			selfSeen = true
			id, version = selfID, selfVersion
		} else {
			// The lock is on the agent row: agent_versions rows are immutable,
			// and what a concurrent writer could change is the archive state.
			var (
				current    int64
				archivedAt *time.Time
			)
			err := tx.QueryRow(ctx,
				`SELECT version, archived_at FROM agents WHERE id = $1 FOR SHARE`, id).
				Scan(&current, &archivedAt)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, errInvalid("multiagent.agents[%d]: agent %s not found", i, id)
			}
			if err != nil {
				return nil, err
			}
			if archivedAt != nil {
				return nil, errInvalid("multiagent.agents[%d]: agent %s is archived", i, id)
			}
			if version == 0 {
				version = current
			}
			var specJSON []byte
			err = tx.QueryRow(ctx,
				`SELECT spec FROM agent_versions WHERE agent_id = $1 AND version = $2`, id, version).
				Scan(&specJSON)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, errInvalid("multiagent.agents[%d]: agent %s version %d not found", i, id, version)
			}
			if err != nil {
				return nil, err
			}
			var member struct {
				Multiagent json.RawMessage `json:"multiagent"`
			}
			if err := json.Unmarshal(specJSON, &member); err != nil {
				return nil, fmt.Errorf("decode stored agent spec: %w", err)
			}
			if len(member.Multiagent) > 0 && !isNull(member.Multiagent) {
				return nil, errInvalid("multiagent.agents[%d]: agent %s is itself a coordinator (depth limit 1)", i, id)
			}
		}
		if seen[id] {
			return nil, errInvalid("multiagent.agents[%d]: agent %s is referenced more than once", i, id)
		}
		seen[id] = true
		out.Agents = append(out.Agents, rosterRef{ID: id, Type: "agent", Version: version})
	}
	return json.Marshal(out)
}

// asObjectRaw decodes raw as a JSON object; null and non-objects are errors.
func asObjectRaw(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, errors.New("not a JSON object")
	}
	return obj, nil
}

// parseRosterEntry reads one roster entry: a bare agent-id string, a
// {type:"agent", id, version?} reference, or {type:"self"}. version 0 means
// "pin the current version".
func parseRosterEntry(raw json.RawMessage) (id string, version int64, isSelf bool, err error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return "", 0, false, errors.New("agent id must not be empty")
		}
		return s, 0, false, nil
	}
	obj, err := asObjectRaw(raw)
	if err != nil {
		return "", 0, false, errors.New(`entry must be an agent id string, {"type":"agent","id",…} or {"type":"self"}`)
	}
	var typ string
	if raw, ok := obj["type"]; !ok || json.Unmarshal(raw, &typ) != nil {
		return "", 0, false, errors.New(`entry type must be "agent" or "self"`)
	}
	switch typ {
	case "self":
		if err := rejectUnknownKeys(obj, "type"); err != nil {
			return "", 0, false, err
		}
		return "", 0, true, nil
	case "agent":
		if err := rejectUnknownKeys(obj, "type", "id", "version"); err != nil {
			return "", 0, false, err
		}
		if raw, ok := obj["id"]; !ok || json.Unmarshal(raw, &id) != nil || id == "" {
			return "", 0, false, errors.New("id is required")
		}
		if raw, ok := obj["version"]; ok && !isNull(raw) {
			if err := json.Unmarshal(raw, &version); err != nil || version < 1 {
				return "", 0, false, errors.New("version must be a positive integer")
			}
		}
		return id, version, false, nil
	default:
		return "", 0, false, errors.New(`entry type must be "agent" or "self"`)
	}
}

// snapshotRoster builds the session-side roster from a stored one: every
// member's definition is fetched from its pinned version, and the `self`
// member — the entry naming the coordinator at the version the session
// resolved — is `self`'s own resolved spec, overrides applied, minus the
// roster. Members are not re-checked for archive state here: the roster was
// validated when it was written and pins immutable versions (INFERRED,
// docs/DIVERGENCES.md).
func snapshotRoster(ctx context.Context, db querier, stored json.RawMessage, self sessionAgentJSON) (json.RawMessage, error) {
	var roster storedRoster
	if err := json.Unmarshal(stored, &roster); err != nil {
		return nil, fmt.Errorf("decode stored roster: %w", err)
	}
	out := sessionRoster{Type: "coordinator", Agents: make([]threadAgentJSON, 0, len(roster.Agents))}
	for _, ref := range roster.Agents {
		if ref.ID == self.ID.String() && ref.Version == self.Version {
			spec := self.AgentSpec
			spec.Multiagent = nil
			out.Agents = append(out.Agents, threadAgentOf(ref.ID, ref.Version, self.Name, spec))
			continue
		}
		var (
			name     string
			specJSON []byte
		)
		err := db.QueryRow(ctx,
			`SELECT name, spec FROM agent_versions WHERE agent_id = $1 AND version = $2`, ref.ID, ref.Version).
			Scan(&name, &specJSON)
		if errors.Is(err, pgx.ErrNoRows) {
			// A pinned version cannot vanish while its agent exists; only a
			// deleted agent (no such route) could get here.
			return nil, errInvalid("multiagent member %s version %d not found", ref.ID, ref.Version)
		}
		if err != nil {
			return nil, err
		}
		var spec domain.AgentSpec
		if err := json.Unmarshal(specJSON, &spec); err != nil {
			return nil, fmt.Errorf("decode stored agent spec: %w", err)
		}
		out.Agents = append(out.Agents, threadAgentOf(ref.ID, ref.Version, name, spec))
	}
	return json.Marshal(out)
}

// patchSelfMember rewrites the `self` member of a session-side roster after a
// session update changed the coordinator's tools or mcp_servers, keeping the
// documented rule — the `self` copy is the coordinator's resolved spec for this
// session — true after the update as it was at create. A snapshot without a
// self member (or without a roster) is returned unchanged.
func patchSelfMember(stored json.RawMessage, self sessionAgentJSON) (json.RawMessage, error) {
	if len(stored) == 0 || isNull(stored) {
		return stored, nil
	}
	var roster sessionRoster
	if err := json.Unmarshal(stored, &roster); err != nil {
		return nil, fmt.Errorf("decode stored roster snapshot: %w", err)
	}
	for i := range roster.Agents {
		if roster.Agents[i].ID == self.ID.String() && roster.Agents[i].Version == self.Version {
			spec := self.AgentSpec
			spec.Multiagent = nil
			roster.Agents[i] = threadAgentOf(roster.Agents[i].ID, roster.Agents[i].Version, self.Name, spec)
		}
	}
	return json.Marshal(roster)
}

// materializeRoster resolves the toolset configuration inside every member of
// a session-side roster, the rule renderSession applies to the coordinator's
// own tools[]. It never fails: a roster it cannot decode ships as stored.
func materializeRoster(stored json.RawMessage) json.RawMessage {
	if len(stored) == 0 || isNull(stored) {
		return stored
	}
	var roster sessionRoster
	if err := json.Unmarshal(stored, &roster); err != nil {
		return stored
	}
	for i := range roster.Agents {
		if roster.Agents[i].Tools == nil {
			roster.Agents[i].Tools = []json.RawMessage{}
		}
		roster.Agents[i].Tools = toolset.MaterializeTools(roster.Agents[i].Tools)
	}
	out, err := json.Marshal(roster)
	if err != nil {
		return stored
	}
	return out
}
