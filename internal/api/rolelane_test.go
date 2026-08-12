package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
)

// The role matrix over real HTTP. TestEveryIdentityReachableRouteDeclaresARole
// proves every route declares a minimum; this proves the minimums are the ones
// plan 31 specifies, and that the check actually fires.
//
// No route needs a real resource to answer the question being asked. A 404 for a
// made-up id is a PASS here: it means the role check let the request through and
// the handler ran. What must never appear on an allowed request is a 403, and
// what must always appear on a denied one is a 403 — which is why the table can
// cover every identity-reachable route instead of a sample.

// matrixRoute is one route and the role the plan's matrix assigns it. path is
// the ServeMux PATTERN, not a request path, so TestTheMatrixCoversEveryAnnotated-
// Route can compare this table against the registrations parsed out of server.go
// and fail if a route is added to one and not the other.
type matrixRoute struct {
	method string
	path   string
	min    identity.Role
}

// pattern renders the route as it is registered: "GET /v1/agents/{id}".
func (r matrixRoute) pattern() string { return r.method + " " + r.path }

// request turns the pattern into a path that can actually be sent, substituting
// an id that deliberately does not exist. Which id does not matter: a 404 and a
// 400 are equally good proof that the role check let the request reach the
// handler, and neither can be reached at all by a caller the check denies.
func (r matrixRoute) request() string {
	id := "x"
	switch seg := strings.SplitN(strings.TrimPrefix(r.path, "/v1/"), "/", 2)[0]; seg {
	case "agents":
		id = "agent_nonexistent"
	case "environments":
		id = "env_nonexistent"
	case "sessions":
		id = "sesn_nonexistent"
	case "vaults":
		id = "vlt_nonexistent"
	case "skills":
		id = "skill_nonexistent"
	case "files":
		id = "file_nonexistent"
	}
	return strings.NewReplacer(
		"{id}", id,
		"{rid}", "sesrsc_nonexistent",
		"{cid}", "vcrd_nonexistent",
		"{version}", "1",
	).Replace(r.path)
}

// roleMatrix mirrors docs/plan/31_console-sso-rbac.md's table, route for route:
// viewer reads everything but the environment-key surface; developer adds
// resource CRUD and session lifecycle, including the vault container; admin adds
// the credential surfaces, enumerated rather than inferred.
func roleMatrix() []matrixRoute {
	const (
		agent = "/v1/agents/{id}"
		env   = "/v1/environments/{id}"
		sesn  = "/v1/sessions/{id}"
		vault = "/v1/vaults/{id}"
		cred  = vault + "/credentials/{cid}"
		skill = "/v1/skills/{id}"
		file  = "/v1/files/{id}"
	)
	v, d, a := identity.RoleViewer, identity.RoleDeveloper, identity.RoleAdmin

	return []matrixRoute{
		// Agents.
		{"GET", "/v1/agents", v}, {"GET", agent, v}, {"GET", agent + "/versions", v},
		{"POST", "/v1/agents", d}, {"POST", agent, d}, {"POST", agent + "/archive", d},

		// Environments. The work API under /v1/environments/{id}/work is a
		// different lane entirely and is covered by TestEnvironmentKeyLane below.
		{"GET", "/v1/environments", v}, {"GET", env, v},
		{"POST", "/v1/environments", d}, {"POST", env, d},
		{"DELETE", env, d}, {"POST", env + "/archive", d},

		// Sessions, including the streaming read.
		{"GET", "/v1/sessions", v}, {"GET", sesn, v},
		{"POST", "/v1/sessions", d}, {"POST", sesn, d},
		{"DELETE", sesn, d}, {"POST", sesn + "/archive", d},
		{"GET", sesn + "/events", v}, {"POST", sesn + "/events", d},
		{"GET", sesn + "/events/stream", v},
		{"GET", sesn + "/resources", v}, {"POST", sesn + "/resources", d},
		{"GET", sesn + "/resources/{rid}", v},
		{"POST", sesn + "/resources/{rid}", d}, {"DELETE", sesn + "/resources/{rid}", d},

		// Vaults: the container is developer CRUD, the credentials inside it are
		// admin — reads excepted, which render sealed metadata only.
		{"GET", "/v1/vaults", v}, {"GET", vault, v},
		{"POST", "/v1/vaults", d}, {"POST", vault, d},
		{"DELETE", vault, d}, {"POST", vault + "/archive", d},
		{"GET", vault + "/credentials", v}, {"GET", cred, v},
		{"POST", vault + "/credentials", a}, {"POST", cred, a}, {"DELETE", cred, a},
		{"POST", cred + "/archive", a}, {"POST", cred + "/mcp_oauth_validate", a},

		// Skills, including the archive download.
		{"GET", "/v1/skills", v}, {"GET", skill, v}, {"GET", skill + "/versions", v},
		{"GET", skill + "/versions/{version}", v}, {"GET", skill + "/versions/{version}/content", v},
		{"POST", "/v1/skills", d}, {"DELETE", skill, d},
		{"POST", skill + "/versions", d}, {"DELETE", skill + "/versions/{version}", d},

		// Files, including the content download.
		{"GET", "/v1/files", v}, {"GET", file, v}, {"GET", file + "/content", v},
		{"POST", "/v1/files", d}, {"DELETE", file, d},
	}
}

