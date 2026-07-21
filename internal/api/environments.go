package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// environmentJSON is the BetaEnvironment wire shape. Note: no "state" field —
// the wire expresses lifecycle via archived_at only (the schema's state
// column stays internal).
type environmentJSON struct {
	ID          string            `json:"id"`
	Type        string            `json:"type"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Config      json.RawMessage   `json:"config"`
	Scope       string            `json:"scope"` // single-tenant v1: always "organization"
	Metadata    map[string]string `json:"metadata"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	ArchivedAt  *time.Time        `json:"archived_at"`
}

// Normalized config shapes: responses always carry the full required surface
// (cloud → networking + all six package lists; self_hosted → type only).
type cloudConfigJSON struct {
	Type       string          `json:"type"`
	Networking json.RawMessage `json:"networking"`
	Packages   packagesJSON    `json:"packages"`
}

type packagesJSON struct {
	Apt   []string `json:"apt"`
	Cargo []string `json:"cargo"`
	Gem   []string `json:"gem"`
	Go    []string `json:"go"`
	Npm   []string `json:"npm"`
	Pip   []string `json:"pip"`
}

type limitedNetworkJSON struct {
	Type                 string   `json:"type"`
	AllowedHosts         []string `json:"allowed_hosts"`
	AllowMCPServers      bool     `json:"allow_mcp_servers"`
	AllowPackageManagers bool     `json:"allow_package_managers"`
}

// normalizeEnvConfig validates the config union and produces the stored,
// fully-populated form. existing is the currently stored config when merging
// an update (nil on create): per the reference's update semantics, omitted
// cloud sub-fields preserve their existing values rather than resetting to
// defaults. A type switch (cloud ⇄ self_hosted) starts from defaults — the
// other union arm has nothing to preserve.
func normalizeEnvConfig(raw json.RawMessage, existing []byte) (kind string, normalized []byte, err error) {
	if raw == nil || isNull(raw) {
		raw = []byte(`{"type":"cloud"}`)
	}
	var obj map[string]json.RawMessage
	if e := json.Unmarshal(raw, &obj); e != nil || obj == nil {
		return "", nil, errInvalid("config must be an object")
	}
	var typ string
	if rawType, ok := obj["type"]; ok {
		_ = json.Unmarshal(rawType, &typ)
	}
	switch typ {
	case string(domain.EnvSelfHosted):
		for key := range obj {
			if key != "type" {
				return "", nil, errInvalid("unknown self_hosted config field %q", key)
			}
		}
		return typ, []byte(`{"type":"self_hosted"}`), nil
	case string(domain.EnvCloud):
		for key := range obj {
			if key != "type" && key != "networking" && key != "packages" {
				return "", nil, errInvalid("unknown cloud config field %q", key)
			}
		}
		// Base: the existing cloud config when updating, defaults otherwise.
		base := cloudConfigJSON{
			Type:       typ,
			Networking: json.RawMessage(`{"type":"unrestricted"}`),
			Packages: packagesJSON{
				Apt: []string{}, Cargo: []string{}, Gem: []string{},
				Go: []string{}, Npm: []string{}, Pip: []string{},
			},
		}
		if existing != nil {
			var prev cloudConfigJSON
			if err := json.Unmarshal(existing, &prev); err == nil && prev.Type == typ {
				base.Networking, base.Packages = prev.Networking, prev.Packages
			}
		}
		if nw, ok := obj["networking"]; ok && !isNull(nw) {
			base.Networking, err = parseNetworking(nw, base.Networking)
			if err != nil {
				return "", nil, err
			}
		}
		if pk, ok := obj["packages"]; ok && !isNull(pk) {
			base.Packages, err = parsePackages(pk, base.Packages)
			if err != nil {
				return "", nil, err
			}
		}
		normalized, err = json.Marshal(base)
		return typ, normalized, err
	default:
		return "", nil, errInvalid(`config.type must be "cloud" or "self_hosted"`)
	}
}

