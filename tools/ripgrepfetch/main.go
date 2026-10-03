// Command ripgrepfetch puts the ripgrep archives internal/ripgrep pins into
// its assets directory, where the next build embeds them (`make ripgrep`).
//
// Each archive is downloaded only when the directory does not already hold a
// regular file of that name with the pinned sha256, and lands only once its
// own bytes hash to the pin: a download is written beside its target under a
// dot name the embed never picks up, and renamed into place after the digest
// matches. What stands at an archive's name and is not a regular file is not
// one the embed takes — go:embed skips a symbolic link in a directory it
// embeds — so a link to a regular file that holds the pin is replaced by a
// regular copy of those bytes, checked the same way, and a link to anything
// else, a directory or any other kind of file there is removed and the
// archive fetched. An archive the directory already holds is left 0644, as a
// download lands, where its mode can be changed; where it cannot, it is taken
// as it is, and the refusal logged. One this user cannot read is replaced.
// Then everything else in the directory is removed — an archive the manifest
// no longer names, an interrupted download, a stray file or directory — but
// the manifest: internal/ripgrep embeds the whole directory, since a build
// made without the archives must still compile, so what it holds is what a
// build carries, and this leaves it the manifest and the pin. A host that
// cannot reach GitHub can put the archives there by any other route; they are
// checked all the same.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/ripgrep"
)

// maxArchive bounds a download: the pinned archives are about 2 MB, and a
// response that runs on past this is not one of them.
const maxArchive = 64 << 20

// attempts is how often a download is tried before the fetch fails, backoff
// apart and then twice that: a CI runner's network drops a request now and
// then, and a gate that reddens on one is a gate people learn to re-run.
const attempts = 3

var backoff = 2 * time.Second

func main() {
	log.SetFlags(0)
	dir := flag.String("dir", "internal/ripgrep/assets", "the directory internal/ripgrep embeds")
	flag.Parse()
	client := &http.Client{Timeout: 2 * time.Minute}
	if err := fetch(context.Background(), client, ripgrep.Pinned, *dir, log.Printf); err != nil {
		log.Fatalf("ripgrepfetch: %v", err)
	}
}

// manifestName is the manifest's file in the assets directory, which
// internal/ripgrep embeds under that name.
const manifestName = "manifest.json"

func fetch(ctx context.Context, client *http.Client, m ripgrep.Manifest, dir string, logf func(string, ...any)) error {
	keep := map[string]bool{manifestName: true}
	for _, arch := range slices.Sorted(maps.Keys(m.Archives)) {
		a := m.Archives[arch]
		keep[a.Name()] = true
		dst := filepath.Join(dir, a.Name())
		ok, err := present(dst, a.SHA256, logf)
		if err != nil {
			return err
		}
		if ok {
			logf("ripgrep %s linux/%s: %s present", m.Version, arch, a.Name())
			continue
		}
		var last error
		for i := range attempts {
			if i > 0 {
				time.Sleep(time.Duration(i) * backoff)
			}
			if last = download(ctx, client, a, dst); last == nil || errors.Is(last, errDigest) {
				break
			}
		}
		if last != nil {
			return last
		}
		logf("ripgrep %s linux/%s: fetched %s", m.Version, arch, a.Name())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if n := e.Name(); !keep[n] {
			// RemoveAll removes a link, never what it names.
			if err := os.RemoveAll(filepath.Join(dir, n)); err != nil {
				return err
			}
			logf("removed %s", n)
		}
	}
	return nil
}

// partPrefix names a download in flight. A leading dot keeps it out of the
// embed even if the fetch dies before removing it.
const partPrefix = ".ripgrepfetch-"

// errDigest is a download that arrived whole and is not the pinned archive.
// Fetching it again would fetch the same wrong bytes, so it is not retried.
var errDigest = errors.New("does not match its pinned sha256")

