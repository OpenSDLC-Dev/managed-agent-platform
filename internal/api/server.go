package api

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gateconfig"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/telemetry"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

type server struct {
	pool   *pgxpool.Pool
	log    *events.Log
	broker *events.Broker
	queue  *queue.Queue
	blobs  blob.Store
	cipher secrets.Cipher
	// emitting marks sessions with an unreachable-credential advisory emission
	// in flight, so a gate fetching faster than the emission drains cannot
	// stack detached goroutines (startEmission).
	emitting sync.Map
}

// NewHandler assembles the control-plane HTTP surface over the given pool.
// blobs is the object store backing skill archives; nil deploys without
// object storage — everything serves except the storage-backed skill routes,
// which answer with a configuration error. cipher seals vault credential
// secrets; nil deploys without one — vault metadata CRUD serves, while the
// secret-bearing paths (credential create/update with secret fields, the
// validate probe) answer with a configuration error (fails closed, plan 12 D1).
// verifier authenticates humans; nil is IDENTITY_MODE=disabled, and the surface
// is then what it was before plan 31 — no lane, no role check — on every
// request shape but one: requireAPIKey refuses a repeated x-api-key field in
// every mode, deliberately (see dispatchManagementAuth).
func NewHandler(pool *pgxpool.Pool, blobs blob.Store, cipher secrets.Cipher, verifier *identity.Verifier) http.Handler {
	s := &server{pool: pool, log: events.NewLog(pool), broker: events.NewBroker(pool), queue: queue.New(pool), blobs: blobs, cipher: cipher}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/agents", s.handle(identity.RoleDeveloper, s.createAgent))
	mux.HandleFunc("GET /v1/agents", s.handle(identity.RoleViewer, s.listAgents))
	mux.HandleFunc("GET /v1/agents/{id}", s.handle(identity.RoleViewer, s.getAgent))
	mux.HandleFunc("POST /v1/agents/{id}", s.handle(identity.RoleDeveloper, s.updateAgent)) // update is POST on the wire, not PATCH
	mux.HandleFunc("GET /v1/agents/{id}/versions", s.handle(identity.RoleViewer, s.listAgentVersions))
	mux.HandleFunc("POST /v1/agents/{id}/archive", s.handle(identity.RoleDeveloper, s.archiveAgent))

	mux.HandleFunc("POST /v1/environments", s.handle(identity.RoleDeveloper, s.createEnvironment))
	mux.HandleFunc("GET /v1/environments", s.handle(identity.RoleViewer, s.listEnvironments))
	mux.HandleFunc("GET /v1/environments/{id}", s.handle(identity.RoleViewer, s.getEnvironment))
	mux.HandleFunc("POST /v1/environments/{id}", s.handle(identity.RoleDeveloper, s.updateEnvironment))
	mux.HandleFunc("DELETE /v1/environments/{id}", s.handle(identity.RoleDeveloper, s.deleteEnvironment))
	mux.HandleFunc("POST /v1/environments/{id}/archive", s.handle(identity.RoleDeveloper, s.archiveEnvironment))

	mux.HandleFunc("POST /v1/deployments", s.handle(identity.RoleDeveloper, s.createDeployment))
	mux.HandleFunc("GET /v1/deployments", s.handle(identity.RoleViewer, s.listDeployments))
	mux.HandleFunc("GET /v1/deployments/{id}", s.handle(identity.RoleViewer, s.getDeployment))
	mux.HandleFunc("POST /v1/deployments/{id}", s.handle(identity.RoleDeveloper, s.updateDeployment))
	mux.HandleFunc("POST /v1/deployments/{id}/archive", s.handle(identity.RoleDeveloper, s.archiveDeployment))
	mux.HandleFunc("POST /v1/deployments/{id}/pause", s.handle(identity.RoleDeveloper, s.pauseDeployment))
	mux.HandleFunc("POST /v1/deployments/{id}/unpause", s.handle(identity.RoleDeveloper, s.unpauseDeployment))
	// RoleDeveloper because it does session-create's work, not because it
	// looks like a read (plan 37 §5.1).
	mux.HandleFunc("POST /v1/deployments/{id}/run", s.handle(identity.RoleDeveloper, s.runDeployment))

	mux.HandleFunc("POST /v1/sessions", s.handle(identity.RoleDeveloper, s.createSession))
	mux.HandleFunc("GET /v1/sessions", s.handle(identity.RoleViewer, s.listSessions))
	mux.HandleFunc("GET /v1/sessions/{id}", s.handle(identity.RoleViewer, s.getSession))
	mux.HandleFunc("POST /v1/sessions/{id}", s.handle(identity.RoleDeveloper, s.updateSession))
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.handle(identity.RoleDeveloper, s.deleteSession))
	mux.HandleFunc("POST /v1/sessions/{id}/archive", s.handle(identity.RoleDeveloper, s.archiveSession))

	mux.HandleFunc("POST /v1/sessions/{id}/events", s.handle(identity.RoleDeveloper, s.sendSessionEvents))
	mux.HandleFunc("GET /v1/sessions/{id}/events", s.handle(identity.RoleViewer, s.listSessionEvents))
	mux.HandleFunc("GET /v1/sessions/{id}/events/stream", roleGate(identity.RoleViewer, s.streamSessionEvents))

	mux.HandleFunc("GET /v1/sessions/{id}/threads", s.handle(identity.RoleViewer, s.listThreads))
	mux.HandleFunc("GET /v1/sessions/{id}/threads/{tid}", s.handle(identity.RoleViewer, s.getThread))
	mux.HandleFunc("POST /v1/sessions/{id}/threads/{tid}/archive", s.handle(identity.RoleDeveloper, s.archiveThread))
	mux.HandleFunc("GET /v1/sessions/{id}/threads/{tid}/events", s.handle(identity.RoleViewer, s.listThreadEvents))
	mux.HandleFunc("GET /v1/sessions/{id}/threads/{tid}/stream", roleGate(identity.RoleViewer, s.streamThreadEvents))

	mux.HandleFunc("GET /v1/sessions/{id}/resources", s.handle(identity.RoleViewer, s.listSessionResources))
	mux.HandleFunc("POST /v1/sessions/{id}/resources", s.handle(identity.RoleDeveloper, s.addSessionResource))
	mux.HandleFunc("GET /v1/sessions/{id}/resources/{rid}", s.handle(identity.RoleViewer, s.getSessionResource))
	mux.HandleFunc("POST /v1/sessions/{id}/resources/{rid}", s.handle(identity.RoleDeveloper, s.updateSessionResource))
	mux.HandleFunc("DELETE /v1/sessions/{id}/resources/{rid}", s.handle(identity.RoleDeveloper, s.deleteSessionResource))

	mux.HandleFunc("POST /v1/vaults", s.handle(identity.RoleDeveloper, s.createVault))
	mux.HandleFunc("GET /v1/vaults", s.handle(identity.RoleViewer, s.listVaults))
	mux.HandleFunc("GET /v1/vaults/{id}", s.handle(identity.RoleViewer, s.getVault))
	mux.HandleFunc("POST /v1/vaults/{id}", s.handle(identity.RoleDeveloper, s.updateVault))
	mux.HandleFunc("DELETE /v1/vaults/{id}", s.handle(identity.RoleDeveloper, s.deleteVault))
	mux.HandleFunc("POST /v1/vaults/{id}/archive", s.handle(identity.RoleDeveloper, s.archiveVault))
	mux.HandleFunc("POST /v1/vaults/{id}/credentials", s.handle(identity.RoleAdmin, s.createVaultCredential))
	mux.HandleFunc("GET /v1/vaults/{id}/credentials", s.handle(identity.RoleViewer, s.listVaultCredentials))
	mux.HandleFunc("GET /v1/vaults/{id}/credentials/{cid}", s.handle(identity.RoleViewer, s.getVaultCredential))
	mux.HandleFunc("POST /v1/vaults/{id}/credentials/{cid}", s.handle(identity.RoleAdmin, s.updateVaultCredential))
	mux.HandleFunc("DELETE /v1/vaults/{id}/credentials/{cid}", s.handle(identity.RoleAdmin, s.deleteVaultCredential))
	mux.HandleFunc("POST /v1/vaults/{id}/credentials/{cid}/archive", s.handle(identity.RoleAdmin, s.archiveVaultCredential))
	mux.HandleFunc("POST /v1/vaults/{id}/credentials/{cid}/mcp_oauth_validate", s.handle(identity.RoleAdmin, s.validateVaultCredential))

	mux.HandleFunc("POST /v1/skills", s.handle(identity.RoleDeveloper, s.createSkill))
	mux.HandleFunc("GET /v1/skills", s.handle(identity.RoleViewer, s.listSkills))
	mux.HandleFunc("GET /v1/skills/{id}", s.handle(identity.RoleViewer, s.getSkill))
	mux.HandleFunc("DELETE /v1/skills/{id}", s.handle(identity.RoleDeveloper, s.deleteSkill))
	mux.HandleFunc("POST /v1/skills/{id}/versions", s.handle(identity.RoleDeveloper, s.createSkillVersion))
	mux.HandleFunc("GET /v1/skills/{id}/versions", s.handle(identity.RoleViewer, s.listSkillVersions))
	mux.HandleFunc("GET /v1/skills/{id}/versions/{version}", s.handle(identity.RoleViewer, s.getSkillVersion))
	mux.HandleFunc("DELETE /v1/skills/{id}/versions/{version}", s.handle(identity.RoleDeveloper, s.deleteSkillVersion))
	mux.HandleFunc("GET /v1/skills/{id}/versions/{version}/content", roleGate(identity.RoleViewer, s.downloadSkillVersion)) // streams the archive; not a typed handler

	mux.HandleFunc("POST /v1/files", s.handle(identity.RoleDeveloper, s.createFile))
	mux.HandleFunc("GET /v1/files", s.handle(identity.RoleViewer, s.listFiles))
	mux.HandleFunc("GET /v1/files/{id}", s.handle(identity.RoleViewer, s.getFile))
	mux.HandleFunc("DELETE /v1/files/{id}", s.handle(identity.RoleDeveloper, s.deleteFile))
	mux.HandleFunc("GET /v1/files/{id}/content", roleGate(identity.RoleViewer, s.downloadFile)) // streams the object; not a typed handler

	mux.HandleFunc("POST /v1/memory_stores", s.handle(identity.RoleDeveloper, s.createMemoryStore))
	mux.HandleFunc("GET /v1/memory_stores", s.handle(identity.RoleViewer, s.listMemoryStores))
	mux.HandleFunc("GET /v1/memory_stores/{id}", s.handle(identity.RoleViewer, s.getMemoryStore))
	mux.HandleFunc("POST /v1/memory_stores/{id}", s.handle(identity.RoleDeveloper, s.updateMemoryStore))
	mux.HandleFunc("DELETE /v1/memory_stores/{id}", s.handle(identity.RoleDeveloper, s.deleteMemoryStore))
	mux.HandleFunc("POST /v1/memory_stores/{id}/archive", s.handle(identity.RoleDeveloper, s.archiveMemoryStore))
	mux.HandleFunc("POST /v1/memory_stores/{id}/memories", s.handle(identity.RoleDeveloper, s.createMemory))
	mux.HandleFunc("GET /v1/memory_stores/{id}/memories", s.handle(identity.RoleViewer, s.listMemories))
	mux.HandleFunc("GET /v1/memory_stores/{id}/memories/{mid}", s.handle(identity.RoleViewer, s.getMemory))
	mux.HandleFunc("POST /v1/memory_stores/{id}/memories/{mid}", s.handle(identity.RoleDeveloper, s.updateMemory))
	mux.HandleFunc("DELETE /v1/memory_stores/{id}/memories/{mid}", s.handle(identity.RoleDeveloper, s.deleteMemory))
	mux.HandleFunc("GET /v1/memory_stores/{id}/memory_versions", s.handle(identity.RoleViewer, s.listMemoryVersions))
	mux.HandleFunc("GET /v1/memory_stores/{id}/memory_versions/{vid}", s.handle(identity.RoleViewer, s.getMemoryVersion))
	// Redaction is admin, alone in this family: it is a compliance action that
	// destroys history, and the role that holds the credential surfaces is the
	// one that holds this (plan 36 decision 14).
	mux.HandleFunc("POST /v1/memory_stores/{id}/memory_versions/{vid}/redact", s.handle(identity.RoleAdmin, s.redactMemoryVersion))

	// The console API — off the /v1 wire, mirroring the reference console's own
	// private path so a console-facing endpoint has a convention rather than an
	// invented namespace (internal/api/consoleapi.go). Management x-api-key, via
	// dispatchAuth's default lane: no other lane's predicate can match an /api/
	// path — they test a /v1/ prefix, or (the gate's) exact equality against
	// "/internal/v1/gate/config". consoleapi.go states the reasoning in full.
	// The role mechanism's first real use, and the only annotated routes in
	// slice 2: issuing a worker credential is the credential surface the
	// reference gates behind console roles, and the whole SECTION is gated —
	// list included — because knowing which hosts hold keys, and their names, is
	// itself the inventory an attacker would want. `admin` rather than
	// `developer` is a local judgment recorded as INFERRED in docs/DIVERGENCES.md:
	// the reference's Developer role can manage API keys, so a future recording
	// may justify relaxing this.
	mux.HandleFunc("POST "+consoleTokensPath, noStore(s.handle(identity.RoleAdmin, s.createEnvironmentKey)))
	mux.HandleFunc("GET "+consoleTokensPath, s.handle(identity.RoleAdmin, s.listEnvironmentKeys))
	mux.HandleFunc("POST "+consoleRevokePath, s.handleNoContent(identity.RoleAdmin, s.revokeEnvironmentKey))
	for _, pattern := range []string{consoleTokensPath, consoleRevokePath} {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			writeError(w, r, methodNotAllowed(r))
		})
	}

	// Management-key issuance (plan 32 slice 2, #378), on the reference's own
	// `/api/console/` prefix rather than the `/api/oauth/` one above — it uses
	// both, and each surface keeps the one it was recorded under. Admin for the
	// same reason the environment-key section is: a listing of which credentials
	// exist, their names and their issuers is itself the inventory an attacker
	// would want, so the section is gated whole rather than only its mutations.
	//
	// There is deliberately no /v1 twin. The reference withholds key creation from
	// its public API on purpose ("new API keys can only be created through the
	// Claude Console for security reasons"), and its read/update pair lives on the
	// Admin API, behind an `sk-ant-admin…` credential class this platform does not
	// have. Both registered in docs/DIVERGENCES.md.
	mux.HandleFunc("POST "+consoleAPIKeysPath, noStore(s.handle(identity.RoleAdmin, s.createAPIKey)))
	mux.HandleFunc("GET "+consoleAPIKeysPath, s.handle(identity.RoleAdmin, s.listAPIKeys))
	mux.HandleFunc("POST "+consoleAPIKeyPath, s.handle(identity.RoleAdmin, s.updateAPIKey))
	for _, pattern := range []string{consoleAPIKeysPath, consoleAPIKeyPath} {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			writeError(w, r, methodNotAllowed(r))
		})
	}

	// Internal gate-config endpoint — not on the public /v1 wire. A session's
	// egress gate authenticates with its per-session gtk_ token (its own auth
	// lane in dispatchAuth) and fetches its networking policy + resolved
	// credentials. The method-less pattern keeps the wire error envelope on a
	// non-GET.
	mux.HandleFunc("GET "+gateconfig.Path, s.handle(identity.RoleNone, s.getGateConfig))
	mux.HandleFunc(gateconfig.Path, func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, methodNotAllowed(r))
	})

	// The mux's built-in 404/405 write plain text; clients expect the wire
	// error envelope, so register explicit fallbacks: "/" for unknown paths
	// and a method-less pattern per route for unsupported methods.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, errNotFound("no such endpoint: %s", r.URL.Path))
	})
	for _, pattern := range []string{
		"/v1/agents", "/v1/agents/{id}", "/v1/agents/{id}/versions", "/v1/agents/{id}/archive",
		"/v1/environments", "/v1/environments/{id}", "/v1/environments/{id}/archive",
		"/v1/sessions", "/v1/sessions/{id}", "/v1/sessions/{id}/archive",
		"/v1/sessions/{id}/events", "/v1/sessions/{id}/events/stream",
		"/v1/sessions/{id}/threads", "/v1/sessions/{id}/threads/{tid}", "/v1/sessions/{id}/threads/{tid}/archive",
		"/v1/sessions/{id}/threads/{tid}/events", "/v1/sessions/{id}/threads/{tid}/stream",
		"/v1/sessions/{id}/resources", "/v1/sessions/{id}/resources/{rid}",
		"/v1/vaults", "/v1/vaults/{id}", "/v1/vaults/{id}/archive",
		"/v1/vaults/{id}/credentials", "/v1/vaults/{id}/credentials/{cid}",
		"/v1/vaults/{id}/credentials/{cid}/archive",
		"/v1/vaults/{id}/credentials/{cid}/mcp_oauth_validate",
		"/v1/skills", "/v1/skills/{id}", "/v1/skills/{id}/versions",
		"/v1/skills/{id}/versions/{version}", "/v1/skills/{id}/versions/{version}/content",
		"/v1/files", "/v1/files/{id}", "/v1/files/{id}/content",
		"/v1/memory_stores", "/v1/memory_stores/{id}", "/v1/memory_stores/{id}/archive",
		"/v1/memory_stores/{id}/memories", "/v1/memory_stores/{id}/memories/{mid}",
		"/v1/memory_stores/{id}/memory_versions", "/v1/memory_stores/{id}/memory_versions/{vid}",
		"/v1/memory_stores/{id}/memory_versions/{vid}/redact",
		"/v1/deployments", "/v1/deployments/{id}", "/v1/deployments/{id}/archive",
		"/v1/deployments/{id}/pause", "/v1/deployments/{id}/unpause",
		"/v1/deployments/{id}/run",
	} {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			writeError(w, r, methodNotAllowed(r))
		})
	}

	// The work API is a separate auth domain — BYOC workers authenticate with an
	// Authorization: Bearer environment key, not the management x-api-key — but
	// it shares this one mux with the management routes so that auth (dispatched
	// per path below) runs before any ServeMux path-cleaning or subtree-slash
	// redirect. Splitting the routes across nested muxes let those redirects
	// answer an unauthenticated request before auth ran.
	mux.HandleFunc("GET /v1/environments/{id}/work", s.handle(identity.RoleNone, s.listWork))
	mux.HandleFunc("GET /v1/environments/{id}/work/poll", noStore(roleGate(identity.RoleNone, s.pollWork))) // emits trace headers; not a typed handler
	mux.HandleFunc("GET /v1/environments/{id}/work/stats", s.handle(identity.RoleNone, s.statsWork))        // literal segment beats {work_id}
	mux.HandleFunc("GET /v1/environments/{id}/work/{work_id}", s.handle(identity.RoleNone, s.getWork))
	mux.HandleFunc("POST /v1/environments/{id}/work/{work_id}", s.handle(identity.RoleNone, s.updateWork)) // metadata patch
	mux.HandleFunc("POST /v1/environments/{id}/work/{work_id}/ack", s.handle(identity.RoleNone, s.ackWork))
	mux.HandleFunc("POST /v1/environments/{id}/work/{work_id}/heartbeat", s.handle(identity.RoleNone, s.heartbeatWork))
	mux.HandleFunc("POST /v1/environments/{id}/work/{work_id}/stop", s.handleNoContent(identity.RoleNone, s.stopWork))
	// Method-less 405 fallbacks. No explicit ".../work/poll" or ".../work/stats"
	// entry: it would be ambiguous against "GET .../work/{work_id}" (more specific
	// in path, less in method — neither dominates, so the mux panics). The
	// ".../work/{work_id}" fallback answers other non-GET methods on those literal
	// paths (PUT/DELETE) with a 405 (work_id="poll"/"stats"); a POST there routes
	// to the metadata update, which — given a valid patch body — 404s on the
	// nonexistent item, as the reference's own POST route does (an empty or
	// malformed body is a 400, since body validation precedes the item lookup).
	for _, pattern := range []string{
		"/v1/environments/{id}/work",
		"/v1/environments/{id}/work/{work_id}",
		"/v1/environments/{id}/work/{work_id}/ack",
		"/v1/environments/{id}/work/{work_id}/heartbeat",
		"/v1/environments/{id}/work/{work_id}/stop",
	} {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			writeError(w, r, methodNotAllowed(r))
		})
	}
	mux.HandleFunc("/v1/environments/{id}/work/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, errNotFound("no such endpoint: %s", r.URL.Path))
	})

	return withRequestID(withTracing(dispatchAuth(pool, verifier, mux)))
}

