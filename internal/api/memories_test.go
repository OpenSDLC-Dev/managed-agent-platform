package api_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The memory wire surface (plan 36 slice 2, #52): shapes per the pinned SDK's
// BetaManagedAgentsMemory and BetaManagedAgentsMemoryPrefix, rules per the
// OpenAPI spec the SDK is generated from (the occupancy 409, the no-op update,
// the precondition short-circuit, the tombstone) and the memory guide's caps.

func digest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func createMemory(t *testing.T, s *tserver, storeID, path, content string) map[string]any {
	t.Helper()
	status, body := s.do(http.MethodPost, "/v1/memory_stores/"+storeID+"/memories",
		map[string]any{"path": path, "content": content})
	if status != http.StatusOK {
		t.Fatalf("create memory %s: status %d (%v)", path, status, body)
	}
	return body
}

// memoryPaths lists a store's paths in the order the endpoint returned them,
// prefix rollups included (their path is the key too).
func memoryPaths(t *testing.T, body map[string]any) []string {
	t.Helper()
	var out []string
	for _, row := range listData(t, body) {
		path, _ := row["path"].(string)
		if row["type"] == "memory_prefix" {
			path = "prefix:" + path
		}
		out = append(out, path)
	}
	return out
}

func TestMemoryCRUD(t *testing.T) {
	s := newTestServer(t)
	store := createMemoryStore(t, s, "notes")

	body := createMemory(t, s, store, "/projects/foo/notes.md", "first")
	id, _ := body["id"].(string)
	if !strings.HasPrefix(id, "mem_") {
		t.Fatalf("id %q lacks the mem_ prefix", id)
	}
	wantFields(t, body, "type", "id", "memory_store_id", "path", "content",
		"content_size_bytes", "content_sha256", "memory_version_id", "created_at", "updated_at")
	if body["type"] != "memory" || body["memory_store_id"] != store || body["path"] != "/projects/foo/notes.md" {
		t.Fatalf("unexpected create body: %v", body)
	}
	// Create defaults to view=basic, so content is null — but the digest and
	// the size are "Always populated, regardless of view".
	if body["content"] != nil {
		t.Errorf("content = %v on a basic create, want null", body["content"])
	}
	if body["content_sha256"] != digest("first") || body["content_size_bytes"] != float64(5) {
		t.Errorf("digest/size = %v/%v, want %s/5", body["content_sha256"], body["content_size_bytes"], digest("first"))
	}
	version, _ := body["memory_version_id"].(string)
	if !strings.HasPrefix(version, "memver_") {
		t.Errorf("memory_version_id %q lacks the memver_ prefix", version)
	}
	if body["updated_at"] != body["created_at"] {
		t.Errorf("updated_at = %v on create, want created_at %v", body["updated_at"], body["created_at"])
	}
	createdAt := stamp(t, body["created_at"])

	// view=full on the create echo carries the content back.
	status, full := s.do(http.MethodPost, "/v1/memory_stores/"+store+"/memories?view=full",
		map[string]any{"path": "/second.md", "content": "two"})
	if status != http.StatusOK || full["content"] != "two" {
		t.Fatalf("create with view=full: status %d (%v)", status, full)
	}

	// Retrieve is the one endpoint that defaults to full.
	status, got := s.do(http.MethodGet, "/v1/memory_stores/"+store+"/memories/"+id, nil)
	if status != http.StatusOK || got["content"] != "first" {
		t.Fatalf("get: status %d (%v)", status, got)
	}
	status, got = s.do(http.MethodGet, "/v1/memory_stores/"+store+"/memories/"+id+"?view=basic", nil)
	if status != http.StatusOK || got["content"] != nil {
		t.Fatalf("get with view=basic: status %d (%v)", status, got)
	}
	for _, bad := range []string{"?view=raw", "?view=BASIC"} {
		status, resp := s.do(http.MethodGet, "/v1/memory_stores/"+store+"/memories/"+id+bad, nil)
		wantErr(t, status, resp, http.StatusBadRequest, "invalid_request_error")
	}

	// Update replaces the content: new digest, new size, new head version.
	status, updated := s.do(http.MethodPost, "/v1/memory_stores/"+store+"/memories/"+id+"?view=full",
		map[string]any{"content": "rewritten"})
	if status != http.StatusOK || updated["content"] != "rewritten" {
		t.Fatalf("update: status %d (%v)", status, updated)
	}
	if updated["content_sha256"] != digest("rewritten") || updated["content_size_bytes"] != float64(9) {
		t.Errorf("update digest/size = %v/%v", updated["content_sha256"], updated["content_size_bytes"])
	}
	if updated["memory_version_id"] == version {
		t.Errorf("memory_version_id did not move on update: %v", updated["memory_version_id"])
	}
	if at := stamp(t, updated["updated_at"]); !at.After(createdAt) {
		t.Errorf("updated_at = %v, want later than created_at %v", at, createdAt)
	}
	if updated["created_at"] != body["created_at"] {
		t.Errorf("created_at moved on update: %v", updated["created_at"])
	}
	// The id is "Stable across renames".
	status, renamed := s.do(http.MethodPost, "/v1/memory_stores/"+store+"/memories/"+id,
		map[string]any{"path": "/projects/foo/renamed.md"})
	if status != http.StatusOK || renamed["id"] != id || renamed["path"] != "/projects/foo/renamed.md" {
		t.Fatalf("rename: status %d (%v)", status, renamed)
	}

	// Delete is a tombstone; the memory then 404s on every route.
	status, tomb := s.do(http.MethodDelete, "/v1/memory_stores/"+store+"/memories/"+id, nil)
	if status != http.StatusOK || tomb["type"] != "memory_deleted" || tomb["id"] != id {
		t.Fatalf("delete: status %d (%v)", status, tomb)
	}
	for _, call := range []struct{ method, path string }{
		{http.MethodGet, "/v1/memory_stores/" + store + "/memories/" + id},
		{http.MethodPost, "/v1/memory_stores/" + store + "/memories/" + id},
		{http.MethodDelete, "/v1/memory_stores/" + store + "/memories/" + id},
	} {
		status, resp := s.do(call.method, call.path, map[string]any{"content": "x"})
		if status != http.StatusNotFound {
			t.Errorf("%s %s after delete: status %d (%v)", call.method, call.path, status, resp)
		}
	}
	// A memory addressed under the wrong store is not found either, and an
	// unknown store 404s before any collection under it answers.
	other := createMemoryStore(t, s, "elsewhere")
	kept := createMemory(t, s, store, "/kept.md", "k")["id"].(string)
	if status, resp := s.do(http.MethodGet, "/v1/memory_stores/"+other+"/memories/"+kept, nil); status != http.StatusNotFound {
		t.Errorf("cross-store get: status %d (%v)", status, resp)
	}
	// A store id that is unknown, wrong-prefixed or malformed 404s on every
	// collection under it, whichever method asks — the last spelling proving
	// checkID runs, since a NUL would otherwise reach Postgres as a 500 (#135).
	for _, missing := range []string{"memstore_missing00000000000", "vlt_" + strings.Repeat("a", 24), "memstore_%00"} {
		base := "/v1/memory_stores/" + missing
		for _, call := range []struct{ method, path string }{
			{http.MethodGet, base + "/memories"},
			{http.MethodPost, base + "/memories"},
			{http.MethodGet, base + "/memories/" + kept},
			{http.MethodPost, base + "/memories/" + kept},
			{http.MethodDelete, base + "/memories/" + kept},
			{http.MethodGet, base + "/memory_versions"},
			{http.MethodGet, base + "/memory_versions/memver_missing00000000000"},
			{http.MethodPost, base + "/memory_versions/memver_missing00000000000/redact"},
		} {
			status, resp := s.do(call.method, call.path, map[string]any{"path": "/x.md", "content": "x"})
			if status != http.StatusNotFound {
				t.Errorf("%s %s: status %d (%v)", call.method, call.path, status, resp)
			}
		}
	}
}

// The path table reaches the route, one documented rejection at a time. The
// rules themselves are memsync's, with their own table there; this pins that
// the handler runs them and answers 400.
func TestMemoryPathRules(t *testing.T) {
	s := newTestServer(t)
	store := createMemoryStore(t, s, "paths")

	for name, path := range map[string]any{
		"absent":              nil,
		"empty":               "",
		"no leading slash":    "notes.md",
		"the root alone":      "/",
		"an empty segment":    "/notes//today.md",
		"a trailing slash":    "/notes/",
		"a dot segment":       "/notes/./today.md",
		"a dot-dot segment":   "/notes/../today.md",
		"a control character": "/notes/\u0007bell.md",
		"a format character":  "/notes/rtl\u200e.md",
		"an NFD path":         "/cafe\u0301.md",
		"1025 bytes":          "/" + strings.Repeat("a", 1024),
		"the marker path":     "/.anthropic-memory-store",
		"not a string":        42,
	} {
		status, body := s.do(http.MethodPost, "/v1/memory_stores/"+store+"/memories",
			map[string]any{"path": path, "content": ""})
		if status != http.StatusBadRequest {
			t.Errorf("create with %s (%v): status %d (%v)", name, path, status, body)
		}
	}
	// The marker collides only at a store's root: a memory of that name in a
	// subdirectory is an ordinary memory.
	createMemory(t, s, store, "/x/.anthropic-memory-store", "not the marker")
	createMemory(t, s, store, "/"+strings.Repeat("a", 1023), "at the byte bound")
	createMemory(t, s, store, "/café.md", "NFC")

	// The same rules hold on a rename.
	id := createMemory(t, s, store, "/renamable.md", "x")["id"].(string)
	for _, path := range []string{"relative.md", "/a//b", "/..", "/.anthropic-memory-store"} {
		status, body := s.do(http.MethodPost, "/v1/memory_stores/"+store+"/memories/"+id,
			map[string]any{"path": path})
		if status != http.StatusBadRequest {
			t.Errorf("rename to %q: status %d (%v)", path, status, body)
		}
	}
}

func TestMemoryContentRules(t *testing.T) {
	s := newTestServer(t)
	store := createMemoryStore(t, s, "content")

	// content is required — "pass \"\" explicitly to create an empty memory" —
	// so an absent or null one is a 400 and an empty string is a memory.
	for name, body := range map[string]map[string]any{
		"absent":       {"path": "/a.md"},
		"null":         {"path": "/a.md", "content": nil},
		"not a string": {"path": "/a.md", "content": 7},
		"unknown key":  {"path": "/a.md", "content": "x", "surprise": true},
		"over 100 kB":  {"path": "/a.md", "content": strings.Repeat("x", 102401)},
	} {
		status, resp := s.do(http.MethodPost, "/v1/memory_stores/"+store+"/memories", body)
		if status != http.StatusBadRequest {
			t.Errorf("create with %s: status %d (%v)", name, status, resp)
		}
	}
	empty := createMemory(t, s, store, "/empty.md", "")
	if empty["content_size_bytes"] != float64(0) || empty["content_sha256"] != digest("") {
		t.Errorf("an empty memory: %v", empty)
	}
	big := createMemory(t, s, store, "/big.md", strings.Repeat("x", 102400))
	if big["content_size_bytes"] != float64(102400) {
		t.Errorf("100 kB exactly: %v", big["content_size_bytes"])
	}
	// And on update.
	status, resp := s.do(http.MethodPost, "/v1/memory_stores/"+store+"/memories/"+big["id"].(string),
		map[string]any{"content": strings.Repeat("x", 102401)})
	if status != http.StatusBadRequest {
		t.Errorf("update over 100 kB: status %d (%v)", status, resp)
	}
}

// A body carrying a byte that is not valid UTF-8 is refused whole, on create
// and on update alike. The bodies are sent raw because no marshaler would
// produce one: encoding/json rewrites such a byte to U+FFFD, and on the way IN
// it does the same — which is why the refusal has to happen at the decode
// rather than in memsync.ValidateContent, where the content would already be
// valid, already altered, and about to be stored.
func TestMemoryRejectsInvalidUTF8(t *testing.T) {
	s := newTestServer(t)
	store := createMemoryStore(t, s, "utf8")
	id := createMemory(t, s, store, "/notes.md", "clean")["id"].(string)

	status, body := s.do(http.MethodPost, "/v1/memory_stores/"+store+"/memories",
		"{\"path\":\"/bad.md\",\"content\":\"\xff\"}")
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	status, body = s.do(http.MethodPost, "/v1/memory_stores/"+store+"/memories/"+id,
		"{\"content\":\"\xff\"}")
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")

	// Neither write reached the database: no second memory row, and the
	// create's version is still the only one in the store's history.
	var memories int
	if err := s.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM memories WHERE memory_store_id = $1`, store).Scan(&memories); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	if n := countVersions(t, s, store); memories != 1 || n != 1 {
		t.Errorf("after the refused writes: %d memories, %d versions, want 1 and 1", memories, n)
	}
	// And the memory still serves the bytes it was created with.
	if status, got := s.do(http.MethodGet, "/v1/memory_stores/"+store+"/memories/"+id, nil); status != http.StatusOK ||
		got["content"] != "clean" {
		t.Fatalf("the memory after the refused update: status %d (%v)", status, got)
	}
}

// Occupancy in all four directions (decision 4): a path is taken when a memory
// sits at it, at an ancestor of it, or at a descendant of it — /a and /a/b
// cannot both be files — on create and on rename alike.
func TestMemoryPathOccupancy(t *testing.T) {
	s := newTestServer(t)

	conflict := func(t *testing.T, store, path string, body map[string]any, wantID, wantPath string) {
		t.Helper()
		status, resp := s.do(http.MethodPost, "/v1/memory_stores/"+store+path, body)
		wantErr(t, status, resp, http.StatusConflict, "memory_path_conflict_error")
		inner, _ := resp["error"].(map[string]any)
		if inner["conflicting_memory_id"] != wantID || inner["conflicting_path"] != wantPath {
			t.Errorf("conflict names %v at %v, want %s at %s",
				inner["conflicting_memory_id"], inner["conflicting_path"], wantID, wantPath)
		}
	}

	// A descendant blocks a create at its ancestor, and the reverse.
	store := createMemoryStore(t, s, "occupancy")
	deep := createMemory(t, s, store, "/a/b", "deep")["id"].(string)
	conflict(t, store, "/memories", map[string]any{"path": "/a", "content": "x"}, deep, "/a/b")
	conflict(t, store, "/memories", map[string]any{"path": "/a/b", "content": "x"}, deep, "/a/b")
	conflict(t, store, "/memories", map[string]any{"path": "/a/b/c", "content": "x"}, deep, "/a/b")

	// Rename onto each — spec-stated for an occupied path, inferred for an
	// ancestor or a descendant of one.
	mover := createMemory(t, s, store, "/mover.md", "m")["id"].(string)
	for _, path := range []string{"/a", "/a/b", "/a/b/c"} {
		conflict(t, store, "/memories/"+mover, map[string]any{"path": path}, deep, "/a/b")
	}
	// Renaming a memory onto its own path is the no-op, not a self-conflict.
	status, same := s.do(http.MethodPost, "/v1/memory_stores/"+store+"/memories/"+mover,
		map[string]any{"path": "/mover.md"})
	if status != http.StatusOK || same["path"] != "/mover.md" {
		t.Fatalf("rename onto itself: status %d (%v)", status, same)
	}

	// The occupancy predicate is a literal left(), never LIKE: `_` and `%` are
	// legal path bytes, and a pattern would make /acb/x occupy /a_b.
	literal := createMemoryStore(t, s, "metacharacters")
	createMemory(t, s, literal, "/acb/x", "under a c b")
	createMemory(t, s, literal, "/a_b", "an underscore")
	createMemory(t, s, literal, "/1000/x", "a thousand")
	createMemory(t, s, literal, "/100%", "a percent")
}

func TestMemoryUpdateSemantics(t *testing.T) {
	s := newTestServer(t)
	store := createMemoryStore(t, s, "updates")
	created := createMemory(t, s, store, "/notes.md", "original")
	id := created["id"].(string)
	head := created["memory_version_id"].(string)

	// Spec: "At least one of content or path must be provided."
	for name, body := range map[string]map[string]any{
		"an empty body":         {},
		"both null":             {"content": nil, "path": nil},
		"an unknown key":        {"surprise": true},
		"a bad precondition":    {"content": "x", "precondition": map[string]any{"type": "etag"}},
		"a string precondition": {"content": "x", "precondition": "sha"},
		"a typeless precondition": {"content": "x",
			"precondition": map[string]any{"content_sha256": digest("x")}},
		"a bodyless precondition": {"content": "x",
			"precondition": map[string]any{"type": "content_sha256"}},
		"an over-full precondition": {"content": "x", "precondition": map[string]any{
			"type": "content_sha256", "content_sha256": digest("x"), "surprise": 1}},
	} {
		status, resp := s.do(http.MethodPost, "/v1/memory_stores/"+store+"/memories/"+id, body)
		if status != http.StatusBadRequest {
			t.Errorf("update with %s: status %d (%v)", name, status, resp)
		}
	}

	// A stale precondition is a 409 of the memory surface's own type.
	status, resp := s.do(http.MethodPost, "/v1/memory_stores/"+store+"/memories/"+id, map[string]any{
		"content": "changed", "precondition": map[string]any{"type": "content_sha256", "content_sha256": digest("stale")},
	})
	wantErr(t, status, resp, http.StatusConflict, "memory_precondition_failed_error")

	// "If the precondition fails but the stored state already exactly matches
	// the requested content and path, the server returns 200 instead of 409."
	status, resp = s.do(http.MethodPost, "/v1/memory_stores/"+store+"/memories/"+id, map[string]any{
		"content": "original", "path": "/notes.md",
		"precondition": map[string]any{"type": "content_sha256", "content_sha256": digest("stale")},
	})
	if status != http.StatusOK || resp["memory_version_id"] != head {
		t.Fatalf("the precondition short-circuit: status %d (%v)", status, resp)
	}

	// "An update where every supplied field already matches the stored value is
	// a no-op: it returns 200 with the existing memory and writes no new
	// version" — so updated_at stays where it was, and the head does not move.
	updatedAt := stamp(t, created["updated_at"])
	for _, body := range []map[string]any{
		{"content": "original"},
		{"path": "/notes.md"},
		{"content": "original", "path": "/notes.md"},
	} {
		status, resp := s.do(http.MethodPost, "/v1/memory_stores/"+store+"/memories/"+id, body)
		if status != http.StatusOK {
			t.Fatalf("no-op update %v: status %d (%v)", body, status, resp)
		}
		if resp["memory_version_id"] != head {
			t.Errorf("no-op update %v moved the head to %v", body, resp["memory_version_id"])
		}
		if at := stamp(t, resp["updated_at"]); !at.Equal(updatedAt) {
			t.Errorf("no-op update %v moved updated_at to %v, want %v", body, at, updatedAt)
		}
	}
	if n := countVersions(t, s, store); n != 1 {
		t.Errorf("no-op updates wrote %d versions, want the create's 1", n)
	}

	// A matching precondition applies the write.
	status, resp = s.do(http.MethodPost, "/v1/memory_stores/"+store+"/memories/"+id, map[string]any{
		"content": "changed", "precondition": map[string]any{"type": "content_sha256", "content_sha256": digest("original")},
	})
	if status != http.StatusOK || resp["content_sha256"] != digest("changed") {
		t.Fatalf("update under a matching precondition: status %d (%v)", status, resp)
	}
	// A rename-only update keeps the content and still appends a version.
	status, resp = s.do(http.MethodPost, "/v1/memory_stores/"+store+"/memories/"+id+"?view=full",
		map[string]any{"path": "/moved.md"})
	if status != http.StatusOK || resp["content"] != "changed" || resp["path"] != "/moved.md" {
		t.Fatalf("rename-only: status %d (%v)", status, resp)
	}
	if n := countVersions(t, s, store); n != 3 {
		t.Errorf("versions = %d, want 3 (created, modified, modified)", n)
	}
}

func countVersions(t *testing.T, s *tserver, storeID string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM memory_versions WHERE memory_store_id = $1`, storeID).Scan(&n); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	return n
}

