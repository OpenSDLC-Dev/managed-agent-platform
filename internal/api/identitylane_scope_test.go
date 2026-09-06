package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
)

// The identity lane's tenancy half (plan 42 §6.2, §7.1): which workspace a human
// request runs in, and what the anthropic-workspace-id header may do about it.
// The fixture is identitylane_test.go's — a real fake provider and a frozen
// clock — with a second workspace inserted by SQL, because no route creates one
// until slice 6.

// laneAgents is the route these tests drive: the most permissive one there is,
// so a refusal is never the role matrix answering first.
const laneAgents = "/v1/agents"

// The two names decision 4's fail-closed refusal must name, spelled out because
// they are what an operator types.
const (
	laneVarClaimWorkspaces = "IDENTITY_CLAIM_WORKSPACES"
	laneVarWorkspaceMap    = "IDENTITY_WORKSPACE_MAP"
)

// laneWorkspace registers a second workspace and returns its id. Archived rows
// stay listed and stop resolving, so a test asks for one directly.
func laneWorkspace(t *testing.T, s *laneServer, name string, archived bool) string {
	t.Helper()
	id := domain.NewID(domain.PrefixWorkspace).String()
	if _, err := s.pool.Exec(context.Background(),
		`INSERT INTO workspaces (id, org_id, name) VALUES ($1, 'default', $2)`, id, name); err != nil {
		t.Fatalf("insert workspace %s: %v", name, err)
	}
	if archived {
		if _, err := s.pool.Exec(context.Background(),
			`UPDATE workspaces SET archived_at = now() WHERE id = $1`, id); err != nil {
			t.Fatalf("archive workspace %s: %v", name, err)
		}
	}
	return id
}

// laneScoped issues a request with a human token and an optional workspace
// header, and returns the status, the error type, the body and the two tenancy
// response headers.
type laneScopedResult struct {
	status  int
	errType string
	body    string
	org     string
	work    string
}

func laneScoped(t *testing.T, s *laneServer, token, workspace string) laneScopedResult {
	t.Helper()
	headers := map[string]string{"Authorization": "Bearer " + token}
	if workspace != "" {
		headers["anthropic-workspace-id"] = workspace
	}
	res := s.doRaw(http.MethodGet, laneAgents, nil, headers)
	out := laneScopedResult{
		org:  res.Header.Get("anthropic-organization-id"),
		work: res.Header.Get("anthropic-workspace-id"),
	}
	out.status, out.errType, out.body = laneRead(t, res)
	return out
}

// laneWantScope asserts a served request and the workspace it ran in.
func laneWantScope(t *testing.T, what string, got laneScopedResult, wantWorkspace string) {
	t.Helper()
	if got.status != http.StatusOK {
		t.Fatalf("%s: status %d, error type %q, want 200 (%s)", what, got.status, got.errType, got.body)
	}
	if got.work != wantWorkspace {
		t.Errorf("%s: anthropic-workspace-id = %q, want %q", what, got.work, wantWorkspace)
	}
	if got.org != "default" {
		t.Errorf("%s: anthropic-organization-id = %q, want %q", what, got.org, "default")
	}
}