// dispatchAuth picks the auth scheme by path and runs it before the router, so
// no request reaches a handler — or a ServeMux redirect — unauthenticated. Work
// API paths take the Authorization: Bearer environment key; the session events
// subtree and the skill read routes are dual-auth (a worker's Bearer key or the
// management x-api-key); everything else takes the management x-api-key.
func dispatchAuth(pool *pgxpool.Pool, v *identity.Verifier, next http.Handler) http.Handler {
	work := requireEnvironmentKey(pool, next)
	workToken := requireWorkToken(pool, next)
	mgmt := dispatchManagementAuth(pool, v, next)
	gate := requireGateToken(pool, next)
	// Built once and shared by every lane that can reach a human: the verifier
	// is safe for concurrent use and the middleware holds no per-request state.
	// nil when identity is disabled, which is what makes every dual-auth branch
	// below collapse to exactly its previous behaviour.
	var human http.Handler
	if v != nil {
		human = requireIdentity(pool, v, next)
	}
	sessionEvents := dispatchSessionEventsAuth(pool, v, human, next)
	skillReads := dualAuth(v, requireEnvironmentKey(pool, next), human, mgmt)
	fileReads := dualAuth(v, requireEnvironmentKey(pool, next), human, mgmt)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Classify on the escaped path, splitting only on real '/' — the segment
		// structure ServeMux routes on (an encoded %2F stays within one segment).
		// This makes the worker lanes strictly no broader than the router: a
		// worker route is recognized only when its literal segments (events /
		// stream / work / poll) appear unencoded, exactly as every real client
		// sends them. The security-critical consequence is that a %2F can never
		// forge a worker segment the router does not also see — GET
		// /v1/sessions/{id}%2Fevents stays a bare /v1/sessions/{id} (CRUD →
		// management auth), so an environment key never reaches a management-only
		// handler. (Classifying on the decoded r.URL.Path instead would let that
		// %2F reach the CRUD handler under env-key auth.) The reverse case is
		// fail-closed and driverless, but by two different mechanisms depending
		// on whether identity is configured. A request that percent-encodes a
		// literal route segment (e.g. /%65vents, or /%77ork) is not recognized as
		// a worker route and falls to management auth, and ServeMux then decodes
		// the segment and matches the machine-lane registration anyway. With no
		// verifier that is a 401 — no key, no entry. With one, the request can
		// instead take the HUMAN lane and reach that registration, where what
		// refuses it is the role: those routes are registered identity.RoleNone,
		// which no role satisfies, so it is a 403. Hence RoleNone on the work and
		// gate registrations is load-bearing rather than decorative
		// (TestAnEncodedPathCannotSlipPastTheWorkLane). Under a management key the
		// handlers are reached with requireRole a no-op, and what closes them is
		// their own scoping — workScope's environment check and getGateConfig's
		// empty session id — not this dispatcher.
		p := r.URL.EscapedPath()
		switch {
		case isWorkTokenBearer(r) && isWorkTokenPath(r, p):
			// A worker's sessions token, on the families it can reach at all
			// (worktokenauth.go); first, because its wtk_ shape is what no
			// other credential carries, so nothing else is misrouted.
			workToken.ServeHTTP(w, r)
		case isWorkPath(p):
			work.ServeHTTP(w, r)
		case isGateConfigPath(p):
			gate.ServeHTTP(w, r)
		case isSessionEventsPath(p), r.Method == http.MethodGet && isBareSessionPath(p):
			sessionEvents.ServeHTTP(w, r)
		case r.Method == http.MethodGet && isSkillReadPath(p):
			skillReads.ServeHTTP(w, r)
		case r.Method == http.MethodGet && isFileReadPath(p):
			fileReads.ServeHTTP(w, r)
		case isConsolePath(p):
			// Explicit, though it resolves exactly as the default arm does.
			// consoleapi.go's header comment argued that /api/ needs no arm
			// because every other predicate is a /v1/ test or an exact match on
			// the gate path — and asked that "a future off-/v1 lane must
			// re-check this rather than assume the prefix rule covers
			// everything". This is that lane, and this arm is the re-check made
			// permanent: the console namespace's auth is now a stated fact at
			// the dispatcher rather than a property inherited from a fallthrough.
			mgmt.ServeHTTP(w, r)
		default:
			mgmt.ServeHTTP(w, r)
		}
	})
}