// parseNetworking validates a networking object strictly (unknown fields are
// rejected — a typo'd allowed_hosts must not silently lock all egress open or
// closed). When both the patch and prior are "limited", omitted fields keep
// their prior values.
func parseNetworking(raw, prior json.RawMessage) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, errInvalid("networking must be an object")
	}
	var typ string
	if rawType, ok := obj["type"]; ok {
		_ = json.Unmarshal(rawType, &typ)
	}
	switch typ {
	case string(domain.NetUnrestricted):
		for key := range obj {
			if key != "type" {
				return nil, errInvalid("unknown unrestricted networking field %q", key)
			}
		}
		return json.RawMessage(`{"type":"unrestricted"}`), nil
	case string(domain.NetLimited):
		out := limitedNetworkJSON{Type: typ, AllowedHosts: []string{}}
		var prev limitedNetworkJSON
		if json.Unmarshal(prior, &prev) == nil && prev.Type == typ {
			out = prev
		}
		for key, val := range obj {
			switch key {
			case "type":
			case "allowed_hosts":
				var hosts []string
				if !isNull(val) {
					if err := json.Unmarshal(val, &hosts); err != nil {
						return nil, errInvalid("allowed_hosts must be a list of hostnames")
					}
				}
				if hosts == nil {
					hosts = []string{}
				}
				out.AllowedHosts = hosts
			case "allow_mcp_servers":
				if err := json.Unmarshal(val, &out.AllowMCPServers); err != nil {
					return nil, errInvalid("allow_mcp_servers must be a boolean")
				}
			case "allow_package_managers":
				if err := json.Unmarshal(val, &out.AllowPackageManagers); err != nil {
					return nil, errInvalid("allow_package_managers must be a boolean")
				}
			default:
				return nil, errInvalid("unknown limited networking field %q", key)
			}
		}
		return json.Marshal(out)
	default:
		return nil, errInvalid(`networking.type must be "unrestricted" or "limited"`)
	}
}

// parsePackages merges a packages patch onto base: managers present in the
// patch replace their list (null clears), absent managers keep base values.
func parsePackages(raw json.RawMessage, base packagesJSON) (packagesJSON, error) {
	var byManager map[string]json.RawMessage
	if err := json.Unmarshal(raw, &byManager); err != nil || byManager == nil {
		return base, errInvalid("packages must map package managers to lists of packages")
	}
	for manager, rawList := range byManager {
		list := []string{}
		if !isNull(rawList) {
			if err := json.Unmarshal(rawList, &list); err != nil {
				return base, errInvalid("packages.%s must be a list of packages", manager)
			}
		}
		switch manager {
		case "apt":
			base.Apt = list
		case "cargo":
			base.Cargo = list
		case "gem":
			base.Gem = list
		case "go":
			base.Go = list
		case "npm":
			base.Npm = list
		case "pip":
			base.Pip = list
		default:
			return base, errInvalid("unknown package manager %q", manager)
		}
	}
	return base, nil
}

// parseScope enforces v1's single-tenant posture: only the default
// "organization" scope is accepted.
func parseScope(obj map[string]json.RawMessage) error {
	val, set, null, err := stringField(obj, "scope")
	if err != nil {
		return err
	}
	if !set || null || val == "organization" {
		return nil
	}
	if val == "account" {
		return errInvalid("account-scoped environments are not supported yet")
	}
	return errInvalid(`scope must be "organization" or "account"`)
}

func renderEnvironment(id, name, description string, config []byte, metadata map[string]string,
	createdAt, updatedAt time.Time, archivedAt *time.Time) environmentJSON {
	if metadata == nil {
		metadata = map[string]string{}
	}
	return environmentJSON{
		ID: id, Type: "environment", Name: name, Description: description,
		Config: config, Scope: "organization", Metadata: metadata,
		CreatedAt: createdAt.UTC(), UpdatedAt: updatedAt.UTC(), ArchivedAt: utcPtr(archivedAt),
	}
}

