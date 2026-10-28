package api_test

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/pagination"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
)

// Assert required fields and native types against the actual pinned bundled spec,
// not a second handwritten DTO. Member-only private extensions are allowed.
func assertNativeManagementSchema(t *testing.T, name string, raw []byte, exact bool) {
	t.Helper()
	dir, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/anthropics/anthropic-sdk-go").Output()
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(filepath.Join(strings.TrimSpace(string(dir)), "scripts", "mock-spec.json.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	var spec struct {
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.NewDecoder(gz).Decode(&spec); err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(spec.Components.Schemas[name], &schema); err != nil || len(schema.Required) == 0 {
		t.Fatalf("missing native schema %s: %v", name, err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	for _, key := range schema.Required {
		if _, ok := obj[key]; !ok {
			t.Fatalf("%s missing required %s: %s", name, key, raw)
		}
	}
	if exact && len(obj) != len(schema.Properties) {
		t.Fatalf("%s fields differ: %s", name, raw)
	}
	var matches func(json.RawMessage, json.RawMessage) bool
	matches = func(rule, value json.RawMessage) bool {
		var r struct {
			Type   string            `json:"type"`
			Ref    string            `json:"$ref"`
			AnyOf  []json.RawMessage `json:"anyOf"`
			Enum   []string          `json:"enum"`
			Const  string            `json:"const"`
			Format string            `json:"format"`
		}
		if json.Unmarshal(rule, &r) != nil {
			return false
		}
		if r.Ref != "" {
			return matches(spec.Components.Schemas[strings.TrimPrefix(r.Ref, "#/components/schemas/")], value)
		}
		if len(r.AnyOf) > 0 {
			for _, alt := range r.AnyOf {
				if matches(alt, value) {
					return true
				}
			}
			return false
		}
		if string(value) == "null" {
			return r.Type == "null"
		}
		var str string
		if r.Type != "string" || json.Unmarshal(value, &str) != nil {
			return false
		}
		if r.Const != "" && str != r.Const {
			return false
		}
		if len(r.Enum) > 0 && !slices.Contains(r.Enum, str) {
			return false
		}
		if r.Format == "date-time" {
			_, err := time.Parse(time.RFC3339Nano, str)
			return err == nil
		}
		return true
	}
	for key, rule := range schema.Properties {
		if value, ok := obj[key]; ok && !matches(rule, value) {
			t.Fatalf("%s.%s violates pinned schema: %s", name, key, value)
		}
	}
}

// Wrong native fields/nulls, mutable names, role aliases, resurrection or offset pagination fails this.
func TestServiceAccountContract(t *testing.T) {
	s := newNativeServer(t, login.ModePassword)
	admin := seedMember(t, s, "admin", "admin", "console")
	cookie, session := s.issue(admin)
	h := nativeHeaders(cookie, session.CSRFToken)
	raw := expectManagement(t, s, "POST", serviceAccountsPath, `{"name":"ci-deploy-bot","description":"builds"}`, h, 200)
	assertNativeManagementSchema(t, "BetaServiceAccount", raw, true)
	var account anthropic.BetaServiceAccount
	if err := json.Unmarshal(raw, &account); err != nil || !strings.HasPrefix(account.ID, "svac_") || account.Type != "service_account" || account.Name != "ci-deploy-bot" || account.Description != "builds" || account.OrganizationRole != "developer" || account.CreatedByActorID != session.ID || account.CreatedAt.IsZero() || account.UpdatedAt.IsZero() {
		t.Fatalf("SDK service-account decode=%+v err=%v raw=%s", account, err, raw)
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil || obj["archived_at"] != nil || obj["archived_by_actor_id"] != nil || obj["updated_by_actor_id"] != nil {
		t.Fatalf("new nullable fields=%s", raw)
	}
	path := serviceAccountsPath + "/" + account.ID
	expectManagement(t, s, "GET", path, "", h, 200)
	expectManagement(t, s, "POST", serviceAccountsPath, `{"name":"ci-deploy-bot"}`, h, 409)
	for i, body := range []string{
		`{"name":""}`, `{"name":"Bad"}`, `{"name":"has_underscore"}`, `{"name":"contains space"}`,
		fmt.Sprintf(`{"name":%q}`, strings.Repeat("x", 256)),
		fmt.Sprintf(`{"name":"too-long-description","description":%q}`, strings.Repeat("界", 2001)),
		`{"name":"ci","organization_role":"user"}`, `{"name":"ci","organization_role":"viewer"}`,
		`{"name":"ci","organization_role":"billing"}`, `{"name":"ci","organization_role":null}`,
		`{"name":"ci","description":7}`, `{"name":"ci","unexpected":true}`,
	} {
		t.Run(fmt.Sprintf("invalid-create-%d", i), func(t *testing.T) { expectManagement(t, s, "POST", serviceAccountsPath, body, h, 400) })
	}
	for _, body := range []string{`{"name":"renamed"}`, `{"organization_role":"user"}`, `{"organization_role":"viewer"}`, `{"organization_role":"billing"}`, `{"description":false}`, `{"status":"active"}`} {
		expectManagement(t, s, "POST", path, body, h, 400)
	}
	raw = expectManagement(t, s, "POST", path, `{"organization_role":"admin"}`, h, 200)
	var updated anthropic.BetaServiceAccount
	if json.Unmarshal(raw, &updated) != nil || updated.OrganizationRole != "admin" || updated.Description != "builds" || updated.UpdatedByActorID != session.ID || updated.UpdatedAt.Before(account.UpdatedAt) {
		t.Fatalf("updated=%s", raw)
	}
	raw = expectManagement(t, s, "POST", path, `{"description":null,"organization_role":null}`, h, 200)
	assertNativeManagementSchema(t, "BetaServiceAccount", raw, true)
	if json.Unmarshal(raw, &updated) != nil || updated.Description != "" || updated.OrganizationRole != "admin" {
		t.Fatalf("null update=%s", raw)
	}
	// Null description is legal in native create; maximum is characters, not bytes.
	expectManagement(t, s, "POST", serviceAccountsPath, `{"name":"nullable","description":null}`, h, 200)
	expectManagement(t, s, "POST", serviceAccountsPath, fmt.Sprintf(`{"name":%q,"description":%q}`, strings.Repeat("x", 255), strings.Repeat("界", 2000)), h, 200)
	for _, id := range []string{"bad", "svac_unknown", principal.PublicID(admin, "service_account"), principal.PublicID(admin, "user")} {
		for _, method := range []string{"GET", "POST"} {
			expectManagement(t, s, method, serviceAccountsPath+"/"+id, `{}`, h, 404)
		}
		expectManagement(t, s, "POST", serviceAccountsPath+"/"+id+"/archive", "", h, 404)
	}
	internalID, _, ok := principal.ParsePublicID(account.ID)
	if !ok {
		t.Fatal("invalid private alias")
	}
	if _, err := s.manager.Issue(context.Background(), internalID); err != login.ErrUnauthenticated {
		t.Fatalf("SA cookie issue=%v", err)
	}
	// An invented user alias does not change the stored principal's kind.
	expectManagement(t, s, "GET", membersPath+"/"+principal.PublicID(internalID, "user"), "", h, 404)
	expectManagement(t, s, "DELETE", membersPath+"/"+principal.PublicID(internalID, "user"), "", h, 404)
	raw = expectManagement(t, s, "POST", path+"/archive", "", h, 200)
	var archived anthropic.BetaServiceAccount
	if json.Unmarshal(raw, &archived) != nil || archived.ArchivedAt.IsZero() || archived.ArchivedByActorID != session.ID {
		t.Fatalf("archive=%s", raw)
	}
	assertNativeManagementSchema(t, "BetaServiceAccount", raw, true)
	again := expectManagement(t, s, "POST", path+"/archive", "", h, 200)
	if string(again) != string(raw) {
		t.Fatalf("idempotent archive changed fields\n%s\n%s", raw, again)
	}
	expectManagement(t, s, "GET", path, "", h, 200)
	expectManagement(t, s, "POST", path, `{}`, h, 400)
	expectManagement(t, s, "POST", serviceAccountsPath, `{"name":"ci-deploy-bot"}`, h, 409)
	var keyCount int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM api_keys`).Scan(&keyCount); err != nil || keyCount != 1 {
		t.Fatalf("SA operations issued keys: count=%d err=%v", keyCount, err)
	}
}

// Native update requires an object; nullable fields do not make the body nullable.
func TestServiceAccountUpdateRequiresObject(t *testing.T) {
	s := newNativeServer(t, login.ModePassword)
	admin := seedMember(t, s, "admin", "admin", "console")
	cookie, session := s.issue(admin)
	h := nativeHeaders(cookie, session.CSRFToken)
	raw := expectManagement(t, s, "POST", serviceAccountsPath, `{"name":"body-contract"}`, h, 200)
	var account anthropic.BetaServiceAccount
	if err := json.Unmarshal(raw, &account); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"null", "", "[]", `{"description":7}`} {
		t.Run(body, func(t *testing.T) { expectManagement(t, s, "POST", serviceAccountsPath+"/"+account.ID, body, h, 400) })
	}
	expectManagement(t, s, "POST", serviceAccountsPath+"/"+account.ID, `{}`, h, 200)
}

func TestServiceAccountPagination(t *testing.T) {
	s := newNativeServer(t, login.ModeOIDC)
	admin := seedMember(t, s, "admin", "admin", "console")
	cookie, session := s.issue(admin)
	h := nativeHeaders(cookie, session.CSRFToken)
	ids := []string{}
	for i := range 23 {
		raw := expectManagement(t, s, "POST", serviceAccountsPath, fmt.Sprintf(`{"name":"ci-%02d"}`, i), h, 200)
		var a anthropic.BetaServiceAccount
		if err := json.Unmarshal(raw, &a); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, a.ID)
	}
	readPage := func(query string) pagination.PageCursor[anthropic.BetaServiceAccount] {
		t.Helper()
		raw := expectManagement(t, s, "GET", serviceAccountsPath+query, "", h, 200)
		var p pagination.PageCursor[anthropic.BetaServiceAccount]
		if json.Unmarshal(raw, &p) != nil {
			t.Fatalf("SDK page decode=%s", raw)
		}
		return p
	}
	first := readPage("")
	if len(first.Data) != 20 || first.Data[0].ID != ids[22] || first.NextPage == "" || strings.HasPrefix(first.NextPage, "svac_") {
		t.Fatalf("first page=%+v", first)
	}
	next := readPage("?page=" + first.NextPage)
	if len(next.Data) != 3 || next.NextPage != "" || next.Data[0].ID != ids[2] || next.Data[2].ID != ids[0] {
		t.Fatalf("next page=%+v", next)
	}
	expectManagement(t, s, "POST", serviceAccountsPath+"/"+ids[22]+"/archive", "", h, 200)
	active := readPage("?limit=100")
	if len(active.Data) != 22 || active.Data[0].ID != ids[21] {
		t.Fatalf("archive filter=%+v", active)
	}
	all := readPage("?include_archived=true&limit=100")
	if len(all.Data) != 23 || all.Data[0].ID != ids[22] {
		t.Fatalf("include_archived=%+v", all)
	}
	for _, query := range []string{"limit=0", "limit=101", "limit=no", "limit=1&limit=2", "page=garbage", "include_archived=garbage"} {
		expectManagement(t, s, "GET", serviceAccountsPath+"?"+query, "", h, 400)
	}
	empty := newNativeServer(t, login.ModeOIDC)
	admin = seedMember(t, empty, "admin", "admin", "console")
	cookie, session = empty.issue(admin)
	raw := expectManagement(t, empty, "GET", serviceAccountsPath, "", nativeHeaders(cookie, session.CSRFToken), 200)
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil || obj["next_page"] != nil || len(obj["data"].([]any)) != 0 {
		t.Fatalf("empty SA page=%s", raw)
	}
}
