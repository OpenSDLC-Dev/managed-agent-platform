package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
)

// authenticateEnvironmentKey resolves a Bearer token to the environment it is
// scoped to and that environment's kind, or "" if the key is unknown, revoked,
// or expired. revoked reports the one of those three the lanes may answer in
// its own words, which lookupEnvironmentKey decides. Expired and unknown take
// the same branch on purpose: the caller turns them into one 401 with one
// message, so a probing client learns nothing about which of them it hit. A
// key that is both revoked and expired is revoked: a revocation is permanent
// and the operator's own act, so a revoked key answers one sentence for the
// rest of its life rather than changing its answer on the day its expiry
// passes. A key minted before keys carried expiries has a NULL expires_at and
// never expires.
//
// The kind rides the same lookup because every lane gates on it, and a second
// query on every poll would be its price. A key dies with its environment (ON
// DELETE CASCADE) and a kind never changes (updateEnvironment), so the join
// drops no live key and the kind cannot go stale within a request.
func authenticateEnvironmentKey(ctx context.Context, pool *pgxpool.Pool, key string) (envID string, kind domain.EnvironmentKind, revoked bool, err error) {
	var k string
	var expired bool
	err = pool.QueryRow(ctx,
		`SELECT k.environment_id, e.kind, k.revoked_at IS NOT NULL,
		        k.expires_at IS NOT NULL AND k.expires_at <= now()
		   FROM environment_keys k
		   JOIN environments e ON e.id = k.environment_id
		  WHERE k.key_hash = $1`,
		hashKey(key)).Scan(&envID, &k, &revoked, &expired)
	switch {
	case err == pgx.ErrNoRows:
		return "", "", false, nil
	case err != nil:
		return "", "", false, err
	case revoked:
		return "", "", true, nil
	case expired:
		return "", "", false, nil
	}
	return envID, domain.EnvironmentKind(k), false, nil
}

// errRevokedEnvironmentKey is the 401 a revoked environment key gets, in the
// reference's words: recorded on the work poll (2026-09-19 custom-mixed-tools,
// custom-order-followup and self-hosted-docker `worker-network.json` idx 169,
// 51 and 63), on a skill version's content (2026-09-05 batch1 idx 84
// `rec82.envkey.dead-key-verify.t4-builtin`, batch2 idx 64
// `rec83.control.revoked-key-A.content`) and on GET /v1/models, a route
// outside every environment-key lane (2026-09-19 custom-mixed-tools,
// custom-order-followup and self-hosted-latest-cli `worker-network.json` idx
// 170, 52 and 61). lookupEnvironmentKey writes it, for every lane that reads
// an environment key, the management lane included (answerEnvironmentKey) —
// every public route; the internal gate-config endpoint takes a gate token and
// nothing else. The reference's bodies carry a null request_id and no
// request-id header; this one carries the request id every response does.
var errRevokedEnvironmentKey = errAuth("OAuth access token has been revoked.")

