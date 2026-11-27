package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
)

func TestWorkspaceManagementLifecycle(t *testing.T) {
	s := newTestServer(t)
	const path = "/api/console/organizations/default/workspaces"
	post := func(s *tserver, suffix string, body any, want int) map[string]any {
		t.Helper()
		headers := s.cookieHeaders()
		headers["Content-Type"] = "application/json"
		status, row := s.decode(s.doRaw("POST", path+suffix, body, headers))
		if status != want {
			t.Fatalf("POST %s: %d %v, want %d", suffix, status, row, want)
		}
		return row
	}
	b := post(s, "", map[string]any{"name": "Research", "display_color": "#123abc"}, 200)
	id, _ := b["id"].(string)
	if len(id) != 33 || b["display_color"] != "#123abc" || b["type"] != "workspace" {
		t.Fatalf("new workspace: %v", b)
	}
	list := func(s *tserver, query string) []map[string]any {
		t.Helper()
		res := s.doRaw("GET", path+query, nil, s.cookieHeaders())
		defer res.Body.Close()
		var rows []map[string]any
		if res.StatusCode != 200 || json.NewDecoder(res.Body).Decode(&rows) != nil {
			t.Fatalf("list HTTP %d", res.StatusCode)
		}
		return rows
	}
	if rows := list(s, ""); len(rows) != 1 || rows[0]["id"] != id {
		t.Fatalf("active list: %v", rows)
	}
	post(s, "/default", map[string]any{"name": "renamed"}, 400)
	post(s, "/default/archive", map[string]any{}, 400)
	member := domain.NewID(domain.PrefixPrincipal).String()
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO principals(id,issuer,subject,role) VALUES($1,'fixture','member','developer')`, member); err != nil {
		t.Fatal(err)
	}
	grantFixture(t, s.pool, member, "developer")
	m := login.New(s.pool, login.Config{Mode: login.ModePassword, PublicOrigin: nativeOrigin, Development: true})
	issued, err := m.Issue(t.Context(), member)
	if err != nil {
		t.Fatal(err)
	}
	other := *s
	other.cookie = "map_session=" + issued.Token
	other.csrf = issued.Session.CSRFToken
	alias := principal.PublicID(member, "user")
	grant := func(role string) {
		post(s, "/"+id+"/members", map[string]any{"principal_id": alias, "role": role}, 200)
	}
	grant("user")
	post(&other, "/"+id, map[string]any{"name": "denied"}, 403)
	post(&other, "", map[string]any{"name": "denied"}, 403)
	grant("admin")
	post(&other, "/"+id, map[string]any{"name": "Renamed"}, 200)
	post(&other, "/"+id+"/archive", map[string]any{}, 403)
	if rows := list(&other, ""); len(rows) != 1 || rows[0]["name"] != "Renamed" {
		t.Fatalf("workspace administration list: %v", rows)
	}
	key := post(s, "/"+id+"/api_keys", map[string]any{"name": "Test B"}, 200)
	worker := *s
	worker.key = key["raw_key"].(string)
	aid, eid := readableFixture(t, &worker)
	dep := createDeployment(t, &worker, deploymentBody(aid, eid))
	status, session := worker.do("POST", "/v1/sessions", map[string]any{"agent": aid, "environment_id": eid})
	if status != 200 {
		t.Fatalf("session %d %v", status, session)
	}
	sid := session["id"].(string)
	pgtest.SetSessionStatus(t, s.pool, domain.ID(sid), "running")
	if _, err := s.pool.Exec(t.Context(), `UPDATE sessions SET running_since=clock_timestamp()-interval '20 seconds' WHERE id=$1`, sid); err != nil {
		t.Fatal(err)
	}
	wid := domain.NewID(domain.PrefixWork).String()
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO work_items(id,workspace_id,environment_id,session_id,kind,state,lease_expires_at) VALUES($1,$2,$3,$4,'model_turn','active',now()+interval '1 minute')`, wid, id, eid, session["id"]); err != nil {
		t.Fatal(err)
	}
	post(s, "/"+id+"/archive", map[string]any{"name": "unrelated"}, 400)
	row := post(s, "/"+id+"/archive", map[string]any{}, 200)
	if row["archived_at"] == nil {
		t.Fatal(row)
	}
	if rows := list(s, ""); len(rows) != 0 {
		t.Fatalf("archived in active list: %v", rows)
	}
	if rows := list(s, "?include_archived=true"); len(rows) != 1 || rows[0]["id"] != id || rows[0]["archived_at"] == nil {
		t.Fatalf("archive history: %v", rows)
	}
	if status, _ := s.doCookie("GET", path+"/"+id, nil); status != 404 {
		t.Fatalf("archive detail %d", status)
	}
	if status, _ := worker.do("GET", "/v1/agents", nil); status != http.StatusUnauthorized {
		t.Errorf("archived key HTTP %d", status)
	}
	var ended bool
	if err := s.pool.QueryRow(t.Context(), `SELECT d.paused_at IS NOT NULL AND w.state='stopped' AND w.lease_expires_at IS NULL FROM deployments d,work_items w WHERE d.id=$1 AND w.id=$2`, dep["id"], wid).Scan(&ended); err != nil || !ended {
		t.Errorf("archive work cleanup: %v, %v", ended, err)
	}
	var closed bool
	if err := s.pool.QueryRow(t.Context(), `SELECT status='terminated' AND running_since IS NULL AND active_seconds>=19 AND EXISTS (SELECT 1 FROM events WHERE session_id=s.id AND type='session.status_terminated') FROM sessions s WHERE id=$1`, sid).Scan(&closed); err != nil || !closed {
		t.Errorf("archive did not close the session's event ledger and active clock: %v %v", closed, err)
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE workspaces SET archived_at=NULL WHERE id=$1`, id); err == nil {
		t.Error("workspace archive was reversible")
	}
}

// The same directory entries gain independent workspace grants; inherited rights remain live.
func TestWorkspaceManagementInheritedAndServiceGrants(t *testing.T) {
	s := newTestServer(t)
	secondWorkspace(t, s)
	const b = "/api/console/organizations/default/workspaces/wrkspc_00000000000000000000000002/members"
	ctx := t.Context()
	h := s.cookieHeaders()
	h["Content-Type"] = "application/json"
	sa := domain.NewID(domain.PrefixPrincipal).String()
	billing := domain.NewID(domain.PrefixPrincipal).String()
	if _, err := s.pool.Exec(ctx, `INSERT INTO principals(id,issuer,subject,kind,role) VALUES($1,'fixture','service','service_account','developer'),($2,'fixture','billing','user','billing')`, sa, billing); err != nil {
		t.Fatal(err)
	}
	change := func(method, path string, body any, want int) {
		t.Helper()
		status, v := s.decode(s.doRaw(method, path, body, h))
		if status != want {
			t.Fatalf("%s %s %d %v", method, path, status, v)
		}
	}
	type memberRow struct {
		ID, Name, Role string
		Inherited      bool
	}
	rows := func() map[string]memberRow {
		t.Helper()
		res := s.doRaw("GET", b, nil, h)
		defer res.Body.Close()
		var items []memberRow
		if res.StatusCode != 200 || json.NewDecoder(res.Body).Decode(&items) != nil {
			t.Fatalf("members HTTP %d", res.StatusCode)
		}
		out := map[string]memberRow{}
		for _, m := range items {
			out[m.ID] = m
		}
		return out
	}
	service := principal.PublicID(sa, "service_account")
	bill := principal.PublicID(billing, "user")
	if x := rows()[bill]; x.Role != "billing" || !x.Inherited || x.Name != bill {
		t.Fatal(x)
	}
	change("POST", b, map[string]string{"principal_id": bill, "role": "developer"}, 400)
	change("POST", b, map[string]string{"principal_id": bill, "role": "admin"}, 200)
	change("POST", b, map[string]string{"principal_id": service, "role": "limited_developer"}, 200)
	got := rows()
	if got[service].Name != "service" || got[service].Role != "limited_developer" || got[bill].Role != "admin" || got[bill].Inherited {
		t.Fatal(got)
	}
	var defaultRole *string
	if err := s.pool.QueryRow(ctx, `SELECT workspace_role($1,'default')`, sa).Scan(&defaultRole); err != nil || defaultRole != nil {
		t.Fatalf("service Default grant %v %v", defaultRole, err)
	}
	change("DELETE", b+"/"+service, nil, 200)
	change("DELETE", b+"/"+bill, nil, 200)
	got = rows()
	if _, ok := got[service]; ok || got[bill].Role != "billing" || !got[bill].Inherited {
		t.Fatal(got)
	}
}
