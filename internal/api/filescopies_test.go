package api_test

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
)

// The per-resource session copy (#578): every path that mounts a file mints
// a files row of the session's own, with a fresh file_id that resources[]
// echoes, aliasing the upload's object rather than copying its bytes. The
// recordings cited per test are in managed-agents-wire-recordings; the
// registry entry is docs/DIVERGENCES.md's "Session file resources".

// fileAlias reads the two columns behind a copy that the wire never shows.
func fileAlias(t *testing.T, s *tserver, id string) (objectKey string, source *string) {
	t.Helper()
	if err := s.pool.QueryRow(context.Background(),
		`SELECT object_key, source_file_id FROM files WHERE id = $1`, id).Scan(&objectKey, &source); err != nil {
		t.Fatalf("read the files row %s: %v", id, err)
	}
	return objectKey, source
}

// objectPresent reports whether the store still holds key.
func objectPresent(t *testing.T, s *tserver, key string) bool {
	t.Helper()
	rc, _, err := s.blobs.Get(context.Background(), key)
	if err != nil {
		return false
	}
	_ = rc.Close()
	return true
}

// getFileOK GETs a file's metadata and asserts 200.
func getFileOK(t *testing.T, s *tserver, id string) map[string]any {
	t.Helper()
	status, obj := s.do(http.MethodGet, "/v1/files/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/files/%s: %d %v", id, status, obj)
	}
	return obj
}

// listFileIDs lists /v1/files with the given query and returns the ids.
func listFileIDs(t *testing.T, s *tserver, query string) []string {
	t.Helper()
	status, body := s.do(http.MethodGet, "/v1/files"+query, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/files%s: %d %v", query, status, body)
	}
	var ids []string
	for _, f := range listData(t, body) {
		ids = append(ids, f["id"].(string))
	}
	return ids
}

// wantCopyOf asserts the recorded shape of a session's copy (2026-09-02 batch2
// idx 396 and 408; console-141 ui-network idx 243, 274 and 292): the upload's
// filename, size and MIME type, a created_at of its own, never downloadable,
// scoped to the session — and, behind the wire, an alias of the upload's
// object rather than an object of its own.
func wantCopyOf(t *testing.T, s *tserver, copyID string, upload map[string]any, sessionID string) {
	t.Helper()
	uploadID := upload["id"].(string)
	if !strings.HasPrefix(copyID, "file_") || copyID == uploadID {
		t.Fatalf("resource file_id = %q, want a fresh file_ id, not the upload's %s", copyID, uploadID)
	}
	c := getFileOK(t, s, copyID)
	for _, k := range []string{"filename", "size_bytes", "mime_type", "expires_at"} {
		if c[k] != upload[k] {
			t.Errorf("copy %s = %v, want the upload's %v", k, c[k], upload[k])
		}
	}
	if c["downloadable"] != false {
		t.Errorf("copy downloadable = %v, want false", c["downloadable"])
	}
	scope, _ := c["scope"].(map[string]any)
	if scope["type"] != "session" || scope["id"] != sessionID {
		t.Errorf("copy scope = %v, want {type: session, id: %s}", c["scope"], sessionID)
	}
	if !wantTime(t, c, "created_at").After(wantTime(t, upload, "created_at")) {
		t.Errorf("copy created_at = %v, want its own, after the upload's %v", c["created_at"], upload["created_at"])
	}
	key, source := fileAlias(t, s, copyID)
	if key != blob.FilesKey(uploadID) || source == nil || *source != uploadID {
		t.Errorf("copy object_key = %q, source_file_id = %v; want the upload's key %q and id %s",
			key, source, blob.FilesKey(uploadID), uploadID)
	}
	if objectPresent(t, s, blob.FilesKey(copyID)) {
		t.Error("the copy has an object of its own; it should alias the upload's")
	}
}

