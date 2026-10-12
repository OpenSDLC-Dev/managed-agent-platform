package executor

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// TestRepoCloneShapeMatchesTheReference 🔍 pins the checkout #582 recorded
// inside a reference sandbox (2026-09-03 batch2 session.events.list.repo-inspect):
// a shallow clone, and a .git/config carrying two remotes at the `.git`
// spelling of the URL, the second one GitHub CLI's base remote, with automatic
// gc off. Every checkout form is depth 1 — a commit included, fetched by its
// exact SHA rather than out of the full history, so the fixture's commit is not
// its default branch's tip — and fetches no tag: the tag on the first commit
// would bring that commit back into a branch's clone. Refs are what `git clone`
// leaves, and origin/HEAD, which names the remote's default branch, appears
// only when that branch is the one checked out. Each tree is judged by the git
// binary itself, which is what an agent's script runs against.
func TestRepoCloneShapeMatchesTheReference(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"first.txt": "one\n"})
	first, err := fx.repo.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	// Annotated, so a fetch that follows tags would carry it with the commit.
	tag, err := fx.repo.CreateTag("v0", first.Hash(), &git.CreateTagOptions{Message: "v0",
		Tagger: &object.Signature{Name: "fixture", Email: "fixture@example.invalid", When: time.Unix(1700000000, 0)}})
	if err != nil {
		t.Fatalf("tag: %v", err)
	}
	fx.commit(t, "second", map[string]string{"second.txt": "two\n"})
	fx.branch(t, "feature/topic", map[string]string{"feature.txt": "on the branch\n"})
	url := fx.srv.URL + "/o/r"

	for _, tc := range []struct {
		name     string
		checkout *repoCheckout
		branch   string // "" for a detached commit checkout
		file     string
	}{
		{"default branch", nil, "master", "second.txt"},
		// Create admits no such checkout, but whatever takes the default
		// branch's clone gets its refs too.
		{"untyped checkout", &repoCheckout{}, "master", "second.txt"},
		{"branch", &repoCheckout{Type: "branch", Name: "feature/topic"}, "feature/topic", "feature.txt"},
		{"commit", &repoCheckout{Type: "commit", Sha: first.Hash().String()}, "", "first.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := cloneAndUnpack(t, repoRef{URL: url, Checkout: tc.checkout})
			if _, err := os.Stat(filepath.Join(dir, tc.file)); err != nil {
				t.Errorf("%s is missing from the checkout: %v", tc.file, err)
			}

			for _, c := range []struct{ args, want string }{
				{"rev-parse --is-shallow-repository", "true"},
				{"rev-list --count HEAD", "1"},
				{"config core.repositoryformatversion", "0"},
				{"config core.filemode", "true"},
				{"config core.logallrefupdates", "true"},
				{"config gc.auto", "0"},
				{"config --get-all remote.origin.url", url + ".git"},
				{"config --get-all remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"},
				{"config --get-all remote.github.url", url + ".git"},
				{"config --get-all remote.github.fetch", "+refs/heads/*:refs/remotes/github/*"},
				{"config remote.github.gh-resolved", "base"},
			} {
				if got := gitIn(t, dir, c.args); got != c.want {
					t.Errorf("git %s = %q, want %q", c.args, got, c.want)
				}
			}
			gitIn(t, dir, "fsck --connectivity-only")
			if gitCmd(t, dir, "cat-file", "-e", tag.Hash().String()).Run() == nil {
				t.Error("the tag on the first commit was fetched")
			}

			refs := gitIn(t, dir, "for-each-ref --format=%(refname)")
			if tc.branch == "" {
				if got := gitIn(t, dir, "rev-parse HEAD"); got != first.Hash().String() {
					t.Errorf("HEAD = %s, want the requested commit %s", got, first.Hash())
				}
				if out, err := gitCmd(t, dir, "symbolic-ref", "-q", "HEAD").Output(); err == nil {
					t.Errorf("HEAD names %s, want it detached", strings.TrimSpace(string(out)))
				}
				if refs != "" {
					t.Errorf("refs = %q, want none beside the detached HEAD", refs)
				}
				// The scratch ref leaves nothing behind, not even a directory.
				entries, err := os.ReadDir(filepath.Join(dir, ".git", "refs"))
				if err != nil {
					t.Fatalf("read .git/refs: %v", err)
				}
				var names []string
				for _, e := range entries {
					names = append(names, e.Name())
				}
				if got := strings.Join(names, " "); got != "heads tags" {
					t.Errorf(".git/refs holds %q, want only the heads and tags `git init` makes", got)
				}
				return
			}
			if got := gitIn(t, dir, "symbolic-ref HEAD"); got != "refs/heads/"+tc.branch {
				t.Errorf("HEAD names %s, want refs/heads/%s", got, tc.branch)
			}
			if gitCmd(t, dir, "cat-file", "-e", first.Hash().String()).Run() == nil {
				t.Error("the tagged first commit was fetched beside the branch")
			}
			for _, c := range []struct{ args, want string }{
				// The upstream and remote-tracking ref `git clone` leaves, so
				// `git status` and `git pull` work.
				{"config branch." + tc.branch + ".remote", "origin"},
				{"config branch." + tc.branch + ".merge", "refs/heads/" + tc.branch},
				{"rev-parse refs/remotes/origin/" + tc.branch, gitIn(t, dir, "rev-parse HEAD")},
				{"rev-parse --abbrev-ref @{upstream}", "origin/" + tc.branch},
			} {
				if got := gitIn(t, dir, c.args); got != c.want {
					t.Errorf("git %s = %q, want %q", c.args, got, c.want)
				}
			}
			want := "refs/heads/" + tc.branch + "\nrefs/remotes/origin/" + tc.branch
			if tc.checkout == nil || tc.checkout.Type == "" {
				if got := gitIn(t, dir, "symbolic-ref refs/remotes/origin/HEAD"); got != "refs/remotes/origin/"+tc.branch {
					t.Errorf("origin/HEAD names %s, want refs/remotes/origin/%s", got, tc.branch)
				}
				want = "refs/heads/" + tc.branch + "\nrefs/remotes/origin/HEAD\nrefs/remotes/origin/" + tc.branch
			}
			if refs != want {
				t.Errorf("refs = %q, want %q", refs, want)
			}
		})
	}
}

