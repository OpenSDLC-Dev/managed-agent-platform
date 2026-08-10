package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// authenticateEnvironmentKey resolves a Bearer token to the environment it is
// scoped to, or "" if the key is unknown, revoked, or expired. Those three take
// the same branch on purpose: the caller turns "" into one 401 with one message,
// so a probing client learns nothing about which of them it hit. A key minted
// before keys carried expiries has a NULL expires_at and never expires.
func authenticateEnvironmentKey(ctx context.Context, pool *pgxpool.Pool, key string) (string, error) {
	var envID string
	err := pool.QueryRow(ctx,
		`SELECT environment_id FROM environment_keys
		  WHERE key_hash = $1 AND revoked_at IS NULL
		    AND (expires_at IS NULL OR expires_at > now())`,
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

// requireEnvironmentKeyForSession is the worker-auth middleware for a session's
// worker-facing routes (GET/POST .../events, GET .../events/stream, and the GET
// /v1/sessions/{id} read): a BYOC worker drives its own session over the same
// Authorization: Bearer environment key it polls the work queue with. The key
// must be valid AND the target session must belong to its environment. For a
// given id, a session in another environment and a session that does not exist
// take the identical branch and return the same 404 (status, type, message), so
// a worker probing an id cannot tell "exists elsewhere" from "does not exist" —
// it can neither reach another environment's sessions nor learn they exist.
// Mutating session CRUD stays management-only; only these worker routes are
// dual-auth.
func requireEnvironmentKeyForSession(pool *pgxpool.Pool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		envID, ok := resolveEnvironmentKey(w, r, pool)
		if !ok {
			return
		}
		// Extract the id from the decoded path so that, for a real (slashless)
		// session id, it matches what the routed handler reads via PathValue. The
		// two diverge only for an id that encodes a %2F, which is never a real
		// session, so the handler 404s either way (see splitSession).
		id, _, _ := splitSession(r.URL.Path)
		sid := normalizeSessionID(id)
		// A malformed session id (an unstorable byte, a wrong prefix/alphabet)
		// cannot name a stored session; reject it on shape here, before it binds
		// into this ownership lookup as a 500 — the same 404 an absent or
		// other-environment session gets, so a worker still cannot probe ids.
		if err := checkID(sid, "session"); err != nil {
			writeError(w, r, err)
			return
		}
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
