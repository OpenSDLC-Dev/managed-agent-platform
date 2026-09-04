//go:build !windows

package memsync_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/memsync"
)

// The two commands run under the host's bash the way both sandbox backends
// run them, against a real directory — the contract the string pins in
// tree_test.go cannot check. hashTreePrereqs and removePrereqs decide whether
// the host can run each of them.

// requirePrereq reports one missing prerequisite: a skip where the host cannot
// be expected to carry the toolchain these commands are written for, and a
// failure on Linux, where it can. Skipping there would leave the package
// reporting full coverage with the contract unexercised — the daemon-and-cluster
// rule the rest of the suite keeps, for the same reason. The trade is not free,
// and is taken deliberately: a Linux host without these tools, a BusyBox image
// somebody develops in, gets a red `make verify` with no way to opt out. The
// runner that has to be bound is CI's, and it is Linux carrying GNU.
func requirePrereq(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		return
	}
	const msg = "%s is unavailable (%v); these commands need GNU coreutils and findutils, or an implementation carrying the same flags"
	if runtime.GOOS == "linux" {
		t.Fatalf(msg, what, err)
	}
	t.Skipf(msg, what, err)
}

// hashTreePrereqs and removePrereqs probe what POSIX does not guarantee — the
// six extensions these two shell strings are built from, and bash — each for
// its own command and no other's. What POSIX does guarantee goes unprobed
// (`rm -f --`, `find -type f`, `[ -d ]`, `cd -P`): a host carrying bash and
// lacking those is not one this suite could describe anyway. Probing per
// command is what costs no coverage: macOS runs the hash tree perfectly well
// and lacks only GNU `rmdir`, so a shared set skips a test whose own tools are
// all present.
//
// Probing one flag as representative is what let this suite run against a
// toolchain it could not survive (#554): macOS ships a `sha256sum (Darwin)`
// satisfying `-z`, and an Apple `sort` taking `-z` too, so both cleared a probe
// of `sha256sum -z` alone — while BSD `rmdir` has no
// `--ignore-fail-on-non-empty`. Probing the flags rather than the vendor string
// matters in the other direction too: Ubuntu 25.10 already ships uutils in place
// of GNU coreutils, so its `sort`, `sha256sum` and `rmdir` answer `--version`
// with "uutils coreutils" and carry the flags anyway. A check for "GNU" would
// skip the suite on a host that passes every probe — the same silent hollowing,
// pointed the other way.
//
// A probe asks "can this run"; whether a flag is honored is mostly left to the
// assertions below, which the hash tree's own `set -o pipefail` and its
// NUL-delimited parse make sharp — a `-print0`, `-0` or `-z` that does not
// delimit fails there. Three cases reach no assertion, so those three probes
// check behavior instead. No fixture hands the pipeline an empty file list (the
// absent mount exits before it), so an `xargs` that runs `sha256sum` on empty
// stdin anyway would go unseen. The listing's byte order is promised by
// HashTreeCommand and asserted by nothing, the tests below comparing it as a
// map, so a `sort` that takes `-z` and does not sort would pass. And
// RemoveCommands has no `pipefail` at all: it swallows `rmdir`'s stderr
// (`2>/dev/null; exit 0`), so an implementation that takes the flags and prunes
// nothing arrives as a surviving directory rather than as an error — precisely
// how #554 mis-reported itself.
func hashTreePrereqs(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	requirePrereq(t, "bash", exec.Command("bash", "-c", "exit 0").Run())
	requirePrereq(t, "find -print0", exec.Command("find", dir, "-type", "f", "-print0").Run())
	// `sort -z` has to be shown to sort, and to sort in byte order, because
	// nothing downstream would notice either failing: the listing's order is
	// what HashTreeCommand promises, and the tests below compare it as a map.
	// `a` and `B` are the pair that separates the two — under LC_ALL=C, which
	// production sets, `B` (0x42) leads; under a collating locale `a` does. So
	// one input catches both a sort that ignores the flag and a sort that
	// ignores the locale, each of them returning the input untouched.
	order := exec.Command("sort", "-z")
	order.Env = append(os.Environ(), "LC_ALL=C")
	order.Stdin = strings.NewReader("a\x00B\x00")
	switch out, err := order.Output(); {
	case err != nil:
		requirePrereq(t, "sort -z", err)
	case string(out) != "B\x00a\x00":
		requirePrereq(t, "a `sort -z` that sorts in byte order", fmt.Errorf("returned %q", out))
	}
	// `false`, not `true`, and the behavior probed is the one the hash tree
	// needs rather than the flag: on a mount holding no files, xargs must not
	// run sha256sum at all. `false` exits 0 only if the command never ran, so it
	// separates that from an xargs that runs it anyway; `true` exits 0 either
	// way. Whether `-r` is what produced the behavior is not the question — BSD
	// xargs takes the flag, ignores it, and skips empty input regardless.
	requirePrereq(t, "xargs -0r", exec.Command("xargs", "-0r", "false").Run())
	requirePrereq(t, "sha256sum -z", exec.Command("sha256sum", "-z", "/dev/null").Run())
}