// TestRepoDefaultBranchUnderHEADClones 🔍: git admits a branch named HEAD or
// HEAD/<x>, and either one's tracking ref takes origin/HEAD's name. `git
// clone` of a repository whose default branch is HEAD/<x> leaves the tracking
// ref and no origin/HEAD, which that name's directory rules out (git 2.54,
// measured). For HEAD, the tracking ref would be origin/HEAD itself, so
// neither is written. The clone succeeds either way.
func TestRepoDefaultBranchUnderHEADClones(t *testing.T) {
	for _, tc := range []struct{ branch, refs string }{
		{"HEAD/topic", "refs/heads/HEAD/topic\nrefs/remotes/origin/HEAD/topic"},
		{"HEAD", "refs/heads/HEAD"},
	} {
		t.Run(tc.branch, func(t *testing.T) {
			fx := newGitFixture(t, map[string]string{"first.txt": "one\n"})
			fx.branch(t, tc.branch, map[string]string{"topic.txt": "on the branch\n"})
			if err := fx.repo.Storer.SetReference(plumbing.NewSymbolicReference(
				plumbing.HEAD, plumbing.NewBranchReferenceName(tc.branch))); err != nil {
				t.Fatalf("make %s the default branch: %v", tc.branch, err)
			}
			dir := cloneAndUnpack(t, repoRef{URL: fx.url()})
			if _, err := os.Stat(filepath.Join(dir, "topic.txt")); err != nil {
				t.Errorf("topic.txt is missing from the checkout: %v", err)
			}
			if got := gitIn(t, dir, "for-each-ref --format=%(refname)"); got != tc.refs {
				t.Errorf("refs = %q, want %q", got, tc.refs)
			}
			// for-each-ref hides an origin/HEAD that names itself; rev-parse
			// warns about it.
			if out, _ := gitCmd(t, dir, "rev-parse", "--verify", "-q", "refs/remotes/origin/HEAD").CombinedOutput(); len(out) != 0 {
				t.Errorf("origin/HEAD: %s, want none", strings.TrimSpace(string(out)))
			}
		})
	}
}