// Session create mints one copy per file resource, two resources naming one
// upload getting two (2026-09-02 batch2 idx 388; 2026-09-03 batch2 idx 17;
// console-141 api-fixtures idx 13), and an omitted mount_path still names
// the id asked for.
func TestSessionCreateMintsACopyPerFileResource(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := readableFixture(t, s)
	text := "text/plain"
	upload := s.uploadFile(t, "rec78-notes.txt", &text, "notes for the recording")
	uploadID := upload["id"].(string)
	objects := s.blobs.Len()

	sess := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{
			map[string]any{"type": "file", "file_id": uploadID},
			map[string]any{"type": "file", "file_id": uploadID, "mount_path": "/mnt/session/uploads/custom/notes.txt"},
		},
	})
	sid := sess["id"].(string)
	res := resourcesOf(t, sess)
	if len(res) != 2 {
		t.Fatalf("resources = %v, want two", res)
	}
	first, second := res[0]["file_id"].(string), res[1]["file_id"].(string)
	if first == second {
		t.Errorf("both resources name %s; two resources get two copies", first)
	}
	wantCopyOf(t, s, first, upload, sid)
	wantCopyOf(t, s, second, upload, sid)
	if want := "/mnt/session/uploads/" + uploadID; res[0]["mount_path"] != want {
		t.Errorf("default mount_path = %v, want %s (the id asked for)", res[0]["mount_path"], want)
	}
	if got := s.blobs.Len(); got != objects {
		t.Errorf("the store holds %d objects after the mount, want %d: a copy costs no bytes", got, objects)
	}

	// The session's GET and the resources routes echo the copies too.
	if got := mountedFileID(t, createGetSession(t, s, sid)); got != first {
		t.Errorf("session GET file_id = %s, want %s", got, first)
	}

	// A copy can itself be mounted (ours, INFERRED): its copy names the same
	// object, the copy it was minted from as its source.
	other := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{map[string]any{"type": "file", "file_id": first}},
	})
	third := mountedFileID(t, other)
	key, source := fileAlias(t, s, third)
	if key != blob.FilesKey(uploadID) || source == nil || *source != first {
		t.Errorf("a copy of a copy: object_key %q, source %v; want %q and %s", key, source, blob.FilesKey(uploadID), first)
	}
}

// Resources add mints a copy as create does (console-141 ui-network idx 288),
// and the copy keeps the upload's filename where the mount renames it (idx 292).
func TestResourcesAddMintsACopy(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := readableFixture(t, s)
	text := "text/plain"
	upload := s.uploadFile(t, "rec141-input.txt", &text, "the recorded input")
	sid := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})["id"].(string)

	status, added := s.do(http.MethodPost, "/v1/sessions/"+sid+"/resources",
		map[string]any{"type": "file", "file_id": upload["id"], "mount_path": "/uploads/rec141-extra.txt"})
	if status != http.StatusOK {
		t.Fatalf("add: %d %v", status, added)
	}
	if added["mount_path"] != "/mnt/session/uploads/rec141-extra.txt" {
		t.Errorf("mount_path = %v, want /mnt/session/uploads/rec141-extra.txt", added["mount_path"])
	}
	wantCopyOf(t, s, added["file_id"].(string), upload, sid)
	if got := getFileOK(t, s, added["file_id"].(string))["filename"]; got != "rec141-input.txt" {
		t.Errorf("copy filename = %v, want the upload's rec141-input.txt, not the mount's name", got)
	}
}

// A deployment stores and echoes the upload's id (console-141 api-fixtures
// idx 15; ui-network idx 232 and 305), and each fire mints the fired session
// copies of its own: a manual run (ui-network idx 266, the session read at
// idx 268; api-fixtures idx 18) and, ours by the same path, a scheduled fire.
func TestDeploymentFiresMintCopiesAndTheDeploymentKeepsTheUpload(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	text := "text/plain"
	upload := s.uploadFile(t, "rec141-input.txt", &text, "the recorded input")
	uploadID := upload["id"].(string)
	body := scheduledBody(agentID, envID, "0 9 * * *", "UTC")
	body["resources"] = []any{map[string]any{"type": "file", "file_id": uploadID, "mount_path": "/uploads/rec141-input.txt"}}
	d := createDeployment(t, s, body)
	deplID := d["id"].(string)
	echoedFile := func(where string, d map[string]any) {
		t.Helper()
		rs, _ := d["resources"].([]any)
		if len(rs) != 1 || rs[0].(map[string]any)["file_id"] != uploadID {
			t.Errorf("%s: deployment resources = %v, want the upload %s", where, d["resources"], uploadID)
		}
	}
	echoedFile("create", d)

	run := runDeployment(t, s, deplID)
	manual, _ := run["session_id"].(string)
	if manual == "" {
		t.Fatalf("the run settled without a session: %v", run)
	}
	manualCopy := mountedFileID(t, createGetSession(t, s, manual))
	wantCopyOf(t, s, manualCopy, upload, manual)

	setResumedAt(t, s, deplID, time.Date(2026, 3, 12, 0, 0, 0, 0, time.UTC))
	if err := api.SchedulerTick(t.Context(), s.pool, time.Date(2026, 3, 12, 9, 0, 30, 0, time.UTC)); err != nil {
		t.Fatalf("tick: %v", err)
	}
	runs := scheduledRuns(t, s, deplID)
	if len(runs) != 1 || runs[0].sessionID == nil {
		t.Fatalf("scheduled runs = %+v, want one with a session", runs)
	}
	fired := *runs[0].sessionID
	firedCopy := mountedFileID(t, createGetSession(t, s, fired))
	wantCopyOf(t, s, firedCopy, upload, fired)
	if firedCopy == manualCopy {
		t.Error("the scheduled fire reused the manual run's copy; each fire mints its own")
	}

	_, got := s.do(http.MethodGet, "/v1/deployments/"+deplID, nil)
	echoedFile("after the fires", got)
}

