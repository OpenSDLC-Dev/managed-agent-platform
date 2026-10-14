package skills

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestRepoDescription pins the lenient read of a repository skill: the
// recorded fixture listed a SKILL.md with no frontmatter (broken) and one with
// no description (nodesc), each with an empty description, so neither is an
// error here.
func TestRepoDescription(t *testing.T) {
	long := strings.Repeat("é", 1500)
	for _, c := range []struct {
		name, md, want string
	}{
		{"valid", "---\nname: alpha\ndescription: Alpha marker.\n---\nbody\n", "Alpha marker."},
		{"bom and crlf", "\ufeff---\r\ndescription: Windows.\r\n---\r\n", "Windows."},
		{"no frontmatter", "# Broken\n\nno frontmatter here\n", ""},
		{"no description", "---\nname: nodesc\n---\nbody\n", ""},
		{"unclosed frontmatter", "---\ndescription: never closed\n", ""},
		{"invalid yaml", "---\ndescription: [unclosed\n---\n", ""},
		{"non-string description", "---\ndescription:\n  - a\n  - b\n---\n", ""},
		{"nul", "---\ndescription: \"a\\0b\"\n---\n", "ab"},
		{"invalid utf-8", "---\ndescription: a\xffb\n---\n", ""},
		{"over the cap", "---\ndescription: " + long + "\n---\n", strings.Repeat("é", 1024)},
	} {
		got := RepoDescription([]byte(c.md))
		if got != c.want {
			t.Errorf("%s: RepoDescription = %q, want %q", c.name, got, c.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("%s: result is not valid UTF-8: %q", c.name, got)
		}
	}
}
