package api_test

import (
	"net/http"
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
// part is ignored and the name still derives from the frontmatter [5], which is
// only true if unknown parts in general are tolerated.
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
