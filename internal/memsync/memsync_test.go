package memsync_test

import (
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/memsync"
)

// The documented path rules, one case per rejection
// (docs/plan/36_memory-stores.md decision 4, and the OpenAPI spec's
// BetaManagedAgentsCreateMemoryParams.path: "Must start with `/`, contain at
// least one non-empty segment, and be at most 1,024 bytes. Must not contain
// empty segments, `.` or `..` segments, control or format characters, and must
// be NFC-normalized"). Every non-ASCII case is written as an escape: the
// difference between the two spellings of cafe-acute is the whole point of the
// NFD row, and a literal would hide it.
func TestValidatePath(t *testing.T) {
	for name, path := range map[string]string{
		"empty":                     "",
		"no leading slash":          "notes.md",
		"a relative path":           "notes/today.md",
		"the root alone":            "/",
		"a leading empty segment":   "//notes.md",
		"an interior empty segment": "/notes//today.md",
		"a trailing slash":          "/notes/",
		"a dot segment":             "/notes/./today.md",
		"a dot-dot segment":         "/notes/../today.md",
		"a bare dot":                "/.",
		"a bare dot-dot":            "/..",
		"a control character (Cc)":  "/notes/\u0007bell.md",
		"a format character (Cf)":   "/notes/rtl\u200e.md",
		"an NFD path":               "/cafe\u0301.md",
		"invalid UTF-8":             "/notes/\xff.md",
		"1025 bytes":                "/" + strings.Repeat("a", 1024),
		"the marker's path":         "/.anthropic-memory-store",
	} {
		if err := memsync.ValidatePath(path); err == nil {
			t.Errorf("%s (%q): accepted, want a rejection", name, path)
		}
	}

	for name, path := range map[string]string{
		"a single segment":            "/notes.md",
		"a nested path":               "/projects/foo/notes.md",
		"a dotfile":                   "/.gitignore",
		"three dots":                  "/...",
		"the marker below the root":   "/x/.anthropic-memory-store",
		"an NFC path":                 "/caf\u00e9.md",
		"a space":                     "/my notes.md",
		"SQL LIKE metacharacters":     "/a_b/100%",
		"exactly 1024 bytes":          "/" + strings.Repeat("a", 1023),
		"a path that ends in a dot":   "/notes.",
		"a segment named like a flag": "/--force",
	} {
		if err := memsync.ValidatePath(path); err != nil {
			t.Errorf("%s (%q): %v, want it accepted", name, path, err)
		}
	}
}

func TestValidateContent(t *testing.T) {
	for name, content := range map[string]string{
		"empty":                 "",
		"text":                  "hello\nworld\n",
		"exactly 102400 bytes":  strings.Repeat("x", 102400),
		"multibyte at the edge": strings.Repeat("é", 51200), // 2 bytes each
	} {
		if err := memsync.ValidateContent(content); err != nil {
			t.Errorf("%s: %v, want it accepted", name, err)
		}
	}
	for name, content := range map[string]string{
		"one byte over":         strings.Repeat("x", 102401),
		"invalid UTF-8":         "hello \xff world",
		"a lone surrogate half": "\xed\xa0\x80",
		"a NUL byte":            "a\x00b", // valid UTF-8; Postgres text refuses it
	} {
		if err := memsync.ValidateContent(content); err == nil {
			t.Errorf("%s: accepted, want a rejection", name)
		}
	}
}

// The mount slug (decision 8): the documented rule — lowercase, every
// non-alphanumeric run to one hyphen — plus the two readings this platform
// chose where the reference states nothing (the hyphen trim, and ASCII as the
// alphabet), and the fallback for a name that slugs to nothing.
func TestSlug(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"User preferences", "user-preferences"},
		{"notes", "notes"},
		{"NOTES", "notes"},
		{"Notes 2 — Archive", "notes-2-archive"},
		{"(Notes)", "notes"},
		{"a--b", "a-b"},
		{"--a--", "a"},
		{"Ünïcode", "n-code"},
		{"!!!", "memstore-fallback"},
		{"", "memstore-fallback"},
		{"...", "memstore-fallback"},
		{"2026", "2026"},
	} {
		if got := memsync.Slug(tc.name, "memstore-fallback"); got != tc.want {
			t.Errorf("Slug(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}