// dispatchManagementAuth is the management arm's credential dispatch: the
// machine key first, the human lane second, and today's 401 when neither is
// offered (#56, plan 31).
//
// Order is the security property. A non-empty x-api-key wins outright and keeps
// its frozen semantics — full authority, no role model — because a key IS its
// authority, which is the reference's own model and what bootstrap, CI and BYO
// automation depend on. Only when no key is offered does the human lane get to
// look, so an assertion header or a Bearer riding alongside a management key can
// never vouch for it, and a caller cannot downgrade a route's role requirement
// by attaching a second credential.
//
// With identity disabled this is requireAPIKey itself, unwrapped — no lane, no
// role check, no dispatch, which is the contract IDENTITY_MODE=disabled carries.
// That contract is byte-for-byte on every request shape but one: the duplicate
// x-api-key refusal inside requireAPIKey is unconditional, so a repeated field
// that header order used to resolve is now a 401 even with identity off. It is
// deliberately not gated on v — one rule in both places is what keeps lane
// selection and authentication from disagreeing about which value is the key,
// and gating it would make the same malformed request answer differently in two
// deployments. No client sends one; the change only ever denies.
func dispatchManagementAuth(pool *pgxpool.Pool, v *identity.Verifier, next http.Handler) http.Handler {
	mgmt := requireAPIKey(pool, next)
	if v == nil {
		return mgmt
	}
	human := requireIdentity(pool, v, next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if apiKeyOffered(r) {
			mgmt.ServeHTTP(w, r)
			return
		}
		if _, ok := identityCredential(r, v); ok {
			human.ServeHTTP(w, r)
			return
		}
		// Neither credential: requireAPIKey produces the same "missing
		// x-api-key" 401 it always has. Deliberately not a new message — an
		// unauthenticated caller learns nothing about whether SSO is enabled.
		mgmt.ServeHTTP(w, r)
	})
}

