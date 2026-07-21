package skills

import (
	"archive/zip"
	"bytes"
	"fmt"
	"sort"
	"strings"
	"testing"
)

const goodSkillMD = `---
name: financial-skill
description: Reads and explains financial statements.
license: Apache-2.0
---

# Financial skill

Instructions the model reads at Level 2.
`

func goodFiles() []File {
	return []File{
		{Path: "financial-skill/SKILL.md", Data: []byte(goodSkillMD)},
		{Path: "financial-skill/reference.md", Data: []byte("ratios cheat sheet")},
	}
}

func TestFromFilesHappy(t *testing.T) {
	b, err := FromFiles(goodFiles())
	if err != nil {
		t.Fatalf("FromFiles: %v", err)
	}
	if b.Name != "financial-skill" {
		t.Errorf("Name = %q", b.Name)
	}
	if b.Description != "Reads and explains financial statements." {
		t.Errorf("Description = %q", b.Description)
	}
	if b.Directory != "financial-skill" {
		t.Errorf("Directory = %q", b.Directory)
	}
	// The canonical zip holds exactly the uploaded files under the directory.
	zr, err := zip.NewReader(bytes.NewReader(b.Zip), int64(len(b.Zip)))
	if err != nil {
		t.Fatalf("read canonical zip: %v", err)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(rc); err != nil {
			t.Fatalf("read %s: %v", f.Name, err)
		}
		rc.Close()
		got[f.Name] = buf.String()
	}
	if len(got) != 2 || got["financial-skill/SKILL.md"] != goodSkillMD ||
		got["financial-skill/reference.md"] != "ratios cheat sheet" {
		t.Errorf("canonical zip contents = %v", got)
	}
}

func TestFromFilesIsDeterministic(t *testing.T) {
	a, err := FromFiles(goodFiles())
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	// Same content, different part order: the archive must not depend on it.
	files := goodFiles()
	files[0], files[1] = files[1], files[0]
	b, err := FromFiles(files)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !bytes.Equal(a.Zip, b.Zip) {
		t.Error("canonical zip bytes differ for identical content")
	}
}

func TestDirectoryUnderscoreCaseInsensitiveMatch(t *testing.T) {
	files := []File{{Path: "Financial_Skill/SKILL.md", Data: []byte(goodSkillMD)}}
	b, err := FromFiles(files)
	if err != nil {
		t.Fatalf("FromFiles: %v", err)
	}
	if b.Directory != "Financial_Skill" {
		t.Errorf("Directory = %q, want the uploaded spelling", b.Directory)
	}
}

func skillMD(name, description string) string {
	return fmt.Sprintf("---\nname: %s\ndescription: %s\n---\nbody\n", name, description)
}

func TestFromFilesRejects(t *testing.T) {
	huge := make([]byte, MaxTotalBytes+1)
	cases := map[string][]File{
		"Empty":        {},
		"FlatBasename": {{Path: "SKILL.md", Data: []byte(goodSkillMD)}},
		"MixedTopLevelDirs": {
			{Path: "a/SKILL.md", Data: []byte(skillMD("a", "d"))},
			{Path: "b/notes.md", Data: []byte("x")},
		},
		"MissingSkillMD":    {{Path: "dir/notes.md", Data: []byte("x")}},
		"NestedSkillMDOnly": {{Path: "dir/sub/SKILL.md", Data: []byte(goodSkillMD)}},
		"DuplicatePath": {
			{Path: "financial-skill/SKILL.md", Data: []byte(goodSkillMD)},
			{Path: "financial-skill/SKILL.md", Data: []byte(goodSkillMD)},
		},
		"DotDotSegment":         {{Path: "financial-skill/../SKILL.md", Data: []byte(goodSkillMD)}},
		"AbsolutePath":          {{Path: "/financial-skill/SKILL.md", Data: []byte(goodSkillMD)}},
		"Backslash":             {{Path: `financial-skill\SKILL.md`, Data: []byte(goodSkillMD)}},
		"EmptySegment":          {{Path: "financial-skill//SKILL.md", Data: []byte(goodSkillMD)}},
		"TrailingSlash":         {{Path: "financial-skill/SKILL.md/", Data: []byte(goodSkillMD)}},
		"OversizedTotal":        {{Path: "financial-skill/SKILL.md", Data: []byte(goodSkillMD)}, {Path: "financial-skill/big.bin", Data: huge}},
		"DirectoryNameMismatch": {{Path: "other-dir/SKILL.md", Data: []byte(goodSkillMD)}},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := FromFiles(files); err == nil {
				t.Error("FromFiles accepted an invalid upload")
			}
		})
	}
}

func TestFromFilesRejectsTooManyMembers(t *testing.T) {
	files := []File{{Path: "s/SKILL.md", Data: []byte(skillMD("s", "d"))}}
	for i := 0; i < MaxMembers; i++ {
		files = append(files, File{Path: fmt.Sprintf("s/f%d", i), Data: []byte("x")})
	}
	if _, err := FromFiles(files); err == nil {
		t.Error("FromFiles accepted more than MaxMembers files")
	}
}

