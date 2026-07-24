package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// credentialJSON is the BetaManagedAgentsCredential wire shape. display_name
// is the one nullable top-level field; auth is the stored secret-free union
// document, embedded verbatim.
type credentialJSON struct {
	ID          string            `json:"id"`
	Type        string            `json:"type"`
	VaultID     string            `json:"vault_id"`
	DisplayName *string           `json:"display_name"`
	Auth        json.RawMessage   `json:"auth"`
	Metadata    map[string]string `json:"metadata"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	ArchivedAt  *time.Time        `json:"archived_at"`
}

// errSecretsUnavailable answers the secret-bearing credential paths on a
// deployment configured without a secrets cipher (fails closed, plan 12 D1);
// metadata CRUD stays available.
var errSecretsUnavailable = &apiError{http.StatusInternalServerError, errTypeAPI,
	"a secrets cipher is not configured on this deployment; vault credential secrets are unavailable"}

func renderCredential(id, vaultID string, displayName *string, auth []byte,
	metadata map[string]string, createdAt, updatedAt time.Time, archivedAt *time.Time) credentialJSON {
	if metadata == nil {
		metadata = map[string]string{}
	}
	return credentialJSON{
		ID: id, Type: "vault_credential", VaultID: vaultID, DisplayName: displayName,
		Auth: auth, Metadata: metadata,
		CreatedAt: createdAt.UTC(), UpdatedAt: updatedAt.UTC(), ArchivedAt: utcPtr(archivedAt),
	}
}

// credentialDisplayName parses the optional, nullable display_name (≤255).
func credentialDisplayName(obj map[string]json.RawMessage) (val *string, set bool, err error) {
	name, set, null, err := stringField(obj, "display_name")
	if err != nil || !set || null {
		return nil, set, err
	}
	if len(name) > vaultDisplayNameMax {
		return nil, true, errInvalid("display_name cannot exceed %d characters", vaultDisplayNameMax)
	}
	return &name, true, nil
}

func (s *server) createVaultCredential(r *http.Request) (any, error) {
	ctx := r.Context()
	vaultID := r.PathValue("id")
	if err := checkID(vaultID, "vault"); err != nil {
		return nil, err
	}
	obj, err := decodeObject(r)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(obj, "auth", "display_name", "metadata"); err != nil {
		return nil, err
	}
	auth, err := parseCredAuthCreate(obj["auth"])
	if err != nil {
		return nil, err
	}
	displayName, _, err := credentialDisplayName(obj)
	if err != nil {
		return nil, err
	}
	metadata, err := parseMetadata(obj)
	if err != nil {
		return nil, err
	}
	if err := validateVaultMetadata(metadata); err != nil {
		return nil, err
	}
	if s.cipher == nil {
		return nil, errSecretsUnavailable
	}

	// Seal before opening the transaction: the seal depends only on the parsed
	// request, so keeping the cipher's network round trip (OpenBao) out of the
	// vault row lock avoids serializing concurrent creates behind it.
	sealed, err := json.Marshal(auth.secrets)
	if err != nil {
		return nil, err
	}
	ciphertext, keyID, err := s.cipher.Encrypt(ctx, sealed)
	if err != nil {
		return nil, fmt.Errorf("seal credential secrets: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var vaultArchived *time.Time
	err = tx.QueryRow(ctx, `SELECT archived_at FROM vaults WHERE id = $1 FOR UPDATE`, vaultID).Scan(&vaultArchived)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("vault %s not found", vaultID)
	}
	if err != nil {
		return nil, err
	}
	if vaultArchived != nil {
		return nil, errInvalid("vault %s is archived", vaultID)
	}
	var active int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM vault_credentials WHERE vault_id = $1 AND archived_at IS NULL`,
		vaultID).Scan(&active); err != nil {
		return nil, err
	}
	if active >= vaultCredentialsMax {
		return nil, errInvalid("vault %s already holds %d active credentials (the maximum)", vaultID, vaultCredentialsMax)
	}

	id := domain.NewID(domain.PrefixCredential).String()
	var createdAt, updatedAt time.Time
	err = tx.QueryRow(ctx,
		`INSERT INTO vault_credentials
		   (id, vault_id, display_name, auth_type, auth, secret_ciphertext, secret_key_id, cred_key, metadata)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING created_at, updated_at`,
		id, vaultID, displayName, auth.authType, auth.doc, ciphertext, keyID, auth.key, metadata).
		Scan(&createdAt, &updatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation: the active-key index
		return nil, errConflict("an active credential with this %s already exists in vault %s",
			credKeyField(auth.authType), vaultID)
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return renderCredential(id, vaultID, displayName, auth.doc, metadata, createdAt, updatedAt, nil), nil
}