// The unfiltered list leaves every session-scoped row out — the copies, as
// recorded (2026-09-02 batch2 idx 391, taken while they existed; console-141
// api-fixtures idx 30), and the harvested outputs, ours — and ?scope_id= lists
// both (ui-network idx 243, 274 and 292). ids[] meets the same default, ours.
func TestFileListLeavesSessionScopedRowsOut(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := readableFixture(t, s)
	uploadID := uploadOneFile(t, s, "input.txt")
	sess := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{map[string]any{"type": "file", "file_id": uploadID}},
	})
	sid, copyID := sess["id"].(string), mountedFileID(t, sess)
	outputID := seedDeliverable(t, s, sid, "report.md")

	if got := listFileIDs(t, s, ""); !slices.Equal(got, []string{uploadID}) {
		t.Errorf("unfiltered list = %v, want only the upload %s", got, uploadID)
	}
	if got := listFileIDs(t, s, "?scope_id="+sid); !slices.Equal(got, []string{outputID, copyID}) {
		t.Errorf("?scope_id list = %v, want the output and the copy, newest first [%s %s]", got, outputID, copyID)
	}
	if got := listFileIDs(t, s, "?ids[]="+copyID+"&ids[]="+uploadID); !slices.Equal(got, []string{uploadID}) {
		t.Errorf("ids[] without scope_id = %v, want only the unscoped %s", got, uploadID)
	}
	if got := listFileIDs(t, s, "?scope_id="+sid+"&ids[]="+copyID); !slices.Equal(got, []string{copyID}) {
		t.Errorf("ids[] under scope_id = %v, want [%s]", got, copyID)
	}
}

// Deleting the upload leaves its copies working (2026-09-02 batch2 idx 405
// to 408 and 413; console-141 api-fixtures idx 32 then 33–35): the copy still
// answers, the session's resources are unchanged, and its bytes are still
// served to the worker that mounts it, because the reference count keeps the
// shared object. Deleting the last copy owes it to the drain.
func TestDeletingTheUploadLeavesItsCopies(t *testing.T) {
	s := newTestServer(t)
	q := startSweeper(t, s, s.blobs)
	agentID, envID := selfHostedFixture(t, s)
	bearer := map[string]string{"Authorization": "Bearer " + issueKey(t, s.pool, envID, "copies")}
	oct := "application/octet-stream"
	uploadID := s.uploadFile(t, "notes.bin", &oct, "shared bytes")["id"].(string)
	key := blob.FilesKey(uploadID)
	sess := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{
			map[string]any{"type": "file", "file_id": uploadID},
			map[string]any{"type": "file", "file_id": uploadID, "mount_path": "/second.bin"},
		},
	})
	sid := sess["id"].(string)
	before := resourcesOf(t, sess)
	first, second := before[0]["file_id"].(string), before[1]["file_id"].(string)

	status, del := s.do(http.MethodDelete, "/v1/files/"+uploadID, nil)
	if status != http.StatusOK || del["type"] != "file_deleted" {
		t.Fatalf("DELETE the upload: %d %v", status, del)
	}
	if status, _ := s.do(http.MethodGet, "/v1/files/"+uploadID, nil); status != http.StatusNotFound {
		t.Errorf("GET the deleted upload = %d, want 404", status)
	}
	getFileOK(t, s, first)
	after := resourcesOf(t, createGetSession(t, s, sid))
	if len(after) != 2 || after[0]["file_id"] != first || after[1]["file_id"] != second {
		t.Errorf("resources after the upload's delete = %v, want them unchanged", after)
	}
	q.Wake()
	awaitDrained(t, s.pool, "after the upload's delete")
	if !objectPresent(t, s, key) {
		t.Fatal("deleting the upload removed the object its copies alias")
	}
	res := s.doRaw(http.MethodGet, "/v1/files/"+first+"/content", nil, bearer)
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || string(got) != "shared bytes" {
		t.Errorf("the worker's read of the copy = %d %q, want 200 and the upload's bytes", res.StatusCode, got)
	}

	// One copy left still names the object.
	if status, _ := s.do(http.MethodDelete, "/v1/files/"+first, nil); status != http.StatusOK {
		t.Fatalf("DELETE the first copy = %d, want 200", status)
	}
	if slices.Contains(pendingKeys(t, s.pool), key) || !objectPresent(t, s, key) {
		t.Fatal("deleting one copy owed the object the other still names")
	}
	// The last one goes, and the object with it.
	if status, _ := s.do(http.MethodDelete, "/v1/files/"+second, nil); status != http.StatusOK {
		t.Fatalf("DELETE the last copy = %d, want 200", status)
	}
	q.Wake()
	awaitDrained(t, s.pool, "after the last copy's delete")
	if objectPresent(t, s, key) {
		t.Error("the object outlived the last row naming it")
	}
}

