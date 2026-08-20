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
	"github.com/jackc/pgx/v5/pgconn"
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
//     write produces — and so does any entry naming the coordinator's own id,
//     because the rendered roster carries no self marker and the echo of a GET
//     has to read back as what it rendered.
//   - stored on the session (BetaManagedAgentsSessionMultiagentCoordinator):
//     {type:"coordinator", agents:[SessionThreadAgent…]} — full definitions
//     fetched from each member's pinned version, except the `self` member,
//     whose definition is the coordinator's own resolved spec for this session
//     (overrides applied, per the docs' "overrides apply to the coordinator and
//     its `self` copies").
//
// The roster is untyped in domain.AgentSpec (a json.RawMessage) so one struct
// serves both shapes; these are the parsers and renderers around it. The
// rewriters (repinSelf, patchSelfMember, materializeRoster) touch one member,
// or one key of one member, and leave every other byte as stored.

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
// the wire; the roster is deliberately not repeated inside a member. Declared
// field by field rather than embedding domain.AgentSpec on purpose: this is
// the SDK's fixed ten-field shape, not the spec, and TestRosterSessionSnapshot
// pins the ten names.
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

// rawRoster is either shape with its members' bytes kept, for the rewriters.
type rawRoster struct {
	Type   string            `json:"type"`
	Agents []json.RawMessage `json:"agents"`
}

func threadAgentOf(id string, version int64, name string, spec domain.AgentSpec) threadAgentJSON {
	spec.Normalize()
	return threadAgentJSON{
		ID: id, Type: "agent", Version: version, Name: name,
		Description: spec.Description, Model: spec.Model, System: spec.System,
		Tools: spec.Tools, MCPServers: spec.MCPServers, Skills: spec.Skills,
	}
}

