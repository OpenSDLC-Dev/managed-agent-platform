package main

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

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
		t.Skip("this volume does not expose a short alias for the fixture")
	}
	for _, paths := range [][2]string{{short, long}, {long, short}} {
		proposed := filepath.Join(paths[1], "src", "planned", "io.go")
		if got := call(t, paths[0], 0, "rules", proposed); !strings.Contains(got, "Source rule.") || !strings.Contains(got, "Storage rule.") {
			t.Fatal(got)
		}
	}
}