func credKeyField(authType string) string {
	if authType == authEnvVar {
		return "secret_name"
	}
	return "mcp_server_url"
}

type credentialRow struct {
	vaultID              string
	displayName          *string
	authType             string
	authDoc              []byte
	ciphertext           []byte
	keyID                *string
	credKey              string
	metaJSON             []byte
	createdAt, updatedAt time.Time
	archivedAt           *time.Time
}

// credentialPathIDs validates the nested path's vault and credential ids.
// Handlers 404 when the vault segment does not match the credential's actual
// vault (INFERRED — recorded in docs/DIVERGENCES.md).
func (s *server) credentialPathIDs(r *http.Request) (vaultID, credID string, err error) {
	vaultID = r.PathValue("id")
	if err := checkID(vaultID, "vault"); err != nil {
		return "", "", err
	}
	credID = r.PathValue("cid")
	if err := checkID(credID, "credential"); err != nil {
		return "", "", err
	}
	return vaultID, credID, nil
}

func (s *server) getVaultCredential(r *http.Request) (any, error) {
	ctx := r.Context()
	vaultID, credID, err := s.credentialPathIDs(r)
	if err != nil {
		return nil, err
	}
	row := &credentialRow{}
	err = s.pool.QueryRow(ctx,
		`SELECT vault_id, display_name, auth_type, auth, metadata, created_at, updated_at, archived_at
		 FROM vault_credentials WHERE id = $1`, credID).
		Scan(&row.vaultID, &row.displayName, &row.authType, &row.authDoc, &row.metaJSON,
			&row.createdAt, &row.updatedAt, &row.archivedAt)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && row.vaultID != vaultID) {
		return nil, errNotFound("credential %s not found in vault %s", credID, vaultID)
	}
	if err != nil {
		return nil, err
	}
	metadata := map[string]string{}
	if err := json.Unmarshal(row.metaJSON, &metadata); err != nil {
		return nil, err
	}
	return renderCredential(credID, vaultID, row.displayName, row.authDoc, metadata,
		row.createdAt, row.updatedAt, row.archivedAt), nil
}

// updateCredentialResealHook is a test-only seam fired between the unlocked
// re-seal read and the locked compare-and-set write; nil in production.
var updateCredentialResealHook func()

