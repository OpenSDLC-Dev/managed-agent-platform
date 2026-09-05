package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
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
// (cloud → networking + all six package lists; self_hosted → type only). A
// rendered cloud response's packages object additionally carries
// "type":"packages" — stamped onto the raw JSON map by packagesTypeEcho at
// render time, never decoded into or persisted through packagesJSON below.
type cloudConfigJSON struct {
	Type       string          `json:"type"`
	Networking json.RawMessage `json:"networking"`
	Packages   packagesJSON    `json:"packages"`
}

// packagesJSON is the six package-manager lists as stored — it has no Type
// field. The wire discriminator "type":"packages" (#382) is a render-time-
// only addition: packagesTypeEcho stamps it onto the raw JSON map after
// normalizeEnvConfig returns, so it never round-trips through this struct.
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
		if err := checkPackagesNetworking(base); err != nil {
			return "", nil, err
		}
		normalized, err = json.Marshal(base)
		return typ, normalized, err
	default:
		return "", nil, errInvalid(`config.type must be "cloud" or "self_hosted"`)
	}
}

// checkPackagesNetworking is the reference's create-time refusal: "If the
// environment uses `limited` networking, also set
// `networking.allow_package_managers` to `true`; otherwise the request is
// rejected with a 400 error" — which its networking section restates as
// applying "whenever the environment specifies `packages` … even if the
// registry hosts are listed in `allowed_hosts`". An install the gate can only
// refuse is better refused at the request.
//
// "Specifies" is read as a non-empty list, not a present key: every stored
// cloud config carries all six lists (normalizeEnvConfig's own base fills
// them), so key presence would refuse every limited environment ever created
// here. The message is ours, and names both remedies the caller has.
//
// It runs on the merged config, after both blocks are parsed, so the two halves
// of the offending shape cannot arrive separately: an update that adds packages
// to a limited environment and one that switches a packaged environment to
// limited are refused exactly as a create is. The consequence worth knowing is
// for rows stored before this rule — any config patch on one is refused, even a
// patch that touches only allowed_hosts, because the merge carries the stored
// lists into the check. A patch setting the flag, or clearing the lists, is
// what lifts it; a patch with no config at all never reaches here.
func checkPackagesNetworking(cfg cloudConfigJSON) error {
	var nw limitedNetworkJSON
	if json.Unmarshal(cfg.Networking, &nw) != nil ||
		nw.Type != string(domain.NetLimited) || nw.AllowPackageManagers {
		return nil
	}
	p := cfg.Packages
	if len(p.Apt)+len(p.Cargo)+len(p.Gem)+len(p.Go)+len(p.Npm)+len(p.Pip) == 0 {
		return nil
	}
	return errInvalid("packages require networking.allow_package_managers to be true under limited networking")
}

