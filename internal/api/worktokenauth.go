package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/worktoken"
)

// The sessions-token lane (plan 36 decision 15). A wtk_ Bearer, minted at poll
// time for an item whose session attaches a memory store, is the reference
// worker's credential for that item, and this lane admits it exactly where the
// v1.66.0 worker sends it: the item's own heartbeat and stop, its own session's
// read and events (list, stream, send), the skill reads (workspace-global, as
// they are for the environment key), and the memories of the stores its
// session attaches — list, create, get, update, delete, the five calls the
// worker's memory sync makes. Every other route refuses it — the rest of the
// work API stays the environment key's; a store's own read, its versions, its
// lifecycle and every management route stay the management key's. The lane is chosen by path family and token
// shape (isWorkTokenBearer): a wtk_ prefix is what an environment key
// (sk-map-env01-…) and a JWT (two dots) can never carry, so the dispatcher
// tries it first and nothing else is misrouted.

// isWorkTokenBearer reports whether the request offers a sessions token: a
// Bearer with the wtk_ prefix and no usable x-api-key beside it (the reference
// worker deletes X-Api-Key before attaching the token; a real management key
// alongside means a management caller, as dualAuth rules).
func isWorkTokenBearer(r *http.Request) bool {
	token, ok := bearerToken(r)
	return ok && !apiKeyOffered(r) && strings.HasPrefix(token, worktoken.TokenPrefix)
}

// isWorkTokenPath reports whether p is in a family the lane serves at all —
// the work API, a session's worker-facing routes, the skill reads, the memory
// stores. Within a family the lane refuses what the token does not authorize;
// outside them a wtk_ Bearer falls to the management lane's 401.
func isWorkTokenPath(r *http.Request, p string) bool {
	return isWorkPath(p) || isSessionEventsPath(p) ||
		(r.Method == http.MethodGet && (isBareSessionPath(p) || isSkillReadPath(p))) ||
		isMemoryStorePath(p)
}

// isMemoryStorePath reports whether p is under one store: /v1/memory_stores/{id}
// and everything beneath it. The collection routes are not.
func isMemoryStorePath(p string) bool {
	return strings.HasPrefix(p, "/v1/memory_stores/") && len(p) > len("/v1/memory_stores/")
}

// splitWork parses /v1/environments/{env}/work/{work_id}/{action} (isWorkPath
// already held), returning empty strings for the parts that are absent.
func splitWork(p string) (env, work, action string) {
	rest := strings.TrimPrefix(p, "/v1/environments/")
	env, rest, _ = strings.Cut(rest, "/")
	rest = strings.TrimPrefix(rest, "work")
	rest = strings.TrimPrefix(rest, "/")
	work, action, _ = strings.Cut(rest, "/")
	return env, work, action
}

// splitMemoryStore parses /v1/memory_stores/{id}[/{rest}] (isMemoryStorePath
// already held).
func splitMemoryStore(p string) (storeID, rest string) {
	storeID, rest, _ = strings.Cut(strings.TrimPrefix(p, "/v1/memory_stores/"), "/")
	return storeID, rest
}

// memoryRouteForWorker reports whether a method and a path beneath a store are
// one of the five memory calls the v1.66.0 reference worker makes with its
// token (lib/environments/memories.go): the memories' list and create, and a
// memory's get, update and delete. Nothing else beneath a store is — not the
// store's own read, not its versions (their history and actors are the
// management key's to read), not its lifecycle, not a redaction.
func memoryRouteForWorker(method, rest string) bool {
	segs := strings.Split(rest, "/")
	if segs[0] != "memories" {
		return false
	}
	switch len(segs) {
	case 1:
		return method == http.MethodGet || method == http.MethodPost
	case 2:
		return method == http.MethodGet || method == http.MethodPost || method == http.MethodDelete
	}
	return false
}

// requireWorkToken is the lane: it resolves the token (a 401 when it is
// unknown or no longer names a live item — worktoken.Authenticate's join
// conditions), then admits the request only on the route family the token's
// principal reaches. A session other than its own — a sibling in the same
// environment included — and an unattached store answer the not-found the
// environment-key lane gives for another environment's session, so a worker
// can neither reach them nor learn they exist. The request continues with the
// environment in context (workScope's check) and the session for the memory
// handlers' attribution.
func requireWorkToken(pool *pgxpool.Pool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, _ := bearerToken(r)
		principal, err := worktoken.Authenticate(r.Context(), pool, token)
		if err != nil {
			writeError(w, r, err)
			return
		}
		if principal.WorkID == "" {
			writeError(w, r, errAuth("invalid sessions token"))
			return
		}
		refused := errAuth("the sessions token does not authorize this route")
		p := r.URL.EscapedPath()
		switch {
		case isWorkPath(p):
			env, work, action := splitWork(p)
			if r.Method != http.MethodPost || env != principal.EnvironmentID || work != principal.WorkID ||
				(action != "heartbeat" && action != "stop") {
				writeError(w, r, refused)
				return
			}
		case isSessionEventsPath(p), isBareSessionPath(p):
			// The decoded path's id, as requireEnvironmentKeyForSession reads
			// it, so the handler and this check agree on the session.
			id, _, _ := splitSession(r.URL.Path)
			sid := normalizeSessionID(id)
			if err := checkID(sid, "session"); err != nil {
				writeError(w, r, err)
				return
			}
			if sid != principal.SessionID {
				writeError(w, r, errNotFound("session %s not found", sid))
				return
			}
		case isSkillReadPath(p):
			// Workspace-global, as for the environment key.
		case isMemoryStorePath(p):
			storeID, rest := splitMemoryStore(p)
			if !memoryRouteForWorker(r.Method, rest) {
				writeError(w, r, refused)
				return
			}
			if err := checkID(storeID, "memory store"); err != nil {
				writeError(w, r, err)
				return
			}
			// coalesce: a session deleted since Authenticate (its token row
			// cascades with it) is the unattached store's not-found, not a 500.
			var attached bool
			if err := pool.QueryRow(r.Context(),
				`SELECT coalesce((SELECT resources @> jsonb_build_array(jsonb_build_object('type', 'memory_store', 'memory_store_id', $2::text))
				                    FROM sessions WHERE id = $1), false)`, principal.SessionID, storeID).Scan(&attached); err != nil {
				writeError(w, r, err)
				return
			}
			if !attached {
				writeError(w, r, errNotFound("memory store %s not found", storeID))
				return
			}
		default:
			writeError(w, r, refused)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyEnvironment, principal.EnvironmentID)
		ctx = context.WithValue(ctx, ctxKeyWorkSession, principal.SessionID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// workSessionFrom returns the session a sessions token authorised, or ""
// outside the lane.
func workSessionFrom(ctx context.Context) string {
	s, _ := ctx.Value(ctxKeyWorkSession).(string)
	return s
}
