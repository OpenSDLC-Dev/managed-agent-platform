package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// memoryStoreJSON is the BetaManagedAgentsMemoryStore wire shape
// (anthropic-sdk-go v1.66.0 betamemorystore.go:179-216). type/id/name/
// created_at/updated_at are api:"required" and archived_at api:"nullable";
// description and metadata are neither, but always render — "" and {} when
// unset, per "Empty string when unset" (:196-198) — so no client sees null
// where the schema types a string or an object.
type memoryStoreJSON struct {
	ID          string            `json:"id"`
	Type        string            `json:"type"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Metadata    map[string]string `json:"metadata"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	ArchivedAt  *time.Time        `json:"archived_at"`
}

// The documented store-surface limits (betamemorystore.go:232-243, and the
// OpenAPI spec's minLength/maxLength on BetaManagedAgentsCreateMemoryStore-
// Request). The metadata caps are the shared documented ones in wire.go.
const (
	memoryStoreNameMax        = 255
	memoryStoreDescriptionMax = 1024
)

// validateMemoryStoreName holds the documented "1–255 characters; no control
// characters". Lengths count runes, not bytes — the repo's reading of a
// documented "character" (validateMetadataCaps). "Control" is Unicode Cc:
// the spec says only "no control characters", and the format characters (Cf)
// a memory PATH also refuses are that rule's, not this one.
func validateMemoryStoreName(name string) error {
	if name == "" {
		return errInvalid("name is required")
	}
	if utf8.RuneCountInString(name) > memoryStoreNameMax {
		return errInvalid("name cannot exceed %d characters", memoryStoreNameMax)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return errInvalid("name cannot contain control characters")
		}
	}
	return nil
}

func validateMemoryStoreDescription(description string) error {
	if utf8.RuneCountInString(description) > memoryStoreDescriptionMax {
		return errInvalid("description cannot exceed %d characters", memoryStoreDescriptionMax)
	}
	return nil
}

func renderMemoryStore(id, name, description string, metadata map[string]string,
	createdAt, updatedAt time.Time, archivedAt *time.Time) memoryStoreJSON {
	if metadata == nil {
		metadata = map[string]string{}
	}
	return memoryStoreJSON{
		ID: id, Type: "memory_store", Name: name, Description: description, Metadata: metadata,
		CreatedAt: createdAt.UTC(), UpdatedAt: updatedAt.UTC(), ArchivedAt: utcPtr(archivedAt),
	}
}

