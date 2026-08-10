package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvironmentKeyTTL is how long an issued environment key authenticates. It
// matches the reference console's own year — its issuance response reports
// expires_in: 31536000 — and is not configurable: an operator wanting a
// shorter-lived credential revokes it, and the alternative, a per-key TTL, is a
// policy the reference does not offer and nobody has asked for.
const EnvironmentKeyTTL = 365 * 24 * time.Hour

// environmentKeySecretPrefix marks an issued key as this platform's. The
// silhouette follows the reference's `sk-ant-oat01-…` (plan 01: "format modeled
// on"), but the middle is deliberately ours: we are not Anthropic's OAuth
// infrastructure, and a key that reads like an Anthropic credential invites
// being pasted into ANTHROPIC_API_KEY. Nothing parses it — to every consumer,
// including the real `ant beta:worker`, the key is an opaque Bearer token.
const environmentKeySecretPrefix = "sk-map-env01-"

// environmentKeySecretBytes is the CSPRNG width behind the prefix: 256 bits,
// rendered as 43 unpadded base64url characters.
const environmentKeySecretBytes = 32

// The three primitives below take ids the server minted — an environment id from
// a validated path and a key id from a listing. Shape validation belongs at the
// HTTP edge, which rejects an id carrying an unstorable byte before it can bind
// into a query as a 500 (the rule checkID exists for); these are the storage
// layer under it, not the boundary.

// EnvironmentKey is an issued credential's metadata — everything about a key
// that outlives its issuance. The secret is not part of it: only the hash is
// stored, so no surface can render the key a second time.
type EnvironmentKey struct {
	ID        string
	Name      string
	CreatedAt time.Time
	// ExpiresAt is nil for a key minted before keys carried expiries; such a
	// key stays live until revoked, which is what it was promised on issue.
	ExpiresAt *time.Time
}

// IssueEnvironmentKey mints a worker credential for an environment and returns
// it in plaintext — the only moment it exists anywhere but the caller's memory,
// since the database receives nothing but its hash. name labels the key so an
// operator can tell one host's credential from another's when revoking one.
//
// An environment may hold any number of live keys: issuing does not disturb the
// ones already out on other hosts. That is the reference console's model, and
// the reason migration 0021 retired the one-live-key invariant.
//
// Issuance itself is a deliberate divergence: the reference mints environment
// keys on its console's private backend, with no public wire endpoint, so a
// self-hostable platform owns the primitive. The consuming side — resolving a
// Bearer token to its environment — stays wire-locked by the real
// `ant beta:worker`.
func IssueEnvironmentKey(ctx context.Context, pool *pgxpool.Pool, environmentID, name string) (string, error) {
	return issueEnvironmentKey(ctx, pool, environmentID, name)
}

// execer is the one method issuing needs, so the insert can run either on the
// pool (seeding, and the tests that do it) or inside a caller's transaction —
// which is how the HTTP route issues, holding FOR SHARE on the environment row
// across the check and the insert.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func issueEnvironmentKey(ctx context.Context, db execer, environmentID, name string) (string, error) {
	buf := make([]byte, environmentKeySecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("api: generate environment key: %w", err)
	}
	key := environmentKeySecretPrefix + base64.RawURLEncoding.EncodeToString(buf)
	// No ON CONFLICT clause: key_hash is UNIQUE, and against 256 bits of entropy
	// a collision is not a condition to recover from but a broken CSPRNG —
	// upserting over the incumbent row would hand this environment a credential
	// another environment is already authenticating with.
	//
	// Both timestamps come from the database clock (created_at defaults to now()),
	// so an expiry can never be minted already-past by a skewed application host.
	if _, err := db.Exec(ctx,
		`INSERT INTO environment_keys (id, environment_id, name, key_hash, expires_at)
		 VALUES ($1, $2, $3, $4, now() + make_interval(secs => $5))`,
		domain.NewID(domain.PrefixEnvironmentKey).String(), environmentID, name, hashKey(key),
		EnvironmentKeyTTL.Seconds()); err != nil {
		return "", err
	}
	return key, nil
}

// ListEnvironmentKeys returns an environment's un-revoked credentials, newest
// first, with the total that matched so the caller can page. A revoked key is
// omitted rather than shown as retired — revocation is final, and the reference
// console drops the row too. An expired key is still listed: an operator whose
// worker has stopped connecting needs to see the credential it is failing on.
func ListEnvironmentKeys(ctx context.Context, pool *pgxpool.Pool, environmentID string, limit, offset int) ([]EnvironmentKey, int, error) {
	// The count and the page are rendered together, so they are read together:
	// under READ COMMITTED each statement takes its own snapshot, and a key
	// issued or revoked between them would leave the two disagreeing — a total
	// of three above a page holding four. One repeatable-read snapshot removes
	// the window; the transaction is read-only and holds no row locks.
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var total int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM environment_keys WHERE environment_id = $1 AND revoked_at IS NULL`,
		environmentID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := tx.Query(ctx,
		// id breaks ties so the order is total rather than merely partial: two
		// keys issued inside one clock tick would otherwise sort arbitrarily, and
		// a page taken twice could show them in either order. It does not make
		// offset paging stable across concurrent writes — nothing but a keyset
		// cursor does — which is a cost the mirrored dialect's offset paging
		// carries and an operator listing a handful of hosts will not notice.
		`SELECT id, name, created_at, expires_at FROM environment_keys
		  WHERE environment_id = $1 AND revoked_at IS NULL
		  ORDER BY created_at DESC, id DESC
		  LIMIT $2 OFFSET $3`,
		environmentID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	keys := []EnvironmentKey{}
	for rows.Next() {
		var k EnvironmentKey
		if err := rows.Scan(&k.ID, &k.Name, &k.CreatedAt, &k.ExpiresAt); err != nil {
			return nil, 0, err
		}
		keys = append(keys, k)
	}
	return keys, total, rows.Err()
}

// RevokeEnvironmentKey retires one key and leaves the environment's others
// authenticating. found reports whether the environment held such a key at all,
// which is how the caller tells a real revocation from a 404 — a key id that
// belongs to another environment is simply not found here, so revocation cannot
// reach across environments or confirm that an id exists elsewhere.
//
// Revoking an already-revoked key succeeds and still reports found, so a retry
// is not an error; coalesce keeps the original revocation timestamp rather than
// sliding it forward on every repeat.
func RevokeEnvironmentKey(ctx context.Context, pool *pgxpool.Pool, environmentID, keyID string) (found bool, err error) {
	tag, err := pool.Exec(ctx,
		`UPDATE environment_keys SET revoked_at = coalesce(revoked_at, now())
		  WHERE id = $1 AND environment_id = $2`, keyID, environmentID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
