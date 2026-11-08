package sandboxtest

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// archiveIndex names the import record `kind load` leaves for an archive by
// its index.json, and answers "" — a load whose record cannot be named, not a
// failure — for the archive a Docker before 25 saves, which has none.
func TestArchiveIndex(t *testing.T) {
	archive := func(files map[string]string) string {
		path := filepath.Join(t.TempDir(), "image.tar")
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		tw := tar.NewWriter(f)
		for name, body := range files {
			if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		return path
	}
	index := `{"schemaVersion":2}`
	sum := sha256.Sum256([]byte(index))
	if got, err := archiveIndex(archive(map[string]string{"manifest.json": "[]", "index.json": index})); err != nil || got != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Errorf("an archive with an index = %q, %v; want its digest", got, err)
	}
	if got, err := archiveIndex(archive(map[string]string{"manifest.json": "[]", "repositories": "{}"})); err != nil || got != "" {
		t.Errorf("an archive without an index = %q, %v; want \"\", no error", got, err)
	}
	if _, err := archiveIndex(filepath.Join(t.TempDir(), "missing.tar")); err == nil {
		t.Error("a missing archive read without an error")
	}
}

// A load is held to its import record wherever the cluster's kind leaves
// records: the load's own on any node, or another load's, says it does. Only a
// cluster none of whose nodes holds an import record at all is one whose
// loads' records cannot be held (#1025).
func TestKindLoadLeavesRecords(t *testing.T) {
	l := &KindLoad{ref: "docker.io/library/map-hooked-test:aa", index: "sha256:11"}
	own, other := "import-2026-11-08@sha256:11", "import-2026-11-08@sha256:22"
	for _, c := range []struct {
		name     string
		listings [][]string
		want     bool
	}{
		{"no nodes", nil, false},
		{"the tag alone (kind v0.30.0 on node image v1.34.0)", [][]string{{l.ref, "docker.io/library/busybox:1"}}, false},
		{"the tag and its record (kind v0.33.0 on node image v1.37.0)", [][]string{{l.ref, own}}, true},
		{"its record on one node of two", [][]string{{l.ref}, {l.ref, own}}, true},
		{"another load's record, not its own", [][]string{{l.ref, other}}, true},
		{"its record under another prefix", [][]string{{l.ref, "imported-2026-11-08@sha256:11"}}, true},
	} {
		if got := l.leavesRecords(c.listings); got != c.want {
			t.Errorf("%s: leavesRecords = %v, want %v", c.name, got, c.want)
		}
	}
}

// The bash tool's checkpoint shell opens with the header Scripted exempts it
// by: renaming the one without the other would read every bash tool call as a
// platform exec that lost its preamble.
func TestBashToolHeader(t *testing.T) {
	template, err := os.ReadFile(filepath.Join("..", "shell", "template.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !Scripted(string(template)) {
		t.Errorf("the bash tool's template does not open with %q", bashToolHeader)
	}
	for _, c := range []struct {
		command string
		want    bool
	}{
		{sandbox.Script("rm -f -- x"), true},
		{sandbox.NewFrame("probe").Wrap("echo ok"), true},
		{sandbox.NewFrame("search").Open() + "close_frame 0\n", true},
		{strings.Replace(sandbox.NewFrame("probe").Wrap("echo ok"), sandbox.ScriptPreamble, "", 1), false},
		{"rm -f -- x", false},
		{"# checkpoint-shell", false},
	} {
		if got := Scripted(c.command); got != c.want {
			t.Errorf("Scripted(%q) = %v, want %v", c.command, got, c.want)
		}
	}
}
