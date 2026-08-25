package sandbox_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"io/fs"
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

// The emptying archive puts the names back without the payloads: one zero-byte
// entry per path, relative like every other entry this file builds, so the one
// extraction at `/` covers a whole batch's residue.
func TestBulkWriteEmptyArchive(t *testing.T) {
	b, err := sandbox.NewBulkWrite("/workspace", []sandbox.FileWrite{
		{Path: "/etc/first", Data: bytes.Repeat([]byte("A"), 4096)},
	})
	if err != nil {
		t.Fatalf("NewBulkWrite: %v", err)
	}
	left := b.LeftBehind("map-bulk-left-begin\nmap-bulk-left 0\nmap-bulk-left m\n")

	var buf bytes.Buffer
	if err := b.EmptyArchive(left, &buf); err != nil {
		t.Fatalf("EmptyArchive: %v", err)
	}
	var names []string
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read the emptying archive: %v", err)
		}
		if h.Size != 0 {
			t.Errorf("entry %s is %d bytes, want 0: the payload is the whole point", h.Name, h.Size)
		}
		if h.Typeflag != tar.TypeReg {
			t.Errorf("entry %s is type %q, want a regular file", h.Name, h.Typeflag)
		}
		if strings.HasPrefix(h.Name, "/") {
			t.Errorf("entry %s is absolute, want a relative name the untar extracts at /", h.Name)
		}
		names = append(names, "/"+h.Name)
	}
	if len(names) != 2 || names[0] != left[0] || names[1] != b.Manifest {
		t.Errorf("the emptying archive carries %v, want exactly what was named: %v", names, left)
	}

	// Nothing named is nothing to send; the caller is what decides not to.
	var none bytes.Buffer
	if err := b.EmptyArchive(nil, &none); err != nil {
		t.Fatalf("EmptyArchive(nil): %v", err)
	}
	if _, err := tar.NewReader(&none).Next(); !errors.Is(err, io.EOF) {
		t.Errorf("an empty batch built an archive with entries in it: %v", err)
	}
}
