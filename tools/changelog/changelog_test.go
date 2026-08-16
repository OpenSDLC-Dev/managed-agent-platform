package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
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
	if err := runNotes(clPath, "0.1.0", out, 0); err != nil {
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

// documentedCap is the cap changelog.d/README.md promises a contributor, read
// from that file rather than from maxFragmentBytes.
//
// A boundary test that computes its own inputs from the constant it is testing
// still passes after the constant moves — and the number is not an
// implementation detail but a contract stated in prose, which is exactly the
// pair that drifts. Reading it back makes the doc and the code fail together or
// not at all.
func documentedCap(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "changelog.d", "README.md"))
	if err != nil {
		t.Fatalf("read changelog.d/README.md: %v", err)
	}
	m := regexp.MustCompile(`hard cap ([\d,]+) bytes`).FindStringSubmatch(string(b))
	if m == nil {
		t.Fatalf("changelog.d/README.md no longer states a %q — either the cap stopped "+
			"being documented, or this locator needs fixing; leaving it unmatched would "+
			"make the check below vacuous.", "hard cap N bytes")
	}
	n, err := strconv.Atoi(strings.ReplaceAll(m[1], ",", ""))
	if err != nil {
		t.Fatalf("changelog.d/README.md states cap %q, which is not a number: %v", m[1], err)
	}
	return n
}