// dualAuth picks between a worker's environment-key lane and management auth
// by the rule dispatchSessionEventsAuth documents: the env lane only when a
// Bearer is present AND no non-empty x-api-key is; otherwise management.
//
// With identity enabled the Bearer branch splits once more, because two
// different credentials now arrive in the same header. A JWT silhouette (two
// dots) goes to the human lane; anything else stays the worker's environment
// key. The discrimination is one-way safe: an environment key this platform
// minted is `sk-map-env01-` + base64url, which has no dots at all, so a real key
// can never be read as a JWT. The reverse — a JWT misread as an environment key
// — cannot happen either, since the check is on the JWT shape.
//
// The residual case is a GRANDFATHERED pre-0021 key, whose value the operator
// chose and which could in principle contain two dots. It would misroute to the
// human lane and fail verification there: a 401, fail-closed, never an
// over-authorization. docs/self-hosted-security.md §6 already tells operators to
// reissue those keys, and the list shows them by their empty name and absent
// expiry.
//
// In trusted_proxy mode Bearer is never a human credential (identityCredential
// reads only the assertion header), so this branch stays exactly what it was and
// the assertion is consulted afterwards, inside the management arm — machine
// lanes first, always.
func dualAuth(v *identity.Verifier, env, human, mgmt http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token, ok := bearerToken(r); ok && !apiKeyOffered(r) {
			if human != nil && v.Mode() == identity.ModeOIDC && identity.LooksLikeJWT(token) {
				human.ServeHTTP(w, r)
				return
			}
			env.ServeHTTP(w, r)
			return
		}
		mgmt.ServeHTTP(w, r)
	})
}