// TestIdentityLaneScopeWithNoClaimConfigured is decision 4's fail-closed arm.
// The deployment that configures no membership is every deployment that
// predates this slice, and it keeps working — until a second live workspace
// exists, at which point there is nothing to guess at and the lane refuses.
func TestIdentityLaneScopeWithNoClaimConfigured(t *testing.T) {
	s := newLaneServer(t)
	admin := s.token("platform-admins")

	t.Run("one live workspace is the scope", func(t *testing.T) {
		laneWantScope(t, "the seeded deployment", laneScoped(t, s, admin, ""), "default")
	})

	t.Run("an archived second workspace changes nothing", func(t *testing.T) {
		laneWorkspace(t, s, "archived-b", true)
		laneWantScope(t, "beside an archived workspace", laneScoped(t, s, admin, ""), "default")
	})

	t.Run("both headers are stamped on an authenticated 4xx", func(t *testing.T) {
		// A viewer on an admin-only route: the scope resolved, then the role
		// refused. The stamp is what says the request was tenanted before it was
		// denied.
		res := s.doRaw(http.MethodGet, consoleAPIKeysPath, nil,
			map[string]string{"Authorization": "Bearer " + s.token("platform-read")})
		org, work := res.Header.Get("anthropic-organization-id"), res.Header.Get("anthropic-workspace-id")
		status, errType, _ := laneRead(t, res)
		if status != http.StatusForbidden || errType != "permission_error" {
			t.Fatalf("a viewer on an admin route: status %d, error type %q, want 403 permission_error", status, errType)
		}
		if org != "default" || work != "default" {
			t.Errorf("tenancy headers = (%q, %q) on an authenticated 4xx, want both %q", org, work, "default")
		}
	})

	t.Run("a second live workspace refuses, naming the configuration", func(t *testing.T) {
		laneWorkspace(t, s, "live-b", false)
		got := laneScoped(t, s, admin, "")
		if got.status != http.StatusForbidden || got.errType != "permission_error" {
			t.Fatalf("two live workspaces with no claim: status %d, error type %q, want 403 permission_error",
				got.status, got.errType)
		}
		msg := laneMessage(t, got.body)
		for _, name := range []string{laneVarClaimWorkspaces, laneVarWorkspaceMap} {
			if !strings.Contains(msg, name) {
				t.Errorf("the refusal %q does not name %s", msg, name)
			}
		}
		if got.work != "" || got.org != "" {
			t.Errorf("tenancy headers = (%q, %q) on a request whose scope never resolved, want neither",
				got.org, got.work)
		}
	})
}

