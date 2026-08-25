package api_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The memory-store wire surface (plan 36 slice 1, #52): shapes per the pinned
// SDK's BetaManagedAgentsMemoryStore, limits per the OpenAPI spec the SDK is
// generated from (name 1–255 and no control characters, description ≤ 1024,
// the shared metadata caps).

func createMemoryStore(t *testing.T, s *tserver, name string) string {
	t.Helper()
	status, body := s.do(http.MethodPost, "/v1/memory_stores", map[string]any{"name": name})
	if status != http.StatusOK {
		t.Fatalf("create memory store: status %d (%v)", status, body)
	}
	return body["id"].(string)
}

func TestMemoryStoreCRUD(t *testing.T) {
	s := newTestServer(t)

	status, body := s.do(http.MethodPost, "/v1/memory_stores", map[string]any{"name": "User preferences"})
	if status != http.StatusOK {
		t.Fatalf("create: status %d (%v)", status, body)
	}
	id, _ := body["id"].(string)
	if !strings.HasPrefix(id, "memstore_") {
		t.Fatalf("id %q lacks the memstore_ prefix", id)
	}
	wantFields(t, body, "type", "id", "name", "created_at", "updated_at", "archived_at", "description", "metadata")
	if body["type"] != "memory_store" || body["name"] != "User preferences" {
		t.Fatalf("unexpected create body: %v", body)
	}
	// description renders "" when unset (never null), metadata {}, archived_at null.
	if body["description"] != "" {
		t.Errorf("description = %v, want the empty string", body["description"])
	}
	if md, ok := body["metadata"].(map[string]any); !ok || len(md) != 0 {
		t.Errorf("metadata = %v, want an empty object", body["metadata"])
	}
	if body["archived_at"] != nil {
		t.Errorf("archived_at should render null, got %v", body["archived_at"])
	}

	// Get returns the same shape; unknown and malformed ids 404 alike (checkID).
	if status, got := s.do(http.MethodGet, "/v1/memory_stores/"+id, nil); status != http.StatusOK || got["id"] != id {
		t.Fatalf("get: status %d (%v)", status, got)
	}
	if status, _ := s.do(http.MethodGet, "/v1/memory_stores/memstore_missing00000000000", nil); status != http.StatusNotFound {
		t.Fatalf("get missing: status %d", status)
	}
	if status, _ := s.do(http.MethodGet, "/v1/memory_stores/vlt_wrongprefix", nil); status != http.StatusNotFound {
		t.Fatalf("get with a wrong prefix: status %d", status)
	}

	// Update: name and description replace, metadata patches.
	status, body = s.do(http.MethodPost, "/v1/memory_stores/"+id, map[string]any{
		"name": "Renamed", "description": "what it holds",
		"metadata": map[string]any{"team": "infra"},
	})
	if status != http.StatusOK {
		t.Fatalf("update: status %d (%v)", status, body)
	}
	if body["name"] != "Renamed" || body["description"] != "what it holds" {
		t.Fatalf("update did not apply: %v", body)
	}
	if md := body["metadata"].(map[string]any); md["team"] != "infra" {
		t.Fatalf("metadata not round-tripped: %v", md)
	}

	// Archive returns the store with archived_at set and is idempotent; an
	// archived store still reads, but no longer updates.
	status, body = s.do(http.MethodPost, "/v1/memory_stores/"+id+"/archive", nil)
	if status != http.StatusOK || body["archived_at"] == nil {
		t.Fatalf("archive: status %d (%v)", status, body)
	}
	first := body["archived_at"]
	status, body = s.do(http.MethodPost, "/v1/memory_stores/"+id+"/archive", nil)
	if status != http.StatusOK || body["archived_at"] != first {
		t.Fatalf("archive not idempotent: status %d, %v vs %v", status, body["archived_at"], first)
	}
	status, body = s.do(http.MethodPost, "/v1/memory_stores/"+id, map[string]any{"name": "X"})
	if status != http.StatusBadRequest {
		t.Fatalf("update archived: status %d (%v)", status, body)
	}
	if msg, _ := body["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "is archived") {
		t.Errorf("update-archived message = %q, want it to say the store is archived", msg)
	}
	if status, got := s.do(http.MethodGet, "/v1/memory_stores/"+id, nil); status != http.StatusOK || got["archived_at"] == nil {
		t.Fatalf("get after archive: status %d (%v) — retrieve includes archived stores", status, got)
	}

	// Delete is a hard delete with a tombstone; everything then 404s.
	status, body = s.do(http.MethodDelete, "/v1/memory_stores/"+id, nil)
	if status != http.StatusOK || body["type"] != "memory_store_deleted" || body["id"] != id {
		t.Fatalf("delete: status %d (%v)", status, body)
	}
	for _, call := range []struct{ method, path string }{
		{http.MethodGet, "/v1/memory_stores/" + id},
		{http.MethodDelete, "/v1/memory_stores/" + id},
		{http.MethodPost, "/v1/memory_stores/" + id + "/archive"},
	} {
		if status, got := s.do(call.method, call.path, nil); status != http.StatusNotFound {
			t.Errorf("%s %s after delete: status %d (%v)", call.method, call.path, status, got)
		}
	}
}

func TestMemoryStoreValidation(t *testing.T) {
	s := newTestServer(t)

	for name, body := range map[string]map[string]any{
		"missing name":         {},
		"empty name":           {"name": ""},
		"name of 256 runes":    {"name": strings.Repeat("é", 256)},
		"control char in name": {"name": "bad\u0007name"},
		"long description":     {"name": "n", "description": strings.Repeat("d", 1025)},
		"unknown key":          {"name": "n", "surprise": true},
		"long metadata key":    {"name": "n", "metadata": map[string]string{strings.Repeat("k", 65): "v"}},
		"long metadata value":  {"name": "n", "metadata": map[string]string{"k": strings.Repeat("v", 513)}},
	} {
		if status, resp := s.do(http.MethodPost, "/v1/memory_stores", body); status != http.StatusBadRequest {
			t.Errorf("%s: status %d (%v)", name, status, resp)
		}
	}
	// The bounds are counted in runes, and their maxima are accepted.
	status, body := s.do(http.MethodPost, "/v1/memory_stores", map[string]any{
		"name": strings.Repeat("é", 255), "description": strings.Repeat("d", 1024),
	})
	if status != http.StatusOK {
		t.Fatalf("255-rune name with a 1024-rune description: status %d (%v)", status, body)
	}
	id := body["id"].(string)

	tooMany := map[string]string{}
	for i := 0; i < 17; i++ {
		tooMany[fmt.Sprintf("k%d", i)] = "v"
	}
	if status, resp := s.do(http.MethodPost, "/v1/memory_stores",
		map[string]any{"name": "n", "metadata": tooMany}); status != http.StatusBadRequest {
		t.Errorf("17 metadata pairs on create: status %d (%v)", status, resp)
	}

	// Every bound holds across an update patch too.
	for name, body := range map[string]map[string]any{
		"name of 256 runes":    {"name": strings.Repeat("é", 256)},
		"control char in name": {"name": "bad\u0007name"},
		"long description":     {"description": strings.Repeat("d", 1025)},
		"unknown key":          {"surprise": true},
		"long metadata key":    {"metadata": map[string]string{strings.Repeat("k", 65): "v"}},
		"long metadata value":  {"metadata": map[string]string{"k": strings.Repeat("v", 513)}},
	} {
		if status, resp := s.do(http.MethodPost, "/v1/memory_stores/"+id, body); status != http.StatusBadRequest {
			t.Errorf("update with %s: status %d (%v)", name, status, resp)
		}
	}
	sixteen := map[string]string{}
	for i := 0; i < 16; i++ {
		sixteen[fmt.Sprintf("k%d", i)] = "v"
	}
	if status, resp := s.do(http.MethodPost, "/v1/memory_stores/"+id,
		map[string]any{"metadata": sixteen}); status != http.StatusOK {
		t.Fatalf("16 pairs must be accepted: status %d (%v)", status, resp)
	}
	if status, resp := s.do(http.MethodPost, "/v1/memory_stores/"+id,
		map[string]any{"metadata": map[string]string{"one-more": "v"}}); status != http.StatusBadRequest {
		t.Errorf("a patch growing metadata past 16 pairs: status %d (%v)", status, resp)
	}
}

// TestMemoryStoreUpdateSemantics covers the three-way metadata patch and the
// documented "pass an empty string to clear it" on description.
func TestMemoryStoreUpdateSemantics(t *testing.T) {
	s := newTestServer(t)
	status, body := s.do(http.MethodPost, "/v1/memory_stores", map[string]any{
		"name": "notes", "description": "scratch",
		"metadata": map[string]string{"team": "infra", "tier": "gold"},
	})
	if status != http.StatusOK {
		t.Fatalf("create: status %d (%v)", status, body)
	}
	id := body["id"].(string)
	createdAt, _ := body["created_at"].(string)

	// A string upserts, a null deletes, an omitted key is kept.
	status, body = s.do(http.MethodPost, "/v1/memory_stores/"+id, map[string]any{
		"metadata": map[string]any{"tier": nil, "env": "prod"},
	})
	if status != http.StatusOK {
		t.Fatalf("metadata patch: status %d (%v)", status, body)
	}
	md := body["metadata"].(map[string]any)
	if _, ok := md["tier"]; ok {
		t.Errorf("null should delete the key: %v", md)
	}
	if md["env"] != "prod" {
		t.Errorf("string should upsert the key: %v", md)
	}
	if md["team"] != "infra" {
		t.Errorf("an omitted key should be kept: %v", md)
	}
	if body["name"] != "notes" || body["description"] != "scratch" {
		t.Errorf("a metadata-only patch changed name/description: %v", body)
	}
	if body["created_at"] != createdAt {
		t.Errorf("created_at moved on update: %v, want %v", body["created_at"], createdAt)
	}

	// An empty description clears it, and renders as "" rather than null.
	status, body = s.do(http.MethodPost, "/v1/memory_stores/"+id, map[string]any{"description": ""})
	if status != http.StatusOK || body["description"] != "" {
		t.Fatalf("clear description: status %d (%v)", status, body)
	}

	// The spec admits a null on all three top-level fields and gives it no
	// meaning; each takes its sibling resources' rule (updateAgent's): a name
	// cannot be cleared — null or "" is a 400 — a null description clears like
	// "", and a null metadata bag, like an empty body, preserves.
	for _, name := range []any{nil, ""} {
		status, got := s.do(http.MethodPost, "/v1/memory_stores/"+id, map[string]any{"name": name})
		if status != http.StatusBadRequest {
			t.Errorf("name %#v: status %d, want 400 (%v)", name, status, got)
		}
	}
	status, body = s.do(http.MethodPost, "/v1/memory_stores/"+id, map[string]any{"description": "again"})
	if status != http.StatusOK || body["description"] != "again" {
		t.Fatalf("set description: status %d (%v)", status, body)
	}
	for _, patch := range []map[string]any{
		{"description": nil, "metadata": nil},
		{},
	} {
		status, got := s.do(http.MethodPost, "/v1/memory_stores/"+id, patch)
		if status != http.StatusOK {
			t.Fatalf("update with %v: status %d (%v)", patch, status, got)
		}
		if got["name"] != "notes" || got["description"] != "" {
			t.Errorf("update with %v: name/description = %v/%v, want notes/\"\"", patch, got["name"], got["description"])
		}
		if md := got["metadata"].(map[string]any); md["team"] != "infra" || md["env"] != "prod" || len(md) != 2 {
			t.Errorf("update with %v changed metadata: %v", patch, md)
		}
	}
}

func TestMemoryStoreList(t *testing.T) {
	s := newTestServer(t)
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, createMemoryStore(t, s, fmt.Sprintf("store %d", i)))
	}
	if status, body := s.do(http.MethodPost, "/v1/memory_stores/"+ids[0]+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive: status %d (%v)", status, body)
	}

	// Archived stores are excluded by default and included on request.
	status, body := s.do(http.MethodGet, "/v1/memory_stores", nil)
	if status != http.StatusOK {
		t.Fatalf("list: status %d (%v)", status, body)
	}
	if n := len(listData(t, body)); n != 2 {
		t.Fatalf("default list returned %d stores, want the 2 active ones", n)
	}
	if _, ok := body["next_page"]; !ok {
		t.Fatal("next_page must be present (null) in the page envelope")
	}
	status, body = s.do(http.MethodGet, "/v1/memory_stores?include_archived=true", nil)
	if n := len(listData(t, body)); status != http.StatusOK || n != 3 {
		t.Fatalf("include_archived: status %d, %d rows", status, n)
	}
	// Newest first.
	rows := listData(t, body)
	if rows[0]["id"] != ids[2] || rows[2]["id"] != ids[0] {
		t.Errorf("list order = %v %v %v, want newest first", rows[0]["id"], rows[1]["id"], rows[2]["id"])
	}

	// Keyset paging walks every store exactly once, one page at a time.
	seen := []string{}
	query := "/v1/memory_stores?include_archived=true&limit=1"
	for page := 0; page < 4; page++ {
		status, body = s.do(http.MethodGet, query, nil)
		if status != http.StatusOK {
			t.Fatalf("page %d: status %d (%v)", page, status, body)
		}
		data := listData(t, body)
		if len(data) != 1 {
			t.Fatalf("page %d returned %d rows, want 1", page, len(data))
		}
		seen = append(seen, data[0]["id"].(string))
		cursor := nextPage(t, body)
		if cursor == "" {
			break
		}
		query = "/v1/memory_stores?include_archived=true&limit=1&page=" + cursor
	}
	if len(seen) != 3 || seen[0] != ids[2] || seen[1] != ids[1] || seen[2] != ids[0] {
		t.Errorf("paged walk = %v, want %v newest first", seen, []string{ids[2], ids[1], ids[0]})
	}

	// created_at bounds are inclusive at the exact boundary.
	status, body = s.do(http.MethodGet, "/v1/memory_stores/"+ids[1], nil)
	if status != http.StatusOK {
		t.Fatalf("get: status %d (%v)", status, body)
	}
	at, _ := body["created_at"].(string)
	status, body = s.do(http.MethodGet, "/v1/memory_stores?include_archived=true&created_at[gte]="+at, nil)
	if status != http.StatusOK {
		t.Fatalf("gte at the boundary: status %d (%v)", status, body)
	}
	if rows := listData(t, body); len(rows) != 2 || rows[1]["id"] != ids[1] {
		t.Errorf("created_at[gte] at the boundary = %v, want it to include the store itself", rows)
	}
	status, body = s.do(http.MethodGet, "/v1/memory_stores?include_archived=true&created_at[lte]="+at, nil)
	if status != http.StatusOK {
		t.Fatalf("lte at the boundary: status %d (%v)", status, body)
	}
	if rows := listData(t, body); len(rows) != 2 || rows[0]["id"] != ids[1] {
		t.Errorf("created_at[lte] at the boundary = %v, want it to include the store itself", rows)
	}
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	status, body = s.do(http.MethodGet, "/v1/memory_stores?created_at[gte]="+future, nil)
	if status != http.StatusOK || len(listData(t, body)) != 0 {
		t.Errorf("a future gte filter: status %d (%v)", status, body)
	}

	// Bad parameters are 400s, not silently-ignored filters.
	for _, q := range []string{
		"limit=0", "limit=101", "limit=abc", "page=@@@",
		"include_archived=maybe", "created_at[gte]=yesterday",
	} {
		status, body := s.do(http.MethodGet, "/v1/memory_stores?"+q, nil)
		wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	}
}