// present reports whether dst is the pinned archive as the embed will take
// it: a regular file whose bytes hash to want, which is left 0644, as land
// leaves a download, whatever mode it came with — or, where the chmod is
// refused (a file another user owns, a read-only checkout), left as it is,
// the refusal logged: its bytes are still the pin, which is what the embed
// needs. A regular file this user cannot read at all (EACCES — another
// user's, say, at 0600) is replaced as one that is not the pin is, the
// refusal logged: the build, which runs as this user, could not embed it
// either, and removing it asks only for the directory's write permission,
// which the sweep below needs anyway. A symbolic link to a regular file whose
// bytes are the pin becomes a regular copy of them (land); whatever else
// stands at dst and is not a regular file — a link to anything else, a
// directory, a FIFO — is removed, so the download can land in its place.
//
// What stands at dst is read only through a descriptor that is a regular
// file's (openRegular): an open of a FIFO would wait for a writer that never
// comes, and one can be put at dst, or at what a link names, between the
// check of its type and the open. Where the check found a regular file the
// open does not follow a link (O_NOFOLLOW): one swapped in after the check
// is refused like any other link that is not the pin — removed, never
// followed, and what it names never chmodded.
func present(dst, want string, logf func(string, ...any)) (bool, error) {
	info, err := os.Lstat(dst)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case info.Mode().IsRegular():
		f, err := openRegular(dst, false)
		if errors.Is(err, fs.ErrPermission) {
			logf("ripgrepfetch: %s cannot be read, so it is replaced: %v", dst, err)
			break
		}
		if err != nil {
			return false, err
		}
		if f == nil {
			break
		}
		defer f.Close()
		ok, err := matches(f, want)
		if !ok || err != nil {
			return ok, err
		}
		if fi, err := f.Stat(); err == nil && fi.Mode().Perm() != 0o644 {
			if err := f.Chmod(0o644); err != nil {
				logf("ripgrepfetch: %s is the pin, left %v: %v", dst, fi.Mode().Perm(), err)
			}
		}
		return true, nil
	case info.Mode()&os.ModeSymlink != 0:
		if f, err := openRegular(dst, true); err == nil && f != nil {
			err = land(f, dst, want)
			f.Close()
			if err == nil {
				return true, nil
			}
		}
	}
	// RemoveAll removes a link, never what it names.
	return false, os.RemoveAll(dst)
}

// openRegular opens name without waiting on it — O_NONBLOCK, which a FIFO's
// open honours and a regular file's read ignores — through a link only where
// follow says so, and hands back the file only if the descriptor it opened is
// a regular file's: nil, with no error, if it is not, if nothing is there, or
// if a link is there and follow is false. That last is O_NOFOLLOW's refusal,
// ELOOP on Linux and darwin (EMLINK on FreeBSD, EFTYPE on NetBSD), so whether
// a link is what was refused is asked of the name rather than of the errno.
func openRegular(name string, follow bool) (*os.File, error) {
	flag := os.O_RDONLY | syscall.O_NONBLOCK
	if !follow {
		flag |= syscall.O_NOFOLLOW
	}
	f, err := os.OpenFile(name, flag, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		if info, lerr := os.Lstat(name); !follow && lerr == nil && info.Mode()&os.ModeSymlink != 0 {
			return nil, nil
		}
		return nil, err
	}
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, err
	}
	return f, nil
}

func matches(f *os.File, want string) (bool, error) {
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == want, nil
}

func download(ctx context.Context, client *http.Client, a ripgrep.Archive, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", a.URL, resp.Status)
	}
	if err := land(resp.Body, dst, a.SHA256); err != nil {
		return fmt.Errorf("GET %s: %w", a.URL, err)
	}
	return nil
}

// land writes src to dst as a regular file, once its bytes hash to want: into
// a file beside dst under a dot name the embed never picks up, renamed over
// dst — over a link there too, never through it — only after the digest
// matches.
func land(src io.Reader, dst, want string) error {
	tmp, err := os.CreateTemp(filepath.Dir(dst), partPrefix+"*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(src, maxArchive+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		return err
	case n > maxArchive:
		return fmt.Errorf("larger than %d bytes", maxArchive)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("sha256 %s %w %s", got, errDigest, want)
	}
	// CreateTemp makes the file 0600; an archive is no secret, and a checkout
	// another user builds from must be able to read it.
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