// selfMember is the `self` member of a session-side roster: the coordinator's
// resolved spec for this session, overrides applied (threadAgentOf carries no
// roster, so "minus `multiagent`" falls out of the shape).
func selfMember(self sessionAgentJSON) threadAgentJSON {
	return threadAgentOf(self.ID.String(), self.Version, self.Name, self.AgentSpec)
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
	obj, err := asObjectRaw(raw)
	if err != nil {
		return nil, errInvalid("multiagent must be an object")
	}
	if err := rejectUnknownKeys(obj, "type", "agents"); err != nil {
		return nil, errInvalid("multiagent: %s", err.Error())
	}
	var typ string
	if raw, ok := obj["type"]; !ok || json.Unmarshal(raw, &typ) != nil || typ != "coordinator" {
		return nil, errInvalid(`multiagent.type must be "coordinator"`)
	}
	entries, err := rawList(obj["agents"], "multiagent.agents")
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 || len(entries) > rosterMaxEntries {
		return nil, errInvalid("multiagent.agents must have between 1 and %d entries", rosterMaxEntries)
	}

	refs := make([]rosterRef, len(entries))
	seen := map[string]bool{}
	selfSeen := false
	var ids []string // the members to look up, in request order
	for i, entry := range entries {
		id, version, isSelf, err := parseRosterEntry(entry)
		if err != nil {
			return nil, errInvalid("multiagent.agents[%d]: %s", i, err.Error())
		}
		if isSelf || id == selfID {
			if selfSeen {
				return nil, errInvalid("multiagent.agents[%d]: at most one self entry", i)
			}
			selfSeen = true
			// An explicit own-id reference may carry the version a GET
			// renders for self (the one being superseded) or the one this
			// write produces; any other is not this coordinator's self.
			if version != 0 && version != selfVersion && version != selfVersion-1 {
				return nil, errInvalid(`multiagent.agents[%d]: agent %s version %d is not this coordinator's current version; use {"type":"self"}`, i, id, version)
			}
			id, version = selfID, selfVersion
		} else {
			ids = append(ids, id)
		}
		if seen[id] {
			return nil, errInvalid("multiagent.agents[%d]: agent %s is referenced more than once", i, id)
		}
		seen[id] = true
		refs[i] = rosterRef{ID: id, Type: "agent", Version: version}
	}

	// One statement locks every member row, in id order so that two rosters
	// naming the same members lock them alike. The lock is on the agent row:
	// agent_versions rows are immutable, and what a concurrent writer could
	// change is the archive state.
	type agentRow struct {
		current  int64
		archived bool
	}
	members := map[string]agentRow{}
	rows, err := tx.Query(ctx,
		`SELECT id, version, archived_at FROM agents WHERE id = ANY($1) ORDER BY id FOR SHARE`, ids)
	if err != nil {
		return nil, rosterLockErr(err)
	}
	for rows.Next() {
		var (
			id         string
			current    int64
			archivedAt *time.Time
		)
		if err := rows.Scan(&id, &current, &archivedAt); err != nil {
			rows.Close()
			return nil, err
		}
		members[id] = agentRow{current: current, archived: archivedAt != nil}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, rosterLockErr(err)
	}
	agentIDs, versions := make([]string, 0, len(ids)), make([]int64, 0, len(ids))
	for i := range refs {
		if refs[i].ID == selfID {
			continue
		}
		m, ok := members[refs[i].ID]
		if !ok {
			return nil, errInvalid("multiagent.agents[%d]: agent %s not found", i, refs[i].ID)
		}
		if m.archived {
			return nil, errInvalid("multiagent.agents[%d]: agent %s is archived", i, refs[i].ID)
		}
		if refs[i].Version == 0 {
			refs[i].Version = m.current
		}
		agentIDs, versions = append(agentIDs, refs[i].ID), append(versions, refs[i].Version)
	}

	// The pinned versions exist, and none carries a roster (depth limit 1):
	// one statement reads just the `multiagent` key of each.
	found, nested := map[string]bool{}, map[string]bool{}
	rows, err = tx.Query(ctx,
		`SELECT v.agent_id, v.spec->'multiagent'
		   FROM agent_versions v
		   JOIN unnest($1::text[], $2::bigint[]) AS p(agent_id, version)
		     ON p.agent_id = v.agent_id AND p.version = v.version`, agentIDs, versions)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var (
			id string
			ma []byte
		)
		if err := rows.Scan(&id, &ma); err != nil {
			rows.Close()
			return nil, err
		}
		found[id] = true
		nested[id] = present(ma)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range refs {
		if refs[i].ID == selfID {
			continue
		}
		if !found[refs[i].ID] {
			return nil, errInvalid("multiagent.agents[%d]: agent %s version %d not found", i, refs[i].ID, refs[i].Version)
		}
		if nested[refs[i].ID] {
			return nil, errInvalid("multiagent.agents[%d]: agent %s is itself a coordinator (depth limit 1)", i, refs[i].ID)
		}
	}
	return json.Marshal(storedRoster{Type: "coordinator", Agents: refs})
}

// rosterLockErr maps the one failure the member locks can add — two
// coordinators' updates naming each other deadlock on the row locks, and
// Postgres aborts one (SQLSTATE 40P01) — to a 409 the client retries, as
// the other concurrent-write sites do; anything else is the caller's error.
func rosterLockErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "40P01" {
		return errConflict("an agent the roster references is being updated concurrently; retry")
	}
	return err
}

// repinSelf moves a stored roster's `self` entry — the one naming the
// coordinator (selfID) at the version being superseded (from) — to the version
// an update that kept the roster produces (to). A roster without one, or no
// roster, is returned unchanged.
func repinSelf(stored json.RawMessage, selfID string, from, to int64) (json.RawMessage, error) {
	if !present(stored) {
		return stored, nil
	}
	return rewriteMembers(stored, func(m map[string]json.RawMessage) (any, error) {
		if !memberIs(m, selfID, from) {
			return nil, nil
		}
		m["version"] = mustJSON(to)
		return m, nil
	})
}

