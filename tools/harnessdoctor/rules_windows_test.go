package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestWindowsJunctionToLinkedWorktree(t *testing.T) {
	original := fixture(t)
	put(t, original, "src/internal/io.go", "fixture\n")
	gitTest(t, original, "add", ".")
	gitTest(t, original, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "seed linked checkout")
	parent := t.TempDir()
	linked := filepath.Join(parent, "linked checkout")
	gitTest(t, original, "worktree", "add", "--quiet", "--detach", linked)
	makeJunction := func(dir, link, target string) {
		t.Helper()
		cmd := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", link, target)
		cmd.Dir = dir
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("create fixture junction: %v: %s", err, b)
		}
	}
	makeJunction(parent, "entry", filepath.Join("linked checkout", "src", "internal"))
	alias := filepath.Join(parent, "entry")
	for _, file := range []string{"io.go", "io-planned.go", filepath.Join("new", "io.go")} {
		if got := call(t, alias, 0, "rules", file); !strings.Contains(got, "Source rule.") || !strings.Contains(got, "Storage rule.") {
			t.Fatal(got)
		}
	}
	// A second repository has the same relative paths. Also check the main
	// checkout which shares the linked worktree's Git object database.
	foreign := filepath.Join(parent, "foreign")
	put(t, foreign, "src/internal/io.go", "foreign\n")
	gitTest(t, foreign, "init", "-q")
	makeJunction(linked, "foreign-entry", filepath.Join("..", "foreign", "src", "internal"))
	for _, file := range []string{
		filepath.Join(linked, "foreign-entry", "io.go"),
		filepath.Join(linked, "foreign-entry", "new", "io.go"),
		filepath.Join(original, "src", "internal", "io.go"),
	} {
		if got := call(t, alias, 1, "rules", file); !strings.Contains(got, "outside current worktree") {
			t.Fatal(got)
		}
	}
	if err := os.Remove(filepath.Join(linked, "foreign-entry")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsMixedLongAndShortRulePaths(t *testing.T) {
	root := fixture(t)
	long, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	input, err := syscall.UTF16PtrFromString(long)
	if err != nil {
		t.Fatal(err)
	}
	size, err := syscall.GetShortPathName(input, nil, 0)
	if err != nil || size == 0 {
		t.Fatalf("GetShortPathName buffer size: %d (%v)", size, err)
	}
	buf := make([]uint16, size)
	if _, err := syscall.GetShortPathName(input, &buf[0], size); err != nil {
		t.Fatal(err)
	}
	short := syscall.UTF16ToString(buf)
	if strings.EqualFold(short, long) {
		t.Skip("this volume does not expose a short alias for the fixture") // hygiene:allow 8.3 alias creation is a Windows volume setting
	}
	for _, paths := range [][2]string{{short, long}, {long, short}} {
		proposed := filepath.Join(paths[1], "src", "planned", "io.go")
		if got := call(t, paths[0], 0, "rules", proposed); !strings.Contains(got, "Source rule.") || !strings.Contains(got, "Storage rule.") {
			t.Fatal(got)
		}
	}
}