// dispatchSessionEventsAuth dual-auths a session's worker-facing routes (the
// events subtree and the GET /v1/sessions/{id} read — see dispatchAuth). A BYOC
// worker drives its session with the same Authorization: Bearer environment key
// it polls work with; an application uses the management x-api-key. The lane is
// the environment key only when a Bearer is present AND no non-empty x-api-key is
// — the reference client deletes x-api-key before attaching the environment
// Bearer (the server rejects both at once), so a real x-api-key present
// unambiguously means a management caller. Keying on Bearer presence alone would
// let a stray Bearer header (a proxy, a client configured with both) knock a
// valid x-api-key caller off management auth. An empty x-api-key value is treated
// as absent (it is not a usable credential); this only ever keeps a Bearer caller
// on the environment lane, which still validates the key and scopes it to its own
// environment. Mutating session CRUD (create/update/delete/archive/list) is not
// routed here, so the environment key never reaches it.
func dispatchSessionEventsAuth(pool *pgxpool.Pool, v *identity.Verifier, human, next http.Handler) http.Handler {
	return dualAuth(v, requireEnvironmentKeyForSession(pool, next), human,
		dispatchManagementAuth(pool, v, next))
}

// apiKeyOffered reports whether the request carries a usable management key —
// the question every lane decision turns on, asked in one place so the two
// dispatchers cannot answer it differently.
//
// It reads EVERY x-api-key field, not Header.Get's first. HTTP lets a field
// repeat, Go's server preserves each value in order, and Get returns only value
// zero — so `x-api-key:` (empty) ahead of a real one would read as "no key
// offered" and move a machine caller onto a lane the machine-first rule exists
// to keep them off.
//
// A REPEATED field counts as offered whatever its values, so it lands on the key
// lane, where requireAPIKey refuses it outright. That pairing is the point: no
// real client sends a credential header twice, and answering "which value wins"
// in two places with two different rules is what produced the bug this helper
// fixes. One rule — a duplicate is an ambiguous credential, and an ambiguous
// credential is a 401.
//
// A single empty value still counts as absent, which is the documented
// pre-existing rule dispatchSessionEventsAuth relies on: it is not a usable
// credential, and treating it as one would knock a Bearer worker off its lane.
//
// The Authorization header has the same duplication exposure and needs no such
// helper: bearerToken reads Get, so a leading empty field makes the request look
// like it carries no Bearer at all — which denies, never over-authorizes.
func apiKeyOffered(r *http.Request) bool {
	values := r.Header.Values("x-api-key")
	if len(values) > 1 {
		return true
	}
	return len(values) == 1 && values[0] != ""
}

