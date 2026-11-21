// Package main implements the release-time changelog tool (plans 27 and 28):
// `assemble` folds the changelog.d/ fragments — plus any legacy [Unreleased]
// body — into a new dated section of CHANGELOG.md, `notes` extracts a
// released section's body for GitHub Release notes — rewriting its relative
// links absolute at the tag, since that body is also served raw, where
// nothing supplies the repository base github.com's own renderer applies on
// the release page — and `archive` moves a
// released section to docs/changelog/<version>.md behind an index stub,
// post-release — byte-reversibly: relative links are re-based for the new
// location and the inverse rewrite must reproduce the moved section. The ritual that runs them is docs/RELEASING.md; the
// Make entry points are `changelog`, `changelog-notes` and
// `changelog-archive`.
package main

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// unreleasedPointer is the steady-state [Unreleased] body once fragments carry
// the unreleased narrative: assemble writes it after a cut and recognizes it
// as "nothing pending" on the next one. Byte-exact on both sides, so drift is
// impossible.
const unreleasedPointer = "Unreleased changes accumulate as one fragment file per PR in\n" +
	"[changelog.d/](./changelog.d/) and are assembled into a dated section here\n" +
	"by `make changelog` at release time (see [docs/RELEASING.md](./docs/RELEASING.md))."

// maxFragmentBytes caps a fragment body — one paragraph, around 200 words —
// measured in bytes after the surrounding-newline trim, the same bytes that
// reach the released section. The convention, and the table naming where each
// longer form belongs instead, live in changelog.d/README.md; the cap is
// enforced here because a documented-only limit is not a limit, and assembly
// freezes what it folds in — an over-long fragment is a paragraph the project
// then maintains forever.
const maxFragmentBytes = 1500

var (
	// SemVer forbids leading zeros — 0.2.01 is not a version.
	versionRe  = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$`)
	fragNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+\.(added|changed|deprecated|removed|fixed|security)\.md$`)
	// Only the labels Keep a Changelog itself puts in the trailing block:
	// a released section body may legitimately end with its own
	// reference-definition line, and that line must stay with the body.
	linkRefRe        = regexp.MustCompile(`^\[(Unreleased|\d+\.\d+\.\d+)\]: \S+$`)
	unreleasedRefRe  = regexp.MustCompile(`^\[Unreleased\]: (\S+)/compare/v\S+\.\.\.HEAD$`)
	releasedRefRe    = regexp.MustCompile(`^\[(\d+\.\d+\.\d+)\]: `)
	releaseHeadingRe = regexp.MustCompile(`^## \[((?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*))\] - \d{4}-\d{2}-\d{2}$`)
	// An ATX heading: up to three leading spaces, 1–6 hashes, then
	// whitespace or end of line — `#123 fixed …` is entry text, not a heading.
	headingRe = regexp.MustCompile(`^ {0,3}#{1,6}([ \t]|$)`)
)

// kacOrder is Keep a Changelog's canonical group order.
var kacOrder = []string{"Added", "Changed", "Deprecated", "Removed", "Fixed", "Security"}

type fragment struct {
	name    string
	section string // Added, Changed, ... (title-cased from the filename)
	body    string // the verbatim entry, trimmed of surrounding whitespace
	addedAt int64  // committer time of the commit that added the file; MaxInt64 when uncommitted
}