func TestRoleMatrixIsEnforcedOnEveryRoute(t *testing.T) {
	s := newLaneServer(t)

	// One token per tier, minted once: the matrix is about routes, not tokens.
	//
	// RoleNone is a real caller here, not a placeholder — a person the deployment
	// authenticated whose IdP groups map to nothing. It is the only tier that is
	// denied on EVERY route, which makes it the denied-case control for the
	// viewer routes: without it, every tier under test would satisfy a viewer
	// minimum, and a viewer check that stopped running entirely would still see
	// each row pass on the handler's own 404.
	tokens := map[identity.Role]string{
		identity.RoleNone:      s.token("unmapped-group"),
		identity.RoleViewer:    s.token("platform-read"),
		identity.RoleDeveloper: s.token("platform-devs"),
		identity.RoleAdmin:     s.token("platform-admins"),
	}
	callers := []identity.Role{identity.RoleNone, identity.RoleViewer, identity.RoleDeveloper, identity.RoleAdmin}

	for _, route := range roleMatrix() {
		for _, caller := range callers {
			res := s.bearer(route.method, route.request(), tokens[caller], nil)
			status, errType, raw := laneRead(t, res)
			allowed := caller.AtLeast(route.min)
			who := string(caller)
			if caller == identity.RoleNone {
				who = "a caller with no mapped role"
			}

			switch {
			case allowed && status == http.StatusForbidden:
				t.Errorf("%s: %s was denied, but the matrix puts this route at %s",
					route.pattern(), who, route.min)
			case allowed && status == http.StatusUnauthorized:
				t.Errorf("%s: %s got 401; a verified token must never read as no credential",
					route.pattern(), who)
			case allowed && status >= 500:
				t.Errorf("%s: %s got %d — the role check passed but the handler failed (%v)",
					route.pattern(), who, status, laneMessage(t, raw))
			case !allowed && status != http.StatusForbidden:
				t.Errorf("%s: %s got %d, want 403 — the matrix puts this route at %s",
					route.pattern(), who, status, route.min)
			case !allowed:
				if errType != "permission_error" {
					t.Errorf("%s: %s denied with error type %q, want permission_error",
						route.pattern(), who, errType)
				}
				// The denial names the ROUTE's requirement, never the caller's
				// own role: an operator's real question is "what does this need",
				// and echoing the caller's role back leaks nothing useful.
				if msg := laneMessage(t, raw); !strings.Contains(msg, string(route.min)) {
					t.Errorf("%s: %s denied with %q, which does not name the required %s role",
						route.pattern(), who, msg, route.min)
				}
			}
		}
	}
}

// TestAnAdminCanWorkTheEnvironmentKeySurfaceEndToEnd covers the direction a
// deny-only suite cannot: that the gate lets the right caller THROUGH.
//
// Who is denied on this surface is already pinned over real HTTP by
// TestIdentityLaneEnvironmentKeyRoutesRequireAdmin (viewer and developer on the
// list, the issue and the revoke). What nothing asserted is the allow path of
// handleNoContent, which exactly two routes use — this revoke at admin, and the
// work-API stop, whose lane has no role to check. So a regression that made
// handleNoContent deny every human would have passed the whole suite, and the
// completeness test cannot see it either: parsing the route table proves a role
// is DECLARED, never that the adapter honours it at runtime.
func TestAnAdminCanWorkTheEnvironmentKeySurfaceEndToEnd(t *testing.T) {
	s := newLaneServer(t)
	envID := s.env()
	admin := s.token("platform-admins")

	// Issue: noStore(handle(admin, …)). The response is the OAuth-shaped
	// {access_token, expires_in}; the key's id comes from the listing.
	status, _, raw := laneRead(t, s.bearer(http.MethodPost, consoleTokens(envID), admin,
		map[string]any{"name": "build-host-1"}))
	if status != http.StatusOK {
		t.Fatalf("admin issuing an environment key: status %d (%v)", status, laneMessage(t, raw))
	}
	var issued map[string]any
	if err := json.Unmarshal([]byte(raw), &issued); err != nil {
		t.Fatalf("decode issued key: %v", err)
	}
	if token, _ := issued["access_token"].(string); token == "" {
		t.Fatalf("issued response carried no access_token: %s", raw)
	}

	// List: handle(admin, …), and the id to revoke.
	status, _, raw = laneRead(t, s.bearer(http.MethodGet, consoleTokens(envID), admin, nil))
	if status != http.StatusOK {
		t.Fatalf("admin listing environment keys: status %d (%v)", status, laneMessage(t, raw))
	}
	var listed map[string]any
	if err := json.Unmarshal([]byte(raw), &listed); err != nil {
		t.Fatalf("decode listing: %v", err)
	}
	rows := listData(t, listed)
	if len(rows) != 1 {
		t.Fatalf("listed %d keys, want the 1 just issued", len(rows))
	}
	keyID, _ := rows[0]["id"].(string)
	if !strings.HasPrefix(keyID, "envkey_") {
		t.Fatalf("listed id = %q, want the envkey_ prefix", keyID)
	}

	// Revoke: handleNoContent(admin, …), whose success is a bodiless 204.
	res := s.bearer(http.MethodPost, consoleRevoke(envID, keyID), admin, nil)
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatalf("read revoke response: %v", err)
	}
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("admin revoking: status %d, body %q, want 204", res.StatusCode, body)
	}
	if len(body) != 0 {
		t.Errorf("204 carried a body: %q", body)
	}
}