// isConsolePath reports whether p is under the off-wire /api/ console namespace.
//
// A prefix test, not an exact match on the two console patterns: the namespace
// is the unit that dispatches, so a route added to consoleapi.go later joins the
// same lane by existing rather than by someone remembering to widen this. The
// 405 and 404 fallbacks registered on those patterns are covered too, which is
// what keeps an unauthenticated caller from learning a path exists.
func isConsolePath(p string) bool {
	return p == "/api" || strings.HasPrefix(p, "/api/")
}

// isWorkPath reports whether p is under a work API route:
// /v1/environments/{id}/work or /v1/environments/{id}/work/... . dispatchAuth
// feeds it the escaped path (URL.EscapedPath, the representation ServeMux routes
// on) so the auth choice never depends on the router or on %2F decoding.
// /v1/environments/{id} and .../{id}/archive are management paths.
func isWorkPath(p string) bool {
	const prefix = "/v1/environments/"
	if !strings.HasPrefix(p, prefix) {
		return false
	}
	rest := p[len(prefix):] // "{id}/work..."
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return false // no segment after the environment id
	}
	rest = rest[slash+1:] // "work..."
	return rest == "work" || strings.HasPrefix(rest, "work/")
}

// splitSession parses /v1/sessions/{id}[/{sub...}]. ok is true when p is under
// /v1/sessions/ with a non-empty {id}; id is the first segment and sub is the
// remainder after it ("" for the bare /v1/sessions/{id}). The collection route
// /v1/sessions is ok=false. One splitter feeds both the auth-lane predicates
// (on the escaped path) and the middleware's ownership id (on the decoded path),
// so the routed handler and the environment it checks can never drift apart.
func splitSession(p string) (id, sub string, ok bool) {
	const prefix = "/v1/sessions/"
	if !strings.HasPrefix(p, prefix) {
		return "", "", false
	}
	rest := p[len(prefix):] // "{id}" or "{id}/sub..."
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		id, sub = rest[:slash], rest[slash+1:]
	} else {
		id = rest
	}
	if id == "" {
		return "", "", false
	}
	return id, sub, true
}