// parseNetworking validates a networking object strictly (unknown fields are
// rejected — a typo'd allowed_hosts must not silently lock all egress open or
// closed). When both the patch and prior are "limited", omitted fields keep
// their prior values.
//
// Each allowed_hosts entry is held to the same grammar a credential's is, and a
// Unicode entry is stored as its A-label (canonicalAllowedHost, over
// egress.CanonicalEntry). This list was unvalidated until plan 43, which is why
// the check runs on what a patch newly supplies rather than on the merged list,
// and skips an entry the row already holds: refusing an update over what an
// earlier one stored would take a stored environment's egress away for a field
// nobody is changing, and would refuse the read-modify-write every client does.
// The reference publishes no grammar for this field and documents no rejection
// for it, so the 400 is ours and is registered in DIVERGENCES.
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
				// Only what this patch newly supplies. An entry the row
				// already holds is carried through as it stands, because a
				// client that GETs a config and POSTs it back — the ordinary
				// read-modify-write — would otherwise be refused on a value
				// this API handed it one call earlier. Rows written before this
				// check are not migrated (plan 43 decision 4), and that promise
				// is worth nothing if echoing the row back breaks it.
				for i, h := range hosts {
					if slices.Contains(prev.AllowedHosts, h) {
						continue
					}
					canonical, err := canonicalAllowedHost(h)
					if err != nil {
						return nil, err
					}
					hosts[i] = canonical
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
// "type" is not a manager — it's the reference's discriminator sibling
// (packagesJSON has no field for it; see packagesTypeEcho), and the only
// value it ever admits is the JSON string "packages".
func parsePackages(raw json.RawMessage, base packagesJSON) (packagesJSON, error) {
	var byManager map[string]json.RawMessage
	if err := json.Unmarshal(raw, &byManager); err != nil || byManager == nil {
		return base, errInvalid("packages must map package managers to lists of packages")
	}
	for manager, rawList := range byManager {
		if manager == "type" {
			// Present requires exactly "packages"; null and "" fall through
			// here too — json.Unmarshal into &typ leaves "" for null, same as
			// a literal empty string, and both already fail typ != "packages".
			var typ string
			if json.Unmarshal(rawList, &typ) != nil || typ != "packages" {
				return base, errInvalid(`packages.type must be "packages"`)
			}
			continue
		}
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
		// Checked after the switch, so an unknown manager is named as such
		// rather than blamed for its entries. domain.ValidPackageEntry is the
		// predicate the executor applies again before building a command, so
		// the two cannot drift.
		total := 0
		for _, entry := range list {
			if !domain.ValidPackageEntry(entry) {
				return base, errInvalid(`packages.%s entries must be non-empty and must not begin with "-"`, manager)
			}
			total += len(entry)
		}
		// A fast bound on entry bytes: the executor hands one manager's whole
		// list to a single `bash -c` argument, which Linux caps near 128 KiB, and
		// a list past that faults at exec startup and reclaim-loops the item. This
		// is not the whole guard — `go` expands to one invocation per entry, so
		// the assembled command can exceed the ceiling while the entry bytes stay
		// small; the executor bounds the assembled command itself
		// (maxInstallCommandBytes). This cap keeps a request cheap to reject and a
		// real list (a few kilobytes) well clear of both.
		if total > maxPackagesManagerBytes {
			return base, errInvalid("packages.%s is too large: %d bytes exceeds the %d-byte limit", manager, total, maxPackagesManagerBytes)
		}
	}
	return base, nil
}

// maxPackagesManagerBytes bounds the summed length of one manager's entries. See
// parsePackages for why it exists and why it sits well under Linux's ~128 KiB
// single-argument ceiling.
const maxPackagesManagerBytes = 16 << 10

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
		Config: packagesTypeEcho(config), Scope: "organization", Metadata: metadata,
		CreatedAt: createdAt.UTC(), UpdatedAt: updatedAt.UTC(), ArchivedAt: utcPtr(archivedAt),
	}
}

