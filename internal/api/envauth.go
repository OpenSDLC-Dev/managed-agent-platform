package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnsureEnvironmentKey makes key the one live worker credential for an
// environment: it inserts (or un-revokes) the hash and revokes every other
// unrevoked key for the same environment_id. That gives one live
// Authorization: Bearer credential per environment's work queue, with
// rotation-by-re-mint semantics (registering a fresh value revokes the prior
// one). Only the hash is stored.
//
// Issuance is a deliberate divergence: the reference mints environment keys in
// its console with no public wire endpoint, so a self-hostable platform owns
// this provisioning primitive. The consuming side — resolving a Bearer token to
// its environment — stays wire-locked by the real `ant beta:worker` client.
func EnsureEnvironmentKey(ctx context.Context, pool *pgxpool.Pool, environmentID, key string) error {
	hash := hashKey(key)
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`INSERT INTO environment_keys (id, environment_id, key_hash) VALUES ($1, $2, $3)
		 ON CONFLICT (key_hash) DO UPDATE SET revoked_at = NULL, environment_id = EXCLUDED.environment_id`,
		domain.NewID("envkey").String(), environmentID, hash); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE environment_keys SET revoked_at = now()
		 WHERE environment_id = $1 AND key_hash <> $2 AND revoked_at IS NULL`,
		environmentID, hash); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// authenticateEnvironmentKey resolves a Bearer token to the environment it is
// scoped to, or "" if the key is unknown or revoked.
func authenticateEnvironmentKey(ctx context.Context, pool *pgxpool.Pool, key string) (string, error) {
	var envID string
	err := pool.QueryRow(ctx,
		`SELECT environment_id FROM environment_keys WHERE key_hash = $1 AND revoked_at IS NULL`,
		hashKey(key)).Scan(&envID)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	return envID, err
}

// requireEnvironmentKey is the worker-auth middleware guarding the work API:
// every /work route needs a valid Authorization: Bearer environment key. The
// resolved environment is stored in the request context; handlers assert it
// matches the path's {id}, so a key scoped to one environment cannot drive
// another's queue.
func requireEnvironmentKey(pool *pgxpool.Pool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		token, ok := strings.CutPrefix(auth, "Bearer ")
		if !ok || token == "" {
			writeError(w, r, errAuth("missing Authorization: Bearer environment key"))
			return
		}
		envID, err := authenticateEnvironmentKey(r.Context(), pool, token)
		if err != nil {
			writeError(w, r, err)
			return
		}
		if envID == "" {
			writeError(w, r, errAuth("invalid environment key"))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyEnvironment, envID)))
	})
}

// environmentFrom returns the environment a worker's Bearer key authorised, or
// "" outside a worker-authenticated request.
func environmentFrom(ctx context.Context) string {
	e, _ := ctx.Value(ctxKeyEnvironment).(string)
	return e
}
