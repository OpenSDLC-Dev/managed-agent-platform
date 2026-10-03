package sandbox_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"io/fs"
	gopath "path"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// entry is one member of a built archive, read back the way an untar sees it.
type entry struct {
	name string
	mode int64
	data []byte
}

func readArchive(t *testing.T, b *sandbox.BulkWrite) []entry {
	t.Helper()
	var buf bytes.Buffer
	if err := b.Archive(&buf); err != nil {
		t.Fatalf("build archive: %v", err)
	}
	var out []entry
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read entry %s: %v", h.Name, err)
		}
		if h.Typeflag != tar.TypeReg {
			t.Errorf("entry %s is type %q, want a regular file: an archive carrying a "+
				"directory entry would chmod a directory that already exists", h.Name, h.Typeflag)
		}
		out = append(out, entry{name: h.Name, mode: h.Mode, data: data})
	}
}

// The archive's shape is the contract both backends' untars read: the manifest
// and the directory list first, so a delivery that fails on a later member has
// still landed what the recovery pass needs, then one entry per member under a
// temporary name in its own target's directory — which is what keeps each
// member's rename inside one filesystem, and therefore atomic.
func TestBulkWriteArchiveShape(t *testing.T) {
	files := []sandbox.FileWrite{
		{Path: "/workspace/skills/a/SKILL.md", Data: []byte("skill")},
		{Path: "/workspace/skills/a/scripts/run.sh", Data: []byte("#!/bin/sh\n")},
		{Path: "/workspace/skills/a/empty", Data: nil},
		{Path: "/etc/elsewhere.conf", Data: []byte{0x00, 0xff}},
		{Path: "/mnt/memory/notes/todo.md", Data: []byte("rw"), Mode: 0o666},
	}
	b, err := sandbox.NewBulkWrite("/workspace", files)
	if err != nil {
		t.Fatalf("NewBulkWrite: %v", err)
	}
	got := readArchive(t, b)
	if len(got) != len(files)+2 {
		t.Fatalf("archive has %d entries, want %d (a manifest, a directory list, one per member)",
			len(got), len(files)+2)
	}

	// Entry names are relative, because both untars extract the archive at `/`.
	for _, e := range got {
		if strings.HasPrefix(e.name, "/") {
			t.Errorf("entry name %q is absolute; both untars extract at /", e.name)
		}
	}
	if want := strings.TrimPrefix(b.Manifest, "/"); got[0].name != want {
		t.Errorf("first entry = %q, want the manifest %q", got[0].name, want)
	}
	if want := strings.TrimPrefix(b.DirList, "/"); got[1].name != want {
		t.Errorf("second entry = %q, want the directory list %q", got[1].name, want)
	}
	if !strings.HasPrefix(b.Manifest, "/workspace/"+sandbox.TempPrefix) {
		t.Errorf("manifest %q must be a temporary name in the workdir", b.Manifest)
	}

	// The manifest is `tmp\0target\0mode\0` per member, in order, and each
	// temporary file is in its own target's directory under the shared temp
	// prefix. The mode is the member's own or 0644, in the header and in the
	// manifest alike: the header is what a root untar restores, the manifest
	// what the rename pass chmods after a sandbox user's untar under its umask.
	var wantManifest, wantDirs bytes.Buffer
	for i, f := range files {
		mode := "0644"
		if f.Mode != 0 {
			mode = "0666"
		}
		tmp := got[i+2].name
		if !strings.HasPrefix(tmp, "/") {
			tmp = "/" + tmp
		}
		dir := f.Path[:strings.LastIndex(f.Path, "/")]
		if base := tmp[strings.LastIndex(tmp, "/")+1:]; !strings.HasPrefix(base, sandbox.TempPrefix) {
			t.Errorf("member %d lands at %q, want a %s name", i, tmp, sandbox.TempPrefix)
		}
		if tmp[:strings.LastIndex(tmp, "/")] != dir {
			t.Errorf("member %d lands in %q, want its target's own directory %q",
				i, tmp[:strings.LastIndex(tmp, "/")], dir)
		}
		if !bytes.Equal(got[i+2].data, f.Data) {
			t.Errorf("member %d carries %q, want %q", i, got[i+2].data, f.Data)
		}
		wantMode := int64(0o644)
		if f.Mode != 0 {
			wantMode = int64(f.Mode)
		}
		if got[i+2].mode != wantMode {
			t.Errorf("member %d has mode %o, want %o", i, got[i+2].mode, wantMode)
		}
		wantManifest.WriteString(tmp + "\x00" + f.Path + "\x00" + mode + "\x00")
	}
	if !bytes.Equal(got[0].data, wantManifest.Bytes()) {
		t.Errorf("manifest = %q, want %q", got[0].data, wantManifest.Bytes())
	}
	// The directory list is deduplicated — a skill of ten thousand files in three
	// directories hands `mkdir -p` three arguments, not ten thousand.
	for _, dir := range []string{"/workspace/skills/a", "/workspace/skills/a/scripts", "/etc", "/mnt/memory/notes"} {
		wantDirs.WriteString(dir + "\x00")
	}
	if !bytes.Equal(got[1].data, wantDirs.Bytes()) {
		t.Errorf("directory list = %q, want %q", got[1].data, wantDirs.Bytes())
	}

	// A batch is replayable: a delivery that failed is retried from the same value.
	// The two builds straddle a second boundary on purpose. A tar header's mtime
	// has one-second granularity, so two archives built microseconds apart match
	// even when every entry stamps its own clock — the assertion would hold
	// against the bug it exists to catch, and pin nothing.
	var first, second bytes.Buffer
	if err := b.Archive(&first); err != nil {
		t.Fatalf("re-archive: %v", err)
	}
	now := time.Now()
	time.Sleep(now.Truncate(time.Second).Add(time.Second + 10*time.Millisecond).Sub(now))
	if err := b.Archive(&second); err != nil {
		t.Fatalf("re-archive: %v", err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Error("two archives of one batch differ; a retried delivery must send the same bytes")
	}
}

// Two batches never collide, even into the same directory: every member's
// temporary name carries the batch's own nonce.
func TestBulkWriteNamesAreUnique(t *testing.T) {
	files := []sandbox.FileWrite{
		{Path: "/workspace/a.txt", Data: []byte("a")},
		{Path: "/workspace/b.txt", Data: []byte("b")},
	}
	one, err := sandbox.NewBulkWrite("", files)
	if err != nil {
		t.Fatalf("NewBulkWrite: %v", err)
	}
	two, err := sandbox.NewBulkWrite("", files)
	if err != nil {
		t.Fatalf("NewBulkWrite: %v", err)
	}
	if one.Manifest == two.Manifest || one.DirList == two.DirList {
		t.Errorf("two batches share bookkeeping paths: %q / %q", one.Manifest, two.Manifest)
	}
	// An empty workdir is the sandbox's default, so a caller that never set one
	// still lands its manifest somewhere that exists.
	if !strings.HasPrefix(one.Manifest, sandbox.DefaultWorkdir+"/") {
		t.Errorf("manifest %q, want it under %s", one.Manifest, sandbox.DefaultWorkdir)
	}
	seen := map[string]bool{}
	for _, e := range append(readArchive(t, one), readArchive(t, two)...) {
		if seen[e.name] {
			t.Errorf("two batches both write %q", e.name)
		}
		seen[e.name] = true
	}
}

// A path that is not absolute and clean is refused rather than normalized: it
// also names an entry in the archive, where `..` would mean something else again.
func TestBulkWriteRejectsUnusablePaths(t *testing.T) {
	// A mode that is not permission bits — setuid, a directory bit — is refused
	// before anything is built: a batch carries files and their permissions,
	// nothing a sandbox user could not chmod for itself.
	for _, mode := range []fs.FileMode{fs.ModeSetuid | 0o644, fs.ModeDir | 0o755, fs.ModeSticky} {
		if _, err := sandbox.NewBulkWrite("/workspace", []sandbox.FileWrite{{Path: "/workspace/x", Mode: mode}}); err == nil {
			t.Errorf("mode %v accepted", mode)
		}
	}
	// A NUL would split the path's own manifest record and land the member on a
	// path the caller never named — the one refusal that is about the manifest's
	// framing rather than about the path being usable.
	for _, path := range []string{"", "relative/path", "/a/../b", "/a/b/", "/a//b", "/", ".",
		"/workspace/a\x00b", "/workspace/\x00"} {
		if _, err := sandbox.NewBulkWrite("/workspace", []sandbox.FileWrite{{Path: path}}); err == nil {
			t.Errorf("NewBulkWrite(%q) = nil error, want a refusal", path)
		}
	}
	if _, err := sandbox.NewBulkWrite("/workspace", []sandbox.FileWrite{{Path: "/a/b"}}); err != nil {
		t.Errorf("NewBulkWrite(%q) = %v, want it accepted", "/a/b", err)
	}
}

// What a script exited with becomes the caller's error, naming the member the
// script blamed — the caller handed over a set, so "one of them is a directory"
// is not an answer it can act on.
func TestBulkWriteFault(t *testing.T) {
	files := []sandbox.FileWrite{
		{Path: "/workspace/first", Data: []byte("1")},
		{Path: "/workspace/second", Data: []byte("2")},
	}
	b, err := sandbox.NewBulkWrite("/workspace", files)
	if err != nil {
		t.Fatalf("NewBulkWrite: %v", err)
	}

	err = b.Fault("docker", sandbox.ExitPathIsDirectory, "map-bulk-fail 1\n")
	if !errors.Is(err, sandbox.ErrIsDirectory) {
		t.Errorf("err = %v, want ErrIsDirectory", err)
	}
	if !strings.Contains(err.Error(), "/workspace/second") {
		t.Errorf("err = %v, want it to name the member the script blamed", err)
	}

	// An image's own noise on the same stream must not be mistaken for the
	// marker, and a marker naming a member that does not exist is not one.
	for _, stderr := range []string{
		"", "some image banner\n", "map-bulk-fail\n", "map-bulk-fail x\n",
		"map-bulk-fail 99\n", "map-bulk-fail -1\n",
	} {
		err := b.Fault("docker", sandbox.ExitPathIsDirectory, stderr)
		if !errors.Is(err, sandbox.ErrIsDirectory) {
			t.Errorf("stderr %q: err = %v, want ErrIsDirectory", stderr, err)
		}
		if strings.Contains(err.Error(), "/workspace/first") || strings.Contains(err.Error(), "/workspace/second") {
			t.Errorf("stderr %q: err = %v, want it to name no member at all", stderr, err)
		}
	}
	// The last marker wins: the script prints one, and anything before it is the
	// image's.
	err = b.Fault("docker", sandbox.ExitPathIsDirectory, "map-bulk-fail 1\nmap-bulk-fail 0\n")
	if !strings.Contains(err.Error(), "/workspace/first") {
		t.Errorf("err = %v, want the last marker to win", err)
	}

	err = b.Fault("k8s", sandbox.ExitPathNotDirectory, "mkdir: cannot create '/workspace/x/y': Not a directory")
	if !errors.Is(err, sandbox.ErrNotDirectory) {
		t.Errorf("err = %v, want ErrNotDirectory", err)
	}
	if !strings.Contains(err.Error(), "/workspace/x/y") {
		t.Errorf("err = %v, want mkdir's own message naming the directory", err)
	}

	err = b.Fault("k8s", sandbox.ExitBulkIncomplete, "map-bulk-fail 0\n")
	if err == nil || !strings.Contains(err.Error(), "/workspace/first") {
		t.Errorf("err = %v, want an incomplete-delivery error naming the member", err)
	}
	if errors.Is(err, sandbox.ErrIsDirectory) || errors.Is(err, sandbox.ErrNotDirectory) {
		t.Errorf("err = %v, want no path sentinel: a short delivery is not the path's fault", err)
	}

	err = b.Fault("docker", 1, "mv: cannot move")
	if err == nil || !strings.Contains(err.Error(), "mv: cannot move") {
		t.Errorf("err = %v, want the sandbox's own message carried through", err)
	}

	err = b.Fault("docker", sandbox.ExitPathNotReplaceable, "map-bulk-fail 1\n")
	if !errors.Is(err, sandbox.ErrNotReplaceable) || !strings.Contains(err.Error(), "/workspace/second") {
		t.Errorf("err = %v, want ErrNotReplaceable naming the member", err)
	}

	// Not writable names a member's target, or — `d` and an index — one of the
	// batch's directories, with the shell's own reason after it. The last
	// marker naming one of the batch's own wins, as it does for blamed.
	var pnw *sandbox.PathNotWritableError
	for _, tc := range []struct{ stderr, path, reason string }{
		{"map-bulk-unwritable 1 Read-only file system\n", "/workspace/second", "Read-only file system"},
		{"map-bulk-unwritable d0 Permission denied\n", "/workspace", "Permission denied"},
		{"map-bulk-unwritable 0 Forged\nnoise\nmap-bulk-unwritable 1 Read-only file system\nmap-bulk-unwritable 7 Forged\n",
			"/workspace/second", "Read-only file system"},
	} {
		err := b.Fault("k8s", sandbox.ExitPathNotWritable, tc.stderr)
		if !errors.As(err, &pnw) || pnw.Path != tc.path || pnw.Reason != tc.reason {
			t.Errorf("stderr %q: err = %#v, want %s not writable: %s", tc.stderr, err, tc.path, tc.reason)
		}
	}
	for _, stderr := range []string{"", "map-bulk-unwritable\n", "map-bulk-unwritable x y\n",
		"map-bulk-unwritable 2 z\n", "map-bulk-unwritable d1 z\n", "map-bulk-unwritable -1 z\n"} {
		err := b.Fault("docker", sandbox.ExitPathNotWritable, stderr)
		if !errors.Is(err, sandbox.ErrNotWritable) || errors.As(err, &pnw) {
			t.Errorf("stderr %q: err = %#v, want a bare ErrNotWritable naming no member", stderr, err)
		}
	}

	// Refusal answers only for the three codes the refused pass exits with; for
	// anything else the delivery's own error is the one the caller keeps.
	for _, code := range []int{0, 1, sandbox.ExitPathNotDirectory, sandbox.ExitBulkIncomplete, sandbox.ExitBulkExtract} {
		if err := b.Refusal("docker", code, "map-bulk-fail 0\n"); err != nil {
			t.Errorf("Refusal(%d) = %v, want nil", code, err)
		}
	}
	if err := b.Refusal("docker", sandbox.ExitPathIsDirectory, "map-bulk-fail 0\n"); !errors.Is(err, sandbox.ErrIsDirectory) {
		t.Errorf("Refusal(ExitPathIsDirectory) = %v, want ErrIsDirectory", err)
	}
}

// What a shed pass names it could not remove is resolved against the batch's own
// list, never against the report — so what a backend then empties is always one
// of the paths this batch chose. That is what makes acting on a report read off
// the agent's own filesystem safe: the manifest the shell counted is one the
// sandbox can rewrite (#316).
func TestBulkWriteLeftBehind(t *testing.T) {
	files := []sandbox.FileWrite{
		{Path: "/workspace/first", Data: []byte("1")},
		{Path: "/etc/second", Data: []byte("2")},
	}
	b, err := sandbox.NewBulkWrite("/workspace", files)
	if err != nil {
		t.Fatalf("NewBulkWrite: %v", err)
	}
	tmps := map[string]string{}
	for _, e := range readArchive(t, b)[2:] { // past the manifest and the directory list
		tmps["/"+e.name] = string(e.data)
	}
	if len(tmps) != 2 {
		t.Fatalf("archive carries %d members, want 2", len(tmps))
	}

	left := b.LeftBehind("map-bulk-left-begin\nmap-bulk-left 0\nmap-bulk-left 1\nmap-bulk-left m\nmap-bulk-left d\n")
	if len(left) != 4 {
		t.Fatalf("LeftBehind named %d paths (%v), want all four of the batch's files", len(left), left)
	}
	for _, path := range left[:2] {
		if _, ok := tmps[path]; !ok {
			t.Errorf("LeftBehind named %q, want one of the temporaries the archive carried", path)
		}
	}
	if left[2] != b.Manifest || left[3] != b.DirList {
		t.Errorf("LeftBehind named %q and %q for the bookkeeping, want %q and %q",
			left[2], left[3], b.Manifest, b.DirList)
	}
	// Order follows the report, so a batch that lost only its second member
	// empties only that one.
	if got := b.LeftBehind("map-bulk-left-begin\nmap-bulk-left 1\n"); len(got) != 1 || got[0] != left[1] {
		t.Errorf("LeftBehind = %v, want only member 1's temporary (%q)", got, left[1])
	}

	// An image shares the stream, and a marker naming a member that is not this
	// batch's is not one. None of these may name a path — emptying a file this
	// batch did not put there is the one thing a shed must never do.
	for _, stdout := range []string{
		"", "some image banner\n", "map-bulk-left\n", "map-bulk-left x\n",
		"map-bulk-left 99\n", "map-bulk-left -1\n", "map-bulk-left 2\n",
		"map-bulk-left M\n", "map-bulk-left /etc/passwd\n",
		"not-the-marker map-bulk-left 0\n",
	} {
		if got := b.LeftBehind("map-bulk-left-begin\n" + stdout); len(got) != 0 {
			t.Errorf("stdout %q: LeftBehind = %v, want nothing named", stdout, got)
		}
	}
}

// An image writes to this stream before the shed does — `bash -c` sources an
// `ENV BASH_ENV` file first, the channel #310 measured — so a marker it forges
// would name a member whose `rm` really did succeed, and emptying that puts an
// empty file back exactly where the cleanup had just taken one away. Only what
// follows the shed's own opening line is read, and a stream without one is not a
// report at all (#316).
func TestBulkWriteLeftBehindRefusesWhatTheImagePrinted(t *testing.T) {
	b, err := sandbox.NewBulkWrite("/workspace", []sandbox.FileWrite{
		{Path: "/etc/first", Data: []byte("1")},
		{Path: "/etc/second", Data: []byte("2")},
	})
	if err != nil {
		t.Fatalf("NewBulkWrite: %v", err)
	}

	// The forgery: everything an image can print runs ahead of the script, so a
	// marker there is not the shed's answer and must not be read as one.
	forged := "map-bulk-left 0\nmap-bulk-left 1\nmap-bulk-left m\nmap-bulk-left d\n"
	if got := b.LeftBehind(forged + "map-bulk-left-begin\n"); len(got) != 0 {
		t.Errorf("LeftBehind = %v, want nothing: every marker came before the shed's own report", got)
	}
	// A hook that forges the opening line too still only gets to go first, so
	// the shed's own report is the one that counts — blamed's bound, restated.
	if got := b.LeftBehind("map-bulk-left-begin\n" + forged + "map-bulk-left-begin\nmap-bulk-left 1\n"); len(got) != 1 {
		t.Errorf("LeftBehind = %v, want only the last report's single member", got)
	}
	// No opening line is no report: a shed that never got that far has said
	// nothing, and emptying on an image's say-so is worse than leaving residue.
	if got := b.LeftBehind(forged); len(got) != 0 {
		t.Errorf("LeftBehind = %v, want nothing named without the shed's opening line", got)
	}
	// And the real report still works when the image printed noise first.
	got := b.LeftBehind("some image banner\nmap-bulk-left-begin\nmap-bulk-left 0\n")
	if len(got) != 1 {
		t.Fatalf("LeftBehind = %v, want the one member the shed reported", got)
	}
}

// The stream arrives as frames concatenated with no separator, so an image whose
// output does not end in a newline would absorb the shed's opening line into its
// own last partial one — measured on a real daemon. The marker would stop being
// a line, the image's forged copy would become the last valid one, and the
// framing would hand an attacker exactly what it exists to take away. The shed
// prints a newline in front of its marker for that reason, and this row is the
// regression guard: the shell's output is prefixed the way the shell prefixes it.
func TestBulkWriteLeftBehindSurvivesAnUnterminatedImageLine(t *testing.T) {
	b, err := sandbox.NewBulkWrite("/workspace", []sandbox.FileWrite{
		{Path: "/etc/first", Data: []byte("1")},
		{Path: "/etc/second", Data: []byte("2")},
	})
	if err != nil {
		t.Fatalf("NewBulkWrite: %v", err)
	}

	// An image that forges a whole report and leaves its prompt unterminated,
	// then the shed's own answer as the shell writes it — leading newline first.
	forged := "map-bulk-left-begin\nmap-bulk-left 0\nmap-bulk-left-nolist\n$ "
	shed := "\nmap-bulk-left-begin\nmap-bulk-left 1\n"

	got := b.LeftBehind(forged + shed)
	if len(got) != 1 {
		t.Fatalf("LeftBehind = %v, want only the member the shed itself named", got)
	}
	if b.LostItsList(forged + shed) {
		t.Error("LostItsList = true, want the image's forged nolist to have been framed out")
	}
	// Without the leading newline the forgery wins, which is what the guard is
	// for: this is the measured failure, asserted so removing the newline from
	// the shell brings the row down with it.
	if got := b.LeftBehind(forged + strings.TrimPrefix(shed, "\n")); len(got) == 1 {
		t.Errorf("LeftBehind = %v; an unterminated image line no longer absorbs the "+
			"opening marker, so this row is testing nothing", got)
	}
}

// extracted is one extraction read back the way the daemon's untar sees it: the
// directory it is extracted at, each entry's name in the archive as sent, and
// each entry under its absolute landing path — Dir joined with that name.
type extracted struct {
	dir     string
	names   []string
	entries []entry
}

func readExtractions(t *testing.T, xs []sandbox.Extraction) []extracted {
	t.Helper()
	var out []extracted
	for _, x := range xs {
		var buf bytes.Buffer
		if err := x.Archive(&buf); err != nil {
			t.Fatalf("build the extraction at %s: %v", x.Dir, err)
		}
		got := extracted{dir: x.Dir}
		tr := tar.NewReader(&buf)
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("read the extraction at %s: %v", x.Dir, err)
			}
			if h.Typeflag != tar.TypeReg {
				t.Errorf("entry %s is type %q, want a regular file", h.Name, h.Typeflag)
			}
			data, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("read entry %s: %v", h.Name, err)
			}
			got.names = append(got.names, h.Name)
			got.entries = append(got.entries, entry{name: gopath.Join(x.Dir, h.Name), mode: h.Mode, data: data})
		}
		out = append(out, got)
	}
	return out
}

