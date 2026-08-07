// Package main implements the release-time changelog tool (plans 27 and 28):
// `assemble` folds the changelog.d/ fragments — plus any legacy [Unreleased]
// body — into a new dated section of CHANGELOG.md, `notes` extracts a
// released section's body for GitHub Release notes, and `archive` moves a
// released section verbatim to docs/changelog/<version>.md behind an index
// stub, post-release. The ritual that runs them is docs/RELEASING.md; the
// Make entry points are `changelog`, `changelog-notes` and
// `changelog-archive`.
package main

import (
	"fmt"
	"math"
	"os"
	"os/exec"
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

var (
	// SemVer forbids leading zeros — 0.2.01 is not a version.
	versionRe  = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$`)
	fragNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+\.(added|changed|deprecated|removed|fixed|security)\.md$`)
	// Only the labels Keep a Changelog itself puts in the trailing block:
	// a released section body may legitimately end with its own
	// reference-definition line, and that line must stay with the body.
	linkRefRe       = regexp.MustCompile(`^\[(Unreleased|\d+\.\d+\.\d+)\]: \S+$`)
	unreleasedRefRe = regexp.MustCompile(`^\[Unreleased\]: (\S+)/compare/v\S+\.\.\.HEAD$`)
	releasedRefRe   = regexp.MustCompile(`^\[(\d+\.\d+\.\d+)\]: `)
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
		if !strings.HasPrefix(body, "- ") {
			return nil, fmt.Errorf("changelog.d/%s: a fragment is the final CHANGELOG entry verbatim and must start with %q", name, "- ")
		}
		for _, line := range strings.Split(body, "\n") {
			if headingRe.MatchString(line) {
				return nil, fmt.Errorf("changelog.d/%s: headings belong to the assembler, not a fragment", name)
			}
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
	refs, doc := splitTrailingRefs(strings.Split(content, "\n"))
	for _, l := range doc {
		if strings.HasPrefix(l, "## ["+version+"]") {
			return "", fmt.Errorf("section [%s] already exists", version)
		}
	}
	unrelIdx := -1
	for i, l := range doc {
		if l == "## [Unreleased]" {
			if unrelIdx >= 0 {
				return "", fmt.Errorf("two ## [Unreleased] sections — refusing to release only the first")
			}
			unrelIdx = i
		}
	}
	if unrelIdx < 0 {
		return "", fmt.Errorf("no ## [Unreleased] section")
	}
	restIdx := len(doc)
	for i := unrelIdx + 1; i < len(doc); i++ {
		if strings.HasPrefix(doc[i], "## ") {
			restIdx = i
			break
		}
	}
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
	return strings.Join(out, "\n") + "\n", nil
}

// notes returns the body of version's released section, without the
// link-reference block.
func notes(content, version string) (string, error) {
	if !versionRe.MatchString(version) {
		return "", fmt.Errorf("version %q is not X.Y.Z", version)
	}
	_, doc := splitTrailingRefs(strings.Split(content, "\n"))
	secIdx := -1
	for i, l := range doc {
		if strings.HasPrefix(l, "## ["+version+"]") {
			secIdx = i
			break
		}
	}
	if secIdx < 0 {
		return "", fmt.Errorf("no section [%s] in the changelog", version)
	}
	end := len(doc)
	for i := secIdx + 1; i < len(doc); i++ {
		if strings.HasPrefix(doc[i], "## ") {
			end = i
			break
		}
	}
	body := joinTrimmed(doc[secIdx+1:end]) + "\n"
	// An archived section's stub must never ship as a release body; re-runs
	// of a release workflow read the tag's checkout, where the section is
	// still inline.
	if strings.Contains(body, archiveMark) {
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
	re := regexp.MustCompile(`^## \[((?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*))\] - \d{4}-\d{2}-\d{2}$`)
	_, doc := splitTrailingRefs(strings.Split(content, "\n"))
	for _, l := range doc {
		if m := re.FindStringSubmatch(l); m != nil {
			return m[1], nil
		}
	}
	return "", fmt.Errorf("no released section in the changelog")
}

// clampNotes bounds body to cap bytes for a GitHub Release (which rejects
// oversized bodies outright): whole leading ### groups are kept while they
// fit, and a trailer links to the full section in the changelog at the tag.
// A cap even the trailer cannot satisfy is an error — over-cap output would
// just move the 422 downstream. The first cut under the fragment scheme
// absorbs the 5,300-line legacy backlog and needs this (plan 27 decision 4).
func clampNotes(body, base, version string, cap int) (string, error) {
	if cap <= 0 || len(body) <= cap {
		return body, nil
	}
	trailer := fmt.Sprintf("---\n\nTruncated: the full section is in [CHANGELOG.md](%s/blob/v%s/CHANGELOG.md).\n", base, version)
	if len(trailer) > cap {
		return "", fmt.Errorf("cap %d is smaller than the %d-byte truncation trailer itself", cap, len(trailer))
	}
	// Split into blocks at ### boundaries, then keep whole leading blocks
	// while they (plus the trailer) fit — no group is ever cut mid-entry.
	var blocks []string
	cur := ""
	for _, line := range strings.SplitAfter(body, "\n") {
		if strings.HasPrefix(line, "### ") && cur != "" {
			blocks = append(blocks, cur)
			cur = ""
		}
		cur += line
	}
	if cur != "" {
		blocks = append(blocks, cur)
	}
	kept := ""
	for _, b := range blocks {
		candidate := strings.TrimRight(kept+b, "\n") + "\n\n" + trailer
		if len(candidate) > cap {
			break
		}
		kept += b
	}
	kept = strings.TrimRight(kept, "\n")
	if kept == "" {
		return trailer, nil
	}
	return kept + "\n\n" + trailer, nil
}

// runNotes is the `notes` subcommand; out == "" or "-" writes to stdout, and
// a positive cap clamps the body (deriving the repository URL from the
// [Unreleased] link reference).
func runNotes(changelogPath, version, out string, cap int) error {
	content, err := os.ReadFile(changelogPath)
	if err != nil {
		return err
	}
	body, err := notes(string(content), version)
	if err != nil {
		return err
	}
	if cap > 0 && len(body) > cap {
		refs, _ := splitTrailingRefs(strings.Split(string(content), "\n"))
		base := ""
		for _, r := range refs {
			if m := unreleasedRefRe.FindStringSubmatch(r); m != nil {
				base = m[1]
				break
			}
		}
		if base == "" {
			return fmt.Errorf("notes exceed the %d-byte cap and no [Unreleased] link reference exists to derive the changelog link from", cap)
		}
		var cerr error
		if body, cerr = clampNotes(body, base, version, cap); cerr != nil {
			return cerr
		}
	}
	if out == "" || out == "-" {
		_, err = os.Stdout.WriteString(body)
		return err
	}
	return os.WriteFile(out, []byte(body), 0o644)
}

// archiveMark is the text every archive stub carries. archiveSection writes
// it; archiveSection and notes refuse a section that already carries it.
const archiveMark = "full section lives in ["

// archiveSection moves the dated section for version out of content (plan 28):
// the returned changelog keeps the exact heading line — `latest` and the
// tag-sanity check parse the file they always did — over a one-line pointer
// stub, and the archive text is the heading plus the body, verbatim. Nothing
// is returned unless swapping the stub back for the section reproduces
// content byte-for-byte — a lossy split is an error, never output.
func archiveSection(content, version, linkPath string) (string, string, error) {
	if !versionRe.MatchString(version) {
		return "", "", fmt.Errorf("version %q is not X.Y.Z", version)
	}
	lines := strings.Split(content, "\n")
	_, doc := splitTrailingRefs(lines)
	start := -1
	for i, l := range doc {
		if strings.HasPrefix(l, "## ["+version+"]") {
			start = i
			break
		}
	}
	if start < 0 {
		return "", "", fmt.Errorf("no section [%s] in the changelog", version)
	}
	end := len(doc)
	for i := start + 1; i < len(doc); i++ {
		if strings.HasPrefix(doc[i], "## ") {
			end = i
			break
		}
	}
	section := lines[start:end]
	// The archive must be exactly one section: the exact dated heading, then
	// a body with no further `## ` heading — a mis-slice that swallows a
	// neighbor (or starts mid-section) is refused before anything is written.
	// (The Replace round-trip below cannot catch slice-boundary errors: a
	// verbatim slice put back verbatim always reproduces the document.)
	if !regexp.MustCompile(`^## \[` + regexp.QuoteMeta(version) + `\] - \d{4}-\d{2}-\d{2}$`).MatchString(section[0]) {
		return "", "", fmt.Errorf("section [%s] heading %q is not the exact dated grammar", version, section[0])
	}
	for _, l := range section[1:] {
		if strings.HasPrefix(l, "## ") {
			return "", "", fmt.Errorf("archiving [%s] would swallow a second section heading (%q) — refusing", version, l)
		}
	}
	sectionBlock := strings.Join(section, "\n")
	if strings.Contains(sectionBlock, archiveMark) {
		return "", "", fmt.Errorf("section [%s] is already archived", version)
	}
	// Summarize the section's groups deduplicated in Keep-a-Changelog order —
	// the absorbed legacy backlog repeats its group headings per PR era, and
	// the stub names each group once.
	seen := map[string]bool{}
	for _, l := range section {
		if strings.HasPrefix(l, "### ") {
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
	for _, l := range section {
		if g := strings.TrimPrefix(l, "### "); g != l && seen[g] {
			groups = append(groups, g)
			delete(seen, g)
		}
	}
	pointer := "The " + archiveMark + linkPath + "](./" + linkPath + ")."
	if len(groups) > 0 {
		pointer = strings.Join(groups, " · ") + " — the " + archiveMark + linkPath + "](./" + linkPath + ")."
	}
	stub := []string{section[0], "", pointer}
	if end < len(doc) {
		stub = append(stub, "")
	}
	newLines := append(append(append([]string{}, lines[:start]...), stub...), lines[end:]...)
	newContent := strings.Join(newLines, "\n")
	stubBlock := strings.Join(stub, "\n")
	if strings.Replace(newContent, stubBlock, sectionBlock, 1) != content {
		return "", "", fmt.Errorf("archiving [%s] would not round-trip byte-identically — refusing", version)
	}
	archive := sectionBlock
	if !strings.HasSuffix(archive, "\n") {
		archive += "\n"
	}
	return newContent, archive, nil
}

// runArchive is the `archive` subcommand: the archive file is written first
// (a crash between the writes leaves the changelog intact and the copy
// harmless), and an existing archive file is never clobbered.
func runArchive(changelogPath, dir, version string) error {
	content, err := os.ReadFile(changelogPath)
	if err != nil {
		return err
	}
	target := filepath.Join(dir, version+".md")
	linkPath, err := filepath.Rel(filepath.Dir(changelogPath), target)
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
	if _, err := os.Stat(target); err == nil {
		return fmt.Errorf("%s already exists — refusing to clobber it", target)
	}
	if err := os.WriteFile(target, []byte(archive), 0o644); err != nil {
		return err
	}
	return os.WriteFile(changelogPath, []byte(newContent), 0o644)
}