// bearerToken extracts a non-empty Authorization: Bearer token. ok reports
// whether the header used the Bearer scheme at all — the dual-auth dispatcher
// keys the scheme off this, so a request with no Bearer header falls through to
// management auth rather than being rejected.
func bearerToken(r *http.Request) (token string, ok bool) {
	return strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// lookupEnvironmentKey resolves token as an environment key for every lane
// that reads one, and answers a revoked key itself, so the revoked 401
// (errRevokedEnvironmentKey) is written here and nowhere else; answered
// reports that it was. Only a key carrying the prefix this platform mints
// (environmentKeySecretPrefix) is answered so: 256 random bits that only their
// holder can present, so the sentence tells nobody else anything. The prefix
// and migration 0021 landed together (#360), so every key carrying it was
// minted here. A grandfathered pre-0021 key is a value its operator chose and
// may be guessable, and a revoked one gets the lane's dead-key answer instead,
// so a dictionary of guesses cannot learn which value once worked. Otherwise
// it returns the live key's environment and kind, or "" for a key that is
// expired, unknown, or revoked and grandfathered; err, a failed lookup, is the
// lane's to answer.
func lookupEnvironmentKey(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool, token string) (envID string, kind domain.EnvironmentKind, answered bool, err error) {
	envID, kind, revoked, err := authenticateEnvironmentKey(r.Context(), pool, token)
	if err != nil || !revoked {
		return envID, kind, false, err
	}
	if !strings.HasPrefix(token, environmentKeySecretPrefix) {
		return "", "", false, nil
	}
	writeError(w, r, errRevokedEnvironmentKey)
	return "", "", true, nil
}

// resolveEnvironmentKey authenticates a request's Authorization: Bearer
// environment key, returning the environment it is scoped to and its kind. On a
// missing/empty header or a key that is not live it writes the wire auth error
// — a revoked key's own (lookupEnvironmentKey), or the one every other dead
// key shares — and returns ok=false. Every environment-key middleware shares
// it so the Bearer-resolution rules live in one place. Unlike the management
// lane it looks up any Bearer, prefixed or not: a grandfathered key is still a
// worker's credential here, and a failed lookup is the 500 an outage is on
// every authenticating lane.
func resolveEnvironmentKey(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool) (envID string, kind domain.EnvironmentKind, ok bool) {
	token, hasBearer := bearerToken(r)
	if !hasBearer || token == "" {
		writeError(w, r, errAuth("missing Authorization: Bearer environment key"))
		return "", "", false
	}
	envID, kind, answered, err := lookupEnvironmentKey(w, r, pool, token)
	switch {
	case answered:
		return "", "", false
	case err != nil:
		writeError(w, r, err)
		return "", "", false
	case envID == "":
		writeError(w, r, errAuth("invalid environment key"))
		return "", "", false
	}
	markVerified(r.Context())
	return envID, kind, true
}

// answerEnvironmentKey is the management lane's answer to an environment key
// presented there as a Bearer, with neither a management key nor a human
// credential beside it (dispatchManagementAuth), and reports whether it wrote
// one. Two things differ from the key lanes, deliberately:
//
//   - The lane looks a key up only when the Bearer carries the prefix every key
//     this platform mints does (environmentKeySecretPrefix), so a management
//     key's request — served before this is asked — and any other Bearer cost
//     no query. A grandfathered pre-0021 key, whose value the operator chose, is
//     not recognised here and keeps the missing-key 401.
//   - A failed lookup is not this request's 500. Nothing authenticated it, so
//     the lane fails closed with the missing-key 401 it would give anyway, and
//     the operator's log gets the failure at Warn.
//
// A revoked key gets its own 401 on every route the lane serves
// (lookupEnvironmentKey). A live key gets the reference's recorded refusal on
// the routes it was recorded on (environmentKeyRefusals). Everywhere else, and
// for an expired or unknown key, the lane answers as it always has, so an
// unauthenticated caller learns nothing about SSO or about which dead state a
// key is in.
func answerEnvironmentKey(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool, router *http.ServeMux) bool {
	token, ok := bearerToken(r)
	if !ok || !strings.HasPrefix(token, environmentKeySecretPrefix) {
		return false
	}
	envID, _, answered, err := lookupEnvironmentKey(w, r, pool, token)
	switch {
	case answered:
		return true
	case err != nil:
		slog.WarnContext(r.Context(), "management lane: environment key lookup failed; answering as for no key",
			"request_id", requestIDFrom(r.Context()), "err", err)
		return false
	case envID == "":
		return false
	}
	_, pattern := router.Handler(r)
	refusal := environmentKeyRefusals[pattern]
	if refusal == nil {
		return false
	}
	markVerified(r.Context())
	slog.InfoContext(r.Context(), "management route refused: environment key",
		"request_id", requestIDFrom(r.Context()), "environment_id", envID, "route", pattern)
	writeError(w, r, refusal)
	return true
}

// skillsScopeRefusal is the reference's 403 for a live environment key on the
// skill collection and a skill's own read, the routes whose OAuth scope its key
// does not carry (2026-09-03 batch2 idx 4 `envkey.skills.collection-list` and
// idx 2 `envkey.skills.get-anthropic`, 2026-09-04 batch2 idx 146
// `rec81.envkey.skills.get.custom`, 2026-09-05 batch1 idx 51
// `rec82.skills.list.envkey`). The scopes are the reference's; this platform
// has no scope vocabulary of its own (#550).
const skillsScopeRefusal = "OAuth token does not meet scope requirement any_of(org:skills, user:developer, user:managed_agents, user:skills, workspace:developer, workspace:skills)"

// environmentKeyRefusals is the reference's answer to a live environment key
// on a management route, for each route it was recorded on, keyed by the
// pattern server.go registers that route under. answerEnvironmentKey asks the
// router which pattern a request reaches (ServeMux.Handler) rather than
// matching paths of its own, so the table reads the request exactly as the
// router does: a HEAD, which the router serves through a GET registration,
// and a percent-encoded spelling it decodes (/v1/%61gents) take the route's
// refusal as they would take its handler. The refusal cannot sit on the
// registration itself, since every lane answers before the router runs
// (dispatchAuth), and a handler would run only after the lane had answered.
//
// An unrecorded route keeps this platform's answer: the reference's refusals
// differ route by route — a 401 on the agents list, a 403 naming the route's
// scopes on the skills — so nothing says which a third route takes.
var environmentKeyRefusals = map[string]*apiError{
	// 2026-09-03 batch2 idx 5 `envkey.agents.list-should-refuse`.
	"GET /v1/agents":      errAuth("Authentication failed"),
	"GET /v1/skills":      errForbidden(skillsScopeRefusal),
	"GET /v1/skills/{id}": errForbidden(skillsScopeRefusal),
}

// withEnvironmentKey records what an environment key resolved to: its
// environment, and that environment's kind beside it.
func withEnvironmentKey(ctx context.Context, envID string, kind domain.EnvironmentKind) context.Context {
	return context.WithValue(context.WithValue(ctx, ctxKeyEnvironment, envID), ctxKeyEnvironmentKind, kind)
}

// errNotSelfHostedKey is the one refusal a key on an environment that is not
// self_hosted gets on the session lane. Since #820 the console issues a key on
// a cloud environment, as the reference does, and that environment's work is
// the platform executor's: the key has no worker to serve, so it must not read
// the environment's sessions, post to them — a tool confirmation the executor
// would act on included — stream their events, or download the files they
// mount. A 404, as the work listing answers such a key, with a message of ours;
// it is answered before any session is looked up, so it says nothing about
// one. The file content download refuses the same key with the same status in
// the reference's recorded words instead (requireSelfHostedEnvironmentKey).
// The skill reads are not refused: they are workspace-global, and the
// reference was recorded serving them to a cloud environment's key.
func errNotSelfHostedKey(envID string) error {
	return errNotFound("environment %s is not a self_hosted environment; only a self_hosted environment's key reaches its sessions and the files they mount", envID)
}

// requireEnvironmentKey is the worker-auth middleware guarding the work API:
// every /work route needs a valid Authorization: Bearer environment key. The
// resolved environment is stored in the request context; handlers assert it
// matches the path's {id}, so a key scoped to one environment cannot drive
// another's queue. A key of any kind is admitted, because the work API answers
// a cloud environment's key route by route, as the reference was recorded
// answering it (pollWork, listWork, statsWork read the kind from the context).
// It is the skill reads' environment-key lane too, where a cloud key is
// served, as recorded (2026-09-05 batch2 idx 57, 59, 62).
func requireEnvironmentKey(pool *pgxpool.Pool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		envID, kind, ok := resolveEnvironmentKey(w, r, pool)
		if !ok {
			return
		}
		next.ServeHTTP(w, r.WithContext(withEnvironmentKey(r.Context(), envID, kind)))
	})
}