// isSessionEventsPath reports whether p is a session events route:
// /v1/sessions/{id}/events or /v1/sessions/{id}/events/stream. dispatchAuth feeds
// it the escaped path (URL.EscapedPath, the representation ServeMux routes on) so
// the auth choice never depends on the router or on %2F decoding.
func isSessionEventsPath(p string) bool {
	_, sub, ok := splitSession(p)
	return ok && (sub == "events" || sub == "events/stream")
}

// isSkillReadPath reports whether p is a skill read route: /v1/skills/{id},
// its versions list, a version get, or the /content download. A GET on these
// is what the reference worker's SetupSkills performs with its environment
// key (resolve "latest" over the versions list → version get → download), so
// they join the dual-auth set; skills are workspace-global resources every
// environment's sandboxes consume, so a valid key from any environment may
// read them — there is no per-environment scoping to enforce. The collection
// list /v1/skills and every mutation stay management-only. Like the other
// predicates this sees the escaped path, so a %2F can never smuggle a skills
// segment past the router's view.
func isSkillReadPath(p string) bool {
	const prefix = "/v1/skills/"
	if !strings.HasPrefix(p, prefix) {
		return false
	}
	segs := strings.Split(p[len(prefix):], "/")
	for _, s := range segs {
		if s == "" {
			return false
		}
	}
	switch len(segs) {
	case 1: // {id}
		return true
	case 2: // {id}/versions
		return segs[1] == "versions"
	case 3: // {id}/versions/{version}
		return segs[1] == "versions"
	case 4: // {id}/versions/{version}/content
		return segs[1] == "versions" && segs[3] == "content"
	}
	return false
}

