package executor

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// errCloneTooLarge aborts a clone whose spool passed the byte budget. It is
// raised by the metering filesystem *during* the clone rather than by a
// post-hoc size check: an adversarial repository would otherwise be free to
// blow far past the cap before anyone measured it (plan 25 decision 1).
var errCloneTooLarge = errors.New("clone exceeded the byte budget")

// tokenUsername is the username GitHub expects beside a personal access token
// in HTTP basic auth. The token rides the Authorization header of each request
// — never the URL and never the on-disk .git/config — which is what keeps it
// out of the sandbox entirely (plan 25 decision 1).
const tokenUsername = "x-access-token"

// cloneToTar clones repo at its requested checkout into a private spool
// directory and packs the whole working tree — `.git` included, so the agent
// gets a real repository — into a tar file. It returns that file's path and
// size for the caller to ship into the sandbox; the caller owns removing the
// spool via the returned cleanup, on every exit path.
//
// The byte budget covers the clone and the tar together, metered as bytes land
// (meteredFS), because the spool sits on executor-local disk outside the
// sandbox's own storage hardening: one unbounded repository could otherwise
// exhaust the executor and disrupt unrelated sessions.
func cloneToTar(ctx context.Context, r repoRef, token string, maxBytes int64) (tarPath string, size int64, cleanup func(), err error) {
	spool, err := os.MkdirTemp("", "map-repo-clone-")
	if err != nil {
		return "", 0, func() {}, err
	}
	cleanup = func() { _ = os.RemoveAll(spool) }

	var spent atomic.Int64
	tree := newMeteredFS(osfs.New(filepath.Join(spool, "tree")), &spent, maxBytes)
	dotgit, err := tree.Chroot(".git")
	if err != nil {
		return "", 0, cleanup, err
	}
	storer := filesystem.NewStorage(dotgit, cache.NewObjectLRUDefault())

	opts := &git.CloneOptions{
		URL:  r.URL,
		Auth: &githttp.BasicAuth{Username: tokenUsername, Password: token},
	}
	// A branch checkout fetches only that branch; a commit checkout needs
	// arbitrary history, so it clones fully and detaches afterwards. Omitted
	// means the remote's own HEAD — the default branch, resolved here rather
	// than at create (plan 25 decisions 3 and 7).
	if r.Checkout != nil && r.Checkout.Type == "branch" {
		opts.ReferenceName = plumbing.NewBranchReferenceName(r.Checkout.Name)
		opts.SingleBranch = true
	}
	repo, err := git.CloneContext(ctx, storer, tree, opts)
	if err != nil {
		return "", 0, cleanup, err
	}
	if r.Checkout != nil && r.Checkout.Type == "commit" {
		wt, werr := repo.Worktree()
		if werr != nil {
			return "", 0, cleanup, werr
		}
		if werr := wt.Checkout(&git.CheckoutOptions{Hash: plumbing.NewHash(r.Checkout.Sha)}); werr != nil {
			return "", 0, cleanup, werr
		}
	}

	tarPath = filepath.Join(spool, "repo.tar")
	size, err = packTree(filepath.Join(spool, "tree"), tarPath, &spent, maxBytes)
	if err != nil {
		return "", 0, cleanup, err
	}
	return tarPath, size, cleanup, nil
}

// packTree writes dir's contents to a tar file with paths relative to dir, so
// extracting with `tar -C <target>` reproduces the tree at <target>. Regular
// files and symlinks are carried; anything else (a device node a hostile
// repository cannot actually produce through go-git, but which would be a
// sandbox-side surprise) is skipped. The tar's own bytes ride the same budget
// the clone spent.
func packTree(dir, tarPath string, spent *atomic.Int64, maxBytes int64) (int64, error) {
	f, err := os.Create(tarPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	counted := &countingWriter{w: f, spent: spent, limit: maxBytes}
	tw := tar.NewWriter(counted)

	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		var link string
		if info.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		} else if !info.Mode().IsRegular() && !info.IsDir() {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		src, err := os.Open(p)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(tw, src)
		return err
	})
	if err != nil {
		return 0, err
	}
	if err := tw.Close(); err != nil {
		return 0, err
	}
	if err := f.Sync(); err != nil {
		return 0, err
	}
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// cloneReason maps a clone failure to the machine-readable reason the
// session.error variant carries (plan 25 decision 4). go-git wraps its
// transport sentinels with %w, so the classification is errors.Is over
// sentinels rather than string matching over messages.
func cloneReason(err error) string {
	switch {
	case errors.Is(err, errCloneTooLarge):
		return repoOutcomeTooLarge
	case errors.Is(err, context.DeadlineExceeded):
		return repoOutcomeTimeout
	case errors.Is(err, transport.ErrAuthenticationRequired),
		errors.Is(err, transport.ErrAuthorizationFailed):
		return repoOutcomeAuth
	case errors.Is(err, transport.ErrRepositoryNotFound),
		errors.Is(err, transport.ErrEmptyRemoteRepository):
		return repoOutcomeNotFound
	case errors.Is(err, plumbing.ErrObjectNotFound),
		errors.Is(err, plumbing.ErrReferenceNotFound),
		errors.Is(err, git.NoMatchingRefSpecError{}):
		return repoOutcomeCheckout
	case isNetworkError(err):
		return repoOutcomeNetwork
	default:
		return repoOutcomeInternal
	}
}

