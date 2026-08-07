package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A small stand-in for the real CHANGELOG.md: preamble, a legacy [Unreleased]
// body carrying its own ### groups (the pre-fragment world), one released
// section, and the Keep-a-Changelog link-reference block.
const legacyChangelog = `# Changelog

Preamble line.

## [Unreleased]

### Added

- **Legacy entry one** — text.

### Fixed

- Legacy fix.

## [0.1.0] - 2026-07-17

Release summary.

### Added

- Old entry.

[Unreleased]: https://github.com/OpenSDLC-Dev/managed-agent-platform/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/OpenSDLC-Dev/managed-agent-platform/releases/tag/v0.1.0
`

// The steady state after the first assembled release: [Unreleased] holds only
// the pointer paragraph.
var steadyChangelog = `# Changelog

Preamble line.

## [Unreleased]

` + unreleasedPointer + `

## [0.2.0] - 2026-08-07

### Added

- A added.

## [0.1.0] - 2026-07-17

Release summary.

[Unreleased]: https://github.com/OpenSDLC-Dev/managed-agent-platform/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/OpenSDLC-Dev/managed-agent-platform/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/OpenSDLC-Dev/managed-agent-platform/releases/tag/v0.1.0
`

func writeFixture(t *testing.T, changelog string, frags map[string]string) (clPath, dir string) {
	t.Helper()
	root := t.TempDir()
	clPath = filepath.Join(root, "CHANGELOG.md")
	if err := os.WriteFile(clPath, []byte(changelog), 0o644); err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join(root, "changelog.d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range frags {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return clPath, dir
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The first assembled release: fragments become KaC groups at the top of the
// new section, and the legacy [Unreleased] body follows them byte-for-byte.
func TestAssembleFirstRelease(t *testing.T) {
	clPath, dir := writeFixture(t, legacyChangelog, map[string]string{
		"b-slug.added.md": "- B added.\n",
		"a-slug.added.md": "- A added.\n",
		"c-slug.fixed.md": "- C fixed.\n",
		"README.md":       "not a fragment\n",
	})
	if err := runAssemble(clPath, dir, "0.2.0", "2026-08-07"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, clPath)

	want := `# Changelog

Preamble line.

## [Unreleased]

` + unreleasedPointer + `

## [0.2.0] - 2026-08-07

### Added

- A added.

- B added.

### Fixed

- C fixed.

### Added

- **Legacy entry one** — text.

### Fixed

- Legacy fix.

## [0.1.0] - 2026-07-17

Release summary.

### Added

- Old entry.

[Unreleased]: https://github.com/OpenSDLC-Dev/managed-agent-platform/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/OpenSDLC-Dev/managed-agent-platform/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/OpenSDLC-Dev/managed-agent-platform/releases/tag/v0.1.0
`
	if got != want {
		t.Errorf("assembled CHANGELOG mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}

	// Fragments are consumed; README.md survives.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if len(left) != 1 || left[0] != "README.md" {
		t.Errorf("changelog.d after assemble = %v, want only README.md", left)
	}
}

// The legacy body must survive the move byte-for-byte.
func TestAssembleLegacyBodyByteIdentical(t *testing.T) {
	clPath, dir := writeFixture(t, legacyChangelog, map[string]string{
		"a.added.md": "- A added.\n",
	})
	if err := runAssemble(clPath, dir, "0.2.0", "2026-08-07"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, clPath)
	legacyBody := "### Added\n\n- **Legacy entry one** — text.\n\n### Fixed\n\n- Legacy fix."
	if !strings.Contains(got, legacyBody) {
		t.Errorf("legacy body not preserved byte-for-byte in:\n%s", got)
	}
}

// Steady state: pointer-only [Unreleased] contributes no tail.
func TestAssembleSteadyState(t *testing.T) {
	clPath, dir := writeFixture(t, steadyChangelog, map[string]string{
		"x.fixed.md": "- X fixed.\n",
	})
	if err := runAssemble(clPath, dir, "0.2.1", "2026-09-01"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, clPath)
	want := `# Changelog

Preamble line.

## [Unreleased]

` + unreleasedPointer + `

## [0.2.1] - 2026-09-01

### Fixed

- X fixed.

## [0.2.0] - 2026-08-07

### Added

- A added.

## [0.1.0] - 2026-07-17

Release summary.

[Unreleased]: https://github.com/OpenSDLC-Dev/managed-agent-platform/compare/v0.2.1...HEAD
[0.2.1]: https://github.com/OpenSDLC-Dev/managed-agent-platform/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/OpenSDLC-Dev/managed-agent-platform/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/OpenSDLC-Dev/managed-agent-platform/releases/tag/v0.1.0
`
	if got != want {
		t.Errorf("assembled CHANGELOG mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// All six KaC groups, in the canonical order regardless of filename order.
func TestAssembleKacGroupOrder(t *testing.T) {
	clPath, dir := writeFixture(t, steadyChangelog, map[string]string{
		"a.security.md":   "- S.\n",
		"b.fixed.md":      "- F.\n",
		"c.removed.md":    "- R.\n",
		"d.deprecated.md": "- D.\n",
		"e.changed.md":    "- C.\n",
		"f.added.md":      "- A.\n",
	})
	if err := runAssemble(clPath, dir, "0.3.0", "2026-09-01"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, clPath)
	section := "## [0.3.0] - 2026-09-01\n\n### Added\n\n- A.\n\n### Changed\n\n- C.\n\n### Deprecated\n\n- D.\n\n### Removed\n\n- R.\n\n### Fixed\n\n- F.\n\n### Security\n\n- S.\n"
	if !strings.Contains(got, section) {
		t.Errorf("KaC group order wrong in:\n%s", got)
	}
}

// Nothing to release: pointer-only body and an empty changelog.d.
func TestAssembleNothingToRelease(t *testing.T) {
	clPath, dir := writeFixture(t, steadyChangelog, nil)
	if err := runAssemble(clPath, dir, "0.2.1", "2026-09-01"); err == nil {
		t.Fatal("want error for nothing to release")
	}
	if got := readFile(t, clPath); got != steadyChangelog {
		t.Error("CHANGELOG modified on refused assemble")
	}
}

// A legacy body with no fragments is still releasable (the v0.2.0 shape if no
// fragment lands between the mechanism and the cut).
func TestAssembleLegacyOnly(t *testing.T) {
	clPath, dir := writeFixture(t, legacyChangelog, nil)
	if err := runAssemble(clPath, dir, "0.2.0", "2026-08-07"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, clPath)
	if !strings.Contains(got, "## [0.2.0] - 2026-08-07\n\n### Added\n\n- **Legacy entry one** — text.") {
		t.Errorf("legacy-only release missing moved body:\n%s", got)
	}
}

func TestAssembleRefusals(t *testing.T) {
	cases := []struct {
		name      string
		changelog string
		frags     map[string]string
		version   string
		date      string
	}{
		{"version exists", steadyChangelog, map[string]string{"a.added.md": "- A.\n"}, "0.2.0", "2026-09-01"},
		{"bad version", steadyChangelog, map[string]string{"a.added.md": "- A.\n"}, "v0.3.0", "2026-09-01"},
		{"bad date", steadyChangelog, map[string]string{"a.added.md": "- A.\n"}, "0.3.0", "yesterday"},
		{"unknown section suffix", steadyChangelog, map[string]string{"a.wat.md": "- A.\n"}, "0.3.0", "2026-09-01"},
		{"no section suffix", steadyChangelog, map[string]string{"a.md": "- A.\n"}, "0.3.0", "2026-09-01"},
		{"empty fragment", steadyChangelog, map[string]string{"a.added.md": "\n"}, "0.3.0", "2026-09-01"},
		{"fragment not a bullet", steadyChangelog, map[string]string{"a.added.md": "prose, not a list entry\n"}, "0.3.0", "2026-09-01"},
		{"fragment with heading", steadyChangelog, map[string]string{"a.added.md": "- A.\n### sneaky heading\n"}, "0.3.0", "2026-09-01"},
		{"no unreleased section", "# Changelog\n\n## [0.1.0] - 2026-07-17\n\n- Old.\n\n[0.1.0]: https://example.com/releases/tag/v0.1.0\n", map[string]string{"a.added.md": "- A.\n"}, "0.3.0", "2026-09-01"},
		{"no unreleased link ref", "# Changelog\n\n## [Unreleased]\n\n### Added\n\n- L.\n", map[string]string{"a.added.md": "- A.\n"}, "0.3.0", "2026-09-01"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clPath, dir := writeFixture(t, tc.changelog, tc.frags)
			if err := runAssemble(clPath, dir, tc.version, tc.date); err == nil {
				t.Fatal("want error")
			}
			if got := readFile(t, clPath); got != tc.changelog {
				t.Error("CHANGELOG modified on refused assemble")
			}
			for name := range tc.frags {
				if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
					t.Errorf("fragment %s deleted on refused assemble", name)
				}
			}
		})
	}
}

// Fragments order newest-first by the commit that added them; an uncommitted
// fragment counts as newest of all. Names are chosen so neither lexicographic
// direction accidentally matches.
func TestFragmentOrderByGitAddTime(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "changelog.d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(date string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date,
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("2026-01-01T00:00:00Z", "init", "-q")
	git("2026-01-01T00:00:00Z", "config", "user.email", "t@example.com")
	git("2026-01-01T00:00:00Z", "config", "user.name", "t")

	write := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("- "+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("older.added.md")
	git("2026-01-01T00:00:00Z", "add", ".")
	git("2026-01-01T00:00:00Z", "commit", "-q", "-m", "older")
	write("newer.added.md")
	git("2026-02-01T00:00:00Z", "add", ".")
	git("2026-02-01T00:00:00Z", "commit", "-q", "-m", "newer")
	write("aa-uncommitted.added.md")

	frags, err := loadFragments(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range frags {
		names = append(names, f.name)
	}
	want := []string{"aa-uncommitted.added.md", "newer.added.md", "older.added.md"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", names, want)
	}
}

// Two [Unreleased] headings would silently release only the first body while
// still deleting every fragment — refuse instead.
func TestAssembleDuplicateUnreleased(t *testing.T) {
	dup := "# Changelog\n\n## [Unreleased]\n\n### Added\n\n- A.\n\n## [Unreleased]\n\n- Stranded.\n\n[Unreleased]: https://github.com/o/r/compare/v0.1.0...HEAD\n"
	clPath, dir := writeFixture(t, dup, map[string]string{"a.added.md": "- A.\n"})
	if err := runAssemble(clPath, dir, "0.2.0", "2026-08-07"); err == nil {
		t.Fatal("want error for duplicate [Unreleased] headings")
	}
	if got := readFile(t, clPath); got != dup {
		t.Error("CHANGELOG modified on refused assemble")
	}
	if _, err := os.Stat(filepath.Join(dir, "a.added.md")); err != nil {
		t.Error("fragment deleted on refused assemble")
	}
}

// A released section body may legitimately end with a reference-definition
// line sitting directly above the global link-reference block; only
// [Unreleased] and X.Y.Z labels belong to the block.
func TestBodyFinalRefLineSurvives(t *testing.T) {
	cl := `# Changelog

## [Unreleased]

` + unreleasedPointer + `

## [0.1.0] - 2026-07-17

- Entry using a [ref link][details].

[details]: https://example.com/details
[Unreleased]: https://github.com/o/r/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/o/r/releases/tag/v0.1.0
`
	clPath, _ := writeFixture(t, cl, nil)
	out := filepath.Join(t.TempDir(), "notes.md")
	if err := runNotes(clPath, "0.1.0", out); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, out); !strings.Contains(got, "[details]: https://example.com/details") {
		t.Errorf("body-final reference line swallowed by the link-ref block: %q", got)
	}
}

// Leading zeros are not SemVer, impossible dates are not dates, and both must
// refuse before anything is written.
func TestAssembleRejectsSloppyMetadata(t *testing.T) {
	for _, tc := range []struct{ version, date string }{
		{"0.2.01", "2026-09-01"},
		{"01.2.0", "2026-09-01"},
		{"0.3.0", "2026-02-30"},
		{"0.3.0", "2026-13-01"},
	} {
		clPath, dir := writeFixture(t, steadyChangelog, map[string]string{"a.added.md": "- A.\n"})
		if err := runAssemble(clPath, dir, tc.version, tc.date); err == nil {
			t.Errorf("version=%q date=%q: want error", tc.version, tc.date)
		}
		if got := readFile(t, clPath); got != steadyChangelog {
			t.Errorf("version=%q date=%q: CHANGELOG modified on refused assemble", tc.version, tc.date)
		}
	}
}

// An indented `### Fixed` is still a markdown heading (up to three leading
// spaces) and must be refused — while a continuation line starting with a
// bare issue reference like `#123` is legitimate entry text.
func TestFragmentHeadingGuard(t *testing.T) {
	clPath, dir := writeFixture(t, steadyChangelog, map[string]string{
		"a.added.md": "- A.\n  ### Fixed\n",
	})
	if err := runAssemble(clPath, dir, "0.3.0", "2026-09-01"); err == nil {
		t.Fatal("want error for indented heading inside a fragment")
	}

	clPath, dir = writeFixture(t, steadyChangelog, map[string]string{
		"a.added.md": "- A fix for\n  #123 and friends.\n",
	})
	if err := runAssemble(clPath, dir, "0.3.0", "2026-09-01"); err != nil {
		t.Fatalf("bare #123 continuation refused: %v", err)
	}
	if got := readFile(t, clPath); !strings.Contains(got, "- A fix for\n  #123 and friends.") {
		t.Error("continuation line not preserved")
	}
}

// Interior bytes and trailing spaces (markdown hard breaks) survive; only
// surrounding newlines are trimmed — the hard break sits on the fragment's
// FINAL line precisely so a whole-file TrimSpace would strip it and fail.
func TestFragmentTrailingSpacesPreserved(t *testing.T) {
	clPath, dir := writeFixture(t, steadyChangelog, map[string]string{
		"a.added.md": "- An interior break  \n  and a final-line hard break  \n",
	})
	if err := runAssemble(clPath, dir, "0.3.0", "2026-09-01"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, clPath)
	if !strings.Contains(got, "- An interior break  \n  and a final-line hard break  \n") {
		t.Error("trailing spaces inside or at the end of the fragment were not preserved")
	}
}

// A fragment name deleted by an earlier release and reused later must sort by
// the commit that added the CURRENT file, not the historical first add.
func TestReusedFragmentNameSortsByCurrentAdd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "changelog.d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(date string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date,
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name string) {
		// git rm prunes the then-empty directory, so recreate it.
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("- "+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("2026-01-01T00:00:00Z", "init", "-q")
	git("2026-01-01T00:00:00Z", "config", "user.email", "t@example.com")
	git("2026-01-01T00:00:00Z", "config", "user.name", "t")
	// reused.added.md is born old, deleted (a release), then reborn newest.
	write("reused.added.md")
	git("2026-01-01T00:00:00Z", "add", ".")
	git("2026-01-01T00:00:00Z", "commit", "-q", "-m", "old add")
	git("2026-02-01T00:00:00Z", "rm", "-q", "changelog.d/reused.added.md")
	git("2026-02-01T00:00:00Z", "commit", "-q", "-m", "release deletes it")
	write("middle.added.md")
	git("2026-03-01T00:00:00Z", "add", ".")
	git("2026-03-01T00:00:00Z", "commit", "-q", "-m", "middle")
	write("reused.added.md")
	git("2026-04-01T00:00:00Z", "add", ".")
	git("2026-04-01T00:00:00Z", "commit", "-q", "-m", "reborn")

	frags, err := loadFragments(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range frags {
		names = append(names, f.name)
	}
	want := []string{"reused.added.md", "middle.added.md"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v (reborn file must use its current add time)", names, want)
	}
}

// OS/editor droppings in changelog.d/ (gitignored, invisible in git status)
// must not block a release; dot-entries are noise, not typo'd fragments.
func TestAssembleSkipsDotEntries(t *testing.T) {
	clPath, dir := writeFixture(t, steadyChangelog, map[string]string{
		"a.added.md": "- A.\n",
		".DS_Store":  "junk",
	})
	if err := os.Mkdir(filepath.Join(dir, ".cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runAssemble(clPath, dir, "0.3.0", "2026-09-01"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".DS_Store")); err != nil {
		t.Error(".DS_Store was deleted; only fragments are consumed")
	}
}

// A typo'd VERSION below the latest release would write a reversed compare
// range and regress the [Unreleased] base — refuse it.
func TestAssembleRefusesVersionRegression(t *testing.T) {
	clPath, dir := writeFixture(t, steadyChangelog, map[string]string{"a.added.md": "- A.\n"})
	if err := runAssemble(clPath, dir, "0.0.9", "2026-09-01"); err == nil {
		t.Fatal("want error for a version below the latest released 0.2.0")
	}
	if got := readFile(t, clPath); got != steadyChangelog {
		t.Error("CHANGELOG modified on refused assemble")
	}
}

// The pointer paragraph is plumbing: if entries were (wrongly) appended below
// it, release the entries but never the pointer text itself.
func TestPointerNeverReleasedAsContent(t *testing.T) {
	cl := `# Changelog

Preamble line.

## [Unreleased]

` + unreleasedPointer + `

### Added

- Late entry added below the pointer.

## [0.1.0] - 2026-07-17

Release summary.

[Unreleased]: https://github.com/o/r/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/o/r/releases/tag/v0.1.0
`
	clPath, dir := writeFixture(t, cl, nil)
	if err := runAssemble(clPath, dir, "0.2.0", "2026-09-01"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, clPath)
	if !strings.Contains(got, "## [0.2.0] - 2026-09-01\n\n### Added\n\n- Late entry added below the pointer.") {
		t.Errorf("late entry not released:\n%s", got)
	}
	if strings.Count(got, "Unreleased changes accumulate") != 1 {
		t.Errorf("pointer paragraph leaked into release content:\n%s", got)
	}
}

// A repository with no release yet gets a /releases/tag/ reference, not a
// compare range against a tag that does not exist.
func TestFirstEverReleaseLinkRef(t *testing.T) {
	cl := `# Changelog

## [Unreleased]

### Added

- First.

[Unreleased]: https://github.com/o/r/compare/v0.0.0...HEAD
`
	clPath, dir := writeFixture(t, cl, nil)
	if err := runAssemble(clPath, dir, "0.1.0", "2026-09-01"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, clPath); !strings.Contains(got, "[0.1.0]: https://github.com/o/r/releases/tag/v0.1.0") {
		t.Errorf("first release must use the tag link form:\n%s", got)
	}
}

// A fragment directory that cannot be modified must fail BEFORE the changelog
// is written — never a released section with fragments left behind.
func TestAssembleStagingFailureLeavesEverythingUntouched(t *testing.T) {
	clPath, dir := writeFixture(t, steadyChangelog, map[string]string{"a.added.md": "- A.\n"})
	// Pre-create .consumed so MkdirAll succeeds and the failure lands on the
	// rename itself (removing a directory entry needs a writable parent).
	// chmod-based denial assumes a non-root runner, which CI and local dev are.
	if err := os.Mkdir(filepath.Join(dir, ".consumed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if err := runAssemble(clPath, dir, "0.3.0", "2026-09-01"); err == nil {
		t.Fatal("want error when fragments cannot be staged")
	}
	if got := readFile(t, clPath); got != steadyChangelog {
		t.Error("CHANGELOG written despite fragment staging failure")
	}
	if _, err := os.Stat(filepath.Join(dir, "a.added.md")); err != nil {
		t.Error("fragment missing after failed staging")
	}
}

// Components longer than an int64 must still order correctly (no leading
// zeros are possible, so longer means greater).
func TestVersionGreaterHugeComponents(t *testing.T) {
	huge := "99999999999999999999" // > int64
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{huge + ".0.0", "9.0.0", true},
		{"9.0.0", huge + ".0.0", false},
		{"0." + huge + ".0", "0.9.0", true},
		{"10.0.0", "9.0.0", true},
		{"0.2.0", "0.2.0", false},
	} {
		if got := versionGreater(tc.a, tc.b); got != tc.want {
			t.Errorf("versionGreater(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// The stdout path is what the default `make changelog-notes` invocation uses.
func TestNotesStdout(t *testing.T) {
	clPath, _ := writeFixture(t, steadyChangelog, nil)
	tmp := filepath.Join(t.TempDir(), "stdout.txt")
	f, err := os.Create(tmp)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = f
	err = runNotes(clPath, "0.2.0", "-")
	os.Stdout = old
	if cerr := f.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if got, want := readFile(t, tmp), "### Added\n\n- A added.\n"; got != want {
		t.Errorf("stdout notes = %q, want %q", got, want)
	}
}

func TestNotes(t *testing.T) {
	clPath, _ := writeFixture(t, steadyChangelog, nil)

	out := filepath.Join(t.TempDir(), "notes.md")
	if err := runNotes(clPath, "0.2.0", out); err != nil {
		t.Fatal(err)
	}
	if got, want := readFile(t, out), "### Added\n\n- A added.\n"; got != want {
		t.Errorf("notes = %q, want %q", got, want)
	}

	// The last released section must not drag the link-ref block along.
	if err := runNotes(clPath, "0.1.0", out); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, out); strings.Contains(got, "[Unreleased]:") || !strings.Contains(got, "Release summary.") {
		t.Errorf("notes for last section wrong: %q", got)
	}

	if err := runNotes(clPath, "9.9.9", out); err == nil {
		t.Error("want error for missing version")
	}
	if err := runNotes(clPath, "Unreleased", out); err == nil {
		t.Error("want error for non-numeric version")
	}
}
