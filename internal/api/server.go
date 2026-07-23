// Package api implements the wire-compatible control-plane REST surface:
// Anthropic Managed Agents resource CRUD (agents / environments / sessions)
// with the reference paths, JSON shapes, ID prefixes, pagination envelope,
// error envelope, and x-api-key management auth. The `?beta=true` query and
// anthropic-version / anthropic-beta headers are accepted and ignored.
package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
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
}

// NewHandler assembles the control-plane HTTP surface over the given pool.
// blobs is the object store backing skill archives; nil deploys without
// object storage — everything serves except the storage-backed skill routes,
// which answer with a configuration error.
func NewHandler(pool *pgxpool.Pool, blobs blob.Store) http.Handler {
	s := &server{pool: pool, log: events.NewLog(pool), broker: events.NewBroker(pool), queue: queue.New(pool), blobs: blobs}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/agents", s.handle(s.createAgent))
	mux.HandleFunc("GET /v1/agents", s.handle(s.listAgents))
	mux.HandleFunc("GET /v1/agents/{id}", s.handle(s.getAgent))
	mux.HandleFunc("POST /v1/agents/{id}", s.handle(s.updateAgent)) // update is POST on the wire, not PATCH
	mux.HandleFunc("GET /v1/agents/{id}/versions", s.handle(s.listAgentVersions))
	mux.HandleFunc("POST /v1/agents/{id}/archive", s.handle(s.archiveAgent))

	mux.HandleFunc("POST /v1/environments", s.handle(s.createEnvironment))
	mux.HandleFunc("GET /v1/environments", s.handle(s.listEnvironments))
	mux.HandleFunc("GET /v1/environments/{id}", s.handle(s.getEnvironment))
	mux.HandleFunc("POST /v1/environments/{id}", s.handle(s.updateEnvironment))
	mux.HandleFunc("DELETE /v1/environments/{id}", s.handle(s.deleteEnvironment))
	mux.HandleFunc("POST /v1/environments/{id}/archive", s.handle(s.archiveEnvironment))

	mux.HandleFunc("POST /v1/sessions", s.handle(s.createSession))
	mux.HandleFunc("GET /v1/sessions", s.handle(s.listSessions))
	mux.HandleFunc("GET /v1/sessions/{id}", s.handle(s.getSession))
	mux.HandleFunc("POST /v1/sessions/{id}", s.handle(s.updateSession))
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.handle(s.deleteSession))
	mux.HandleFunc("POST /v1/sessions/{id}/archive", s.handle(s.archiveSession))

	mux.HandleFunc("POST /v1/sessions/{id}/events", s.handle(s.sendSessionEvents))
	mux.HandleFunc("GET /v1/sessions/{id}/events", s.handle(s.listSessionEvents))
	mux.HandleFunc("GET /v1/sessions/{id}/events/stream", s.streamSessionEvents)

	mux.HandleFunc("GET /v1/sessions/{id}/resources", s.handle(s.listSessionResources))
	mux.HandleFunc("POST /v1/sessions/{id}/resources", s.handle(s.addSessionResource))
	mux.HandleFunc("GET /v1/sessions/{id}/resources/{rid}", s.handle(s.getSessionResource))
	mux.HandleFunc("POST /v1/sessions/{id}/resources/{rid}", s.handle(s.updateSessionResource))
	mux.HandleFunc("DELETE /v1/sessions/{id}/resources/{rid}", s.handle(s.deleteSessionResource))

	mux.HandleFunc("POST /v1/skills", s.handle(s.createSkill))
	mux.HandleFunc("GET /v1/skills", s.handle(s.listSkills))
	mux.HandleFunc("GET /v1/skills/{id}", s.handle(s.getSkill))
	mux.HandleFunc("DELETE /v1/skills/{id}", s.handle(s.deleteSkill))
	mux.HandleFunc("POST /v1/skills/{id}/versions", s.handle(s.createSkillVersion))
	mux.HandleFunc("GET /v1/skills/{id}/versions", s.handle(s.listSkillVersions))
	mux.HandleFunc("GET /v1/skills/{id}/versions/{version}", s.handle(s.getSkillVersion))
	mux.HandleFunc("DELETE /v1/skills/{id}/versions/{version}", s.handle(s.deleteSkillVersion))
	mux.HandleFunc("GET /v1/skills/{id}/versions/{version}/content", s.downloadSkillVersion) // streams the archive; not a typed handler

	mux.HandleFunc("POST /v1/files", s.handle(s.createFile))
	mux.HandleFunc("GET /v1/files", s.handle(s.listFiles))
	mux.HandleFunc("GET /v1/files/{id}", s.handle(s.getFile))
	mux.HandleFunc("DELETE /v1/files/{id}", s.handle(s.deleteFile))
	mux.HandleFunc("GET /v1/files/{id}/content", s.downloadFile) // streams the object; not a typed handler

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
		"/v1/sessions/{id}/resources", "/v1/sessions/{id}/resources/{rid}",
		"/v1/skills", "/v1/skills/{id}", "/v1/skills/{id}/versions",
		"/v1/skills/{id}/versions/{version}", "/v1/skills/{id}/versions/{version}/content",
		"/v1/files", "/v1/files/{id}", "/v1/files/{id}/content",
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
	mux.HandleFunc("GET /v1/environments/{id}/work", s.handle(s.listWork))
	mux.HandleFunc("GET /v1/environments/{id}/work/poll", s.pollWork)             // emits trace headers; not a typed handler
	mux.HandleFunc("GET /v1/environments/{id}/work/stats", s.handle(s.statsWork)) // literal segment beats {work_id}
	mux.HandleFunc("GET /v1/environments/{id}/work/{work_id}", s.handle(s.getWork))
	mux.HandleFunc("POST /v1/environments/{id}/work/{work_id}", s.handle(s.updateWork)) // metadata patch
	mux.HandleFunc("POST /v1/environments/{id}/work/{work_id}/ack", s.handle(s.ackWork))
	mux.HandleFunc("POST /v1/environments/{id}/work/{work_id}/heartbeat", s.handle(s.heartbeatWork))
	mux.HandleFunc("POST /v1/environments/{id}/work/{work_id}/stop", s.handleNoContent(s.stopWork))
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

	return withRequestID(withTracing(dispatchAuth(pool, mux)))
}