// loadFragments reads dir and returns its fragments newest-first by the
// commit that added each file (an uncommitted file counts as newest of all),
// tie-broken by filename. README.md is the directory's own documentation and
// is skipped; any other name that is not <slug>.<section>.md is an error, so
// a typo'd section cannot silently drop an entry from the release.
func loadFragments(dir string) ([]fragment, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var frags []fragment
	for _, e := range entries {
		name := e.Name()
		// OS and editor droppings (.DS_Store, swap files, tool caches)
		// are noise, not typo'd fragments — no slug can start with a dot.
		if strings.HasPrefix(name, ".") {
			continue
		}
		if e.IsDir() {
			return nil, fmt.Errorf("changelog.d/%s: unexpected directory", name)
		}
		if strings.EqualFold(name, "README.md") {
			continue
		}
		m := fragNameRe.FindStringSubmatch(name)
		if m == nil {
			return nil, fmt.Errorf("changelog.d/%s: fragment names are <slug>.<section>.md with section one of added|changed|deprecated|removed|fixed|security", name)
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		// Trim only surrounding newlines: interior bytes — trailing
		// spaces (markdown hard breaks) included — are the entry.
		body := strings.Trim(string(raw), "\n")
		if strings.TrimSpace(body) == "" {
			return nil, fmt.Errorf("changelog.d/%s: empty fragment", name)
		}
		if len(body) > maxFragmentBytes {
			return nil, fmt.Errorf("changelog.d/%s: %d bytes, over the %d-byte cap — a fragment holds what a reader of the release notes needs; changelog.d/README.md names the home of every longer form", name, len(body), maxFragmentBytes)
		}
		if !strings.HasPrefix(body, "- ") {
			return nil, fmt.Errorf("changelog.d/%s: a fragment is the final CHANGELOG entry verbatim and must start with %q", name, "- ")
		}
		lines := strings.Split(body, "\n")
		layout, err := markdownLines(lines)
		if err != nil {
			return nil, fmt.Errorf("changelog.d/%s: %w", name, err)
		}
		for i, line := range lines {
			if layout[i] != codeLine && headingRe.MatchString(line) {
				return nil, fmt.Errorf("changelog.d/%s: headings belong to the assembler, not a fragment", name)
			}
		}
		// Use the release-notes rules with placeholder metadata. Keep the
		// original body for assembly, which copies fragments verbatim.
		if _, err := absolutizeLinks(lines, layout, "https://example.invalid/repo", "0.0.0"); err != nil {
			return nil, fmt.Errorf("changelog.d/%s: %w", name, err)
		}
		frags = append(frags, fragment{
			name:    name,
			section: strings.ToUpper(m[1][:1]) + m[1][1:],
			body:    body,
			addedAt: fragmentAddTime(dir, name),
		})
	}
	sort.SliceStable(frags, func(i, j int) bool {
		if frags[i].addedAt != frags[j].addedAt {
			return frags[i].addedAt > frags[j].addedAt
		}
		return frags[i].name < frags[j].name
	})
	return frags, nil
}

// fragmentAddTime returns the committer time of the commit that first added
// dir/name, or MaxInt64 when git does not know the file (not committed yet,
// or dir outside a repository) — unknown means newest, which is where an
// entry still in flight belongs.
func fragmentAddTime(dir, name string) int64 {
	cmd := exec.Command("git", "log", "--diff-filter=A", "--format=%ct", "--", name)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return math.MaxInt64
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) == 0 {
		return math.MaxInt64
	}
	// git log lists newest-first; the first line is the commit that added
	// the CURRENT file — a name a past release deleted and a later PR
	// reused must sort by its rebirth, not its ancestor.
	t, err := strconv.ParseInt(lines[0], 10, 64)
	if err != nil {
		return math.MaxInt64
	}
	return t
}

// splitTrailingRefs splits the Keep-a-Changelog link-reference block off the
// end of the document. Returns the refs (in order) and the document lines
// with the block and any trailing blank lines removed.
func splitTrailingRefs(lines []string) (refs, doc []string) {
	end := len(lines)
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	start := end
	for start > 0 && linkRefRe.MatchString(lines[start-1]) {
		start--
	}
	refs = lines[start:end]
	doc = lines[:start]
	for len(doc) > 0 && strings.TrimSpace(doc[len(doc)-1]) == "" {
		doc = doc[:len(doc)-1]
	}
	return refs, doc
}

func changelogSections(lines []string) (refs, doc []string, layout []markdownLine, err error) {
	refs, doc = splitTrailingRefs(lines)
	layout, err = markdownLines(doc)
	if err != nil {
		return nil, nil, nil, err
	}
	// Check the index independently of fence state. A fence that closes in
	// a later release is balanced across the file but still hides a section.
	indexed := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if m := releasedRefRe.FindStringSubmatch(ref); m != nil {
			indexed[m[1]] = true
		}
	}
	visible := make(map[string]bool, len(refs)+1)
	type hiddenHeading struct {
		key  string
		line int
	}
	var hidden []hiddenHeading
	for i, line := range doc {
		key := ""
		if line == "## [Unreleased]" {
			key = "Unreleased"
		} else if m := releaseHeadingRe.FindStringSubmatch(line); m != nil && indexed[m[1]] {
			key = m[1]
		}
		if key == "" {
			continue
		}
		if layout[i] == codeLine {
			hidden = append(hidden, hiddenHeading{key, i})
		} else {
			visible[key] = true
		}
	}
	for _, heading := range hidden {
		if !visible[heading.key] {
			return nil, nil, nil, fmt.Errorf("Markdown fence hides changelog heading %q at line %d", doc[heading.line], heading.line+1)
		}
	}
	return refs, doc, layout, nil
}