func (s *server) updateVaultCredential(r *http.Request) (any, error) {
	ctx := r.Context()
	vaultID, credID, err := s.credentialPathIDs(r)
	if err != nil {
		return nil, err
	}
	obj, err := decodeObject(r)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(obj, "auth", "display_name", "metadata"); err != nil {
		return nil, err
	}

	// An auth update re-seals the secret (Decrypt then Encrypt — two cipher
	// round trips, network calls under OpenBao). Compute it from an unlocked
	// read so the credential row is never pinned across the cipher; the write
	// below re-checks under the row lock that the secret has not rotated since.
	var reseal *credAuthReseal
	if raw, ok := obj["auth"]; ok && !isNull(raw) {
		if s.cipher == nil {
			return nil, errSecretsUnavailable
		}
		base := &credentialRow{}
		err = s.pool.QueryRow(ctx,
			`SELECT vault_id, auth_type, auth, secret_ciphertext, secret_key_id, archived_at
			 FROM vault_credentials WHERE id = $1`, credID).
			Scan(&base.vaultID, &base.authType, &base.authDoc, &base.ciphertext, &base.keyID, &base.archivedAt)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && base.vaultID != vaultID) {
			return nil, errNotFound("credential %s not found in vault %s", credID, vaultID)
		}
		if err != nil {
			return nil, err
		}
		if base.archivedAt != nil {
			return nil, errInvalid("credential %s is archived", credID)
		}
		reseal, err = s.resealCredAuth(ctx, raw, base)
		if err != nil {
			return nil, err
		}
		// Test seam: exercise the compare-and-set below by rotating the stored
		// ciphertext out of band in exactly this window. nil in production.
		if updateCredentialResealHook != nil {
			updateCredentialResealHook()
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row := &credentialRow{}
	err = tx.QueryRow(ctx,
		`SELECT vault_id, display_name, auth_type, auth, secret_ciphertext, secret_key_id,
		        metadata, created_at, updated_at, archived_at
		 FROM vault_credentials WHERE id = $1 FOR UPDATE`, credID).
		Scan(&row.vaultID, &row.displayName, &row.authType, &row.authDoc, &row.ciphertext, &row.keyID,
			&row.metaJSON, &row.createdAt, &row.updatedAt, &row.archivedAt)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && row.vaultID != vaultID) {
		return nil, errNotFound("credential %s not found in vault %s", credID, vaultID)
	}
	if err != nil {
		return nil, err
	}
	// Like archived sibling resources: reject rather than resurrect (INFERRED,
	// docs/DIVERGENCES.md — the archive purged the secrets).
	if row.archivedAt != nil {
		return nil, errInvalid("credential %s is archived", credID)
	}
	if reseal != nil {
		// The unlocked read fed the re-seal; if the secret rotated between then
		// and the lock, our sealed value is stale — refuse rather than clobber.
		if !bytes.Equal(row.ciphertext, reseal.baseCiphertext) {
			return nil, errConflict("credential %s was modified concurrently; retry", credID)
		}
		row.authDoc, row.ciphertext, row.keyID = reseal.doc, reseal.ciphertext, &reseal.keyID
	}
	metadata := map[string]string{}
	if err := json.Unmarshal(row.metaJSON, &metadata); err != nil {
		return nil, err
	}

	if name, set, err := credentialDisplayName(obj); err != nil {
		return nil, err
	} else if set {
		row.displayName = name
	}
	if raw, ok := obj["metadata"]; ok {
		metadata, err = patchMetadata(metadata, raw, false)
		if err != nil {
			return nil, err
		}
		if err := validateVaultMetadata(metadata); err != nil {
			return nil, err
		}
	}

	if err := tx.QueryRow(ctx,
		`UPDATE vault_credentials SET display_name = $2, auth = $3, secret_ciphertext = $4,
		   secret_key_id = $5, metadata = $6, updated_at = now()
		 WHERE id = $1 RETURNING updated_at`,
		credID, row.displayName, row.authDoc, row.ciphertext, row.keyID, metadata).Scan(&row.updatedAt); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return renderCredential(credID, vaultID, row.displayName, row.authDoc, metadata,
		row.createdAt, row.updatedAt, row.archivedAt), nil
}

// credAuthReseal is the sealed result of an auth update, computed off the row
// lock and carrying the ciphertext it was derived from for the compare-and-set.
type credAuthReseal struct {
	doc            []byte
	ciphertext     []byte
	keyID          string
	baseCiphertext []byte
}

