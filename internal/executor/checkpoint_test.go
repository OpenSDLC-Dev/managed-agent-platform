package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

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
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ""
	}
	if err != nil {
		t.Fatalf("read checkpoint marker: %v", err)
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

// TestCaptureAndRestoreShareOneBudget: both sides meter the same framed tar
// stream, so any capture the budget accepts is arithmetically restorable under
// the same cap. 3600 content bytes under a 4096 cap fit by content but not by
// framing (a 512-byte header plus padding to a block boundary), so capture
// must refuse them — metering content alone would accept the capture and wedge
// the session at restore forever.
func TestCaptureAndRestoreShareOneBudget(t *testing.T) {
	over := newHarnessWith(t, &fakeProvider{sb: &fakeSandbox{}}, Config{CheckpointMaxBytes: 4096})
	over.prov = over.exec.provider.(*fakeProvider)
	over.prov.owned = []domain.ID{over.sid}
	over.prov.exports = map[string][]byte{
		"/workspace": exportTar(t, map[string]string{"workspace/big.bin": strings.Repeat("A", 3600)}, nil),
	}
	if err := over.exec.captureCheckpoint(context.Background(), over.sid); !errors.Is(err, ErrCheckpointTooLarge) {
		t.Fatalf("capture whose framing exceeds the budget: %v, want ErrCheckpointTooLarge", err)
	}

	// And the round trip: what capture accepts, restore replays under the
	// same cap.
	sb := &fakeSandbox{}
	fits := newHarnessWith(t, &fakeProvider{sb: sb}, Config{CheckpointMaxBytes: 4096})
	fits.prov = fits.exec.provider.(*fakeProvider)
	fits.prov.owned = []domain.ID{fits.sid}
	fits.prov.exports = map[string][]byte{
		"/workspace": exportTar(t, map[string]string{"workspace/f.txt": strings.Repeat("A", 100)}, nil),
	}
	if err := fits.exec.captureCheckpoint(context.Background(), fits.sid); err != nil {
		t.Fatalf("capture within budget: %v", err)
	}
	if _, err := fits.exec.provisionSandbox(context.Background(), fits.sid,
		sessionRun{networking: domain.Networking{Type: domain.NetUnrestricted}}, func() {}); err != nil {
		t.Fatalf("restore of an accepted capture under the same budget: %v", err)
	}
	if state, _ := markerState(t, fits); state != "consumed" {
		t.Errorf("marker after the round trip = %q, want consumed", state)
	}
}

// TestCaptureSurfacesAStreamErrorAfterTheArchive: a K8s export's tar can exit
// non-zero after streaming a syntactically complete archive — the pipe's close
// error arrives only past the end-of-archive blocks the tar walk stops at.
// Capture must drain the stream and fail, not mark a possibly-partial export
// ready.
func TestCaptureSurfacesAStreamErrorAfterTheArchive(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	h.prov.owned = []domain.ID{h.sid}
	h.prov.exports = map[string][]byte{
		"/workspace": exportTar(t, map[string]string{"workspace/f.txt": "x"}, nil),
	}
	h.prov.exportTrailErr = errors.New("tar exited 1")
	if err := h.exec.captureCheckpoint(context.Background(), h.sid); err == nil {
		t.Fatal("capture succeeded over an export stream that failed after its archive")
	}
	if state, _ := markerState(t, h); state != "" {
		t.Errorf("marker written on a failed capture: %q", state)
	}
}

// TestRecaptureRearmsAConsumedMarker: an idle reap after a restore captures
// again — the consumed marker flips back to ready and the blob is the fresh
// capture, not the one the last restore consumed.
func TestRecaptureRearmsAConsumedMarker(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	h.prov.owned = []domain.ID{h.sid}
	setMarker(t, h, "consumed", checkpointBlob(t, h, map[string]string{"workspace/old.txt": "stale"}))
	h.prov.exports = map[string][]byte{
		"/workspace": exportTar(t, map[string]string{"workspace/new.txt": "fresh"}, nil),
	}
	if err := h.exec.captureCheckpoint(context.Background(), h.sid); err != nil {
		t.Fatalf("re-capture: %v", err)
	}
	if state, _ := markerState(t, h); state != "ready" {
		t.Errorf("marker after re-capture = %q, want ready", state)
	}
	got := checkpointMembers(t, h)
	if _, ok := got["workspace/new.txt"]; !ok {
		t.Errorf("blob not replaced by the re-capture: %v", keys(got))
	}
	if _, ok := got["workspace/old.txt"]; ok {
		t.Error("the consumed blob's members survived the re-capture")
	}
}

// TestCaptureAcceptsANonDirectoryRoot: an agent can replace a root with a
// plain file. The root member is then written under its bare path — a
// trailing slash on a non-directory member would be a malformed archive.
func TestCaptureAcceptsANonDirectoryRoot(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	h.prov.owned = []domain.ID{h.sid}
	h.prov.exports = map[string][]byte{
		outputsDir: exportTar(t, map[string]string{"outputs": "i am a file now"}, nil),
	}
	if err := h.exec.captureCheckpoint(context.Background(), h.sid); err != nil {
		t.Fatalf("capture: %v", err)
	}
	got := checkpointMembers(t, h)
	if got["mnt/session/outputs"] != "i am a file now" {
		t.Errorf("members = %v, want mnt/session/outputs as a regular file", keys(got))
	}
	if _, ok := got["mnt/session/outputs/"]; ok {
		t.Error("non-directory root written with a trailing slash")
	}
}

// gnuLongNameTar hand-crafts what busybox/GNU tar (the K8s export path)
// streams for a path longer than ustar's 100-byte name field: a ././@LongLink
// pseudo-member carrying the full name, then the real member under the
// truncated name. Go's tar.Writer never produces this shape, so the fixture is
// raw blocks.
func gnuLongNameTar(t *testing.T, name, content string) []byte {
	t.Helper()
	octal := func(b []byte, v int64) {
		copy(b, fmt.Sprintf("%0*o", len(b)-1, v))
		b[len(b)-1] = 0
	}
	header := func(name string, typeflag byte, size int64) []byte {
		blk := make([]byte, 512)
		copy(blk[0:100], name)
		octal(blk[100:108], 0o644)
		octal(blk[108:116], 0)
		octal(blk[116:124], 0)
		octal(blk[124:136], size)
		octal(blk[136:148], 0)
		for i := 148; i < 156; i++ {
			blk[i] = ' '
		}
		blk[156] = typeflag
		copy(blk[257:265], "ustar  \x00")
		var sum int64
		for _, c := range blk {
			sum += int64(c)
		}
		copy(blk[148:155], fmt.Sprintf("%06o", sum))
		blk[154] = 0
		blk[155] = ' '
		return blk
	}
	pad := func(b []byte) []byte {
		if r := len(b) % 512; r != 0 {
			b = append(b, make([]byte, 512-r)...)
		}
		return b
	}
	var buf bytes.Buffer
	buf.Write(header("././@LongLink", 'L', int64(len(name)+1)))
	buf.Write(pad(append([]byte(name), 0)))
	buf.Write(header(name[:100], '0', int64(len(content))))
	buf.Write(pad([]byte(content)))
	buf.Write(make([]byte, 1024))
	return buf.Bytes()
}

// TestCaptureCarriesAGNULongNameExport: the K8s export path is GNU tar, whose
// long names ride LongLink pseudo-members. The re-rooting walk must carry the
// full name through — and never surface the pseudo-member itself.
func TestCaptureCarriesAGNULongNameExport(t *testing.T) {
	longName := "outputs/" + strings.Repeat("n", 150) + ".bin"
	h := newHarness(t, &fakeSandbox{})
	h.prov.owned = []domain.ID{h.sid}
	h.prov.exports = map[string][]byte{
		outputsDir: gnuLongNameTar(t, longName, "deliverable"),
	}
	if err := h.exec.captureCheckpoint(context.Background(), h.sid); err != nil {
		t.Fatalf("capture of a GNU long-name export: %v", err)
	}
	got := checkpointMembers(t, h)
	if want := "mnt/session/" + longName; got[want] != "deliverable" {
		t.Errorf("member %q missing or wrong; members = %v", want, keys(got))
	}
	for name := range got {
		if strings.Contains(name, "@LongLink") {
			t.Errorf("GNU pseudo-member %q leaked into the checkpoint", name)
		}
	}
}

// TestCaptureCarriesAUSTARDeepPathExport: a strict-ustar export encodes a deep
// path via the 155-byte prefix split, and re-rooting can push the name past
// what any USTAR split carries (max 256). The walk resets the detected format
// so the writer re-picks (PAX) instead of aborting the capture on a
// cannot-encode error.
func TestCaptureCarriesAUSTARDeepPathExport(t *testing.T) {
	deep := "outputs/" + strings.Repeat("a", 145) + "/" + strings.Repeat("b", 95)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name: deep, Typeflag: tar.TypeReg, Mode: 0o644, Size: 1, Format: tar.FormatUSTAR,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	h := newHarness(t, &fakeSandbox{})
	h.prov.owned = []domain.ID{h.sid}
	h.prov.exports = map[string][]byte{outputsDir: buf.Bytes()}
	if err := h.exec.captureCheckpoint(context.Background(), h.sid); err != nil {
		t.Fatalf("capture of a ustar deep-path export: %v", err)
	}
	got := checkpointMembers(t, h)
	if want := "mnt/session/" + deep; got[want] != "x" {
		t.Errorf("member %q missing; members = %v", want, keys(got))
	}
}

// TestProvisionFailsClosedOnAReadyMarkerWithoutBlobs: an executor deployed
// without an object store must refuse to provision a session whose checkpoint
// is ready — silently continuing would hand the agent an empty workspace while
// its state sits unrestorable in the blob it cannot reach.
func TestProvisionFailsClosedOnAReadyMarkerWithoutBlobs(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	setMarker(t, h, "ready", checkpointBlob(t, h, map[string]string{"workspace/prev.txt": "x"}))
	bare := New(h.pool, h.log, h.queue, h.prov, nil, h.cipher, Config{})
	_, err := bare.provisionSandbox(context.Background(), h.sid,
		sessionRun{networking: domain.Networking{Type: domain.NetUnrestricted}}, func() {})
	if err == nil || !strings.Contains(err.Error(), "no object store") {
		t.Fatalf("err = %v, want the no-object-store refusal", err)
	}
	if len(h.prov.reapedSnapshot()) != 0 || indexOf(h.prov.calls, "provision") >= 0 {
		t.Errorf("sandbox touched before failing closed: calls = %v", h.prov.calls)
	}
}

// TestValidateWorkdir: the startup guard refuses a workdir that would alias
// the checkpoint's other roots or its own machinery — in either direction.
func TestValidateWorkdir(t *testing.T) {
	for workdir, wantErr := range map[string]bool{
		"":                               false,
		"/workspace":                     false,
		"/srv/agent":                     false,
		"relative":                       true,
		"/":                              true,
		"/tmp":                           true,
		"/tmp/work":                      true,
		sandbox.ShellStateRoot:           true,
		sandbox.ShellStateRoot + "/deep": true,
		"/mnt/session/workdir":           true,
		"/mnt":                           true, // parent of /mnt/session captures it wholesale
		"/var/lib":                       true, // parent of the shell-state root
	} {
		if err := ValidateWorkdir(workdir); (err != nil) != wantErr {
			t.Errorf("ValidateWorkdir(%q) = %v, want error %v", workdir, err, wantErr)
		}
	}
}

// TestCaptureRejectsAnEscapingMember: a member outside the export's top-level
// directory (absolute, or dot-dot) aborts the capture — never silently
// reshaped.
func TestCaptureRejectsAnEscapingMember(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	h.prov.owned = []domain.ID{h.sid}
	for name, tarBytes := range map[string][]byte{
		"dot-dot":     exportTar(t, map[string]string{"../evil": "x"}, nil),
		"absolute":    exportTar(t, map[string]string{"/etc/evil": "x"}, nil),
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
		sessionRun{networking: domain.Networking{Type: domain.NetUnrestricted}}, func() {}); err != nil {
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
			sessionRun{networking: domain.Networking{Type: domain.NetUnrestricted}}, func() {}); err != nil {
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
		sessionRun{networking: domain.Networking{Type: domain.NetUnrestricted}}, func() {}); err == nil {
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
		"escape":   {"../etc/passwd": "evil"},
		"absolute": {"/etc/passwd": "evil"},
		// Relative but outside every durable root: extraction runs `tar -C /`,
		// so without the root restriction this would land on the real /etc.
		"outside-roots": {"etc/profile": "evil"},
	} {
		sb := &fakeSandbox{}
		h := newHarness(t, sb)
		setMarker(t, h, "ready", checkpointBlob(t, h, files))
		if _, err := h.exec.provisionSandbox(context.Background(), h.sid,
			sessionRun{networking: domain.Networking{Type: domain.NetUnrestricted}}, func() {}); err == nil {
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
		sessionRun{networking: domain.Networking{Type: domain.NetUnrestricted}}, func() {}); err == nil {
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
		sessionRun{networking: domain.Networking{Type: domain.NetUnrestricted}}, func() {})
	if !errors.Is(err, ErrCheckpointTooLarge) {
		t.Fatalf("restore over budget: %v, want ErrCheckpointTooLarge", err)
	}
	if _, ok := sb.files[restoreTarPath]; ok {
		t.Error("over-budget tar reached the sandbox")
	}
}

// TestRestoreReportsBetweenItsSteps: a restore is not one step. It fetches the
// workspace from object storage, validates the spooled tar, ships it into the
// sandbox and extracts it there, and then consumes the marker — five round
// trips, four of them able to take real time on a large workspace over a distant
// bucket, and `EXECUTOR_CHECKPOINT_MAX_BYTES` is operator-raisable with no
// relation to the stall budget. Reported only on return, the whole restore was
// one silent interval: a budget that a single step clears comfortably would
// cancel the sum of them, the marker would deliberately stay `ready` (D6's crash
// rule), and every reclaim would reap the sandbox and restore again from zero
// (#383).
func TestRestoreReportsBetweenItsSteps(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	setMarker(t, h, "ready", checkpointBlob(t, h, map[string]string{"workspace/prev.txt": "x"}))

	var reports int
	if _, err := h.exec.provisionSandbox(context.Background(), h.sid,
		sessionRun{networking: domain.Networking{Type: domain.NetUnrestricted}},
		func() { reports++ }); err != nil {
		t.Fatalf("provisionSandbox: %v", err)
	}
	// Three provisioning steps (credentials, session lock, sandbox) plus the
	// pre-restore reap, then the restore's own four: fetched and spooled,
	// validated, shipped into the sandbox, extracted — the last of them landing
	// before the marker update, which is a round trip of its own.
	if reports != 8 {
		t.Errorf("provision-with-restore reports = %d, want 8 (4 provisioning steps + 4 restore steps)", reports)
	}
}
