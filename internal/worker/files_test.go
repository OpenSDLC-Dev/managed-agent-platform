package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
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

// TestSetupFilesUnansweredProbeKeepsTheAgentsEdit is the executor's rule over
// the wire: a probe that does not answer — the presence exec's answer an
// image's startup pushed out of the output (sandbox.StartupOutputError) —
// says nothing of whether a mount has gone, so the set the sentinel says
// landed is not re-streamed over the agent's edit (#860).
func TestSetupFilesUnansweredProbeKeepsTheAgentsEdit(t *testing.T) {
	flood := &sandbox.StartupOutputError{What: "the command's exit record", Ran: true}
	for _, c := range []struct {
		name  string
		flood func(sb *fakeSandbox)
	}{
		{"the presence exec", func(sb *fakeSandbox) { sb.execErr = flood }},
	} {
		t.Run(c.name, func(t *testing.T) {
			sb := &fakeSandbox{}
			h := newHarness(t, sb)
			mount := "/workspace/uploads/report.txt"
			fileID := domain.NewID("file").String()
			h.seedFile(t, fileID, "report.txt", "text/plain", "as uploaded")
			h.refFileMounts(t, [2]string{fileID, mount})
			h.suspend(t, writeUse("out.txt", "hello"))
			if err := h.run(); err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := sb.files[mount]; got != "as uploaded" {
				t.Fatalf("mounted file = %q", got)
			}

			sb.files[mount] = "the agent's edit"
			c.flood(sb)
			h.suspend(t, writeUse("out2.txt", "again"))
			if err := h.run(); err != nil {
				t.Fatalf("second run: %v", err)
			}
			if got := sb.files[mount]; got != "the agent's edit" {
				t.Errorf("mount = %q after a probe that did not answer, want the agent's edit kept", got)
			}
		})
	}
}

// TestSetupFilesUnreadableSentinelStillLandsANewMount is the executor's rule
// over the wire: an unreadable sentinel is no record, so it counts as a
// changed set and the whole current set lands, the mount added since with it.
func TestSetupFilesUnreadableSentinelStillLandsANewMount(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	a, b := "/workspace/uploads/a.txt", "/workspace/uploads/b.txt"
	idA, idB := domain.NewID("file").String(), domain.NewID("file").String()
	h.seedFile(t, idA, "a.txt", "text/plain", "aaa")
	h.seedFile(t, idB, "b.txt", "text/plain", "bbb")
	h.refFileMounts(t, [2]string{idA, a})
	h.suspend(t, writeUse("out.txt", "hello"))
	if err := h.run(); err != nil {
		t.Fatalf("run: %v", err)
	}

	sb.readErr = errors.New("k8s: read /workspace/.files_materialized: exit 1: cat: Permission denied")
	h.refFileMounts(t, [2]string{idA, a}, [2]string{idB, b})
	h.suspend(t, writeUse("out2.txt", "again"))
	if err := h.run(); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := sb.files[b]; got != "bbb" {
		t.Errorf("new mount = %q behind an unreadable sentinel, want it landed", got)
	}
}

