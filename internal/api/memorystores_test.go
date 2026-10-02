package api_test

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The memory-store wire surface (plan 36 slice 1, #52): shapes per the SDK's
// BetaManagedAgentsMemoryStore (checked against anthropic-sdk-go v1.70.1 —
// betamemorystore.go BetaManagedAgentsMemoryStore), limits per the spec the SDK
// is generated from (name 1–255 and no control characters, description ≤ 1024,
// the shared metadata caps), checked against anthropic-sdk-go v1.70.1 — spec
// components.schemas.BetaManagedAgentsCreateMemoryStoreRequest.

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
	wantFields(t, body, "type", "id", "name", "created_at", "updated_at", "description", "metadata")
	if body["type"] != "memory_store" || body["name"] != "User preferences" {
		t.Fatalf("unexpected create body: %v", body)
	}
	// description renders "" when unset (never null), metadata {}, and
	// archived_at not at all until the store is archived, as the reference's
	// recorded create, get, list and update do (2026-09-02 free_batch1
	// `store.create`, `store.get`, `store.list.include_archived`,
	// `store.update.name+description`; #817).
	if body["description"] != "" {
		t.Errorf("description = %v, want the empty string", body["description"])
	}
	if md, ok := body["metadata"].(map[string]any); !ok || len(md) != 0 {
		t.Errorf("metadata = %v, want an empty object", body["metadata"])
	}
	wantNoFields(t, body, "archived_at")
	if body["updated_at"] != body["created_at"] {
		t.Errorf("updated_at = %v on create, want created_at %v", body["updated_at"], body["created_at"])
	}
	updatedAt := stamp(t, body["updated_at"])

	// Get returns the same shape; an unknown well-formed id 404s from the row
	// lookup, a wrong-prefix or malformed one from checkID before it.
	status, got := s.do(http.MethodGet, "/v1/memory_stores/"+id, nil)
	if status != http.StatusOK || got["id"] != id {
		t.Fatalf("get: status %d (%v)", status, got)
	}
	wantNoFields(t, got, "archived_at")
	// The NUL case proves the shape check runs: without it the byte reaches
	// Postgres, which refuses it as a 500 rather than a miss. Every spelling
	// takes the reference's words for this route (2026-09-05 batch8 idx 7
	// `item1.read.absent-store-control`; #540).
	token := strings.Repeat("a", len(strings.TrimPrefix(id, "memstore_")))
	for _, bad := range []struct{ path, id string }{
		{"memstore_" + token, "memstore_" + token},
		{"vlt_" + token, "vlt_" + token},
		{"memstore_missing00000000000", "memstore_missing00000000000"},
		{"memstore_" + token[1:] + "%00", "memstore_" + token[1:] + "\x00"},
	} {
		status, body := s.do(http.MethodGet, "/v1/memory_stores/"+bad.path, nil)
		wantErrMsg(t, status, body, http.StatusNotFound, "not_found_error", "memory store not found: "+bad.id)
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
	wantNoFields(t, body, "archived_at")
	// updated_at advances when name, description or metadata change ...
	if got := stamp(t, body["updated_at"]); !got.After(updatedAt) {
		t.Errorf("updated_at = %v after update, want later than %v", got, updatedAt)
	}
	updatedAt = stamp(t, body["updated_at"])

	// Archive returns the store with archived_at set and is idempotent; an
	// archived store still reads, but no longer updates.
	status, body = s.do(http.MethodPost, "/v1/memory_stores/"+id+"/archive", nil)
	if status != http.StatusOK || body["archived_at"] == nil {
		t.Fatalf("archive: status %d (%v)", status, body)
	}
	// ... and so does the first archive, to archived_at itself, as the
	// reference's does (#685) — though the spec's definition of the field names
	// only the three. A repeat archive moves neither.
	if got := stamp(t, body["updated_at"]); !got.Equal(stamp(t, body["archived_at"])) || !got.After(updatedAt) {
		t.Errorf("updated_at = %v after archive, want archived_at %v, later than %v", got, body["archived_at"], updatedAt)
	}
	first, firstUpdated := body["archived_at"], body["updated_at"]
	status, body = s.do(http.MethodPost, "/v1/memory_stores/"+id+"/archive", nil)
	if status != http.StatusOK || body["archived_at"] != first || body["updated_at"] != firstUpdated {
		t.Fatalf("archive not idempotent: status %d, archived_at %v vs %v, updated_at %v vs %v",
			status, body["archived_at"], first, body["updated_at"], firstUpdated)
	}
	// The reference's words (2026-09-02 free_batch1 idx 82
	// `store.update.archived.rename`; #540).
	status, body = s.do(http.MethodPost, "/v1/memory_stores/"+id, map[string]any{"name": "X"})
	wantErrMsg(t, status, body, http.StatusBadRequest, "invalid_request_error",
		"cannot modify archived resource: memory store "+id)
	if status, got := s.do(http.MethodGet, "/v1/memory_stores/"+id, nil); status != http.StatusOK || got["archived_at"] == nil {
		t.Fatalf("get after archive: status %d (%v) — retrieve includes archived stores", status, got)
	}

	// Delete is a hard delete with a tombstone; everything then 404s.
	status, body = s.do(http.MethodDelete, "/v1/memory_stores/"+id, nil)
	if status != http.StatusOK || body["type"] != "memory_store_deleted" || body["id"] != id {
		t.Fatalf("delete: status %d (%v)", status, body)
	}
	// The read takes the reference's words (2026-09-05 batch4 idx 14
	// `rec85.teardown.store.get.after-delete`; #540); the delete and the
	// archive, never recorded missing a store, keep ours.
	for _, call := range []struct{ method, path, msg string }{
		{http.MethodGet, "/v1/memory_stores/" + id, "memory store not found: " + id},
		{http.MethodDelete, "/v1/memory_stores/" + id, "memory store " + id + " not found"},
		{http.MethodPost, "/v1/memory_stores/" + id + "/archive", "memory store " + id + " not found"},
	} {
		status, got := s.do(call.method, call.path, nil)
		wantErrMsg(t, status, got, http.StatusNotFound, "not_found_error", call.msg)
	}
}

// stamp parses a rendered timestamp; comparing the strings would not do, since
// RFC 3339 rendering trims trailing zeros from the fraction.
func stamp(t *testing.T, v any) time.Time {
	t.Helper()
	s, _ := v.(string)
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("timestamp %v: %v", v, err)
	}
	return ts
}

