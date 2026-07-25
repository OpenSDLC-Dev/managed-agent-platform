package sandbox_test

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// The shared path-fault shell is what turns "the sandbox is broken" into "the
// path you asked for cannot exist" on both backends, so its contract is pinned
// here against a real shell rather than left to the two live contract suites: a
// host runs it in milliseconds and stages blocked paths a container makes
// expensive to reach. That the sandbox image carries a userland able to run it is
// the live contract test's job.
//
// The path travels as an argument, never as script text — the same way both
// backends pass it — so nothing in a path can be read as shell.
func TestPathFaultShell(t *testing.T) {
	run := func(t *testing.T, path string) int {
		t.Helper()
		cmd := exec.Command("/bin/bash", "-c",
			sandbox.PathFaultShell+"\n__map_path_fault \"$1\"\nexit 0", "map-fault", path)
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("run the path-fault shell: %v", err)
			}
			return ee.ExitCode()
		}
		return 0
	}

	dir := t.TempDir()
	if err := os.WriteFile(dir+"/plain", []byte("i am a file"), 0o644); err != nil {
		t.Fatalf("stage a regular file: %v", err)
	}
	if err := os.Mkdir(dir+"/sub", 0o755); err != nil {
		t.Fatalf("stage a directory: %v", err)
	}
	for target, link := range map[string]string{
		dir + "/plain":        dir + "/link-to-file",
		dir + "/sub":          dir + "/link-to-dir",
		dir + "/nothing-here": dir + "/dangling",
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("stage symlink %s: %v", link, err)
		}
	}

	// Every shape of "a non-directory is in the way", at every distance from the
	// leaf. The path itself counts, because a backend asking whether the directory
	// a write needs is usable passes that directory.
	for name, path := range map[string]string{
		"the parent is a regular file":      dir + "/plain/child",
		"a grandparent is a regular file":   dir + "/plain/x/y",
		"the path itself is a regular file": dir + "/plain",
		"the parent is a symlink to a file": dir + "/link-to-file/child",
		"the parent is a dangling symlink":  dir + "/dangling/child",
	} {
		t.Run(name, func(t *testing.T) {
			if code := run(t, path); code != sandbox.ExitPathNotDirectory {
				t.Errorf("exit %d for %s, want %d", code, path, sandbox.ExitPathNotDirectory)
			}
		})
	}

	// Nothing blocks these, so the shell says nothing and whatever the caller's
	// own failure was — a full disk, a read-only mount — is what it reports. A
	// symlink to a directory is a directory: that is what the kernel resolves too.
	for name, path := range map[string]string{
		"an existing directory":               dir + "/sub",
		"a file that does not exist yet":      dir + "/sub/new.txt",
		"a directory that does not exist yet": dir + "/sub/deep/deeper",
		"a symlink to a directory":            dir + "/link-to-dir/child",
		"the root":                            "/",
		"a relative path":                     "relative/child",
	} {
		t.Run(name, func(t *testing.T) {
			if code := run(t, path); code != 0 {
				t.Errorf("exit %d for %s, want 0", code, path)
			}
		})
	}
}

// The name a write lands under: hidden, unique per call, and a single path
// component so it stays in the target's own directory — the property that makes
// the rename atomic.
func TestTempName(t *testing.T) {
	seen := make(map[string]bool, 64)
	for range 64 {
		n := sandbox.TempName()
		switch {
		case !strings.HasPrefix(n, sandbox.TempPrefix):
			t.Fatalf("%q does not carry the prefix the contract suite looks for", n)
		case strings.ContainsAny(n, "/ "):
			t.Fatalf("%q is not a single plain path component", n)
		case seen[n]:
			t.Fatalf("%q was minted twice", n)
		}
		seen[n] = true
	}
}
