package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/pagination"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
)

const membersPath = "/api/console/organizations/default/members"
const serviceAccountsPath = "/api/console/organizations/default/service_accounts"

func seedMember(t *testing.T, s *nativeServer, subject, role, source string) string {
	t.Helper()
	id := domain.NewID(domain.PrefixPrincipal).String()
	_, err := s.pool.Exec(context.Background(), `INSERT INTO principals(id,issuer,subject,role,role_source,email,display_name)
		VALUES ($1,'https://idp.example',$2,$3,$4,$5,$2)`, id, subject, role, source, subject+"@example.test")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func expectManagement(t *testing.T, s *nativeServer, method, path, body string, headers http.Header, want int) []byte {
	t.Helper()
	res, raw := s.request(method, path, body, headers)
	if res.StatusCode != want {
		t.Fatalf("%s %s status=%d want=%d body=%s", method, path, res.StatusCode, want, raw)
	}
	return raw
}

// Dropping cookie-only dispatch, the admin floor, or CSRF must fail these rows.
func TestMembersCookieOnly(t *testing.T) {
	s := newNativeServer(t, login.ModeOIDC)
	admin := seedMember(t, s, "admin", "admin", "console")
	cookie, session := s.issue(admin)
	user := seedMember(t, s, "user", "user", "console")
	userCookie, userSession := s.issue(user)
	developer := seedMember(t, s, "dev", "developer", "console")
	devCookie, devSession := s.issue(developer)
	id := principal.PublicID(user, "user")
	for _, route := range []struct{ method, path, body string }{
		{"GET", membersPath, ""}, {"POST", membersPath, `{}`},
		{"GET", membersPath + "/" + id, ""}, {"POST", membersPath + "/" + id, `{"role":"developer"}`},
		{"DELETE", membersPath + "/" + id, ""},
		{"GET", serviceAccountsPath, ""}, {"POST", serviceAccountsPath, `{"name":"ci"}`},
		{"GET", serviceAccountsPath + "/svac_unknown", ""},
		{"POST", serviceAccountsPath + "/svac_unknown", `{}`},
		{"POST", serviceAccountsPath + "/svac_unknown/archive", ""},
	} {
		t.Run(route.method+route.path, func(t *testing.T) {
			for name, h := range map[string]http.Header{
				"none":          nil,
				"key":           {"X-Api-Key": {testKey}},
				"bearer":        {"Authorization": {"Bearer " + s.idp.Mint(t, s.idp.Claims(laneAudience, time.Now()))}},
				"cookie-key":    {"Cookie": {cookie}, "X-Api-Key": {testKey}},
				"cookie-bearer": {"Cookie": {cookie}, "Authorization": {"Bearer invalid"}},
			} {
				t.Run(name, func(t *testing.T) { expectManagement(t, s, route.method, route.path, route.body, h, 401) })
			}
			for name, h := range map[string]http.Header{"user": nativeHeaders(userCookie, userSession.CSRFToken), "developer": nativeHeaders(devCookie, devSession.CSRFToken)} {
				t.Run(name, func(t *testing.T) {
					res, raw := s.request(route.method, route.path, route.body, h)
					if res.StatusCode != 403 || res.Header.Get("x-platform-signed-out") != "" {
						t.Fatalf("permission refusal=%d marker=%q body=%s", res.StatusCode, res.Header.Get("x-platform-signed-out"), raw)
					}
				})
			}
			if route.method != "GET" {
				for _, h := range []http.Header{nativeHeaders(cookie, ""), {"Cookie": {cookie}, "X-CSRF-Token": {session.CSRFToken}, "Origin": {"https://evil.example"}}} {
					expectManagement(t, s, route.method, route.path, route.body, h, 403)
				}
			}
		})
	}
	h := nativeHeaders(cookie, session.CSRFToken)
	expectManagement(t, s, "GET", strings.Replace(membersPath, "/members", "/%6dembers", 1), "", h, 200)
	expectManagement(t, s, "GET", strings.Replace(membersPath, "/members", "/%6dembers", 1), "", http.Header{"X-Api-Key": {testKey}}, 401)
	expectManagement(t, s, "GET", strings.Replace(membersPath, "/default/", "/other/", 1), "", h, 400)
	expectManagement(t, s, "GET", strings.Replace(membersPath, "/default/", "/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/", 1), "", h, 401)
	for _, path := range []string{membersPath, membersPath + "/" + id, serviceAccountsPath, serviceAccountsPath + "/svac_unknown", serviceAccountsPath + "/svac_unknown/archive"} {
		expectManagement(t, s, "PUT", path, "", h, 405)
		expectManagement(t, s, "PUT", path, "", http.Header{"X-Api-Key": {testKey}}, 401)
	}
	// Existing private key management retains both its key lane and admin floor.
	expectManagement(t, s, "GET", "/api/console/organizations/default/workspaces/default/api_keys", "", http.Header{"X-Api-Key": {testKey}}, 200)
	expectManagement(t, s, "GET", "/api/console/organizations/default/workspaces/default/api_keys", "", nativeHeaders(devCookie, devSession.CSRFToken), 403)
	legacy := newTestServer(t)
	res := legacy.doRaw("GET", membersPath, nil, map[string]string{"x-api-key": testKey})
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("disabled-native member admission=%d", res.StatusCode)
	}
}