// TestFragmentSizeCap holds the cap to what it promises: a body exactly at it
// releases, one byte over is refused with a message naming the file, the size
// and the cap, and the refusal leaves CHANGELOG.md and the fragment untouched.
// The cap counts bytes, so a body under it in runes but over it in bytes is
// refused too — an entry is prose, and this project's prose is full of em
// dashes. The number itself comes from the README rather than from
// maxFragmentBytes, so the documented cap and the enforced one cannot part.
func TestFragmentSizeCap(t *testing.T) {
	if want := documentedCap(t); maxFragmentBytes != want {
		t.Fatalf("maxFragmentBytes = %d but changelog.d/README.md promises contributors %d: "+
			"a fragment written to the documented limit would be refused, or one over it "+
			"accepted. Move both or neither.", maxFragmentBytes, want)
	}
	atCap := "- " + strings.Repeat("a", maxFragmentBytes-2)
	clPath, dir := writeFixture(t, steadyChangelog, map[string]string{
		"a.added.md": atCap + "\n",
	})
	if err := runAssemble(clPath, dir, "0.3.0", "2026-09-01"); err != nil {
		t.Fatalf("a fragment exactly at the %d-byte cap must load: %v", maxFragmentBytes, err)
	}
	if got := readFile(t, clPath); !strings.Contains(got, atCap) {
		t.Error("at-cap fragment not released")
	}

	for _, tc := range []struct{ name, body string }{
		{"one byte over", "- " + strings.Repeat("a", maxFragmentBytes-1)},
		{"under in runes, over in bytes", "- " + strings.Repeat("—", maxFragmentBytes/2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clPath, dir := writeFixture(t, steadyChangelog, map[string]string{
				"a.added.md": tc.body + "\n",
			})
			err := runAssemble(clPath, dir, "0.3.0", "2026-09-01")
			if err == nil {
				t.Fatal("want error for an over-cap fragment")
			}
			for _, want := range []string{
				"changelog.d/a.added.md",
				strconv.Itoa(len(tc.body)) + " bytes",
				strconv.Itoa(maxFragmentBytes) + "-byte cap",
				"changelog.d/README.md",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
			if got := readFile(t, clPath); got != steadyChangelog {
				t.Error("CHANGELOG modified on refused assemble")
			}
			if _, statErr := os.Stat(filepath.Join(dir, "a.added.md")); statErr != nil {
				t.Error("fragment deleted on refused assemble")
			}
		})
	}
}

// TestTheShippingFragmentsLoad runs the loader against this repository's own
// changelog.d/ — the only fragments that will ever be released.
//
// Every other test here builds a fixture, so the loader's rules (naming, the
// "- " prefix, no headings, the byte cap) were checked only against fragments
// written to be checked. Nothing in `make verify` or CI touched the real
// directory, so a fragment breaking one of them merged green and failed at the
// release PR — the point where the cost is highest and the author is elsewhere.
// This puts the failure in the PR that causes it.
//
// loadFragments only reads. Never reach for assemble here: it DELETES every
// fragment it folds, which is why every other test in this file copies its
// fixtures (docs/plan/34_doc-trim.md, "Never run the assembler to check a
// fragment").
//
// Directly after a release the directory is empty and this test proves nothing.
// That is correct: there is then nothing to break.
func TestTheShippingFragmentsLoad(t *testing.T) {
	dir := filepath.Join("..", "..", "changelog.d")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("changelog.d/ must exist at the repo root: %v", err)
	}
	if _, err := loadFragments(dir); err != nil {
		t.Fatalf("the repository's own changelog.d/ does not load, so the next release "+
			"PR would fail on it: %v", err)
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
	if os.Geteuid() == 0 {
		t.Skip("root ignores the write bit, so this proves nothing")
	}
	clPath, dir := writeFixture(t, steadyChangelog, map[string]string{"a.added.md": "- A.\n"})
	// Pre-create .consumed so MkdirAll succeeds and the failure lands on the
	// rename itself (removing a directory entry needs a writable parent).
	// chmod-based denial needs a non-root runner: CAP_DAC_OVERRIDE makes the
	// 0555 below inert, so the rename succeeds and the assertion is vacuous —
	// hence the skip above, matching internal/sandbox's five prior instances.
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

// `latest` answers the release workflow's tag-sanity check: the newest
// released section heading, nothing else.
func TestLatest(t *testing.T) {
	if got, err := latest(steadyChangelog); err != nil || got != "0.2.0" {
		t.Errorf("latest = %q, %v; want 0.2.0", got, err)
	}
	if _, err := latest("# Changelog\n\n## [Unreleased]\n\n- X.\n"); err == nil {
		t.Error("want error when no released section exists")
	}
	// Only the assembler's exact dated-heading grammar counts — a decoy
	// heading must not gate a release.
	decoy := "# Changelog\n\n## [9.9.9] not-a-release\n\n## [0.2.0] - 2026-08-07\n\n- A.\n"
	if got, err := latest(decoy); err != nil || got != "0.2.0" {
		t.Errorf("latest over decoy = %q, %v; want 0.2.0", got, err)
	}
	leadingZero := "# Changelog\n\n## [0.02.0] - 2026-08-07\n\n- A.\n\n## [0.1.0] - 2026-07-17\n\n- B.\n"
	if got, err := latest(leadingZero); err != nil || got != "0.1.0" {
		t.Errorf("latest over leading-zero heading = %q, %v; want 0.1.0", got, err)
	}
}

// The clamp budget must charge the trailer: the body is exactly one byte
// over the cap, so both groups "fit" if candidates are sized without the
// trailer — only trailer accounting can reject the second group.
func TestNotesCapChargesTrailer(t *testing.T) {
	const base = "https://github.com/OpenSDLC-Dev/managed-agent-platform"
	body := "### Added\n\n- " + strings.Repeat("a", 250) + "\n\n" +
		"### Fixed\n\n- " + strings.Repeat("b", 172) + "\n"
	cap := len(body) - 1
	got, err := clampNotes(body, base, "0.2.0", cap)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > cap {
		t.Errorf("clamped notes are %d bytes, above the %d-byte cap", len(got), cap)
	}
	if strings.Contains(got, "### Fixed") {
		t.Error("second group kept — it fits only if the trailer is not charged against the cap")
	}
	if !strings.Contains(got, "### Added") {
		t.Error("first group should survive the clamp")
	}
}

// A cap the trailer alone cannot satisfy is an error, never an over-cap
// "success".
func TestNotesCapUnsatisfiable(t *testing.T) {
	clPath, _ := writeFixture(t, steadyChangelog, nil)
	out := filepath.Join(t.TempDir(), "notes.md")
	err := runNotes(clPath, "0.2.0", out, 10)
	if err == nil {
		t.Fatal("want error for a cap smaller than the trailer")
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("no output file should be written on a refused clamp")
	}
}

// Above the cap, notes truncate at a ### group boundary and end with a link
// into the changelog at the tag; below it, output is byte-identical.
func TestNotesCap(t *testing.T) {
	clPath, _ := writeFixture(t, steadyChangelog, nil)
	out := filepath.Join(t.TempDir(), "notes.md")

	if err := runNotes(clPath, "0.2.0", out, 100000); err != nil {
		t.Fatal(err)
	}
	if got, want := readFile(t, out), "### Added\n\n- A added.\n"; got != want {
		t.Errorf("under-cap notes must be untouched: %q", got)
	}

	big := `# Changelog

## [Unreleased]

` + unreleasedPointer + `

## [0.2.0] - 2026-08-07

### Added

- ` + strings.Repeat("a", 300) + `

### Fixed

- ` + strings.Repeat("b", 300) + `

[Unreleased]: https://github.com/o/r/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/o/r/compare/v0.1.0...v0.2.0
`
	clPath2, _ := writeFixture(t, big, nil)
	if err := runNotes(clPath2, "0.2.0", out, 450); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, out)
	if len(got) > 450 {
		t.Errorf("clamped notes exceed the cap: %d bytes", len(got))
	}
	if !strings.Contains(got, "### Added") || strings.Contains(got, "### Fixed") {
		t.Errorf("clamp must cut at a group boundary keeping whole leading groups:\n%s", got)
	}
	if !strings.Contains(got, "https://github.com/o/r/blob/v0.2.0/CHANGELOG.md") {
		t.Errorf("clamped notes must link to the changelog at the tag:\n%s", got)
	}

	// A cap nothing fits under still yields a valid pointer-only body.
	if err := runNotes(clPath2, "0.2.0", out, 200); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, out); len(got) > 200 || !strings.Contains(got, "CHANGELOG.md") {
		t.Errorf("pointer-only clamp wrong (%d bytes):\n%s", len(got), got)
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
	err = runNotes(clPath, "0.2.0", "-", 0)
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
	if err := runNotes(clPath, "0.2.0", out, 0); err != nil {
		t.Fatal(err)
	}
	if got, want := readFile(t, out), "### Added\n\n- A added.\n"; got != want {
		t.Errorf("notes = %q, want %q", got, want)
	}

	// The last released section must not drag the link-ref block along.
	if err := runNotes(clPath, "0.1.0", out, 0); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, out); strings.Contains(got, "[Unreleased]:") || !strings.Contains(got, "Release summary.") {
		t.Errorf("notes for last section wrong: %q", got)
	}

	if err := runNotes(clPath, "9.9.9", out, 0); err == nil {
		t.Error("want error for missing version")
	}
	if err := runNotes(clPath, "Unreleased", out, 0); err == nil {
		t.Error("want error for non-numeric version")
	}
}

