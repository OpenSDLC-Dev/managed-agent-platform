package api_test

import (
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

// matrixRoute is one route and the role the plan's matrix assigns it.
type matrixRoute struct {
	method string
	path   string
	min    identity.Role
}

// roleMatrix mirrors docs/plan/31_console-sso-rbac.md's table, route for route:
// viewer reads everything but the environment-key surface; developer adds
// resource CRUD and session lifecycle, including the vault container; admin adds
// the credential surfaces, enumerated rather than inferred.
func roleMatrix() []matrixRoute {
	const (
		agent = "/v1/agents/agent_nonexistent"
		env   = "/v1/environments/env_nonexistent"
		sesn  = "/v1/sessions/sesn_nonexistent"
		vault = "/v1/vaults/vlt_nonexistent"
		cred  = vault + "/credentials/vcrd_nonexistent"
		skill = "/v1/skills/skill_nonexistent"
		file  = "/v1/files/file_nonexistent"
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
		{"GET", sesn + "/resources/sesrsc_x", v},
		{"POST", sesn + "/resources/sesrsc_x", d}, {"DELETE", sesn + "/resources/sesrsc_x", d},

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
		{"GET", skill + "/versions/1", v}, {"GET", skill + "/versions/1/content", v},
		{"POST", "/v1/skills", d}, {"DELETE", skill, d},
		{"POST", skill + "/versions", d}, {"DELETE", skill + "/versions/1", d},

		// Files, including the content download.
		{"GET", "/v1/files", v}, {"GET", file, v}, {"GET", file + "/content", v},
		{"POST", "/v1/files", d}, {"DELETE", file, d},
	}
}

func TestRoleMatrixIsEnforcedOnEveryRoute(t *testing.T) {
	s := newLaneServer(t)

	// One token per tier, minted once: the matrix is about routes, not tokens.
	tokens := map[identity.Role]string{
		identity.RoleViewer:    s.token("platform-read"),
		identity.RoleDeveloper: s.token("platform-devs"),
		identity.RoleAdmin:     s.token("platform-admins"),
	}

	for _, route := range roleMatrix() {
		for _, caller := range []identity.Role{identity.RoleViewer, identity.RoleDeveloper, identity.RoleAdmin} {
			res := s.bearer(route.method, route.path, tokens[caller], nil)
			status, errType, raw := laneRead(t, res)
			allowed := caller.AtLeast(route.min)

			switch {
			case allowed && status == http.StatusForbidden:
				t.Errorf("%s %s: %s was denied, but the matrix puts this route at %s",
					route.method, route.path, caller, route.min)
			case allowed && status == http.StatusUnauthorized:
				t.Errorf("%s %s: %s got 401; a verified token must never read as no credential",
					route.method, route.path, caller)
			case allowed && status >= 500:
				t.Errorf("%s %s: %s got %d — the role check passed but the handler failed (%v)",
					route.method, route.path, caller, status, laneMessage(t, raw))
			case !allowed && status != http.StatusForbidden:
				t.Errorf("%s %s: %s got %d, want 403 — the matrix puts this route at %s",
					route.method, route.path, caller, status, route.min)
			case !allowed:
				if errType != "permission_error" {
					t.Errorf("%s %s: %s denied with error type %q, want permission_error",
						route.method, route.path, caller, errType)
				}
				// The denial names the ROUTE's requirement, never the caller's
				// own role: an operator's real question is "what does this need",
				// and echoing the caller's role back leaks nothing useful.
				if msg := laneMessage(t, raw); !strings.Contains(msg, string(route.min)) {
					t.Errorf("%s %s: %s denied with %q, which does not name the required %s role",
						route.method, route.path, caller, msg, route.min)
				}
			}
		}
	}
}

// TestEnvironmentKeySurfaceIsAdminOnly covers the one console surface, which the
// matrix deliberately keeps whole at admin — listing included, because which
// hosts hold live credentials and what they are named is itself an inventory.
// It needs a real environment, so it is separate from the table above.
func TestEnvironmentKeySurfaceIsAdminOnly(t *testing.T) {
	s := newLaneServer(t)
	envID := s.env()
	list := consoleTokens(envID)

	for _, tc := range []struct {
		role  string
		allow bool
	}{
		{"platform-read", false},
		{"platform-devs", false},
		{"platform-admins", true},
	} {
		res := s.bearer(http.MethodGet, list, s.token(tc.role), nil)
		status, errType, raw := laneRead(t, res)
		if tc.allow {
			if status != http.StatusOK {
				t.Errorf("%s listing environment keys: status %d, want 200 (%v)", tc.role, status, laneMessage(t, raw))
			}
			continue
		}
		if status != http.StatusForbidden {
			t.Errorf("%s listing environment keys: status %d, want 403 — the env-key surface is gated whole", tc.role, status)
		}
		if errType != "permission_error" {
			t.Errorf("%s: error type %q, want permission_error", tc.role, errType)
		}
		if msg := laneMessage(t, raw); !strings.Contains(msg, string(identity.RoleAdmin)) {
			t.Errorf("%s: denial %q does not name the required admin role", tc.role, msg)
		}
	}
}

// TestEnvironmentKeyLaneIsUntouchedByRoles pins the other half of the promise:
// annotating 53 routes changed nothing for machine callers. The work API carries
// identity.RoleNone precisely because identity never reaches it, and a worker's
// environment key must keep working exactly as it did.
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