// TestSetupFilesUnreadableSentinelRelandsTheSet is the executor's rule over
// the wire: behind a marker that cannot be read, or does not parse, the whole
// set lands — a reassigned mount holds the file the set now names there, a
// sibling lands again whether deleted or edited — and the marker names that
// set.
func TestSetupFilesUnreadableSentinelRelandsTheSet(t *testing.T) {
	unreadable := func(sb *fakeSandbox, _ string) {
		sb.readErr = errors.New("k8s: read /workspace/.files_materialized: exit 1: cat: Permission denied")
	}
	// A marker whose first entry decodes and whose second does not: the
	// decoded one is no record either.
	unparsed := func(sb *fakeSandbox, idA string) {
		sb.files["/workspace/"+filesSentinelName] = `[{"file_id":"` + idA + `","mount_path":"/workspace/uploads/a"},{"file_id":7}]`
	}
	for _, c := range []struct {
		name    string
		deleteA bool
		marker  func(sb *fakeSandbox, idA string)
	}{
		{"a deleted, the marker unreadable", true, unreadable},
		{"a edited, the marker unreadable", false, unreadable},
		{"a edited, the marker unparsed", false, unparsed},
	} {
		t.Run(c.name, func(t *testing.T) {
			sb := &fakeSandbox{}
			h := newHarness(t, sb)
			a, b := "/workspace/uploads/a", "/workspace/uploads/b"
			idA, idB, idC := domain.NewID("file").String(), domain.NewID("file").String(), domain.NewID("file").String()
			h.seedFile(t, idA, "a", "text/plain", "A's bytes")
			h.seedFile(t, idB, "b", "text/plain", "B's bytes")
			h.seedFile(t, idC, "c", "text/plain", "C's bytes")
			h.refFileMounts(t, [2]string{idA, a}, [2]string{idB, b})
			h.suspend(t, writeUse("out.txt", "hello"))
			if err := h.run(); err != nil {
				t.Fatalf("run: %v", err)
			}

			if c.deleteA {
				delete(sb.files, a)
			} else {
				sb.files[a] = "the agent's edit"
			}
			h.refFileMounts(t, [2]string{idA, a}, [2]string{idC, b})
			c.marker(sb, idA)
			h.suspend(t, writeUse("out2.txt", "again"))
			if err := h.run(); err != nil {
				t.Fatalf("second run: %v", err)
			}
			if sb.files[a] != "A's bytes" || sb.files[b] != "C's bytes" {
				t.Errorf("a, b = %q, %q; want A's bytes and C's", sb.files[a], sb.files[b])
			}
			want := filesSentinel([]fileRef{{FileID: idA, MountPath: a}, {FileID: idC, MountPath: b}})
			if got := sb.files["/workspace/"+filesSentinelName]; got != string(want) {
				t.Errorf("marker = %s, want %s", got, want)
			}
		})
	}
}

// TestSetupFilesRelandOnlyTheMountThatIsGone is the executor's rule over the
// wire: the agent moves a.csv away and edits b.csv, and a.csv alone lands
// again — b.csv keeps the edit, on that pass and the next.
func TestSetupFilesRelandOnlyTheMountThatIsGone(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	a, b := "/workspace/uploads/a.csv", "/workspace/uploads/b.csv"
	idA, idB := domain.NewID("file").String(), domain.NewID("file").String()
	h.seedFile(t, idA, "a.csv", "text/csv", "a's bytes")
	h.seedFile(t, idB, "b.csv", "text/csv", "b's bytes")
	h.refFileMounts(t, [2]string{idA, a}, [2]string{idB, b})
	h.suspend(t, writeUse("out.txt", "hello"))
	if err := h.run(); err != nil {
		t.Fatalf("run: %v", err)
	}

	delete(sb.files, a)
	sb.files[b] = "the agent's edit"
	for i, use := range []string{writeUse("out2.txt", "again"), writeUse("out3.txt", "thrice")} {
		h.suspend(t, use)
		if err := h.run(); err != nil {
			t.Fatalf("run %d: %v", i+2, err)
		}
		if sb.files[a] != "a's bytes" || sb.files[b] != "the agent's edit" {
			t.Errorf("run %d: a, b = %q, %q; want a landed again and b as the agent left it", i+2, sb.files[a], sb.files[b])
		}
	}
	want := filesSentinel([]fileRef{{FileID: idA, MountPath: a}, {FileID: idB, MountPath: b}})
	if got := sb.files["/workspace/"+filesSentinelName]; got != string(want) {
		t.Errorf("marker = %s, want %s", got, want)
	}
}