// A session delete takes its copies with it (ours, INFERRED: the docs delete
// "files the session itself produced", and a copy is the session's without
// being produced by it) and counts the object like any other remover: kept
// while the upload or another session's copy names it, owed once none does.
func TestSessionDeleteTakesItsCopies(t *testing.T) {
	s := newTestServer(t)
	q := startSweeper(t, s, s.blobs)
	agentID, envID := readableFixture(t, s)
	uploadID := uploadOneFile(t, s, "shared.txt")
	key := blob.FilesKey(uploadID)
	mount := func() (string, string) {
		sess := createSession(t, s, map[string]any{
			"agent": agentID, "environment_id": envID,
			"resources": []any{map[string]any{"type": "file", "file_id": uploadID}},
		})
		return sess["id"].(string), mountedFileID(t, sess)
	}
	firstSession, firstCopy := mount()
	secondSession, secondCopy := mount()
	deleteSession := func(sid string) {
		t.Helper()
		if status, body := s.do(http.MethodDelete, "/v1/sessions/"+sid, nil); status != http.StatusOK {
			t.Fatalf("DELETE session %s: %d %v", sid, status, body)
		}
		q.Wake()
		awaitDrained(t, s.pool, "after deleting "+sid)
	}

	deleteSession(firstSession)
	if status, _ := s.do(http.MethodGet, "/v1/files/"+firstCopy, nil); status != http.StatusNotFound {
		t.Errorf("the deleted session's copy = %d, want 404", status)
	}
	getFileOK(t, s, uploadID)
	if !objectPresent(t, s, key) {
		t.Fatal("a session delete removed the object its upload still names")
	}

	if status, _ := s.do(http.MethodDelete, "/v1/files/"+uploadID, nil); status != http.StatusOK {
		t.Fatalf("DELETE the upload = %d", status)
	}
	q.Wake()
	awaitDrained(t, s.pool, "after the upload's delete")
	if !objectPresent(t, s, key) {
		t.Fatal("the upload's delete removed the object the second session's copy names")
	}
	getFileOK(t, s, secondCopy)

	deleteSession(secondSession)
	if objectPresent(t, s, key) {
		t.Error("the object outlived the session delete that took the last row naming it")
	}
}

// Archiving a session leaves its copies (console-141 api-fixtures idx 25;
// ui-network idx 301), and each still answers DELETE afterwards (api-fixtures
// idx 33–35). A copy is never downloadable, so the management lane refuses
// its content as it refuses any upload's (ours, from downloadable:false).
func TestArchivingASessionLeavesItsCopies(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := readableFixture(t, s)
	uploadID := uploadOneFile(t, s, "kept.txt")
	sess := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{map[string]any{"type": "file", "file_id": uploadID}},
	})
	sid, copyID := sess["id"].(string), mountedFileID(t, sess)

	status, body := s.do(http.MethodGet, "/v1/files/"+copyID+"/content", nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	wantDetails(t, body, map[string]any{"error_code": "file_not_downloadable"})

	status, archived := s.do(http.MethodPost, "/v1/sessions/"+sid+"/archive", nil)
	if status != http.StatusOK {
		t.Fatalf("archive: %d %v", status, archived)
	}
	if got := mountedFileID(t, archived); got != copyID {
		t.Errorf("archived session's resource = %s, want its copy %s", got, copyID)
	}
	getFileOK(t, s, copyID)
	if status, del := s.do(http.MethodDelete, "/v1/files/"+copyID, nil); status != http.StatusOK || del["type"] != "file_deleted" {
		t.Errorf("DELETE the archived session's copy = %d %v, want 200 file_deleted", status, del)
	}
}

