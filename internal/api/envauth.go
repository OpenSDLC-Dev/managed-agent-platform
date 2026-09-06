package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// authenticateEnvironmentKey resolves a Bearer token to the environment it is
// scoped to and that environment's scope, or "" if the key is unknown, revoked,
// expired, or its environment sits in a workspace that is no longer live. Those
// take the same branch on purpose: the caller turns "" into one 401 with one
// message, so a probing client learns nothing about which of them it hit. A key
// minted before keys carried expiries has a NULL expires_at and never expires.
//
// The scope comes from the ENVIRONMENT's row, never from environment_keys' own
// reserved columns, so a key can never disagree with the environment it serves
// — no copy at mint time, no drift, no backfill (plan 42 §6.1). Those columns
// stay unread, said out loud so the omission reads as deliberate. The registry
// join is composite — org too — because org has one authority, the registry: an
// environment whose org_id drifted from its workspace's resolves to nothing.
func authenticateEnvironmentKey(ctx context.Context, pool *pgxpool.Pool, key string) (string, domain.Scope, error) {
	var envID string
	var scope domain.Scope
	err := pool.QueryRow(ctx,
		`SELECT k.environment_id, e.org_id, e.workspace_id, e.project_id
		   FROM environment_keys k
		   JOIN environments e ON e.id = k.environment_id
		   JOIN workspaces w ON w.id = e.workspace_id AND w.org_id = e.org_id AND w.archived_at IS NULL
		  WHERE k.key_hash = $1 AND k.revoked_at IS NULL
		    AND (k.expires_at IS NULL OR k.expires_at > now())`,
		hashKey(key)).Scan(&envID, &scope.OrgID, &scope.WorkspaceID, &scope.ProjectID)
	if err == pgx.ErrNoRows {
		return "", domain.Scope{}, nil
	}
	return envID, scope, err
}

// bearerToken extracts a non-empty Authorization: Bearer token. ok reports
// whether the header used the Bearer scheme at all — the dual-auth dispatcher
// keys the scheme off this, so a request with no Bearer header falls through to
// management auth rather than being rejected.
func bearerToken(r *http.Request) (token string, ok bool) {
	return strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// resolveEnvironmentKey authenticates a request's Authorization: Bearer
// environment key, returning the environment it is scoped to and the scope that
// environment resolves. On a missing/empty header, an unknown/revoked key or a
// workspace header it may not narrow to, it writes the wire error and returns
// ok=false. Both worker-auth middlewares share it so the Bearer-resolution rules
// live in one place.
//
// The tenancy headers are stamped HERE rather than in the middlewares, because
// the second caller is requireEnvironmentKeyForSession — the worker half of the
// dual-auth session routes, whose own ownership 404 would otherwise go out bare
// on exactly the routes a BYOC deployment uses most (plan 42 §6.2).
func resolveEnvironmentKey(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool) (envID string, scope domain.Scope, ok bool) {
	token, hasBearer := bearerToken(r)
	if !hasBearer || token == "" {
		writeError(w, r, errAuth("missing Authorization: Bearer environment key"))
		return "", domain.Scope{}, false
	}
	envID, bound, err := authenticateEnvironmentKey(r.Context(), pool, token)
	if err != nil {
		writeError(w, r, err)
		return "", domain.Scope{}, false
	}
	if envID == "" {
		writeError(w, r, errAuth("invalid environment key"))
		return "", domain.Scope{}, false
	}
	scope, err = selectWorkspace(r, []domain.Scope{bound})
	if err != nil {
		writeError(w, r, err)
		return "", domain.Scope{}, false
	}
	stampScope(w, scope)
	return envID, scope, true
}

// requireEnvironmentKey is the worker-auth middleware guarding the work API:
// every /work route needs a valid Authorization: Bearer environment key. The
// resolved environment is stored in the request context; handlers assert it
// matches the path's {id}, so a key scoped to one environment cannot drive
// another's queue.
func requireEnvironmentKey(pool *pgxpool.Pool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		envID, scope, ok := resolveEnvironmentKey(w, r, pool)
		if !ok {
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyEnvironment, envID)
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, ctxKeyScope, scope)))
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
		envID, scope, ok := resolveEnvironmentKey(w, r, pool)
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
		ctx := context.WithValue(r.Context(), ctxKeyEnvironment, envID)
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, ctxKeyScope, scope)))
	})
}

// environmentFrom returns the environment a worker's Bearer key authorised, or
// "" outside a worker-authenticated request.
func environmentFrom(ctx context.Context) string {
	e, _ := ctx.Value(ctxKeyEnvironment).(string)
	return e
}
