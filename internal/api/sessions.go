package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// sessionAgentJSON is the resolved-agent snapshot embedded in a session
// (BetaManagedAgentsSessionAgent) — the domain wire shape, stored verbatim
// in sessions.resolved_agent, so rendering is a passthrough.
type sessionAgentJSON = domain.ResolvedAgent

// usageJSON is the session-level usage wire shape — the domain type (nested
// cache_creation, unlike the event-level usage on span.model_request_end).
type usageJSON = domain.Usage

type statsJSON struct {
	ActiveSeconds   float64 `json:"active_seconds"`
	DurationSeconds float64 `json:"duration_seconds"`
}

// sessionJSON is the BetaManagedAgentsSession wire shape.
type sessionJSON struct {
	ID                 string            `json:"id"`
	Type               string            `json:"type"` // "session"
	Agent              sessionAgentJSON  `json:"agent"`
	EnvironmentID      string            `json:"environment_id"`
	Status             string            `json:"status"`
	Title              string            `json:"title"`
	Metadata           map[string]string `json:"metadata"`
	Usage              usageJSON         `json:"usage"`
	Stats              statsJSON         `json:"stats"`
	OutcomeEvaluations []json.RawMessage `json:"outcome_evaluations"`
	Resources          []json.RawMessage `json:"resources"`
	VaultIDs           []string          `json:"vault_ids"`
	DeploymentID       *string           `json:"deployment_id"` // deployments are post-v1: always null
	CreatedAt          time.Time         `json:"created_at"`
	UpdatedAt          time.Time         `json:"updated_at"`
	ArchivedAt         *time.Time        `json:"archived_at"`
}

// normalizeSessionID maps the alternate wire spelling session_… onto the
// canonical sesn_… form we store.
func normalizeSessionID(id string) string {
	if rest, ok := strings.CutPrefix(id, "session_"); ok {
		return "sesn_" + rest
	}
	return id
}

// sessionRow carries one sessions row through scan → render.
type sessionRow struct {
	id                   string
	agentJSON            []byte
	environmentID        string
	status, title        string
	metaJSON, usageJSON  []byte
	resourcesJSON        []byte
	vaultIDs             []string
	createdAt, updatedAt time.Time
	archivedAt           *time.Time
}

const sessionColumns = `id, resolved_agent, environment_id, status, title,
	metadata, usage, resources, vault_ids, created_at, updated_at, archived_at`

func scanSession(row pgx.Row) (sessionRow, error) {
	var r sessionRow
	err := row.Scan(&r.id, &r.agentJSON, &r.environmentID, &r.status, &r.title,
		&r.metaJSON, &r.usageJSON, &r.resourcesJSON, &r.vaultIDs,
		&r.createdAt, &r.updatedAt, &r.archivedAt)
	return r, err
}

func renderSession(r sessionRow) (sessionJSON, error) {
	var agent sessionAgentJSON
	if err := json.Unmarshal(r.agentJSON, &agent); err != nil {
		return sessionJSON{}, fmt.Errorf("decode stored resolved agent: %w", err)
	}
	if agent.Tools == nil {
		agent.Tools = []json.RawMessage{}
	}
	if agent.MCPServers == nil {
		agent.MCPServers = []json.RawMessage{}
	}
	if agent.Skills == nil {
		agent.Skills = []json.RawMessage{}
	}
	metadata := map[string]string{}
	if err := json.Unmarshal(r.metaJSON, &metadata); err != nil {
		return sessionJSON{}, err
	}
	var usage usageJSON
	if err := json.Unmarshal(r.usageJSON, &usage); err != nil {
		return sessionJSON{}, err
	}
	resources := []json.RawMessage{}
	if err := json.Unmarshal(r.resourcesJSON, &resources); err != nil {
		return sessionJSON{}, err
	}
	if resources == nil {
		resources = []json.RawMessage{}
	}
	if r.vaultIDs == nil {
		r.vaultIDs = []string{}
	}
	return sessionJSON{
		ID: r.id, Type: "session", Agent: agent, EnvironmentID: r.environmentID,
		Status: r.status, Title: r.title, Metadata: metadata, Usage: usage,
		Stats: statsJSON{}, OutcomeEvaluations: []json.RawMessage{},
		Resources: resources, VaultIDs: r.vaultIDs,
		CreatedAt: r.createdAt.UTC(), UpdatedAt: r.updatedAt.UTC(), ArchivedAt: utcPtr(r.archivedAt),
	}, nil
}