func sectionEnd(doc []string, layout []markdownLine, start int) int {
	for i := start + 1; i < len(doc); i++ {
		if layout[i] != codeLine && strings.HasPrefix(doc[i], "## ") {
			return i
		}
	}
	return len(doc)
}

// joinTrimmed joins lines after dropping leading and trailing blank ones;
// interior lines stay byte-identical.
func joinTrimmed(lines []string) string {
	start, end := 0, len(lines)
	for start < end && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return strings.Join(lines[start:end], "\n")
}

// prevReleased returns the newest released version named in the trailing
// link-reference block, or "" when none exists yet.
func prevReleased(refs []string) string {
	for _, r := range refs {
		if m := releasedRefRe.FindStringSubmatch(r); m != nil {
			return m[1]
		}
	}
	return ""
}

// versionGreater reports a > b for X.Y.Z versions (both already
// shape-validated, so components carry no leading zeros — which makes
// longer-is-greater plus lexicographic comparison exact at any length,
// with no integer overflow to mishandle).
func versionGreater(a, b string) bool {
	as, bs := strings.SplitN(a, ".", 3), strings.SplitN(b, ".", 3)
	for i := 0; i < 3; i++ {
		if cmp := compareVersionPart(as[i], bs[i]); cmp != 0 {
			return cmp > 0
		}
	}
	return false
}

