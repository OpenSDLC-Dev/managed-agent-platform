package api_test

import (
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
)

// Some race fixtures need a developer grant that survives org-admin demotion.
func seedWorkspaceDeveloper(t *testing.T, s *nativeServer, id string) {
	t.Helper()
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO workspace_memberships(workspace_id,principal_id,role)
		VALUES ('default',$1,'developer')`, id); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultWorkspaceGrantAndBoundCredential(t *testing.T) {
	s := newNativeServer(t, login.ModePassword)
	admin := ownedCookie(t, s, s.seedLocal("workspace-admin", "admin", "active"))
	member := ownedObject(t, expectManagement(t, s, "POST", membersPath,
		`{"login_name":"workspace-dev","password":"long-enough-password","role":"developer"}`, admin, 200))
	id, _, ok := principal.ParsePublicID(member["id"].(string))
	if !ok {
		t.Fatal("invalid member ID")
	}
	cookie := ownedCookie(t, s, id)
	memberPath := membersPath + "/" + member["id"].(string)
	const proxy = "/api/organizations/default/proxy/v1/"
	expectManagement(t, s, "GET", proxy+"agents", "", cookie, 403)
	expectManagement(t, s, "POST", consoleAPIKeysPath, `{"name":"unused"}`, cookie, 403)
	expectManagement(t, s, "POST", memberPath, `{"default_workspace_role":"developer"}`, admin, 200)
	key, _ := issueOwnedHTTP(t, s, cookie, "workspace-key", nil)
	expectManagement(t, s, "GET", proxy+"agents", "", cookie, 200)
	assertOwnedResponse(t, s.doRaw("GET", "/v1/agents", nil, ownedHeaders("x-api-key", key)), 200)
	expectManagement(t, s, "POST", memberPath, `{"role":"user"}`, admin, 200)
	expectManagement(t, s, "GET", proxy+"agents", "", cookie, 200)
	expectManagement(t, s, "POST", memberPath, `{"default_workspace_role":"limited_developer"}`, admin, 200)
	expectManagement(t, s, "GET", proxy+"sessions", "", cookie, 403)
	assertOwnedResponse(t, s.doRaw("GET", "/v1/sessions", nil, ownedHeaders("x-api-key", key)), 200)
	expectManagement(t, s, "POST", memberPath, `{"default_workspace_role":"billing"}`, admin, 400)
	expectManagement(t, s, "POST", memberPath, `{"default_workspace_role":null}`, admin, 200)
	expectManagement(t, s, "GET", proxy+"agents", "", cookie, 403)
	assertOwnedResponse(t, s.doRaw("GET", "/v1/agents", nil, ownedHeaders("x-api-key", key)), 403)

	account := ownedObject(t, expectManagement(t, s, "POST", serviceAccountsPath, `{"name":"workspace-service"}`, admin, 200))
	alias := account["id"].(string)
	expectManagement(t, s, "POST", consoleAPIKeysPath, `{"name":"unused","principal_id":"`+alias+`"}`, admin, 404)
	expectManagement(t, s, "POST", serviceAccountsPath+"/"+alias, `{"default_workspace_role":"user"}`, admin, 200)
	expectManagement(t, s, "POST", consoleAPIKeysPath, `{"name":"unused","principal_id":"`+alias+`"}`, admin, 403)
	expectManagement(t, s, "POST", serviceAccountsPath+"/"+alias, `{"default_workspace_role":"developer"}`, admin, 200)
	serviceKey, _ := issueOwnedHTTP(t, s, admin, "service", &alias)
	assertOwnedResponse(t, s.doRaw("GET", "/v1/agents", nil, ownedHeaders("x-api-key", serviceKey)), 200)
	expectManagement(t, s, "POST", serviceAccountsPath+"/"+alias+"/archive", `{}`, admin, 200)
	assertOwnedResponse(t, s.doRaw("GET", "/v1/agents", nil, ownedHeaders("x-api-key", serviceKey)), 401)
}
