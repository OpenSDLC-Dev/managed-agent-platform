//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// ufImmutable is UF_IMMUTABLE, <sys/stat.h>'s user immutable flag, which a
// file's owner may set and which makes every chmod of it EPERM.
const ufImmutable = 0x2

// An archive that is the pin and whose mode cannot be changed — a file
// another user owns, a read-only checkout; here one marked immutable — is
// still present: the fetch takes it as it is, downloads nothing, and logs the
// refused chmod rather than failing the build over a mode.
func TestFetchTakesAPresentArchiveItCannotChmod(t *testing.T) {
	backoff = 0
	srv, hits := server(t, map[string]string{})
	m := manifest(srv.URL, map[string]string{"amd64": sum("amd64 bytes")})
	dir := t.TempDir()
	dst := filepath.Join(dir, "rg-amd64.tar.gz")
	if err := os.WriteFile(dst, []byte("amd64 bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Chflags(dst, ufImmutable); err != nil {
		t.Fatalf("chflags: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Chflags(dst, 0) })
	if err := os.Chmod(dst, 0o644); err == nil {
		t.Fatal("an immutable file took a chmod; this test cannot refuse one")
	}
	var logged []string
	logf := func(format string, a ...any) { logged = append(logged, fmt.Sprintf(format, a...)) }
	if err := fetch(context.Background(), srv.Client(), m, dir, logf); err != nil {
		t.Fatalf("fetch over an archive it cannot chmod: %v", err)
	}
	if hits.Load() != 0 {
		t.Errorf("a present archive was downloaded again: %d requests", hits.Load())
	}
	if fi, err := os.Stat(dst); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the archive is %v, %v; want it as it was", fi, err)
	}
	if !strings.Contains(strings.Join(logged, "\n"), dst+" is the pin, left -rw-------: chmod ") {
		t.Errorf("logged %q; want the refused chmod", logged)
	}
}