func compareVersionPart(a, b string) int {
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

// updateRefs rewrites [Unreleased] to compare from the new version and
// inserts the new version's ref right after it, comparing from the previous
// release when one exists.
func updateRefs(refs []string, version string) ([]string, error) {
	unrelIdx, base := -1, ""
	for i, r := range refs {
		if m := unreleasedRefRe.FindStringSubmatch(r); m != nil {
			unrelIdx, base = i, m[1]
			break
		}
	}
	if unrelIdx < 0 {
		return nil, fmt.Errorf("no [Unreleased] link reference at the end of the changelog — cannot derive the repository URL")
	}
	prev := ""
	for _, r := range refs {
		if m := releasedRefRe.FindStringSubmatch(r); m != nil {
			prev = m[1]
			break
		}
	}
	versionRef := fmt.Sprintf("[%s]: %s/releases/tag/v%s", version, base, version)
	if prev != "" {
		versionRef = fmt.Sprintf("[%s]: %s/compare/v%s...v%s", version, base, prev, version)
	}
	out := make([]string, 0, len(refs)+1)
	out = append(out, refs[:unrelIdx]...)
	out = append(out, fmt.Sprintf("[Unreleased]: %s/compare/v%s...HEAD", base, version))
	out = append(out, versionRef)
	out = append(out, refs[unrelIdx+1:]...)
	return out, nil
}

// assemble builds the new document: fragments grouped in KaC order under a
// dated section, the legacy [Unreleased] body (anything other than the
// pointer paragraph) appended verbatim below the groups, the pointer left as
// the new [Unreleased] body, and the link references advanced.
func assemble(content string, frags []fragment, version, date string) (string, error) {
	if !versionRe.MatchString(version) {
		return "", fmt.Errorf("version %q is not X.Y.Z", version)
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return "", fmt.Errorf("date %q is not a real YYYY-MM-DD date: %v", date, err)
	}
	refs, doc, layout, err := changelogSections(strings.Split(content, "\n"))
	if err != nil {
		return "", err
	}
	for i, l := range doc {
		if layout[i] != codeLine && strings.HasPrefix(l, "## ["+version+"]") {
			return "", fmt.Errorf("section [%s] already exists", version)
		}
	}
	unrelIdx := -1
	for i, l := range doc {
		if layout[i] != codeLine && l == "## [Unreleased]" {
			if unrelIdx >= 0 {
				return "", fmt.Errorf("two ## [Unreleased] sections — refusing to release only the first")
			}
			unrelIdx = i
		}
	}
	if unrelIdx < 0 {
		return "", fmt.Errorf("no ## [Unreleased] section")
	}
	restIdx := sectionEnd(doc, layout, unrelIdx)
	tail := joinTrimmed(doc[unrelIdx+1 : restIdx])
	// The pointer paragraph is plumbing, never release content — strip it
	// whether it stands alone or something was (wrongly) appended below it.
	if strings.HasPrefix(tail, unreleasedPointer) {
		tail = strings.Trim(strings.TrimPrefix(tail, unreleasedPointer), "\n")
	}
	if tail == "" && len(frags) == 0 {
		return "", fmt.Errorf("nothing to release: changelog.d/ is empty and [Unreleased] holds no entries")
	}
	if prev := prevReleased(refs); prev != "" && !versionGreater(version, prev) {
		return "", fmt.Errorf("version %s does not advance the latest released %s", version, prev)
	}
	newRefs, err := updateRefs(refs, version)
	if err != nil {
		return "", err
	}

	out := make([]string, 0, len(doc)+len(frags)*2+16)
	out = append(out, doc[:unrelIdx+1]...)
	out = append(out, "", unreleasedPointer, "")
	out = append(out, "## ["+version+"] - "+date)
	for _, group := range kacOrder {
		first := true
		for _, f := range frags {
			if f.section != group {
				continue
			}
			if first {
				out = append(out, "", "### "+group)
				first = false
			}
			out = append(out, "", f.body)
		}
	}
	if tail != "" {
		out = append(out, "", tail)
	}
	if restIdx < len(doc) {
		out = append(out, "")
		out = append(out, doc[restIdx:]...)
	}
	out = append(out, "")
	out = append(out, newRefs...)
	assembled := strings.Join(out, "\n") + "\n"
	if _, _, _, err := changelogSections(strings.Split(assembled, "\n")); err != nil {
		return "", fmt.Errorf("assembled changelog: %w", err)
	}
	return assembled, nil
}

// notes returns the body of version's released section, without the
// link-reference block.
func notes(content, version string) (string, error) {
	if !versionRe.MatchString(version) {
		return "", fmt.Errorf("version %q is not X.Y.Z", version)
	}
	_, doc, layout, err := changelogSections(strings.Split(content, "\n"))
	if err != nil {
		return "", err
	}
	secIdx := -1
	for i, l := range doc {
		if layout[i] != codeLine && strings.HasPrefix(l, "## ["+version+"]") {
			secIdx = i
			break
		}
	}
	if secIdx < 0 {
		return "", fmt.Errorf("no section [%s] in the changelog", version)
	}
	end := sectionEnd(doc, layout, secIdx)
	body := joinTrimmed(doc[secIdx+1:end]) + "\n"
	// An archived section's stub must never ship as a release body; re-runs
	// of a release workflow read the tag's checkout, where the section is
	// still inline. (Stub-shaped only: a real body quoting the phrase
	// mid-entry is not a stub.)
	if isArchiveStub(strings.Split(body, "\n")) {
		return "", fmt.Errorf("section [%s] is archived — %s", version, strings.TrimSpace(body))
	}
	return body, nil
}

// runAssemble is the `assemble` subcommand. Failure anywhere leaves the
// pre-command state: fragments are first staged into `.consumed/` (a
// dot-directory, so a stray leftover is invisible to a later run), the
// changelog is written only after staging succeeds, and a failed write
// unstages them (best-effort — a fragment the rename-back cannot move is
// named in the error, and git still holds it). Only after the new document
// is on disk is the staging directory removed — the one step whose failure
// leaves harmless residue rather than a half-released state.
func runAssemble(changelogPath, dir, version, date string) error {
	content, err := os.ReadFile(changelogPath)
	if err != nil {
		return err
	}
	frags, err := loadFragments(dir)
	if err != nil {
		return err
	}
	out, err := assemble(string(content), frags, version, date)
	if err != nil {
		return err
	}

	consumed := filepath.Join(dir, ".consumed")
	if len(frags) > 0 {
		if err := os.MkdirAll(consumed, 0o755); err != nil {
			return err
		}
	}
	// Best-effort: a rename-back can itself fail, so report what stayed
	// stranded in .consumed/ instead of claiming a clean restore — git
	// still holds every fragment, so recovery is a checkout away.
	unstage := func(n int) []string {
		var stranded []string
		for _, f := range frags[:n] {
			if err := os.Rename(filepath.Join(consumed, f.name), filepath.Join(dir, f.name)); err != nil {
				stranded = append(stranded, f.name)
			}
		}
		return stranded
	}
	for i, f := range frags {
		if err := os.Rename(filepath.Join(dir, f.name), filepath.Join(consumed, f.name)); err != nil {
			if stranded := unstage(i); len(stranded) > 0 {
				return fmt.Errorf("staging %s: %w (nothing was released; %s could not be moved back out of %s — restore from git)", f.name, err, strings.Join(stranded, ", "), consumed)
			}
			return fmt.Errorf("staging %s: %w (nothing was released)", f.name, err)
		}
	}
	if err := os.WriteFile(changelogPath, []byte(out), 0o644); err != nil {
		if stranded := unstage(len(frags)); len(stranded) > 0 {
			return fmt.Errorf("writing the changelog: %w (nothing was released; %s could not be moved back out of %s — restore from git)", err, strings.Join(stranded, ", "), consumed)
		}
		return fmt.Errorf("writing the changelog: %w (fragments restored, nothing was released)", err)
	}
	if len(frags) > 0 {
		if err := os.RemoveAll(consumed); err != nil {
			fmt.Fprintf(os.Stderr, "warning: the release is complete but %s could not be removed (%v); it is inert and can be deleted by hand\n", consumed, err)
		}
	}
	return nil
}

// latest returns the newest released version — the first heading in the
// assembler's exact dated grammar, so a decoy or malformed heading cannot
// gate a release. The release workflow's tag-sanity check compares it to
// the tag.
func latest(content string) (string, error) {
	_, doc, layout, err := changelogSections(strings.Split(content, "\n"))
	if err != nil {
		return "", err
	}
	for i, l := range doc {
		if m := releaseHeadingRe.FindStringSubmatch(l); layout[i] != codeLine && m != nil {
			return m[1], nil
		}
	}
	return "", fmt.Errorf("no released section in the changelog")
}

// renderNotes rewrites destinations at the tag and applies the byte cap to
// complete groups. A bad destination only fails the render if its group is
// emitted. Successful rewrites still count toward the cap in every candidate.
func renderNotes(body, base, version string, cap int) (string, error) {
	lines := strings.Split(body, "\n")
	layout, err := markdownLines(lines)
	if err != nil {
		return "", err
	}
	type group struct {
		text string
		err  error
	}
	var groups []group
	start, size := 0, 0
	for end := 1; end <= len(lines); end++ {
		if end < len(lines) && (layout[end] == codeLine || !strings.HasPrefix(lines[end], "### ")) {
			continue
		}
		mapped, linkErr := absolutizeLinks(lines[start:end], layout[start:end], base, version)
		text := strings.Join(mapped, "\n")
		if end < len(lines) {
			text += "\n"
		}
		groups = append(groups, group{text, linkErr})
		size += len(text)
		start = end
	}

	trailer := ""
	if cap > 0 && size > cap {
		if base == "" {
			return "", fmt.Errorf("notes exceed the %d-byte cap and no [Unreleased] link reference exists to derive the changelog link from", cap)
		}
		trailer = fmt.Sprintf("---\n\nTruncated: the full section is in [CHANGELOG.md](%s/blob/v%s/CHANGELOG.md).\n", base, version)
		if len(trailer) > cap {
			return "", fmt.Errorf("cap %d is smaller than the %d-byte truncation trailer itself", cap, len(trailer))
		}
	}
	var out strings.Builder
	for _, g := range groups {
		if trailer != "" && out.Len()+len(strings.TrimRight(g.text, "\n"))+2+len(trailer) > cap {
			break
		}
		if g.err != nil {
			return "", g.err
		}
		out.WriteString(g.text)
	}
	if trailer == "" {
		return out.String(), nil
	}
	if kept := strings.TrimRight(out.String(), "\n"); kept != "" {
		return kept + "\n\n" + trailer, nil
	}
	return trailer, nil
}

// runNotes is the notes subcommand. An empty out or "-" writes to stdout.
func runNotes(changelogPath, version, out string, cap int) error {
	content, err := os.ReadFile(changelogPath)
	if err != nil {
		return err
	}
	body, err := notes(string(content), version)
	if err != nil {
		return err
	}
	body, err = renderNotes(body, repoBase(string(content)), version, cap)
	if err != nil {
		return err
	}
	if out == "" || out == "-" {
		_, err = os.Stdout.WriteString(body)
		return err
	}
	return os.WriteFile(out, []byte(body), 0o644)
}

// archiveMark is the text every archive stub carries. archiveSection writes
// it; archiveSection and notes refuse a stub-shaped section that carries it.
const archiveMark = "full section lives in ["

// isArchiveStub reports whether a section body is an archive stub: exactly
// one non-empty line, carrying archiveMark. A multi-line body that merely
// quotes the phrase is a real section, not a stub.
func isArchiveStub(body []string) bool {
	nonEmpty, marked := 0, false
	for _, l := range body {
		if strings.TrimSpace(l) == "" {
			continue
		}
		nonEmpty++
		if strings.Contains(l, archiveMark) {
			marked = true
		}
	}
	return nonEmpty == 1 && marked
}

// repoBase returns the repository URL carried by the [Unreleased] link
// reference, or "" when the changelog has none.
func repoBase(content string) string {
	refs, _ := splitTrailingRefs(strings.Split(content, "\n"))
	for _, r := range refs {
		if m := unreleasedRefRe.FindStringSubmatch(r); m != nil {
			return m[1]
		}
	}
	return ""
}

// archiveSection moves the dated section for version out of content (plan 28):
// the returned changelog keeps the exact heading line — `latest` and the
// tag-sanity check parse the file they always did — over a one-line pointer
// stub, and the archive text is the heading plus the body with its relative
// links re-based for docs/changelog/ (rebaseLinks). Nothing is returned
// unless swapping the stub back for the section reproduces content
// byte-for-byte AND the archive bytes invert to the moved section — a lossy
// split is an error, never output.
func archiveSection(content, version, linkPath string) (string, string, error) {
	if !versionRe.MatchString(version) {
		return "", "", fmt.Errorf("version %q is not X.Y.Z", version)
	}
	// The link re-base hardcodes its two mappings for an archive exactly two
	// levels below the changelog, in docs/changelog/ — any other layout would
	// re-base wrongly, so it is refused rather than guessed at.
	if path.Dir(filepath.ToSlash(linkPath)) != "docs/changelog" {
		return "", "", fmt.Errorf("archive path %q is not docs/changelog/ next to the changelog — the link re-base assumes that layout", linkPath)
	}
	lines := strings.Split(content, "\n")
	_, doc, layout, err := changelogSections(lines)
	if err != nil {
		return "", "", err
	}
	start := -1
	for i, l := range doc {
		if layout[i] != codeLine && strings.HasPrefix(l, "## ["+version+"]") {
			start = i
			break
		}
	}
	if start < 0 {
		return "", "", fmt.Errorf("no section [%s] in the changelog", version)
	}
	end := sectionEnd(doc, layout, start)
	section := lines[start:end]
	// The archive must be exactly one section: the exact dated heading, then
	// a body with no further section heading. The boundary scan above stops
	// at the first unfenced column-0 `## ` line, so what this guard adds is
	// the boundary shape that scan does not see but a renderer does — an ATX
	// `## ` heading indented one to three spaces — plus, under a future
	// boundary regression, the swallowed neighbor itself (the mutation the
	// tests pin). A fenced `## ` line is a quoted example and stays.
	if !releaseHeadingRe.MatchString(section[0]) {
		return "", "", fmt.Errorf("section [%s] heading %q is not the exact dated grammar", version, section[0])
	}
	for j, l := range section[1:] {
		if layout[start+1+j] == codeLine {
			continue
		}
		trim := strings.TrimLeft(l, " ")
		if len(l)-len(trim) <= 3 && strings.HasPrefix(trim, "## ") {
			return "", "", fmt.Errorf("archiving [%s] would swallow a second section heading (%q) — refusing", version, l)
		}
	}
	sectionBlock := strings.Join(section, "\n")
	if isArchiveStub(section[1:]) {
		return "", "", fmt.Errorf("section [%s] is already archived", version)
	}
	pointer := archivePointer(section, layout[start:end], linkPath)
	stub := []string{section[0], "", pointer}
	if end < len(doc) {
		stub = append(stub, "")
	}
	newLines := append(append(append([]string{}, lines[:start]...), stub...), lines[end:]...)
	newContent := strings.Join(newLines, "\n")
	stubBlock := strings.Join(stub, "\n")
	// Re-composition must find the stub it just planted: if the stub text
	// already occurs earlier in the document (say, quoted mid-entry), the
	// first-occurrence Replace would restore the wrong spot — refuse.
	if strings.Replace(newContent, stubBlock, sectionBlock, 1) != content {
		return "", "", fmt.Errorf("archiving [%s] would not round-trip byte-identically — refusing", version)
	}
	rebased, err := rebaseLinks(section, layout[start:end])
	if err != nil {
		return "", "", fmt.Errorf("archiving [%s]: %v", version, err)
	}
	archive := strings.Join(rebased, "\n")
	appended := false
	if !strings.HasSuffix(archive, "\n") {
		archive += "\n"
		appended = true
	}
	// The inversion guard is anchored on the exact bytes runArchive writes,
	// not an intermediate: strip only the newline this function added, then
	// the inverse rewrite must reproduce the moved section. Under today's
	// two mappings the inverse is exact by construction — the golden tests
	// pin the written bytes; what this guard adds is refusal the day a
	// mapping is added to rebaseLinks without its inverse.
	candidate := archive
	if appended {
		candidate = strings.TrimSuffix(candidate, "\n")
	}
	restored := strings.Join(unrebaseLinks(strings.Split(candidate, "\n"), layout[start:end]), "\n")
	if restored != sectionBlock {
		return "", "", fmt.Errorf("archiving [%s]: the written archive would not invert to the moved section byte-for-byte — refusing", version)
	}
	return newContent, archive, nil
}

func archivePointer(section []string, layout []markdownLine, linkPath string) string {
	// Summarize the section's groups deduplicated in Keep-a-Changelog order —
	// the absorbed legacy backlog repeats its group headings per PR era, and
	// the stub names each group once.
	seen := map[string]bool{}
	for j, l := range section {
		if layout[j] != codeLine && strings.HasPrefix(l, "### ") {
			seen[strings.TrimPrefix(l, "### ")] = true
		}
	}
	groups := []string{}
	for _, g := range kacOrder {
		if seen[g] {
			groups = append(groups, g)
			delete(seen, g)
		}
	}
	for j, l := range section {
		if layout[j] == codeLine {
			continue
		}
		if g := strings.TrimPrefix(l, "### "); g != l && seen[g] {
			groups = append(groups, g)
			delete(seen, g)
		}
	}
	pointer := "The " + archiveMark + linkPath + "](./" + linkPath + ")."
	if len(groups) > 0 {
		pointer = strings.Join(groups, " · ") + " — the " + archiveMark + linkPath + "](./" + linkPath + ")."
	}
	return pointer
}

// runArchive is the `archive` subcommand: the archive file is written first
// (a crash between the writes leaves the changelog intact and the copy
// harmless), creation is exclusive so an existing archive file is never
// clobbered — except that a file left by exactly such an interrupted run,
// byte-identical to what this run would write, converges the retry.
func runArchive(changelogPath, dir, version string) error {
	content, err := os.ReadFile(changelogPath)
	if err != nil {
		return err
	}
	// Resolve both ends to absolute paths first: with a relative -dir and an
	// absolute -changelog (or the reverse) filepath.Rel has no common root
	// and fails with a raw stdlib error.
	absChangelog, err := filepath.Abs(changelogPath)
	if err != nil {
		return err
	}
	target := filepath.Join(dir, version+".md")
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	linkPath, err := filepath.Rel(filepath.Dir(absChangelog), absTarget)
	if err != nil {
		return err
	}
	newContent, archive, err := archiveSection(string(content), version, filepath.ToSlash(linkPath))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	switch {
	case errors.Is(err, fs.ErrExist):
		existing, rerr := os.ReadFile(target)
		if rerr != nil {
			return rerr
		}
		if string(existing) != archive {
			return fmt.Errorf("%s already exists with different content — refusing to clobber it", target)
		}
	case err != nil:
		return err
	default:
		_, werr := f.Write([]byte(archive))
		cerr := f.Close()
		if werr != nil {
			return werr
		}
		if cerr != nil {
			return cerr
		}
	}
	return os.WriteFile(changelogPath, []byte(newContent), 0o644)
}
