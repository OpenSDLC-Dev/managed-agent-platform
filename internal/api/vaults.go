package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// vaultJSON is the BetaManagedAgentsVault wire shape.
type vaultJSON struct {
	ID          string            `json:"id"`
	Type        string            `json:"type"`
	DisplayName string            `json:"display_name"`
	Metadata    map[string]string `json:"metadata"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	ArchivedAt  *time.Time        `json:"archived_at"`
}

// The documented vault-surface limits (plan 12 D7). Unlike sibling resources,
// the reference documents these for vaults explicitly, so they are enforced
// here and only here (the asymmetry is recorded in docs/DIVERGENCES.md).
const (
	vaultDisplayNameMax    = 255
	vaultMetadataMaxPairs  = 16
	vaultMetadataKeyMax    = 64
	vaultMetadataValueMax  = 512
	vaultCredentialsMax    = 20
	credentialAllowedHosts = 16
)

// validateVaultMetadata enforces the documented caps on a full metadata map —
// called on create and after every patch, so an update cannot grow past them.
func validateVaultMetadata(md map[string]string) error {
	if len(md) > vaultMetadataMaxPairs {
		return errInvalid("metadata cannot exceed %d pairs", vaultMetadataMaxPairs)
	}
	for k, v := range md {
		if len(k) > vaultMetadataKeyMax {
			return errInvalid("metadata keys cannot exceed %d characters", vaultMetadataKeyMax)
		}
		if len(v) > vaultMetadataValueMax {
			return errInvalid("metadata values cannot exceed %d characters", vaultMetadataValueMax)
		}
	}
	return nil
}

func validateVaultDisplayName(name string) error {
	if name == "" {
		return errInvalid("display_name is required")
	}
	if len(name) > vaultDisplayNameMax {
		return errInvalid("display_name cannot exceed %d characters", vaultDisplayNameMax)
	}
	return nil
}

func renderVault(id, displayName string, metadata map[string]string,
	createdAt, updatedAt time.Time, archivedAt *time.Time) vaultJSON {
	if metadata == nil {
		metadata = map[string]string{}
	}
	return vaultJSON{
		ID: id, Type: "vault", DisplayName: displayName, Metadata: metadata,
		CreatedAt: createdAt.UTC(), UpdatedAt: updatedAt.UTC(), ArchivedAt: utcPtr(archivedAt),
	}
}

func (s *server) createVault(r *http.Request) (any, error) {
	ctx := r.Context()
	obj, err := decodeObject(r)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(obj, "display_name", "metadata"); err != nil {
		return nil, err
	}
	displayName, err := requiredString(obj, "display_name")
	if err != nil {
		return nil, err
	}
	if err := validateVaultDisplayName(displayName); err != nil {
		return nil, err
	}
	metadata, err := parseMetadata(obj)
	if err != nil {
		return nil, err
	}
	if err := validateVaultMetadata(metadata); err != nil {
		return nil, err
	}

	id := domain.NewID(domain.PrefixVault).String()
	var createdAt, updatedAt time.Time
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO vaults (id, display_name, metadata)
		 VALUES ($1, $2, $3) RETURNING created_at, updated_at`,
		id, displayName, metadata).Scan(&createdAt, &updatedAt); err != nil {
		return nil, err
	}
	return renderVault(id, displayName, metadata, createdAt, updatedAt, nil), nil
}

type vaultRow struct {
	displayName          string
	metaJSON             []byte
	createdAt, updatedAt time.Time
	archivedAt           *time.Time
}