func extractionDirs(xs []extracted) string {
	dirs := make([]string, len(xs))
	for i, x := range xs {
		dirs[i] = x.dir
	}
	return strings.Join(dirs, " ")
}

// On a read-only root the docker daemon resolves and checks the one directory an
// extraction names, and nothing below it (#859). So there every directory is its
// own extraction and every entry is named by its base name alone: nothing in an
// archive names a directory the daemon did not check, so a symlink the sandbox
// made under a mount cannot carry a member past it. A target that is itself a
// mount point — /tmp — has its temporary in `/`, and that is an extraction too,
// for the daemon to refuse.
func TestBulkWriteExtractsEachDirectoryOnItsOwn(t *testing.T) {
	files := []sandbox.FileWrite{
		{Path: "/workspace/skills/pack/SKILL.md", Data: []byte("skill")},
		{Path: "/tmp/a.txt", Data: []byte("tmp")},
		{Path: "/workspace/skills/pack/scripts/deep/run.sh", Data: []byte("#!/bin/sh\n"), Mode: 0o755},
		{Path: "/mnt/memory/notes/todo.md", Data: []byte("rw"), Mode: 0o666},
		{Path: "/workspace/skills/pack/README.md", Data: []byte("readme")},
		{Path: "/tmp", Data: []byte("onto a mount point")},
	}
	b, err := sandbox.NewBulkWrite("/workspace", files)
	if err != nil {
		t.Fatalf("NewBulkWrite: %v", err)
	}
	xs := readExtractions(t, b.Members(true))
	// In the order each directory first appears in the batch, one each.
	want := "/workspace/skills/pack /tmp /workspace/skills/pack/scripts/deep /mnt/memory/notes /"
	if got := extractionDirs(xs); got != want {
		t.Fatalf("extractions at %s, want %s", got, want)
	}
	// Every member lands exactly where the whole-batch archive lands it — under
	// its temporary name in its target's own directory — with its bytes and its
	// mode, named by its base name alone, in the batch's order.
	whole := readArchive(t, b)[2:] // past the manifest and the directory list
	byPath := map[string]entry{}
	for _, x := range xs {
		for i, e := range x.entries {
			if strings.Contains(x.names[i], "/") {
				t.Errorf("entry %q at %s names a directory; every name is a base name", x.names[i], x.dir)
			}
			byPath[e.name] = e
		}
	}
	for _, w := range whole {
		e, ok := byPath["/"+w.name]
		if !ok {
			t.Errorf("member %s is in no extraction", w.name)
			continue
		}
		if !bytes.Equal(e.data, w.data) || e.mode != w.mode {
			t.Errorf("member %s carries %q mode %o, want %q mode %o", w.name, e.data, e.mode, w.data, w.mode)
		}
	}
	if len(byPath) != len(files) {
		t.Errorf("the extractions carry %d members, want %d", len(byPath), len(files))
	}
	if pack := xs[0].entries; len(pack) != 2 || pack[0].name != "/"+whole[0].name || pack[1].name != "/"+whole[4].name {
		t.Errorf("the pack directory's extraction carries %v, want SKILL.md's then README.md's temporaries", pack)
	}

	// The bookkeeping is one extraction at the workdir, which both files share.
	bk := readExtractions(t, b.Bookkeeping(true))
	if len(bk) != 1 || bk[0].dir != "/workspace" {
		t.Fatalf("bookkeeping extractions at %q, want one at the workdir", extractionDirs(bk))
	}
	if es := bk[0].entries; len(es) != 2 || es[0].name != b.Manifest || es[1].name != b.DirList {
		t.Errorf("bookkeeping carries %+v, want the manifest then the directory list", es)
	}
}

