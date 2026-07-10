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

// EnsureAPIKey inserts a management API key (hashed) if it is not already
// present. It is the bootstrap path: cmd/controlplane calls it at startup
// with the operator-configured key, and tests use it to seed credentials.
func EnsureAPIKey(ctx context.Context, pool *pgxpool.Pool, name, key string) error {
	_, err := pool.Exec(ctx,
		`INSERT INTO api_keys (id, name, key_hash) VALUES ($1, $2, $3)
		 ON CONFLICT (key_hash) DO NOTHING`,
		domain.NewID("apikey").String(), name, hashKey(key))
	return err
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
