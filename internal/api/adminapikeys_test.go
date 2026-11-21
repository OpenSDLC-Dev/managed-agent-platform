package api_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/apikey"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/clioauth"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

const auditKeysPath = "/v1/organizations/api_keys"

func TestAdminAPIKeyPagingAndProjection(t *testing.T) {
	s := newTestServer(t)
	token := issueCLITokens(t, s, "org:admin")["access_token"].(string)
	client := anthropic.NewClient(option.WithBaseURL(s.url+"/"), option.WithAuthToken(token), option.WithHeaderDel("x-api-key"))
	keys := client.Beta.Organization.APIKeys
	ctx := t.Context()
	issuer := ownedPrincipalFixture(t, s.pool, "user", "admin")
	sa := ownedPrincipalFixture(t, s.pool, "service_account", "developer")
	var ids []string
	for range 4 {
		_, id := ownedKeyFixture(t, s.pool, sa, issuer)
		ids = append(ids, id)
	}
	// Identical timestamps exercise the ID tie-breaker. Retirement outranks expiry.
	if _, err := s.pool.Exec(ctx, `UPDATE api_keys SET status='archived',created_at='2020-01-01',expires_at='2021-01-01' WHERE id=ANY($1)`, ids); err != nil {
		t.Fatal(err)
	}
	slices.Sort(ids)
	slices.Reverse(ids)
	user := principal.PublicID(issuer, "user")
	filter := anthropic.BetaOrganizationAPIKeyListParams{
		CreatedByUserID: anthropic.String(user), Limit: anthropic.Int(1),
		WorkspaceID: anthropic.String(clioauth.WorkspaceID), Status: "archived",
	}
	for _, backwards := range []bool{false, true} {
		want := ids
		if backwards {
			filter.BeforeID = anthropic.String(ids[3])
			want = []string{ids[2], ids[1], ids[0]}
		}
		var got []string
		pages := keys.ListAutoPaging(ctx, filter)
		for pages.Next() {
			got = append(got, pages.Current().ID)
			if len(got) > len(want) {
				t.Fatal("pagination repeated rows")
			}
		}
		if err := pages.Err(); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("backwards=%t got %v want %v", backwards, got, want)
		}
	}
	filter.Limit = anthropic.Int(2)
	page, err := keys.List(ctx, filter)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != 2 || page.Data[0].ID != ids[1] || page.Data[1].ID != ids[2] || page.FirstID != ids[1] || page.LastID != ids[2] || !page.HasMore {
		t.Fatal("before_id must return the nearest rows in normal list order")
	}

	key, err := keys.Get(ctx, ids[1])
	if err != nil {
		t.Fatal(err)
	}
	if key.Status != "archived" || key.CreatedBy.ID != user || key.CreatedBy.Type != "user" || key.Principal.ServiceAccountID != principal.PublicID(sa, "service_account") ||
		key.Principal.Type != "service_account_actor" || key.Scope.Type != "workspace" || key.Scope.WorkspaceID != clioauth.WorkspaceID || key.JSON.WorkspaceID.Valid() {
		t.Fatal("wrong public actor, scope or archive projection")
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(key.RawJSON()), &obj); err != nil {
		t.Fatal(err)
	}
	if len(obj) != 11 || obj["partial_key_hint"] != nil || obj["workspace_id"] != nil {
		t.Fatalf("unexpected public fields: %d", len(obj))
	}
	for _, field := range []string{"id", "type", "name", "created_at", "created_by", "partial_key_hint", "expires_at", "status", "workspace_id", "principal", "scope"} {
		if _, ok := obj[field]; !ok {
			t.Fatalf("missing %s", field)
		}
	}
	legacy := insertHistoricalKey(t, s.pool, "historical", "unused historical fixture")
	if _, err := s.pool.Exec(ctx, `UPDATE api_keys SET status='archived',created_by=$1 WHERE id=$2`, ids[0], legacy); err != nil {
		t.Fatal(err)
	}
	key, err = keys.Get(ctx, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if key.JSON.Principal.Valid() || key.JSON.CreatedBy.Valid() {
		t.Fatal("historical machine issuer invented a public owner or user creator")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE api_keys SET created_by=$1 WHERE id=$2`, issuer, legacy); err != nil {
		t.Fatal(err)
	}
	key, err = keys.Get(ctx, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if key.JSON.Principal.Valid() || key.CreatedBy.ID != user || key.CreatedBy.Type != "user" {
		t.Fatal("historical human issuer lost its user alias or acquired ownership")
	}

	// The single served tenancy applies to lookup, cursor resolution and collection reads.
	if _, err := s.pool.Exec(ctx, `UPDATE api_keys SET org_id='another' WHERE id=$1`, legacy); err != nil {
		t.Fatal(err)
	}
	visible, err := keys.List(ctx, anthropic.BetaOrganizationAPIKeyListParams{Status: "archived", CreatedByUserID: anthropic.String(user)})
	if err != nil || len(visible.Data) != len(ids) {
		t.Fatalf("archived keys in the granted organization: %v", err)
	}
	auth := asBearer(token)
	for _, path := range []string{"?status=active&workspace_id=missing", "?status=archived&created_by_user_id=invalid", "?status=inactive", "?after_id=" + legacy} {
		status, body := s.decode(s.doRaw("GET", auditKeysPath+path, nil, auth))
		data, ok := body["data"].([]any)
		if status != 200 || !ok || len(data) != 0 || body["first_id"] != nil || body["last_id"] != nil || body["has_more"] != false {
			t.Errorf("unexpected empty page for %s: HTTP %d", path, status)
		}
	}
	for _, id := range []string{legacy, domain.NewID(domain.PrefixAPIKey).String()} {
		status, _ := s.decode(s.doRaw("GET", auditKeysPath+"/"+id, nil, auth))
		if status != 404 {
			t.Errorf("hidden or unknown key returned HTTP %d", status)
		}
	}
	if _, err := s.pool.Exec(ctx, `UPDATE api_keys SET status='inactive',expires_at='2000-01-01' WHERE id=$1`, ids[1]); err != nil {
		t.Fatal(err)
	}
	expired, err := keys.List(ctx, anthropic.BetaOrganizationAPIKeyListParams{Status: "expired"})
	if err != nil || len(expired.Data) != 1 || expired.Data[0].ID != ids[1] || expired.Data[0].Status != "expired" {
		t.Fatalf("effective expiry filter: %v", err)
	}
}

func TestAdminAPIKeyReadCredentials(t *testing.T) {
	s := newTestServer(t)
	orgToken := issueCLITokens(t, s, "org:admin")["access_token"].(string)
	developerToken := issueCLITokens(t, s, "user:developer")["access_token"].(string)
	var id, issuer string
	if err := s.pool.QueryRow(t.Context(), `SELECT id,owner_principal_id FROM api_keys WHERE owned_key_hash=$1`, apikey.Hash(testKey)).Scan(&id, &issuer); err != nil {
		t.Fatal(err)
	}
	issuedAdminKey, _ := ownedKeyFixture(t, s.pool, issuer, issuer)
	for _, path := range []string{auditKeysPath, auditKeysPath + "/" + id} {
		for _, tc := range []struct {
			name    string
			headers map[string]string
			want    int
		}{
			{"missing", nil, 401},
			{"developer-grant", asBearer(developerToken), 403},
			{"browser-cookie", s.cookieHeaders(), 401},
			{"owned-admin-key", map[string]string{"x-api-key": testKey}, 403},
			{"owned-admin-bearer", asBearer(issuedAdminKey), 403},
			{"owned-key-and-oauth", map[string]string{"x-api-key": testKey, "Authorization": "Bearer " + orgToken}, 403},
			{"other-workspace", map[string]string{"anthropic-workspace-id": "wrkspc_other", "Authorization": "Bearer " + orgToken}, 403},
			{"admin-grant", asBearer(orgToken), 200},
		} {
			status, _ := s.decode(s.doRaw("GET", path, nil, tc.headers))
			if status != tc.want {
				t.Errorf("%s %s: HTTP %d want %d", tc.name, path, status, tc.want)
			}
		}
	}
	status, _ := s.decode(s.doRaw("GET", "/api/organizations/default/proxy"+auditKeysPath, nil, s.cookieHeaders()))
	if status != 404 {
		t.Fatalf("private proxy answered HTTP %d", status)
	}
	for _, q := range []string{"?after_id=a&before_id=b", "?limit=0", "?limit=1001", "?limit=x", "?status=unknown", "?status=active&status=archived", "?after_id=%00"} {
		status, _ := s.decode(s.doRaw("GET", auditKeysPath+q, nil, asBearer(orgToken)))
		if status != 400 {
			t.Errorf("%s: HTTP %d", q, status)
		}
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE principals SET role='developer' WHERE id=$1`, issuer); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{auditKeysPath, auditKeysPath + "/" + id} {
		status, _ := s.decode(s.doRaw(http.MethodGet, path, nil, asBearer(orgToken)))
		if status != 403 {
			t.Errorf("demoted current principal: HTTP %d", status)
		}
	}
}