// TestRepoDefaultBranchNameIsKeptWhole 🔍: git admits a non-breaking space in
// a branch name, and go-git's ReferenceName.Short reads one as the end of the
// name. `HEAD<nbsp>topic` cut short is `HEAD`, which would make origin/HEAD
// name itself.
func TestRepoDefaultBranchNameIsKeptWhole(t *testing.T) {
	branch := "HEAD" + string(rune(0x00a0)) + "topic"
	fx := newGitFixture(t, map[string]string{"first.txt": "one\n"})
	fx.branch(t, branch, map[string]string{"topic.txt": "on the branch\n"})
	if err := fx.repo.Storer.SetReference(plumbing.NewSymbolicReference(
		plumbing.HEAD, plumbing.NewBranchReferenceName(branch))); err != nil {
		t.Fatalf("make %q the default branch: %v", branch, err)
	}
	dir := cloneAndUnpack(t, repoRef{URL: fx.url()})
	out, err := gitCmd(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD").Output()
	if got, want := strings.TrimSpace(string(out)), "refs/remotes/origin/"+branch; err != nil || got != want {
		t.Errorf("origin/HEAD names %q (%v), want %q", got, err, want)
	}
}

// TestRepoDetachedRemoteHEADClones 🔍: a remote whose HEAD names no branch
// leaves the clone detached, and `git clone` then leaves no origin/HEAD —
// there is no default branch for it to name. Without a branch to track,
// origin/HEAD must not be made to name itself.
func TestRepoDetachedRemoteHEADClones(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"first.txt": "one\n"})
	first, err := fx.repo.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	// go-git matches a HEAD the remote names no branch for against any ref at
	// its commit — this tag — and so checks it out detached.
	if _, err := fx.repo.CreateTag("v0", first.Hash(), nil); err != nil {
		t.Fatalf("tag: %v", err)
	}
	fx.commit(t, "second", map[string]string{"second.txt": "two\n"})
	if err := fx.repo.Storer.SetReference(plumbing.NewHashReference(plumbing.HEAD, first.Hash())); err != nil {
		t.Fatalf("detach the remote's HEAD: %v", err)
	}
	dir := cloneAndUnpack(t, repoRef{URL: fx.url()})
	if got := gitIn(t, dir, "rev-parse HEAD"); got != first.Hash().String() {
		t.Errorf("HEAD = %s, want the remote's HEAD %s", got, first.Hash())
	}
	// Silent only when there is no such ref: git prints the hash of one that
	// resolves, and a warning for one that names itself, which for-each-ref
	// would hide.
	if out, _ := gitCmd(t, dir, "rev-parse", "--verify", "-q", "refs/remotes/origin/HEAD").CombinedOutput(); len(out) != 0 {
		t.Errorf("origin/HEAD: %s, want none", strings.TrimSpace(string(out)))
	}
}

// TestRepoGitSuffixIsNotDoubled 🔍: a URL already spelled with `.git` keeps
// that one suffix in both remotes.
func TestRepoGitSuffixIsNotDoubled(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
	dir := cloneAndUnpack(t, repoRef{URL: fx.url()})
	for _, remote := range []string{"origin", "github"} {
		if got := gitIn(t, dir, "config remote."+remote+".url"); got != fx.url() {
			t.Errorf("remote.%s.url = %q, want %q", remote, got, fx.url())
		}
	}
}

// TestRepoUnknownCommitBlamesTheCheckout 🔍: a commit fetched by its exact SHA
// fails at the remote, which answers `not our ref` in a message go-git carries
// as plain text. The SHA is the caller's, so the reason is `checkout`, as it was
// when the full clone's checkout reported the object missing.
func TestRepoUnknownCommitBlamesTheCheckout(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
	_, _, cleanup, err := cloneToTar(context.Background(),
		repoRef{URL: fx.url(), Checkout: &repoCheckout{Type: "commit", Sha: strings.Repeat("1234567890", 4)}},
		"ghp_fixture", 1<<30)
	defer cleanup()
	if err == nil {
		t.Fatal("a commit the remote does not have cloned successfully")
	}
	if got := cloneReason(err); got != repoOutcomeCheckout {
		t.Errorf("cloneReason = %q, want %q — the sha is the caller's: %v", got, repoOutcomeCheckout, err)
	}
}