// The notes become a GitHub Release body, where a repo-root-relative target
// resolves against the release page and 404s. Both forms the fragment rules
// permit are rewritten to an absolute blob URL at the tag — the same job
// rebaseLinks does for docs/changelog/, which the notes path never had.
func TestNotesAbsolutizesRelativeLinks(t *testing.T) {
	cl := "# Changelog\n\n## [Unreleased]\n\n" + unreleasedPointer + `

## [0.3.0] - 2026-08-16

### Added

- Bare form [security](docs/self-hosted-security.md), dot form
  [gcp](./deploy/gcp/README.md#continuous-delivery).
- Untouched: [abs](https://example.com/x), [anchor](#top).

` + "```" + `
- fenced [keep](./docs/keep.md)
` + "```" + `

[Unreleased]: https://github.com/o/r/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/o/r/compare/v0.2.0...v0.3.0
`
	clPath, _ := writeFixture(t, cl, nil)
	out := filepath.Join(t.TempDir(), "notes.md")
	if err := runNotes(clPath, "0.3.0", out, 0); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, out)
	for _, want := range []string{
		"](https://github.com/o/r/blob/v0.3.0/docs/self-hosted-security.md)",
		"](https://github.com/o/r/blob/v0.3.0/deploy/gcp/README.md#continuous-delivery)",
		"](https://example.com/x)",
		"](#top)",
		"- fenced [keep](./docs/keep.md)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("notes missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "](docs/") || strings.Contains(got, "](./deploy/") {
		t.Errorf("a relative target survived into the release body:\n%s", got)
	}
}