func (s *server) createMemoryStore(r *http.Request) (any, error) {
	ctx := r.Context()
	obj, err := decodeObject(r)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(obj, "name", "description", "metadata"); err != nil {
		return nil, err
	}
	name, err := requiredString(obj, "name")
	if err != nil {
		return nil, err
	}
	if err := validateMemoryStoreName(name); err != nil {
		return nil, err
	}
	// An absent description is "", and so is an explicit null: there is nothing
	// to preserve on a create, and stringField already reads null as the empty
	// value (the parseMetadata precedent, which reads a null bag as {}).
	description, _, _, err := stringField(obj, "description")
	if err != nil {
		return nil, err
	}
	if err := validateMemoryStoreDescription(description); err != nil {
		return nil, err
	}
	metadata, err := parseMetadata(obj)
	if err != nil {
		return nil, err
	}
	if err := validateMetadataCaps(metadata); err != nil {
		return nil, err
	}

	// Audit only, never on the wire: the api key's id on the machine lane, the
	// human's principal id on the identity lane (sessions.created_by's rule).
	var createdBy *string
	if p := principalFrom(ctx); p != "" {
		createdBy = &p
	}

	id := domain.NewID(domain.PrefixMemoryStore).String()
	var createdAt, updatedAt time.Time
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO memory_stores (id, name, description, metadata, created_by)
		 VALUES ($1, $2, $3, $4, $5) RETURNING created_at, updated_at`,
		id, name, description, metadata, createdBy).Scan(&createdAt, &updatedAt); err != nil {
		return nil, err
	}
	return renderMemoryStore(id, name, description, metadata, createdAt, updatedAt, nil), nil
}

type memoryStoreRow struct {
	name, description    string
	metaJSON             []byte
	createdAt, updatedAt time.Time
	archivedAt           *time.Time
}

func (s *server) getMemoryStore(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkID(id, "memory store"); err != nil {
		return nil, err
	}
	// No archived filter: retrieve returns the store "including archived
	// stores" (the spec's BetaManagedAgentsGetMemoryStoreResponse).
	var row memoryStoreRow
	err := s.pool.QueryRow(ctx,
		`SELECT name, description, metadata, created_at, updated_at, archived_at
		 FROM memory_stores WHERE id = $1`, id).
		Scan(&row.name, &row.description, &row.metaJSON, &row.createdAt, &row.updatedAt, &row.archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("memory store %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	metadata := map[string]string{}
	if err := json.Unmarshal(row.metaJSON, &metadata); err != nil {
		return nil, err
	}
	return renderMemoryStore(id, row.name, row.description, metadata,
		row.createdAt, row.updatedAt, row.archivedAt), nil
}

func (s *server) updateMemoryStore(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkID(id, "memory store"); err != nil {
		return nil, err
	}
	obj, err := decodeObject(r)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(obj, "name", "description", "metadata"); err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var row memoryStoreRow
	err = tx.QueryRow(ctx,
		`SELECT name, description, metadata, created_at, updated_at, archived_at
		 FROM memory_stores WHERE id = $1 FOR UPDATE`, id).
		Scan(&row.name, &row.description, &row.metaJSON, &row.createdAt, &row.updatedAt, &row.archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("memory store %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	// Archived is read-only (the vault rule — plan 36 decision 3; the reference
	// says only "read-only", with no status, registered as INFERRED).
	if row.archivedAt != nil {
		return nil, errInvalid("memory store %s is archived", id)
	}
	metadata := map[string]string{}
	if err := json.Unmarshal(row.metaJSON, &metadata); err != nil {
		return nil, err
	}

	// The spec admits JSON null on all three top-level fields and says nothing
	// about what it means; the SDK's `omitzero` never sends one. Each field
	// takes the rule its sibling resources already apply: `name` is required,
	// so null or "" is "cannot be cleared" (updateAgent, updateEnvironment,
	// updateVault); a null `description` clears, like its documented ""
	// (updateAgent's description); a null metadata bag preserves (patchMetadata).
	if name, set, null, err := stringField(obj, "name"); err != nil {
		return nil, err
	} else if set {
		if null || name == "" {
			return nil, errInvalid("name cannot be cleared")
		}
		if err := validateMemoryStoreName(name); err != nil {
			return nil, err
		}
		row.name = name
	}
	if description, set, null, err := stringField(obj, "description"); err != nil {
		return nil, err
	} else if set {
		if null {
			description = ""
		}
		if err := validateMemoryStoreDescription(description); err != nil {
			return nil, err
		}
		row.description = description
	}
	if raw, ok := obj["metadata"]; ok {
		// Patch semantics: string upserts, null deletes, omitted keys keep
		// (emptyDeletes=false — the environments empty-string rule is
		// documented for environments only).
		metadata, err = patchMetadata(metadata, raw, false)
		if err != nil {
			return nil, err
		}
		if err := validateMetadataCaps(metadata); err != nil {
			return nil, err
		}
	}

	if err := tx.QueryRow(ctx,
		`UPDATE memory_stores SET name = $2, description = $3, metadata = $4, updated_at = now()
		 WHERE id = $1 RETURNING updated_at`,
		id, row.name, row.description, metadata).Scan(&row.updatedAt); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return renderMemoryStore(id, row.name, row.description, metadata,
		row.createdAt, row.updatedAt, row.archivedAt), nil
}

func (s *server) listMemoryStores(r *http.Request) (any, error) {
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
	// Both bounds are inclusive (betamemorystore.go:290-296).
	gte, err := parseTimeParam(q, "created_at[gte]")
	if err != nil {
		return nil, err
	}
	lte, err := parseTimeParam(q, "created_at[lte]")
	if err != nil {
		return nil, err
	}

	query := `SELECT id, name, description, metadata, created_at, updated_at, archived_at
	          FROM memory_stores WHERE true`
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
		var id string
		var row memoryStoreRow
		if err := rows.Scan(&id, &row.name, &row.description, &row.metaJSON,
			&row.createdAt, &row.updatedAt, &row.archivedAt); err != nil {
			return nil, err
		}
		metadata := map[string]string{}
		if err := json.Unmarshal(row.metaJSON, &metadata); err != nil {
			return nil, err
		}
		data = append(data, renderMemoryStore(id, row.name, row.description, metadata,
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

func (s *server) archiveMemoryStore(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkID(id, "memory store"); err != nil {
		return nil, err
	}
	// Idempotent, the vault rule: archived_at is set once and never cleared, so
	// a second archive returns the store with the first call's timestamp and
	// leaves updated_at where it was.
	var row memoryStoreRow
	err := s.pool.QueryRow(ctx,
		`UPDATE memory_stores SET
		   updated_at  = CASE WHEN archived_at IS NULL THEN now() ELSE updated_at END,
		   archived_at = COALESCE(archived_at, now())
		 WHERE id = $1
		 RETURNING name, description, metadata, created_at, updated_at, archived_at`, id).
		Scan(&row.name, &row.description, &row.metaJSON, &row.createdAt, &row.updatedAt, &row.archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("memory store %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	metadata := map[string]string{}
	if err := json.Unmarshal(row.metaJSON, &metadata); err != nil {
		return nil, err
	}
	return renderMemoryStore(id, row.name, row.description, metadata,
		row.createdAt, row.updatedAt, row.archivedAt), nil
}

func (s *server) deleteMemoryStore(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkID(id, "memory store"); err != nil {
		return nil, err
	}
	// Hard delete: "The store and all its memories and versions are no longer
	// retrievable" (betamemorystore.go:149-153). Slice 2's tables carry the
	// ON DELETE CASCADE that makes the second half true.
	tag, err := s.pool.Exec(ctx, `DELETE FROM memory_stores WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, errNotFound("memory store %s not found", id)
	}
	return map[string]string{"id": id, "type": "memory_store_deleted"}, nil
}