func removePrereqs(t *testing.T) {
	t.Helper()
	requirePrereq(t, "bash", exec.Command("bash", "-c", "exit 0").Run())
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "prune", "me"), 0o755); err != nil {
		t.Fatal(err)
	}
	// `rmdir -p` climbs its whole argument, so the probe hands it a relative
	// path from inside the fixture — which is how production bounds the same
	// walk, after a `cd -P` into the mount (RemoveCommands says so: the pruning
	// never reaches the mount because a relative `rmdir -p` cannot). An absolute
	// argument would climb out of the fixture and take the temp root with it,
	// failing every later t.TempDir() in the binary.
	prune := exec.Command("rmdir", "-p", "--ignore-fail-on-non-empty", "--", filepath.Join("prune", "me"))
	prune.Dir = dir
	var why strings.Builder
	prune.Stderr = &why
	err := prune.Run()
	_, stat := os.Stat(filepath.Join(dir, "prune"))
	switch {
	case stat == nil:
		// What the probe can prove is that this host has no prune that works,
		// and that is what it reports. It cannot prove why: BSD's rmdir has no
		// `--ignore-fail-on-non-empty`, but a filesystem that will not remove
		// the fixture — an NFS silly-rename, a handle still held — looks
		// identical from here, and naming a coreutils vendor for that would
		// send the reader after a problem that does not exist.
		requirePrereq(t, "an `rmdir -p --ignore-fail-on-non-empty` that prunes",
			fmt.Errorf("prune/ still stands, %v: %s", err, strings.TrimSpace(why.String())))
	case !errors.Is(stat, fs.ErrNotExist):
		t.Fatalf("stat probe fixture: %v", stat)
	case err != nil:
		// Pruned, and still nonzero: not a missing tool, so not a skip.
		t.Fatalf("rmdir pruned the fixture yet exited %v: %s", err, strings.TrimSpace(why.String()))
	}
}

// physicalTempDir is t.TempDir() with its symlinks resolved, which is what a
// mount these commands are meant to accept has to be: both guard themselves
// with `cd -P` and a $PWD comparison (memsync.mountPrelude), so a directory
// reached through a link is refused rather than walked. macOS puts TMPDIR under
// /var by default, a link to /private/var, so a fixture taken from there
// unresolved makes the guard refuse it and the tests below fail before reaching
// the contract they pin (#554). The tests that mean to reach the refusal build a
// symlink of their own and pass it unresolved.
func physicalTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve %s: %v", dir, err)
	}
	return resolved
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
	hashTreePrereqs(t)
	mount := physicalTempDir(t)
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
	removePrereqs(t)
	mount := physicalTempDir(t)
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
	for p, want := range map[string]bool{"a/b/c.md": false, "a/b": false, "e.md": false, "a/d.md": true, "a": true} {
		if got := exists(filepath.Join(mount, p)); got != want {
			t.Errorf("%s exists = %v, want %v", p, got, want)
		}
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