// A relative target the rewrite has no mapping for must fail the notes rather
// than ship a link that 404s from the release page.
func TestNotesRefusesUnmappedRelativeLink(t *testing.T) {
	cl := "# Changelog\n\n## [Unreleased]\n\n" + unreleasedPointer + `

## [0.3.0] - 2026-08-16

- Parent-relative [x](../elsewhere.md).

[Unreleased]: https://github.com/o/r/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/o/r/compare/v0.2.0...v0.3.0
`
	clPath, _ := writeFixture(t, cl, nil)
	out := filepath.Join(t.TempDir(), "notes.md")
	if err := runNotes(clPath, "0.3.0", out, 0); err == nil {
		t.Error("want an error for a relative target with no absolute mapping")
	}
}

// The rewrite works on parsed destinations, not on the substring `](docs/`.
// The two spellings that motivated it are the ones a substring rewrite got
// silently wrong: `]( ./x)`, which it left relative *and* hid from the guard,
// and a `](docs/` sitting inside an already-absolute destination, which it
// corrupted while the guard accepted the result as absolute.
func TestAbsolutizeLinksDestinations(t *testing.T) {
	const base = "https://github.com/o/r"
	const pre = base + "/blob/v1.2.3/"
	for _, tc := range []struct {
		name, in, want string
		wantErr        bool
	}{
		{name: "dot form keeps its anchor", in: "[a](./deploy/x.md#frag)", want: "[a](" + pre + "deploy/x.md#frag)"},
		{name: "bare docs form", in: "[a](docs/x.md)", want: "[a](" + pre + "docs/x.md)"},
		// Only the destination span is rewritten, so the whitespace CommonMark
		// allows before it survives — still a valid link, now an absolute one.
		{name: "whitespace before the destination", in: "[a]( ./docs/x.md)", want: "[a]( " + pre + "docs/x.md)"},
		{name: "mapping-like text inside an absolute destination", in: "[x](https://example.com/a](docs/b)", want: "[x](https://example.com/a](docs/b)"},
		{name: "absolute untouched", in: "[a](https://example.com/x)", want: "[a](https://example.com/x)"},
		{name: "mailto untouched", in: "[a](mailto:x@y.z)", want: "[a](mailto:x@y.z)"},
		{name: "anchor untouched", in: "[a](#top)", want: "[a](#top)"},
		{name: "empty destination untouched", in: "[a]()", want: "[a]()"},
		{name: "title after the destination", in: `[a](docs/y.md "T")`, want: "[a](" + pre + `docs/y.md "T")`},
		{name: "several destinations on one line", in: "[a](docs/x.md), [b](./y.md), [c](#z)", want: "[a](" + pre + "docs/x.md), [b](" + pre + "y.md), [c](#z)"},
		{name: "bracketed label still guarded", in: "[a [b] c](x.md)", wantErr: true},
		{name: "parent-relative refused", in: "[a](../x.md)", wantErr: true},
		{name: "unmapped bare relative refused", in: "[a](x.md)", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := absolutizeLinks([]string{tc.in}, []bool{false}, base, "1.2.3")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("absolutizeLinks(%q) = %q, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("absolutizeLinks(%q): %v", tc.in, err)
			}
			if got[0] != tc.want {
				t.Errorf("absolutizeLinks(%q)\n got %q\nwant %q", tc.in, got[0], tc.want)
			}
		})
	}

	// A fenced line is a quoted example, left byte-identical.
	fenced, err := absolutizeLinks([]string{"[a](docs/x.md)"}, []bool{true}, base, "1.2.3")
	if err != nil || fenced[0] != "[a](docs/x.md)" {
		t.Errorf("fenced line rewritten: %q, %v", fenced, err)
	}

	// With no [Unreleased] reference there is no repository URL to build, and
	// a relative target must fail rather than publish unresolvable.
	if _, err := absolutizeLinks([]string{"[a](docs/x.md)"}, []bool{false}, "", "1.2.3"); err == nil {
		t.Error("want an error for a relative target with no base URL")
	}
}

// --- archive: the CHANGELOG slimming subcommand (plan 28) ---