func (s *server) getVault(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkID(id, "vault"); err != nil {
		return nil, err
	}
	var row vaultRow
	err := s.pool.QueryRow(ctx,
		`SELECT display_name, metadata, created_at, updated_at, archived_at
		 FROM vaults WHERE id = $1`, id).
		Scan(&row.displayName, &row.metaJSON, &row.createdAt, &row.updatedAt, &row.archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("vault %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	metadata := map[string]string{}
	if err := json.Unmarshal(row.metaJSON, &metadata); err != nil {
		return nil, err
	}
	return renderVault(id, row.displayName, metadata, row.createdAt, row.updatedAt, row.archivedAt), nil
}

func (s *server) updateVault(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkID(id, "vault"); err != nil {
		return nil, err
	}
	obj, err := decodeObject(r)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(obj, "display_name", "metadata"); err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var row vaultRow
	err = tx.QueryRow(ctx,
		`SELECT display_name, metadata, created_at, updated_at, archived_at
		 FROM vaults WHERE id = $1 FOR UPDATE`, id).
		Scan(&row.displayName, &row.metaJSON, &row.createdAt, &row.updatedAt, &row.archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("vault %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	if row.archivedAt != nil {
		return nil, errInvalid("vault %s is archived", id)
	}
	metadata := map[string]string{}
	if err := json.Unmarshal(row.metaJSON, &metadata); err != nil {
		return nil, err
	}

	if name, set, null, err := stringField(obj, "display_name"); err != nil {
		return nil, err
	} else if set {
		if null {
			return nil, errInvalid("display_name cannot be cleared")
		}
		if err := validateVaultDisplayName(name); err != nil {
			return nil, err
		}
		row.displayName = name
	}
	if raw, ok := obj["metadata"]; ok {
		// Patch semantics: string upserts, null deletes, omitted keys keep
		// (emptyDeletes=false — the environments empty-string rule is
		// documented for environments only).
		metadata, err = patchMetadata(metadata, raw, false)
		if err != nil {
			return nil, err
		}
		if err := validateVaultMetadata(metadata); err != nil {
			return nil, err
		}
	}

	if err := tx.QueryRow(ctx,
		`UPDATE vaults SET display_name = $2, metadata = $3, updated_at = now()
		 WHERE id = $1 RETURNING updated_at`,
		id, row.displayName, metadata).Scan(&row.updatedAt); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return renderVault(id, row.displayName, metadata, row.createdAt, row.updatedAt, row.archivedAt), nil
}

func (s *server) listVaults(r *http.Request) (any, error) {
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

	query := `SELECT id, display_name, metadata, created_at, updated_at, archived_at
	          FROM vaults WHERE true`
	var args []any
	if !includeArchived {
		query += ` AND archived_at IS NULL`
	}
	if page.cur != nil {
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
		var row vaultRow
		if err := rows.Scan(&id, &row.displayName, &row.metaJSON,
			&row.createdAt, &row.updatedAt, &row.archivedAt); err != nil {
			return nil, err
		}
		metadata := map[string]string{}
		if err := json.Unmarshal(row.metaJSON, &metadata); err != nil {
			return nil, err
		}
		data = append(data, renderVault(id, row.displayName, metadata,
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

func (s *server) archiveVault(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkID(id, "vault"); err != nil {
		return nil, err
	}
	// Archiving the vault purges every credential's sealed secrets (the docs'
	// "secrets are purged; records are retained") and archives them with it,
	// idempotently: an already-archived credential keeps its own archived_at.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var row vaultRow
	err = tx.QueryRow(ctx,
		`UPDATE vaults SET
		   updated_at  = CASE WHEN archived_at IS NULL THEN now() ELSE updated_at END,
		   archived_at = COALESCE(archived_at, now())
		 WHERE id = $1
		 RETURNING display_name, metadata, created_at, updated_at, archived_at`, id).
		Scan(&row.displayName, &row.metaJSON, &row.createdAt, &row.updatedAt, &row.archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("vault %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE vault_credentials SET
		   secret_ciphertext = NULL, secret_key_id = NULL,
		   updated_at  = CASE WHEN archived_at IS NULL THEN now() ELSE updated_at END,
		   archived_at = COALESCE(archived_at, now())
		 WHERE vault_id = $1`, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	metadata := map[string]string{}
	if err := json.Unmarshal(row.metaJSON, &metadata); err != nil {
		return nil, err
	}
	return renderVault(id, row.displayName, metadata, row.createdAt, row.updatedAt, row.archivedAt), nil
}

func (s *server) deleteVault(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkID(id, "vault"); err != nil {
		return nil, err
	}
	// Hard delete; credentials go with the vault (ON DELETE CASCADE — a
	// credential cannot outlive its vault). Recorded as INFERRED in
	// docs/DIVERGENCES.md: the reference's behavior for a non-empty vault is
	// unobserved.
	tag, err := s.pool.Exec(ctx, `DELETE FROM vaults WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, errNotFound("vault %s not found", id)
	}
	return map[string]string{"id": id, "type": "vault_deleted"}, nil
}
