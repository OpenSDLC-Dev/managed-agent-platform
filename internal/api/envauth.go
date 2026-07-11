package api

import (
	"context"
	"errors"
	"fmt"
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
	// Insert or un-revoke this value. A key value is bound to one environment
	// for life: on conflict we never re-point it (rebinding environment_id would
	// silently move a live worker credential to a different environment). If the
	// value already belongs to another environment, reject rather than escalate.
	var boundEnv string
	if err := tx.QueryRow(ctx,
		`INSERT INTO environment_keys (id, environment_id, key_hash) VALUES ($1, $2, $3)
		 ON CONFLICT (key_hash) DO UPDATE SET revoked_at = NULL
		 RETURNING environment_id`,
		domain.NewID("envkey").String(), environmentID, hash).Scan(&boundEnv); err != nil {
		return err
	}
	if boundEnv != environmentID {
		return fmt.Errorf("api: environment key value is already bound to a different environment")
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

// bearerToken extracts a non-empty Authorization: Bearer token. ok reports
// whether the header used the Bearer scheme at all — the dual-auth dispatcher
// keys the scheme off this, so a request with no Bearer header falls through to
// management auth rather than being rejected.
func bearerToken(r *http.Request) (token string, ok bool) {
	return strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// resolveEnvironmentKey authenticates a request's Authorization: Bearer
// environment key, returning the environment it is scoped to. On a missing/empty
// header or an unknown/revoked key it writes the wire auth error and returns
// ok=false. Both worker-auth middlewares share it so the Bearer-resolution rules
// live in one place.
func resolveEnvironmentKey(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool) (envID string, ok bool) {
	token, hasBearer := bearerToken(r)
	if !hasBearer || token == "" {
		writeError(w, r, errAuth("missing Authorization: Bearer environment key"))
		return "", false
	}
	envID, err := authenticateEnvironmentKey(r.Context(), pool, token)
	if err != nil {
		writeError(w, r, err)
		return "", false
	}
	if envID == "" {
		writeError(w, r, errAuth("invalid environment key"))
		return "", false
	}
	return envID, true
}

// requireEnvironmentKey is the worker-auth middleware guarding the work API:
// every /work route needs a valid Authorization: Bearer environment key. The
// resolved environment is stored in the request context; handlers assert it
// matches the path's {id}, so a key scoped to one environment cannot drive
// another's queue.
func requireEnvironmentKey(pool *pgxpool.Pool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		envID, ok := resolveEnvironmentKey(w, r, pool)
		if !ok {
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyEnvironment, envID)))
	})
}

// requireEnvironmentKeyForSession is the worker-auth middleware for the session
// events subtree (GET/POST .../events and GET .../events/stream): a BYOC worker
// drives its own session over the same Authorization: Bearer environment key it
// polls the work queue with. The key must be valid AND the target session must
// belong to its environment — a session in another environment (or one that does
// not exist) is not-found, so a worker can neither read nor write another
// environment's sessions and cross-environment existence never leaks. Session
// CRUD stays management-only; only these event routes are dual-auth.
func requireEnvironmentKeyForSession(pool *pgxpool.Pool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		envID, ok := resolveEnvironmentKey(w, r, pool)
		if !ok {
			return
		}
		sid := normalizeSessionID(sessionIDFromEventsPath(r.URL.Path))
		var sessEnv string
		err := pool.QueryRow(r.Context(),
			`SELECT environment_id FROM sessions WHERE id = $1`, sid).Scan(&sessEnv)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && sessEnv != envID) {
			writeError(w, r, errNotFound("session %s not found", sid))
			return
		}
		if err != nil {
			writeError(w, r, err)
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
