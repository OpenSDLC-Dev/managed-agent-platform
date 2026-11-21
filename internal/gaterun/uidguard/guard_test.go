package uidguard

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestStageReplacesExecutableAtomically(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Stage(source, dir); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "uidguard")
	old, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Stage(source, dir); err != nil {
		t.Fatal(err)
	}
	now, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != "second" || os.SameFile(old, now) {
		t.Fatalf("published launcher = %q, %v, same inode = %v", data, err, os.SameFile(old, now))
	}
	if err := Stage(filepath.Join(dir, "missing"), dir); err == nil {
		t.Fatal("staging a missing launcher succeeded")
	}
	if err := Stage(source, filepath.Join(dir, "missing")); err == nil {
		t.Fatal("staging without a volume succeeded")
	}
	if err := Stage(dir, dir); err == nil {
		t.Fatal("staging a directory succeeded")
	}
	data, err = os.ReadFile(dest)
	if err != nil || string(data) != "second" {
		t.Fatalf("failed staging altered the published launcher: %q, %v", data, err)
	}
}

func TestCommandPreservesArguments(t *testing.T) {
	args := []string{"sh", "-c", "echo 'two words'"}
	if got := Command(args); !slices.Equal(got, []string{Path, "sh", "-c", "echo 'two words'"}) {
		t.Errorf("guarded command = %q", got)
	}
}
