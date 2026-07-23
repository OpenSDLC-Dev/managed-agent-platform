package api_test

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// uploadOneFile uploads a tiny file and returns its file_ id.
func uploadOneFile(t *testing.T, s *tserver, name string) string {
	t.Helper()
	oct := "application/octet-stream"
	obj := s.uploadFile(t, name, &oct, "contents of "+name)
	id, _ := obj["id"].(string)
	if !strings.HasPrefix(id, "file_") {
		t.Fatalf("upload %s: id = %q, want a file_ id", name, id)
	}
	return id
}

// resourcesOf pulls the resources[] array out of a session object.
func resourcesOf(t *testing.T, sess map[string]any) []map[string]any {
	t.Helper()
	raw, ok := sess["resources"].([]any)
	if !ok {
		t.Fatalf("session missing resources[] array: %v", sess["resources"])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("resource entry is not an object: %v", e)
		}
		out = append(out, m)
	}
	return out
}

// wantResourceFields asserts the materialized file-resource wire shape — every
// field is api:"required" (betasessionresource.go:176-209).
func wantResourceFields(t *testing.T, res map[string]any) {
	t.Helper()
	wantFields(t, res, "id", "created_at", "file_id", "mount_path", "type", "updated_at")
	if id, _ := res["id"].(string); !strings.HasPrefix(id, "sesrsc_") {
		t.Errorf("resource id = %q, want a sesrsc_ id", res["id"])
	}
	if res["type"] != "file" {
		t.Errorf("resource type = %v, want file", res["type"])
	}
}

// TestSessionFileResourceRoundTrip is the slice-2 end-to-end integration test:
// a real control-plane handler over a real Postgres (pgtest) and blob store,
// exercised entirely through HTTP — upload a file, mount it at create with the
// default and an explicit mount_path, then drive every sub-resource endpoint
// (list / get / add / delete).
func TestSessionFileResourceRoundTrip(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	fileA := uploadOneFile(t, s, "a.txt")
	fileB := uploadOneFile(t, s, "b.txt")

	// Create with the default mount path.
	sess := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{map[string]any{"type": "file", "file_id": fileA}},
	})
	sid := sess["id"].(string)
	res := resourcesOf(t, sess)
	if len(res) != 1 {
		t.Fatalf("resources = %v, want one", res)
	}
	wantResourceFields(t, res[0])
	if res[0]["file_id"] != fileA {
		t.Errorf("file_id = %v, want %s", res[0]["file_id"], fileA)
	}
	if want := "/mnt/session/uploads/" + fileA; res[0]["mount_path"] != want {
		t.Errorf("mount_path = %v, want the default %s", res[0]["mount_path"], want)
	}
	ridA := res[0]["id"].(string)

	// The session GET renders the same resources.
	got := createGetSession(t, s, sid)
	if gr := resourcesOf(t, got); len(gr) != 1 || gr[0]["id"] != ridA {
		t.Errorf("session GET resources = %v, want the one created", gr)
	}

	// Explicit mount path is honored.
	sess2 := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{map[string]any{"type": "file", "file_id": fileA, "mount_path": "/data/in.txt"}},
	})
	if r := resourcesOf(t, sess2); r[0]["mount_path"] != "/data/in.txt" {
		t.Errorf("explicit mount_path = %v, want /data/in.txt", r[0]["mount_path"])
	}

	// List the first session's resources.
	status, list := s.do("GET", "/v1/sessions/"+sid+"/resources", nil)
	if status != http.StatusOK {
		t.Fatalf("list resources: %d %v", status, list)
	}
	if data := listData(t, list); len(data) != 1 || data[0]["id"] != ridA {
		t.Errorf("list = %v, want the one resource", data)
	}

	// Get the resource by id.
	status, one := s.do("GET", "/v1/sessions/"+sid+"/resources/"+ridA, nil)
	if status != http.StatusOK {
		t.Fatalf("get resource: %d %v", status, one)
	}
	wantResourceFields(t, one)
	if one["file_id"] != fileA {
		t.Errorf("get resource file_id = %v, want %s", one["file_id"], fileA)
	}

	// Add a second resource (a different file), then confirm the session shows two.
	status, added := s.do("POST", "/v1/sessions/"+sid+"/resources",
		map[string]any{"type": "file", "file_id": fileB})
	if status != http.StatusOK {
		t.Fatalf("add resource: %d %v", status, added)
	}
	wantResourceFields(t, added)
	ridB := added["id"].(string)
	if gr := resourcesOf(t, createGetSession(t, s, sid)); len(gr) != 2 {
		t.Errorf("after add: %d resources, want 2", len(gr))
	}

	// Delete the first resource.
	status, del := s.do("DELETE", "/v1/sessions/"+sid+"/resources/"+ridA, nil)
	if status != http.StatusOK {
		t.Fatalf("delete resource: %d %v", status, del)
	}
	if del["id"] != ridA || del["type"] != "session_resource_deleted" {
		t.Errorf("delete response = %v, want {id:%s, type:session_resource_deleted}", del, ridA)
	}
	gr := resourcesOf(t, createGetSession(t, s, sid))
	if len(gr) != 1 || gr[0]["id"] != ridB {
		t.Errorf("after delete: resources = %v, want only %s", gr, ridB)
	}
}