// Accepting legacy viewer aliases or SDK-only roles would grant unintended authority.
func TestMappedRoleCannotBeChanged(t *testing.T) {
	s := newNativeServer(t, login.ModeOIDC)
	admin := seedMember(t, s, "admin", "admin", "console")
	cookie, session := s.issue(admin)
	h := nativeHeaders(cookie, session.CSRFToken)
	mapped := seedMember(t, s, "mapped", "developer", "idp")
	path := membersPath + "/" + principal.PublicID(mapped, "user")
	expectManagement(t, s, "POST", path, `{"role":"admin"}`, h, 409)
	for _, role := range []string{"viewer", "billing", "owner", "primary_owner", "membership_admin", "managed", "claude_code_user", "Admin", ""} {
		t.Run(role, func(t *testing.T) { expectManagement(t, s, "POST", path, fmt.Sprintf(`{"role":%q}`, role), h, 400) })
	}
	expectManagement(t, s, "POST", path, `{"role":null}`, h, 400)
	expectManagement(t, s, "POST", path, `{"role":7}`, h, 400)
	expectManagement(t, s, "POST", path, `{"role":"developer","status":"active"}`, h, 400)
	// Mapping-owned role does not prevent removal, provided another human admin exists.
	expectManagement(t, s, "DELETE", path, "", h, 200)
}

// Deleting the identity row, resetting status, or conflating target removal with caller sign-out fails this.
func TestRemovedMemberCannotReturn(t *testing.T) {
	s := newNativeServer(t, login.ModeOIDC)
	admin := seedMember(t, s, "admin", "admin", "console")
	cookie, session := s.issue(admin)
	h := nativeHeaders(cookie, session.CSRFToken)
	target := seedMember(t, s, "removed", "developer", "console")
	targetCookie, _ := s.issue(target)
	id := principal.PublicID(target, "user")
	res, raw := s.request("DELETE", membersPath+"/"+id, "", h)
	var deleted anthropic.BetaOrganizationUserRemoveResponse
	if res.StatusCode != 200 || json.Unmarshal(raw, &deleted) != nil || deleted.ID != id || deleted.Type != "user_deleted" || res.Header.Get("x-platform-signed-out") != "" {
		t.Fatalf("removal=%d marker=%q body=%s", res.StatusCode, res.Header.Get("x-platform-signed-out"), raw)
	}
	expectManagement(t, s, "DELETE", membersPath+"/"+id, "", h, 200)
	res, raw = s.request("POST", membersPath+"/"+id, `{"role":"admin"}`, h)
	if res.StatusCode != 400 || res.Header.Get("x-platform-signed-out") != "" {
		t.Fatalf("inactive target=%d marker=%q body=%s", res.StatusCode, res.Header.Get("x-platform-signed-out"), raw)
	}
	expectManagement(t, s, "GET", membersPath+"/"+id, "", h, 200)
	expectManagement(t, s, "GET", "/api/auth/session", "", h, 200)
	res, _ = s.request("GET", "/api/auth/session", "", nativeHeaders(targetCookie, ""))
	if res.StatusCode != 403 || res.Header.Get("x-platform-signed-out") != "1" {
		t.Fatalf("target session=%d", res.StatusCode)
	}
	_, err := principal.Resolve(context.Background(), s.pool, identity.Identity{Issuer: "https://idp.example", Subject: "removed", Email: "new@example.test", Role: identity.RoleAdmin})
	if err != principal.ErrDeactivated {
		t.Fatalf("removed Resolve=%v", err)
	}
	if _, err := s.pool.Exec(context.Background(), `UPDATE principals SET email='',display_name='' WHERE id=$1`, target); err != nil {
		t.Fatal(err)
	}
	_, err = principal.Resolve(context.Background(), s.pool, identity.Identity{Issuer: "https://idp.example", Subject: "removed", Role: identity.RoleAdmin})
	if err != principal.ErrDeactivated {
		t.Fatalf("scrubbed Resolve=%v", err)
	}
}

