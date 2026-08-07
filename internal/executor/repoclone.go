package executor

import (
	"archive/tar"
	"context"
	"encoding/base64"
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
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
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
	// Every error out of here is logged by the caller, and the ones go-git
	// builds from a refusing host's response body can quote the credential
	// straight back at us. Scrubbed once, at the boundary, rather than at each
	// of the seven returns below.
	defer func() { err = scrubTokenErr(err, token) }()

	spool, err := os.MkdirTemp("", "map-repo-clone-")
	if err != nil {
		return "", 0, func() {}, err
	}
	cleanup = func() { _ = os.RemoveAll(spool) }

	var spent atomic.Int64
	tree := newCloneFS(filepath.Join(spool, "tree"), &spent, maxBytes)
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
	// go-git applies the context to transport only — its post-fetch worktree
	// reset runs to completion whatever the deadline says — so the phases after
	// it are bounded here instead. Without this a repository that downloads
	// inside the deadline and then checks out or packs for hours would hold the
	// executor indefinitely, and the deadline would bound nothing but the fetch.
	if err := ctx.Err(); err != nil {
		return "", 0, cleanup, err
	}
	if r.Checkout != nil && r.Checkout.Type == "commit" {
		hash := plumbing.NewHash(r.Checkout.Sha)
		// The null object id is forty hex characters, so it passes the create
		// endpoint's sha validation — and go-git's CheckoutOptions.Validate
		// substitutes `master` for a zero hash rather than refusing it. Left
		// alone, a session asking for 000…0 would be told its clone succeeded
		// while the mount held a branch nobody named. (GitHub's webhooks put
		// forty zeros in `before`/`after` on branch create and delete, so a
		// forwarding app reaches this without inventing anything.)
		if hash.IsZero() {
			return "", 0, cleanup, fmt.Errorf("%w: the null commit id %s", plumbing.ErrObjectNotFound, r.Checkout.Sha)
		}
		wt, werr := repo.Worktree()
		if werr != nil {
			return "", 0, cleanup, werr
		}
		if werr := wt.Checkout(&git.CheckoutOptions{Hash: hash}); werr != nil {
			return "", 0, cleanup, werr
		}
		if err := ctx.Err(); err != nil {
			return "", 0, cleanup, err
		}
	}

	tarPath = filepath.Join(spool, "repo.tar")
	size, err = packTree(ctx, filepath.Join(spool, "tree"), tarPath, &spent, maxBytes)
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
func packTree(ctx context.Context, dir, tarPath string, spent *atomic.Int64, maxBytes int64) (int64, error) {
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
		// Per entry, so a tree large enough to outlast the deadline stops at it
		// rather than after it: this walk is otherwise the one unbounded phase
		// of a clone, and a repository with millions of entries spends its time
		// here, not in the fetch.
		if err := ctx.Err(); err != nil {
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
	// Nothing but this platform cancels a clone without a deadline — a lost
	// lease, or an executor shutting down. go-git surfaces it as a *url.Error,
	// which the network fallback below happily matches, so our own withdrawal
	// would otherwise be counted and logged as the git host's failure.
	case errors.Is(err, context.Canceled):
		return repoOutcomeInternal
	case errors.Is(err, transport.ErrAuthenticationRequired),
		errors.Is(err, transport.ErrAuthorizationFailed):
		return repoOutcomeAuth
	case errors.Is(err, transport.ErrRepositoryNotFound),
		errors.Is(err, transport.ErrEmptyRemoteRepository):
		return repoOutcomeNotFound
	case errors.Is(err, plumbing.ErrObjectNotFound),
		errors.Is(err, plumbing.ErrReferenceNotFound),
		errors.Is(err, git.NoMatchingRefSpecError{}),
		// A checkout descriptor go-git will not even build a refspec from —
		// `foo:bar` as a branch name, say. It is the caller's ref that is
		// wrong, so `internal` would send the operator to read our logs over
		// their own typo.
		errors.Is(err, config.ErrRefSpecMalformedSeparator),
		errors.Is(err, config.ErrRefSpecMalformedWildcard):
		return repoOutcomeCheckout
	case isRemoteStatusError(err), isNetworkError(err), isProtocolError(err):
		return repoOutcomeNetwork
	default:
		return repoOutcomeInternal
	}
}

// isRemoteStatusError reports whether the remote answered with an HTTP status
// go-git had no sentinel for — a 5xx, a 429, anything but the 401/403/404 the
// arms above already claim. It is the remote's failure, not ours, so reporting
// it as `internal` would send the operator to read our logs over an outage at
// the git host.
//
// The unwrapping is manual because go-git's UnexpectedError implements no
// Unwrap, so errors.As cannot reach the transport error it holds.
func isRemoteStatusError(err error) bool {
	var unexpected *plumbing.UnexpectedError
	if !errors.As(err, &unexpected) {
		return false
	}
	var httpErr *githttp.Err
	return errors.As(unexpected.Err, &httpErr)
}

// isProtocolError reports whether the remote answered but did not speak git:
// a 200 carrying an outage page, a captive portal, or a proxy that replaced the
// response. The bytes arrived, so nothing above matches and no HTTP status
// betrays it — only the pkt-line scanner refusing what it was handed. The
// remote's failure, not ours.
func isProtocolError(err error) bool {
	return errors.Is(err, pktline.ErrInvalidPktLen)
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

// scrubToken removes the credential from a message in both the forms a git host
// can hand back: verbatim, and inside the base64 basic-auth blob it was sent as.
//
// This is not belt and braces. go-git copies a failing response's body into the
// error it returns for 401, 403 and 404 (`fmt.Errorf("%w: %s", sentinel,
// reason)`), and a host that rejects a credential has already decoded it — so a
// host that says which credential it rejected puts our token in an error we log.
// Auth failure is the likeliest clone failure there is, which makes this the
// likeliest path, not the unlikeliest.
func scrubToken(msg, token string) string {
	if token == "" {
		return msg
	}
	msg = strings.ReplaceAll(msg, token, redactedToken)
	return strings.ReplaceAll(msg, basicAuthBlob(token), redactedToken)
}

const redactedToken = "[redacted]"

// basicAuthBlob is what go-git puts after "Basic " for our credential — the
// only encoded form of it that can appear anywhere.
func basicAuthBlob(token string) string {
	return base64.StdEncoding.EncodeToString([]byte(tokenUsername + ":" + token))
}

// scrubTokenErr presents err with the credential removed while keeping the
// error itself reachable: the caller classifies the failure with errors.Is over
// go-git's sentinels, so a scrub that returned a fresh error would silently
// turn every redacted auth failure into an `internal` one.
func scrubTokenErr(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	msg := err.Error()
	scrubbed := scrubToken(msg, token)
	if scrubbed == msg {
		return err
	}
	return &scrubbedError{err: err, msg: scrubbed}
}

type scrubbedError struct {
	err error
	msg string
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.err }

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

// newCloneFS is the spool filesystem the clone checks out into: metered, and
// rooted at root with **BoundOS** rather than go-billy's v5-compatibility
// default.
//
// The default (ChrootOS, deprecated by go-billy itself) rewrites an absolute
// symlink target to sit under its own root, so a repository's
// `hostfile -> /etc/hosts` would be packed as a link to the executor's
// temporary spool path and reach the sandbox dangling — inside a clone that
// reported success, which the `.git` probe then trusts forever. BoundOS keeps
// the target verbatim while still refusing to escape the root, which is what
// git does and what the sandbox needs to resolve the link in its own namespace.
func newCloneFS(root string, spent *atomic.Int64, limit int64) billy.Filesystem {
	return newMeteredFS(osfs.New(root, osfs.WithBoundOS()), spent, limit)
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

// Symlink charges the link's own content — its target — against the budget.
// go-git checks a mode-120000 tree entry out through here rather than through
// Create, so without this a repository of hundreds of thousands of entries all
// naming one stored blob would cost almost nothing in objects while landing
// gigabytes of link data in the spool, past a cap that never counted it.
func (m *meteredFS) Symlink(target, link string) error {
	if n := m.spent.Add(int64(len(target))); n > m.limit {
		return fmt.Errorf("%w: %d bytes", errCloneTooLarge, n)
	}
	return m.Filesystem.Symlink(target, link)
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