// On a writable root nothing changes from before #859: the bookkeeping is one
// extraction at `/` and the members are another, each the whole tree with every
// entry named by its path made relative — one request each, carrying what the
// whole-batch archive carries.
func TestBulkWriteExtractsTheWholeTreeAtTheRoot(t *testing.T) {
	b, err := sandbox.NewBulkWrite("/workspace", []sandbox.FileWrite{
		{Path: "/workspace/skills/pack/SKILL.md", Data: []byte("skill")},
		{Path: "/workspace/skills/pack/scripts/run.sh", Data: []byte("#!/bin/sh\n"), Mode: 0o755},
		{Path: "/mnt/memory/notes/todo.md", Data: []byte("rw"), Mode: 0o666},
		{Path: "/tmp/a.txt", Data: []byte("tmp")},
	})
	if err != nil {
		t.Fatalf("NewBulkWrite: %v", err)
	}
	bk, members := readExtractions(t, b.Bookkeeping(false)), readExtractions(t, b.Members(false))
	if len(bk) != 1 || bk[0].dir != "/" || len(members) != 1 || members[0].dir != "/" {
		t.Fatalf("bookkeeping at %q and members at %q, want one extraction each, at /",
			extractionDirs(bk), extractionDirs(members))
	}
	whole := readArchive(t, b)
	got := append(append([]string(nil), bk[0].names...), members[0].names...)
	if len(got) != len(whole) {
		t.Fatalf("the two extractions carry %d entries, want the whole archive's %d", len(got), len(whole))
	}
	sent := append(append([]entry(nil), bk[0].entries...), members[0].entries...)
	for i, w := range whole {
		if got[i] != w.name || !bytes.Equal(sent[i].data, w.data) || sent[i].mode != w.mode {
			t.Errorf("entry %d is %q (%d bytes, mode %o), want the whole archive's %q (%d bytes, mode %o)",
				i, got[i], len(sent[i].data), sent[i].mode, w.name, len(w.data), w.mode)
		}
	}
}