// TestAHumanCreatedSessionRecordsThePrincipal closes the loop slice 2 could only
// test in-package. Its note there said the audit column could not be observed
// over HTTP because every mutation sat at RoleNone, so no human could create a
// session — and that slice 3 would make it reachable. It does: POST /v1/sessions
// is developer now, so the column can be watched from outside, and this asserts
// what principalFrom actually wrote.
//
// The failure it guards is silent by construction: created_by is nullable and
// nothing downstream reads it, so a human-created session that recorded nobody
// would look exactly like a working one until someone needed the audit trail.
func TestAHumanCreatedSessionRecordsThePrincipal(t *testing.T) {
	s := newLaneServer(t)
	agent := createAgent(t, s.tserver, map[string]any{"name": "a", "model": "m"})
	envID := s.env()

	status, _, raw := laneRead(t, s.bearer(http.MethodPost, "/v1/sessions", s.token("platform-devs"),
		map[string]any{"agent": agent["id"], "environment_id": envID}))
	if status != http.StatusOK {
		t.Fatalf("a developer creating a session: status %d (%v)", status, laneMessage(t, raw))
	}
	var session map[string]any
	if err := json.Unmarshal([]byte(raw), &session); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	sessionID, _ := session["id"].(string)

	var createdBy *string
	if err := s.pool.QueryRow(t.Context(),
		`SELECT created_by FROM sessions WHERE id = $1`, sessionID).Scan(&createdBy); err != nil {
		t.Fatalf("read created_by: %v", err)
	}
	if createdBy == nil || *createdBy == "" {
		t.Fatal("created_by is empty on a session a human created; the audit trail records nobody")
	}

	// And it is the principal's id, not the api key's name and not the subject.
	var principalID string
	if err := s.pool.QueryRow(t.Context(), `SELECT id FROM principals`).Scan(&principalID); err != nil {
		t.Fatalf("read principal: %v", err)
	}
	if *createdBy != principalID {
		t.Errorf("created_by = %q, want the principal id %q", *createdBy, principalID)
	}
}

// TestEnvironmentKeyLaneIsUntouchedByRoles pins the other half of the promise:
// annotating 53 routes changed nothing for machine callers. The work API carries
// identity.RoleNone because a worker holds no role for requireRole to check —
// which is a different statement from "identity never arrives here", and that one
// is false (see TestAnEncodedPathCannotSlipPastTheWorkLane). Either way a
// worker's environment key must keep working exactly as it did.
func TestEnvironmentKeyLaneIsUntouchedByRoles(t *testing.T) {
	s := newLaneServer(t)
	envID := s.env()

	// The management key: no role, full authority, every tier of the matrix.
	for _, path := range []string{"/v1/agents", "/v1/vaults", "/v1/environments"} {
		if status, body := s.do(http.MethodGet, path, nil); status != http.StatusOK {
			t.Errorf("management key GET %s: status %d, want 200 (%v)", path, status, body)
		}
	}

	// And the work lane, whose routes the matrix leaves at RoleNone: a role
	// check there would deny the worker that the whole platform depends on.
	key := issueKey(t, s.pool, envID, "worker-1")
	res := s.doRaw(http.MethodGet, "/v1/environments/"+envID+"/work", nil,
		map[string]string{"Authorization": "Bearer " + key})
	if status, errType := laneStatus(t, res); status != http.StatusOK {
		t.Errorf("environment key listing work: status %d, error %q, want 200", status, errType)
	}
}