// createGetSession GETs a session and asserts 200.
func createGetSession(t *testing.T, s *tserver, sid string) map[string]any {
	t.Helper()
	status, obj := s.do("GET", "/v1/sessions/"+sid, nil)
	if status != http.StatusOK {
		t.Fatalf("get session %s: %d %v", sid, status, obj)
	}
	return obj
}

// TestSessionResourceValidation covers the create- and add-time rejections.
func TestSessionResourceValidation(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	fileA := uploadOneFile(t, s, "a.txt")

	create := func(resources any) (int, map[string]any) {
		return s.do("POST", "/v1/sessions", map[string]any{
			"agent": agentID, "environment_id": envID, "resources": resources})
	}

	for name, tc := range map[string]struct {
		resources  any
		wantStatus int
	}{
		"nonexistent file":     {[]any{map[string]any{"type": "file", "file_id": "file_0000000000000000000000gk"}}, 404},
		"malformed file id":    {[]any{map[string]any{"type": "file", "file_id": "not-an-id"}}, 400},
		"missing file id":      {[]any{map[string]any{"type": "file"}}, 400},
		"github unsupported":   {[]any{map[string]any{"type": "github_repository"}}, 400},
		"memory unsupported":   {[]any{map[string]any{"type": "memory_store"}}, 400},
		"unknown type":         {[]any{map[string]any{"type": "wizard"}}, 400},
		"missing type":         {[]any{map[string]any{"file_id": fileA}}, 400},
		"unknown resource key": {[]any{map[string]any{"type": "file", "file_id": fileA, "bogus": 1}}, 400},
		"relative mount path":  {[]any{map[string]any{"type": "file", "file_id": fileA, "mount_path": "rel/path"}}, 400},
		"duplicate mount path": {[]any{
			map[string]any{"type": "file", "file_id": fileA, "mount_path": "/same"},
			map[string]any{"type": "file", "file_id": fileA, "mount_path": "/same"},
		}, 400},
	} {
		status, body := create(tc.resources)
		if status != tc.wantStatus {
			t.Errorf("%s: status %d, want %d (%v)", name, status, tc.wantStatus, body)
			continue
		}
		wantType := "invalid_request_error"
		if tc.wantStatus == 404 {
			wantType = "not_found_error"
		}
		wantErr(t, status, body, tc.wantStatus, wantType)
	}

	// A session with one resource, for the add/get/delete rejections.
	sess := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{map[string]any{"type": "file", "file_id": fileA, "mount_path": "/taken"}},
	})
	sid := sess["id"].(string)
	rid := resourcesOf(t, sess)[0]["id"].(string)

	// Add rejections.
	addCases := map[string]struct {
		body       any
		wantStatus int
	}{
		"add duplicate mount path": {map[string]any{"type": "file", "file_id": fileA, "mount_path": "/taken"}, 400},
		"add nonexistent file":     {map[string]any{"type": "file", "file_id": "file_0000000000000000000000gk"}, 404},
		"add github":               {map[string]any{"type": "github_repository"}, 400},
	}
	for name, tc := range addCases {
		status, body := s.do("POST", "/v1/sessions/"+sid+"/resources", tc.body)
		wantType := "invalid_request_error"
		if tc.wantStatus == 404 {
			wantType = "not_found_error"
		}
		if status != tc.wantStatus {
			t.Errorf("%s: status %d, want %d (%v)", name, status, tc.wantStatus, body)
			continue
		}
		wantErr(t, status, body, tc.wantStatus, wantType)
	}

	// Update (token rotation) is rejected for a file resource.
	status, body := s.do("POST", "/v1/sessions/"+sid+"/resources/"+rid,
		map[string]any{"authorization_token": "tok"})
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")

	// Unknown resource id → 404 on get and delete.
	for _, method := range []string{"GET", "DELETE"} {
		status, body := s.do(method, "/v1/sessions/"+sid+"/resources/sesrsc_0000000000000000000000gk", nil)
		wantErr(t, status, body, http.StatusNotFound, "not_found_error")
	}

	// Unknown session → 404 on the sub-resource routes.
	status, body = s.do("GET", "/v1/sessions/sesn_0000000000000000000000gk/resources", nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
}