func TestMemoryDeletePrecondition(t *testing.T) {
	s := newTestServer(t)
	store := createMemoryStore(t, s, "deletes")
	id := createMemory(t, s, store, "/doomed.md", "bytes")["id"].(string)
	path := "/v1/memory_stores/" + store + "/memories/" + id

	// A byte Postgres cannot store is refused before it can bind into the
	// comparison as a 500 (#135), and a malformed id is the 404 an unknown one
	// gets. Both run before the transaction opens.
	if status, resp := s.do(http.MethodDelete, path+"?expected_content_sha256=%00", nil); status != http.StatusBadRequest {
		t.Errorf("a NUL in expected_content_sha256: status %d (%v)", status, resp)
	}
	for _, bad := range []string{"not-an-id", "mem_%00", "vlt_" + strings.Repeat("a", 24)} {
		bad := "/v1/memory_stores/" + store + "/memories/" + bad
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
			if status, resp := s.do(method, bad, map[string]any{"content": "x"}); status != http.StatusNotFound {
				t.Errorf("%s %s: status %d (%v)", method, bad, status, resp)
			}
		}
	}

	// The precondition rides the query string on delete, and a mismatch is the
	// same 409 an update's is (the reference worker accepts 409 or 412).
	status, resp := s.do(http.MethodDelete, path+"?expected_content_sha256="+digest("other"), nil)
	wantErr(t, status, resp, http.StatusConflict, "memory_precondition_failed_error")
	if status, got := s.do(http.MethodGet, path, nil); status != http.StatusOK || got["content"] != "bytes" {
		t.Fatalf("the refused delete changed the memory: status %d (%v)", status, got)
	}
	status, resp = s.do(http.MethodDelete, path+"?expected_content_sha256="+digest("bytes"), nil)
	if status != http.StatusOK || resp["type"] != "memory_deleted" {
		t.Fatalf("delete under a matching precondition: status %d (%v)", status, resp)
	}
	if status, resp = s.do(http.MethodDelete, path+"?expected_content_sha256="+digest("bytes"), nil); status != http.StatusNotFound {
		t.Fatalf("delete after delete: status %d (%v)", status, resp)
	}
}