// TestIdentityLaneScopeFromClaims is the configured deployment: membership comes
// from the token, selection from the reference's own header, and the header may
// only narrow to a workspace the identity already covers.
func TestIdentityLaneScopeFromClaims(t *testing.T) {
	// The map is fixed at boot, so workspace B's id is minted before the server
	// and inserted after it — there is no route that creates one until slice 6.
	workspaceB := domain.NewID(domain.PrefixWorkspace).String()
	s := newLaneServerWith(t, func(c *identity.Config) {
		c.WorkspacesClaim = "workspaces"
		c.WorkspaceMap = map[string]string{"tenant-a": "default", "tenant-b": workspaceB}
	})
	if _, err := s.pool.Exec(context.Background(),
		`INSERT INTO workspaces (id, org_id, name) VALUES ($1, 'default', 'B')`, workspaceB); err != nil {
		t.Fatalf("insert workspace B: %v", err)
	}
	// A real workspace this identity is not a member of, for the arm that must
	// not tell a caller it exists.
	foreign := laneWorkspace(t, s, "C", false)

	// token mints an admin carrying the named workspace claim values.
	token := func(values ...string) string {
		t.Helper()
		claims := s.claims("platform-admins")
		vs := make([]any, len(values))
		for i, v := range values {
			vs[i] = v
		}
		claims["workspaces"] = vs
		return s.idp.Mint(t, claims)
	}

	t.Run("a claim resolving to nothing has no authority", func(t *testing.T) {
		got := laneScoped(t, s, token("all-employees"), "")
		if got.status != http.StatusForbidden || got.errType != "permission_error" {
			t.Fatalf("an identity mapping to no workspace: status %d, error type %q, want 403 permission_error",
				got.status, got.errType)
		}
	})

	t.Run("one member and no header is that workspace", func(t *testing.T) {
		laneWantScope(t, "a single-workspace identity", laneScoped(t, s, token("tenant-b"), ""), workspaceB)
	})

	t.Run("two members and no header must choose", func(t *testing.T) {
		got := laneScoped(t, s, token("tenant-a", "tenant-b"), "")
		if got.status != http.StatusBadRequest || got.errType != "invalid_request_error" {
			t.Fatalf("two members, no header: status %d, error type %q, want 400 invalid_request_error",
				got.status, got.errType)
		}
		if msg := laneMessage(t, got.body); !strings.Contains(msg, "anthropic-workspace-id") {
			t.Errorf("the refusal %q does not name the header that chooses", msg)
		}
	})

	t.Run("a header narrows to a member", func(t *testing.T) {
		both := token("tenant-a", "tenant-b")
		laneWantScope(t, "the header naming B", laneScoped(t, s, both, workspaceB), workspaceB)
		laneWantScope(t, "the header naming default", laneScoped(t, s, both, "default"), "default")
	})

	t.Run("a workspace outside the set is one 404 whether or not it exists", func(t *testing.T) {
		unknown := domain.NewID(domain.PrefixWorkspace).String()
		both := token("tenant-a", "tenant-b")
		foreignRes := laneScoped(t, s, both, foreign)
		unknownRes := laneScoped(t, s, both, unknown)
		for _, tc := range []struct {
			what string
			got  laneScopedResult
			id   string
		}{
			{what: "a real workspace this identity does not cover", got: foreignRes, id: foreign},
			{what: "a workspace that does not exist", got: unknownRes, id: unknown},
		} {
			if tc.got.status != http.StatusNotFound || tc.got.errType != "not_found_error" {
				t.Fatalf("%s: status %d, error type %q, want 404 not_found_error",
					tc.what, tc.got.status, tc.got.errType)
			}
			if msg := laneMessage(t, tc.got.body); msg != "Workspace `"+tc.id+"` not found." {
				t.Errorf("%s: message = %q, want the reference's own body echoing %q", tc.what, msg, tc.id)
			}
		}
		// The two bodies differ only in the id they echo — existence never leaks.
		if a, b := strings.Replace(laneMessage(t, foreignRes.body), foreign, "ID", 1),
			strings.Replace(laneMessage(t, unknownRes.body), unknown, "ID", 1); a != b {
			t.Errorf("the uncovered workspace answers %q and the unknown one %q; the difference is an existence oracle", a, b)
		}
	})

	t.Run("an archived member is outside the set too", func(t *testing.T) {
		if _, err := s.pool.Exec(context.Background(),
			`UPDATE workspaces SET archived_at = now() WHERE id = $1`, workspaceB); err != nil {
			t.Fatalf("archive workspace B: %v", err)
		}
		t.Cleanup(func() {
			if _, err := s.pool.Exec(context.Background(),
				`UPDATE workspaces SET archived_at = NULL WHERE id = $1`, workspaceB); err != nil {
				t.Fatalf("restore workspace B: %v", err)
			}
		})
		got := laneScoped(t, s, token("tenant-a", "tenant-b"), workspaceB)
		if got.status != http.StatusNotFound || got.errType != "not_found_error" {
			t.Fatalf("an archived member: status %d, error type %q, want 404 not_found_error", got.status, got.errType)
		}
		if msg := laneMessage(t, got.body); msg != "Workspace `"+workspaceB+"` not found." {
			t.Errorf("an archived member: message = %q, want the same body an uncovered workspace gets", msg)
		}
		// And the identity still runs, in the member that is still live.
		laneWantScope(t, "the surviving member", laneScoped(t, s, token("tenant-a", "tenant-b"), ""), "default")

		// An identity whose ONLY workspace is archived has no authority — the
		// same refusal a claim that mapped to nothing gets, because archive is
		// what it means: the membership is real and the deployment no longer
		// runs it.
		if got := laneScoped(t, s, token("tenant-b"), ""); got.status != http.StatusForbidden ||
			got.errType != "permission_error" {
			t.Errorf("an identity whose only member is archived: status %d, error type %q, want 403 permission_error",
				got.status, got.errType)
		}
	})
}