// rewriteMembers applies fn to every member of a stored roster (either
// shape), each decoded as a key → raw-value map so that whatever fn leaves
// alone survives byte for byte; fn returns the member to write back, or nil
// to keep it as stored.
func rewriteMembers(stored json.RawMessage, fn func(m map[string]json.RawMessage) (any, error)) (json.RawMessage, error) {
	var roster rawRoster
	if err := json.Unmarshal(stored, &roster); err != nil {
		return nil, fmt.Errorf("decode stored roster: %w", err)
	}
	for i, raw := range roster.Agents {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("decode stored roster member: %w", err)
		}
		if m == nil {
			return nil, errors.New("decode stored roster member: not an object")
		}
		out, err := fn(m)
		if err != nil {
			return nil, err
		}
		if out == nil {
			continue
		}
		if roster.Agents[i], err = json.Marshal(out); err != nil {
			return nil, err
		}
	}
	return json.Marshal(roster)
}

// memberIs reports whether a roster member (either shape) names id at version.
func memberIs(m map[string]json.RawMessage, id string, version int64) bool {
	var (
		mid string
		mv  int64
	)
	return json.Unmarshal(m["id"], &mid) == nil && json.Unmarshal(m["version"], &mv) == nil &&
		mid == id && mv == version
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
// "pin the current version" (an explicit null reads as omitted, as session
// create's agent.version does).
func parseRosterEntry(raw json.RawMessage) (id string, version int64, isSelf bool, err error) {
	shapeErr := errors.New(`entry must be an agent id string, {"type":"agent","id",…} or {"type":"self"}`)
	if isNull(raw) {
		return "", 0, false, shapeErr
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, 0, false, checkAgentID(s)
	}
	obj, err := asObjectRaw(raw)
	if err != nil {
		return "", 0, false, shapeErr
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
		id, err := requiredString(obj, "id")
		if err != nil {
			return "", 0, false, err
		}
		if err := checkAgentID(id); err != nil {
			return "", 0, false, err
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

// checkAgentID rejects a member id on shape before it reaches a bind
// parameter (the vault_ids precedent): a value that is not an agent_ id can
// never name a stored agent.
func checkAgentID(id string) error {
	if id == "" {
		return errors.New("agent id must not be empty")
	}
	if !domain.ID(id).HasPrefix(domain.PrefixAgent) || !domain.ID(id).Valid() {
		return fmt.Errorf("%q is not an agent id", id)
	}
	return nil
}

// snapshotRoster builds the session-side roster from a stored one: every
// member's definition is fetched from its pinned version, and the `self`
// member — the entry naming the coordinator at the version the session
// resolved — is `self`'s own resolved spec, overrides applied. Members are not
// re-checked for archive state here: the roster was validated when it was
// written and pins immutable versions (INFERRED, docs/DIVERGENCES.md). Each
// member does answer to the whole-spec caps, as resolveAgent's rule for the
// coordinator says: a pre-cap stored spec fails here, at create.
func snapshotRoster(ctx context.Context, db querier, stored json.RawMessage, self sessionAgentJSON) (json.RawMessage, error) {
	var roster storedRoster
	if err := json.Unmarshal(stored, &roster); err != nil {
		return nil, fmt.Errorf("decode stored roster: %w", err)
	}
	// One statement fetches every pinned member definition; the loop below
	// keeps the roster's order. Members are distinct, so the map keys by id.
	type memberRow struct {
		name string
		spec domain.AgentSpec
	}
	agentIDs, versions := make([]string, 0, len(roster.Agents)), make([]int64, 0, len(roster.Agents))
	for _, ref := range roster.Agents {
		if ref.ID != self.ID.String() || ref.Version != self.Version {
			agentIDs, versions = append(agentIDs, ref.ID), append(versions, ref.Version)
		}
	}
	members := map[string]memberRow{}
	rows, err := db.Query(ctx,
		`SELECT v.agent_id, v.name, v.spec
		   FROM agent_versions v
		   JOIN unnest($1::text[], $2::bigint[]) AS p(agent_id, version)
		     ON p.agent_id = v.agent_id AND p.version = v.version`, agentIDs, versions)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var (
			id, name string
			specJSON []byte
			spec     domain.AgentSpec
		)
		if err := rows.Scan(&id, &name, &specJSON); err != nil {
			rows.Close()
			return nil, err
		}
		if err := json.Unmarshal(specJSON, &spec); err != nil {
			rows.Close()
			return nil, fmt.Errorf("decode stored agent spec: %w", err)
		}
		members[id] = memberRow{name: name, spec: spec}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := sessionRoster{Type: "coordinator", Agents: make([]threadAgentJSON, 0, len(roster.Agents))}
	for _, ref := range roster.Agents {
		if ref.ID == self.ID.String() && ref.Version == self.Version {
			out.Agents = append(out.Agents, selfMember(self))
			continue
		}
		m, ok := members[ref.ID]
		if !ok {
			// A pinned version cannot vanish while its agent exists; only a
			// deleted agent (no such route) could get here.
			return nil, errInvalid("multiagent member %s version %d not found", ref.ID, ref.Version)
		}
		m.spec.Normalize()
		if err := validateAgentSpec(m.spec); err != nil {
			return nil, errInvalid("multiagent member %s: %s", ref.ID, err.Error())
		}
		out.Agents = append(out.Agents, threadAgentOf(ref.ID, ref.Version, m.name, m.spec))
	}
	// A roster addresses its members by name and by nothing else: create_agent
	// takes an agent_name, the platform surfaces the roster to the model
	// nowhere, and so a second member sharing a name is a member no coordinator
	// can ever spawn — the settlement takes the first match and the other is
	// unreachable for the life of the session. Two agents may share a name (the
	// resource has no unique constraint and needs none); putting both on one
	// roster is what cannot work, so it is refused here, where the names are
	// the pinned ones the coordinator will actually address rather than
	// whatever the agents are called now. The self member counts: it is on the
	// roster and answers to a name like any other.
	byName := map[string]int{}
	for i, m := range out.Agents {
		if first, dup := byName[m.Name]; dup {
			return nil, errInvalid(
				"multiagent.agents[%d]: agent %s is named %q, like agent %s at index %d; "+
					"a coordinator spawns a member by name, so two members cannot share one",
				i, m.ID, m.Name, out.Agents[first].ID, first)
		}
		byName[m.Name] = i
	}
	return json.Marshal(out)
}

// patchSelfMember rewrites the `self` member of a session-side roster after a
// session update changed the coordinator's tools or mcp_servers, keeping the
// documented rule — the `self` copy is the coordinator's resolved spec for this
// session — true after the update as it was at create. A snapshot without a
// self member (or without a roster) is returned unchanged.
func patchSelfMember(stored json.RawMessage, self sessionAgentJSON) (json.RawMessage, error) {
	if !present(stored) {
		return stored, nil
	}
	return rewriteMembers(stored, func(m map[string]json.RawMessage) (any, error) {
		if !memberIs(m, self.ID.String(), self.Version) {
			return nil, nil
		}
		return selfMember(self), nil
	})
}

// materializeRoster resolves the toolset configuration inside every member of
// a session-side roster, the rule renderSession applies to the coordinator's
// own tools[]. It never fails: a roster it cannot decode ships as stored.
func materializeRoster(stored json.RawMessage) json.RawMessage {
	if !present(stored) {
		return stored
	}
	out, err := rewriteMembers(stored, func(m map[string]json.RawMessage) (any, error) {
		var tools []json.RawMessage
		if raw, ok := m["tools"]; !ok || json.Unmarshal(raw, &tools) != nil {
			return nil, nil
		}
		m["tools"] = mustJSON(toolset.MaterializeTools(tools))
		return m, nil
	})
	if err != nil {
		return stored
	}
	return out
}
