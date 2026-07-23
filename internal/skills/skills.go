// Package skills validates skill uploads and normalizes them into the
// canonical archive form the registry stores: a zip whose single top-level
// entry is the skill directory, with SKILL.md at its root. Both the /v1/skills
// upload forms (loose path-qualified files, or one zip archive) and the slice-3
// operator import funnel through here, so the validation rules — the
// skills-guide's published constraints on name/description/size — cannot drift
// between entry points. Every error returned by this package is caused by the
// upload's content and is safe to echo to the client as a 400.
package skills

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

const (
	// MaxTotalBytes caps a skill's total uncompressed content (the
	// skills-guide's published 30 MB bundle limit).
	MaxTotalBytes = 30 << 20
	// MaxMembers caps the file count, matching the reference worker's own
	// extraction guard (anthropic-sdk-go tools/agenttoolset).
	MaxMembers = 10000

	maxNameLen        = 64
	maxDescriptionLen = 1024
	skillMDName       = "SKILL.md"
)

// File is one uploaded file: a slash-separated path as sent by the client
// ("financial-skill/SKILL.md") and its content.
type File struct {
	Path string
	Data []byte
}

// Bundle is a validated upload: the SKILL.md frontmatter extraction plus the
// canonical archive the registry stores and later materializes into sandboxes.
// SHA256 is Digest(Zip), recorded beside the metadata so materialization can
// prove the object it reads back is the archive that was validated here.
type Bundle struct {
	Name        string
	Description string
	Directory   string
	Zip         []byte
	SHA256      string
}

var nameRe = regexp.MustCompile(`^[a-z0-9-]+$`)

// xmlTagRe spots XML-shaped tags, which the skills-guide forbids in the
// description (they would collide with the model's prompt markup). A bare "<"
// in prose stays legal.
var xmlTagRe = regexp.MustCompile(`</?[a-zA-Z][^>]*>`)

// IsZip reports whether data begins with the zip local-file-header magic. The
// API uses it to pick the upload form when exactly one file part arrives;
// magic-byte detection is an inference recorded in docs/DIVERGENCES.md.
func IsZip(data []byte) bool {
	return bytes.HasPrefix(data, []byte("PK\x03\x04"))
}

// FromFiles validates the loose-files upload form and builds the canonical
// zip. Part order does not affect the archive bytes.
func FromFiles(files []File) (*Bundle, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("no files uploaded")
	}
	if len(files) > MaxMembers {
		return nil, fmt.Errorf("upload has %d files; the maximum is %d", len(files), MaxMembers)
	}
	dir := ""
	seen := make(map[string]bool, len(files))
	var total int64
	for _, f := range files {
		top, err := checkPath(f.Path)
		if err != nil {
			return nil, err
		}
		if dir == "" {
			dir = top
		} else if top != dir {
			return nil, fmt.Errorf("all files must share one top-level directory (found %q and %q)", dir, top)
		}
		if seen[f.Path] {
			return nil, fmt.Errorf("duplicate file %q", f.Path)
		}
		seen[f.Path] = true
		total += int64(len(f.Data))
	}
	if total > MaxTotalBytes {
		return nil, fmt.Errorf("upload is %d bytes; the maximum total is %d", total, MaxTotalBytes)
	}
	var skillMD []byte
	for _, f := range files {
		if f.Path == dir+"/"+skillMDName {
			skillMD = f.Data
		}
	}
	if skillMD == nil {
		return nil, fmt.Errorf("missing %s at the root of directory %q", skillMDName, dir)
	}
	name, description, err := parseFrontmatter(skillMD)
	if err != nil {
		return nil, err
	}
	if err := checkDirectoryName(dir, name); err != nil {
		return nil, err
	}

	// Canonical zip: entries sorted by path, no timestamps, so identical
	// content always stores identical bytes.
	sorted := make([]File, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, f := range sorted {
		fw, err := w.CreateHeader(&zip.FileHeader{Name: f.Path, Method: zip.Deflate})
		if err != nil {
			return nil, fmt.Errorf("archive %q: %v", f.Path, err)
		}
		if _, err := fw.Write(f.Data); err != nil {
			return nil, fmt.Errorf("archive %q: %v", f.Path, err)
		}
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("finish archive: %v", err)
	}
	zipped := buf.Bytes()
	return &Bundle{Name: name, Description: description, Directory: dir,
		Zip: zipped, SHA256: Digest(zipped)}, nil
}