// slimmed is steadyChangelog after archiving 0.2.0 — the golden document
// TestArchiveMovesSection asserts and later tests reuse.
var slimmed = `# Changelog

Preamble line.

## [Unreleased]

` + unreleasedPointer + `

## [0.2.0] - 2026-08-07

Added — the full section lives in [docs/changelog/0.2.0.md](./docs/changelog/0.2.0.md).

## [0.1.0] - 2026-07-17

Release summary.

[Unreleased]: https://github.com/OpenSDLC-Dev/managed-agent-platform/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/OpenSDLC-Dev/managed-agent-platform/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/OpenSDLC-Dev/managed-agent-platform/releases/tag/v0.1.0
`

func TestArchiveMovesSection(t *testing.T) {
	newContent, archived, err := archiveSection(steadyChangelog, "0.2.0", "docs/changelog/0.2.0.md")
	if err != nil {
		t.Fatal(err)
	}
	wantArchive := "## [0.2.0] - 2026-08-07\n\n### Added\n\n- A added.\n"
	if archived != wantArchive {
		t.Errorf("archive content:\n%q\nwant:\n%q", archived, wantArchive)
	}
	if newContent != slimmed {
		t.Errorf("slimmed changelog:\n%q\nwant:\n%q", newContent, slimmed)
	}
	// The move is lossless: swapping the stub block back for the archived
	// section reproduces the original document byte-for-byte.
	stub := "## [0.2.0] - 2026-08-07\n\nAdded — the full section lives in [docs/changelog/0.2.0.md](./docs/changelog/0.2.0.md).\n"
	if got := strings.Replace(newContent, stub, archived, 1); got != steadyChangelog {
		t.Errorf("re-composition is not byte-identical:\n%q", got)
	}
}

// A section bounded by the trailing link-reference block (the oldest release)
// archives cleanly, and a groupless body gets a plain pointer line.
func TestArchiveOldestSection(t *testing.T) {
	newContent, archived, err := archiveSection(slimmed, "0.1.0", "docs/changelog/0.1.0.md")
	if err != nil {
		t.Fatal(err)
	}
	wantArchive := "## [0.1.0] - 2026-07-17\n\nRelease summary.\n"
	if archived != wantArchive {
		t.Errorf("archive content:\n%q\nwant:\n%q", archived, wantArchive)
	}
	wantStub := "## [0.1.0] - 2026-07-17\n\nThe full section lives in [docs/changelog/0.1.0.md](./docs/changelog/0.1.0.md).\n"
	if !strings.Contains(newContent, wantStub) {
		t.Errorf("stub missing:\n%q", newContent)
	}
	if !strings.Contains(newContent, "\n[Unreleased]: https://") {
		t.Error("trailing link references were disturbed")
	}
	if got := strings.Replace(newContent, wantStub, archived, 1); got != slimmed {
		t.Errorf("re-composition is not byte-identical:\n%q", got)
	}
}

func TestArchiveRefusals(t *testing.T) {
	if _, _, err := archiveSection(steadyChangelog, "9.9.9", "docs/changelog/9.9.9.md"); err == nil || !strings.Contains(err.Error(), "no section [9.9.9]") {
		t.Errorf("want no-section refusal, got %v", err)
	}
	if _, _, err := archiveSection(steadyChangelog, "Unreleased", "docs/changelog/Unreleased.md"); err == nil || !strings.Contains(err.Error(), "is not X.Y.Z") {
		t.Errorf("want non-semver refusal, got %v", err)
	}
	if _, _, err := archiveSection(slimmed, "0.2.0", "docs/changelog/0.2.0.md"); err == nil || !strings.Contains(err.Error(), "is already archived") {
		t.Errorf("want already-archived refusal, got %v", err)
	}
	// The re-base mappings assume the canonical layout; any other archive
	// location is refused, not guessed at.
	if _, _, err := archiveSection(steadyChangelog, "0.2.0", "archive/0.2.0.md"); err == nil || !strings.Contains(err.Error(), "assumes that layout") {
		t.Errorf("want layout refusal, got %v", err)
	}
}

