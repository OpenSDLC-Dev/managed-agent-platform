//go:build unix

package main

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO at an archive's name, or a link to one, is no archive: it is removed
// and the archive fetched, and never opened — an open of a FIFO waits for a
// writer that never comes, and the fetch, and the build, would wait with it.
// What the link named is left as it was.
func TestFetchNeverOpensAFIFO(t *testing.T) {
	backoff = 0
	srv, hits := server(t, map[string]string{"rg-amd64.tar.gz": "amd64 bytes"})
	m := manifest(srv.URL, map[string]string{"amd64": sum("amd64 bytes")})
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, plant := range map[string]func(dst string) error{
		"a FIFO":           func(dst string) error { return syscall.Mkfifo(dst, 0o644) },
		"a link to a FIFO": func(dst string) error { return os.Symlink(fifo, dst) },
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			dst := filepath.Join(dir, "rg-amd64.tar.gz")
			if err := plant(dst); err != nil {
				t.Fatal(err)
			}
			before := hits.Load()
			done := make(chan error, 1)
			go func() { done <- fetch(context.Background(), srv.Client(), m, dir, quiet) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("fetch: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the fetch is blocked opening the FIFO")
			}
			if got := hits.Load() - before; got != 1 {
				t.Errorf("downloads = %d, want 1", got)
			}
			fi, err := os.Lstat(dst)
			if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o644 {
				t.Fatalf("%s = %v, %v; want a regular 0644 file", dst, fi, err)
			}
			if b, _ := os.ReadFile(dst); string(b) != "amd64 bytes" {
				t.Errorf("archive = %q, want the pin", b)
			}
			if fi, err := os.Lstat(fifo); err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
				t.Errorf("what the link named = %v, %v; want the FIFO it was", fi, err)
			}
		})
	}
}
