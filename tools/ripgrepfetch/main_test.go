package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
// the pin alone, and sweeps everything else but the manifest — an older
// archive, an interrupted download, a stray file, a directory, a link, whose
// target it leaves alone — since the embed carries whatever the directory
// holds.
func TestFetchLandsThePinAndOnlyThePin(t *testing.T) {
	backoff = 0
	srv, hits := server(t, map[string]string{"rg-amd64.tar.gz": "amd64 bytes", "rg-arm64.tar.gz": "arm64 bytes"})
	m := manifest(srv.URL, map[string]string{"amd64": sum("amd64 bytes"), "arm64": sum("arm64 bytes")})
	dir, elsewhere := t.TempDir(), t.TempDir()
	for _, f := range []string{"manifest.json", "ripgrep-1.0.0-old.tar.gz", partPrefix + "123", "notes.txt", ".DS_Store",
		"old/ripgrep-1.0.0-old.tar.gz", filepath.Join(elsewhere, "kept")} {
		if !filepath.IsAbs(f) {
			f = filepath.Join(dir, f)
		}
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(elsewhere, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := fetch(context.Background(), srv.Client(), m, dir, quiet); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got, want := names(t, dir), []string{"manifest.json", "rg-amd64.tar.gz", "rg-arm64.tar.gz"}; !slices.Equal(got, want) {
		t.Fatalf("assets = %v, want %v", got, want)
	}
	if got := names(t, elsewhere); !slices.Equal(got, []string{"kept"}) {
		t.Errorf("the link's target holds %v, want what it held", got)
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

// An archive already present as the pin is taken as it is, with no download,
// and left 0644 as a download lands, whatever mode it was put there with: a
// checkout another user builds from must be able to read it.
func TestFetchLeavesAPresentArchive0644(t *testing.T) {
	backoff = 0
	srv, hits := server(t, map[string]string{})
	m := manifest(srv.URL, map[string]string{"amd64": sum("amd64 bytes")})
	for _, mode := range []os.FileMode{0o600, 0o755, 0o644} {
		dir := t.TempDir()
		dst := filepath.Join(dir, "rg-amd64.tar.gz")
		if err := os.WriteFile(dst, []byte("amd64 bytes"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dst, mode); err != nil { // past the umask
			t.Fatal(err)
		}
		if err := fetch(context.Background(), srv.Client(), m, dir, quiet); err != nil {
			t.Fatalf("fetch over a %v archive: %v", mode, err)
		}
		if fi, err := os.Stat(dst); err != nil || fi.Mode().Perm() != 0o644 {
			t.Errorf("a %v archive is %v, %v after the fetch; want 0644", mode, fi.Mode().Perm(), err)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("a present archive was downloaded again: %d requests", hits.Load())
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

// What stands at an archive's name must end a regular file: go:embed skips a
// symbolic link in the directory it embeds, so a link there would build a
// binary with no ripgrep while the fetch said the archive was present. A link
// to a regular file that is the pin becomes a regular copy of it, with no
// download (a FIFO is TestFetchNeverOpensAFIFO's); a
// link to other bytes, a dangling one, and a directory are removed and the
// archive fetched — and what a link named is left as it was.
func TestFetchReplacesWhatIsNotARegularFileAtAnArchivesName(t *testing.T) {
	backoff = 0
	srv, hits := server(t, map[string]string{"rg-amd64.tar.gz": "amd64 bytes"})
	m := manifest(srv.URL, map[string]string{"amd64": sum("amd64 bytes")})
	elsewhere := t.TempDir()
	write := func(p, body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(elsewhere, "pinned"), "amd64 bytes")
	write(filepath.Join(elsewhere, "other"), "other bytes")
	for _, tc := range []struct {
		name      string
		plant     func(dst string)
		downloads int32
	}{
		{"a link to the pin", func(dst string) { _ = os.Symlink(filepath.Join(elsewhere, "pinned"), dst) }, 0},
		{"a link to other bytes", func(dst string) { _ = os.Symlink(filepath.Join(elsewhere, "other"), dst) }, 1},
		{"a dangling link", func(dst string) { _ = os.Symlink(filepath.Join(elsewhere, "absent"), dst) }, 1},
		{"a link to a directory", func(dst string) { _ = os.Symlink(elsewhere, dst) }, 1},
		{"a directory", func(dst string) {
			_ = os.MkdirAll(filepath.Join(dst, "sub"), 0o755)
			write(filepath.Join(dst, "sub", "f"), "x")
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dst := filepath.Join(dir, "rg-amd64.tar.gz")
			tc.plant(dst)
			before := hits.Load()
			if err := fetch(context.Background(), srv.Client(), m, dir, quiet); err != nil {
				t.Fatalf("fetch: %v", err)
			}
			if got := hits.Load() - before; got != tc.downloads {
				t.Errorf("downloads = %d, want %d", got, tc.downloads)
			}
			fi, err := os.Lstat(dst)
			if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o644 {
				t.Fatalf("%s = %v, %v; want a regular 0644 file", dst, fi, err)
			}
			if b, _ := os.ReadFile(dst); string(b) != "amd64 bytes" {
				t.Errorf("archive = %q, want the pin", b)
			}
			if got := names(t, dir); !slices.Equal(got, []string{"rg-amd64.tar.gz"}) {
				t.Errorf("assets = %v", got)
			}
			if got := names(t, elsewhere); !slices.Equal(got, []string{"other", "pinned"}) {
				t.Errorf("what a link named holds %v, want what it held", got)
			}
			for f, want := range map[string]string{"pinned": "amd64 bytes", "other": "other bytes"} {
				if b, _ := os.ReadFile(filepath.Join(elsewhere, f)); string(b) != want {
					t.Errorf("%s = %q, want it untouched", f, b)
				}
			}
		})
	}
}

// An archive this user cannot read is one the build, which runs as this user,
// could not embed: it is replaced by the download, as bytes that are not the
// pin are, and the fetch says why — where it used to fail the fetch, and every
// build behind it, on the open. (Root reads it all the same, and takes it as
// the pin it is.)
func TestFetchReplacesAnArchiveItCannotRead(t *testing.T) {
	backoff = 0
	srv, hits := server(t, map[string]string{"rg-amd64.tar.gz": "amd64 bytes"})
	m := manifest(srv.URL, map[string]string{"amd64": sum("amd64 bytes")})
	dir := t.TempDir()
	dst := filepath.Join(dir, "rg-amd64.tar.gz")
	if err := os.WriteFile(dst, []byte("amd64 bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dst, 0); err != nil {
		t.Fatal(err)
	}
	var logged []string
	logf := func(format string, a ...any) { logged = append(logged, fmt.Sprintf(format, a...)) }
	if err := fetch(context.Background(), srv.Client(), m, dir, logf); err != nil {
		t.Fatalf("fetch over an archive it cannot read: %v", err)
	}
	fi, err := os.Lstat(dst)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o644 {
		t.Fatalf("%s = %v, %v; want a regular 0644 file", dst, fi, err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "amd64 bytes" {
		t.Errorf("archive = %q, want the pin", b)
	}
	if os.Geteuid() == 0 {
		return
	}
	if hits.Load() != 1 {
		t.Errorf("downloads = %d, want the one that replaced it", hits.Load())
	}
	if !strings.Contains(strings.Join(logged, "\n"), dst+" cannot be read, so it is replaced: ") {
		t.Errorf("logged %q; want why it was replaced", logged)
	}
}

// Where present found a regular file it opens without following a link: one
// put at the name after the check is refused, not opened — and what it names
// is never read or chmodded — while a link present found as one is followed.
func TestOpenRegularFollowsALinkOnlyWhenTold(t *testing.T) {
	dir := t.TempDir()
	target, link := filepath.Join(dir, "target"), filepath.Join(dir, "link")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if f, err := openRegular(link, false); f != nil || err != nil {
		if f != nil {
			f.Close()
		}
		t.Fatalf("openRegular(a link, false) = %v, %v; want nil, nil", f, err)
	}
	f, err := openRegular(link, true)
	if err != nil || f == nil {
		t.Fatalf("openRegular(a link, true) = %v, %v; want what it names", f, err)
	}
	f.Close()
	if f, err := openRegular(target, false); err != nil || f == nil {
		t.Fatalf("openRegular(a regular file, false) = %v, %v", f, err)
	} else {
		f.Close()
	}
}
