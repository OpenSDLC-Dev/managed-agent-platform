package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
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

// TestMaterializesASessionCopy: a session's copy of an upload (#578) has no
// object at its own id's key — it aliases the upload's, named by object_key —
// and mounts from there, even once the upload's row is gone. Reading
// blob.FilesKey(copy) would find nothing and skip the mount.
func TestMaterializesASessionCopy(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedFile(t, "file_upload", "aliased bytes")
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO files (id, filename, mime_type, size_bytes, scope_type, scope_id, object_key, source_file_id)
		 SELECT 'file_copy', filename, mime_type, size_bytes, 'session', $1, `+store.FileObjectKeySQL+`, id
		   FROM files WHERE id = 'file_upload'`, h.sid.String()); err != nil {
		t.Fatalf("seed the copy: %v", err)
	}
	if _, err := h.pool.Exec(context.Background(), `DELETE FROM files WHERE id = 'file_upload'`); err != nil {
		t.Fatal(err)
	}
	mount := "/mnt/session/uploads/file_upload"
	h.refFiles(t, [2]string{"file_copy", mount})

	h.suspend(t, writeUse("out.txt", "x"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}
	if got := sb.files[mount]; got != "aliased bytes" {
		t.Errorf("mounted content = %q, want the upload's bytes through the copy", got)
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

// TestMaterializeExpiredFileNotMounted: past expires_at the content route
// answers 404, so the platform-managed half must not stream the bytes into the
// sandbox either. Otherwise the two deployment points of one pull protocol
// disagree — a cloud session mounts what a BYOC worker is refused (#655).
func TestMaterializeExpiredFileNotMounted(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedFile(t, "file_expired", "expired bytes")
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE files SET expires_at = now() - interval '1 second' WHERE id = $1`,
		"file_expired"); err != nil {
		t.Fatalf("expire the seeded file: %v", err)
	}
	mount := "/mnt/session/uploads/file_expired"
	h.refFiles(t, [2]string{"file_expired", mount})

	h.suspend(t, writeUse("out.txt", "x"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}
	if got, ok := sb.files[mount]; ok {
		t.Errorf("an expired file must not mount, got %q", got)
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

// TestFilesUnansweredProbeKeepsTheAgentsEdit: a probe that does not answer —
// the presence exec's answer an image's startup pushed out of the output
// (sandbox.StartupOutputError) — says nothing of whether a mount has gone.
// Taken for "gone", it re-streamed every mount over the agent's edits on
// every pass (#860); the set landed once (the sentinel), so what is there
// stays. (A sentinel that could not be read is another matter: a set the
// pass cannot match, TestFilesUnreadableSentinelRelandsTheSet.)
func TestFilesUnansweredProbeKeepsTheAgentsEdit(t *testing.T) {
	flood := &sandbox.StartupOutputError{What: "the command's exit record", Ran: true}
	for _, c := range []struct {
		name  string
		flood func(sb *fakeSandbox)
	}{
		{"the presence exec", func(sb *fakeSandbox) { sb.execErr, sb.execErrOn = flood, "test -e " }},
	} {
		t.Run(c.name, func(t *testing.T) {
			sb := &fakeSandbox{}
			h := newHarness(t, sb)
			mount := "/mnt/session/uploads/file_edit"
			h.seedFile(t, "file_edit", "as uploaded")
			h.refFiles(t, [2]string{"file_edit", mount})

			h.suspend(t, writeUse("a.txt", "x"))
			if _, err := h.exec.step(context.Background()); err != nil {
				t.Fatalf("first step: %v", err)
			}
			if sb.files[mount] != "as uploaded" {
				t.Fatalf("first materialization = %q", sb.files[mount])
			}

			// The agent edits the mount, and the image's startup floods.
			sb.files[mount] = "the agent's edit"
			c.flood(sb)
			h.suspend(t, writeUse("b.txt", "y"))
			if _, err := h.exec.step(context.Background()); err != nil {
				t.Fatalf("second step: %v", err)
			}
			if sb.files[mount] != "the agent's edit" {
				t.Errorf("mount = %q after a probe that did not answer, want the agent's edit kept", sb.files[mount])
			}
		})
	}
}

// TestFilesUnreadableSentinelStillLandsANewMount: the sentinel is only a
// shortcut. One the sandbox cannot read — the agent chmod 000s it, and the
// read's cat exits 1 — is no record either way, so it counts as a changed
// set: the whole current set lands, the mount added since with it, and the
// marker is rewritten. An unreadable marker that ended the pass kept a new
// mount out for good.
func TestFilesUnreadableSentinelStillLandsANewMount(t *testing.T) {
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

	sb.readErr = errors.New("k8s: read /workspace/.files_materialized: exit 1: cat: Permission denied")
	h.refFiles(t, [2]string{"file_a", a}, [2]string{"file_b", b})
	h.suspend(t, writeUse("t2.txt", "y"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("second step: %v", err)
	}
	if sb.files[b] != "bbb" {
		t.Errorf("new mount = %q behind an unreadable sentinel, want it landed", sb.files[b])
	}
}

// TestFilesRematerializeAMountDeletedFromASetTooLongForOneProbe: 130 mounts
// of 1 KB paths make a presence probe past the bound on one exec. Refused
// unasked, it read as unknown and kept a deleted mount out for good; the
// probe is asked in batches (sandbox.ProbePaths), and the deleted mount is
// absent and lands again.
func TestFilesRematerializeAMountDeletedFromASetTooLongForOneProbe(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	var mounts [][2]string
	for i := range 130 {
		id := fmt.Sprintf("file_big%03d", i)
		h.seedFile(t, id, id)
		mounts = append(mounts, [2]string{id, fmt.Sprintf("/mnt/session/uploads/%03d/%s", i, strings.Repeat("p", 1000))})
	}
	h.refFiles(t, mounts...)
	h.suspend(t, writeUse("a.txt", "x"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("first step: %v", err)
	}
	gone := mounts[117][1]
	if sb.files[gone] != "file_big117" {
		t.Fatalf("first materialization of %s = %q", gone, sb.files[gone])
	}

	delete(sb.files, gone)
	h.suspend(t, writeUse("b.txt", "y"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("second step: %v", err)
	}
	if sb.files[gone] != "file_big117" {
		t.Errorf("deleted mount = %q, want it re-materialized", sb.files[gone])
	}
}

// TestFilesUnreadableSentinelRelandsTheSet: mounts A at a and B at b land;
// then b is reassigned to file C — with a deleted, or with a edited by the
// agent — and the marker cannot be read, or does not parse. A marker the
// pass cannot read is no record of any mount, so the whole current set lands
// — b holds C's bytes, never B's kept as though they were C's, and a lands
// again over the edit — and the marker is rewritten to name it.
func TestFilesUnreadableSentinelRelandsTheSet(t *testing.T) {
	unreadable := func(sb *fakeSandbox) {
		sb.readErr = errors.New("k8s: read /workspace/.files_materialized: exit 1: cat: Permission denied")
	}
	// A marker whose first entry decodes and whose second does not: the
	// decoded one is no record either.
	unparsed := func(sb *fakeSandbox) {
		sb.files["/workspace/"+filesSentinelName] = `[{"file_id":"file_A","mount_path":"/mnt/session/uploads/a"},{"file_id":7}]`
	}
	for _, c := range []struct {
		name    string
		deleteA bool
		marker  func(*fakeSandbox)
	}{
		{"a deleted, the marker unreadable", true, unreadable},
		{"a edited, the marker unreadable", false, unreadable},
		{"a edited, the marker unparsed", false, unparsed},
	} {
		t.Run(c.name, func(t *testing.T) {
			sb := &fakeSandbox{}
			h := newHarness(t, sb)
			a, b := "/mnt/session/uploads/a", "/mnt/session/uploads/b"
			h.seedFile(t, "file_A", "A's bytes")
			h.seedFile(t, "file_B", "B's bytes")
			h.seedFile(t, "file_C", "C's bytes")
			h.refFiles(t, [2]string{"file_A", a}, [2]string{"file_B", b})
			h.suspend(t, writeUse("t1.txt", "x"))
			if _, err := h.exec.step(context.Background()); err != nil {
				t.Fatalf("first step: %v", err)
			}

			if c.deleteA {
				delete(sb.files, a)
			} else {
				sb.files[a] = "the agent's edit"
			}
			h.refFiles(t, [2]string{"file_A", a}, [2]string{"file_C", b})
			c.marker(sb)
			h.suspend(t, writeUse("t2.txt", "y"))
			if _, err := h.exec.step(context.Background()); err != nil {
				t.Fatalf("second step: %v", err)
			}
			if sb.files[a] != "A's bytes" || sb.files[b] != "C's bytes" {
				t.Errorf("a, b = %q, %q; want A's bytes and C's", sb.files[a], sb.files[b])
			}
			want := filesSentinel([]fileRef{{FileID: "file_A", MountPath: a}, {FileID: "file_C", MountPath: b}})
			if got := sb.files["/workspace/"+filesSentinelName]; got != string(want) {
				t.Errorf("marker = %s, want %s", got, want)
			}
		})
	}
}

// TestFilesRelandOnlyTheMountThatIsGone: the agent moves a.csv away and edits
// b.csv. The marker still records both under their files, and the probe finds
// a.csv alone gone, so a.csv alone lands again: b.csv keeps the agent's edit —
// on that pass and on the next, the marker still recording it. Re-landing the
// set instead overwrote b.csv.
func TestFilesRelandOnlyTheMountThatIsGone(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	a, b := "/mnt/session/uploads/a.csv", "/mnt/session/uploads/b.csv"
	h.seedFile(t, "file_a", "a's bytes")
	h.seedFile(t, "file_b", "b's bytes")
	h.refFiles(t, [2]string{"file_a", a}, [2]string{"file_b", b})
	h.suspend(t, writeUse("t1.txt", "x"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("first step: %v", err)
	}

	delete(sb.files, a)
	sb.files[b] = "the agent's edit"
	for i, use := range []string{writeUse("t2.txt", "y"), writeUse("t3.txt", "z")} {
		h.suspend(t, use)
		if _, err := h.exec.step(context.Background()); err != nil {
			t.Fatalf("step %d: %v", i+2, err)
		}
		if sb.files[a] != "a's bytes" || sb.files[b] != "the agent's edit" {
			t.Errorf("step %d: a, b = %q, %q; want a landed again and b as the agent left it", i+2, sb.files[a], sb.files[b])
		}
	}
	want := filesSentinel([]fileRef{{FileID: "file_a", MountPath: a}, {FileID: "file_b", MountPath: b}})
	if got := sb.files["/workspace/"+filesSentinelName]; got != string(want) {
		t.Errorf("marker = %s, want %s", got, want)
	}
}

// TestFilesFailedMountDoesNotRelandTheOthers: a pass that cannot land one
// mount — its file deleted or expired, or a fault writing it — records the
// others alone in the marker. The next pass keeps those, and the agent's edit
// to one with them, and tries the failed mount again: it lands where its file
// is back. A marker recording less than the set re-landed the whole set over
// the agent's edits, on every pass while the file stayed gone.
func TestFilesFailedMountDoesNotRelandTheOthers(t *testing.T) {
	for _, c := range []struct {
		name string
		// fail makes g's mount fail on the first pass, and says what it holds
		// after the second.
		fail func(t *testing.T, h *harness, sb *fakeSandbox) (after string, landsAfter bool)
	}{
		{"its file deleted", func(*testing.T, *harness, *fakeSandbox) (string, bool) { return "", false }},
		{"its file expired", func(t *testing.T, h *harness, _ *fakeSandbox) (string, bool) {
			h.seedFile(t, "file_g", "g's bytes")
			if _, err := h.pool.Exec(context.Background(),
				`UPDATE files SET expires_at = now() - interval '1 second' WHERE id = 'file_g'`); err != nil {
				t.Fatal(err)
			}
			return "", false
		}},
		{"a fault writing it", func(t *testing.T, h *harness, sb *fakeSandbox) (string, bool) {
			h.seedFile(t, "file_g", "g's bytes")
			sb.failPath = "/mnt/session/uploads/g"
			return "g's bytes", true
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			sb := &fakeSandbox{}
			h := newHarness(t, sb)
			a, g := "/mnt/session/uploads/a", "/mnt/session/uploads/g"
			h.seedFile(t, "file_a", "a's bytes")
			after, landsAfter := c.fail(t, h, sb)
			h.refFiles(t, [2]string{"file_a", a}, [2]string{"file_g", g})
			h.suspend(t, writeUse("t1.txt", "x"))
			if _, err := h.exec.step(context.Background()); err != nil {
				t.Fatalf("first step: %v", err)
			}
			if _, ok := sb.files[g]; ok || sb.files[a] != "a's bytes" {
				t.Fatalf("first pass: a = %q, g landed %v; want a alone", sb.files[a], ok)
			}
			want := filesSentinel([]fileRef{{FileID: "file_a", MountPath: a}})
			if got := sb.files["/workspace/"+filesSentinelName]; got != string(want) {
				t.Errorf("first marker = %s, want %s", got, want)
			}

			sb.failPath = ""
			sb.files[a] = "the agent's edit"
			h.suspend(t, writeUse("t2.txt", "y"))
			if _, err := h.exec.step(context.Background()); err != nil {
				t.Fatalf("second step: %v", err)
			}
			if sb.files[a] != "the agent's edit" {
				t.Errorf("a = %q after a pass that failed g, want the agent's edit kept", sb.files[a])
			}
			if got, ok := sb.files[g]; ok != landsAfter || got != after {
				t.Errorf("g = %q (landed %v), want %q (landed %v)", got, ok, after, landsAfter)
			}
		})
	}
}

// TestFilesReassignedMountRelandsAlone: b is reassigned from file B to file C
// while the agent has edited a. The marker records b under B, so b lands
// again, with C's bytes; a, recorded under the file the set still names there
// and still there, keeps the edit; and the marker records b under C.
func TestFilesReassignedMountRelandsAlone(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	a, b := "/mnt/session/uploads/a", "/mnt/session/uploads/b"
	h.seedFile(t, "file_A", "A's bytes")
	h.seedFile(t, "file_B", "B's bytes")
	h.seedFile(t, "file_C", "C's bytes")
	h.refFiles(t, [2]string{"file_A", a}, [2]string{"file_B", b})
	h.suspend(t, writeUse("t1.txt", "x"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("first step: %v", err)
	}

	sb.files[a] = "the agent's edit"
	h.refFiles(t, [2]string{"file_A", a}, [2]string{"file_C", b})
	h.suspend(t, writeUse("t2.txt", "y"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("second step: %v", err)
	}
	if sb.files[a] != "the agent's edit" || sb.files[b] != "C's bytes" {
		t.Errorf("a, b = %q, %q; want the agent's edit and C's bytes", sb.files[a], sb.files[b])
	}
	want := filesSentinel([]fileRef{{FileID: "file_A", MountPath: a}, {FileID: "file_C", MountPath: b}})
	if got := sb.files["/workspace/"+filesSentinelName]; got != string(want) {
		t.Errorf("marker = %s, want %s", got, want)
	}
}

// TestFilesRemovedMountIsForgotten: b is removed from the set, the agent writes
// a file of its own at b, and the set mounts B at b again. The pass after the
// removal forgets b — nothing lands, but the marker no longer records it — so
// the pass after the re-add lands B's bytes there, and a, kept throughout,
// keeps the agent's edit. A marker still recording b would keep the agent's
// file as though it were B.
func TestFilesRemovedMountIsForgotten(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	a, b := "/mnt/session/uploads/a", "/mnt/session/uploads/b"
	h.seedFile(t, "file_A", "A's bytes")
	h.seedFile(t, "file_B", "B's bytes")
	h.refFiles(t, [2]string{"file_A", a}, [2]string{"file_B", b})
	h.suspend(t, writeUse("t1.txt", "x"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("first step: %v", err)
	}

	sb.files[a] = "the agent's edit"
	h.refFiles(t, [2]string{"file_A", a})
	h.suspend(t, writeUse("t2.txt", "y"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("second step: %v", err)
	}
	sb.files[b] = "the agent's own file"
	h.refFiles(t, [2]string{"file_A", a}, [2]string{"file_B", b})
	h.suspend(t, writeUse("t3.txt", "z"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("third step: %v", err)
	}
	if sb.files[a] != "the agent's edit" || sb.files[b] != "B's bytes" {
		t.Errorf("a, b = %q, %q; want the agent's edit and B's bytes", sb.files[a], sb.files[b])
	}
}

// TestFilesProbeOfAnOversizedSetIsBounded: 130 mounts of 1 KB paths, every
// one gone — the uploads directory removed. The probe asks them all in as
// few execs as the bound on one command allows — two — and every mount lands
// again; a search that halved the set made hundreds of execs here, and
// thousands for a larger one.
func TestFilesProbeOfAnOversizedSetIsBounded(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	var mounts [][2]string
	for i := range 130 {
		id := fmt.Sprintf("file_big%03d", i)
		h.seedFile(t, id, id)
		mounts = append(mounts, [2]string{id, fmt.Sprintf("/mnt/session/uploads/%03d/%s", i, strings.Repeat("p", 1000))})
	}
	h.refFiles(t, mounts...)
	h.suspend(t, writeUse("a.txt", "x"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("first step: %v", err)
	}

	for _, m := range mounts {
		delete(sb.files, m[1])
	}
	sb.cmds = nil
	h.suspend(t, writeUse("b.txt", "y"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("second step: %v", err)
	}
	probes := 0
	for _, cmd := range sb.cmds {
		if strings.HasPrefix(cmd, "test -e '/mnt/session/uploads/") {
			probes++
		}
	}
	if probes == 0 || probes > 2 {
		t.Errorf("the probe took %d execs for 130 mounts, want at most 2", probes)
	}
	for _, m := range mounts {
		if sb.files[m[1]] != m[0] {
			t.Fatalf("%s = %q, want it landed again", m[1], sb.files[m[1]])
		}
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
	// The read-side skip must be disabled on collision too: plant the exact marker
	// bytes at the mount (a pre-guard clobber healed on upgrade, or bytes the agent
	// wrote). Without the read guard the skip fires on marker-equal bytes and the
	// stale marker wedges the mount; with it, the file re-materializes.
	sb.files[sentinelPath] = string(filesSentinel([]fileRef{{FileID: "file_collide", MountPath: sentinelPath}}))
	h.suspend(t, writeUse("out2.txt", "y"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("second step: %v", err)
	}
	if got := sb.files[sentinelPath]; got != "the user's bytes" {
		t.Errorf("collision mount not re-materialized (read-side skip unguarded): %q, want the user's file", got)
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
