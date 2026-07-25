package sandbox_test

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// pathFault runs the shared shell against one path and returns what it exited
// with. The path travels as an argument, never as script text — the same way
// every backend passes it — so nothing in a path can be read as shell.
func pathFault(t *testing.T, path string, env ...string) int {
	t.Helper()
	cmd := exec.Command("/bin/bash", "-c",
		sandbox.PathFaultShell+"\n__map_path_fault \"$1\"\nexit 0", "map-fault", path)
	if len(env) > 0 {
		cmd.Env = env
	}
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run the path-fault shell: %v", err)
		}
		return ee.ExitCode()
	}
	return 0
}

// The shared path-fault shell is what turns "the sandbox is broken" into "the
// path you asked for cannot exist" on both backends, so its contract is pinned
// here against a real shell rather than left to the two live contract suites: a
// host runs it in milliseconds and stages blocked paths a container makes
// expensive to reach. That the sandbox image carries a userland able to run it is
// the live contract test's job.
func TestPathFaultShell(t *testing.T) {
	run := pathFault
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

// A component whose name ends in a newline is walked as itself. A `$(dirname …)`
// substitution would have eaten those bytes and walked a *different* path — the
// sibling without the newline — and answered for it; the tools accept such names
// (only NUL is rejected), so the shell has to survive them.
func TestPathFaultShellWalksNewlineComponents(t *testing.T) {
	dir := t.TempDir()
	// Two siblings: one a directory, the other the same name plus a newline, a
	// regular file. Confusing them inverts the answer, in both directions.
	if err := os.Mkdir(dir+"/block", 0o755); err != nil {
		t.Fatalf("stage a directory: %v", err)
	}
	if err := os.WriteFile(dir+"/block\n", nil, 0o644); err != nil {
		t.Fatalf("stage its newline-suffixed sibling: %v", err)
	}
	if code := pathFault(t, dir+"/block\n/x/y"); code != sandbox.ExitPathNotDirectory {
		t.Errorf("exit %d for a file with a trailing newline in its name, want %d",
			code, sandbox.ExitPathNotDirectory)
	}
	if code := pathFault(t, dir+"/block/x/y"); code != 0 {
		t.Errorf("exit %d for the directory sibling, want 0", code)
	}
}

// The sandbox filesystem is the agent's, so anything the shell resolves through
// PATH is the agent's to replace. The walk uses builtins only, and this is what
// says so: a `dirname` that echoes its argument back would spin the loop forever,
// and one that lied would answer for it — with PATH pointed at both, the answers
// must not move.
func TestPathFaultShellIgnoresAShadowedDirname(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(bin+"/dirname", []byte("#!/bin/sh\nprintf '%s' \"$1\"\n"), 0o755); err != nil {
		t.Fatalf("stage a shadowing dirname: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/plain", []byte("i am a file"), 0o644); err != nil {
		t.Fatalf("stage a regular file: %v", err)
	}
	env := []string{"PATH=" + bin}
	// The blocked path still exits on the block rather than looping until the
	// test's own deadline, and the clear path is still clear.
	if code := pathFault(t, dir+"/plain/x/y", env...); code != sandbox.ExitPathNotDirectory {
		t.Errorf("exit %d with a shadowed dirname, want %d", code, sandbox.ExitPathNotDirectory)
	}
	if code := pathFault(t, dir+"/fresh/x", env...); code != 0 {
		t.Errorf("exit %d with a shadowed dirname on a clear path, want 0", code)
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
