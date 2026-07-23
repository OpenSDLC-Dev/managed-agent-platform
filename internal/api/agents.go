package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// agentJSON is the BetaManagedAgentsAgent wire shape: every field is
// api:"required" and always rendered.
type agentJSON struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Version int64  `json:"version"`

	domain.AgentSpec

	Metadata   map[string]string `json:"metadata"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
	ArchivedAt *time.Time        `json:"archived_at"`
}

func renderAgent(id, name string, version int64, spec agentSpec, metadata map[string]string,
	createdAt, updatedAt time.Time, archivedAt *time.Time) agentJSON {
	spec.Normalize()
	if metadata == nil {
		metadata = map[string]string{}
	}
	return agentJSON{
		ID: id, Type: "agent", Name: name, Version: version, AgentSpec: spec,
		Metadata: metadata, CreatedAt: createdAt.UTC(), UpdatedAt: updatedAt.UTC(),
		ArchivedAt: utcPtr(archivedAt),
	}
}

// parseAgentSpecFields reads the spec-shaped fields shared by create and
// update bodies into spec, tracking which keys were present.
func parseAgentSpecFields(obj map[string]json.RawMessage, spec *agentSpec) error {
	if raw, ok := obj["multiagent"]; ok && !isNull(raw) {
		return errInvalid("multiagent is not supported yet")
	}
	if raw, ok := obj["model"]; ok {
		if isNull(raw) {
			return errInvalid("model cannot be cleared")
		}
		m, err := parseModel(raw)
		if err != nil {
			return err
		}
		spec.Model = m
	}
	for key, dst := range map[string]*string{"system": &spec.System, "description": &spec.Description} {
		val, set, null, err := stringField(obj, key)
		if err != nil {
			return err
		}
		if set {
			if null {
				val = ""
			}
			*dst = val
		}
	}
	if raw, ok := obj["tools"]; ok {
		items, err := parseTools(raw)
		if err != nil {
			return err
		}
		spec.Tools = items
	}
	if raw, ok := obj["mcp_servers"]; ok {
		items, err := parseMCPServers(raw)
		if err != nil {
			return err
		}
		spec.MCPServers = items
	}
	if raw, ok := obj["skills"]; ok {
		items, err := parseSkills(raw)
		if err != nil {
			return err
		}
		spec.Skills = items
	}
	return nil
}

func (s *server) createAgent(r *http.Request) (any, error) {
	ctx := r.Context()
	obj, err := decodeObject(r)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(obj, "name", "model", "system", "description",
		"tools", "mcp_servers", "skills", "metadata", "multiagent"); err != nil {
		return nil, err
	}
	name, err := requiredString(obj, "name")
	if err != nil {
		return nil, err
	}
	if raw, ok := obj["model"]; !ok || isNull(raw) {
		return nil, errInvalid("model is required")
	}
	var spec agentSpec
	if err := parseAgentSpecFields(obj, &spec); err != nil {
		return nil, err
	}
	spec.Normalize()
	metadata, err := parseMetadata(obj)
	if err != nil {
		return nil, err
	}

	specJSON, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	id := domain.NewID(domain.PrefixAgent).String()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var createdAt, updatedAt time.Time
	if err := tx.QueryRow(ctx,
		`INSERT INTO agents (id, name, version, spec, metadata)
		 VALUES ($1, $2, 1, $3, $4) RETURNING created_at, updated_at`,
		id, name, specJSON, metadata).Scan(&createdAt, &updatedAt); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO agent_versions (agent_id, version, name, spec) VALUES ($1, 1, $2, $3)`,
		id, name, specJSON); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return renderAgent(id, name, 1, spec, metadata, createdAt, updatedAt, nil), nil
}