// querier is the read surface shared by pgxpool.Pool and pgx.Tx, letting
// resolution run inside the caller's transaction.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// resolveAgent resolves the create-time agent union (plain id string,
// {type:"agent"}, or {type:"agent_with_overrides"}) into the immutable
// snapshot the session will carry.
func (s *server) resolveAgent(ctx context.Context, db querier, raw json.RawMessage) (sessionAgentJSON, error) {
	var snap sessionAgentJSON

	var agentID string
	var version int64 // 0 = latest
	overrides := map[string]json.RawMessage{}

	if err := json.Unmarshal(raw, &agentID); err != nil {
		var obj struct {
			Type    string          `json:"type"`
			ID      string          `json:"id"`
			Version *int64          `json:"version"`
			Model   json.RawMessage `json:"model"`
			System  json.RawMessage `json:"system"`
			Tools   json.RawMessage `json:"tools"`
			MCP     json.RawMessage `json:"mcp_servers"`
			Skills  json.RawMessage `json:"skills"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			return snap, errInvalid("agent must be an agent id string or an agent reference object")
		}
		if obj.ID == "" {
			return snap, errInvalid("agent.id is required")
		}
		switch obj.Type {
		case "agent":
		case "agent_with_overrides":
			for key, val := range map[string]json.RawMessage{
				"model": obj.Model, "system": obj.System, "tools": obj.Tools,
				"mcp_servers": obj.MCP, "skills": obj.Skills,
			} {
				if len(val) > 0 {
					// Only system documents null semantics ("set to null to
					// clear the agent's system prompt").
					if isNull(val) && key != "system" {
						return snap, errInvalid("agent override %s cannot be null", key)
					}
					overrides[key] = val
				}
			}
		default:
			return snap, errInvalid(`agent.type must be "agent" or "agent_with_overrides"`)
		}
		agentID = obj.ID
		if obj.Version != nil {
			// Explicit versions must be >= 1; only omission means "latest".
			if *obj.Version < 1 {
				return snap, errInvalid("agent.version must be a positive integer")
			}
			version = *obj.Version
		}
	}

	var (
		name       string
		specJSON   []byte
		archivedAt *time.Time
	)
	if version == 0 {
		err := db.QueryRow(ctx,
			`SELECT name, version, spec, archived_at FROM agents WHERE id = $1`, agentID).
			Scan(&name, &version, &specJSON, &archivedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return snap, errNotFound("agent %s not found", agentID)
		}
		if err != nil {
			return snap, err
		}
	} else {
		err := db.QueryRow(ctx,
			`SELECT v.name, v.spec, a.archived_at
			 FROM agent_versions v JOIN agents a ON a.id = v.agent_id
			 WHERE v.agent_id = $1 AND v.version = $2`, agentID, version).
			Scan(&name, &specJSON, &archivedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return snap, errNotFound("agent %s version %d not found", agentID, version)
		}
		if err != nil {
			return snap, err
		}
	}
	if archivedAt != nil {
		return snap, errInvalid("agent %s is archived", agentID)
	}

	var spec agentSpec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return snap, fmt.Errorf("decode stored agent spec: %w", err)
	}
	if raw, ok := overrides["model"]; ok {
		m, err := parseModel(raw)
		if err != nil {
			return snap, err
		}
		spec.Model = m
	}
	if raw, ok := overrides["system"]; ok {
		if isNull(raw) {
			spec.System = "" // null clears the agent's system prompt
		} else if err := json.Unmarshal(raw, &spec.System); err != nil {
			return snap, errInvalid("agent override system must be a string")
		}
	}
	if raw, ok := overrides["tools"]; ok {
		items, err := parseTools(raw)
		if err != nil {
			return snap, err
		}
		spec.Tools = items
	}
	if raw, ok := overrides["mcp_servers"]; ok {
		items, err := parseMCPServers(raw)
		if err != nil {
			return snap, err
		}
		spec.MCPServers = items
	}
	if raw, ok := overrides["skills"]; ok {
		items, err := parseSkills(raw)
		if err != nil {
			return snap, err
		}
		spec.Skills = items
	}
	spec.Normalize()

	return sessionAgentJSON{
		Type: "agent", ID: domain.ID(agentID), Version: version, Name: name,
		AgentSpec: spec,
	}, nil
}

// rejectUnsupportedList returns a wire error when a post-v1 feature list is
// present and non-empty (empty lists are accepted as no-ops).
func rejectUnsupportedList(obj map[string]json.RawMessage, key, feature string) error {
	raw, ok := obj[key]
	if !ok || isNull(raw) {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return errInvalid("%s must be an array", key)
	}
	if len(items) > 0 {
		return errInvalid("%s are not supported yet", feature)
	}
	return nil
}

func (s *server) createSession(r *http.Request) (any, error) {
	ctx := r.Context()
	obj, err := decodeObject(r)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(obj, "agent", "environment_id", "title", "metadata",
		"resources", "vault_ids"); err != nil {
		return nil, err
	}
	envID, err := requiredString(obj, "environment_id")
	if err != nil {
		return nil, err
	}
	agentRaw, ok := obj["agent"]
	if !ok || isNull(agentRaw) {
		return nil, errInvalid("agent is required")
	}
	if err := rejectUnsupportedList(obj, "resources", "session resources"); err != nil {
		return nil, err
	}
	if err := rejectUnsupportedList(obj, "vault_ids", "vaults"); err != nil {
		return nil, err
	}
	title, _, null, err := stringField(obj, "title")
	if err != nil {
		return nil, err
	}
	if null {
		title = ""
	}
	metadata, err := parseMetadata(obj)
	if err != nil {
		return nil, err
	}

	// One transaction around check → resolve → insert: FOR SHARE on the
	// environment row blocks a concurrent delete/archive from slipping in
	// between the check and the insert.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var envArchivedAt *time.Time
	err = tx.QueryRow(ctx,
		`SELECT archived_at FROM environments WHERE id = $1 FOR SHARE`, envID).Scan(&envArchivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("environment %s not found", envID)
	}
	if err != nil {
		return nil, err
	}
	if envArchivedAt != nil {
		return nil, errInvalid("environment %s is archived", envID)
	}

	agent, err := s.resolveAgent(ctx, tx, agentRaw)
	if err != nil {
		return nil, err
	}
	agentJSON, err := json.Marshal(agent)
	if err != nil {
		return nil, err
	}

	id := domain.NewID(domain.PrefixSession).String()
	var createdBy *string
	if p := principalFrom(ctx); p != "" {
		createdBy = &p
	}
	row := sessionRow{
		id: id, agentJSON: agentJSON, environmentID: envID,
		status: string(domain.SessionIdle), title: title,
		metaJSON: mustJSON(metadata), usageJSON: []byte(`{}`), resourcesJSON: []byte(`[]`),
	}
	err = tx.QueryRow(ctx,
		`INSERT INTO sessions (id, agent_id, agent_version, resolved_agent, environment_id,
		   status, title, metadata, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 RETURNING created_at, updated_at`,
		id, agent.ID, agent.Version, agentJSON, envID, row.status, title, metadata, createdBy).
		Scan(&row.createdAt, &row.updatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" { // foreign_key_violation backstop
		if strings.Contains(pgErr.ConstraintName, "environment") {
			return nil, errNotFound("environment %s not found", envID)
		}
		return nil, errNotFound("agent %s version %d not found", agent.ID, agent.Version)
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return renderSession(row)
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err) // map[string]string cannot fail to marshal
	}
	return raw
}

// jsonEqual compares two JSON documents by value: stored jsonb comes back
// with Postgres's own spacing, key order, and number formatting, so neither a
// byte comparison nor a literal one can tell a no-op update from a change.
// Numbers are compared as exact rationals — `1e2` equals `100` (Postgres
// rewrites one as the other), while two integers past 2^53 stay distinct
// (float64 would collapse them).
func jsonEqual(a, b []byte) bool {
	x, err := decodeJSONValue(a)
	if err != nil {
		return false
	}
	y, err := decodeJSONValue(b)
	if err != nil {
		return false
	}
	return sameJSON(x, y)
}

func decodeJSONValue(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func sameJSON(a, b any) bool {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !sameJSON(v, w) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !sameJSON(x[i], y[i]) {
				return false
			}
		}
		return true
	case json.Number:
		y, ok := b.(json.Number)
		return ok && numberEqual(x, y)
	default:
		return a == b // string, bool, null
	}
}

func numberEqual(a, b json.Number) bool {
	if a == b {
		return true
	}
	x, xok := new(big.Rat).SetString(a.String())
	y, yok := new(big.Rat).SetString(b.String())
	return xok && yok && x.Cmp(y) == 0
}

func (s *server) getSession(r *http.Request) (any, error) {
	ctx := r.Context()
	id := normalizeSessionID(r.PathValue("id"))
	if err := checkID(id, "session"); err != nil {
		return nil, err
	}
	row, err := scanSession(s.pool.QueryRow(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("session %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	return renderSession(row)
}

func (s *server) updateSession(r *http.Request) (any, error) {
	ctx := r.Context()
	id := normalizeSessionID(r.PathValue("id"))
	obj, err := decodeObject(r)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(obj, "title", "metadata", "agent", "vault_ids"); err != nil {
		return nil, err
	}
	if raw, ok := obj["vault_ids"]; ok && !isNull(raw) {
		// The reference server rejects this too ("Not yet supported").
		return nil, errInvalid("vault_ids updates are not yet supported")
	}
	if err := checkID(id, "session"); err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row, err := scanSession(tx.QueryRow(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("session %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	if row.archivedAt != nil {
		return nil, errInvalid("session %s is archived", id)
	}
	metadata := map[string]string{}
	if err := json.Unmarshal(row.metaJSON, &metadata); err != nil {
		return nil, err
	}

	prevTitle, prevMetaJSON, prevAgentJSON := row.title, row.metaJSON, row.agentJSON

	if title, set, null, err := stringField(obj, "title"); err != nil {
		return nil, err
	} else if set {
		if null {
			title = ""
		}
		row.title = title
	}
	if raw, ok := obj["metadata"]; ok {
		metadata, err = patchMetadata(metadata, raw, false)
		if err != nil {
			return nil, err
		}
		row.metaJSON = mustJSON(metadata)
	}
	if raw, ok := obj["agent"]; ok && !isNull(raw) {
		var patch map[string]json.RawMessage
		if err := json.Unmarshal(raw, &patch); err != nil {
			return nil, errInvalid("agent must be an object")
		}
		var agent sessionAgentJSON
		if err := json.Unmarshal(row.agentJSON, &agent); err != nil {
			return nil, err
		}
		for key, val := range patch {
			switch key {
			case "tools":
				items, err := parseTools(val)
				if err != nil {
					return nil, err
				}
				agent.Tools = items
			case "mcp_servers":
				items, err := parseMCPServers(val)
				if err != nil {
					return nil, err
				}
				agent.MCPServers = items
			default:
				return nil, errInvalid("only agent.tools and agent.mcp_servers can be updated on a session")
			}
		}
		row.agentJSON, err = json.Marshal(agent)
		if err != nil {
			return nil, err
		}
	}

	if err := tx.QueryRow(ctx,
		`UPDATE sessions SET resolved_agent = $2, title = $3, metadata = $4, updated_at = now()
		 WHERE id = $1 RETURNING updated_at`,
		id, row.agentJSON, row.title, row.metaJSON).Scan(&row.updatedAt); err != nil {
		return nil, err
	}

	// session.updated is emitted only when the update changed at least one
	// field, and carries only the changed fields (metadata additionally only
	// when the new value is non-empty) — the SDK-documented shape. Appended
	// in the same transaction as the update itself. Change detection is
	// semantic, not byte-wise: the previous values come back jsonb-normalized
	// (Postgres's spacing and key order), which never byte-matches a fresh Go
	// marshal even for identical content.
	metaChanged := !jsonEqual(row.metaJSON, prevMetaJSON)
	agentChanged := !jsonEqual(row.agentJSON, prevAgentJSON)
	payload := map[string]any{}
	if row.title != prevTitle {
		payload["title"] = row.title
	}
	if metaChanged && len(metadata) > 0 {
		payload["metadata"] = metadata
	}
	if agentChanged {
		payload["agent"] = json.RawMessage(row.agentJSON)
	}
	changed := row.title != prevTitle || metaChanged || agentChanged
	if changed {
		if _, err := s.log.AppendInTx(ctx, tx, domain.ID(id), []events.NewEvent{
			{Type: domain.EventSessionUpdated, Payload: mustJSON(payload)},
		}, events.AppendOptions{}); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return renderSession(row)
}

var validSessionStatuses = map[string]bool{
	string(domain.SessionIdle): true, string(domain.SessionRunning): true,
	string(domain.SessionRescheduling): true, string(domain.SessionTerminated): true,
}

func (s *server) listSessions(r *http.Request) (any, error) {
	ctx := r.Context()
	q := r.URL.Query()
	page, err := parsePage(q)
	if err != nil {
		return nil, err
	}
	includeArchived, err := parseBoolParam(q, "include_archived")
	if err != nil {
		return nil, err
	}
	order := q.Get("order")
	switch order {
	case "":
		order = "desc"
	case "asc", "desc":
	default:
		return nil, errInvalid(`order must be "asc" or "desc"`)
	}
	statuses := append(q["statuses[]"], q["statuses"]...)
	for _, st := range statuses {
		if !validSessionStatuses[st] {
			return nil, errInvalid("invalid session status %q", st)
		}
	}

	// Deployments and memory stores are post-v1 features: no session can
	// reference one, so filtering by them yields an empty result, not an error.
	if q.Get("deployment_id") != "" || q.Get("memory_store_id") != "" {
		return biPageJSON{Data: []any{}}, nil
	}

	query := `SELECT ` + sessionColumns + ` FROM sessions WHERE true`
	var args []any
	if !includeArchived {
		query += ` AND archived_at IS NULL`
	}
	if agentID := q.Get("agent_id"); agentID != "" {
		// A malformed agent_id can never name a stored agent; reject it on shape
		// (a valid but absent one still filters to an empty page) so an unstorable
		// byte does not reach the bind parameter as a 500. See #135.
		if !domain.ID(agentID).Valid() {
			return nil, errInvalid("agent_id must be a valid agent id")
		}
		args = append(args, agentID)
		query += fmt.Sprintf(` AND agent_id = $%d`, len(args))
	}
	if v := q.Get("agent_version"); v != "" {
		version, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, errInvalid("agent_version must be an integer")
		}
		// Per the reference: "Only applies when agent_id is also set" —
		// without agent_id the parameter is ignored, not rejected.
		if q.Get("agent_id") != "" {
			args = append(args, version)
			query += fmt.Sprintf(` AND agent_version = $%d`, len(args))
		}
	}
	if len(statuses) > 0 {
		args = append(args, statuses)
		query += fmt.Sprintf(` AND status = ANY($%d)`, len(args))
	}
	for key, op := range map[string]string{
		"created_at[gt]": ">", "created_at[gte]": ">=",
		"created_at[lt]": "<", "created_at[lte]": "<=",
	} {
		ts, err := parseTimeParam(q, key)
		if err != nil {
			return nil, err
		}
		if ts != nil {
			args = append(args, *ts)
			query += fmt.Sprintf(` AND created_at %s $%d`, op, len(args))
		}
	}
	sortDir := "DESC"
	if order == "asc" {
		sortDir = "ASC"
	}
	orderDir, reversed := sortDir, false
	if page.cur != nil {
		if page.cur.versioned {
			return nil, errInvalid("invalid page cursor")
		}
		var clause string
		clause, orderDir, reversed = keysetClause(sortDir, page.cur, len(args))
		args = append(args, page.cur.t, page.cur.id)
		query += clause
	}
	args = append(args, page.limit+1)
	query += fmt.Sprintf(` ORDER BY created_at %s, id %s LIMIT $%d`, orderDir, orderDir, len(args))

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var data []any
	type rowKey struct {
		t  time.Time
		id string
	}
	var keys []rowKey
	fetched := 0
	for rows.Next() {
		fetched++
		if fetched > page.limit {
			break
		}
		row, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		rendered, err := renderSession(row)
		if err != nil {
			return nil, err
		}
		data = append(data, rendered)
		keys = append(keys, rowKey{row.createdAt, row.id})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if reversed {
		for i, j := 0, len(data)-1; i < j; i, j = i+1, j-1 {
			data[i], data[j] = data[j], data[i]
			keys[i], keys[j] = keys[j], keys[i]
		}
	}
	out := biPageJSON{Data: data}
	if out.Data == nil {
		out.Data = []any{}
	}
	out.NextPage, out.PrevPage = pageEdges(len(data), fetched > page.limit, page.cur != nil, reversed,
		func(i int) (time.Time, string) { return keys[i].t, keys[i].id })
	return out, nil
}

func (s *server) archiveSession(r *http.Request) (any, error) {
	ctx := r.Context()
	id := normalizeSessionID(r.PathValue("id"))
	if err := checkID(id, "session"); err != nil {
		return nil, err
	}
	row, err := scanSession(s.pool.QueryRow(ctx,
		`UPDATE sessions SET
		   updated_at  = CASE WHEN archived_at IS NULL THEN now() ELSE updated_at END,
		   archived_at = COALESCE(archived_at, now())
		 WHERE id = $1 RETURNING `+sessionColumns, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("session %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	return renderSession(row)
}

func (s *server) deleteSession(r *http.Request) (any, error) {
	ctx := r.Context()
	id := normalizeSessionID(r.PathValue("id"))
	if err := checkID(id, "session"); err != nil {
		return nil, err
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, errNotFound("session %s not found", id)
	}
	// The session.deleted event terminates any active event stream. It
	// cannot be persisted — the log rows just cascaded away with the
	// session — so it goes out as an ephemeral broadcast, best-effort:
	// the delete itself has already succeeded.
	_ = s.log.PublishEventFrame(ctx, domain.ID(id), map[string]any{
		"id":           domain.NewID("sevt").String(),
		"type":         "session.deleted",
		"processed_at": time.Now().UTC(),
	})
	return map[string]string{"id": id, "type": "session_deleted"}, nil
}