// An extraction refuses to build an archive naming anything its shape does not
// allow — below its directory, beside it, above it, the directory itself, a path
// that is not absolute and clean, or a whole tree anywhere but `/` — and it
// checks before writing the first header, so an extraction that fails sends the
// daemon nothing at all, not even the entries ahead of the one that failed.
func TestExtractionRefusesAnEntryOutsideItsDirectory(t *testing.T) {
	for _, tc := range []struct {
		dir  string
		tree bool
		ok   string
		bad  string
	}{
		{"/workspace/skills/pack", false, "/workspace/skills/pack/ok", "/workspace/skills/pack/scripts/run.sh"},
		{"/workspace/skills/pack", false, "/workspace/skills/pack/ok", "/workspace/skills/other/x"},
		{"/workspace/skills/pack", false, "/workspace/skills/pack/ok", "/workspace/skills/pack"},
		{"/workspace/skills/pack", false, "/workspace/skills/pack/ok", "/workspace/skills/x"},
		{"/workspace", false, "/workspace/ok", "/workspace/../etc/x"},
		{"/workspace", false, "/workspace/ok", "workspace/x"},
		{"/", false, "/ok", "/etc/x"},
		{"/", true, "/a/ok", "/"},
		{"/", true, "/a/ok", "/a/../b"},
		{"/workspace", true, "", "/workspace/x"},
	} {
		paths := []string{tc.bad}
		if tc.ok != "" {
			paths = []string{tc.ok, tc.bad}
		}
		var buf bytes.Buffer
		err := sandbox.ExtractionForTest(tc.dir, tc.tree, paths...).Archive(&buf)
		if err == nil || !strings.Contains(err.Error(), tc.bad) {
			t.Errorf("an extraction at %s (tree %v) carrying %s: err = %v, want a refusal naming it",
				tc.dir, tc.tree, tc.bad, err)
		}
		if buf.Len() != 0 {
			t.Errorf("an extraction at %s carrying %s wrote %d bytes before refusing, want none",
				tc.dir, tc.bad, buf.Len())
		}
	}
	// What fits is named as the daemon must see it.
	for _, tc := range []struct {
		dir, path, name string
		tree            bool
	}{
		{"/", "/x", "x", false},
		{"/workspace/skills/pack", "/workspace/skills/pack/x", "x", false},
		{"/", "/a/b/c", "a/b/c", true},
	} {
		xs := readExtractions(t, []sandbox.Extraction{sandbox.ExtractionForTest(tc.dir, tc.tree, tc.path)})
		if len(xs[0].names) != 1 || xs[0].names[0] != tc.name {
			t.Errorf("%s at %s (tree %v) is named %v, want %q", tc.path, tc.dir, tc.tree, xs[0].names, tc.name)
		}
	}
}