func TestMemoryList(t *testing.T) {
	s := newTestServer(t)
	store := createMemoryStore(t, s, "listing")
	// Written out of order so the list's own ordering is what sorts them.
	for _, path := range []string{"/b.md", "/a/deep.md", "/c.md", "/a/also.md"} {
		createMemory(t, s, store, path, path)
	}

	status, body := s.do(http.MethodGet, "/v1/memory_stores/"+store+"/memories", nil)
	if status != http.StatusOK {
		t.Fatalf("list: status %d (%v)", status, body)
	}
	if got := memoryPaths(t, body); !equalStrings(got, []string{"/a/also.md", "/a/deep.md", "/b.md", "/c.md"}) {
		t.Errorf("list order = %v, want byte-wise path order", got)
	}
	if _, ok := body["next_page"]; !ok {
		t.Fatal("next_page must be present (null) in the page envelope")
	}
	// The list defaults to basic.
	for _, row := range listData(t, body) {
		if row["content"] != nil {
			t.Errorf("list defaults to basic, but %v carries content", row["path"])
		}
		if row["content_sha256"] == nil || row["content_size_bytes"] == nil {
			t.Errorf("the digest and size must survive view=basic: %v", row)
		}
	}
	status, body = s.do(http.MethodGet, "/v1/memory_stores/"+store+"/memories?view=full", nil)
	if status != http.StatusOK {
		t.Fatalf("list view=full: status %d (%v)", status, body)
	}
	for _, row := range listData(t, body) {
		if row["content"] != row["path"] {
			t.Errorf("view=full should carry content: %v", row)
		}
	}

	// Keyset paging walks each memory exactly once.
	var seen []string
	query := "/v1/memory_stores/" + store + "/memories?limit=1"
	for page := 0; page < 5; page++ {
		status, body = s.do(http.MethodGet, query, nil)
		if status != http.StatusOK {
			t.Fatalf("page %d: status %d (%v)", page, status, body)
		}
		seen = append(seen, memoryPaths(t, body)...)
		cursor := nextPage(t, body)
		if cursor == "" {
			break
		}
		query = "/v1/memory_stores/" + store + "/memories?limit=1&page=" + cursor
	}
	if !equalStrings(seen, []string{"/a/also.md", "/a/deep.md", "/b.md", "/c.md"}) {
		t.Errorf("paged walk = %v", seen)
	}

	// view=full caps the limit at 20 silently; view=basic does not.
	bulk := createMemoryStore(t, s, "bulk")
	for i := 0; i < 25; i++ {
		createMemory(t, s, bulk, fmt.Sprintf("/n%02d.md", i), "x")
	}
	status, body = s.do(http.MethodGet, "/v1/memory_stores/"+bulk+"/memories?view=full&limit=100", nil)
	if n := len(listData(t, body)); status != http.StatusOK || n != 20 || nextPage(t, body) == "" {
		t.Fatalf("view=full limit: status %d, %d rows, cursor %q — want 20 and a cursor", status, n, nextPage(t, body))
	}
	status, body = s.do(http.MethodGet, "/v1/memory_stores/"+bulk+"/memories?limit=100", nil)
	if n := len(listData(t, body)); status != http.StatusOK || n != 25 {
		t.Fatalf("view=basic limit=100: status %d, %d rows, want 25", status, n)
	}
	status, body = s.do(http.MethodGet, "/v1/memory_stores/"+bulk+"/memories", nil)
	if n := len(listData(t, body)); status != http.StatusOK || n != 20 {
		t.Fatalf("the default limit: status %d, %d rows, want 20", status, n)
	}

	for _, q := range []string{
		"limit=0", "limit=101", "limit=abc", "page=@@@", "view=deep",
		"depth=2", "depth=-1", "depth=all", "path_prefix=%00",
	} {
		status, body := s.do(http.MethodGet, "/v1/memory_stores/"+store+"/memories?"+q, nil)
		wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	}
	// A cursor from another list's grammar is not a memory cursor.
	status, body = s.do(http.MethodGet, "/v1/memory_stores?limit=1", nil)
	if status != http.StatusOK {
		t.Fatalf("store list: status %d (%v)", status, body)
	}
	if c := nextPage(t, body); c != "" {
		status, resp := s.do(http.MethodGet, "/v1/memory_stores/"+store+"/memories?page="+c, nil)
		wantErr(t, status, resp, http.StatusBadRequest, "invalid_request_error")
	}
}