// packagesTypeEcho stamps "type":"packages" onto a cloud config's packages
// object at render time — every create/get/list/update/archive response
// goes through here, via renderEnvironment. The reference SDK types this key
// as a discriminator sibling to the six manager lists, admitting only the
// literal "packages" (anthropic-sdk-go v1.66.0 betaenvironment.go
// BetaPackages.Type / BetaPackagesParams.Type, #382); packagesJSON has no
// field for it, and parsePackages validates but never persists it (see its
// own comment). That the reference's own response always renders the field
// is unobserved (docs/DIVERGENCES.md, INFERRED); this platform does so
// unconditionally.
//
// It works on maps of json.RawMessage rather than decoding through
// cloudConfigJSON/packagesJSON: an earlier version that round-tripped through
// those typed structs silently dropped any field the current normalizer
// doesn't know about (a future top-level key, a future package manager) and
// rewrote a stored "packages": null into an object of null lists (unmarshal
// into a struct field is a silent no-op for JSON null, so nothing there
// distinguished the two). Editing only the "packages" sub-map's "type" key in
// place keeps both: every other field survives unchanged as a JSON value —
// key order and whitespace are not preserved, since re-marshaling a
// map[string]json.RawMessage re-sorts keys and HTML-escapes a raw &, < or >
// inside any sibling value. self_hosted configs have no packages object and
// pass through untouched (this function returns config unmodified, with no
// marshal at all), as does anything that doesn't decode as an object at the
// level being read — including a "packages" that is null or otherwise not an
// object — unreachable through this platform's own writes, reachable only by
// a direct DB write bypassing them.
func packagesTypeEcho(config []byte) []byte {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(config, &top); err != nil {
		return config
	}
	var typ string
	if raw, ok := top["type"]; !ok || json.Unmarshal(raw, &typ) != nil || typ != string(domain.EnvCloud) {
		return config
	}
	rawPackages, ok := top["packages"]
	if !ok {
		return config
	}
	var pkgs map[string]json.RawMessage
	if err := json.Unmarshal(rawPackages, &pkgs); err != nil || pkgs == nil {
		return config
	}
	pkgs["type"] = json.RawMessage(`"packages"`)
	newPackages, err := json.Marshal(pkgs)
	if err != nil {
		return config
	}
	top["packages"] = newPackages
	out, err := json.Marshal(top)
	if err != nil {
		return config
	}
	return out
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

// environmentStillReferenced renders the 400 for a delete a foreign key
// refused. It reads what actually references the environment rather than
// trusting the constraint the error names: Postgres reports one violated
// constraint, and when both a session and a deployment are in the way it
// reports the older of the two, so an operator told only about the sessions
// would clear them and find the delete still refused.
//
// One statement, so the counts, the classification and the named ids all come
// from one snapshot. Two would let a deployment created between them turn a
// "nothing references this any more" read into advice that can never work.
//
// Which remedy to name is the rest of the work, and getting it wrong is
// expensive. A live deployment can be pointed at another environment, which
// clears the reference and lets the delete through. An archived one refuses
// every update, so it can never be moved and the reference can never be
// cleared. Sending an operator to archive the environment when a repoint would
// have done trades a reversible fix for an irreversible one — nothing
// unarchives an environment either.
//
// Sessions are counted rather than named, and the count is load-bearing: a
// message that promised the delete would go through after the repoint alone
// would be false whenever a session blocks too, which is the same defect as
// blaming the sessions and hiding the deployment, only reversed.
//
// The deployment list has no archived_at predicate — every row blocks, and this
// is where the two deployment refusals part company: the agent archive asks
// which deployments can still fire, and a foreign key cannot ask that. Archived
// rows sort first, so the one that makes the delete permanent is named rather
// than truncated away behind five live ones.
func environmentStillReferenced(ctx context.Context, db querier, envID string) error {
	var sessions, deployments, stuck int
	var envArchived bool
	var named []string
	if err := db.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM sessions WHERE environment_id = $1),
		        (SELECT count(*) FROM deployments WHERE environment_id = $1),
		        (SELECT count(*) FROM deployments
		          WHERE environment_id = $1 AND archived_at IS NOT NULL),
		        COALESCE((SELECT archived_at IS NOT NULL FROM environments WHERE id = $1), false),
		        COALESCE((SELECT array_agg(id) FROM
		          (SELECT id FROM deployments WHERE environment_id = $1
		            ORDER BY (archived_at IS NULL), created_at, id LIMIT $2) t), '{}')`,
		envID, blockingDeploymentsNamed).
		Scan(&sessions, &deployments, &stuck, &envArchived, &named); err != nil {
		return err
	}

	switch {
	case deployments == 0 && sessions == 0:
		// The delete's own statement found a referent and this read did not.
		// Sessions and deployments are the only two referents Postgres will not
		// cascade away, and both can be cleared concurrently — a session
		// deleted, a deployment repointed. A retry then succeeds, or answers
		// 404 if the environment went with it.
		return errInvalid("environment %s was still referenced when the delete ran; try again", envID)
	case deployments == 0:
		return errInvalid("environment %s still has sessions; delete them first", envID)
	}

	list := strings.Join(named, ", ")
	if deployments > len(named) {
		list = fmt.Sprintf("%s and %d more", list, deployments-len(named))
	}
	blockers := fmt.Sprintf("%d %s (%s)", deployments, plural(deployments, "deployment"), list)
	if sessions > 0 {
		blockers = fmt.Sprintf("%s and %d %s", blockers, sessions, plural(sessions, "session"))
	}

	if stuck > 0 {
		// An archived deployment can never be moved, so the delete is out of
		// reach whatever else is cleared — the sessions included. Archiving the
		// environment is what is left, and worth naming only while it has not
		// already been done: the operator who took that advice must not be
		// given it again.
		remedy := " — archive it instead"
		if envArchived {
			remedy = ""
		}
		return errInvalid("environment %s is referenced by %s, %d of them archived and so unmovable; it can no longer be deleted%s",
			envID, blockers, stuck, remedy)
	}
	remedy := "point each at another environment"
	if sessions > 0 {
		remedy = "point each deployment at another environment and delete the sessions"
	}
	return errInvalid("environment %s is referenced by %s; %s and the delete will go through", envID, blockers, remedy)
}

// plural is the s-suffix these refusals need and nothing more.
func plural(n int, noun string) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}

// selfHostedQueueRefusal is the reference's 409 when the environment's work
// queue still holds an item a worker could be handed or is running, and nil
// when it does not. What counts as the queue is queue.HasUndrainedWork's to
// define rather than this package's — it is the wire's own scope, so it
// answers false for a cloud environment and for one that does not exist, and
// the 404 is left to the delete.
func (s *server) selfHostedQueueRefusal(ctx context.Context, envID string) error {
	undrained, err := s.queue.HasUndrainedWork(ctx, domain.ID(envID))
	if err != nil {
		return err
	}
	if !undrained {
		return nil
	}
	return errConflict("Cannot delete self-hosted environment with work in the queue. " +
		"Either archive the environment first to allow the queue to drain, or use force=true to delete immediately.")
}

// deleteEnvironment hard-deletes an environment. A self_hosted environment
// whose work queue still holds undrained items is refused — 409, in the
// reference's own sentence (recorded 2026-09-02, #546).
//
// It is a refusal this platform already made, in the wrong words. work_items
// cascades from environments, but every item carries a session, a session
// holds its environment through a foreign key that does not cascade, and
// nothing moves a session between environments — so the delete raised 23503
// before any cascade could run. What it said was that the environment still
// had sessions, and clearing those is exactly what cascades the queue away:
// the loss the reference refuses in order to prevent.
//
// Which is why the queue is read on both sides of the delete rather than
// locked across it. Before, so the refusal never rides on a foreign key that
// a row the schema permits — an item whose environment differs from its
// session's, which no enqueue path produces — would not raise. After, because
// a live session's first enqueue can land in the gap between the two, and
// there the sessions' refusal would send an operator to delete the sessions,
// taking that item with them. Locking instead would invert this handler's
// lock order against every enqueue path, which takes the session row first.
//
// ?force=true lifts the queue refusal and only that. On the reference the
// forced delete answers 200; here the sessions refuse it in their own words,
// so force exchanges one refusal for another rather than deleting — the
// divergence docs/DIVERGENCES.md registers.
func (s *server) deleteEnvironment(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkID(id, "environment"); err != nil {
		return nil, err
	}
	force, err := parseBoolParam(r.URL.Query(), "force")
	if err != nil {
		return nil, err
	}
	if !force {
		if err := s.selfHostedQueueRefusal(ctx, id); err != nil {
			return nil, err
		}
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM environments WHERE id = $1`, id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" { // foreign_key_violation
		if !force {
			if err := s.selfHostedQueueRefusal(ctx, id); err != nil {
				return nil, err
			}
		}
		return nil, environmentStillReferenced(ctx, s.pool, id)
	}
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, errNotFound("environment %s not found", id)
	}
	return map[string]string{"id": id, "type": "environment_deleted"}, nil
}