func TestMemoryStoreValidation(t *testing.T) {
	s := newTestServer(t)

	// An absent name and an empty one take the reference's words (2026-09-02
	// free_batch1 idx 73 `store.create.no-name`, idx 72
	// `store.create.empty-name`; #540); a null one, never recorded, keeps ours.
	for _, tc := range []struct {
		body map[string]any
		msg  string
	}{
		{map[string]any{}, "name: Field required"},
		{map[string]any{"name": ""}, "name: minimum string length is 1"},
		{map[string]any{"name": nil}, "name is required"},
	} {
		status, resp := s.do(http.MethodPost, "/v1/memory_stores", tc.body)
		wantErrMsg(t, status, resp, http.StatusBadRequest, "invalid_request_error", tc.msg)
	}
	for name, body := range map[string]map[string]any{
		"name of 256 runes":    {"name": strings.Repeat("é", 256)},
		"control char in name": {"name": "bad\u0007name"},
		"long description":     {"name": "n", "description": strings.Repeat("d", 1025)},
		"unknown key":          {"name": "n", "surprise": true},
		"long metadata key":    {"name": "n", "metadata": map[string]string{strings.Repeat("k", 65): "v"}},
		"empty metadata key":   {"name": "n", "metadata": map[string]string{"": "v"}},
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
		"empty metadata key":   {"metadata": map[string]string{"": "v"}},
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

	// The empty-key bound is checked on what a caller sends, not on the stored
	// bag: a row that acquired an empty key before the bound existed stays
	// patchable, and {"": null} sheds the key.
	if _, err := s.pool.Exec(t.Context(),
		`UPDATE memory_stores SET metadata = '{"": "legacy", "k": "v"}' WHERE id = $1`, id); err != nil {
		t.Fatalf("plant a legacy empty key: %v", err)
	}
	status, body = s.do(http.MethodPost, "/v1/memory_stores/"+id, map[string]any{"metadata": map[string]any{"k": "w"}})
	if md, _ := body["metadata"].(map[string]any); status != http.StatusOK || md[""] != "legacy" || md["k"] != "w" {
		t.Fatalf("patch beside a legacy empty key: status %d (%v)", status, body)
	}
	status, body = s.do(http.MethodPost, "/v1/memory_stores/"+id, map[string]any{"metadata": map[string]any{"": nil}})
	if md, _ := body["metadata"].(map[string]any); status != http.StatusOK || len(md) != 1 || md["k"] != "w" {
		t.Fatalf("delete the legacy empty key: status %d (%v)", status, body)
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
	// meaning; the reference was recorded answering each (2026-09-03 batch1
	// `store.update.name-null`, `store.update.description-null`,
	// `store.update.metadata-null`). A null name is the recorded 400; an empty
	// one, unrecorded on update, takes the same words. A null
	// description clears like "". A null metadata bag is the recorded parse
	// refusal, while a null inside the bag still deletes its key (above).
	_, before := s.do(http.MethodGet, "/v1/memory_stores/"+id, nil)
	for _, tc := range []struct {
		patch map[string]any
		msg   string
	}{
		{map[string]any{"name": nil}, "name cannot be empty"},
		{map[string]any{"name": ""}, "name cannot be empty"},
		{map[string]any{"metadata": nil}, nullMetadataRefusal},
		{map[string]any{"name": "renamed", "metadata": nil}, nullMetadataRefusal},
	} {
		status, got := s.do(http.MethodPost, "/v1/memory_stores/"+id, tc.patch)
		wantInvalidRequest(t, fmt.Sprintf("update with %v", tc.patch), status, got, tc.msg)
	}
	if _, after := s.do(http.MethodGet, "/v1/memory_stores/"+id, nil); !reflect.DeepEqual(after, before) {
		t.Errorf("a refused null update changed the store: %v, was %v", after, before)
	}
	status, body = s.do(http.MethodPost, "/v1/memory_stores/"+id, map[string]any{"description": "again"})
	if status != http.StatusOK || body["description"] != "again" {
		t.Fatalf("set description: status %d (%v)", status, body)
	}
	status, body = s.do(http.MethodPost, "/v1/memory_stores/"+id, map[string]any{"description": nil})
	if status != http.StatusOK || body["description"] != "" {
		t.Fatalf("null description: status %d (%v), want it cleared", status, body)
	}
	// updated_at records when name, description or metadata last changed, so
	// a request that changes none of them — an empty bag, the stored values
	// sent back — leaves it where the last real change put it. (An empty body
	// is refused instead: TestMemoryStoreEmptyUpdate.)
	updatedAt := stamp(t, body["updated_at"])
	for _, patch := range []map[string]any{
		{"metadata": map[string]any{}},
		{"name": "notes", "description": "", "metadata": map[string]any{"team": "infra"}},
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
		if at := stamp(t, got["updated_at"]); !at.Equal(updatedAt) {
			t.Errorf("update with %v moved updated_at to %v, want %v unchanged", patch, at, updatedAt)
		}
	}
}

// TestMemoryStoreEmptyUpdate pins the reference's answer to an update that
// names none of the three fields (#817): the 2026-09-02 recording's
// `store.update.empty` sent `{}` and got a 400 `invalid_request_error` with the
// message below and no `details`. A key names its field whatever its value: the
// 2026-09-03 recording's `store.update.description-null` answered 200. Two
// readings are ours, registered in docs/DIVERGENCES.md: an empty metadata bag
// names metadata (TestMemoryStoreUpdateSemantics), and the body is judged before
// the store is looked up, so a missing or an archived store draws this 400, and
// the null-bag refusal, too.
func TestMemoryStoreEmptyUpdate(t *testing.T) {
	s := newTestServer(t)
	id := createMemoryStore(t, s, "notes")
	_, before := s.do(http.MethodGet, "/v1/memory_stores/"+id, nil)

	wantEmptyRefused := func(storeID string) {
		t.Helper()
		status, body := s.do(http.MethodPost, "/v1/memory_stores/"+storeID, map[string]any{})
		wantInvalidRequest(t, "empty update of "+storeID, status, body,
			"at least one of name, description, or metadata must be provided")
	}

	wantEmptyRefused(id)
	if _, after := s.do(http.MethodGet, "/v1/memory_stores/"+id, nil); !reflect.DeepEqual(after, before) {
		t.Errorf("a refused empty update changed the store: %v, was %v", after, before)
	}

	missing := "memstore_" + strings.Repeat("a", len(strings.TrimPrefix(id, "memstore_")))
	wantEmptyRefused(missing)
	archived := createMemoryStore(t, s, "archived")
	if status, body := s.do(http.MethodPost, "/v1/memory_stores/"+archived+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive: status %d (%v)", status, body)
	}
	wantEmptyRefused(archived)

	// A null metadata bag, the reference's parse refusal, is judged there too.
	for _, storeID := range []string{missing, archived} {
		status, body := s.do(http.MethodPost, "/v1/memory_stores/"+storeID, map[string]any{"metadata": nil})
		wantInvalidRequest(t, "null metadata on "+storeID, status, body, nullMetadataRefusal)
	}

	// A request with no body, or a JSON null for one, reads as {} and draws the
	// same 400; neither was recorded.
	for _, body := range []any{nil, "null"} {
		status, res := s.do(http.MethodPost, "/v1/memory_stores/"+id, body)
		wantInvalidRequest(t, fmt.Sprintf("update with body %v", body), status, res,
			"at least one of name, description, or metadata must be provided")
	}

	// Every shape refusal is a parse error, judged before the lookup as the
	// null bag is: a missing store answers it rather than its 404.
	const badBag = "metadata must be an object of string-or-null values"
	for _, tc := range []struct {
		body map[string]any
		msg  string
	}{
		{map[string]any{"name": 5}, "name must be a string"},
		{map[string]any{"description": true}, "description must be a string"},
		{map[string]any{"metadata": "x"}, badBag},
		{map[string]any{"metadata": map[string]any{"k": 1}}, badBag},
	} {
		status, res := s.do(http.MethodPost, "/v1/memory_stores/"+missing, tc.body)
		wantInvalidRequest(t, fmt.Sprintf("%v on a missing store", tc.body), status, res, tc.msg)
	}

	// A null name or description is a field's value, judged after the
	// lookup: a missing store is the 404 and an archived one the archived
	// refusal, never the null's own answer.
	for _, body := range []map[string]any{{"name": nil}, {"name": ""}, {"description": nil}} {
		if status, res := s.do(http.MethodPost, "/v1/memory_stores/"+missing, body); status != http.StatusNotFound {
			t.Errorf("%v on a missing store: status %d (%v), want 404", body, status, res)
		}
		status, res := s.do(http.MethodPost, "/v1/memory_stores/"+archived, body)
		wantInvalidRequest(t, fmt.Sprintf("%v on an archived store", body), status, res,
			"cannot modify archived resource: memory store "+archived)
	}
}

// nullMetadataRefusal is the reference's recorded answer to a null metadata
// bag on a store update (2026-09-03 batch1 `store.update.metadata-null`).
const nullMetadataRefusal = `Failed to parse request: params: field "metadata": null is not a valid value for a map field; omit the field to preserve, or set individual keys to null to delete them`

// wantInvalidRequest asserts a recorded 400 envelope exactly: an
// invalid_request_error carrying msg and no other member (no `details`), under
// a request_id.
func wantInvalidRequest(t *testing.T, label string, status int, body map[string]any, msg string) {
	t.Helper()
	want := map[string]any{"type": "invalid_request_error", "message": msg}
	if status != http.StatusBadRequest || body["type"] != "error" || !reflect.DeepEqual(body["error"], want) {
		t.Errorf("%s: status %d, body %v; want 400 with error %v and nothing else in it", label, status, body, want)
	}
	if _, ok := body["request_id"].(string); !ok {
		t.Errorf("%s: request_id = %v, want a string", label, body["request_id"])
	}
}

func TestMemoryStoreList(t *testing.T) {
	s := newTestServer(t)
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, createMemoryStore(t, s, fmt.Sprintf("store %d", i)))
	}
	stampCreatedAt(t, s, "memory_stores", ids...)
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
	wantNoFields(t, body, "next_page")
	status, body = s.do(http.MethodGet, "/v1/memory_stores?include_archived=true", nil)
	if n := len(listData(t, body)); status != http.StatusOK || n != 3 {
		t.Fatalf("include_archived: status %d, %d rows", status, n)
	}
	// Newest first.
	rows := listData(t, body)
	if rows[0]["id"] != ids[2] || rows[2]["id"] != ids[0] {
		t.Errorf("list order = %v %v %v, want newest first", rows[0]["id"], rows[1]["id"], rows[2]["id"])
	}
	// Only the archived item carries archived_at, as in the recorded
	// `store.list.include_archived` (2026-09-02 free_batch1; #817).
	wantNoFields(t, rows[0], "archived_at")
	wantNoFields(t, rows[1], "archived_at")
	if _, ok := rows[2]["archived_at"].(string); !ok {
		t.Errorf("archived item's archived_at = %v, want a timestamp", rows[2]["archived_at"])
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

	// The limit defaults to 20 and admits 100; and the cursor is the
	// (created_at, id) pair, not the timestamp alone: with every row on one
	// timestamp, a page walk still visits each store exactly once, id-descending.
	for i := 3; i < 21; i++ {
		ids = append(ids, createMemoryStore(t, s, fmt.Sprintf("store %d", i)))
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE memory_stores SET created_at = now()`); err != nil {
		t.Fatalf("tie every created_at: %v", err)
	}
	status, body = s.do(http.MethodGet, "/v1/memory_stores?include_archived=true", nil)
	if n := len(listData(t, body)); status != http.StatusOK || n != 20 {
		t.Fatalf("default limit: status %d, %d rows — want 20 rows and a cursor", status, n)
	}
	wantCursor(t, body)
	status, body = s.do(http.MethodGet, "/v1/memory_stores?include_archived=true&limit=100", nil)
	if n := len(listData(t, body)); status != http.StatusOK || n != 21 {
		t.Fatalf("limit=100: status %d, %d rows, want all 21", status, n)
	}
	seen = nil
	query = "/v1/memory_stores?include_archived=true&limit=5"
	for page := 0; page < 6; page++ {
		status, body = s.do(http.MethodGet, query, nil)
		if status != http.StatusOK {
			t.Fatalf("tied page %d: status %d (%v)", page, status, body)
		}
		for _, row := range listData(t, body) {
			seen = append(seen, row["id"].(string))
		}
		cursor := nextPage(t, body)
		if cursor == "" {
			break
		}
		query = "/v1/memory_stores?include_archived=true&limit=5&page=" + cursor
	}
	unique := map[string]bool{}
	for i, id := range seen {
		unique[id] = true
		if i > 0 && seen[i-1] <= id {
			t.Errorf("tied walk not id-descending at %d: %s then %s", i, seen[i-1], id)
		}
	}
	if len(seen) != 21 || len(unique) != 21 {
		t.Errorf("tied walk visited %d rows (%d unique), want 21 once each", len(seen), len(unique))
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

// A create's null metadata bag reads as {}, as every create here does; the
// update's recorded refusal of one was never sent to the reference's create
// (docs/DIVERGENCES.md, the memory-store update INFERRED entry, reading 4).
func TestMemoryStoreCreateReadsANullBagAsEmpty(t *testing.T) {
	s := newTestServer(t)
	status, body := s.do(http.MethodPost, "/v1/memory_stores", map[string]any{"name": "nullbag", "metadata": nil})
	if status != http.StatusOK {
		t.Fatalf("create with a null bag: status %d (%v)", status, body)
	}
	if md, ok := body["metadata"].(map[string]any); !ok || len(md) != 0 {
		t.Errorf("metadata = %v, want {}", body["metadata"])
	}
}
