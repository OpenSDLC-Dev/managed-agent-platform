package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// seedFile plants a file server-side as an upload would: a downloadable=false row
// plus its object in the control plane's store. The worker only ever sees it over
// the wire, through the environment-scoped environment-key content lane.
func (h *harness) seedFile(t *testing.T, id, filename, mime, content string) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO files (id, filename, mime_type, size_bytes, downloadable)
		 VALUES ($1,$2,$3,$4,false) ON CONFLICT (id) DO NOTHING`,
		id, filename, mime, len(content)); err != nil {
		t.Fatalf("seed file row: %v", err)
	}
	if err := h.blobs.Put(ctx, blob.FilesKey(id),
		bytes.NewReader([]byte(content)), int64(len(content)), mime); err != nil {
		t.Fatalf("seed file object: %v", err)
	}
}

// refFileMounts points the session's resources[] at the given {file_id, mount_path}
// mounts, the file-variant shape the API stores.
func (h *harness) refFileMounts(t *testing.T, mounts ...[2]string) {
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

// TestSetupFilesOverTheWire is the BYOC file-materialization end to end: a worker
// pulls a mounted file's bytes from the control plane over its environment key —
// exercising the environment-scoped content lane slice 4 adds — and streams them
// into the sandbox. It also pins the sentinel: an unchanged set skips, a deleted
// mount re-streams.
func TestSetupFilesOverTheWire(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	mount := "/workspace/uploads/report.txt"
	// A real (domain-valid) file id — the download API validates the id format.
	fileID := domain.NewID("file").String()
	h.seedFile(t, fileID, "report.txt", "text/plain", "quarterly numbers")
	h.refFileMounts(t, [2]string{fileID, mount})
	h.suspend(t, writeUse("out.txt", "hello"))

	if err := h.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	// The file landed through the environment-key content lane: session GET →
	// resources[] → GET /v1/files/{id}/content (environment-scoped) → stream.
	if got := sb.files[mount]; got != "quarterly numbers" {
		t.Errorf("mounted file = %q, want the uploaded content", got)
	}
	if sb.files["/workspace/out.txt"] != "hello" {
		t.Errorf("tool write = %q", sb.files["/workspace/out.txt"])
	}
	if sentinel := sb.files["/workspace/"+filesSentinelName]; !strings.Contains(sentinel, fileID) {
		t.Errorf("sentinel = %q", sentinel)
	}

	// An unchanged set skips restreaming on a reclaiming pass.
	sb.files[mount] = "mutated"
	h.suspend(t, writeUse("out2.txt", "again"))
	if err := h.run(); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := sb.files[mount]; got != "mutated" {
		t.Errorf("unchanged set was rewritten: %q", got)
	}

	// The workdir is agent-writable: a deleted mount while the marker survives is
	// caught by the test -e probe and restored on the next pass.
	delete(sb.files, mount)
	h.suspend(t, writeUse("out3.txt", "thrice"))
	if err := h.run(); err != nil {
		t.Fatalf("third run: %v", err)
	}
	if got := sb.files[mount]; got != "quarterly numbers" {
		t.Errorf("deleted mount not restored: %q", got)
	}
}

// TestSetupFilesSentinelPathCollision: a caller may mount a file at the worker's
// sentinel path. The marker must never overwrite that mount — the file wins, the
// sentinel write is dropped, and the mount re-materializes every pass instead of
// being replaced by marker JSON and wedged. Without the mountAtPath guard the
// first assertion sees the sentinel JSON.
func TestSetupFilesSentinelPathCollision(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	sentinelPath := "/workspace/" + filesSentinelName
	fileID := domain.NewID("file").String()
	h.seedFile(t, fileID, "marker.txt", "text/plain", "the user's bytes")
	h.refFileMounts(t, [2]string{fileID, sentinelPath})
	h.suspend(t, writeUse("out.txt", "x"))
	if err := h.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := sb.files[sentinelPath]; got != "the user's bytes" {
		t.Fatalf("mount at the sentinel path = %q, want the user's file (the sentinel must not clobber it)", got)
	}
	// The read-side skip must be disabled on collision too: plant the exact marker
	// bytes at the mount (a pre-guard clobber healed on upgrade, or bytes the agent
	// wrote). Without the read guard the skip fires on marker-equal bytes and the
	// stale marker wedges the mount; with it, the file re-materializes.
	sb.files[sentinelPath] = string(filesSentinel([]fileRef{{FileID: fileID, MountPath: sentinelPath}}))
	h.suspend(t, writeUse("out2.txt", "y"))
	if err := h.run(); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := sb.files[sentinelPath]; got != "the user's bytes" {
		t.Errorf("collision mount not re-materialized (read-side skip unguarded): %q, want the user's file", got)
	}
}

// TestSetupFilesTolerance: a dangling mount (no such file — the content lane
// answers 404) is skipped, and the run still materializes the good mount and
// proceeds. A per-file miss is never fatal.
func TestSetupFilesTolerance(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	good := "/workspace/uploads/good.txt"
	goodID := domain.NewID("file").String()
	goneID := domain.NewID("file").String() // valid id, never seeded -> content lane 404
	h.seedFile(t, goodID, "good.txt", "text/plain", "present")
	h.refFileMounts(t,
		[2]string{goneID, "/workspace/uploads/gone.txt"}, // no row/object -> 404 -> skip
		[2]string{goodID, good},
	)
	h.suspend(t, writeUse("out.txt", "hello"))

	if err := h.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := sb.files[good]; got != "present" {
		t.Errorf("good mount = %q, want present", got)
	}
	if _, ok := sb.files["/workspace/uploads/gone.txt"]; ok {
		t.Error("a dangling mount must land nothing")
	}
	if sb.files["/workspace/out.txt"] != "hello" {
		t.Errorf("tools did not run after a tolerated file miss: %q", sb.files["/workspace/out.txt"])
	}
	// The sentinel records only what landed, so the dangling mount is retried next
	// pass rather than wedged.
	if sentinel := sb.files["/workspace/"+filesSentinelName]; strings.Contains(sentinel, goneID) {
		t.Errorf("sentinel recorded a mount that never landed: %q", sentinel)
	}
}