// path_prefix is a literal prefix match, and depth=1 rolls everything below the
// next segment into one memory_prefix item that interleaves with memories in
// path order — including across a page boundary.
func TestMemoryListPrefixAndDepth(t *testing.T) {
	s := newTestServer(t)
	store := createMemoryStore(t, s, "tree")
	for _, path := range []string{
		"/100%/x", "/1000/x", "/a_b", "/acb/x",
		"/notes/a.md", "/notes/deep/b.md", "/notes/deep/c.md", "/notes-archive/old.md",
		"/z.md",
	} {
		createMemory(t, s, store, path, "x")
	}
	list := func(t *testing.T, q string) []string {
		t.Helper()
		status, body := s.do(http.MethodGet, "/v1/memory_stores/"+store+"/memories?limit=100&"+q, nil)
		if status != http.StatusOK {
			t.Fatalf("list %s: status %d (%v)", q, status, body)
		}
		return memoryPaths(t, body)
	}

	// Segment alignment: /notes/ must not reach /notes-archive/.
	if got := list(t, "path_prefix=%2Fnotes%2F"); !equalStrings(got,
		[]string{"/notes/a.md", "/notes/deep/b.md", "/notes/deep/c.md"}) {
		t.Errorf("path_prefix=/notes/ = %v", got)
	}
	// Literal metacharacters: neither `_` nor `%` is a wildcard.
	if got := list(t, "path_prefix="+url.QueryEscape("/a_b")); !equalStrings(got, []string{"/a_b"}) {
		t.Errorf("path_prefix=/a_b = %v", got)
	}
	if got := list(t, "path_prefix="+url.QueryEscape("/100%")); !equalStrings(got, []string{"/100%/x"}) {
		t.Errorf("path_prefix=/100%% = %v", got)
	}
	// An omitted prefix means "/", which is every path.
	if got := list(t, "depth=0"); len(got) != 9 {
		t.Errorf("depth=0 returned %d rows, want all 9: %v", len(got), got)
	}

	// depth=1 is ls: immediate children, everything deeper rolled up.
	if got := list(t, "depth=1"); !equalStrings(got, []string{
		"prefix:/100%/", "prefix:/1000/", "/a_b", "prefix:/acb/",
		"prefix:/notes-archive/", "prefix:/notes/", "/z.md",
	}) {
		t.Errorf("depth=1 at the root = %v", got)
	}
	if got := list(t, "depth=1&path_prefix=%2Fnotes%2F"); !equalStrings(got,
		[]string{"/notes/a.md", "prefix:/notes/deep/"}) {
		t.Errorf("depth=1 under /notes/ = %v", got)
	}
	// A prefix rollup can be drilled into by passing it straight back.
	if got := list(t, "depth=1&path_prefix=%2Fnotes%2Fdeep%2F"); !equalStrings(got,
		[]string{"/notes/deep/b.md", "/notes/deep/c.md"}) {
		t.Errorf("drilling into /notes/deep/ = %v", got)
	}

	// The rollup interleaves in path order across a page boundary: seven items
	// at depth=1, walked two at a time, must come back in the same order and
	// with no item repeated or skipped.
	var seen []string
	query := "/v1/memory_stores/" + store + "/memories?depth=1&limit=2"
	for page := 0; page < 5; page++ {
		status, body := s.do(http.MethodGet, query, nil)
		if status != http.StatusOK {
			t.Fatalf("depth=1 page %d: status %d (%v)", page, status, body)
		}
		seen = append(seen, memoryPaths(t, body)...)
		cursor := nextPage(t, body)
		if cursor == "" {
			break
		}
		query = "/v1/memory_stores/" + store + "/memories?depth=1&limit=2&page=" + cursor
	}
	if !equalStrings(seen, []string{
		"prefix:/100%/", "prefix:/1000/", "/a_b", "prefix:/acb/",
		"prefix:/notes-archive/", "prefix:/notes/", "/z.md",
	}) {
		t.Errorf("the paged depth=1 walk = %v", seen)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// The documented per-store cap: at 2,000 memories "writes to new memories fail
// … Existing memories remain readable and editable". The 2,000 rows are
// planted in SQL — 2,000 round trips through the API would be the same
// assertion at a hundred times the cost.
func TestMemoryStoreCapacity(t *testing.T) {
	s := newTestServer(t)
	store := createMemoryStore(t, s, "full")
	if _, err := s.pool.Exec(t.Context(),
		`INSERT INTO memories (id, memory_store_id, path, content, content_sha256,
		                       content_size_bytes, memory_version_id)
		 SELECT 'mem_' || lpad(i::text, 24, '0'), $1, '/bulk/' || i, '', $2, 0,
		        'memver_' || lpad(i::text, 24, '0')
		   FROM generate_series(1, 2000) i`, store, digest("")); err != nil {
		t.Fatalf("plant 2000 memories: %v", err)
	}
	status, body := s.do(http.MethodPost, "/v1/memory_stores/"+store+"/memories",
		map[string]any{"path": "/one-more.md", "content": "x"})
	if status != http.StatusBadRequest {
		t.Fatalf("the 2001st memory: status %d (%v)", status, body)
	}
	if msg, _ := body["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "2000 memories") {
		t.Errorf("cap message = %q, want it to name the cap", msg)
	}
	// The cap is on new memories only.
	status, body = s.do(http.MethodPost,
		"/v1/memory_stores/"+store+"/memories/mem_"+strings.Repeat("0", 23)+"1",
		map[string]any{"content": "still editable"})
	if status != http.StatusOK {
		t.Fatalf("editing an existing memory at the cap: status %d (%v)", status, body)
	}
}

// An archived store is read-only (decision 3): its memories still read, and
// every write answers the store's own 400.
func TestMemoryWritesRefusedOnAnArchivedStore(t *testing.T) {
	s := newTestServer(t)
	store := createMemoryStore(t, s, "archived")
	id := createMemory(t, s, store, "/kept.md", "bytes")["id"].(string)
	if status, body := s.do(http.MethodPost, "/v1/memory_stores/"+store+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive: status %d (%v)", status, body)
	}

	for _, call := range []struct {
		method, path string
		body         map[string]any
	}{
		{http.MethodPost, "/v1/memory_stores/" + store + "/memories", map[string]any{"path": "/new.md", "content": "x"}},
		{http.MethodPost, "/v1/memory_stores/" + store + "/memories/" + id, map[string]any{"content": "x"}},
		{http.MethodDelete, "/v1/memory_stores/" + store + "/memories/" + id, nil},
	} {
		status, body := s.do(call.method, call.path, call.body)
		if status != http.StatusBadRequest {
			t.Errorf("%s %s on an archived store: status %d (%v)", call.method, call.path, status, body)
		}
		if msg, _ := body["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "is archived") {
			t.Errorf("%s %s message = %q", call.method, call.path, msg)
		}
	}
	if status, body := s.do(http.MethodGet, "/v1/memory_stores/"+store+"/memories/"+id, nil); status != http.StatusOK || body["content"] != "bytes" {
		t.Fatalf("read on an archived store: status %d (%v)", status, body)
	}
	if status, body := s.do(http.MethodGet, "/v1/memory_stores/"+store+"/memories", nil); status != http.StatusOK || len(listData(t, body)) != 1 {
		t.Fatalf("list on an archived store: status %d (%v)", status, body)
	}
}

// "The store and all its memories and versions are no longer retrievable" — by
// the ON DELETE CASCADE 0029 gives both tables, so the tombstone is one
// statement rather than a containment scan.
func TestMemoryStoreDeleteCascades(t *testing.T) {
	s := newTestServer(t)
	store := createMemoryStore(t, s, "doomed")
	id := createMemory(t, s, store, "/a.md", "one")["id"].(string)
	if status, body := s.do(http.MethodPost, "/v1/memory_stores/"+store+"/memories/"+id,
		map[string]any{"content": "two"}); status != http.StatusOK {
		t.Fatalf("update: status %d (%v)", status, body)
	}
	if n := countVersions(t, s, store); n != 2 {
		t.Fatalf("versions before delete = %d, want 2", n)
	}
	if status, body := s.do(http.MethodDelete, "/v1/memory_stores/"+store, nil); status != http.StatusOK {
		t.Fatalf("delete store: status %d (%v)", status, body)
	}
	var memories int
	if err := s.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM memories WHERE memory_store_id = $1`, store).Scan(&memories); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	if memories != 0 || countVersions(t, s, store) != 0 {
		t.Errorf("after the store's delete: %d memories, %d versions", memories, countVersions(t, s, store))
	}
	for _, path := range []string{"/memories", "/memories/" + id, "/memory_versions"} {
		if status, body := s.do(http.MethodGet, "/v1/memory_stores/"+store+path, nil); status != http.StatusNotFound {
			t.Errorf("GET %s after the store's delete: status %d (%v)", path, status, body)
		}
	}
}

func TestMemoryMethodNotAllowed(t *testing.T) {
	s := newTestServer(t)
	store := createMemoryStore(t, s, "fallbacks")
	id := createMemory(t, s, store, "/a.md", "x")["id"].(string)
	base := "/v1/memory_stores/" + store

	for _, call := range []struct{ method, path string }{
		{http.MethodPut, base + "/memories"},
		{http.MethodDelete, base + "/memories"},
		{http.MethodPut, base + "/memories/" + id},
		{http.MethodPost, base + "/memory_versions"},
		{http.MethodDelete, base + "/memory_versions"},
		{http.MethodPost, base + "/memory_versions/memver_nonexistent"},
		{http.MethodDelete, base + "/memory_versions/memver_nonexistent"},
		{http.MethodGet, base + "/memory_versions/memver_nonexistent/redact"},
	} {
		status, body := s.do(call.method, call.path, nil)
		wantErr(t, status, body, http.StatusMethodNotAllowed, "invalid_request_error")
	}
}

// The memories list must order by bytes on every database this platform runs
// against, and no test here can prove that: the suite's postgres:16-alpine is
// musl, which sorts en_US.utf8 as bytes anyway, so a page reordered on Cloud
// SQL's glibc would still come back correct here. Two things ARE checkable, and
// together they are why listMemories can rely on a bare `ORDER BY key`. The
// column's own collation is "C" — the migration says so, and this asks the
// database rather than the file. And an explicitly collated expression keeps
// that collation out through a subquery, which is the derivation rule the list
// query leans on when it collates the rolled key once, inside.
func TestMemoryPathsSortUnderTheCCollation(t *testing.T) {
	s := newTestServer(t)
	store := createMemoryStore(t, s, "collation")
	createMemory(t, s, store, "/B.md", "upper")
	createMemory(t, s, store, "/a.md", "lower")

	var column string
	if err := s.pool.QueryRow(t.Context(),
		`SELECT pg_collation_for(path) FROM memories LIMIT 1`).Scan(&column); err != nil {
		t.Fatalf("read the column's collation: %v", err)
	}
	var key string
	if err := s.pool.QueryRow(t.Context(),
		`SELECT pg_collation_for(key) FROM (
		   SELECT (CASE WHEN false THEN '' ELSE path END) COLLATE "C" AS key FROM memories
		 ) rolled LIMIT 1`).Scan(&key); err != nil {
		t.Fatalf("read the rolled key's collation: %v", err)
	}
	if column != `"C"` || key != `"C"` {
		t.Errorf(`collations: path %s, the rolled key %s — want "C" for both`, column, key)
	}

	// And on this database that is the order the endpoint serves: uppercase
	// before lowercase, which is byte order and not en_US's.
	status, body := s.do(http.MethodGet, "/v1/memory_stores/"+store+"/memories", nil)
	if status != http.StatusOK {
		t.Fatalf("list: status %d (%v)", status, body)
	}
	if got := memoryPaths(t, body); !equalStrings(got, []string{"/B.md", "/a.md"}) {
		t.Errorf("list order = %v, want /B.md before /a.md", got)
	}
}
