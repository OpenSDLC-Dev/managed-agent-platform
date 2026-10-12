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
)

// TestRepoCloneShapeMatchesTheReference 🔍 pins the checkout #582 recorded
// inside a reference sandbox (2026-09-03 batch2 session.events.list.repo-inspect):
// a shallow clone, and a .git/config carrying two remotes at the `.git`
// spelling of the URL, the second one GitHub CLI's base remote, with automatic
// gc off. Every checkout form is depth 1 — a commit included, fetched by its
// exact SHA rather than out of the full history, so the fixture's commit is not
// its default branch's tip. Each tree is judged by the git binary itself, which
// is what an agent's script runs against.
func TestRepoCloneShapeMatchesTheReference(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"first.txt": "one\n"})
	first, err := fx.repo.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	fx.commit(t, "second", map[string]string{"second.txt": "two\n"})
	fx.branch(t, "feature", map[string]string{"feature.txt": "on the branch\n"})
	url := fx.srv.URL + "/o/r"

	for _, tc := range []struct {
		name     string
		checkout *repoCheckout
		branch   string // "" for a detached commit checkout
		file     string
	}{
		{"default branch", nil, "master", "second.txt"},
		{"branch", &repoCheckout{Type: "branch", Name: "feature"}, "feature", "feature.txt"},
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
				return
			}
			if got := gitIn(t, dir, "symbolic-ref HEAD"); got != "refs/heads/"+tc.branch {
				t.Errorf("HEAD names %s, want refs/heads/%s", got, tc.branch)
			}
			for _, c := range []struct{ args, want string }{
				{"config branch." + tc.branch + ".remote", "origin"},
				{"config branch." + tc.branch + ".merge", "refs/heads/" + tc.branch},
				// What `git clone` leaves: the branch's remote-tracking ref,
				// and origin/HEAD naming it, so `git status` has an upstream.
				{"rev-parse refs/remotes/origin/" + tc.branch, gitIn(t, dir, "rev-parse HEAD")},
				{"symbolic-ref refs/remotes/origin/HEAD", "refs/remotes/origin/" + tc.branch},
				{"rev-parse --abbrev-ref @{upstream}", "origin/" + tc.branch},
			} {
				if got := gitIn(t, dir, c.args); got != c.want {
					t.Errorf("git %s = %q, want %q", c.args, got, c.want)
				}
			}
			want := "refs/heads/" + tc.branch + "\nrefs/remotes/origin/HEAD\nrefs/remotes/origin/" + tc.branch
			if refs != want {
				t.Errorf("refs = %q, want %q", refs, want)
			}
		})
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

// gitCmd is git in dir, isolated from the host's own git configuration.
func gitCmd(t *testing.T, dir string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	return cmd
}