// TestSessionResourceMutationsRejectedOnArchivedSession: add and delete are
// mutations, gated by the same archived-session lock the session update uses.
func TestSessionResourceMutationsRejectedOnArchivedSession(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	fileA := uploadOneFile(t, s, "a.txt")
	sess := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{map[string]any{"type": "file", "file_id": fileA}},
	})
	sid := sess["id"].(string)
	rid := resourcesOf(t, sess)[0]["id"].(string)

	if status, body := s.do("POST", "/v1/sessions/"+sid+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive session: %d %v", status, body)
	}

	// Reads still answer on an archived session.
	if status, _ := s.do("GET", "/v1/sessions/"+sid+"/resources", nil); status != http.StatusOK {
		t.Errorf("list on archived session = %d, want 200", status)
	}
	// Mutations are rejected.
	status, body := s.do("POST", "/v1/sessions/"+sid+"/resources",
		map[string]any{"type": "file", "file_id": fileA, "mount_path": "/late"})
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	status, body = s.do("DELETE", "/v1/sessions/"+sid+"/resources/"+rid, nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
}

// TestSessionResourceListPagination walks the in-array cursor: limit slices the
// list and yields a next_page; an omitted limit returns all.
func TestSessionResourceListPagination(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	var resources []any
	for i := 0; i < 3; i++ {
		f := uploadOneFile(t, s, "f")
		resources = append(resources, map[string]any{"type": "file", "file_id": f, "mount_path": "/m" + string(rune('a'+i))})
	}
	sess := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID, "resources": resources,
	})
	sid := sess["id"].(string)

	// Omitted limit → all three, no next_page.
	status, all := s.do("GET", "/v1/sessions/"+sid+"/resources", nil)
	if status != http.StatusOK || len(listData(t, all)) != 3 {
		t.Fatalf("list all: %d, %d rows", status, len(listData(t, all)))
	}
	if all["next_page"] != nil {
		t.Errorf("next_page = %v, want null when all returned", all["next_page"])
	}

	// limit=2 → two rows and a cursor; following it yields the last row.
	status, page1 := s.do("GET", "/v1/sessions/"+sid+"/resources?limit=2", nil)
	if status != http.StatusOK {
		t.Fatalf("page 1: %d", status)
	}
	if len(listData(t, page1)) != 2 {
		t.Errorf("page 1 rows = %d, want 2", len(listData(t, page1)))
	}
	cur, _ := page1["next_page"].(string)
	if cur == "" {
		t.Fatalf("page 1 next_page missing: %v", page1["next_page"])
	}
	status, page2 := s.do("GET", "/v1/sessions/"+sid+"/resources?limit=2&page="+cur, nil)
	if status != http.StatusOK || len(listData(t, page2)) != 1 {
		t.Errorf("page 2: %d, %d rows (want 1)", status, len(listData(t, page2)))
	}
	if page2["next_page"] != nil {
		t.Errorf("page 2 next_page = %v, want null", page2["next_page"])
	}
}