// FromZip validates the zip upload form. The original bytes are kept verbatim
// as the stored archive — the download endpoint streams them unmodified.
func FromZip(data []byte) (*Bundle, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("upload is not a readable zip archive")
	}
	if len(zr.File) == 0 {
		return nil, fmt.Errorf("zip archive is empty")
	}
	if len(zr.File) > MaxMembers {
		return nil, fmt.Errorf("zip archive has %d entries; the maximum is %d", len(zr.File), MaxMembers)
	}
	dir := ""
	var total uint64
	var skillMD *zip.File
	for _, f := range zr.File {
		isDir := strings.HasSuffix(f.Name, "/")
		p := strings.TrimSuffix(f.Name, "/")
		var top string
		if isDir && !strings.Contains(p, "/") {
			// The top-level directory's own entry: a single segment is fine
			// here (files still must be path-qualified beneath it).
			if p == "" || p == "." || p == ".." || strings.ContainsRune(p, '\\') ||
				!utf8.ValidString(p) || strings.ContainsRune(p, 0) {
				return nil, fmt.Errorf("invalid zip entry %q", f.Name)
			}
			top = p
		} else {
			var err error
			if top, err = checkPath(p); err != nil {
				return nil, err
			}
		}
		if dir == "" {
			dir = top
		} else if top != dir {
			return nil, fmt.Errorf("zip archive must contain the skill directory as its single top-level entry (found %q and %q)", dir, top)
		}
		if !strings.HasSuffix(f.Name, "/") {
			total += f.UncompressedSize64
			if f.Name == dir+"/"+skillMDName {
				skillMD = f
			}
		}
	}
	if total > MaxTotalBytes {
		return nil, fmt.Errorf("archive content is %d bytes uncompressed; the maximum total is %d", total, MaxTotalBytes)
	}
	if skillMD == nil {
		return nil, fmt.Errorf("missing %s at the root of directory %q", skillMDName, dir)
	}
	rc, err := skillMD.Open()
	if err != nil {
		return nil, fmt.Errorf("read %s: %v", skillMDName, err)
	}
	defer rc.Close()
	// LimitReader guards against an archive whose declared sizes lie.
	md, err := io.ReadAll(io.LimitReader(rc, MaxTotalBytes+1))
	if err != nil || len(md) > MaxTotalBytes {
		return nil, fmt.Errorf("read %s: content does not match the archive's declared size", skillMDName)
	}
	name, description, err := parseFrontmatter(md)
	if err != nil {
		return nil, err
	}
	if err := checkDirectoryName(dir, name); err != nil {
		return nil, err
	}
	return &Bundle{Name: name, Description: description, Directory: dir,
		Zip: data, SHA256: Digest(data)}, nil
}

// checkPath validates one slash-separated file path — path-qualified, no
// escapes, storable — and returns its top-level directory segment.
func checkPath(p string) (top string, err error) {
	if !utf8.ValidString(p) || strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("file path is not valid UTF-8 text")
	}
	if strings.ContainsRune(p, '\\') {
		return "", fmt.Errorf("file path %q must use forward slashes", p)
	}
	segs := strings.Split(p, "/")
	if len(segs) < 2 {
		return "", fmt.Errorf("file %q must be path-qualified under the skill's top-level directory", p)
	}
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return "", fmt.Errorf("file path %q contains an empty, relative, or parent segment", p)
		}
	}
	return segs[0], nil
}

// checkDirectoryName enforces the skills-guide rule that the uploaded
// directory names the skill: compared to SKILL.md's name case- and
// underscore-insensitively.
func checkDirectoryName(dir, name string) error {
	norm := func(s string) string { return strings.ReplaceAll(strings.ToLower(s), "_", "-") }
	if norm(dir) != norm(name) {
		return fmt.Errorf("top-level directory %q does not match the skill name %q", dir, name)
	}
	return nil
}

// parseFrontmatter extracts and validates name/description from SKILL.md's
// YAML frontmatter. Unknown keys are tolerated.
func parseFrontmatter(md []byte) (name, description string, err error) {
	body, ok := frontmatterBlock(md)
	if !ok {
		return "", "", fmt.Errorf("%s must open with a --- YAML frontmatter block", skillMDName)
	}
	var fm struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal(body, &fm); err != nil {
		return "", "", fmt.Errorf("%s frontmatter is not valid YAML", skillMDName)
	}
	if fm.Name == "" {
		return "", "", fmt.Errorf("%s frontmatter is missing name", skillMDName)
	}
	if len(fm.Name) > maxNameLen {
		return "", "", fmt.Errorf("name must be at most %d characters", maxNameLen)
	}
	if !nameRe.MatchString(fm.Name) {
		return "", "", fmt.Errorf("name %q must contain only lowercase letters, digits, and hyphens", fm.Name)
	}
	for _, reserved := range []string{"anthropic", "claude"} {
		if strings.Contains(fm.Name, reserved) {
			return "", "", fmt.Errorf("name must not contain the reserved word %q", reserved)
		}
	}
	if fm.Description == "" {
		return "", "", fmt.Errorf("%s frontmatter is missing description", skillMDName)
	}
	if !utf8.ValidString(fm.Description) || strings.ContainsRune(fm.Description, 0) {
		return "", "", fmt.Errorf("description is not valid UTF-8 text")
	}
	if utf8.RuneCountInString(fm.Description) > maxDescriptionLen {
		return "", "", fmt.Errorf("description must be at most %d characters", maxDescriptionLen)
	}
	if xmlTagRe.MatchString(fm.Description) {
		return "", "", fmt.Errorf("description must not contain XML tags")
	}
	return fm.Name, fm.Description, nil
}

// frontmatterBlock returns the YAML between the opening --- line and the
// closing --- line (which may sit at EOF without a trailing newline). A
// leading UTF-8 BOM is tolerated — some Windows editors emit one, and it is
// not a reason to reject an otherwise-valid SKILL.md.
func frontmatterBlock(md []byte) ([]byte, bool) {
	s := strings.TrimPrefix(string(md), "\ufeff")
	line, rest, ok := strings.Cut(s, "\n")
	if !ok || strings.TrimRight(line, "\r") != "---" {
		return nil, false
	}
	var b strings.Builder
	for rest != "" {
		line, next, _ := strings.Cut(rest, "\n")
		if strings.TrimRight(line, "\r") == "---" {
			return []byte(b.String()), true
		}
		b.WriteString(line)
		b.WriteString("\n")
		rest = next
	}
	return nil, false
}