// The emptying puts the names back without the payloads, one zero-byte entry per
// path, cut as the deliveries were: on a read-only root one extraction per
// directory a left-behind path sits in, so the daemon checks each directory it
// empties into; on a writable root one at `/`, as before #859.
func TestBulkWriteEmptying(t *testing.T) {
	b, err := sandbox.NewBulkWrite("/workspace", []sandbox.FileWrite{
		{Path: "/mnt/memory/notes/first", Data: bytes.Repeat([]byte("A"), 4096)},
		{Path: "/mnt/memory/notes/second", Data: bytes.Repeat([]byte("B"), 4096)},
		{Path: "/workspace/skills/pack/third", Data: bytes.Repeat([]byte("C"), 4096)},
	})
	if err != nil {
		t.Fatalf("NewBulkWrite: %v", err)
	}
	left := b.LeftBehind("map-bulk-left-begin\nmap-bulk-left 0\nmap-bulk-left m\nmap-bulk-left 2\nmap-bulk-left 1\n")
	if len(left) != 4 {
		t.Fatalf("LeftBehind = %v, want four paths", left)
	}

	perDir := readExtractions(t, b.Emptying(left, true))
	if want := "/mnt/memory/notes /workspace /workspace/skills/pack"; extractionDirs(perDir) != want {
		t.Fatalf("emptying extractions at %s, want %s", extractionDirs(perDir), want)
	}
	var names []string
	for _, x := range perDir {
		for i, e := range x.entries {
			if len(e.data) != 0 {
				t.Errorf("entry %s is %d bytes, want 0: the payload is the whole point", e.name, len(e.data))
			}
			if x.names[i] != gopath.Base(e.name) {
				t.Errorf("entry %q at %s, want its base name", x.names[i], x.dir)
			}
			names = append(names, e.name)
		}
	}
	if want := []string{left[0], left[3], left[1], left[2]}; strings.Join(names, " ") != strings.Join(want, " ") {
		t.Errorf("the emptying carries %v, want exactly what was named, by directory: %v", names, want)
	}

	whole := readExtractions(t, b.Emptying(left, false))
	if len(whole) != 1 || whole[0].dir != "/" {
		t.Fatalf("emptying on a writable root at %q, want one extraction at /", extractionDirs(whole))
	}
	for i, e := range whole[0].entries {
		if e.name != left[i] || len(e.data) != 0 {
			t.Errorf("entry %d empties %s (%d bytes), want %s at 0 bytes", i, e.name, len(e.data), left[i])
		}
	}

	// Nothing named is nothing to send; the caller is what decides not to.
	if xs := b.Emptying(nil, true); len(xs) != 0 {
		t.Errorf("an empty list built %d extractions, want none", len(xs))
	}
	if xs := b.Emptying(nil, false); len(xs) != 0 {
		t.Errorf("an empty list built %d extractions at /, want none", len(xs))
	}
}
