// Package api is the control plane's whole HTTP surface: every route the
// platform serves, and the dispatcher deciding which credential may reach which
// route. Five surfaces share one http.ServeMux, four credentials reach them, and
// dispatchAuth (server.go) picks the credential by path and runs it before the
// router — splitting the routes across nested muxes would let ServeMux's own
// path-cleaning and subtree-slash redirects answer an unauthenticated request
// before auth had run. Surfaces and credentials do not line up one to one, and
// the exceptions are the part a reader has to carry: they are collected at the
// end of the map below rather than left implicit in it.
//
// The public /v1 wire — agents, environments, sessions with their events and
// resources, vaults and credentials, skills, files — is wire-compatible with
// Anthropic Managed Agents down to paths, JSON shapes, ID prefixes and the
// pagination and error envelopes, and takes the management x-api-key (auth.go)
// everywhere the dual-auth set at the end does not say otherwise;
// anthropic-version / anthropic-beta and ?beta=true are accepted and ignored.
// The work API under /v1/environments/{id}/work is the second credential: a
// BYOC worker presents an environment key as Authorization: Bearer, and
// workScope asserts that key's environment against the path's, so one key
// drives one queue and no other (envauth.go, workapi.go). The off-wire /api
// console namespace mirrors the reference console's own private paths rather
// than inventing a namespace (consoleapi.go, consoleapikeys.go); it is reached
// by a verified human whose asserted role must clear the route's declared
// minimum (identitylane.go), or by
// the management key, which no dispatch can confine to the console: one
// credential cannot distinguish its callers, so "console-only" means off the
// wire and built for the console, nothing more (consoleapi.go). That human lane
// is not confined to /api — it is the management arm's alternate credential
// everywhere, tried only when no x-api-key is offered, which is why every route
// declares its role beside its path in server.go. Fourth, the internal
// gate-config endpoint, and with it the fourth credential: a session's egress
// gate authenticates with its per-session gtk_ token and fetches the
// networking policy and resolved credentials it needs (gateauth.go,
// gateconfig.go) — off /v1, and registered as a divergence in
// docs/DIVERGENCES.md. Fifth is the event layer (events.go): the SSE tail of a
// session's log, and the state-machine triggers a posted batch fires — waking an
// idle session, resuming a suspended turn, clearing a confirmation gate — which
// is why sending events is a transaction holding the session row rather than a
// bare append. Both sit in the dual-auth set below.
//
// The exceptions, then, and they run one way: the environment key is not
// confined to the work API. Every route a BYOC worker needs to set up and drive
// its session is dual-auth (dualAuth) — reached by a worker's Bearer
// environment key, or by a management or human caller, whichever the request
// carries. That is the session events subtree; the bare GET /v1/sessions/{id};
// the GET skill reads at and under /v1/skills/{id} — the skill itself, its
// versions, a version, and a version's /content (isSkillReadPath, server.go);
// and the GET /v1/files/{id}/content download, which is the
// worker's own SetupSkills and SetupFiles path. Skill content and file content
// therefore do NOT need a management key. What keeps the lane narrow is
// per-resource scoping inside the handlers rather than the dispatcher: a
// session route's key must own the session
// (requireEnvironmentKeyForSession), a file download's key must belong to an
// environment in which some session mounts that file (downloadFile), and
// skills, workspace-global resources every environment's sandboxes consume,
// need no scoping at all. Everything else on /v1 — the collections, the file
// metadata read, every mutation — is management-only, and outside the events
// subtree the dual-auth routes are GET-only.
//
// The cross-cutting fact no single file makes obvious: dispatchAuth classifies
// on r.URL.EscapedPath() while ServeMux matches the DECODED path, and the
// asymmetry cuts both ways on purpose. An encoded %2F cannot forge a segment the
// router does not also see, so an environment key can never be admitted to a
// management-only handler. In the other direction a percent-encoded spelling of
// a machine route (/%77ork) fails the lane predicates, falls through to the
// management arm, and is then decoded and routed to that machine registration
// anyway — arriving there, with identity enabled, on the human lane with a real
// principal. What refuses it is identity.RoleNone on those registrations, which
// no role satisfies. RoleNone on the work and gate routes is therefore a live
// denial, not a placeholder (TestAnEncodedPathCannotSlipPastTheWorkLane).
package api