// TestMemoryStoreRecordsItsCreator pins the audit column on both auth lanes: a
// machine key records the apikey_ row id, a human the principal_ id. created_by
// is never on the wire — sessions.created_by's rule — so only the database
// answers, and a store that recorded nobody would look exactly like a working
// one until someone needed the audit trail.
func TestMemoryStoreRecordsItsCreator(t *testing.T) {
	s := newLaneServer(t)

	// The machine lane: the x-api-key's own row id.
	machineID := createMemoryStore(t, s.tserver, "machine-made")
	var createdBy *string
	if err := s.pool.QueryRow(t.Context(),
		`SELECT created_by FROM memory_stores WHERE id = $1`, machineID).Scan(&createdBy); err != nil {
		t.Fatalf("read created_by: %v", err)
	}
	var keyID string
	if err := s.pool.QueryRow(t.Context(), `SELECT id FROM api_keys`).Scan(&keyID); err != nil {
		t.Fatalf("read api key: %v", err)
	}
	if createdBy == nil || *createdBy != keyID {
		t.Errorf("created_by = %v on the machine lane, want the api key id %q", createdBy, keyID)
	}

	// The human lane: the principal id, not the api key's and not the subject.
	status, _, raw := laneRead(t, s.bearer(http.MethodPost, "/v1/memory_stores",
		s.token("platform-devs"), map[string]any{"name": "human-made"}))
	if status != http.StatusOK {
		t.Fatalf("a developer creating a store: status %d (%v)", status, laneMessage(t, raw))
	}
	var humanID string
	if err := s.pool.QueryRow(t.Context(),
		`SELECT id FROM memory_stores WHERE name = 'human-made'`).Scan(&humanID); err != nil {
		t.Fatalf("read the human-made store: %v", err)
	}
	if err := s.pool.QueryRow(t.Context(),
		`SELECT created_by FROM memory_stores WHERE id = $1`, humanID).Scan(&createdBy); err != nil {
		t.Fatalf("read created_by: %v", err)
	}
	var principalID string
	if err := s.pool.QueryRow(t.Context(), `SELECT id FROM principals`).Scan(&principalID); err != nil {
		t.Fatalf("read principal: %v", err)
	}
	if createdBy == nil || *createdBy != principalID {
		t.Errorf("created_by = %v on the identity lane, want the principal id %q", createdBy, principalID)
	}
}

func TestMemoryStoreMethodNotAllowed(t *testing.T) {
	s := newTestServer(t)
	id := createMemoryStore(t, s, "fallbacks")

	for _, call := range []struct{ method, path string }{
		{http.MethodPut, "/v1/memory_stores"},
		{http.MethodDelete, "/v1/memory_stores"},
		{http.MethodPut, "/v1/memory_stores/" + id},
		{http.MethodPut, "/v1/memory_stores/" + id + "/archive"},
		{http.MethodGet, "/v1/memory_stores/" + id + "/archive"},
	} {
		status, body := s.do(call.method, call.path, nil)
		wantErr(t, status, body, http.StatusMethodNotAllowed, "invalid_request_error")
	}
}
