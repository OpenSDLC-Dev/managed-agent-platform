package api_test

import (
	"archive/zip"
	"bytes"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// The create form's GA shape (plan 39 decisions 7 and 8), every case a byte the
// 2026-09-04 recording observed rather than a reading of the SDK types.

// TestSkillCreateDisplayName pins the field name, its derivation, its cap and
// the uniqueness that went with the rename [2, 3, 4, 6, 7].
func TestSkillCreateDisplayName(t *testing.T) {
	s := newTestServer(t)

	ct, body := skillFormRaw(t, skillMDPart(), [2]string{"display_name", "Quarterly Reports"})
	status, obj := s.doForm("POST", "/v1/skills", ct, body)
	if status != http.StatusOK || obj["display_name"] != "Quarterly Reports" {
		t.Fatalf("create with display_name: %d %v", status, obj)
	}

	// The same name again: display_name is not unique. Refusing a create the
	// reference accepts is a harder break than accepting one it refuses.
	ct, body = skillFormRaw(t, skillMDPart(), [2]string{"display_name", "Quarterly Reports"})
	status, obj = s.doForm("POST", "/v1/skills", ct, body)
	if status != http.StatusOK || obj["display_name"] != "Quarterly Reports" {
		t.Fatalf("duplicate display_name: %d %v", status, obj)
	}

	// Omitted: derived from the SKILL.md frontmatter name.
	ct, body = skillFormRaw(t, skillMDPart())
	status, obj = s.doForm("POST", "/v1/skills", ct, body)
	if status != http.StatusOK || obj["display_name"] != "financial-skill" {
		t.Fatalf("derived display_name: %d %v", status, obj)
	}

	// 255 characters is accepted; 256 gets the reference's own sentence.
	ct, body = skillFormRaw(t, skillMDPart(), [2]string{"display_name", strings.Repeat("n", 255)})
	if status, obj := s.doForm("POST", "/v1/skills", ct, body); status != http.StatusOK {
		t.Fatalf("255-character display_name: %d %v", status, obj)
	}
	ct, body = skillFormRaw(t, skillMDPart(), [2]string{"display_name", strings.Repeat("n", 256)})
	status, obj = s.doForm("POST", "/v1/skills", ct, body)
	wantErrMsg(t, status, obj, http.StatusBadRequest, "invalid_request_error",
		"display_name must be at most 255 characters long")

	// The unit is characters, as the sentence says: 255 CJK ones are 765 bytes
	// and still accepted, whole, and 256 are still the refusal. The rows above
	// cannot see this — in ASCII the two units coincide.
	cjk := strings.Repeat("名", 255)
	ct, body = skillFormRaw(t, skillMDPart(), [2]string{"display_name", cjk})
	status, obj = s.doForm("POST", "/v1/skills", ct, body)
	if status != http.StatusOK {
		t.Fatalf("255-character multi-byte display_name: %d %v", status, obj)
	}
	if obj["display_name"] != cjk {
		t.Errorf("display_name round-tripped as %d characters, want the 255 sent",
			utf8.RuneCountInString(obj["display_name"].(string)))
	}
	ct, body = skillFormRaw(t, skillMDPart(), [2]string{"display_name", strings.Repeat("名", 256)})
	status, obj = s.doForm("POST", "/v1/skills", ct, body)
	wantErrMsg(t, status, obj, http.StatusBadRequest, "invalid_request_error",
		"display_name must be at most 255 characters long")
}

// TestSkillCreateIgnoresUnknownFormParts pins decision 8: a stray display_title
// part is ignored and the name still derives from the frontmatter [5]. The
// reference ignores description and xyzzy the same way, so the rule this pins
// extrapolates from three observed names rather than from one.
func TestSkillCreateIgnoresUnknownFormParts(t *testing.T) {
	s := newTestServer(t)

	ct, body := skillFormRaw(t, skillMDPart(),
		[2]string{"display_title", "Ignored Title"}, [2]string{"nope", "x"})
	status, obj := s.doForm("POST", "/v1/skills", ct, body)
	if status != http.StatusOK {
		t.Fatalf("create carrying a stray display_title part: %d %v", status, obj)
	}
	if obj["display_name"] != "financial-skill" {
		t.Errorf("display_name = %v, want the frontmatter name — the display_title part is ignored, not honored",
			obj["display_name"])
	}
	if _, ok := obj["display_title"]; ok {
		t.Errorf("display_title survived on the wire: %v", obj)
	}

	// The version form has no display_name of its own, so a client that sends
	// one gets the same tolerance rather than a rejection.
	id, _ := obj["id"].(string)
	ct, body = skillFormRaw(t, skillMDPart(), [2]string{"display_name", "ignored on this form"})
	if status, obj := s.doForm("POST", "/v1/skills/"+id+"/versions", ct, body); status != http.StatusOK {
		t.Fatalf("version create carrying a display_name part: %d %v", status, obj)
	}
}

// TestSkillCreateRequiresFilesPart pins the one part that is still required,
// with the reference's message: a form with no file part [11], and a part named
// bare "files" [8], which is not files[] and so is ignored into the same state.
func TestSkillCreateRequiresFilesPart(t *testing.T) {
	s := newTestServer(t)
	const want = "files[]: Field required"

	ct, body := skillFormRaw(t, nil, [2]string{"display_name", "no files"})
	status, obj := s.doForm("POST", "/v1/skills", ct, body)
	wantErrMsg(t, status, obj, http.StatusBadRequest, "invalid_request_error", want)

	ct, body = skillFormRaw(t, []skillFilePart{{"files", "financial-skill/SKILL.md", testSkillMD}})
	status, obj = s.doForm("POST", "/v1/skills", ct, body)
	wantErrMsg(t, status, obj, http.StatusBadRequest, "invalid_request_error", want)

	if n := s.blobs.Len(); n != 0 {
		t.Errorf("refused uploads left %d objects in storage", n)
	}
}

// TestSkillUploadRefusesInReferenceWords pins the bundle refusals the
// reference was recorded wording on both upload routes (2026-09-12-followups
// skills-api.json). A frontmatter name or description over its cap gets one
// sentence for both [8 `rec.skill-upload.create.long-name`, 9
// `rec.skill-upload.create.long-description`, 18 and 19 the version twins],
// naming neither the field nor the cap, so the rejection log line carries
// both. A manifest stored as a symbolic link gets the archive-wide sentence
// [5 `rec.skill-upload.create.symlink-manifest`, 15 the version twin]; the
// recorded archive's entries were flat, which ours refuses earlier for want
// of a top-level directory (#630), so the fixture here is path-qualified.
func TestSkillUploadRefusesInReferenceWords(t *testing.T) {
	s := newTestServer(t)
	skillID, _ := s.createSkill(t)["id"].(string)
	routes := map[string]string{"create": "/v1/skills", "version": "/v1/skills/" + skillID + "/versions"}

	longName := strings.Repeat("x", 65)
	for _, tc := range []struct {
		name  string
		file  upFile
		field string
		limit int
	}{
		// The recorded long-name manifest used CRLF line endings; so does this.
		{"long-name", upFile{longName + "/SKILL.md",
			"---\r\nname: " + longName + "\r\ndescription: d\r\n---\r\n"}, "name", 64},
		{"long-description", upFile{"financial-skill/SKILL.md",
			"---\nname: financial-skill\ndescription: " + strings.Repeat("d", 1025) + "\n---\n"}, "description", 1024},
	} {
		for route, path := range routes {
			t.Run(route+"_"+tc.name, func(t *testing.T) {
				logs := captureLogs(t, slog.LevelInfo)
				ct, body := skillForm(t, nil, []upFile{tc.file})
				status, obj := s.doForm("POST", path, ct, body)
				wantErrMsg(t, status, obj, http.StatusBadRequest, "invalid_request_error",
					"`name` and `description` must resolve from `SKILL.md` frontmatter or its fallbacks, within their length limits")
				line := ""
				for _, l := range strings.Split(logs(), "\n") {
					if strings.Contains(l, "skill upload rejected") {
						line = l
					}
				}
				for _, want := range []string{
					`reason="` + tc.field + " must be at most " + strconv.Itoa(tc.limit) + ` characters"`,
					"request_id=" + obj["request_id"].(string)} {
					if !strings.Contains(line, want) {
						t.Errorf("rejection log line %q lacks %q", line, want)
					}
				}
			})
		}
	}

	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	h := &zip.FileHeader{Name: "financial-skill/SKILL.md", Method: zip.Deflate}
	h.SetMode(0o777 | fs.ModeSymlink)
	h.CreatorVersion = 3 << 8 // Unix host, so the type bits are read
	f, err := w.CreateHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(testSkillMD)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	for route, path := range routes {
		t.Run(route+"_symlink-manifest", func(t *testing.T) {
			ct, body := skillForm(t, nil, []upFile{{"symlink-manifest.zip", buf.String()}})
			status, obj := s.doForm("POST", path, ct, body)
			wantErrMsg(t, status, obj, http.StatusBadRequest, "invalid_request_error",
				"archives must not contain symbolic links")
		})
	}

	if n := s.blobs.Len(); n != 1 {
		t.Errorf("refused uploads left %d objects in storage, want only the fixture skill's", n)
	}
}
