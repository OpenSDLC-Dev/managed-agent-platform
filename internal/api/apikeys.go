package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
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
	// CreatedBy is nil for a key nobody issued — one seeded from
	// CONTROLPLANE_API_KEY. That is the same predicate api_keys_one_live_unissued
	// keys on, so "nil issuer" and "env-var-managed" are one fact, not two.
	CreatedBy *string
	// ExpiresAt is nil for a key that never expires: the console's own "Never".
	ExpiresAt *time.Time
}

// managementKeyColumns is the single spelling of a key's projection, shared by
// every read so the derived status cannot drift between them.
//
// The `expired` derivation is the exact complement of the one in authenticate
// (`expires_at IS NULL OR expires_at > now()`): a key reads `expired` precisely
// when that query would refuse it for having lapsed. Both run on the database's
// clock, so a listing can never show `active` for a key the very next request is
// about to reject.
//
// Only an `active` row derives to `expired`. A key an operator disabled reports
// `inactive` even past its expiry, because that is the operator's own action and
// the more useful of the two answers; the reference's console does not
// distinguish them either — its "not usable" marker covers both — so nothing is
// contradicted. Recorded as INFERRED in docs/DIVERGENCES.md.
const managementKeyColumns = `id, name,
	CASE WHEN status = 'active' AND expires_at IS NOT NULL AND expires_at <= now()
	     THEN 'expired' ELSE status END,
	partial_key_hint, created_at, created_by, expires_at`

func scanManagementKey(row pgx.Row) (ManagementKey, error) {
	var k ManagementKey
	err := row.Scan(&k.ID, &k.Name, &k.Status, &k.PartialKeyHint,
		&k.CreatedAt, &k.CreatedBy, &k.ExpiresAt)
	return k, err
}

// IssueManagementKey mints a management credential and returns it in plaintext —
// the only moment it exists anywhere but the caller's memory, since the database
// receives nothing but its hash — alongside the row a caller renders.
//
// createdBy must be non-empty, and every caller is an authenticated route, so it
// always is: it is the `principal_` id of the human who issued the key over SSO,
// or the `apikey_` id of the machine credential that did. Beyond audit it is
// load-bearing, because non-NULL is exactly what puts this row **outside**
// api_keys_one_live_unissued — issued keys may share a name, as the reference
// allows, while EnsureAPIKey keeps one-live-per-name over the rows it owns.
//
// expiresAt is the caller's choice and may be nil, which means never. That is a
// deliberate divergence from environment keys' server-fixed EnvironmentKeyTTL,
// and it is the reference's: an operator issuing a management credential picks
// its lifetime; a worker credential's lifetime is the platform's business.
func IssueManagementKey(ctx context.Context, pool *pgxpool.Pool, name string, expiresAt *time.Time, createdBy string) (string, ManagementKey, error) {
	// Enforced rather than asserted, because two code paths read this field for
	// the same fact and would disagree about a blank one: the listing renders an
	// empty issuer as `created_by: null`, which is the documented signal for "this
	// row belongs to CONTROLPLANE_API_KEY and is not the console's to change",
	// while the update route tests for SQL NULL and would happily rename it. A
	// blank issuer would also sit under api_keys_one_live_unissued's complement —
	// non-NULL — so the row would be outside the one-live rule while claiming, in
	// every listing, to be inside it.
	if createdBy == "" {
		return "", ManagementKey{}, fmt.Errorf("api: issue management key: no issuer")
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
	// mint a key that is already expired or that outlives what it was promised.
	//
	// The WHERE is the second half of that: the route rejects a past expires_at
	// against the *process* clock, which is the right place for a friendly 400 but
	// cannot be authoritative. An instant a few milliseconds ahead passes that
	// check and can still lapse before this statement runs — a skewed host makes it
	// systematic rather than unlucky — and the row would be born already refused by
	// authenticate, having answered 200 with a key in it. Deciding on the database
	// clock costs nothing here: no row is written, and the caller turns the missing
	// row back into the same 400. ErrNoRows is therefore not an impossible state,
	// which is why it is named rather than folded into err.
	row := pool.QueryRow(ctx,
		`INSERT INTO api_keys (id, name, key_hash, partial_key_hint, created_by, expires_at)
		 SELECT $1, $2, $3, $4, $5, $6::timestamptz
		  WHERE $6::timestamptz IS NULL OR $6::timestamptz > now()
		 RETURNING `+managementKeyColumns,
		domain.NewID(domain.PrefixAPIKey).String(), name, hashKey(key), partialKeyHint(key),
		createdBy, expiresAt)
	k, err := scanManagementKey(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ManagementKey{}, errExpiryLapsed
	}
	if err != nil {
		return "", ManagementKey{}, err
	}
	return key, k, nil
}

// errExpiryLapsed reports that the requested expiry was still in the future when
// the route validated it and no longer was when the row would have been written.
var errExpiryLapsed = errors.New("api: requested expires_at lapsed before the key was written")

// updateManagementKey applies a status and/or name patch and returns the row as
// a listing would render it. A nil argument leaves that column alone, which is
// what makes a name-only patch possible without a read-modify-write.
//
// It takes a transaction rather than the pool because the caller's decisions —
// whether the row is env-var-managed, archived, or lapsed — are read under
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
// Every one, including archived rows and including the key seeded from
// CONTROLPLANE_API_KEY. Archived rows are returned because the reference returns
// them — its console filters them client-side — and because an archive that
// erased the row would erase the evidence that a credential once existed. The
// bootstrap key is returned because it is a management key: a listing that hid
// the credential the operator is authenticating with would be lying about what
// can reach this API. It renders with a null issuer, which is the honest answer
// to "who created this" for a key an environment variable did.
//
// No paging. The reference's console list is a bare array with no pagination at
// all, and this table holds one row per issued credential plus the bootstrap
// one — an operator-paced count, not a growing log.
//
// And no index, which settles a question migration 0024 left open. Its closing
// comment declined `api_keys (created_at DESC)` as build-ahead and said "the
// listing that would want one lands in slice 2"; the listing has landed and does
// not want one. A migration is immutable once merged, so the reconsideration has
// to be recorded where a reader will meet it rather than by editing that file:
// the sort is over an operator-paced table with no growth driver — issuance is a
// deliberate admin action, and nothing else writes rows — so an index would be
// paid for on every write to speed a read that has nothing to scan. If a
// deployment ever does accumulate keys, the missing bound is the *response size*,
// which an index does not fix; that would be paging, and the reference's own
// surface has none to mirror.
func ListManagementKeys(ctx context.Context, pool *pgxpool.Pool) ([]ManagementKey, error) {
	// id breaks ties so the order is total rather than merely partial: two keys
	// issued inside one clock tick would otherwise sort arbitrarily, and the same
	// listing taken twice could show them in either order.
	rows, err := pool.Query(ctx,
		`SELECT `+managementKeyColumns+` FROM api_keys ORDER BY created_at DESC, id DESC`)
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
