package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// hashKey derives the stored form of an API key. Only this hash ever touches
// the database.
func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// partialKeyHint is the masked form a listing shows: the value's leading run up
// to and including its last '-', then three characters, an ellipsis, and the
// final four. It mirrors the reference's `sk-ant-api03-R2D...igAA`.
//
// It is computed from the plaintext because it cannot be recovered from the hash,
// and it is the only part of a key that outlives issuance. A value too short to
// mask that way is reported as "" rather than leaked in full — an operator-chosen
// CONTROLPLANE_API_KEY may be anything, including something short enough that
// "prefix plus last four" would print most of it.
func partialKeyHint(key string) string {
	const lead, tail = 3, 4
	prefix := ""
	if i := strings.LastIndex(key, "-"); i >= 0 {
		prefix = key[:i+1]
	}
	rest := key[len(prefix):]
	if len(rest) < lead+tail+1 {
		return ""
	}
	return prefix + rest[:lead] + "..." + rest[len(rest)-tail:]
}

// EnsureAPIKey makes key the one live credential for the named logical key:
// it inserts (or reactivates) the hash and archives every other live key
// under the same name. That gives rotation-by-restart semantics — changing
// CONTROLPLANE_API_KEY and restarting cmd/controlplane retires the previous
// key instead of leaving it valid forever. All replicas must therefore share
// one key value per name; replicas booting with *different* values for one name
// race, and api_keys_one_live_unissued resolves that by failing the loser's
// transaction rather than leaving the name with two live credentials.
//
// It only ever writes rows with created_by NULL, which is what puts them under
// that index and marks them env-var-managed. A key issued over the console
// records its issuer and is deliberately outside the one-live rule (plan 32).
func EnsureAPIKey(ctx context.Context, pool *pgxpool.Pool, name, key string) error {
	hash := hashKey(key)
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Archive before inserting: api_keys_one_live_unissued admits one active
	// unissued row per name and Postgres enforces it per statement, so
	// registering the replacement while the incumbent is still live would fail
	// every rotation.
	if _, err := tx.Exec(ctx,
		`UPDATE api_keys SET status = 'archived'
		 WHERE name = $1 AND key_hash <> $2 AND status = 'active' AND created_by IS NULL`,
		name, hash); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO api_keys (id, name, key_hash, partial_key_hint) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (key_hash) DO UPDATE
		 SET status = 'active', name = EXCLUDED.name, partial_key_hint = EXCLUDED.partial_key_hint`,
		domain.NewID("apikey").String(), name, hash, partialKeyHint(key)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// authenticate resolves an x-api-key value to the key's row ID, or "" if the key
// is unknown, not active, or past its expiry.
//
// Expiry is evaluated here rather than swept: a key whose expires_at has passed
// stops authenticating the moment it passes, with no background job to be down.
// The comparison is against the database's clock, the same one that stamped
// created_at, so a control-plane replica with a skewed clock cannot extend or
// shorten a credential's life.
func authenticate(ctx context.Context, pool *pgxpool.Pool, key string) (string, error) {
	var id string
	err := pool.QueryRow(ctx,
		`SELECT id FROM api_keys
		 WHERE key_hash = $1 AND status = 'active'
		   AND (expires_at IS NULL OR expires_at > now())`,
		hashKey(key)).Scan(&id)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	return id, err
}

// requireAPIKey is the management-auth middleware: every /v1 route needs a
// valid, unrevoked x-api-key. The authenticated key's ID is stored in the
// request context as the audit principal (sessions.created_by).
func requireAPIKey(pool *pgxpool.Pool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A repeated field is refused before the value is read. HTTP allows one,
		// no real client sends one, and Header.Get would silently pick the first —
		// so without this the answer to "which key authenticated?" would depend on
		// header order, and it would differ from the answer apiKeyOffered gives
		// when choosing the lane. One rule in both places: a duplicate credential
		// is ambiguous, and ambiguous is a 401.
		if len(r.Header.Values("x-api-key")) > 1 {
			writeError(w, r, errAuth("multiple x-api-key headers"))
			return
		}
		key := r.Header.Get("x-api-key")
		if key == "" {
			writeError(w, r, errAuth("missing x-api-key header"))
			return
		}
		principal, err := authenticate(r.Context(), pool, key)
		if err != nil {
			writeError(w, r, err)
			return
		}
		if principal == "" {
			writeError(w, r, errAuth("invalid x-api-key"))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyPrincipal, principal)))
	})
}

// principalFrom is the audit answer to "who made this request" — the value
// sessions.created_by records. It resolves either lane's principal: the api key's
// name on the machine lane, and the human's `principal_` id on the identity lane
// (plan 31 slice 2, #56).
//
// Reading only ctxKeyPrincipal was the pre-plan-31 shape, when a machine key was
// the only thing that could reach a mutation. Left that way, the moment slice 3
// lets a human create a session the row would record NO creator at all — silently,
// since created_by is nullable and nothing checks it. The whole point of a stable
// `principal_` id is that an audit trail can name the human it belongs to.
//
// The machine lane wins when both are somehow set, matching dispatch: only one is
// ever populated today, and if that ever changed, the credential that
// authenticated the request is the machine one.
func principalFrom(ctx context.Context) string {
	if p, _ := ctx.Value(ctxKeyPrincipal).(string); p != "" {
		return p
	}
	if p, ok := identityFrom(ctx); ok {
		return p.ID
	}
	return ""
}