// isFileReadPath reports whether p is the file-content read route
// /v1/files/{id}/content — and nothing else. This is the single route a BYOC
// worker's SetupFiles hits with its environment key to pull a mounted file's
// bytes, so it alone joins the dual-auth set — deliberately narrower than the
// skills read set: the metadata GET /v1/files/{id}, the list, and every mutation
// stay management-only. Unlike skills (workspace-global), file content can be
// sensitive, so admission to the env lane is not sufficient — downloadFile
// additionally scopes an environment key to files a session in that environment
// mounts. Sees the escaped path, so a %2F can never smuggle a files segment past
// the router's view.
func isFileReadPath(p string) bool {
	const prefix = "/v1/files/"
	if !strings.HasPrefix(p, prefix) {
		return false
	}
	segs := strings.Split(p[len(prefix):], "/")
	return len(segs) == 2 && segs[0] != "" && segs[1] == "content"
}

// isBareSessionPath reports whether p is exactly /v1/sessions/{id} — a single
// non-empty id segment with no subpath. A GET on it is the session read the
// reference `ant beta:worker` performs with its environment key (SetupSkills →
// Beta.Sessions.Get), so it joins the events subtree in the env-key dual-auth
// set; the collection route /v1/sessions and the subpaths (.../events,
// .../archive) are not bare.
func isBareSessionPath(p string) bool {
	_, sub, ok := splitSession(p)
	return ok && sub == ""
}

// methodNotAllowed is the wire 405 for a known path reached with an
// unsupported method.
func methodNotAllowed(r *http.Request) *apiError {
	return &apiError{http.StatusMethodNotAllowed, errTypeInvalidRequest,
		"method " + r.Method + " is not allowed on " + r.URL.Path}
}

// roleGate is the adapters' min parameter for the handful of routes that cannot
// use an adapter — the streaming and header-writing handlers, which own their
// own response. Same check, same place (beside the path), so the completeness
// test slice 3 adds can read every route's requirement from one file.
func roleGate(min identity.Role, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := requireRole(r.Context(), min); err != nil {
			writeError(w, r, err)
			return
		}
		h(w, r)
	}
}

// handle adapts a typed handler to http.HandlerFunc: JSON out, error envelope
// on failure. The reference returns 200 for every successful call it answers
// with a body, including creates; the bodiless exception is Stop, which uses
// handleNoContent.
//
// min is the route's authorization requirement, and it sits here — beside the
// path, at the single place every route is declared — for the reason
// requireEnvironmentKeyForSession already established: authorization is checked
// where the route is defined, not somewhere a reader has to go find. It applies
// to the identity lane alone; requireRole is a no-op on every machine lane.
//
// identity.RoleNone DENIES on the identity lane (see requireRole), which is what
// makes the floor safe: slice 2 registered every route there and slice 3 relaxed
// them one at a time, so a route nobody annotates stays shut rather than open.
// The routes still carrying it are the machine lanes — the work API and the gate
// config. Normally spelled, they dispatch away from identity before min is read.
// Percent-encoded, they do not: dispatchAuth classifies the escaped path and
// ServeMux matches the decoded one, so those spellings reach the identity lane
// and RoleNone is what refuses them. It is a live denial there, not a placeholder.
// TestEveryIdentityReachableRouteDeclaresARole keeps the two uses of the constant
// apart: it fails on an identity-reachable route left at the floor, and on a
// machine-lane route that declares a role or declares none at all.
func (s *server) handle(min identity.Role, fn func(*http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := requireRole(r.Context(), min); err != nil {
			writeError(w, r, err)
			return
		}
		v, err := fn(r)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
}

// handleNoContent adapts a typed handler whose success carries no body: the
// same error envelope as handle, but a bodiless 204 instead of 200 + JSON.
func (s *server) handleNoContent(min identity.Role, fn func(*http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := requireRole(r.Context(), min); err != nil {
			writeError(w, r, err)
			return
		}
		if err := fn(r); err != nil {
			writeError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// withRequestID stamps every response (success and error) with a request-id
// header and threads the ID into the context for error envelopes.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := domain.NewID("req").String()
		w.Header().Set("request-id", rid)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID, rid)))
	})
}

// withTracing continues the caller's W3C trace context and opens one server
// span per request (CLAUDE.md: every cross-process call propagates OTel
// context). With no tracer provider installed this is a no-op passthrough.
func withTracing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		carrier := make(map[string]string, len(r.Header))
		for k, v := range r.Header {
			if len(v) > 0 {
				carrier[k] = v[0]
			}
		}
		ctx := telemetry.Extract(r.Context(), carrier)
		ctx, span := otel.GetTracerProvider().
			Tracer("github.com/OpenSDLC-Dev/managed-agent-platform/internal/api").
			Start(ctx, r.Method+" "+r.URL.Path, trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