func TestFrontmatterValidation(t *testing.T) {
	longDesc := strings.Repeat("d", 1025)
	okLongDesc := strings.Repeat("d", 1024)
	cases := map[string]struct {
		md string
		ok bool
	}{
		"NoFrontmatter":        {"# just markdown\n", false},
		"Unterminated":         {"---\nname: a\ndescription: d\n", false},
		"MissingName":          {"---\ndescription: d\n---\n", false},
		"MissingDescription":   {"---\nname: a\n---\n", false},
		"EmptyDescription":     {"---\nname: a\ndescription: \"\"\n---\n", false},
		"BadYAML":              {"---\nname: [\n---\n", false},
		"UppercaseName":        {skillMD("Financial", "d"), false},
		"NameWithSpace":        {"---\nname: my skill\ndescription: d\n---\n", false},
		"NameTooLong":          {skillMD(strings.Repeat("a", 65), "d"), false},
		"NameMaxLen":           {skillMD(strings.Repeat("a", 64), "d"), true},
		"ReservedClaude":       {skillMD("claude-helper", "d"), false},
		"ReservedAnthropic":    {skillMD("my-anthropic-skill", "d"), false},
		"DescriptionTooLong":   {skillMD("a", longDesc), false},
		"DescriptionMaxLen":    {skillMD("a", okLongDesc), true},
		"DescriptionXMLTag":    {skillMD("a", "use <thinking> tags"), false},
		"UnknownKeysTolerated": {"---\nname: a\ndescription: d\nallowed-tools: [bash]\nextra: 1\n---\n", true},
		"CRLFLineEndings":      {"---\r\nname: a\r\ndescription: d\r\n---\r\nbody\r\n", true},
		"NoBody":               {"---\nname: a\ndescription: d\n---", true},
		"LeadingBOM":           {"\ufeff---\nname: a\ndescription: d\n---\n", true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := "a"
			if strings.Contains(tc.md, "name: "+strings.Repeat("a", 64)) {
				dir = strings.Repeat("a", 64)
			}
			_, err := FromFiles([]File{{Path: dir + "/SKILL.md", Data: []byte(tc.md)}})
			if tc.ok && err != nil {
				t.Errorf("rejected a valid SKILL.md: %v", err)
			}
			if !tc.ok && err == nil {
				t.Error("accepted an invalid SKILL.md")
			}
		})
	}
}

func buildZip(t *testing.T, entries map[string]string, dirs ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, d := range dirs {
		if _, err := w.Create(d); err != nil {
			t.Fatalf("create dir entry: %v", err)
		}
	}
	// Deterministic order for the test's own sanity.
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f, err := w.Create(n)
		if err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
		if _, err := f.Write([]byte(entries[n])); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func TestFromZipHappy(t *testing.T) {
	data := buildZip(t, map[string]string{
		"financial-skill/SKILL.md":     goodSkillMD,
		"financial-skill/reference.md": "notes",
	}, "financial-skill/")
	b, err := FromZip(data)
	if err != nil {
		t.Fatalf("FromZip: %v", err)
	}
	if b.Name != "financial-skill" || b.Directory != "financial-skill" {
		t.Errorf("Name/Directory = %q/%q", b.Name, b.Directory)
	}
	// The uploaded archive is stored verbatim (the download endpoint streams
	// the stored object unmodified).
	if !bytes.Equal(b.Zip, data) {
		t.Error("FromZip did not keep the original archive bytes")
	}
}

func TestFromZipRejects(t *testing.T) {
	cases := map[string][]byte{
		"NotAZip":     []byte("PK but not really"),
		"TwoTopLevel": buildZip(t, map[string]string{"a/SKILL.md": goodSkillMD, "b/x": "y"}),
		"MissingSkillMD": buildZip(t, map[string]string{
			"financial-skill/notes.md": "x",
		}),
		"DotDotMember":   buildZip(t, map[string]string{"financial-skill/SKILL.md": goodSkillMD, "financial-skill/../x": "y"}),
		"AbsoluteMember": buildZip(t, map[string]string{"/financial-skill/SKILL.md": goodSkillMD}),
		"BackslashMember": buildZip(t, map[string]string{
			`financial-skill\SKILL.md`: goodSkillMD,
		}),
		"Empty": buildZip(t, map[string]string{}),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := FromZip(data); err == nil {
				t.Error("FromZip accepted an invalid archive")
			}
		})
	}
}

func TestFromZipRejectsDeclaredSizeOverTotal(t *testing.T) {
	big := bytes.Repeat([]byte{0}, MaxTotalBytes+1) // compresses to almost nothing
	data := buildZip(t, map[string]string{
		"s/SKILL.md": skillMD("s", "d"),
		"s/big.bin":  string(big),
	})
	if _, err := FromZip(data); err == nil {
		t.Error("FromZip accepted an archive whose declared sizes exceed the total cap")
	}
}

func TestFromZipRejectsTooManyMembers(t *testing.T) {
	entries := map[string]string{"s/SKILL.md": skillMD("s", "d")}
	for i := 0; i <= MaxMembers; i++ {
		entries[fmt.Sprintf("s/f%d", i)] = "x"
	}
	if _, err := FromZip(buildZip(t, entries)); err == nil {
		t.Error("FromZip accepted more than MaxMembers entries")
	}
}

func TestIsZip(t *testing.T) {
	if !IsZip(buildZip(t, map[string]string{"a/SKILL.md": goodSkillMD})) {
		t.Error("IsZip = false for a real zip")
	}
	for _, data := range [][]byte{nil, []byte("PK"), []byte("---\nname: a\n---\n")} {
		if IsZip(data) {
			t.Errorf("IsZip = true for %q", data)
		}
	}
}