// dispatchAuth picks the auth scheme by path and runs it before the router, so
// no request reaches a handler — or a ServeMux redirect — unauthenticated. Work
// API paths take the Authorization: Bearer environment key; the session events
// subtree and the skill read routes are dual-auth (a worker's Bearer key or the
// management x-api-key); everything else takes the management x-api-key.
func dispatchAuth(pool *pgxpool.Pool, next http.Handler) http.Handler {
	work := requireEnvironmentKey(pool, next)
	mgmt := requireAPIKey(pool, next)
	sessionEvents := dispatchSessionEventsAuth(pool, next)
	skillReads := dualAuth(requireEnvironmentKey(pool, next), mgmt)
	fileReads := dualAuth(requireEnvironmentKey(pool, next), mgmt)
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
		// fail-closed and driverless: a request that percent-encodes a literal
		// route segment (e.g. /%65vents) is not recognized as a worker route and
		// falls to management auth — a 401, never an over-authorization.
		p := r.URL.EscapedPath()
		switch {
		case isWorkPath(p):
			work.ServeHTTP(w, r)
		case isSessionEventsPath(p), r.Method == http.MethodGet && isBareSessionPath(p):
			sessionEvents.ServeHTTP(w, r)
		case r.Method == http.MethodGet && isSkillReadPath(p):
			skillReads.ServeHTTP(w, r)
		case r.Method == http.MethodGet && isFileReadPath(p):
			fileReads.ServeHTTP(w, r)
		default:
			mgmt.ServeHTTP(w, r)
		}
	})
}

// dualAuth picks between a worker's environment-key lane and management auth
// by the rule dispatchSessionEventsAuth documents: the env lane only when a
// Bearer is present AND no non-empty x-api-key is; otherwise management.
func dualAuth(env, mgmt http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := bearerToken(r); ok && r.Header.Get("x-api-key") == "" {
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
func dispatchSessionEventsAuth(pool *pgxpool.Pool, next http.Handler) http.Handler {
	return dualAuth(requireEnvironmentKeyForSession(pool, next), requireAPIKey(pool, next))
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

// handle adapts a typed handler to http.HandlerFunc: JSON out, error envelope
// on failure. The reference returns 200 for every successful call it answers
// with a body, including creates; the bodiless exception is Stop, which uses
// handleNoContent.
func (s *server) handle(fn func(*http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
func (s *server) handleNoContent(fn func(*http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
