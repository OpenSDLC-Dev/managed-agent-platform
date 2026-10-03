package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/ripgrep"
)

// server answers the two archive names with bodies of its own and counts the
// requests it took.
func server(t *testing.T, bodies map[string]string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, ok := bodies[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func manifest(base string, pins map[string]string) ripgrep.Manifest {
	m := ripgrep.Manifest{Version: "9.9.9", Archives: map[string]ripgrep.Archive{}}
	for arch, pin := range pins {
		m.Archives[arch] = ripgrep.Archive{URL: base + "/rg-" + arch + ".tar.gz", SHA256: pin}
	}
	return m
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func quiet(string, ...any) {}

// A fetch lands each pinned archive once, leaves an archive that is already
// the pin alone, and sweeps what the pin no longer names — an older archive,
// an interrupted download — while keeping the manifest and anything else.
func TestFetchLandsThePinAndOnlyThePin(t *testing.T) {
	backoff = 0
	srv, hits := server(t, map[string]string{"rg-amd64.tar.gz": "amd64 bytes", "rg-arm64.tar.gz": "arm64 bytes"})
	m := manifest(srv.URL, map[string]string{"amd64": sum("amd64 bytes"), "arm64": sum("arm64 bytes")})
	dir := t.TempDir()
	for _, f := range []string{"manifest.json", "ripgrep-1.0.0-old.tar.gz", partPrefix + "123", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := fetch(context.Background(), srv.Client(), m, dir, quiet); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got, want := names(t, dir), []string{"manifest.json", "notes.txt", "rg-amd64.tar.gz", "rg-arm64.tar.gz"}; !slices.Equal(got, want) {
		t.Fatalf("assets = %v, want %v", got, want)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "rg-arm64.tar.gz")); string(b) != "arm64 bytes" {
		t.Errorf("arm64 archive = %q", b)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "rg-amd64.tar.gz")); fi.Mode().Perm() != 0o644 {
		t.Errorf("archive mode = %v, want 0644", fi.Mode().Perm())
	}
	before := hits.Load()
	if err := fetch(context.Background(), srv.Client(), m, dir, quiet); err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if hits.Load() != before {
		t.Errorf("a second fetch downloaded again: %d requests", hits.Load()-before)
	}
}

// Bytes that are not the pin never land, under the archive's name or any
// other, and a file already there under the name is replaced only by the pin.
func TestFetchRefusesBytesThatAreNotThePin(t *testing.T) {
	backoff = 0
	srv, hits := server(t, map[string]string{"rg-amd64.tar.gz": "tampered"})
	dir := t.TempDir()
	stale := filepath.Join(dir, "rg-amd64.tar.gz")
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := fetch(context.Background(), srv.Client(), manifest(srv.URL, map[string]string{"amd64": sum("genuine")}), dir, quiet)
	if err == nil || !strings.Contains(err.Error(), "does not match its pinned sha256 "+sum("genuine")) {
		t.Fatalf("fetch = %v, want the digest refusal", err)
	}
	if hits.Load() != 1 {
		t.Errorf("a wrong digest was fetched %d times; the same bytes come back every time", hits.Load())
	}
	if b, _ := os.ReadFile(stale); string(b) != "old" {
		t.Errorf("the refused download replaced the file: %q", b)
	}
	if got := names(t, dir); !slices.Equal(got, []string{"rg-amd64.tar.gz"}) {
		t.Errorf("assets = %v, want nothing but what was there", got)
	}
}

// A download that does not arrive is tried again, and a fetch that never gets
// one fails rather than building without the binary.
func TestFetchRetriesAndThenFails(t *testing.T) {
	backoff = 0
	srv, hits := server(t, map[string]string{})
	err := fetch(context.Background(), srv.Client(), manifest(srv.URL, map[string]string{"amd64": sum("x")}), t.TempDir(), quiet)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("fetch = %v, want the 404", err)
	}
	if hits.Load() != attempts {
		t.Errorf("requests = %d, want %d", hits.Load(), attempts)
	}
}