// Repeated groups — the shape the legacy-backlog absorption left in
// § [0.2.0] — summarize deduplicated, in Keep-a-Changelog order.
func TestArchiveStubDedupesGroups(t *testing.T) {
	cl := "# C\n\n## [0.3.0] - 2026-08-08\n\n### Fixed\n\n- F1.\n\n### Added\n\n- A1.\n\n### Fixed\n\n- F2.\n\n[0.3.0]: https://e/compare/v0.2.0...v0.3.0\n"
	newContent, _, err := archiveSection(cl, "0.3.0", "docs/changelog/0.3.0.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(newContent, "\nAdded · Fixed — the full section lives in [") {
		t.Errorf("stub summary not deduplicated/ordered:\n%q", newContent)
	}
}

// notes must refuse a stub instead of shipping a pointer line as the release
// body; the error names the archive file.
func TestNotesRefusesArchivedSection(t *testing.T) {
	if _, err := notes(slimmed, "0.2.0"); err == nil || !strings.Contains(err.Error(), "docs/changelog/0.2.0.md") {
		t.Errorf("want an error naming the archive file, got %v", err)
	}
}

// latest keeps answering from the slimmed document — the stubs retain the
// exact dated-heading grammar.
func TestLatestOnSlimmedChangelog(t *testing.T) {
	got, err := latest(slimmed)
	if err != nil || got != "0.2.0" {
		t.Errorf("latest = %q, %v; want 0.2.0", got, err)
	}
}

// runArchive writes both files, refuses to clobber an existing archive file,
// and a second run refuses the now-stubbed section.
func TestRunArchive(t *testing.T) {
	root := t.TempDir()
	clPath := filepath.Join(root, "CHANGELOG.md")
	if err := os.WriteFile(clPath, []byte(steadyChangelog), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "docs", "changelog")
	if err := runArchive(clPath, dir, "0.2.0"); err != nil {
		t.Fatal(err)
	}
	archived, err := os.ReadFile(filepath.Join(dir, "0.2.0.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(archived) != "## [0.2.0] - 2026-08-07\n\n### Added\n\n- A added.\n" {
		t.Errorf("archive file:\n%q", archived)
	}
	got, err := os.ReadFile(clPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != slimmed {
		t.Errorf("changelog on disk:\n%q", got)
	}
	if err := runArchive(clPath, dir, "0.2.0"); err == nil || !strings.Contains(err.Error(), "is already archived") {
		t.Errorf("want already-archived refusal, got %v", err)
	}
	// A pre-existing archive file is never clobbered.
	if err := os.WriteFile(filepath.Join(dir, "0.1.0.md"), []byte("occupied"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runArchive(clPath, dir, "0.1.0"); err == nil {
		t.Error("want error when the archive file already exists")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "0.1.0.md")); string(b) != "occupied" {
		t.Error("existing archive file was clobbered")
	}
}

// Relative links are re-based for docs/changelog/ — `](./` two levels up,
// the bare `](docs/` form one — while absolute URLs and fenced examples stay
// untouched, and the written bytes invert to the moved section.
func TestArchiveRebasesLinks(t *testing.T) {
	cl := "# C\n\n## [0.4.0] - 2026-09-01\n\n### Added\n\n" +
		"- See [plan](./docs/plan/29_x.md) and [old plan](docs/plan/10_y.md), plus\n" +
		"  [the SDK](https://example.com/sdk).\n\n" +
		"```\nquoted [link](./docs/plan/inside-fence.md) stays\n```\n\n" +
		"## [0.3.0] - 2026-08-08\n\nOlder.\n\n" +
		"[0.4.0]: https://e/1\n[0.3.0]: https://e/2\n"
	newContent, archived, err := archiveSection(cl, "0.4.0", "docs/changelog/0.4.0.md")
	if err != nil {
		t.Fatal(err)
	}
	wantArchive := "## [0.4.0] - 2026-09-01\n\n### Added\n\n" +
		"- See [plan](../../docs/plan/29_x.md) and [old plan](../plan/10_y.md), plus\n" +
		"  [the SDK](https://example.com/sdk).\n\n" +
		"```\nquoted [link](./docs/plan/inside-fence.md) stays\n```\n"
	if archived != wantArchive {
		t.Errorf("archive content:\n%q\nwant:\n%q", archived, wantArchive)
	}
	// The stub keeps the original-document link style — it lives in the
	// changelog, which never moved.
	if !strings.Contains(newContent, "Added — the full section lives in [docs/changelog/0.4.0.md](./docs/changelog/0.4.0.md).") {
		t.Errorf("stub wrong:\n%q", newContent)
	}
}

func TestArchiveRefusesUnhandledLinkForms(t *testing.T) {
	link := "docs/changelog/0.4.0.md"
	parent := "# C\n\n## [0.4.0] - 2026-09-01\n\n- A [link](../escape.md).\n\n[0.4.0]: https://e/1\n"
	if _, _, err := archiveSection(parent, "0.4.0", link); err == nil || !strings.Contains(err.Error(), "parent-relative") {
		t.Errorf("want parent-relative refusal, got %v", err)
	}
	bare := "# C\n\n## [0.4.0] - 2026-09-01\n\n- A [link](internal/q.md).\n\n[0.4.0]: https://e/1\n"
	if _, _, err := archiveSection(bare, "0.4.0", link); err == nil || !strings.Contains(err.Error(), "unhandled relative link target") {
		t.Errorf("want unhandled-form refusal, got %v", err)
	}
	// A link-reference definition carries a relative target in a form the
	// inline scan does not see — refused, not moved unrebased.
	refDef := "# C\n\n## [0.4.0] - 2026-09-01\n\n- A [plan][p] entry.\n\n[p]: docs/plan/29_x.md\n\n[0.4.0]: https://e/1\n"
	if _, _, err := archiveSection(refDef, "0.4.0", link); err == nil || !strings.Contains(err.Error(), "link-reference definition with a relative target") {
		t.Errorf("want ref-def refusal, got %v", err)
	}
	// An absolute-URL definition is untouched and archives fine.
	absDef := "# C\n\n## [0.4.0] - 2026-09-01\n\n- A [site][s] entry.\n\n[s]: https://example.com/x\n\n[0.4.0]: https://e/1\n"
	if _, _, err := archiveSection(absDef, "0.4.0", link); err != nil {
		t.Errorf("absolute-URL ref def refused: %v", err)
	}
}

// The re-composition guard is reachable: when the exact stub text already
// occurs earlier in the document (quoted mid-entry), the first-occurrence
// Replace would restore the wrong spot, and archiveSection must refuse.
// Deleting the round-trip comparison turns this red — the mutation duty
// plan 28 slice 1 names.
func TestArchiveRoundTripGuardFires(t *testing.T) {
	cl := "# C\n\n## [0.3.0] - 2026-08-08\n\n### Added\n\n" +
		"- Quoting a stub verbatim: junk ## [0.2.0] - 2026-08-07\n\n" +
		"Added — the full section lives in [docs/changelog/0.2.0.md](./docs/changelog/0.2.0.md).\n\n" +
		"## [0.2.0] - 2026-08-07\n\n### Added\n\n- Real entry.\n\n" +
		"[0.3.0]: https://e/1\n[0.2.0]: https://e/2\n"
	_, _, err := archiveSection(cl, "0.2.0", "docs/changelog/0.2.0.md")
	if err == nil || !strings.Contains(err.Error(), "would not round-trip") {
		t.Errorf("want round-trip refusal, got %v", err)
	}
}

// The exact-dated-grammar guard refuses a heading `latest` and the tag-sanity
// check could not parse back.
func TestArchiveRefusesLooseHeadingGrammar(t *testing.T) {
	cl := "# C\n\n## [0.8.0] - 2026-9-4\n\nBody.\n\n[0.8.0]: https://e/1\n"
	if _, _, err := archiveSection(cl, "0.8.0", "docs/changelog/0.8.0.md"); err == nil || !strings.Contains(err.Error(), "exact dated grammar") {
		t.Errorf("want grammar refusal, got %v", err)
	}
}

// The swallow guard is reachable: an indented `## ` heading is a renderer
// boundary the column-0 scan does not see, and archiving over it is refused;
// a fenced `## ` line is quoted content and archives untouched.
func TestArchiveSecondHeadingShapes(t *testing.T) {
	indented := "# C\n\n## [0.6.0] - 2026-09-03\n\nBody.\n\n ## Legacy heading\n\n[0.6.0]: https://e/1\n"
	if _, _, err := archiveSection(indented, "0.6.0", "docs/changelog/0.6.0.md"); err == nil || !strings.Contains(err.Error(), "would swallow") {
		t.Errorf("want swallow refusal, got %v", err)
	}
	fenced := "# C\n\n## [0.7.0] - 2026-09-04\n\nBody.\n\n```\n## [9.9.9] - 2026-01-01\n```\n\n[0.7.0]: https://e/1\n"
	_, archived, err := archiveSection(fenced, "0.7.0", "docs/changelog/0.7.0.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(archived, "\n## [9.9.9] - 2026-01-01\n") {
		t.Errorf("fenced example line altered:\n%q", archived)
	}
}

// Stub detection is structural — one non-empty body line carrying the mark —
// so a real multi-line body that quotes the phrase is neither "already
// archived" nor refused by notes.
func TestArchiveStubPhraseInProseIsNotAStub(t *testing.T) {
	cl := "# C\n\n## [0.9.0] - 2026-09-05\n\n" +
		"- One entry noting the full section lives in [the ops guide](https://e/g).\n" +
		"- Another entry.\n\n[0.9.0]: https://e/1\n"
	if _, _, err := archiveSection(cl, "0.9.0", "docs/changelog/0.9.0.md"); err != nil {
		t.Errorf("prose quoting the stub phrase refused: %v", err)
	}
	if _, err := notes(cl, "0.9.0"); err != nil {
		t.Errorf("notes refused a real section quoting the stub phrase: %v", err)
	}
}

// A group outside Keep a Changelog's canon still reaches the stub summary,
// after the canonical ones, in appearance order.
func TestArchiveStubNonKacGroup(t *testing.T) {
	cl := "# C\n\n## [0.5.0] - 2026-09-02\n\n### Special\n\n- S.\n\n### Added\n\n- A.\n\n[0.5.0]: https://e/1\n"
	newContent, _, err := archiveSection(cl, "0.5.0", "docs/changelog/0.5.0.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(newContent, "\nAdded · Special — the full section lives in [") {
		t.Errorf("non-canonical group missing or misordered:\n%q", newContent)
	}
}

// The write order is the crash-safety invariant: when the archive write
// fails, the changelog must be untouched.
func TestRunArchiveFailedArchiveWriteLeavesChangelog(t *testing.T) {
	root := t.TempDir()
	clPath := filepath.Join(root, "CHANGELOG.md")
	if err := os.WriteFile(clPath, []byte(steadyChangelog), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "docs", "changelog")
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	// Root bypasses directory write permission, so the unwritable-dir
	// precondition cannot be established there.
	if probe, err := os.Create(filepath.Join(dir, ".probe")); err == nil {
		probe.Close()
		t.Skip("process writes through a 0o555 dir (running as root?)")
	}
	if err := runArchive(clPath, dir, "0.2.0"); err == nil {
		t.Fatal("want error from an unwritable archive dir")
	}
	if got := readFile(t, clPath); got != steadyChangelog {
		t.Error("changelog was rewritten although the archive write failed")
	}
}

// An archive file left by an interrupted run — byte-identical to what this
// run would write — converges the retry instead of blocking it.
func TestRunArchiveIdempotentResume(t *testing.T) {
	root := t.TempDir()
	clPath := filepath.Join(root, "CHANGELOG.md")
	if err := os.WriteFile(clPath, []byte(steadyChangelog), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "docs", "changelog")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	prior := "## [0.2.0] - 2026-08-07\n\n### Added\n\n- A added.\n"
	if err := os.WriteFile(filepath.Join(dir, "0.2.0.md"), []byte(prior), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runArchive(clPath, dir, "0.2.0"); err != nil {
		t.Fatalf("identical leftover archive should converge, got %v", err)
	}
	if got := readFile(t, clPath); got != slimmed {
		t.Errorf("changelog on disk:\n%q", got)
	}
}

// An absolute -changelog with the relative -dir default must still resolve
// the stub's link path (filepath.Rel needs a common root).
func TestRunArchiveAbsoluteChangelogRelativeDir(t *testing.T) {
	t.Chdir(t.TempDir())
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	clPath := filepath.Join(root, "CHANGELOG.md")
	if err := os.WriteFile(clPath, []byte(steadyChangelog), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runArchive(clPath, filepath.Join("docs", "changelog"), "0.2.0"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, clPath); !strings.Contains(got, "[docs/changelog/0.2.0.md](./docs/changelog/0.2.0.md)") {
		t.Errorf("stub link path wrong:\n%q", got)
	}
}
