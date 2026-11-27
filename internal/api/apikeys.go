package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/apikey"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The three states an operator may set, and the fourth the server computes.
//
// The settable set is the reference's own, quoted from the 400 its server
// returned to a `{"status":"deleted"}` probe: "status: Input should be 'active',
// 'inactive' or 'archived'" (#378). `expired` is **rendered, never stored** —
// storing it would need a sweeper to keep true, and a key that lapsed while the
// sweeper was down would keep authenticating.
const (
	KeyStatusActive   = "active"
	KeyStatusInactive = "inactive"
	KeyStatusArchived = "archived"
	KeyStatusExpired  = "expired"
)

// issuedKeySecretBytes is the CSPRNG width behind IssuedKeyPrefix: 256 bits,
// rendered as 43 unpadded base64url characters — the same width plan 30 gave
// environment keys, for the same reason.
const issuedKeySecretBytes = 32

// ManagementKey is an `x-api-key` credential's metadata: everything about a key
// that outlives its issuance. The secret is not part of it — only the hash is
// stored, so no surface can render the key a second time, and PartialKeyHint is
// the only trace of the plaintext that survives.
type ManagementKey struct {
	ID   string
	Name string
	// Status is the rendered status, so it may read `expired`, which is not a
	// value any row holds.
	Status         string
	PartialKeyHint string
	CreatedAt      time.Time
	// CreatedBy is nil only on historical, unissued rows.
	CreatedBy *string
	// ExpiresAt is nil for a key that never expires: the console's own "Never".
	ExpiresAt *time.Time
	// Owner and creator are independent. OwnerKind comes from the principal row.
	OwnerPrincipalID *string
	OwnerKind        string
}

// managementKeyColumns is the single spelling of a key's projection, shared by
// every read so the derived status cannot drift between them.
//
// Expiry is compared against the database clock, as in authenticate. A projection
// describes that instant only: a key can lapse before the next request.
//
// The precedence is archived > expired > the stored status, and both halves were
// measured on the reference (#389, 2026-08-13). A key disabled while live and
// then left to lapse lists as `expired`, not `inactive` — the clock outranks the
// operator's own action, which is the opposite of what this platform inferred
// before the probe. And a key archived after its expiry had passed lists as
// `archived`, not `expired` — retirement is the more final fact, and it is the
// one an operator acted on.
//
// Use clock_timestamp(), not transaction-start now(): an authority-lock wait can
// outlast the key's expiry before INSERT RETURNING projects it.
const managementKeyColumns = `id, name,
	CASE WHEN status <> 'archived' AND expires_at IS NOT NULL AND expires_at <= clock_timestamp()
	     THEN 'expired' ELSE status END AS status,
	partial_key_hint, created_at, created_by, expires_at, owner_principal_id,
	coalesce((SELECT kind FROM principals WHERE principals.id = api_keys.owner_principal_id), '')`

func scanManagementKey(row pgx.Row) (ManagementKey, error) {
	var k ManagementKey
	err := row.Scan(&k.ID, &k.Name, &k.Status, &k.PartialKeyHint,
		&k.CreatedAt, &k.CreatedBy, &k.ExpiresAt, &k.OwnerPrincipalID, &k.OwnerKind)
	return k, err
}

// issueManagementKey also accepts the transaction holding the issuer's authority.
func issueManagementKey(ctx context.Context, db principal.Querier, name string, expiresAt *time.Time, createdBy string, ownerID *string) (string, ManagementKey, error) {
	if createdBy == "" || ownerID == nil || *ownerID == "" {
		return "", ManagementKey{}, fmt.Errorf("api: issue management key: an issuer and owner are required")
	}
	buf := make([]byte, issuedKeySecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", ManagementKey{}, fmt.Errorf("api: generate management key: %w", err)
	}
	key := IssuedKeyPrefix + base64.RawURLEncoding.EncodeToString(buf)
	// No ON CONFLICT clause, for the reason issueEnvironmentKey gives: against 256
	// bits of entropy a key_hash collision is a broken CSPRNG, not a condition to
	// recover from, and upserting would hand this caller a credential another row
	// is already authenticating with.
	//
	// created_at comes from the column default, i.e. the database clock — the same
	// clock the expiry is compared against, so a skewed application host cannot
	// mint a key that outlives what it was promised.
	//
	// A past expires_at is written like any other. This platform used to refuse it,
	// and guarded the refusal here against the database clock so a value that
	// lapsed between the route's check and the insert could not slip through. The
	// reference accepts it (#389): posting an instant in 2020 answers 200 with a
	// resource already reporting `expired`. Nothing needs guarding once the value
	// is allowed to be in the past — the row is simply born in the derived state,
	// which the credential path refuses exactly as it refuses any lapsed key.
	digest := apikey.Hash(key)
	row := db.QueryRow(ctx,
		`INSERT INTO api_keys (id, name, owned_key_hash, partial_key_hint, created_by, expires_at, owner_principal_id, workspace_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, 'default')
		 RETURNING `+managementKeyColumns,
		domain.NewID(domain.PrefixAPIKey).String(), name, digest, partialKeyHint(key),
		createdBy, expiresAt, *ownerID)
	k, err := scanManagementKey(row)
	if err != nil {
		return "", ManagementKey{}, err
	}
	return key, k, nil
}

// updateManagementKey applies a status and/or name patch and returns the row as
// a listing would render it. A nil argument leaves that column alone, which is
// what makes a name-only patch possible without a read-modify-write.
//
// It takes a transaction rather than the pool because the caller's decisions —
// whether the row is historical, archived, or lapsed — are read under
// FOR UPDATE and must not be separated from the write that acts on them. Keeping
// the statement here rather than in the route is what stops the projection from
// being spelled twice: a second caller copying SQL is how managementKeyColumns
// stops being the single spelling it exists to be.
func updateManagementKey(ctx context.Context, tx pgx.Tx, id string, status, name *string) (ManagementKey, error) {
	return scanManagementKey(tx.QueryRow(ctx,
		`UPDATE api_keys SET status = coalesce($2, status), name = coalesce($3, name)
		  WHERE id = $1
		  RETURNING `+managementKeyColumns, id, status, name))
}

// ListManagementKeys returns every management key, newest first.
//
// Historical and archived rows remain visible for audit attribution.
// A historical unowned row no longer authenticates.
//
// No paging: the reference's console list is a bare array with no pagination.
// Developers and admins can issue keys, so neither table nor response size is
// bounded. Owner filtering limits the developer query, not the admin response.
func ListManagementKeys(ctx context.Context, pool *pgxpool.Pool) ([]ManagementKey, error) {
	return listManagementKeys(ctx, pool, "")
}

// An empty ownerID preserves the full administrative listing.
func listManagementKeys(ctx context.Context, pool *pgxpool.Pool, ownerID string) ([]ManagementKey, error) {
	query := `SELECT ` + managementKeyColumns + ` FROM api_keys`
	var args []any
	if ownerID != "" {
		query += ` WHERE owner_principal_id = $1`
		args = append(args, ownerID)
	}
	// id breaks ties so the order is total rather than merely partial: two keys
	// issued inside one clock tick would otherwise sort arbitrarily, and the same
	// listing taken twice could show them in either order.
	rows, err := pool.Query(ctx, query+` ORDER BY created_at DESC, id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := []ManagementKey{}
	for rows.Next() {
		k, err := scanManagementKey(rows)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}