// TestRepoAnnotatedTagShaMountsItsCommit 🔍: an annotated tag's id is what
// `git rev-parse <tag>` gives, and the full clone's checkout peeled it to the
// tag's commit. The shallow fetch must mount that commit too.
func TestRepoAnnotatedTagShaMountsItsCommit(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
	head, err := fx.repo.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	tag, err := fx.repo.CreateTag("v1", head.Hash(), &git.CreateTagOptions{Message: "v1",
		Tagger: &object.Signature{Name: "fixture", Email: "fixture@example.invalid", When: time.Unix(1700000000, 0)}})
	if err != nil {
		t.Fatalf("tag: %v", err)
	}
	dir := cloneAndUnpack(t, repoRef{URL: fx.url(), Checkout: &repoCheckout{Type: "commit", Sha: tag.Hash().String()}})
	if got := gitIn(t, dir, "rev-parse HEAD"); got != head.Hash().String() {
		t.Errorf("HEAD = %s, want the tag's commit %s", got, head.Hash())
	}
}

// TestRepoBranchNameHoldingRefsHeads 🔍: git admits a branch name that holds
// `refs/heads/`, and go-git fetches the right branch for it, but names the
// [branch] section from the text before that second `refs/heads/`. A name
// whose cut is a valid branch mounted before #582 and must still; one whose cut
// is not fails after the fetch, as it did, on go-git's account.
func TestRepoBranchNameHoldingRefsHeads(t *testing.T) {
	t.Run("cut is a valid name", func(t *testing.T) {
		const name = "arefs/heads/b"
		fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
		fx.branch(t, name, map[string]string{"b.txt": "on the branch\n"})
		dir := cloneAndUnpack(t, repoRef{URL: fx.url(), Checkout: &repoCheckout{Type: "branch", Name: name}})
		if got := gitIn(t, dir, "symbolic-ref HEAD"); got != "refs/heads/"+name {
			t.Errorf("HEAD = %s, want refs/heads/%s", got, name)
		}
		if _, err := os.Stat(filepath.Join(dir, "b.txt")); err != nil {
			t.Errorf("the branch's tree is not mounted: %v", err)
		}
	})
	t.Run("cut is not a valid name", func(t *testing.T) {
		const name = "feature/refs/heads/x"
		fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
		fx.branch(t, name, map[string]string{"x.txt": "on the branch\n"})
		_, _, cleanup, err := cloneToTar(context.Background(),
			repoRef{URL: fx.url(), Checkout: &repoCheckout{Type: "branch", Name: name}}, "ghp_fixture", 1<<30)
		defer cleanup()
		if got := cloneReason(err); err == nil || got != repoOutcomeInternal {
			t.Errorf("cloneReason = %q, want %q — the limit is go-git's: %v", got, repoOutcomeInternal, err)
		}
	})
}

// cloneAndUnpack clones r and unpacks the tar cloneToTar ships, returning the
// directory the sandbox's mount would hold.
func cloneAndUnpack(t *testing.T, r repoRef) string {
	t.Helper()
	tarPath, _, cleanup, err := cloneToTar(context.Background(), r, "ghp_fixture", 1<<30)
	t.Cleanup(cleanup)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dir := t.TempDir()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return dir
		}
		if err != nil {
			t.Fatalf("read the tar: %v", err)
		}
		target := filepath.Join(dir, hdr.Name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(target, 0o755)
		case tar.TypeSymlink:
			err = os.Symlink(hdr.Linkname, target)
		default:
			if err = os.MkdirAll(filepath.Dir(target), 0o755); err == nil {
				var out *os.File
				if out, err = os.OpenFile(target, os.O_CREATE|os.O_WRONLY, os.FileMode(hdr.Mode)); err == nil {
					_, err = io.Copy(out, tr)
					out.Close()
				}
			}
		}
		if err != nil {
			t.Fatalf("unpack %s: %v", hdr.Name, err)
		}
	}
}

// gitIn runs git with the space-separated args in dir and returns its trimmed
// output, failing the test if git does.
func gitIn(t *testing.T, dir, args string) string {
	t.Helper()
	out, err := gitCmd(t, dir, strings.Fields(args)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitCmd is git in dir, isolated from the host's own git configuration and
// from any GIT_* variable the caller carries — a git hook running the tests
// exports GIT_DIR, which would point every check at the outer repository.
func gitCmd(t *testing.T, dir string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "HOME="+t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	return cmd
}
