// Package gitworktree isolates Git operations from the caller's checkout.
package gitworktree

import (
	"os"
	"slices"
	"strings"
)

// LocationVariable reports whether a setting can redirect worktree discovery or state.
func LocationVariable(name string) bool {
	switch strings.ToUpper(name) {
	case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_PREFIX", "GIT_CEILING_DIRECTORIES":
		return true
	}
	return false
}

// Environment lets Git discover the checkout and index from cmd.Dir.
func Environment() []string {
	return slices.DeleteFunc(os.Environ(), func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		return LocationVariable(name)
	})
}
