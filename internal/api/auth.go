package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"

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

// EnsureAPIKey makes key the one live credential for the named logical key:
// it inserts (or un-revokes) the hash and revokes every other unrevoked key
// under the same name. That gives rotation-by-restart semantics — changing
// CONTROLPLANE_API_KEY and restarting cmd/controlplane revokes the previous
// key instead of leaving it valid forever. All replicas must therefore share
// one key value per name.
func EnsureAPIKey(ctx context.Context, pool *pgxpool.Pool, name, key string) error {
	hash := hashKey(key)
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`INSERT INTO api_keys (id, name, key_hash) VALUES ($1, $2, $3)
		 ON CONFLICT (key_hash) DO UPDATE SET revoked_at = NULL, name = EXCLUDED.name`,
		domain.NewID("apikey").String(), name, hash); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE api_keys SET revoked_at = now()
		 WHERE name = $1 AND key_hash <> $2 AND revoked_at IS NULL`,
		name, hash); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// authenticate resolves an x-api-key value to the key's row ID, or "" if the
// key is unknown or revoked.
func authenticate(ctx context.Context, pool *pgxpool.Pool, key string) (string, error) {
	var id string
	err := pool.QueryRow(ctx,
		`SELECT id FROM api_keys WHERE key_hash = $1 AND revoked_at IS NULL`,
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

func principalFrom(ctx context.Context) string {
	p, _ := ctx.Value(ctxKeyPrincipal).(string)
	return p
}
