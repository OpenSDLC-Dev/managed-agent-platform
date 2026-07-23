package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// seedFile plants a files-table row and its object, as the /v1/files upload
// would, so a session resource can reference it.
func (h *harness) seedFile(t *testing.T, id, content string) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO files (id, filename, mime_type, size_bytes, downloadable)
		 VALUES ($1, $1 || '.txt', 'text/plain', $2, false)
		 ON CONFLICT (id) DO NOTHING`, id, len(content)); err != nil {
		t.Fatalf("seed file row: %v", err)
	}
	if err := h.blobs.Put(ctx, blob.FilesKey(id),
		bytes.NewReader([]byte(content)), int64(len(content)), "text/plain"); err != nil {
		t.Fatalf("seed file object: %v", err)
	}
}

// refFiles points the session's resources[] at the given {file_id, mount_path}
// mounts, the file-variant shape the API stores.
func (h *harness) refFiles(t *testing.T, mounts ...[2]string) {
	t.Helper()
	entries := make([]map[string]string, len(mounts))
	for i, m := range mounts {
		entries[i] = map[string]string{"type": "file", "file_id": m[0], "mount_path": m[1]}
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET resources = $2::jsonb WHERE id = $1`,
		h.sid.String(), raw); err != nil {
		t.Fatalf("set session resources: %v", err)
	}
}

