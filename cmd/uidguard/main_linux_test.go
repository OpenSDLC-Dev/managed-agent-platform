package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGuardMountRequiresDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := checkMount(dir); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "guard")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := checkMount(link); err == nil {
		t.Fatal("accepted a symbolic link as the guard mount")
	}
}
