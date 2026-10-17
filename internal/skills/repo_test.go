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
	long := strings.Repeat("é", 2500)
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
		{"at the cap", "---\ndescription: " + long[:2*2000] + "\n---\n", strings.Repeat("é", 2000)},
		{"over the cap", "---\ndescription: " + long + "\n---\n", strings.Repeat("é", 2000)},
		// A name YAML cannot read as a string leaves the description.
		{"non-string name", "---\nname:\n  - a\ndescription: Kept.\n---\n", "Kept."},
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

// TestRepoName pins the name a repository skill is announced under: its
// frontmatter's, as the reference was recorded announcing `other-name` from a
// directory named dir-name (managed-agents-wire-recordings
// 2026-10-15-repo-followups, setup[14]), and "" for the caller to take the
// directory's where the frontmatter gives none usable (ours: unrecorded).
func TestRepoName(t *testing.T) {
	for _, c := range []struct {
		name, md, want string
	}{
		{"valid", "---\nname: other-name\ndescription: Marker.\n---\n", "other-name"},
		{"bom and crlf", "\ufeff---\r\nname: windows\r\n---\r\n", "windows"},
		{"spaces kept", "---\nname: My Skill\n---\n", "My Skill"},
		{"at the upload cap", "---\nname: " + strings.Repeat("n", 64) + "\n---\n", strings.Repeat("n", 64)},
		{"over the upload cap", "---\nname: " + strings.Repeat("n", 65) + "\n---\n", ""},
		{"no frontmatter", "# Broken\n", ""},
		{"no name", "---\ndescription: Nameless.\n---\n", ""},
		{"empty name", "---\nname: \"\"\n---\n", ""},
		{"non-string name", "---\nname:\n  - a\n---\n", ""},
		{"newline", "---\nname: \"a\\nb\"\n---\n", ""},
		{"nul", "---\nname: \"a\\0b\"\n---\n", ""},
		{"invalid yaml", "---\nname: [unclosed\n---\n", ""},
		// A description YAML cannot read as a string leaves the name.
		{"non-string description", "---\nname: kept\ndescription:\n  - a\n---\n", "kept"},
	} {
		if got := RepoName([]byte(c.md)); got != c.want {
			t.Errorf("%s: RepoName = %q, want %q", c.name, got, c.want)
		}
	}
}