func (s *server) createEnvironment(r *http.Request) (any, error) {
	ctx := r.Context()
	obj, err := decodeObject(r)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(obj, "name", "description", "config", "scope", "metadata"); err != nil {
		return nil, err
	}
	name, err := requiredString(obj, "name")
	if err != nil {
		return nil, err
	}
	if err := parseScope(obj); err != nil {
		return nil, err
	}
	description, _, null, err := stringField(obj, "description")
	if err != nil {
		return nil, err
	}
	if null {
		description = ""
	}
	kind, config, err := normalizeEnvConfig(obj["config"], nil)
	if err != nil {
		return nil, err
	}
	metadata, err := parseMetadata(obj)
	if err != nil {
		return nil, err
	}

	id := domain.NewID(domain.PrefixEnvironment).String()
	var createdAt, updatedAt time.Time
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO environments (id, name, kind, config, description, metadata)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING created_at, updated_at`,
		id, name, kind, config, description, metadata).Scan(&createdAt, &updatedAt); err != nil {
		return nil, err
	}
	return renderEnvironment(id, name, description, config, metadata, createdAt, updatedAt, nil), nil
}

type environmentRow struct {
	name, description    string
	config, metaJSON     []byte
	createdAt, updatedAt time.Time
	archivedAt           *time.Time
}

func (s *server) getEnvironment(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkID(id, "environment"); err != nil {
		return nil, err
	}
	var row environmentRow
	err := s.pool.QueryRow(ctx,
		`SELECT name, description, config, metadata, created_at, updated_at, archived_at
		 FROM environments WHERE id = $1`, id).
		Scan(&row.name, &row.description, &row.config, &row.metaJSON,
			&row.createdAt, &row.updatedAt, &row.archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("environment %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	metadata := map[string]string{}
	if err := json.Unmarshal(row.metaJSON, &metadata); err != nil {
		return nil, err
	}
	return renderEnvironment(id, row.name, row.description, row.config, metadata,
		row.createdAt, row.updatedAt, row.archivedAt), nil
}

func (s *server) updateEnvironment(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	obj, err := decodeObject(r)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(obj, "name", "description", "config", "scope", "metadata"); err != nil {
		return nil, err
	}
	if err := parseScope(obj); err != nil {
		return nil, err
	}
	if err := checkID(id, "environment"); err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var row environmentRow
	var kind string
	err = tx.QueryRow(ctx,
		`SELECT name, kind, description, config, metadata, created_at, updated_at, archived_at
		 FROM environments WHERE id = $1 FOR UPDATE`, id).
		Scan(&row.name, &kind, &row.description, &row.config, &row.metaJSON,
			&row.createdAt, &row.updatedAt, &row.archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("environment %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	if row.archivedAt != nil {
		return nil, errInvalid("environment %s is archived", id)
	}
	metadata := map[string]string{}
	if err := json.Unmarshal(row.metaJSON, &metadata); err != nil {
		return nil, err
	}

	if name, set, null, err := stringField(obj, "name"); err != nil {
		return nil, err
	} else if set {
		if null || name == "" {
			return nil, errInvalid("name cannot be cleared")
		}
		row.name = name
	}
	if desc, set, null, err := stringField(obj, "description"); err != nil {
		return nil, err
	} else if set {
		if null {
			desc = ""
		}
		row.description = desc
	}
	if raw, ok := obj["config"]; ok && !isNull(raw) {
		var newKind string
		newKind, row.config, err = normalizeEnvConfig(raw, row.config)
		if err != nil {
			return nil, err
		}
		// An environment's kind is fixed at creation: cloud vs self_hosted is a
		// deployment-boundary property, not a config field. Changing it would
		// re-home a session's compute (and re-route its work queue between the
		// executor and a BYOC worker), so a config update that flips the kind is
		// rejected rather than silently switching hands mid-flight.
		if newKind != kind {
			return nil, errInvalid("environment kind cannot be changed (from %s to %s)", kind, newKind)
		}
	}
	// Environments alone treat an empty-string value as a delete (the SDK's
	// map[string]string params cannot express null).
	if raw, ok := obj["metadata"]; ok {
		metadata, err = patchMetadata(metadata, raw, true)
		if err != nil {
			return nil, err
		}
	}

	if err := tx.QueryRow(ctx,
		`UPDATE environments SET name = $2, kind = $3, config = $4, description = $5,
		   metadata = $6, updated_at = now()
		 WHERE id = $1 RETURNING updated_at`,
		id, row.name, kind, row.config, row.description, metadata).Scan(&row.updatedAt); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return renderEnvironment(id, row.name, row.description, row.config, metadata,
		row.createdAt, row.updatedAt, row.archivedAt), nil
}

func (s *server) listEnvironments(r *http.Request) (any, error) {
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

	query := `SELECT id, name, description, config, metadata, created_at, updated_at, archived_at
	          FROM environments WHERE true`
	var args []any
	if !includeArchived {
		query += ` AND archived_at IS NULL`
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
		var id string
		var row environmentRow
		if err := rows.Scan(&id, &row.name, &row.description, &row.config, &row.metaJSON,
			&row.createdAt, &row.updatedAt, &row.archivedAt); err != nil {
			return nil, err
		}
		metadata := map[string]string{}
		if err := json.Unmarshal(row.metaJSON, &metadata); err != nil {
			return nil, err
		}
		data = append(data, renderEnvironment(id, row.name, row.description, row.config, metadata,
			row.createdAt, row.updatedAt, row.archivedAt))
		lastT, lastID = row.createdAt, id
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

func (s *server) archiveEnvironment(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkID(id, "environment"); err != nil {
		return nil, err
	}
	var row environmentRow
	err := s.pool.QueryRow(ctx,
		`UPDATE environments SET
		   updated_at  = CASE WHEN archived_at IS NULL THEN now() ELSE updated_at END,
		   archived_at = COALESCE(archived_at, now())
		 WHERE id = $1
		 RETURNING name, description, config, metadata, created_at, updated_at, archived_at`, id).
		Scan(&row.name, &row.description, &row.config, &row.metaJSON,
			&row.createdAt, &row.updatedAt, &row.archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("environment %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	metadata := map[string]string{}
	if err := json.Unmarshal(row.metaJSON, &metadata); err != nil {
		return nil, err
	}
	return renderEnvironment(id, row.name, row.description, row.config, metadata,
		row.createdAt, row.updatedAt, row.archivedAt), nil
}

func (s *server) deleteEnvironment(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkID(id, "environment"); err != nil {
		return nil, err
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM environments WHERE id = $1`, id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" { // foreign_key_violation
		return nil, errInvalid("environment %s still has sessions; delete them first", id)
	}
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, errNotFound("environment %s not found", id)
	}
	return map[string]string{"id": id, "type": "environment_deleted"}, nil
}