// TestSessionResourceEdgeCases exercises the sub-resource routes' shape
// validation and cursor handling — the branches the round-trip does not reach.
func TestSessionResourceEdgeCases(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	fileA := uploadOneFile(t, s, "a.txt")

	// Create-time shape errors the round-trip does not hit.
	longPath := "/" + strings.Repeat("x", 1100) // > maxMountPathBytes
	for name, resources := range map[string]any{
		"non-object element":  []any{5},
		"resources not array": "nope",
		"mount path too long": []any{map[string]any{"type": "file", "file_id": fileA, "mount_path": longPath}},
	} {
		status, body := s.do("POST", "/v1/sessions", map[string]any{
			"agent": agentID, "environment_id": envID, "resources": resources})
		if status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%v)", name, status, body)
			continue
		}
		wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	}

	sess := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{map[string]any{"type": "file", "file_id": fileA}},
	})
	sid := sess["id"].(string)

	// List parameter validation.
	unknownCursor := base64.RawURLEncoding.EncodeToString([]byte("r1|sesrsc_0000000000000000000000gk"))
	listCases := map[string]struct {
		query      string
		wantStatus int
		wantRows   int // only checked on 200
	}{
		"limit zero":         {"?limit=0", 400, 0},
		"limit not integer":  {"?limit=abc", 400, 0},
		"limit over max":     {"?limit=5000", 400, 0},
		"bad base64 cursor":  {"?page=!!!not-base64", 400, 0},
		"cursor wrong magic": {"?page=" + base64.RawURLEncoding.EncodeToString([]byte("xx")), 400, 0},
		"unknown cursor":     {"?page=" + unknownCursor, 200, 0},
	}
	for name, tc := range listCases {
		status, body := s.do("GET", "/v1/sessions/"+sid+"/resources"+tc.query, nil)
		if status != tc.wantStatus {
			t.Errorf("%s: status %d, want %d (%v)", name, status, tc.wantStatus, body)
			continue
		}
		if tc.wantStatus == http.StatusOK && len(listData(t, body)) != tc.wantRows {
			t.Errorf("%s: %d rows, want %d", name, len(listData(t, body)), tc.wantRows)
		}
	}

	// Malformed ids on the sub-resource routes → 404 (checkID / checkResourceID).
	for _, path := range []string{
		"/v1/sessions/not-a-session/resources",
		"/v1/sessions/" + sid + "/resources/not-a-resource",
	} {
		status, body := s.do("GET", path, nil)
		wantErr(t, status, body, http.StatusNotFound, "not_found_error")
	}

	// Update endpoint branches.
	rid := resourcesOf(t, sess)[0]["id"].(string)
	status, body := s.do("POST", "/v1/sessions/"+sid+"/resources/"+rid, map[string]any{"bogus": 1})
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error") // unknown key
	status, body = s.do("POST", "/v1/sessions/sesn_0000000000000000000000gk/resources/"+rid,
		map[string]any{"authorization_token": "t"})
	wantErr(t, status, body, http.StatusNotFound, "not_found_error") // unknown session
	status, body = s.do("POST", "/v1/sessions/"+sid+"/resources/sesrsc_0000000000000000000000gk",
		map[string]any{"authorization_token": "t"})
	wantErr(t, status, body, http.StatusNotFound, "not_found_error") // unknown resource

	// Malformed add body → 400.
	status, body = s.do("POST", "/v1/sessions/"+sid+"/resources", `{"type":`)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")

	// A well-formed but nonexistent session → 404 on every sub-resource route.
	ghost := "/v1/sessions/sesn_0000000000000000000000gk/resources"
	absent := []struct {
		method, path string
		body         any
	}{
		{"GET", ghost + "/" + rid, nil},
		{"DELETE", ghost + "/" + rid, nil},
		{"POST", ghost, map[string]any{"type": "file", "file_id": fileA}},
	}
	for _, c := range absent {
		status, body := s.do(c.method, c.path, c.body)
		wantErr(t, status, body, http.StatusNotFound, "not_found_error")
	}
}

// resourceMutationCount sums the session.resources counter for one outcome.
func resourceMutationCount(t *testing.T, rm metricdata.ResourceMetrics, outcome string) int64 {
	t.Helper()
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != api.MetricSessionResources {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want an int64 sum", api.MetricSessionResources, m.Data)
			}
			for _, p := range sum.DataPoints {
				if v, ok := p.Attributes.Value("outcome"); ok && v.AsString() == outcome {
					total += p.Value
				}
			}
		}
	}
	return total
}

// TestSessionResourceMetrics asserts the session.resources counter records the
// mutation chain by outcome.
func TestSessionResourceMetrics(t *testing.T) {
	collect := collectMetrics(t)
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	fileA := uploadOneFile(t, s, "a.txt")

	// One create attaching two resources → ok += 2.
	sess := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{
			map[string]any{"type": "file", "file_id": fileA, "mount_path": "/one"},
			map[string]any{"type": "file", "file_id": fileA, "mount_path": "/two"},
		},
	})
	sid := sess["id"].(string)
	rid := resourcesOf(t, sess)[0]["id"].(string)

	// One add → ok += 1.
	if status, _ := s.do("POST", "/v1/sessions/"+sid+"/resources",
		map[string]any{"type": "file", "file_id": fileA, "mount_path": "/three"}); status != http.StatusOK {
		t.Fatal("add failed")
	}
	// One delete → ok += 1.
	if status, _ := s.do("DELETE", "/v1/sessions/"+sid+"/resources/"+rid, nil); status != http.StatusOK {
		t.Fatal("delete failed")
	}
	// One add referencing a missing file → not_found += 1.
	if status, _ := s.do("POST", "/v1/sessions/"+sid+"/resources",
		map[string]any{"type": "file", "file_id": "file_0000000000000000000000gk"}); status != http.StatusNotFound {
		t.Fatal("add-missing did not 404")
	}

	rm := collect()
	if got := resourceMutationCount(t, rm, "ok"); got != 4 {
		t.Errorf("session.resources{outcome=ok} = %d, want 4", got)
	}
	if got := resourceMutationCount(t, rm, "not_found"); got != 1 {
		t.Errorf("session.resources{outcome=not_found} = %d, want 1", got)
	}
}