// resealCredAuth unseals the base secret, applies the auth update, and re-seals
// — the pair of cipher round trips the caller keeps out of the row lock.
func (s *server) resealCredAuth(ctx context.Context, raw json.RawMessage, base *credentialRow) (*credAuthReseal, error) {
	plain, err := s.cipher.Decrypt(ctx, base.ciphertext, deref(base.keyID))
	if err != nil {
		return nil, fmt.Errorf("unseal credential secrets: %w", err)
	}
	existingSecrets := map[string]string{}
	if err := json.Unmarshal(plain, &existingSecrets); err != nil {
		return nil, err
	}
	newDoc, newSecrets, err := applyCredAuthUpdate(raw, base.authType, base.authDoc, existingSecrets)
	if err != nil {
		return nil, err
	}
	sealed, err := json.Marshal(newSecrets)
	if err != nil {
		return nil, err
	}
	ciphertext, keyID, err := s.cipher.Encrypt(ctx, sealed)
	if err != nil {
		return nil, fmt.Errorf("seal credential secrets: %w", err)
	}
	return &credAuthReseal{doc: newDoc, ciphertext: ciphertext, keyID: keyID, baseCiphertext: base.ciphertext}, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (s *server) listVaultCredentials(r *http.Request) (any, error) {
	ctx := r.Context()
	vaultID := r.PathValue("id")
	if err := checkID(vaultID, "vault"); err != nil {
		return nil, err
	}
	q := r.URL.Query()
	page, err := parsePage(q)
	if err != nil {
		return nil, err
	}
	includeArchived, err := parseBoolParam(q, "include_archived")
	if err != nil {
		return nil, err
	}
	// A list on a missing vault is a 404, not an empty page.
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT true FROM vaults WHERE id = $1`, vaultID).Scan(&exists); errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("vault %s not found", vaultID)
	} else if err != nil {
		return nil, err
	}

	query := `SELECT id, display_name, auth, metadata, created_at, updated_at, archived_at
	          FROM vault_credentials WHERE vault_id = $1`
	args := []any{vaultID}
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
		row := credentialRow{}
		if err := rows.Scan(&id, &row.displayName, &row.authDoc, &row.metaJSON,
			&row.createdAt, &row.updatedAt, &row.archivedAt); err != nil {
			return nil, err
		}
		metadata := map[string]string{}
		if err := json.Unmarshal(row.metaJSON, &metadata); err != nil {
			return nil, err
		}
		data = append(data, renderCredential(id, vaultID, row.displayName, row.authDoc, metadata,
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

func (s *server) archiveVaultCredential(r *http.Request) (any, error) {
	ctx := r.Context()
	vaultID, credID, err := s.credentialPathIDs(r)
	if err != nil {
		return nil, err
	}
	// Archive purges the sealed secrets and keeps the record (the docs);
	// idempotent — an archived credential archives to itself.
	row := &credentialRow{}
	// Scope the mutation to the path's vault: a wrong-vault archive must 404
	// without destroying anything, so the vault_id is in the WHERE, not a
	// post-hoc check on a row already purged.
	err = s.pool.QueryRow(ctx,
		`UPDATE vault_credentials SET
		   secret_ciphertext = NULL, secret_key_id = NULL,
		   updated_at  = CASE WHEN archived_at IS NULL THEN now() ELSE updated_at END,
		   archived_at = COALESCE(archived_at, now())
		 WHERE id = $1 AND vault_id = $2
		 RETURNING vault_id, display_name, auth, metadata, created_at, updated_at, archived_at`, credID, vaultID).
		Scan(&row.vaultID, &row.displayName, &row.authDoc, &row.metaJSON,
			&row.createdAt, &row.updatedAt, &row.archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("credential %s not found in vault %s", credID, vaultID)
	}
	if err != nil {
		return nil, err
	}
	metadata := map[string]string{}
	if err := json.Unmarshal(row.metaJSON, &metadata); err != nil {
		return nil, err
	}
	return renderCredential(credID, vaultID, row.displayName, row.authDoc, metadata,
		row.createdAt, row.updatedAt, row.archivedAt), nil
}

func (s *server) deleteVaultCredential(r *http.Request) (any, error) {
	ctx := r.Context()
	vaultID, credID, err := s.credentialPathIDs(r)
	if err != nil {
		return nil, err
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM vault_credentials WHERE id = $1 AND vault_id = $2`, credID, vaultID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, errNotFound("credential %s not found in vault %s", credID, vaultID)
	}
	return map[string]string{"id": credID, "type": "vault_credential_deleted"}, nil
}
