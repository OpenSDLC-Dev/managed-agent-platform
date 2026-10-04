package sandboxtest

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
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
