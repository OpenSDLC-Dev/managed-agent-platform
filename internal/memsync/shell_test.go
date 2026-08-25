//go:build !windows

package memsync_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/memsync"
)

// The two commands run under the host's bash the way both sandbox backends
// run them, against a real directory — the contract the string pins in
// tree_test.go cannot check. Skipped where bash or GNU coreutils are missing.

func shellPrereqs(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"bash", "find", "sort", "xargs", "sha256sum", "rmdir"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on PATH", tool)
		}
	}
	if err := exec.Command("sha256sum", "-z", "/dev/null").Run(); err != nil {
		t.Skip("sha256sum lacks -z: not GNU coreutils")
	}
}

func runBash(t *testing.T, cmd string) (stdout, stderr string, code int) {
	t.Helper()
	c := exec.Command("bash", "-c", cmd)
	var out, errs strings.Builder
	c.Stdout, c.Stderr = &out, &errs
	err := c.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case asExitError(err, &exit):
		code = exit.ExitCode()
	default:
		t.Fatalf("bash -c %q: %v", cmd, err)
	}
	return out.String(), errs.String(), code
}

func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

func plant(t *testing.T, mount string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(mount, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func TestHashTreeCommandUnderBash(t *testing.T) {
	shellPrereqs(t)
	mount := t.TempDir()
	plant(t, mount, map[string]string{memsync.MarkerName: "version 1\nmemstore_x", "a/b.md": "hello", "c.md": "world"})
	digest := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return hex.EncodeToString(sum[:])
	}

	stdout, stderr, code := runBash(t, memsync.HashTreeCommand(mount))
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	got, err := memsync.ParseHashTree([]byte(stdout))
	if err != nil {
		t.Fatalf("parse %q: %v", stdout, err)
	}
	if len(got) != 2 || got["/a/b.md"] != digest("hello") || got["/c.md"] != digest("world") {
		t.Fatalf("listing = %v", got)
	}

	// An absent mount is an empty listing that exits 0 and says nothing.
	stdout, stderr, code = runBash(t, memsync.HashTreeCommand(filepath.Join(mount, "none")))
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("absent mount: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	// A mount replaced by a symlink — to anywhere — is refused, not listed
	// through: the files behind the link are somebody else's.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(mount, link); err != nil {
		t.Fatal(err)
	}
	stdout, _, code = runBash(t, memsync.HashTreeCommand(link))
	if code != 3 || stdout != "" {
		t.Fatalf("symlinked mount: exit %d, stdout %q; want exit 3 and nothing", code, stdout)
	}

	// A shell that warns on stderr — an image whose locale is not installed
	// — still lists: only the exit status is judged.
	c := exec.Command("bash", "-c", memsync.HashTreeCommand(mount))
	c.Env = append(os.Environ(), "LC_ALL=xx_XX.UTF-8")
	var warned strings.Builder
	c.Stderr = &warned
	out, err := c.Output()
	if err != nil {
		t.Fatalf("under a broken locale (stderr %q): %v", warned.String(), err)
	}
	if got, err := memsync.ParseHashTree(out); err != nil || len(got) != 2 {
		t.Fatalf("under a broken locale (stderr %q): listing %v, %v", warned.String(), got, err)
	}

	// A directory find cannot enter fails the command under pipefail —
	// without it the pipeline's status would be xargs's 0 and the files it
	// hid would read as deleted. Root enters anything, so only a non-root
	// run can pin it.
	if os.Geteuid() == 0 {
		t.Log("running as root: the unreadable-directory case cannot be exercised")
		return
	}
	locked := filepath.Join(mount, "a")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	stdout, stderr, code = runBash(t, memsync.HashTreeCommand(mount))
	if code == 0 {
		t.Fatalf("an unreadable directory listed with exit 0: stdout %q, stderr %q", stdout, stderr)
	}
	if strings.Contains(stdout, "./a/b.md") {
		t.Fatalf("the unreadable directory's file was listed: %q", stdout)
	}
}

func TestRemoveCommandsUnderBash(t *testing.T) {
	shellPrereqs(t)
	mount := t.TempDir()
	plant(t, mount, map[string]string{memsync.MarkerName: "version 1\nmemstore_x", "a/b/c.md": "1", "a/d.md": "2", "e.md": "3"})
	run := func(paths ...string) {
		t.Helper()
		for _, cmd := range memsync.RemoveCommands(mount, paths) {
			if _, stderr, code := runBash(t, cmd); code != 0 {
				t.Fatalf("%s: exit %d, stderr %q", cmd, code, stderr)
			}
		}
	}

	// `/a/b/c.md` and `/e.md` go, and `a/b` with them; `a` stays for `a/d.md`.
	run("/a/b/c.md", "/e.md")
	for p, want := range map[string]bool{"a/b/c.md": false, "a/b": false, "e.md": true, "a/d.md": true, "a": true} {
		if want {
			continue
		}
		if exists(filepath.Join(mount, p)) {
			t.Errorf("%s still exists", p)
		}
	}
	if exists(filepath.Join(mount, "e.md")) || !exists(filepath.Join(mount, "a/d.md")) {
		t.Fatal("the wrong file went")
	}
	// The last file under `a` takes `a` with it; the mount and its marker stay.
	run("/a/d.md")
	if exists(filepath.Join(mount, "a")) {
		t.Error("an emptied directory was left behind")
	}
	if !exists(filepath.Join(mount, memsync.MarkerName)) {
		t.Fatal("the mount or its marker went")
	}
	// A store-side `/a/d` deleted and `/a` created: the pull that follows
	// finds a file's place, not a directory's.
	if err := os.WriteFile(filepath.Join(mount, "a"), []byte("new"), 0o644); err != nil {
		t.Fatalf("the removed directory's path is not writable as a file: %v", err)
	}
	// An unlink that fails fails the command, so the caller keeps its baseline.
	if err := os.Mkdir(filepath.Join(mount, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmds := memsync.RemoveCommands(mount, []string{"/x"})
	if _, _, code := runBash(t, cmds[0]); code == 0 {
		t.Fatal("removing a directory as a file succeeded")
	}
	// A mount replaced by a symlink is refused before anything is unlinked:
	// a store-side deletion never reaches through it.
	elsewhere := t.TempDir()
	plant(t, elsewhere, map[string]string{"victim.md": "not the store's"})
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runBash(t, memsync.RemoveCommands(link, []string{"/victim.md"})[0]); code != 1 {
		t.Fatalf("removal through a symlinked mount exited %d, want 1", code)
	}
	if !exists(filepath.Join(elsewhere, "victim.md")) {
		t.Fatal("a removal followed the symlinked mount")
	}
}
