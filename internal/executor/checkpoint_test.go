package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// exportTar builds what a backend's Export streams: members under one
// top-level directory. files maps member path (already prefixed with the top
// dir) to content; a path ending in "/" is a directory; links maps a symlink
// member to its target.
func exportTar(t *testing.T, files map[string]string, links map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for p, content := range files {
		if strings.HasSuffix(p, "/") {
			if err := tw.WriteHeader(&tar.Header{Name: p, Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := tw.WriteHeader(&tar.Header{Name: p, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	for p, target := range links {
		if err := tw.WriteHeader(&tar.Header{Name: p, Typeflag: tar.TypeSymlink, Linkname: target, Mode: 0o777}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// checkpointMembers downloads and walks the session's checkpoint blob:
// member path → content ("" for dirs and links).
func checkpointMembers(t *testing.T, h *harness) map[string]string {
	t.Helper()
	rc, _, err := h.blobs.Get(context.Background(), blob.SessionCheckpointKey(h.sid.String()))
	if err != nil {
		t.Fatalf("get checkpoint blob: %v", err)
	}
	defer rc.Close()
	gz, err := gzip.NewReader(rc)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	members := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return members
		}
		if err != nil {
			t.Fatalf("walk checkpoint: %v", err)
		}
		var content []byte
		if hdr.Typeflag == tar.TypeReg {
			if content, err = io.ReadAll(tr); err != nil {
				t.Fatal(err)
			}
		}
		members[hdr.Name] = string(content)
	}
}

func markerState(t *testing.T, h *harness) (state, key string) {
	t.Helper()
	err := h.pool.QueryRow(context.Background(),
		`SELECT state, blob_key FROM session_checkpoints WHERE session_id = $1`, h.sid.String()).
		Scan(&state, &key)
	if err != nil {
		return "", ""
	}
	return state, key
}

func setMarker(t *testing.T, h *harness, state, key string) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO session_checkpoints (session_id, blob_key, state) VALUES ($1, $2, $3)
		 ON CONFLICT (session_id) DO UPDATE SET blob_key = $2, state = $3`,
		h.sid.String(), key, state); err != nil {
		t.Fatal(err)
	}
}

// shellRoot is the harness session's shell-state export root.
func shellRoot(h *harness) string { return sandbox.ShellStateRoot + "/" + h.sid.String() }

// TestCaptureCombinesRootsStripsSentinelsMarksReady: the three roots land in
// one gzipped tar re-rooted at their real paths, the workdir's
// materialization sentinels are stripped (and only the workdir's — a
// same-named file in another root is state), symlinks survive, and the
// marker comes up ready.
func TestCaptureCombinesRootsStripsSentinelsMarksReady(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	sid := h.sid.String()
	h.prov.owned = []domain.ID{h.sid}
	h.prov.exports = map[string][]byte{
		"/workspace": exportTar(t, map[string]string{
			"workspace/":                        "",
			"workspace/notes.txt":               "keep me",
			"workspace/skills/.materialized":    "sentinel",
			"workspace/.files_materialized":     "sentinel",
			"workspace/sub/.files_materialized": "decoy — agent state, kept",
		}, map[string]string{"workspace/notes.link": "notes.txt"}),
		shellRoot(h): exportTar(t, map[string]string{
			sid + "/":                    "",
			sid + "/head":                "snap3",
			sid + "/.files_materialized": "not the workdir — kept",
		}, nil),
		outputsDir: exportTar(t, map[string]string{
			"outputs/report.pdf": "deliverable",
		}, nil),
	}

	if err := h.exec.captureCheckpoint(context.Background(), h.sid); err != nil {
		t.Fatalf("capture: %v", err)
	}
	got := checkpointMembers(t, h)
	for _, want := range []string{
		"workspace/notes.txt",
		"workspace/notes.link",
		"var/lib/map-shell/" + sid + "/head",
		"var/lib/map-shell/" + sid + "/.files_materialized",
		"mnt/session/outputs/report.pdf",
		"workspace/sub/.files_materialized",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("checkpoint missing member %q (have %v)", want, keys(got))
		}
	}
	if got["workspace/notes.txt"] != "keep me" {
		t.Errorf("notes.txt content = %q", got["workspace/notes.txt"])
	}
	for _, stripped := range []string{"workspace/skills/.materialized", "workspace/.files_materialized"} {
		if _, ok := got[stripped]; ok {
			t.Errorf("sentinel %q survived capture", stripped)
		}
	}
	if state, key := markerState(t, h); state != "ready" || key != blob.SessionCheckpointKey(sid) {
		t.Errorf("marker = (%q, %q), want (ready, canonical key)", state, key)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestCaptureSkipsRootsTheSessionNeverUsed: ErrFileNotExist from Export is the
// normal shape for an unused root, not a failure.
func TestCaptureSkipsRootsTheSessionNeverUsed(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	h.prov.owned = []domain.ID{h.sid}
	h.prov.exports = map[string][]byte{
		"/workspace": exportTar(t, map[string]string{"workspace/only.txt": "x"}, nil),
	}
	if err := h.exec.captureCheckpoint(context.Background(), h.sid); err != nil {
		t.Fatalf("capture: %v", err)
	}
	got := checkpointMembers(t, h)
	if _, ok := got["workspace/only.txt"]; !ok || len(got) != 1 {
		t.Errorf("members = %v, want exactly workspace/only.txt", keys(got))
	}
}

// TestCaptureOverBudgetFailsWithoutMarker: the budget is a hard stop — no
// blob, no marker, and the sentinel error the TTL tier degrades on.
func TestCaptureOverBudgetFailsWithoutMarker(t *testing.T) {
	h := newHarnessWith(t, &fakeProvider{sb: &fakeSandbox{}}, Config{CheckpointMaxBytes: 8})
	h.prov = h.exec.provider.(*fakeProvider)
	h.prov.owned = []domain.ID{h.sid}
	h.prov.exports = map[string][]byte{
		"/workspace": exportTar(t, map[string]string{"workspace/big.bin": "far more than eight bytes"}, nil),
	}
	err := h.exec.captureCheckpoint(context.Background(), h.sid)
	if !errors.Is(err, ErrCheckpointTooLarge) {
		t.Fatalf("capture over budget: %v, want ErrCheckpointTooLarge", err)
	}
	if state, _ := markerState(t, h); state != "" {
		t.Errorf("marker written on a failed capture: %q", state)
	}
	if _, _, err := h.blobs.Get(context.Background(), blob.SessionCheckpointKey(h.sid.String())); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("blob written on a failed capture: %v", err)
	}
}

// TestCaptureRejectsAnEscapingMember: a member outside the export's top-level
// directory (or dot-dot) aborts the capture — never silently reshaped.
func TestCaptureRejectsAnEscapingMember(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	h.prov.owned = []domain.ID{h.sid}
	for name, tarBytes := range map[string][]byte{
		"dot-dot":     exportTar(t, map[string]string{"../evil": "x"}, nil),
		"foreign-top": exportTar(t, map[string]string{"other/file": "x"}, nil),
	} {
		h.prov.exports = map[string][]byte{"/workspace": tarBytes}
		if err := h.exec.captureCheckpoint(context.Background(), h.sid); err == nil {
			t.Errorf("%s member accepted", name)
		}
		if state, _ := markerState(t, h); state != "" {
			t.Errorf("%s: marker written on a failed capture", name)
		}
	}
}

// TestCaptureExportFailureAborts: a root that exists but cannot be read is a
// failed capture — a partial checkpoint restored later would silently lose
// state.
func TestCaptureExportFailureAborts(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	h.prov.owned = []domain.ID{h.sid}
	h.prov.exportErr = errors.New("daemon hiccup")
	if err := h.exec.captureCheckpoint(context.Background(), h.sid); err == nil {
		t.Fatal("capture succeeded with a failing export")
	}
	if state, _ := markerState(t, h); state != "" {
		t.Error("marker written on a failed capture")
	}
}

// checkpointBlob seeds the session's checkpoint blob with the given members
// and returns its key.
func checkpointBlob(t *testing.T, h *harness, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for p, content := range files {
		hdr := &tar.Header{Name: p, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))}
		if strings.HasSuffix(p, "/") {
			hdr.Typeflag = tar.TypeDir
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(content)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	key := blob.SessionCheckpointKey(h.sid.String())
	if err := h.blobs.Put(context.Background(), key, bytes.NewReader(buf.Bytes()), int64(buf.Len()), "application/gzip"); err != nil {
		t.Fatal(err)
	}
	return key
}

// TestProvisionRestoresAReadyCheckpoint: a ready marker makes provision
// replace whatever exists (reap before provision), ship the tar in, extract
// it in-sandbox, and flip the marker consumed — the full D6 path.
func TestProvisionRestoresAReadyCheckpoint(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	key := checkpointBlob(t, h, map[string]string{"workspace/prev.txt": "from before the reap"})
	setMarker(t, h, "ready", key)

	if _, err := h.exec.provisionSandbox(context.Background(), h.sid,
		sessionRun{networking: domain.Networking{Type: domain.NetUnrestricted}}); err != nil {
		t.Fatalf("provision with a ready checkpoint: %v", err)
	}

	if !slices.Contains(h.prov.reapedSnapshot(), h.sid) {
		t.Error("provision did not replace the pre-restore sandbox (no reap)")
	}
	if i, j := indexOf(h.prov.calls, "reap"), indexOf(h.prov.calls, "provision"); i < 0 || j < 0 || i > j {
		t.Errorf("call order = %v, want reap before provision", h.prov.calls)
	}
	shipped, ok := sb.files[restoreTarPath]
	if !ok {
		t.Fatal("no tar shipped to the sandbox")
	}
	tr := tar.NewReader(strings.NewReader(shipped))
	hdr, err := tr.Next()
	if err != nil || hdr.Name != "workspace/prev.txt" {
		t.Errorf("shipped tar first member = %v (%v), want workspace/prev.txt", hdr, err)
	}
	var extracted bool
	for _, cmd := range sb.cmds {
		if strings.Contains(cmd, "tar -xf "+restoreTarPath) && strings.Contains(cmd, "-C /") {
			extracted = true
		}
	}
	if !extracted {
		t.Errorf("no in-sandbox extraction ran; cmds = %v", sb.cmds)
	}
	if state, _ := markerState(t, h); state != "consumed" {
		t.Errorf("marker after restore = %q, want consumed", state)
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

// TestProvisionWithoutAReadyMarkerRestoresNothing: no marker and a consumed
// marker are the same case — the cattle path recreates fresh, never rewinds
// (a mid-turn container death must resume on the event log's truth).
func TestProvisionWithoutAReadyMarkerRestoresNothing(t *testing.T) {
	for _, seed := range []string{"none", "consumed"} {
		sb := &fakeSandbox{}
		h := newHarness(t, sb)
		if seed == "consumed" {
			setMarker(t, h, "consumed", checkpointBlob(t, h, map[string]string{"workspace/old.txt": "stale"}))
		}
		if _, err := h.exec.provisionSandbox(context.Background(), h.sid,
			sessionRun{networking: domain.Networking{Type: domain.NetUnrestricted}}); err != nil {
			t.Fatalf("provision (%s): %v", seed, err)
		}
		if len(h.prov.reapedSnapshot()) != 0 {
			t.Errorf("provision (%s) reaped without a ready marker", seed)
		}
		if _, ok := sb.files[restoreTarPath]; ok {
			t.Errorf("provision (%s) shipped a restore tar", seed)
		}
	}
}

// TestRestoreFailureLeavesTheMarkerReady: a failed extraction surfaces and
// leaves `ready` standing, so the next provision replaces the half-restored
// tree and retries (D6's crash rule).
func TestRestoreFailureLeavesTheMarkerReady(t *testing.T) {
	sb := &fakeSandbox{}
	sb.execHook = func(req sandbox.ExecRequest) *sandbox.ExecResult {
		if strings.Contains(req.Command, "tar -xf") {
			return &sandbox.ExecResult{ExitCode: 2, Stderr: "disk full"}
		}
		return nil
	}
	h := newHarness(t, sb)
	setMarker(t, h, "ready", checkpointBlob(t, h, map[string]string{"workspace/prev.txt": "x"}))

	if _, err := h.exec.provisionSandbox(context.Background(), h.sid,
		sessionRun{networking: domain.Networking{Type: domain.NetUnrestricted}}); err == nil {
		t.Fatal("provision succeeded with a failing extraction")
	}
	if state, _ := markerState(t, h); state != "ready" {
		t.Errorf("marker after failed restore = %q, want ready", state)
	}
}

// TestRestoreRejectsATamperedBlob: the spooled tar is re-validated before
// anything ships into the sandbox — an escaping path or a forbidden member
// type aborts with the marker left ready.
func TestRestoreRejectsATamperedBlob(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"escape": {"../etc/passwd": "evil"},
	} {
		sb := &fakeSandbox{}
		h := newHarness(t, sb)
		setMarker(t, h, "ready", checkpointBlob(t, h, files))
		if _, err := h.exec.provisionSandbox(context.Background(), h.sid,
			sessionRun{networking: domain.Networking{Type: domain.NetUnrestricted}}); err == nil {
			t.Fatalf("%s: tampered blob restored", name)
		}
		if _, ok := sb.files[restoreTarPath]; ok {
			t.Errorf("%s: tampered tar reached the sandbox", name)
		}
	}

	// A forbidden member type (a fifo) is tampering too: capture never writes
	// one, so the walk refuses it before anything ships.
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "workspace/pipe", Typeflag: tar.TypeFifo, Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	key := blob.SessionCheckpointKey(h.sid.String())
	if err := h.blobs.Put(context.Background(), key, bytes.NewReader(buf.Bytes()), int64(buf.Len()), "application/gzip"); err != nil {
		t.Fatal(err)
	}
	setMarker(t, h, "ready", key)
	if _, err := h.exec.provisionSandbox(context.Background(), h.sid,
		sessionRun{networking: domain.Networking{Type: domain.NetUnrestricted}}); err == nil {
		t.Fatal("fifo member restored")
	}
	if _, ok := sb.files[restoreTarPath]; ok {
		t.Error("fifo tar reached the sandbox")
	}
}

// TestRestoreOverBudgetFails: the decompressed budget is enforced from actual
// bytes — a blob that inflates past the cap never ships.
func TestRestoreOverBudgetFails(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarnessWith(t, &fakeProvider{sb: sb}, Config{CheckpointMaxBytes: 16})
	h.prov = h.exec.provider.(*fakeProvider)
	setMarker(t, h, "ready", checkpointBlob(t, h, map[string]string{
		"workspace/big.bin": strings.Repeat("A", 64),
	}))
	_, err := h.exec.provisionSandbox(context.Background(), h.sid,
		sessionRun{networking: domain.Networking{Type: domain.NetUnrestricted}})
	if !errors.Is(err, ErrCheckpointTooLarge) {
		t.Fatalf("restore over budget: %v, want ErrCheckpointTooLarge", err)
	}
	if _, ok := sb.files[restoreTarPath]; ok {
		t.Error("over-budget tar reached the sandbox")
	}
}