// A copy expires with its upload (ours, INFERRED: no recording mounts an
// upload with a lifetime), and the retention sweep's removal of both owes
// their one object once.
func TestACopyExpiresWithItsUpload(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := readableFixture(t, s)
	ct, form := expiringFileForm(t, "short-lived.txt", "brief", "3600", false)
	status, upload := s.doForm(http.MethodPost, "/v1/files", ct, form)
	if status != http.StatusOK {
		t.Fatalf("upload: %d %v", status, upload)
	}
	uploadID := upload["id"].(string)
	sess := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{map[string]any{"type": "file", "file_id": uploadID}},
	})
	copyID := mountedFileID(t, sess)
	wantCopyOf(t, s, copyID, upload, sess["id"].(string))

	expireBy(t, s, uploadID, 31*24*time.Hour)
	expireBy(t, s, copyID, 31*24*time.Hour)
	if n, err := api.PurgeExpiredFilesForTest(t.Context(), s.pool, 30*24*time.Hour); err != nil || n != 2 {
		t.Fatalf("purge = %d, %v; want both rows", n, err)
	}
	if got := pendingKeys(t, s.pool); !slices.Equal(got, []string{blob.FilesKey(uploadID)}) {
		t.Errorf("queue = %v, want the shared object owed once", got)
	}
}

// A legacy session — written before #578, its resources[] naming the upload
// itself — keeps working: the worker lane serves the upload it mounts, from
// the key its id derives, which migration 0046 left it.
func TestALegacySessionKeepsMountingTheUpload(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := selfHostedFixture(t, s)
	bearer := map[string]string{"Authorization": "Bearer " + issueKey(t, s.pool, envID, "legacy")}
	oct := "application/octet-stream"
	uploadID := s.uploadFile(t, "legacy.bin", &oct, "legacy bytes")["id"].(string)
	sess := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{map[string]any{"type": "file", "file_id": uploadID}},
	})
	sid, copyID := sess["id"].(string), mountedFileID(t, sess)
	// What the previous build stored: no copy, the upload's id in resources[].
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE sessions SET resources = jsonb_set(resources, '{0,file_id}', to_jsonb($2::text)) WHERE id = $1`,
		sid, uploadID); err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := store.AllowFileCopyDeletes(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	if tag, err := tx.Exec(context.Background(), `DELETE FROM files WHERE id = $1`, copyID); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("delete the copy: %v rows, err %v", tag.RowsAffected(), err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := mountedFileID(t, createGetSession(t, s, sid)); got != uploadID {
		t.Fatalf("legacy resource file_id = %s, want the upload %s", got, uploadID)
	}
	res := s.doRaw(http.MethodGet, "/v1/files/"+uploadID+"/content", nil, bearer)
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || string(got) != "legacy bytes" {
		t.Errorf("the worker's read of a legacy mount = %d %q, want 200 and its bytes", res.StatusCode, got)
	}
}

// A file rubric may name a session's copy: its bytes are snapshotted from the
// object the copy aliases, not from a key derived from the copy's own id.
func TestAFileRubricMayNameACopy(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := readableFixture(t, s)
	text := "text/markdown"
	uploadID := s.uploadFile(t, "rubric.md", &text, "# Rubric\n- cite the input")["id"].(string)
	sess := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{map[string]any{"type": "file", "file_id": uploadID}},
	})
	echo := sendEvents(t, s, sess["id"].(string), defineOutcome("Answer it", map[string]any{
		"rubric": map[string]any{"type": "file", "file_id": mountedFileID(t, sess)},
	}))
	rc, _, err := s.blobs.Get(context.Background(), events.RubricSnapshotKey(domain.ID(echo[0]["outcome_id"].(string))))
	if err != nil {
		t.Fatalf("rubric snapshot missing: %v", err)
	}
	snap, _ := io.ReadAll(rc)
	rc.Close()
	if string(snap) != "# Rubric\n- cite the input" {
		t.Errorf("snapshot = %q, want the upload's bytes", snap)
	}
}