// TestSetupFilesFailedMountDoesNotRelandTheOthers is the executor's rule over
// the wire: a pass that cannot land one mount — its file deleted or expired
// (the content lane answers 404), or a fault writing it — records the others
// alone, and the next pass keeps them, the agent's edit with them, and tries
// the failed one again.
func TestSetupFilesFailedMountDoesNotRelandTheOthers(t *testing.T) {
	for _, c := range []struct {
		name string
		// fail makes g's mount fail on the first pass, and says what it holds
		// after the second.
		fail func(t *testing.T, h *harness, sb *fakeSandbox, idG string) (after string, landsAfter bool)
	}{
		{"its file deleted", func(*testing.T, *harness, *fakeSandbox, string) (string, bool) { return "", false }},
		{"its file expired", func(t *testing.T, h *harness, _ *fakeSandbox, idG string) (string, bool) {
			h.seedFile(t, idG, "g", "text/plain", "g's bytes")
			if _, err := h.pool.Exec(context.Background(),
				`UPDATE files SET expires_at = now() - interval '1 second' WHERE id = $1`, idG); err != nil {
				t.Fatal(err)
			}
			return "", false
		}},
		{"a fault writing it", func(t *testing.T, h *harness, sb *fakeSandbox, idG string) (string, bool) {
			h.seedFile(t, idG, "g", "text/plain", "g's bytes")
			sb.failPath = "/workspace/uploads/g"
			return "g's bytes", true
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			sb := &fakeSandbox{}
			h := newHarness(t, sb)
			a, g := "/workspace/uploads/a", "/workspace/uploads/g"
			idA, idG := domain.NewID("file").String(), domain.NewID("file").String()
			h.seedFile(t, idA, "a", "text/plain", "a's bytes")
			after, landsAfter := c.fail(t, h, sb, idG)
			h.refFileMounts(t, [2]string{idA, a}, [2]string{idG, g})
			h.suspend(t, writeUse("out.txt", "hello"))
			if err := h.run(); err != nil {
				t.Fatalf("run: %v", err)
			}
			if _, ok := sb.files[g]; ok || sb.files[a] != "a's bytes" {
				t.Fatalf("first pass: a = %q, g landed %v; want a alone", sb.files[a], ok)
			}
			want := filesSentinel([]fileRef{{FileID: idA, MountPath: a}})
			if got := sb.files["/workspace/"+filesSentinelName]; got != string(want) {
				t.Errorf("first marker = %s, want %s", got, want)
			}

			sb.failPath = ""
			sb.files[a] = "the agent's edit"
			h.suspend(t, writeUse("out2.txt", "again"))
			if err := h.run(); err != nil {
				t.Fatalf("second run: %v", err)
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

// TestSetupFilesReassignedMountRelandsAlone is the executor's rule over the
// wire: b reassigned from file B to file C lands C's bytes, while a — the
// marker recording it under the file the set still names there — keeps the
// agent's edit.
func TestSetupFilesReassignedMountRelandsAlone(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	a, b := "/workspace/uploads/a", "/workspace/uploads/b"
	idA, idB, idC := domain.NewID("file").String(), domain.NewID("file").String(), domain.NewID("file").String()
	h.seedFile(t, idA, "a", "text/plain", "A's bytes")
	h.seedFile(t, idB, "b", "text/plain", "B's bytes")
	h.seedFile(t, idC, "c", "text/plain", "C's bytes")
	h.refFileMounts(t, [2]string{idA, a}, [2]string{idB, b})
	h.suspend(t, writeUse("out.txt", "hello"))
	if err := h.run(); err != nil {
		t.Fatalf("run: %v", err)
	}

	sb.files[a] = "the agent's edit"
	h.refFileMounts(t, [2]string{idA, a}, [2]string{idC, b})
	h.suspend(t, writeUse("out2.txt", "again"))
	if err := h.run(); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if sb.files[a] != "the agent's edit" || sb.files[b] != "C's bytes" {
		t.Errorf("a, b = %q, %q; want the agent's edit and C's bytes", sb.files[a], sb.files[b])
	}
	want := filesSentinel([]fileRef{{FileID: idA, MountPath: a}, {FileID: idC, MountPath: b}})
	if got := sb.files["/workspace/"+filesSentinelName]; got != string(want) {
		t.Errorf("marker = %s, want %s", got, want)
	}
}

// TestSetupFilesRemovedMountIsForgotten is the executor's rule over the wire:
// a mount removed from the set is forgotten by the next pass, so when it is
// mounted again over a file the agent wrote at its path, its file's bytes
// land there; the mount kept throughout keeps the agent's edit.
func TestSetupFilesRemovedMountIsForgotten(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	a, b := "/workspace/uploads/a", "/workspace/uploads/b"
	idA, idB := domain.NewID("file").String(), domain.NewID("file").String()
	h.seedFile(t, idA, "a", "text/plain", "A's bytes")
	h.seedFile(t, idB, "b", "text/plain", "B's bytes")
	h.refFileMounts(t, [2]string{idA, a}, [2]string{idB, b})
	h.suspend(t, writeUse("out.txt", "hello"))
	if err := h.run(); err != nil {
		t.Fatalf("run: %v", err)
	}

	sb.files[a] = "the agent's edit"
	h.refFileMounts(t, [2]string{idA, a})
	h.suspend(t, writeUse("out2.txt", "again"))
	if err := h.run(); err != nil {
		t.Fatalf("second run: %v", err)
	}
	sb.files[b] = "the agent's own file"
	h.refFileMounts(t, [2]string{idA, a}, [2]string{idB, b})
	h.suspend(t, writeUse("out3.txt", "thrice"))
	if err := h.run(); err != nil {
		t.Fatalf("third run: %v", err)
	}
	if sb.files[a] != "the agent's edit" || sb.files[b] != "B's bytes" {
		t.Errorf("a, b = %q, %q; want the agent's edit and B's bytes", sb.files[a], sb.files[b])
	}
}

// TestSetupFilesProbeOfAnOversizedSetIsBounded is the executor's rule over the
// wire: 130 mounts of 1 KB paths, every one gone, are asked in two probe
// execs — as few as the bound on one command allows — and all land again.
func TestSetupFilesProbeOfAnOversizedSetIsBounded(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	var mounts [][2]string
	for i := range 130 {
		id := domain.NewID("file").String()
		h.seedFile(t, id, "big", "text/plain", id)
		mounts = append(mounts, [2]string{id, fmt.Sprintf("/workspace/uploads/%03d/%s", i, strings.Repeat("p", 1000))})
	}
	h.refFileMounts(t, mounts...)
	h.suspend(t, writeUse("out.txt", "hello"))
	if err := h.run(); err != nil {
		t.Fatalf("run: %v", err)
	}

	for _, m := range mounts {
		delete(sb.files, m[1])
	}
	sb.cmds = nil
	h.suspend(t, writeUse("out2.txt", "again"))
	if err := h.run(); err != nil {
		t.Fatalf("second run: %v", err)
	}
	probes := 0
	for _, cmd := range sb.cmds {
		if strings.HasPrefix(cmd, "test -e '/workspace/uploads/") {
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

// unlengthed answers with chunked framing: it drops the Content-Length the
// control plane set and commits the header before the body, which is what makes
// the server frame the rest in chunks. A client then reads ContentLength -1 —
// the same thing it reads through a proxy that re-frames the response, or that
// gzips it and lets Go decompress it transparently.
type unlengthed struct {
	http.ResponseWriter
	committed bool
	stripped  *atomic.Bool // set when a declared length was actually removed
}

func (u *unlengthed) WriteHeader(code int) {
	if u.committed {
		return
	}
	u.committed = true
	if u.Header().Get("Content-Length") != "" {
		u.Header().Del("Content-Length")
		u.stripped.Store(true)
	}
	u.ResponseWriter.WriteHeader(code)
	if f, ok := u.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (u *unlengthed) Write(b []byte) (int, error) {
	u.WriteHeader(http.StatusOK)
	return u.ResponseWriter.Write(b)
}

// stripContentLength wraps the control plane so the file-content lane — and only
// that lane — answers without a declared length. Everything else, the session
// read and the work API the worker runs on, is served untouched. It reports
// whether it ever had a length to remove, because a test whose premise quietly
// stopped holding — a download that declares its length after all — would go on
// passing while exercising the wrong branch entirely.
func stripContentLength(stripped *atomic.Bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasSuffix(r.URL.Path, "/content") {
				next.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(&unlengthed{ResponseWriter: w, stripped: stripped}, r)
		})
	}
}

// TestSetupFilesDownloadWithoutContentLength: the worker does not control how
// its answers are framed. A proxy, a service mesh, or a hop that compresses on
// the way leaves the download with no Content-Length, which Go reports as -1 —
// and the worker has no second source for the count: the session resource
// carries no size and its environment key cannot read file metadata. It must
// still land the file. Before the fix the -1 went straight to the write seam,
// which refused it, so every mount behind such an intermediary was skipped and
// the agent started with an empty workspace.
func TestSetupFilesDownloadWithoutContentLength(t *testing.T) {
	sb := &fakeSandbox{}
	var stripped atomic.Bool
	h := newHarnessWrapped(t, sb, stripContentLength(&stripped))
	mount := "/workspace/uploads/report.txt"
	fileID := domain.NewID("file").String()
	h.seedFile(t, fileID, "report.txt", "text/plain", "quarterly numbers")
	h.refFileMounts(t, [2]string{fileID, mount})
	h.suspend(t, writeUse("out.txt", "hello"))

	if err := h.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	// The premise first: the download really did leave the control plane with no
	// declared length. Without this the row could pass on the branch it is not
	// about.
	if !stripped.Load() {
		t.Fatal("the download declared a length after all; this row proves nothing")
	}
	if got := sb.files[mount]; got != "quarterly numbers" {
		t.Errorf("mount from a length-less download = %q, want the uploaded content", got)
	}
	// It landed as a mount, not as a tolerated miss: the sentinel records it, so
	// the next pass skips instead of re-streaming.
	if sentinel := sb.files["/workspace/"+filesSentinelName]; !strings.Contains(sentinel, fileID) {
		t.Errorf("sentinel = %q, want the mount recorded as landed", sentinel)
	}
}

// TestSpoolBodyBudget: the spool exists because the body declares no end, so it
// needs an end of its own. A body at the budget is spooled — the bound is not
// off by one, and the 500 MB a mount may legitimately be is not refused — and
// one byte past it is refused rather than truncated into a silently short mount.
//
// And no path leaves a file behind, including the one still in use: the spool is
// unlinked as soon as it exists, so a worker killed mid-mount — an eviction, an
// OOM, a redeploy — strands none of a customer's file bytes on its disk. The
// directory is required to be empty while the spool is still being read from,
// which is the only moment a create-then-remove-later spool would show one.
func TestSpoolBodyBudget(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp) // so the residue check sees only this test's spools
	defer func(prev int64) { maxSpoolBytes = prev }(maxSpoolBytes)
	maxSpoolBytes = 8

	spool, n, err := spoolBody(strings.NewReader("12345678"))
	if err != nil || n != 8 {
		t.Fatalf("a body at the budget: (%d, %v), want (8, nil)", n, err)
	}
	if left, err := os.ReadDir(tmp); err != nil {
		t.Fatal(err)
	} else if len(left) != 0 {
		t.Errorf("the spool in use is named in the filesystem (%d entries), want it already unlinked", len(left))
	}
	if got, err := io.ReadAll(spool); err != nil || string(got) != "12345678" {
		t.Errorf("the spool reads back %q (%v), want it rewound to the whole body", got, err)
	}
	spool.Close()

	if spool, n, err := spoolBody(strings.NewReader("123456789")); err == nil {
		spool.Close()
		t.Fatalf("a body one byte over the budget was spooled as %d bytes", n)
	}
	if left, err := os.ReadDir(tmp); err != nil {
		t.Fatal(err)
	} else if len(left) != 0 {
		t.Errorf("a refused spool left %d file(s) behind", len(left))
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
