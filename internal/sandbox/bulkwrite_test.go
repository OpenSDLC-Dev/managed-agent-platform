package sandbox_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

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

	// The manifest is `tmp\0target\0` per member, in order, and each temporary
	// file is in its own target's directory under the shared temp prefix.
	var wantManifest, wantDirs bytes.Buffer
	for i, f := range files {
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
		if got[i+2].mode != 0o644 {
			t.Errorf("member %d has mode %o, want 644", i, got[i+2].mode)
		}
		wantManifest.WriteString(tmp + "\x00" + f.Path + "\x00")
	}
	if !bytes.Equal(got[0].data, wantManifest.Bytes()) {
		t.Errorf("manifest = %q, want %q", got[0].data, wantManifest.Bytes())
	}
	// The directory list is deduplicated — a skill of ten thousand files in three
	// directories hands `mkdir -p` three arguments, not ten thousand.
	for _, dir := range []string{"/workspace/skills/a", "/workspace/skills/a/scripts", "/etc"} {
		wantDirs.WriteString(dir + "\x00")
	}
	if !bytes.Equal(got[1].data, wantDirs.Bytes()) {
		t.Errorf("directory list = %q, want %q", got[1].data, wantDirs.Bytes())
	}

	// A batch is replayable: a delivery that failed is retried from the same value.
	var first, second bytes.Buffer
	if err := b.Archive(&first); err != nil {
		t.Fatalf("re-archive: %v", err)
	}
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
	for _, path := range []string{"", "relative/path", "/a/../b", "/a/b/", "/a//b", "/", "."} {
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