// isNetworkError reports whether err looks like a failure to reach or speak to
// the remote at all, as opposed to a repository-level refusal. go-git returns
// the transport's own error here (a *url.Error, a dial failure), which carries
// no sentinel of its own, so this is the one classification that must read the
// message — and it is the fallback arm, never a guard.
func isNetworkError(err error) bool {
	if err == nil {
		return false
	}
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) {
		return true
	}
	msg := err.Error()
	for _, s := range []string{"connection refused", "no such host", "network is unreachable",
		"connection reset", "EOF", "TLS handshake", "i/o timeout"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// scrubToken removes a token that somehow reached an error message before it
// can be logged. Auth rides a header, not the URL, so go-git's messages are
// token-free by construction; this is belt and braces (plan 25 decision 4).
func scrubToken(msg, token string) string {
	if token == "" {
		return msg
	}
	return strings.ReplaceAll(msg, token, "[redacted]")
}

// meteredFS is a billy filesystem that counts every byte written beneath it
// against a shared budget and fails the write past the cap. go-git chroots the
// .git directory out of the worktree filesystem, so Chroot must propagate the
// *same* counter — otherwise the objects, which are the bulk of a clone, would
// escape the meter entirely.
type meteredFS struct {
	billy.Filesystem
	spent *atomic.Int64
	limit int64
}

func newMeteredFS(inner billy.Filesystem, spent *atomic.Int64, limit int64) billy.Filesystem {
	return &meteredFS{Filesystem: inner, spent: spent, limit: limit}
}

func (m *meteredFS) Create(filename string) (billy.File, error) {
	f, err := m.Filesystem.Create(filename)
	return m.wrap(f), err
}

func (m *meteredFS) OpenFile(filename string, flag int, perm os.FileMode) (billy.File, error) {
	f, err := m.Filesystem.OpenFile(filename, flag, perm)
	return m.wrap(f), err
}

func (m *meteredFS) TempFile(dir, prefix string) (billy.File, error) {
	f, err := m.Filesystem.TempFile(dir, prefix)
	return m.wrap(f), err
}

func (m *meteredFS) Chroot(path string) (billy.Filesystem, error) {
	inner, err := m.Filesystem.Chroot(path)
	if err != nil {
		return nil, err
	}
	return &meteredFS{Filesystem: inner, spent: m.spent, limit: m.limit}, nil
}

func (m *meteredFS) wrap(f billy.File) billy.File {
	if f == nil {
		return nil
	}
	return &meteredFile{File: f, spent: m.spent, limit: m.limit}
}

// meteredFile counts a file's writes against the shared budget.
type meteredFile struct {
	billy.File
	spent *atomic.Int64
	limit int64
}

func (f *meteredFile) Write(p []byte) (int, error) {
	if n := f.spent.Add(int64(len(p))); n > f.limit {
		return 0, fmt.Errorf("%w: %d bytes", errCloneTooLarge, n)
	}
	return f.File.Write(p)
}

// countingWriter is meteredFile's plain-io twin, for the tar the clone is
// packed into: the budget covers tree and tar together.
type countingWriter struct {
	w     io.Writer
	spent *atomic.Int64
	limit int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	if n := c.spent.Add(int64(len(p))); n > c.limit {
		return 0, fmt.Errorf("%w: %d bytes", errCloneTooLarge, n)
	}
	return c.w.Write(p)
}