// A non-atomic credential write, wrong mode, reused tombstone, or password disclosure fails this.
func TestLocalMemberCreation(t *testing.T) {
	s := newNativeServer(t, login.ModePassword)
	admin := seedMember(t, s, "admin", "admin", "console")
	cookie, session := s.issue(admin)
	h := nativeHeaders(cookie, session.CSRFToken)
	body := fmt.Sprintf(`{"login_name":"local.person","password":%q,"name":"Local Person","role":"admin"}`, localPassword)
	raw := expectManagement(t, s, "POST", membersPath, body, h, 200)
	var member map[string]any
	if json.Unmarshal(raw, &member) != nil || member["login_name"] != "local.person" || member["role_source"] != "console" || member["status"] != "active" || member["name"] != "Local Person" || member["role"] != "admin" || member["email"] != "" {
		t.Fatalf("local DTO=%s", raw)
	}
	assertNativeManagementSchema(t, "BetaUser", raw, false)
	for _, secret := range []string{localPassword, "password_hash", "issuer", "subject", "$argon2"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("local response disclosed %q", secret)
		}
	}
	var internalID, hash string
	if err := s.pool.QueryRow(context.Background(), `SELECT p.id,a.password_hash FROM principals p JOIN local_accounts a ON a.principal_id=p.id WHERE p.issuer=$1 AND p.subject='local.person'`, login.LocalIssuer).Scan(&internalID, &hash); err != nil || !login.VerifyPassword(hash, localPassword) {
		t.Fatalf("local row+credential err=%v", err)
	}
	expectManagement(t, s, "POST", membersPath, body, h, 409)
	expectManagement(t, s, "POST", "/api/auth/login", fmt.Sprintf(`{"login_name":"local.person","password":%q}`, localPassword), nativeHeaders("", ""), 200)
	for _, invalid := range []string{
		`{"login_name":"Upper","password":"long-enough-password","role":"user"}`,
		`{"login_name":"bad","password":"short","role":"user"}`,
		`{"login_name":"bad","password":"long-enough-password","role":"viewer"}`,
		`{"login_name":"bad","password":"long-enough-password","role":"billing"}`,
		`{"login_name":"bad","password":"long-enough-password","role":null}`,
		`{"login_name":"bad","password":"long-enough-password"}`,
		`{"login_name":"bad","password":"long-enough-password","role":"user","email":"bad@example.test"}`,
	} {
		expectManagement(t, s, "POST", membersPath, invalid, h, 400)
	}
	expectManagement(t, s, "DELETE", membersPath+"/"+member["id"].(string), "", h, 200)
	expectManagement(t, s, "POST", membersPath, body, h, 409)
	res, raw := s.request("POST", "/api/auth/login", fmt.Sprintf(`{"login_name":"local.person","password":%q}`, localPassword), nativeHeaders("", ""))
	if res.StatusCode != 403 || res.Header.Get("x-platform-signed-out") != "1" {
		t.Fatalf("removed local login=%d body=%s", res.StatusCode, raw)
	}
	var orphan int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM principals WHERE issuer=$1 AND subject='bad'`, login.LocalIssuer).Scan(&orphan); err != nil || orphan != 0 {
		t.Fatalf("invalid local create wrote %d: %v", orphan, err)
	}
	other := newNativeServer(t, login.ModeOIDC)
	otherAdmin := seedMember(t, other, "admin", "admin", "console")
	otherCookie, otherSession := other.issue(otherAdmin)
	expectManagement(t, other, "POST", membersPath, body, nativeHeaders(otherCookie, otherSession.CSRFToken), 400)
}

// Wrong pagination dialect, missing native fields, or inclusion of tombstones fails this.
func TestMembersPagination(t *testing.T) {
	s := newNativeServer(t, login.ModeOIDC)
	admin := seedMember(t, s, "admin", "admin", "console")
	cookie, session := s.issue(admin)
	h := nativeHeaders(cookie, session.CSRFToken)
	for i := range 22 {
		seedMember(t, s, fmt.Sprintf("person-%02d", i), "developer", "console")
	}
	var page pagination.Page[anthropic.BetaOrganizationUser]
	raw := expectManagement(t, s, "GET", membersPath, "", h, 200)
	if json.Unmarshal(raw, &page) != nil || len(page.Data) != 20 || !page.HasMore || page.FirstID != page.Data[0].ID || page.LastID != page.Data[19].ID {
		t.Fatalf("default page=%s", raw)
	}
	var first []json.RawMessage
	var envelope struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	first = envelope.Data
	for _, member := range first {
		assertNativeManagementSchema(t, "BetaUser", member, false)
	}
	seen := map[string]bool{}
	for _, member := range page.Data {
		seen[member.ID] = true
	}
	raw = expectManagement(t, s, "GET", membersPath+"?after_id="+url.QueryEscape(page.LastID), "", h, 200)
	var next pagination.Page[anthropic.BetaOrganizationUser]
	if json.Unmarshal(raw, &next) != nil || len(next.Data) != 3 || next.HasMore {
		t.Fatalf("next page=%s", raw)
	}
	for _, member := range next.Data {
		if seen[member.ID] {
			t.Fatal("duplicate member across pages")
		}
		seen[member.ID] = true
	}
	raw = expectManagement(t, s, "GET", membersPath+"?before_id="+url.QueryEscape(next.FirstID), "", h, 200)
	var before pagination.Page[anthropic.BetaOrganizationUser]
	if json.Unmarshal(raw, &before) != nil || len(before.Data) != 20 || before.FirstID != page.FirstID || before.LastID != page.LastID {
		t.Fatalf("before page=%s", raw)
	}
	raw = expectManagement(t, s, "GET", membersPath+"?email=missing%40example.test", "", h, 200)
	var empty map[string]any
	if json.Unmarshal(raw, &empty) != nil || empty["first_id"] != nil || empty["last_id"] != nil || empty["has_more"] != false || len(empty["data"].([]any)) != 0 {
		t.Fatalf("empty page=%s", raw)
	}
	raw = expectManagement(t, s, "GET", membersPath+"?roles%5B%5D=developer&roles%5B%5D=admin&email=admin%40example.test&limit=1000", "", h, 200)
	if json.Unmarshal(raw, &before) != nil || len(before.Data) != 1 || before.Data[0].ID != principal.PublicID(admin, "user") {
		t.Fatalf("filtered=%s", raw)
	}
	for _, query := range []string{"limit=0", "limit=1001", "limit=bad", "limit=1&limit=2", "roles%5B%5D=viewer", "after_id=bad", "before_id=bad", "after_id=" + page.FirstID + "&before_id=" + page.LastID} {
		expectManagement(t, s, "GET", membersPath+"?"+query, "", h, 400)
	}
	expectManagement(t, s, "GET", membersPath+"/"+principal.PublicID(domain.NewID(domain.PrefixPrincipal).String(), "user"), "", h, 404)
	removed := next.Data[0].ID
	expectManagement(t, s, "DELETE", membersPath+"/"+removed, "", h, 200)
	raw = expectManagement(t, s, "GET", membersPath+"?limit=1000", "", h, 200)
	if json.Unmarshal(raw, &before) != nil || len(before.Data) != 22 {
		t.Fatalf("active-only=%s", raw)
	}
}