func (s *server) getAgent(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkID(id, "agent"); err != nil {
		return nil, err
	}
	if v := r.URL.Query().Get("version"); v != "" {
		version, err := strconv.ParseInt(v, 10, 64)
		if err != nil || version < 1 {
			return nil, errInvalid("version must be a positive integer")
		}
		return s.getAgentVersion(ctx, id, version)
	}

	var (
		name                 string
		version              int64
		specJSON, metaJSON   []byte
		createdAt, updatedAt time.Time
		archivedAt           *time.Time
	)
	err := s.pool.QueryRow(ctx,
		`SELECT name, version, spec, metadata, created_at, updated_at, archived_at
		 FROM agents WHERE id = $1`, id).
		Scan(&name, &version, &specJSON, &metaJSON, &createdAt, &updatedAt, &archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("agent %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	spec, metadata, err := decodeSpecAndMetadata(specJSON, metaJSON)
	if err != nil {
		return nil, err
	}
	return renderAgent(id, name, version, spec, metadata, createdAt, updatedAt, archivedAt), nil
}

// getAgentVersion renders a pinned immutable snapshot. The snapshot's
// created_at doubles as updated_at: that is when this version came to exist
// and it never changed afterwards.
func (s *server) getAgentVersion(ctx context.Context, id string, version int64) (any, error) {
	var (
		name               string
		specJSON, metaJSON []byte
		createdAt, vAt     time.Time
		archivedAt         *time.Time
	)
	err := s.pool.QueryRow(ctx,
		`SELECT v.name, v.spec, a.metadata, a.created_at, v.created_at, a.archived_at
		 FROM agents a JOIN agent_versions v ON v.agent_id = a.id
		 WHERE a.id = $1 AND v.version = $2`, id, version).
		Scan(&name, &specJSON, &metaJSON, &createdAt, &vAt, &archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("agent %s version %d not found", id, version)
	}
	if err != nil {
		return nil, err
	}
	spec, metadata, err := decodeSpecAndMetadata(specJSON, metaJSON)
	if err != nil {
		return nil, err
	}
	return renderAgent(id, name, version, spec, metadata, createdAt, vAt, archivedAt), nil
}

func decodeSpecAndMetadata(specJSON, metaJSON []byte) (agentSpec, map[string]string, error) {
	var spec agentSpec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return spec, nil, fmt.Errorf("decode stored agent spec: %w", err)
	}
	metadata := map[string]string{}
	if err := json.Unmarshal(metaJSON, &metadata); err != nil {
		return spec, nil, fmt.Errorf("decode stored metadata: %w", err)
	}
	return spec, metadata, nil
}

func (s *server) updateAgent(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	obj, err := decodeObject(r)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(obj, "version", "name", "model", "system", "description",
		"tools", "mcp_servers", "skills", "metadata", "multiagent"); err != nil {
		return nil, err
	}
	// version is the optimistic-concurrency check, and it is opt-in: supplied, the
	// update must match the stored version (which is at least 1); *omitted*, the
	// update applies unconditionally. An explicit null is not omission — the wire
	// types the field as an integer, so accepting null would silently drop the
	// check for a client that serialized a nil pointer.
	var expected *int64
	if raw, ok := obj["version"]; ok {
		var v int64
		if isNull(raw) {
			return nil, errInvalid("version must be an integer")
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, errInvalid("version must be an integer")
		}
		if v < 1 {
			return nil, errInvalid("version must be at least 1")
		}
		expected = &v
	}
	if err := checkID(id, "agent"); err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		name                 string
		current              int64
		specJSON, metaJSON   []byte
		createdAt, updatedAt time.Time
		archivedAt           *time.Time
	)
	err = tx.QueryRow(ctx,
		`SELECT name, version, spec, metadata, created_at, updated_at, archived_at
		 FROM agents WHERE id = $1 FOR UPDATE`, id).
		Scan(&name, &current, &specJSON, &metaJSON, &createdAt, &updatedAt, &archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("agent %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	if archivedAt != nil {
		return nil, errInvalid("agent %s is archived", id)
	}
	if expected != nil && *expected != current {
		return nil, errConflict("agent version mismatch: expected %d, currently %d", *expected, current)
	}

	spec, metadata, err := decodeSpecAndMetadata(specJSON, metaJSON)
	if err != nil {
		return nil, err
	}
	newName, set, null, err := stringField(obj, "name")
	if err != nil {
		return nil, err
	}
	if set {
		if null || newName == "" {
			return nil, errInvalid("name cannot be cleared")
		}
		name = newName
	}
	if err := parseAgentSpecFields(obj, &spec); err != nil {
		return nil, err
	}
	spec.Normalize()
	if raw, ok := obj["metadata"]; ok {
		metadata, err = patchMetadata(metadata, raw, false)
		if err != nil {
			return nil, err
		}
	}

	newSpecJSON, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	newVersion := current + 1
	if err := tx.QueryRow(ctx,
		`UPDATE agents SET name = $2, version = $3, spec = $4, metadata = $5, updated_at = now()
		 WHERE id = $1 RETURNING updated_at`,
		id, name, newVersion, newSpecJSON, metadata).Scan(&updatedAt); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO agent_versions (agent_id, version, name, spec) VALUES ($1, $2, $3, $4)`,
		id, newVersion, name, newSpecJSON); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return renderAgent(id, name, newVersion, spec, metadata, createdAt, updatedAt, archivedAt), nil
}

func (s *server) listAgents(r *http.Request) (any, error) {
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
	gte, err := parseTimeParam(q, "created_at[gte]")
	if err != nil {
		return nil, err
	}
	lte, err := parseTimeParam(q, "created_at[lte]")
	if err != nil {
		return nil, err
	}

	query := `SELECT id, name, version, spec, metadata, created_at, updated_at, archived_at FROM agents WHERE true`
	var args []any
	if !includeArchived {
		query += ` AND archived_at IS NULL`
	}
	if gte != nil {
		args = append(args, *gte)
		query += fmt.Sprintf(` AND created_at >= $%d`, len(args))
	}
	if lte != nil {
		args = append(args, *lte)
		query += fmt.Sprintf(` AND created_at <= $%d`, len(args))
	}
	if page.cur != nil {
		// Unidirectional list: only forward time cursors are valid here.
		if page.cur.versioned || page.cur.dir != dirNext {
			return nil, errInvalid("invalid page cursor")
		}
		args = append(args, page.cur.t, page.cur.id)
		query += fmt.Sprintf(` AND (created_at, id) < ($%d, $%d)`, len(args)-1, len(args))
	}
	args = append(args, page.limit+1)
	query += fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d`, len(args))

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var data []any
	var lastT time.Time
	var lastID string
	fetched := 0
	for rows.Next() {
		fetched++
		if fetched > page.limit {
			break
		}
		var (
			id, name             string
			version              int64
			specJSON, metaJSON   []byte
			createdAt, updatedAt time.Time
			archivedAt           *time.Time
		)
		if err := rows.Scan(&id, &name, &version, &specJSON, &metaJSON, &createdAt, &updatedAt, &archivedAt); err != nil {
			return nil, err
		}
		spec, metadata, err := decodeSpecAndMetadata(specJSON, metaJSON)
		if err != nil {
			return nil, err
		}
		data = append(data, renderAgent(id, name, version, spec, metadata, createdAt, updatedAt, archivedAt))
		lastT, lastID = createdAt, id
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := pageJSON{Data: data}
	if out.Data == nil {
		out.Data = []any{}
	}
	if fetched > page.limit {
		c := encodeTimeCursor(dirNext, lastT, lastID)
		out.NextPage = &c
	}
	return out, nil
}

func (s *server) listAgentVersions(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkID(id, "agent"); err != nil {
		return nil, err
	}
	page, err := parsePage(r.URL.Query())
	if err != nil {
		return nil, err
	}

	var (
		metaJSON   []byte
		createdAt  time.Time
		archivedAt *time.Time
	)
	err = s.pool.QueryRow(ctx,
		`SELECT metadata, created_at, archived_at FROM agents WHERE id = $1`, id).
		Scan(&metaJSON, &createdAt, &archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("agent %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	metadata := map[string]string{}
	if err := json.Unmarshal(metaJSON, &metadata); err != nil {
		return nil, err
	}

	query := `SELECT version, name, spec, created_at FROM agent_versions WHERE agent_id = $1`
	args := []any{id}
	if page.cur != nil {
		// Version lists paginate on the version number itself.
		if !page.cur.versioned || page.cur.dir != dirNext {
			return nil, errInvalid("invalid page cursor")
		}
		args = append(args, page.cur.version)
		query += fmt.Sprintf(` AND version < $%d`, len(args))
	}
	args = append(args, page.limit+1)
	query += fmt.Sprintf(` ORDER BY version DESC LIMIT $%d`, len(args))

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var data []any
	var lastVersion int64
	fetched := 0
	for rows.Next() {
		fetched++
		if fetched > page.limit {
			break
		}
		var (
			version  int64
			name     string
			specJSON []byte
			vAt      time.Time
		)
		if err := rows.Scan(&version, &name, &specJSON, &vAt); err != nil {
			return nil, err
		}
		var spec agentSpec
		if err := json.Unmarshal(specJSON, &spec); err != nil {
			return nil, err
		}
		data = append(data, renderAgent(id, name, version, spec, metadata, createdAt, vAt, archivedAt))
		lastVersion = version
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := pageJSON{Data: data}
	if out.Data == nil {
		out.Data = []any{}
	}
	if fetched > page.limit {
		c := encodeVersionCursor(lastVersion)
		out.NextPage = &c
	}
	return out, nil
}

func (s *server) archiveAgent(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkID(id, "agent"); err != nil {
		return nil, err
	}
	var (
		name                 string
		version              int64
		specJSON, metaJSON   []byte
		createdAt, updatedAt time.Time
		archivedAt           *time.Time
	)
	// Idempotent: the first archive stamps archived_at; later calls change
	// nothing (all SET expressions read the pre-update row).
	err := s.pool.QueryRow(ctx,
		`UPDATE agents SET
		   updated_at  = CASE WHEN archived_at IS NULL THEN now() ELSE updated_at END,
		   archived_at = COALESCE(archived_at, now())
		 WHERE id = $1
		 RETURNING name, version, spec, metadata, created_at, updated_at, archived_at`, id).
		Scan(&name, &version, &specJSON, &metaJSON, &createdAt, &updatedAt, &archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("agent %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	spec, metadata, err := decodeSpecAndMetadata(specJSON, metaJSON)
	if err != nil {
		return nil, err
	}
	return renderAgent(id, name, version, spec, metadata, createdAt, updatedAt, archivedAt), nil
}
