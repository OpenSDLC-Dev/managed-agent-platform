package api_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The memory-version wire surface (plan 36 slice 2, #52): one immutable
// attributed row per non-no-op mutation, per the pinned SDK's
// BetaManagedAgentsMemoryVersion, with the nulls the spec states for a
// `deleted` operation and for redaction.

func listVersions(t *testing.T, s *tserver, storeID, query string) []map[string]any {
	t.Helper()
	path := "/v1/memory_stores/" + storeID + "/memory_versions"
	if query != "" {
		path += "?" + query
	}
	status, body := s.do(http.MethodGet, path, nil)
	if status != http.StatusOK {
		t.Fatalf("list versions %q: status %d (%v)", query, status, body)
	}
	return listData(t, body)
}

func TestMemoryVersionsPerOperation(t *testing.T) {
	s := newTestServer(t)
	store := createMemoryStore(t, s, "history")
	created := createMemory(t, s, store, "/notes.md", "one")
	id := created["id"].(string)

	// Every non-no-op mutation appends exactly one row.
	if status, body := s.do(http.MethodPost, "/v1/memory_stores/"+store+"/memories/"+id,
		map[string]any{"content": "two"}); status != http.StatusOK {
		t.Fatalf("update: status %d (%v)", status, body)
	}
	if status, body := s.do(http.MethodPost, "/v1/memory_stores/"+store+"/memories/"+id,
		map[string]any{"path": "/renamed.md"}); status != http.StatusOK {
		t.Fatalf("rename: status %d (%v)", status, body)
	}
	if status, body := s.do(http.MethodDelete, "/v1/memory_stores/"+store+"/memories/"+id, nil); status != http.StatusOK {
		t.Fatalf("delete: status %d (%v)", status, body)
	}

	rows := listVersions(t, s, store, "view=full")
	if len(rows) != 4 {
		t.Fatalf("versions = %d, want 4 (created, modified, modified, deleted): %v", len(rows), rows)
	}
	// Newest first, with id as the tiebreak.
	wantOps := []string{"deleted", "modified", "modified", "created"}
	for i, row := range rows {
		if row["operation"] != wantOps[i] {
			t.Errorf("version %d operation = %v, want %s", i, row["operation"], wantOps[i])
		}
		wantFields(t, row, "type", "id", "memory_store_id", "memory_id", "operation",
			"path", "content", "content_size_bytes", "content_sha256", "created_by",
			"created_at", "redacted_at", "redacted_by")
		if row["type"] != "memory_version" || row["memory_id"] != id || row["memory_store_id"] != store {
			t.Errorf("version %d: %v", i, row)
		}
		if !strings.HasPrefix(row["id"].(string), "memver_") {
			t.Errorf("version id %v lacks the memver_ prefix", row["id"])
		}
		if row["redacted_at"] != nil || row["redacted_by"] != nil {
			t.Errorf("version %d is not redacted but says otherwise: %v", i, row)
		}
		// The API key that wrote it, under the reference's api_actor arm.
		actor, _ := row["created_by"].(map[string]any)
		if actor == nil || actor["type"] != "api_actor" || !strings.HasPrefix(actor["api_key_id"].(string), "apikey_") {
			t.Errorf("version %d created_by = %v, want an api_actor", i, row["created_by"])
		}
	}
	// "content … null when … operation is deleted", and the same for the
	// digest and the size — while the path is the one it had when it died.
	tombstone := rows[0]
	if tombstone["content"] != nil || tombstone["content_sha256"] != nil || tombstone["content_size_bytes"] != nil {
		t.Errorf("the deleted version carries content: %v", tombstone)
	}
	if tombstone["path"] != "/renamed.md" {
		t.Errorf("the deleted version's path = %v, want /renamed.md", tombstone["path"])
	}
	// The rename kept the content and appended `modified`.
	if rows[1]["content"] != "two" || rows[1]["path"] != "/renamed.md" {
		t.Errorf("the rename version = %v", rows[1])
	}
	if rows[2]["content"] != "two" || rows[2]["path"] != "/notes.md" {
		t.Errorf("the content version = %v", rows[2])
	}
	if rows[3]["content"] != "one" || rows[3]["content_sha256"] != digest("one") {
		t.Errorf("the create version = %v", rows[3])
	}

	// view: basic hides content, and retrieve defaults to full. The digest and
	// the size survive basic — a sync client diffs on them.
	basic := listVersions(t, s, store, "")
	for _, row := range basic {
		if row["content"] != nil {
			t.Errorf("the list defaults to basic: %v", row)
		}
	}
	if basic[3]["content_sha256"] != digest("one") || basic[3]["content_size_bytes"] != float64(3) {
		t.Errorf("digest/size must survive view=basic: %v", basic[3])
	}
	versionID := rows[3]["id"].(string)
	status, got := s.do(http.MethodGet, "/v1/memory_stores/"+store+"/memory_versions/"+versionID, nil)
	if status != http.StatusOK || got["content"] != "one" {
		t.Fatalf("get version: status %d (%v)", status, got)
	}
	status, got = s.do(http.MethodGet, "/v1/memory_stores/"+store+"/memory_versions/"+versionID+"?view=basic", nil)
	if status != http.StatusOK || got["content"] != nil {
		t.Fatalf("get version view=basic: status %d (%v)", status, got)
	}
	// The lineage of a deleted memory is still listable by its id.
	if rows := listVersions(t, s, store, "memory_id="+id); len(rows) != 4 {
		t.Errorf("the deleted memory's lineage = %d rows, want 4", len(rows))
	}
	// A wrong-prefix or unknown id 404s from the row lookup, a malformed one
	// from checkID before it — the NUL case proving checkID runs at all.
	for _, bad := range []string{
		"memstore_" + strings.Repeat("a", 24), "memver_missing00000000000",
		"not-an-id", "memver_%00",
	} {
		for _, call := range []struct{ method, path string }{
			{http.MethodGet, "/v1/memory_stores/" + store + "/memory_versions/" + bad},
			{http.MethodPost, "/v1/memory_stores/" + store + "/memory_versions/" + bad + "/redact"},
		} {
			if status, body := s.do(call.method, call.path, nil); status != http.StatusNotFound {
				t.Errorf("%s %s: status %d (%v)", call.method, call.path, status, body)
			}
		}
	}
}

func TestMemoryVersionList(t *testing.T) {
	s := newTestServer(t)
	store := createMemoryStore(t, s, "versions")
	first := createMemory(t, s, store, "/a.md", "a")["id"].(string)
	second := createMemory(t, s, store, "/b.md", "b")["id"].(string)
	if status, body := s.do(http.MethodDelete, "/v1/memory_stores/"+store+"/memories/"+second, nil); status != http.StatusOK {
		t.Fatalf("delete: status %d (%v)", status, body)
	}

	if rows := listVersions(t, s, store, "memory_id="+first); len(rows) != 1 || rows[0]["memory_id"] != first {
		t.Errorf("memory_id filter = %v", rows)
	}
	if rows := listVersions(t, s, store, "operation=deleted"); len(rows) != 1 || rows[0]["memory_id"] != second {
		t.Errorf("operation filter = %v", rows)
	}
	// The api_key_id filter reaches inside the actor object.
	var keyID string
	if err := s.pool.QueryRow(t.Context(), `SELECT id FROM api_keys`).Scan(&keyID); err != nil {
		t.Fatalf("read api key: %v", err)
	}
	if rows := listVersions(t, s, store, "api_key_id="+keyID); len(rows) != 3 {
		t.Errorf("api_key_id filter = %d rows, want all 3", len(rows))
	}
	if rows := listVersions(t, s, store, "api_key_id=apikey_nobody"); len(rows) != 0 {
		t.Errorf("an unmatched api_key_id filter = %v", rows)
	}
	// session_id reaches the same object, for the writes slice 4 will make.
	// Planted here rather than waited for: the filter is on the wire now.
	if _, err := s.pool.Exec(t.Context(),
		`UPDATE memory_versions SET created_by = '{"type":"session_actor","session_id":"sesn_synced0000000000000"}'
		  WHERE memory_id = $1`, first); err != nil {
		t.Fatalf("plant a session actor: %v", err)
	}
	rows := listVersions(t, s, store, "session_id=sesn_synced0000000000000")
	if len(rows) != 1 || rows[0]["memory_id"] != first {
		t.Fatalf("session_id filter = %v", rows)
	}
	if actor, _ := rows[0]["created_by"].(map[string]any); actor["type"] != "session_actor" {
		t.Errorf("the planted actor renders as %v", rows[0]["created_by"])
	}

	// Inclusive created_at bounds, at the exact boundary.
	all := listVersions(t, s, store, "")
	at, _ := all[1]["created_at"].(string)
	if got := listVersions(t, s, store, "created_at[gte]="+at); len(got) != 2 {
		t.Errorf("created_at[gte] at the boundary = %d rows, want 2", len(got))
	}
	if got := listVersions(t, s, store, "created_at[lte]="+at); len(got) != 2 {
		t.Errorf("created_at[lte] at the boundary = %d rows, want 2", len(got))
	}
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	if got := listVersions(t, s, store, "created_at[gte]="+future); len(got) != 0 {
		t.Errorf("a future bound = %v", got)
	}

	for _, q := range []string{
		"limit=0", "limit=101", "limit=abc", "page=@@@", "view=deep",
		"operation=renamed", "memory_id=nonsense", "session_id=nonsense",
	} {
		status, body := s.do(http.MethodGet, "/v1/memory_stores/"+store+"/memory_versions?"+q, nil)
		wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	}

	// The default limit is 20, and the walk visits each row exactly once.
	bulk := createMemoryStore(t, s, "many")
	id := createMemory(t, s, bulk, "/churn.md", "0")["id"].(string)
	for i := 1; i < 25; i++ {
		if status, body := s.do(http.MethodPost, "/v1/memory_stores/"+bulk+"/memories/"+id,
			map[string]any{"content": fmt.Sprintf("%d", i)}); status != http.StatusOK {
			t.Fatalf("churn %d: status %d (%v)", i, status, body)
		}
	}
	status, body := s.do(http.MethodGet, "/v1/memory_stores/"+bulk+"/memory_versions", nil)
	if n := len(listData(t, body)); status != http.StatusOK || n != 20 || nextPage(t, body) == "" {
		t.Fatalf("default limit: status %d, %d rows, cursor %q", status, n, nextPage(t, body))
	}
	seen := map[string]bool{}
	query := "/v1/memory_stores/" + bulk + "/memory_versions?limit=7"
	for page := 0; page < 5; page++ {
		status, body = s.do(http.MethodGet, query, nil)
		if status != http.StatusOK {
			t.Fatalf("page %d: status %d (%v)", page, status, body)
		}
		for _, row := range listData(t, body) {
			seen[row["id"].(string)] = true
		}
		cursor := nextPage(t, body)
		if cursor == "" {
			break
		}
		query = "/v1/memory_stores/" + bulk + "/memory_versions?limit=7&page=" + cursor
	}
	if len(seen) != 25 {
		t.Errorf("the paged walk visited %d versions, want 25", len(seen))
	}
}

// Redaction is the one in-place mutation a version row takes, and the only
// route in this family gated at admin.
func TestMemoryVersionRedact(t *testing.T) {
	s := newTestServer(t)
	store := createMemoryStore(t, s, "redaction")
	id := createMemory(t, s, store, "/secret.md", "a passphrase")["id"].(string)
	status, updated := s.do(http.MethodPost, "/v1/memory_stores/"+store+"/memories/"+id,
		map[string]any{"content": "rewritten"})
	if status != http.StatusOK {
		t.Fatalf("update: status %d (%v)", status, updated)
	}
	head := updated["memory_version_id"].(string)
	rows := listVersions(t, s, store, "view=full")
	older := rows[1]["id"].(string)

	// "A version that is the current head of a live memory cannot be redacted."
	status, body := s.do(http.MethodPost, "/v1/memory_stores/"+store+"/memory_versions/"+head+"/redact", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("redacting the head: status %d (%v)", status, body)
	}

	// The superseded one nulls all four fields, keeps its attribution, and
	// records who redacted it.
	status, body = s.do(http.MethodPost, "/v1/memory_stores/"+store+"/memory_versions/"+older+"/redact", nil)
	if status != http.StatusOK {
		t.Fatalf("redact: status %d (%v)", status, body)
	}
	for _, field := range []string{"content", "path", "content_sha256", "content_size_bytes"} {
		if body[field] != nil {
			t.Errorf("redacted %s = %v, want null", field, body[field])
		}
	}
	if body["redacted_at"] == nil {
		t.Fatalf("redacted_at is null after a redaction: %v", body)
	}
	if actor, _ := body["redacted_by"].(map[string]any); actor == nil || actor["type"] != "api_actor" {
		t.Errorf("redacted_by = %v, want an api_actor", body["redacted_by"])
	}
	if actor, _ := body["created_by"].(map[string]any); actor == nil || actor["type"] != "api_actor" {
		t.Errorf("created_by must survive redaction: %v", body["created_by"])
	}
	if body["operation"] != "created" {
		t.Errorf("operation must survive redaction: %v", body["operation"])
	}
	// "Retrieving a redacted version returns 200 … branch on redacted_at, not
	// HTTP status." And a second redaction is a no-op on the same row.
	status, got := s.do(http.MethodGet, "/v1/memory_stores/"+store+"/memory_versions/"+older, nil)
	if status != http.StatusOK || got["content"] != nil || got["redacted_at"] != body["redacted_at"] {
		t.Fatalf("get after redact: status %d (%v)", status, got)
	}
	status, again := s.do(http.MethodPost, "/v1/memory_stores/"+store+"/memory_versions/"+older+"/redact", nil)
	if status != http.StatusOK || again["redacted_at"] != body["redacted_at"] {
		t.Fatalf("redact twice: status %d (%v)", status, again)
	}
	// The head is untouched, so the memory still serves its bytes.
	if status, memory := s.do(http.MethodGet, "/v1/memory_stores/"+store+"/memories/"+id, nil); status != http.StatusOK || memory["content"] != "rewritten" {
		t.Fatalf("the memory after redacting an older version: status %d (%v)", status, memory)
	}

	if status, body := s.do(http.MethodPost,
		"/v1/memory_stores/"+store+"/memory_versions/memver_missing00000000000/redact", nil); status != http.StatusNotFound {
		t.Errorf("redacting an unknown version: status %d (%v)", status, body)
	}
}

// Decision 3's carve-out: an archived store refuses every write except a
// redaction, and on an archived store even the head is redactable — nothing can
// supersede it — which empties the memory so the bytes stop being served.
func TestMemoryVersionRedactOnAnArchivedStore(t *testing.T) {
	s := newTestServer(t)
	store := createMemoryStore(t, s, "archived-redaction")
	created := createMemory(t, s, store, "/secret.md", "a passphrase")
	id, head := created["id"].(string), created["memory_version_id"].(string)
	if status, body := s.do(http.MethodPost, "/v1/memory_stores/"+store+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive: status %d (%v)", status, body)
	}

	status, body := s.do(http.MethodPost, "/v1/memory_stores/"+store+"/memory_versions/"+head+"/redact", nil)
	if status != http.StatusOK {
		t.Fatalf("redacting the head of an archived store: status %d (%v)", status, body)
	}
	if body["content"] != nil || body["path"] != nil {
		t.Errorf("the redacted head still carries content: %v", body)
	}
	status, memory := s.do(http.MethodGet, "/v1/memory_stores/"+store+"/memories/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("get the memory: status %d (%v)", status, memory)
	}
	if memory["content"] != "" || memory["content_sha256"] != digest("") || memory["content_size_bytes"] != float64(0) {
		t.Errorf("the memory was not emptied by the head's redaction: %v", memory)
	}
	if memory["path"] != "/secret.md" {
		t.Errorf("the memory's own path must survive: %v", memory["path"])
	}
}

// Attribution on both auth lanes (decision 6): a machine key writes as
// api_actor carrying its own row id, a human as user_actor carrying their
// principal id — the reference's arms, with this platform's id shapes.
func TestMemoryVersionActorsOnBothLanes(t *testing.T) {
	s := newLaneServer(t)
	store := createMemoryStore(t, s.tserver, "actors")
	createMemory(t, s.tserver, store, "/machine.md", "written by a key")

	status, _, raw := laneRead(t, s.bearer(http.MethodPost, "/v1/memory_stores/"+store+"/memories",
		s.token("platform-devs"), map[string]any{"path": "/human.md", "content": "written by a person"}))
	if status != http.StatusOK {
		t.Fatalf("a developer creating a memory: status %d (%v)", status, laneMessage(t, raw))
	}

	var keyID, principalID string
	if err := s.pool.QueryRow(t.Context(), `SELECT id FROM api_keys`).Scan(&keyID); err != nil {
		t.Fatalf("read api key: %v", err)
	}
	if err := s.pool.QueryRow(t.Context(), `SELECT id FROM principals`).Scan(&principalID); err != nil {
		t.Fatalf("read principal: %v", err)
	}
	for _, want := range []struct{ path, actorType, field, id string }{
		{"/machine.md", "api_actor", "api_key_id", keyID},
		{"/human.md", "user_actor", "user_id", principalID},
	} {
		var rendered map[string]any
		for _, row := range listVersions(t, s.tserver, store, "") {
			if row["path"] == want.path {
				rendered, _ = row["created_by"].(map[string]any)
			}
		}
		if rendered == nil {
			t.Fatalf("no version for %s", want.path)
		}
		if rendered["type"] != want.actorType || rendered[want.field] != want.id {
			t.Errorf("%s created_by = %v, want %s %s=%s", want.path, rendered, want.actorType, want.field, want.id)
		}
	}
}