// TestMaterializesFiles: a mounted file's bytes land at its mount_path before
// the tools run, a dangling reference mounts nothing (tolerated), and the
// sentinel records the pass.
func TestMaterializesFiles(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedFile(t, "file_matone", "hello mount")
	present := "/mnt/session/uploads/file_matone"
	missing := "/mnt/session/uploads/file_gone"
	h.refFiles(t,
		[2]string{"file_matone", present},
		[2]string{"file_gone", missing}, // no row/object: a dangling mount
	)
	h.suspend(t, writeUse("out.txt", "x"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}
	if got := sb.files[present]; got != "hello mount" {
		t.Errorf("mounted content = %q, want %q", got, "hello mount")
	}
	if _, ok := sb.files[missing]; ok {
		t.Error("a dangling file reference must mount nothing")
	}
	if _, ok := sb.files[sandbox.DefaultWorkdir+"/"+filesSentinelName]; !ok {
		t.Error("no files sentinel written")
	}
}

// TestMaterializeOrphanBlobNotMounted: a file whose registry row is gone but
// whose object was left behind (api deleteFile orphans the blob best-effort) must
// NOT be mounted — the executor checks the files row like the brain, so a deleted
// file is the documented absent mount, not stale bytes from the orphan object.
// This fails if materializeFile trusts blob.Get alone (the orphan would mount).
func TestMaterializeOrphanBlobNotMounted(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	mount := "/mnt/session/uploads/file_orphan"
	// An orphan object with no files row — the row was deleted, the blob delete
	// did not land.
	if err := h.blobs.Put(context.Background(), blob.FilesKey("file_orphan"),
		bytes.NewReader([]byte("orphan bytes")), 12, "text/plain"); err != nil {
		t.Fatal(err)
	}
	h.refFiles(t, [2]string{"file_orphan", mount})

	h.suspend(t, writeUse("out.txt", "x"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}
	if got, ok := sb.files[mount]; ok {
		t.Errorf("an orphaned blob (no files row) must not mount, got %q", got)
	}
}

// TestFilesMaterializeIdempotent: re-provisioning a live sandbox whose sentinel
// matches the mounted set and whose mounts are present skips restreaming — the
// object can change underneath and the sandbox keeps the materialized bytes.
func TestFilesMaterializeIdempotent(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	mount := "/mnt/session/uploads/file_idem"
	h.seedFile(t, "file_idem", "v1")
	h.refFiles(t, [2]string{"file_idem", mount})

	h.suspend(t, writeUse("a.txt", "x"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("first step: %v", err)
	}
	if sb.files[mount] != "v1" {
		t.Fatalf("first materialization = %q, want v1", sb.files[mount])
	}

	// Rewrite the object, then run another tool_exec against the same sandbox.
	if err := h.blobs.Put(context.Background(), blob.FilesKey("file_idem"),
		bytes.NewReader([]byte("v2")), 2, "text/plain"); err != nil {
		t.Fatal(err)
	}
	h.suspend(t, writeUse("b.txt", "y"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("second step: %v", err)
	}
	if sb.files[mount] != "v1" {
		t.Errorf("after re-step mount = %q, want the unchanged v1 (sentinel skip)", sb.files[mount])
	}
}

// TestFilesRematerializeAfterMountDeleted: the sentinel skip is guarded by a
// test -e presence probe, so a mount an agent tool deleted is re-streamed on the
// next pass even though the sentinel still names the unchanged set. This fails if
// the `&& mountsPresent` conjunct is dropped (a stale sentinel would skip
// forever) — the property the always-true fake could not otherwise exercise.
func TestFilesRematerializeAfterMountDeleted(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	mount := "/mnt/session/uploads/file_del"
	h.seedFile(t, "file_del", "keep me")
	h.refFiles(t, [2]string{"file_del", mount})

	h.suspend(t, writeUse("a.txt", "x"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("first step: %v", err)
	}
	if sb.files[mount] != "keep me" {
		t.Fatalf("first materialization = %q, want %q", sb.files[mount], "keep me")
	}

	// An agent tool removes the mount. The sentinel still matches the set, but the
	// presence probe must catch the absence and force a re-stream.
	delete(sb.files, mount)
	h.suspend(t, writeUse("b.txt", "y"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("second step: %v", err)
	}
	if sb.files[mount] != "keep me" {
		t.Errorf("deleted mount not re-materialized: %q, want %q", sb.files[mount], "keep me")
	}
}

// TestFilesRematerializeWhenSetChanges: adding a mount to a live session
// re-materializes and the new mount lands. The added path is absent, so the
// presence probe alone would force this pass; the same-path reassignment case
// that isolates the sentinel-set comparison is
// TestFilesRematerializeWhenMountReassigned.
func TestFilesRematerializeWhenSetChanges(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	a := "/mnt/session/uploads/file_a"
	b := "/mnt/session/uploads/file_b"
	h.seedFile(t, "file_a", "aaa")
	h.seedFile(t, "file_b", "bbb")
	h.refFiles(t, [2]string{"file_a", a})

	h.suspend(t, writeUse("t1.txt", "x"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("first step: %v", err)
	}
	if sb.files[a] != "aaa" {
		t.Fatalf("first pass mount a = %q, want aaa", sb.files[a])
	}
	if _, ok := sb.files[b]; ok {
		t.Fatalf("mount b materialized before it was added")
	}

	// Add a second mount. The changed set must re-materialize and land b.
	h.refFiles(t, [2]string{"file_a", a}, [2]string{"file_b", b})
	h.suspend(t, writeUse("t2.txt", "y"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("second step: %v", err)
	}
	if sb.files[b] != "bbb" {
		t.Errorf("added mount not materialized: %q, want bbb", sb.files[b])
	}
}

// TestFilesRematerializeWhenMountReassigned: pointing an existing mount_path at a
// different file_id keeps every path present, so the test -e probe cannot force a
// re-stream — only the changed sentinel set can. The mount's bytes must switch to
// the new file, which makes the bytes.Equal(prev, marker) guard load-bearing (a
// true-mutant of it leaves the stale bytes and fails here).
func TestFilesRematerializeWhenMountReassigned(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	mount := "/mnt/session/uploads/slot"
	h.seedFile(t, "file_one", "one")
	h.seedFile(t, "file_two", "two")
	h.refFiles(t, [2]string{"file_one", mount})

	h.suspend(t, writeUse("s1.txt", "x"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("first step: %v", err)
	}
	if sb.files[mount] != "one" {
		t.Fatalf("first materialization = %q, want one", sb.files[mount])
	}

	// Reassign the same mount_path to a different file. The path stays present, so
	// only the changed sentinel set can trigger the re-stream.
	h.refFiles(t, [2]string{"file_two", mount})
	h.suspend(t, writeUse("s2.txt", "y"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("second step: %v", err)
	}
	if sb.files[mount] != "two" {
		t.Errorf("reassigned mount = %q, want two (a changed sentinel set must re-stream)", sb.files[mount])
	}
}

// TestFilesSentinelPathCollision: a caller may mount a file at the sentinel's own
// path. The bookkeeping marker must never clobber that mount — the file's bytes
// win, the sentinel write is dropped, and the mount re-materializes every pass
// instead of being silently replaced by marker JSON and then skipped forever.
// Without the mountAtPath guard the first assertion sees the sentinel JSON.
func TestFilesSentinelPathCollision(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	sentinelPath := sandbox.DefaultWorkdir + "/" + filesSentinelName
	h.seedFile(t, "file_collide", "the user's bytes")
	h.refFiles(t, [2]string{"file_collide", sentinelPath})
	h.suspend(t, writeUse("out.txt", "x"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}
	if got := sb.files[sentinelPath]; got != "the user's bytes" {
		t.Fatalf("mount at the sentinel path = %q, want the user's file (the sentinel must not clobber it)", got)
	}
	// No wedge: with no sentinel written, a later pass re-materializes rather than
	// short-circuiting on a marker.
	sb.files[sentinelPath] = "mutated"
	h.suspend(t, writeUse("out2.txt", "y"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("second step: %v", err)
	}
	if got := sb.files[sentinelPath]; got != "the user's bytes" {
		t.Errorf("collision mount not re-materialized: %q, want the user's file", got)
	}
}

// TestMaterializeFilesNoResources: a session with no file resources materializes
// nothing and writes no sentinel — the common case must not touch the sandbox.
func TestMaterializeFilesNoResources(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.suspend(t, writeUse("out.txt", "x"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}
	if _, ok := sb.files[sandbox.DefaultWorkdir+"/"+filesSentinelName]; ok {
		t.Error("a resource-less session wrote a files sentinel")
	}
}