// requireSelfHostedEnvironmentKey is the environment-key lane of the file
// content download: requireEnvironmentKey, refusing a key whose environment is
// not self_hosted before any file is looked up. The refusal is the reference's
// bare "Not found" (2026-09-03 batch2 `envkey.files.content-original`,
// `envkey.files.content-session-copy`; #540), the words every other 404 on
// this lane takes (downloadFile), so it says nothing about the file either.
// Which reason fired is the operator's, in the log.
func requireSelfHostedEnvironmentKey(pool *pgxpool.Pool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		envID, kind, ok := resolveEnvironmentKey(w, r, pool)
		if !ok {
			return
		}
		if kind != domain.EnvSelfHosted {
			slog.InfoContext(r.Context(), "file download refused: environment key is not self_hosted",
				"request_id", requestIDFrom(r.Context()), "environment_id", envID, "environment_kind", kind)
			writeError(w, r, errNotFound("Not found"))
			return
		}
		next.ServeHTTP(w, r.WithContext(withEnvironmentKey(r.Context(), envID, kind)))
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
// dual-auth. A key whose environment is not self_hosted is refused before the
// session is looked up (errNotSelfHostedKey), so it neither reaches nor probes
// any session.
func requireEnvironmentKeyForSession(pool *pgxpool.Pool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		envID, kind, ok := resolveEnvironmentKey(w, r, pool)
		if !ok {
			return
		}
		if kind != domain.EnvSelfHosted {
			writeError(w, r, errNotSelfHostedKey(envID))
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
		ctx := withEnvironmentKey(r.Context(), envID, kind)
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, ctxKeyCredential, events.EnvironmentCredential)))
	})
}

// environmentFrom returns the environment a worker's Bearer key authorised, or
// "" outside a worker-authenticated request.
func environmentFrom(ctx context.Context) string {
	e, _ := ctx.Value(ctxKeyEnvironment).(string)
	return e
}

// selfHostedKeyFrom reports whether the request's environment key resolved to
// a self_hosted environment. False outside the environment-key lanes — the
// sessions-token lane included, whose routes on the work API (an item's
// heartbeat and stop) never ask — so a handler gating on it fails closed.
func selfHostedKeyFrom(ctx context.Context) bool {
	k, _ := ctx.Value(ctxKeyEnvironmentKind).(domain.EnvironmentKind)
	return k == domain.EnvSelfHosted
}

// credentialFrom returns the class of credential that signed the request:
// EnvironmentCredential where one of the two lanes that admit a worker to its
// session's events marked it (requireEnvironmentKeyForSession,
// requireWorkToken), and the zero value, ManagementCredential, everywhere
// else. It is its own value rather than read off environmentFrom, so a lane
// that stores an environment for some other reason fails closed.
func credentialFrom(ctx context.Context) events.Credential {
	c, _ := ctx.Value(ctxKeyCredential).(events.Credential)
	return c
}
