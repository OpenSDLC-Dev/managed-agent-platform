package api_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/blobtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/skills"
)

const testSkillMD = `---
name: financial-skill
description: Reads and explains financial statements.
---

# Financial skill
`

type upFile struct{ name, content string }

// skillForm builds a multipart body with one files[] part per file and an
// optional display_name field, the exact shape every reference client emits.
func skillForm(t *testing.T, displayName *string, files []upFile) (contentType string, body string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, f := range files {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="files[]"; filename="%s"`, f.name))
		h.Set("Content-Type", "application/octet-stream")
		pw, err := w.CreatePart(h)
		if err != nil {
			t.Fatalf("create part: %v", err)
		}
		if _, err := pw.Write([]byte(f.content)); err != nil {
			t.Fatalf("write part: %v", err)
		}
	}
	if displayName != nil {
		if err := w.WriteField("display_name", *displayName); err != nil {
			t.Fatalf("write display_name: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close form: %v", err)
	}
	return w.FormDataContentType(), buf.String()
}

// skillFilePart is one file part of an upload with the form field name spelled
// out, so a test can send the bare "files" the reference refuses alongside the
// "files[]" it requires.
type skillFilePart struct{ field, name, content string }

// skillFormRaw builds a skill upload from exactly the parts named: the file
// parts first, then the text fields in order. skillForm covers the well-formed
// case; this one exists for the parts the form does not define.
func skillFormRaw(t *testing.T, parts []skillFilePart, fields ...[2]string) (contentType string, body string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, p := range parts {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, p.field, p.name))
		h.Set("Content-Type", "application/octet-stream")
		pw, err := w.CreatePart(h)
		if err != nil {
			t.Fatalf("create part: %v", err)
		}
		if _, err := pw.Write([]byte(p.content)); err != nil {
			t.Fatalf("write part: %v", err)
		}
	}
	for _, f := range fields {
		if err := w.WriteField(f[0], f[1]); err != nil {
			t.Fatalf("write field %s: %v", f[0], err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close form: %v", err)
	}
	return w.FormDataContentType(), buf.String()
}

// skillMDPart is the fixture SKILL.md as a well-formed files[] part.
func skillMDPart() []skillFilePart {
	return []skillFilePart{{"files[]", "financial-skill/SKILL.md", testSkillMD}}
}

// wantErrMsg is wantErr with the message pinned exactly: plan 39 reproduces
// several of the reference's sentences verbatim, and a paraphrase of one is a
// wire divergence, not a wording preference.
func wantErrMsg(t *testing.T, status int, body map[string]any, wantStatus int, wantType, wantMsg string) {
	t.Helper()
	wantErr(t, status, body, wantStatus, wantType)
	inner, _ := body["error"].(map[string]any)
	if msg, _ := inner["message"].(string); msg != wantMsg {
		t.Errorf("error.message = %q, want %q", msg, wantMsg)
	}
}

// wantOnlyFields asserts the object carries exactly the named keys. The GA
// skill and version objects are closed shapes (plan 39 decision 2), so a
// retired key surviving on the wire is a failure, not a harmless extra.
func wantOnlyFields(t *testing.T, obj map[string]any, keys ...string) {
	t.Helper()
	wantFields(t, obj, keys...)
	want := map[string]bool{}
	for _, k := range keys {
		want[k] = true
	}
	for k := range obj {
		if !want[k] {
			t.Errorf("unexpected wire field %q in %v", k, obj)
		}
	}
}

func (s *tserver) doForm(method, path, contentType, body string) (int, map[string]any) {
	s.t.Helper()
	res := s.doRaw(method, path, body, map[string]string{
		"x-api-key": testKey, "Content-Type": contentType,
	})
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		s.t.Fatalf("read response body: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		obj = nil
	}
	return res.StatusCode, obj
}

// createSkill uploads the default fixture skill and returns its rendered object.
func (s *tserver) createSkill(t *testing.T) map[string]any {
	t.Helper()
	ct, body := skillForm(t, nil, []upFile{
		{name: "financial-skill/SKILL.md", content: testSkillMD},
		{name: "financial-skill/reference.md", content: "notes"},
	})
	status, obj := s.doForm("POST", "/v1/skills", ct, body)
	if status != http.StatusOK {
		t.Fatalf("create skill: status %d, body %v", status, obj)
	}
	return obj
}

func testZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, content := range entries {
		f, err := w.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// TestSkillCreateLooseFiles covers the path-qualified multi-file upload form.
// The rendered shapes it produces are pinned exhaustively by
// TestSkillWireShapeIsGA; what is under test here is the form.
func TestSkillCreateLooseFiles(t *testing.T) {
	s := newTestServer(t)
	obj := s.createSkill(t)
	id, _ := obj["id"].(string)
	if !strings.HasPrefix(id, "skill_") {
		t.Errorf("id = %q, want skill_ prefix", id)
	}
	// display_name defaults from the SKILL.md name.
	if obj["display_name"] != "financial-skill" {
		t.Errorf("display_name = %v, want the frontmatter name", obj["display_name"])
	}

	// The create also minted the first version: it lists and resolves, and the
	// numeric identity behind it is the epoch-timestamp string the blob key
	// uses — internal now, but still what the storage layer is keyed on.
	status, body := s.do("GET", "/v1/skills/"+id+"/versions", nil)
	if status != http.StatusOK {
		t.Fatalf("list versions: %d %v", status, body)
	}
	versions := listData(t, body)
	if len(versions) != 1 {
		t.Fatalf("versions = %v, want exactly the minted one", versions)
	}
	v := versions[0]
	vid, _ := v["id"].(string)
	if vid != obj["latest_version_id"] {
		t.Errorf("version id %v is not the skill's latest_version_id %v", vid, obj["latest_version_id"])
	}
	if v["name"] != "financial-skill" || v["description"] != "Reads and explains financial statements." {
		t.Errorf("version object = %v", v)
	}
	if n := s.versionNumber(t, id, vid); n == "" || strings.Trim(n, "0123456789") != "" {
		t.Errorf("stored version number = %q, want an epoch-timestamp string", n)
	}
}

func TestSkillCreateZipRoundTrip(t *testing.T) {
	s := newTestServer(t)
	archive := testZip(t, map[string]string{
		"financial-skill/SKILL.md":  testSkillMD,
		"financial-skill/notes.txt": "keep",
	})
	ct, body := skillForm(t, nil, []upFile{{name: "financial-skill.zip", content: string(archive)}})
	status, obj := s.doForm("POST", "/v1/skills", ct, body)
	if status != http.StatusOK {
		t.Fatalf("zip create: %d %v", status, obj)
	}
	id, _ := obj["id"].(string)
	version, _ := obj["latest_version_id"].(string)

	// Download streams the stored object unmodified: byte-identical to the upload.
	res := s.doRaw("GET", "/v1/skills/"+id+"/versions/"+version+"/content", nil,
		map[string]string{"x-api-key": testKey})
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("download: status %d", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "application/zip" {
		t.Errorf("download content-type = %q", got)
	}
	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read download: %v", err)
	}
	if !bytes.Equal(got, archive) {
		t.Errorf("downloaded bytes differ from the uploaded archive (%d vs %d bytes)", len(got), len(archive))
	}
	if res.Header.Get("Content-Length") != fmt.Sprint(len(archive)) {
		t.Errorf("content-length = %q, want %d", res.Header.Get("Content-Length"), len(archive))
	}
}

// downloadArchive fetches a version's stored archive, returning the body and
// the digest the response advertised (empty when it advertised none). slot is
// any form the {version} path accepts.
func (s *tserver) downloadArchive(t *testing.T, id, slot string) (body []byte, digest string) {
	t.Helper()
	res := s.doRaw("GET", "/v1/skills/"+id+"/versions/"+slot+"/content", nil,
		map[string]string{"x-api-key": testKey})
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("download %s/%s: status %d", id, slot, res.StatusCode)
	}
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read download: %v", err)
	}
	return b, res.Header.Get(skills.ArchiveDigestHeader)
}

// storedDigest is the digest the registry recorded for a version, NULL (no
// digest recorded) coming back as the empty string.
func (s *tserver) storedDigest(t *testing.T, id, versionID string) string {
	t.Helper()
	var sha *string
	if err := s.pool.QueryRow(t.Context(),
		`SELECT sha256 FROM skill_versions WHERE skill_id = $1 AND id = $2`,
		id, versionID).Scan(&sha); err != nil {
		t.Fatalf("read stored digest: %v", err)
	}
	if sha == nil {
		return ""
	}
	return *sha
}

func TestSkillArchiveDigest(t *testing.T) {
	s := newTestServer(t)

	// Loose-files upload: the digest of the canonical zip the platform built is
	// recorded at upload and advertised on the download, so a consumer can tell
	// the served object from a corrupted or substituted one.
	created := s.createSkill(t)
	id, _ := created["id"].(string)
	v1, _ := created["latest_version_id"].(string)
	body, digest := s.downloadArchive(t, id, v1)
	if want := skills.Digest(body); digest != want {
		t.Errorf("download digest = %q, want %q (the served body's sha256)", digest, want)
	}
	if got := s.storedDigest(t, id, v1); got != digest {
		t.Errorf("stored digest = %q, header %q — the header must be the registry's record", got, digest)
	}

	// A second version records its own digest, not the first one's.
	ct, form := skillForm(t, nil, []upFile{
		{name: "financial-skill/SKILL.md", content: testSkillMD},
		{name: "financial-skill/v2.txt", content: "second"},
	})
	status, obj := s.doForm("POST", "/v1/skills/"+id+"/versions", ct, form)
	if status != http.StatusOK {
		t.Fatalf("create version: %d %v", status, obj)
	}
	v2, _ := obj["id"].(string)
	body2, digest2 := s.downloadArchive(t, id, v2)
	if want := skills.Digest(body2); digest2 != want {
		t.Errorf("second version digest = %q, want %q", digest2, want)
	}
	if digest2 == digest {
		t.Errorf("both versions recorded the same digest %q despite different content", digest)
	}

	// The zip upload form stores the client's bytes verbatim, so the recorded
	// digest is the digest of what the client sent.
	archive := testZip(t, map[string]string{
		"financial-skill/SKILL.md":  testSkillMD,
		"financial-skill/notes.txt": "keep",
	})
	ct, form = skillForm(t, nil, []upFile{{name: "financial-skill.zip", content: string(archive)}})
	status, obj = s.doForm("POST", "/v1/skills/"+id+"/versions", ct, form)
	if status != http.StatusOK {
		t.Fatalf("create zip version: %d %v", status, obj)
	}
	v3, _ := obj["id"].(string)
	if got := s.storedDigest(t, id, v3); got != skills.Digest(archive) {
		t.Errorf("zip-form digest = %q, want the uploaded archive's %q", got, skills.Digest(archive))
	}

	// A version predating the column records nothing: the download advertises
	// no digest at all rather than a wrong one, and consumers read it
	// unverified rather than refusing it.
	if _, err := s.pool.Exec(t.Context(),
		`UPDATE skill_versions SET sha256 = NULL WHERE skill_id = $1 AND id = $2`, id, v1); err != nil {
		t.Fatal(err)
	}
	if _, digest := s.downloadArchive(t, id, v1); digest != "" {
		t.Errorf("digest header on a version with no recorded digest = %q, want none", digest)
	}
}

// An explicit display_name, its derivation, its 255-byte cap and the
// uniqueness the GA rename dropped are all pinned by TestSkillCreateDisplayName
// in skillsupload_test.go, alongside the rest of the create form.

func TestSkillUploadRejects(t *testing.T) {
	s := newTestServer(t)
	empty := ""
	cases := map[string]struct {
		displayName *string
		files       []upFile
	}{
		"NoFiles":          {nil, nil},
		"FlatBasename":     {nil, []upFile{{name: "SKILL.md", content: testSkillMD}}},
		"MissingSkillMD":   {nil, []upFile{{name: "dir/notes.md", content: "x"}}},
		"BadName":          {nil, []upFile{{name: "Financial/SKILL.md", content: "---\nname: Financial\ndescription: d\n---\n"}}},
		"DirMismatch":      {nil, []upFile{{name: "other/SKILL.md", content: testSkillMD}}},
		"EmptyDisplayName": {&empty, []upFile{{name: "financial-skill/SKILL.md", content: testSkillMD}}},
		"BadZip":           {nil, []upFile{{name: "s.zip", content: "PK\x03\x04 corrupt"}}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ct, body := skillForm(t, tc.displayName, tc.files)
			status, obj := s.doForm("POST", "/v1/skills", ct, body)
			wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")
		})
	}
	if n := s.blobs.Len(); n != 0 {
		t.Errorf("rejected uploads left %d objects in storage", n)
	}
}

func TestSkillUploadFormErrors(t *testing.T) {
	s := newTestServer(t)

	t.Run("NotMultipart", func(t *testing.T) {
		status, obj := s.do("POST", "/v1/skills", map[string]any{"display_name": "x"})
		wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")
	})
	// A form carrying only an unknown field is not rejected for the unknown
	// field — that is ignored — but for the files[] part it therefore lacks.
	t.Run("OnlyAnUnknownField", func(t *testing.T) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		_ = w.WriteField("nope", "x")
		_ = w.Close()
		status, obj := s.doForm("POST", "/v1/skills", w.FormDataContentType(), buf.String())
		wantErrMsg(t, status, obj, http.StatusBadRequest, "invalid_request_error", "files[]: Field required")
	})
	t.Run("FilePartWithoutFilename", func(t *testing.T) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="files[]"`)
		pw, _ := w.CreatePart(h)
		_, _ = pw.Write([]byte("content"))
		_ = w.Close()
		status, obj := s.doForm("POST", "/v1/skills", w.FormDataContentType(), buf.String())
		wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")
	})
	t.Run("OversizedBody", func(t *testing.T) {
		big := strings.Repeat("x", 33<<20)
		ct, body := skillForm(t, nil, []upFile{{name: "financial-skill/big.bin", content: big}})
		status, obj := s.doForm("POST", "/v1/skills", ct, body)
		wantErr(t, status, obj, http.StatusRequestEntityTooLarge, "request_too_large")
	})
	t.Run("DisplayNameWithNUL", func(t *testing.T) {
		title := "bad\x00name"
		ct, body := skillForm(t, &title, []upFile{{name: "financial-skill/SKILL.md", content: testSkillMD}})
		status, obj := s.doForm("POST", "/v1/skills", ct, body)
		wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")
	})
}

func TestSkillGet(t *testing.T) {
	s := newTestServer(t)
	created := s.createSkill(t)
	id, _ := created["id"].(string)

	status, obj := s.do("GET", "/v1/skills/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("get: %d %v", status, obj)
	}
	for _, k := range []string{"id", "display_name", "latest_version_id", "type"} {
		if obj[k] != created[k] {
			t.Errorf("get %s = %v, create returned %v", k, obj[k], created[k])
		}
	}
	if src, _ := obj["source"].(map[string]any); src == nil || src["type"] != "custom" {
		t.Errorf("get source = %v, want the create's object", obj["source"])
	}

	status, obj = s.do("GET", "/v1/skills/skill_0000000000000000000000ok", nil)
	wantErr(t, status, obj, http.StatusNotFound, "not_found_error")
	// Malformed and short-name-shaped ids are 404s, never 500s.
	status, obj = s.do("GET", "/v1/skills/no-such-short-name", nil)
	wantErr(t, status, obj, http.StatusNotFound, "not_found_error")
	status, obj = s.do("GET", "/v1/skills/agent_0000000000000000000000ok", nil)
	wantErr(t, status, obj, http.StatusNotFound, "not_found_error")
	status, obj = s.do("GET", "/v1/skills/%00bad", nil)
	wantErr(t, status, obj, http.StatusNotFound, "not_found_error")
}

// insertAnthropicSkill plants an imported catalog row the way slice 3's
// importer will: a short-name id and a date-based version.
func (s *tserver) insertAnthropicSkill(t *testing.T, id, title, version string) {
	t.Helper()
	ctx := t.Context()
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO skills (id, source, display_title, latest_version) VALUES ($1, 'anthropic', $2, $3)`,
		id, title, version); err != nil {
		t.Fatalf("insert anthropic skill: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO skill_versions (id, skill_id, version, name, description, directory)
		 VALUES ('skillver_'||md5($1), $1, $2, $1, 'Prebuilt '||$1||' skill.', $1)`,
		id, version); err != nil {
		t.Fatalf("insert anthropic skill version: %v", err)
	}
}

func TestSkillList(t *testing.T) {
	s := newTestServer(t)
	custom := s.createSkill(t)
	s.insertAnthropicSkill(t, "xlsx", "Excel", "20250929")
	s.insertAnthropicSkill(t, "pdf", "PDF", "20250929")

	status, body := s.do("GET", "/v1/skills", nil)
	if status != http.StatusOK {
		t.Fatalf("list: %d %v", status, body)
	}
	if len(listData(t, body)) != 3 {
		t.Errorf("list = %v, want 3 skills", body)
	}
	if _, ok := body["next_page"]; !ok {
		t.Error("next_page missing from list envelope")
	}

	status, body = s.do("GET", "/v1/skills?source=custom", nil)
	if status != http.StatusOK {
		t.Fatalf("list custom: %d %v", status, body)
	}
	data := listData(t, body)
	if len(data) != 1 || data[0]["id"] != custom["id"] {
		t.Errorf("source=custom = %v", data)
	}

	// Cursor pagination walks the anthropic catalog, whose short-name ids are
	// not prefixed — the cursor must survive them.
	seen := map[string]bool{}
	page := ""
	for i := 0; i < 5; i++ {
		path := "/v1/skills?source=anthropic&limit=1"
		if page != "" {
			path += "&page=" + page
		}
		status, body = s.do("GET", path, nil)
		if status != http.StatusOK {
			t.Fatalf("page %d: %d %v", i, status, body)
		}
		for _, e := range listData(t, body) {
			seen[e["id"].(string)] = true
		}
		if page = nextPage(t, body); page == "" {
			break
		}
	}
	if !seen["xlsx"] || !seen["pdf"] || len(seen) != 2 {
		t.Errorf("paged anthropic ids = %v", seen)
	}

	// The parameter validation itself — the 1000 ceiling and the source pair —
	// is pinned by TestSkillListParams.
	status, body = s.do("GET", "/v1/skills?source=bogus", nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
}

func TestSkillVersionCreateAndLatestTracking(t *testing.T) {
	s := newTestServer(t)
	created := s.createSkill(t)
	id, _ := created["id"].(string)
	v1, _ := created["latest_version_id"].(string)

	ct, body := skillForm(t, nil, []upFile{
		{name: "financial-skill/SKILL.md", content: testSkillMD},
		{name: "financial-skill/v2.txt", content: "second"},
	})
	status, obj := s.doForm("POST", "/v1/skills/"+id+"/versions", ct, body)
	if status != http.StatusOK {
		t.Fatalf("create version: %d %v", status, obj)
	}
	v2, _ := obj["id"].(string)
	if v2 == "" || v2 == v1 {
		t.Fatalf("second version = %q (first %q)", v2, v1)
	}
	if obj["skill_id"] != id || obj["type"] != "skill_version" {
		t.Errorf("version object = %v", obj)
	}

	// The skill's latest_version_id follows, and updated_at moves.
	status, skill := s.do("GET", "/v1/skills/"+id, nil)
	if status != http.StatusOK || skill["latest_version_id"] != v2 {
		t.Errorf("skill after version create = %v, want latest_version_id %q", skill, v2)
	}

	// Versions of anthropic skills are not API-managed.
	s.insertAnthropicSkill(t, "xlsx", "Excel", "20250929")
	ct, body = skillForm(t, nil, []upFile{{name: "financial-skill/SKILL.md", content: testSkillMD}})
	status, obj = s.doForm("POST", "/v1/skills/xlsx/versions", ct, body)
	wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")

	// Neither are deletes: the imported catalog cannot be removed through the
	// management API (rerunning the importer is the operator's path back, so an
	// accidental DELETE must not empty the catalog).
	status, obj = s.do("DELETE", "/v1/skills/xlsx/versions/20250929", nil)
	wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")
	status, obj = s.do("DELETE", "/v1/skills/xlsx", nil)
	wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")
	status, skill = s.do("GET", "/v1/skills/xlsx", nil)
	if status != http.StatusOK {
		t.Fatalf("anthropic skill after refused deletes = %d %v", status, skill)
	}
	if got := s.versionNumber(t, "xlsx", skill["latest_version_id"].(string)); got != "20250929" {
		t.Errorf("anthropic latest_version_id names version %q, want the planted 20250929", got)
	}

	// Unknown skill 404s before any upload processing.
	ct, body = skillForm(t, nil, []upFile{{name: "financial-skill/SKILL.md", content: testSkillMD}})
	status, obj = s.doForm("POST", "/v1/skills/skill_0000000000000000000000ok/versions", ct, body)
	wantErr(t, status, obj, http.StatusNotFound, "not_found_error")
}

func TestSkillVersionListLimits(t *testing.T) {
	s := newTestServer(t)
	created := s.createSkill(t)
	id, _ := created["id"].(string)

	// The versions list accepts limit up to 1000, as the skills list now does.
	status, body := s.do("GET", "/v1/skills/"+id+"/versions?limit=1000", nil)
	if status != http.StatusOK {
		t.Fatalf("limit=1000: %d %v", status, body)
	}
	// Out of range, the versions list phrases it exactly as the collection
	// list does — the reference answers both with this one sentence.
	status, body = s.do("GET", "/v1/skills/"+id+"/versions?limit=1001", nil)
	wantErrMsg(t, status, body, http.StatusBadRequest, "invalid_request_error",
		"limit must be between 1 and 1000")

	status, body = s.do("GET", "/v1/skills/skill_0000000000000000000000ok/versions", nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")

	// Cursor pagination over versions.
	ct, form := skillForm(t, nil, []upFile{{name: "financial-skill/SKILL.md", content: testSkillMD}})
	if st, obj := s.doForm("POST", "/v1/skills/"+id+"/versions", ct, form); st != http.StatusOK {
		t.Fatalf("second version: %d %v", st, obj)
	}
	status, body = s.do("GET", "/v1/skills/"+id+"/versions?limit=1", nil)
	if status != http.StatusOK || len(listData(t, body)) != 1 {
		t.Fatalf("first page: %d %v", status, body)
	}
	cursor := nextPage(t, body)
	if cursor == "" {
		t.Fatal("expected a next_page cursor")
	}
	status, body = s.do("GET", "/v1/skills/"+id+"/versions?limit=1&page="+cursor, nil)
	if status != http.StatusOK || len(listData(t, body)) != 1 {
		t.Fatalf("second page: %d %v", status, body)
	}
	if nextPage(t, body) != "" {
		t.Error("expected the walk to end after two versions")
	}
}

func TestSkillVersionGet(t *testing.T) {
	s := newTestServer(t)
	created := s.createSkill(t)
	id, _ := created["id"].(string)
	version, _ := created["latest_version_id"].(string)

	status, obj := s.do("GET", "/v1/skills/"+id+"/versions/"+version, nil)
	if status != http.StatusOK || obj["id"] != version || obj["skill_id"] != id {
		t.Fatalf("get version: %d %v", status, obj)
	}

	// The slot's accepted forms and its refusals are pinned by
	// TestSkillVersionAddressing; here, that an id naming nothing is a 404.
	status, obj = s.do("GET", "/v1/skills/"+id+"/versions/999", nil)
	wantErr(t, status, obj, http.StatusNotFound, "not_found_error")
}

func TestSkillVersionDeleteRecomputesLatest(t *testing.T) {
	s := newTestServer(t)
	created := s.createSkill(t)
	id, _ := created["id"].(string)
	v1, _ := created["latest_version_id"].(string)
	newVersion := func() string {
		t.Helper()
		ct, form := skillForm(t, nil, []upFile{{name: "financial-skill/SKILL.md", content: testSkillMD}})
		st, obj := s.doForm("POST", "/v1/skills/"+id+"/versions", ct, form)
		if st != http.StatusOK {
			t.Fatalf("create version: %d %v", st, obj)
		}
		vid, _ := obj["id"].(string)
		return vid
	}
	v2, v3 := newVersion(), newVersion()

	// Deleting the newest version rolls latest_version_id back to the next one,
	// and the response echoes the deleted version's own id.
	status, del := s.do("DELETE", "/v1/skills/"+id+"/versions/"+v3, nil)
	if status != http.StatusOK || del["id"] != v3 || del["type"] != "skill_version_deleted" {
		t.Fatalf("delete version: %d %v", status, del)
	}
	status, skill := s.do("GET", "/v1/skills/"+id, nil)
	if status != http.StatusOK || skill["latest_version_id"] != v2 {
		t.Errorf("after deleting v3, skill = %v, want latest_version_id %q", skill, v2)
	}
	// Its archive left object storage with it.
	if n := s.blobs.Len(); n != 2 {
		t.Errorf("stored objects = %d, want 2 after deleting one of three versions", n)
	}

	status, del = s.do("DELETE", "/v1/skills/"+id+"/versions/"+v3, nil)
	wantErr(t, status, del, http.StatusNotFound, "not_found_error")

	// Deleting the newest again rolls back again. The last one standing cannot
	// be deleted at all — that refusal is TestSkillVersionDeleteRefusesOnlyVersion's.
	if status, del = s.do("DELETE", "/v1/skills/"+id+"/versions/"+v2, nil); status != http.StatusOK {
		t.Fatalf("delete second version: %d %v", status, del)
	}
	status, skill = s.do("GET", "/v1/skills/"+id, nil)
	if status != http.StatusOK || skill["latest_version_id"] != v1 {
		t.Errorf("after deleting v2, skill = %v, want latest_version_id %q", skill, v1)
	}
}

// TestSkillVersionCreateDoesNotRollBackLatest pins the create path's guard:
// versions are minted before the parent row is locked, so two concurrent
// creates can commit in the opposite order to their mint times. latest_version
// must only ever advance to a numerically newer version — a late-committing
// older create must not clobber a newer one. Simulated by planting a newer
// latest_version, then creating a version whose fresh (older) mint is
// numerically smaller.
func TestSkillVersionCreateDoesNotRollBackLatest(t *testing.T) {
	s := newTestServer(t)
	created := s.createSkill(t)
	id, _ := created["id"].(string)

	// A concurrent create already advanced latest_version to a same-length but
	// numerically greater value than any epoch-micros mint (16 nines > 17xx…).
	const newer = "9999999999999999"
	if _, err := s.pool.Exec(t.Context(),
		`UPDATE skills SET latest_version = $2 WHERE id = $1`, id, newer); err != nil {
		t.Fatalf("plant newer latest_version: %v", err)
	}

	ct, form := skillForm(t, nil, []upFile{{name: "financial-skill/SKILL.md", content: testSkillMD}})
	status, obj := s.doForm("POST", "/v1/skills/"+id+"/versions", ct, form)
	if status != http.StatusOK {
		t.Fatalf("create version: %d %v", status, obj)
	}
	if v := s.versionNumber(t, id, obj["id"].(string)); v >= newer {
		t.Fatalf("minted version %q is not older than the planted %q; test assumption broken", v, newer)
	}
	// The older mint landed as a row but did not roll latest_version back.
	if got := s.latestVersion(t, id); got != newer {
		t.Errorf("after creating an older version, latest_version = %q, want unchanged %q", got, newer)
	}
}

// TestSkillVersionDeleteLatestIsNumericMax pins the delete recompute: the new
// latest_version is the numerically greatest surviving version (length-then-
// lexical), not merely the most-recently-created row — the two diverge for
// imported or backfilled versions. Two extra versions are planted directly so
// created_at order is the opposite of numeric order, then the API-created
// version is deleted to trigger the recompute over the survivors.
func TestSkillVersionDeleteLatestIsNumericMax(t *testing.T) {
	s := newTestServer(t)
	ctx := t.Context()
	created := s.createSkill(t)
	id, _ := created["id"].(string)
	v1, _ := created["latest_version_id"].(string) // the 16-digit epoch mint, created now

	seed := func(vid, version, ago string) {
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO skill_versions (id, skill_id, version, name, description, directory, created_at)
			 VALUES ($1, $2, $3, 'M2', '', 'M2', now() - $4::interval)`,
			vid, id, version, ago); err != nil {
			t.Fatalf("seed version %s: %v", version, err)
		}
	}
	// The numerically largest (20 digits) is the EARLIEST created; the most
	// recently created survivor (16 digits) is numerically smaller.
	seed("skillver_m2big", "99999999999999999999", "2 hours")
	seed("skillver_m2recent", "1700000000000000", "1 minute")

	status, del := s.do("DELETE", "/v1/skills/"+id+"/versions/"+v1, nil)
	if status != http.StatusOK || del["type"] != "skill_version_deleted" {
		t.Fatalf("delete version: %d %v", status, del)
	}
	// created_at DESC would pick "1700000000000000"; numeric order must pick the
	// 20-digit value.
	if got := s.latestVersion(t, id); got != "99999999999999999999" {
		t.Errorf("after delete, latest_version = %q, want numeric max %q", got, "99999999999999999999")
	}
}

// The skill delete's cascade, its refusal on the anthropic catalog and its 404s
// are pinned by TestSkillDeleteCascadesOverVersions and
// TestSkillVersionCreateAndLatestTracking.

func TestSkillDownloadErrors(t *testing.T) {
	s := newTestServer(t)
	created := s.createSkill(t)
	id, _ := created["id"].(string)
	version, _ := created["latest_version_id"].(string)

	status, obj := s.do("GET", "/v1/skills/"+id+"/versions/999/content", nil)
	wantErr(t, status, obj, http.StatusNotFound, "not_found_error")

	// A version row whose archive vanished from object storage is an operator
	// incident, not a 404: the resource exists. The blob key is still keyed on
	// the numeric identity, which is why the test has to ask for it.
	if err := s.blobs.Delete(t.Context(),
		"skills/"+id+"/"+s.versionNumber(t, id, version)+".zip"); err != nil {
		t.Fatalf("delete object: %v", err)
	}
	status, obj = s.do("GET", "/v1/skills/"+id+"/versions/"+version+"/content", nil)
	wantErr(t, status, obj, http.StatusInternalServerError, "api_error")
}

func TestSkillsUnavailableWithoutObjectStorage(t *testing.T) {
	// A deployment without object storage keeps serving everything else;
	// the skills upload/download surface reports its absence cleanly.
	pool := newPoolWithKey(t)
	srv := httptest.NewServer(api.NewHandler(pool, nil, nil, nil))
	t.Cleanup(srv.Close)
	s := &tserver{t: t, url: srv.URL, pool: pool}

	ct, body := skillForm(t, nil, []upFile{{name: "financial-skill/SKILL.md", content: testSkillMD}})
	status, obj := s.doForm("POST", "/v1/skills", ct, body)
	wantErr(t, status, obj, http.StatusInternalServerError, "api_error")

	// Read paths still answer from the database.
	status, obj = s.do("GET", "/v1/skills", nil)
	if status != http.StatusOK {
		t.Errorf("list without object storage: %d %v", status, obj)
	}
}

// failingStore errors every Put — the probe for the claim-row → put → commit
// ordering: a storage failure must never leave a committed row behind.
type failingStore struct{ blob.Store }

func (failingStore) Put(context.Context, string, io.Reader, int64, string) error {
	return errors.New("storage down")
}

func TestFailedPutCommitsNoRows(t *testing.T) {
	pool := newPoolWithKey(t)
	working := blobtest.Mem()
	okSrv := httptest.NewServer(api.NewHandler(pool, working, nil, nil))
	t.Cleanup(okSrv.Close)
	badSrv := httptest.NewServer(api.NewHandler(pool, failingStore{working}, nil, nil))
	t.Cleanup(badSrv.Close)
	ok := &tserver{t: t, url: okSrv.URL, pool: pool, blobs: working}
	bad := &tserver{t: t, url: badSrv.URL, pool: pool, blobs: working}

	// Skill create against the failing store: 500, and no skill exists.
	ct, body := skillForm(t, nil, []upFile{{name: "financial-skill/SKILL.md", content: testSkillMD}})
	status, obj := bad.doForm("POST", "/v1/skills", ct, body)
	wantErr(t, status, obj, http.StatusInternalServerError, "api_error")
	if status, list := ok.do("GET", "/v1/skills", nil); status != http.StatusOK || len(listData(t, list)) != 0 {
		t.Fatalf("after failed create: %d %v, want an empty list", status, list)
	}

	// Version create against the failing store: 500, and the skill's version
	// set and latest_version are untouched.
	created := ok.createSkill(t)
	id, _ := created["id"].(string)
	v1, _ := created["latest_version_id"].(string)
	ct, body = skillForm(t, nil, []upFile{{name: "financial-skill/SKILL.md", content: testSkillMD}})
	status, obj = bad.doForm("POST", "/v1/skills/"+id+"/versions", ct, body)
	wantErr(t, status, obj, http.StatusInternalServerError, "api_error")
	status, versions := ok.do("GET", "/v1/skills/"+id+"/versions", nil)
	if status != http.StatusOK || len(listData(t, versions)) != 1 {
		t.Errorf("after failed version create: %d %v, want only the original version", status, versions)
	}
	if _, skill := ok.do("GET", "/v1/skills/"+id, nil); skill["latest_version_id"] != v1 {
		t.Errorf("latest_version_id = %v, want unchanged %q", skill["latest_version_id"], v1)
	}
}

func TestSkillRoutesAuthAndMethodFallbacks(t *testing.T) {
	s := newTestServer(t)
	res := s.doRaw("GET", "/v1/skills", nil, nil)
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated list = %d, want 401", res.StatusCode)
	}
	status, obj := s.do("PUT", "/v1/skills", nil)
	wantErr(t, status, obj, http.StatusMethodNotAllowed, "invalid_request_error")
	status, obj = s.do("PATCH", "/v1/skills/skill_x/versions/123", nil)
	wantErr(t, status, obj, http.StatusMethodNotAllowed, "invalid_request_error")
}

// The GA wire shapes and semantics (plan 39). Every expectation below is a byte
// the 2026-09-04 recording observed, not a reading of the SDK types; the
// bracketed numbers are that recording's entry indices.

// versionNumber is the internal numeric identity of a version row — the blob
// key's component, which plan 39 decision 3 keeps internally and takes off the
// wire. Tests exercising the numeric legacy alias read it from here rather than
// from a response, because no response carries it any more.
func (s *tserver) versionNumber(t *testing.T, skillID, versionID string) string {
	t.Helper()
	var version string
	if err := s.pool.QueryRow(t.Context(),
		`SELECT version FROM skill_versions WHERE skill_id = $1 AND id = $2`,
		skillID, versionID).Scan(&version); err != nil {
		t.Fatalf("read version number: %v", err)
	}
	return version
}

// latestVersion is the numeric latest_version column the registry maintains and
// the wire's latest_version_id is derived from. Tests that plant a value with
// no matching version row have to read it here, because the wire deliberately
// renders nothing for one.
func (s *tserver) latestVersion(t *testing.T, skillID string) string {
	t.Helper()
	var latest *string
	if err := s.pool.QueryRow(t.Context(),
		`SELECT latest_version FROM skills WHERE id = $1`, skillID).Scan(&latest); err != nil {
		t.Fatalf("read latest_version: %v", err)
	}
	if latest == nil {
		return ""
	}
	return *latest
}

// TestSkillWireShapeIsGA pins the converged object shapes [0, 2, 16, 30, 143]:
// seven keys on the skill, six on the version, source an object, and
// latest_version_id the version row's id rather than the numeric timestamp.
func TestSkillWireShapeIsGA(t *testing.T) {
	s := newTestServer(t)
	obj := s.createSkill(t)
	wantOnlyFields(t, obj, "type", "id", "display_name", "source", "latest_version_id",
		"created_at", "updated_at")
	if obj["type"] != "skill" {
		t.Errorf("type = %v", obj["type"])
	}
	src, _ := obj["source"].(map[string]any)
	if src == nil || len(src) != 1 || src["type"] != "custom" {
		t.Errorf(`source = %v, want the object {"type":"custom"}`, obj["source"])
	}
	id, _ := obj["id"].(string)
	lvid, _ := obj["latest_version_id"].(string)
	if !strings.HasPrefix(lvid, "skillver_") && !strings.HasPrefix(lvid, "skver_") {
		t.Fatalf("latest_version_id = %q, want a version id", lvid)
	}

	status, body := s.do("GET", "/v1/skills/"+id+"/versions", nil)
	if status != http.StatusOK {
		t.Fatalf("list versions: %d %v", status, body)
	}
	versions := listData(t, body)
	if len(versions) != 1 {
		t.Fatalf("versions = %v, want exactly the minted one", versions)
	}
	v := versions[0]
	wantOnlyFields(t, v, "type", "id", "skill_id", "name", "description", "created_at")
	if v["id"] != lvid {
		t.Errorf("latest_version_id = %v, but the only version's id is %v", lvid, v["id"])
	}
	if v["type"] != "skill_version" || v["skill_id"] != id || v["name"] != "financial-skill" ||
		v["description"] != "Reads and explains financial statements." {
		t.Errorf("version object = %v", v)
	}

	// The same shape on the single-version read and on the version create.
	status, one := s.do("GET", "/v1/skills/"+id+"/versions/"+lvid, nil)
	if status != http.StatusOK {
		t.Fatalf("get version: %d %v", status, one)
	}
	wantOnlyFields(t, one, "type", "id", "skill_id", "name", "description", "created_at")
	ct, form := skillForm(t, nil, []upFile{{name: "financial-skill/SKILL.md", content: testSkillMD}})
	status, made := s.doForm("POST", "/v1/skills/"+id+"/versions", ct, form)
	if status != http.StatusOK {
		t.Fatalf("create version: %d %v", status, made)
	}
	wantOnlyFields(t, made, "type", "id", "skill_id", "name", "description", "created_at")

	// And on the list envelope's entries, whose latest_version_id follows.
	status, list := s.do("GET", "/v1/skills", nil)
	if status != http.StatusOK {
		t.Fatalf("list skills: %d %v", status, list)
	}
	entries := listData(t, list)
	if len(entries) != 1 {
		t.Fatalf("list = %v, want one skill", entries)
	}
	wantOnlyFields(t, entries[0], "type", "id", "display_name", "source", "latest_version_id",
		"created_at", "updated_at")
	if entries[0]["latest_version_id"] != made["id"] {
		t.Errorf("listed latest_version_id = %v, want the newly created version %v",
			entries[0]["latest_version_id"], made["id"])
	}
}

// TestSkillVersionAddressing pins the {version} slot's four accepted forms
// (decision 4): a GA skver_ id, the legacy skillver_ id, "latest" on a read,
// and the numeric as a registered legacy alias.
func TestSkillVersionAddressing(t *testing.T) {
	s := newTestServer(t)
	created := s.createSkill(t)
	id, _ := created["id"].(string)
	vid, _ := created["latest_version_id"].(string)
	numeric := s.versionNumber(t, id, vid)

	for _, slot := range []string{vid, numeric, "latest"} {
		status, obj := s.do("GET", "/v1/skills/"+id+"/versions/"+slot, nil)
		if status != http.StatusOK || obj["id"] != vid {
			t.Errorf("GET versions/%s = %d %v, want the version %s", slot, status, obj, vid)
		}
	}

	// A GA skver_ id resolves on the same slot. The prefix is minted by a later
	// slice, so the row is renamed directly here — what is under test is the
	// path slot, not the minting.
	const ga = "skver_0123456789abcdefghjkmnpq"
	if _, err := s.pool.Exec(t.Context(),
		`UPDATE skill_versions SET id = $3 WHERE skill_id = $1 AND id = $2`, id, vid, ga); err != nil {
		t.Fatalf("rename version id: %v", err)
	}
	status, obj := s.do("GET", "/v1/skills/"+id+"/versions/"+ga, nil)
	if status != http.StatusOK || obj["id"] != ga {
		t.Errorf("GET versions/%s = %d %v", ga, status, obj)
	}
	if _, skill := s.do("GET", "/v1/skills/"+id, nil); skill["latest_version_id"] != ga {
		t.Errorf("latest_version_id = %v, want the renamed %q", skill["latest_version_id"], ga)
	}

	// And so does a legacy skillver_ id, which existing rows keep forever
	// because agent configs pin them (decision 9).
	const legacy = "skillver_9876543210zyxwvtsrqpnmkj"
	if _, err := s.pool.Exec(t.Context(),
		`UPDATE skill_versions SET id = $3 WHERE skill_id = $1 AND id = $2`, id, ga, legacy); err != nil {
		t.Fatalf("rename version id: %v", err)
	}
	if status, obj := s.do("GET", "/v1/skills/"+id+"/versions/"+legacy, nil); status != http.StatusOK || obj["id"] != legacy {
		t.Errorf("GET versions/%s = %d %v", legacy, status, obj)
	}
	if _, err := s.pool.Exec(t.Context(),
		`UPDATE skill_versions SET id = $3 WHERE skill_id = $1 AND id = $2`, id, legacy, ga); err != nil {
		t.Fatalf("restore version id: %v", err)
	}

	// The archive downloads by id as well as by the numeric legacy alias.
	for _, slot := range []string{ga, numeric} {
		res := s.doRaw("GET", "/v1/skills/"+id+"/versions/"+slot+"/content", nil,
			map[string]string{"x-api-key": testKey})
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("download by %s = %d, want 200", slot, res.StatusCode)
		}
	}

	// A value in none of the four forms is a 400; a well-shaped id naming
	// nothing is a 404.
	for _, slot := range []string{"not-a-version", "skver_UPPER", "%00bad"} {
		status, obj := s.do("GET", "/v1/skills/"+id+"/versions/"+slot, nil)
		wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")
	}
	status, obj = s.do("GET", "/v1/skills/"+id+"/versions/skver_nosuchversionid", nil)
	wantErr(t, status, obj, http.StatusNotFound, "not_found_error")
	status, obj = s.do("GET", "/v1/skills/"+id+"/versions/999", nil)
	wantErr(t, status, obj, http.StatusNotFound, "not_found_error")

	// "latest" on a skill that has none is a 404, not a 500.
	if _, err := s.pool.Exec(t.Context(),
		`UPDATE skills SET latest_version = NULL WHERE id = $1`, id); err != nil {
		t.Fatalf("clear latest_version: %v", err)
	}
	status, obj = s.do("GET", "/v1/skills/"+id+"/versions/latest", nil)
	wantErr(t, status, obj, http.StatusNotFound, "not_found_error")
}

// TestSkillContentDisposition pins the download's Content-Disposition, which
// the recording shows on every observed content download: an attachment named
// for the version's own slug, carried only as an RFC 5987 filename*.
func TestSkillContentDisposition(t *testing.T) {
	s := newTestServer(t)
	created := s.createSkill(t)
	id, _ := created["id"].(string)
	vid, _ := created["latest_version_id"].(string)

	disposition := func() string {
		t.Helper()
		res := s.doRaw("GET", "/v1/skills/"+id+"/versions/"+vid+"/content", nil,
			map[string]string{"x-api-key": testKey})
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("download: status %d", res.StatusCode)
		}
		return res.Header.Get("Content-Disposition")
	}
	if got, want := disposition(), "attachment; filename*=utf-8''financial-skill.zip"; got != want {
		t.Errorf("Content-Disposition = %q, want %q", got, want)
	}

	// A name that needs escaping is percent-encoded rather than interpolated,
	// so it cannot break the header. Planted directly: the upload validator
	// would never accept one.
	if _, err := s.pool.Exec(t.Context(),
		`UPDATE skill_versions SET name = $2 WHERE skill_id = $1`, id, `a b"é`); err != nil {
		t.Fatalf("plant a name needing escaping: %v", err)
	}
	if got, want := disposition(), "attachment; filename*=utf-8''a%20b%22%C3%A9.zip"; got != want {
		t.Errorf("Content-Disposition = %q, want %q", got, want)
	}
}

// TestSkillVersionLatestAliasRefusals pins the two sentences the reference
// answers "latest" with where it does not accept the alias [60, 136].
func TestSkillVersionLatestAliasRefusals(t *testing.T) {
	s := newTestServer(t)
	created := s.createSkill(t)
	id, _ := created["id"].(string)

	const addressIt = " Address a specific version id: read the skill's latest_version_id, " +
		"or GET /v1/skills/{skill_id}/versions/latest, and use that id."
	status, obj := s.do("DELETE", "/v1/skills/"+id+"/versions/latest", nil)
	wantErrMsg(t, status, obj, http.StatusBadRequest, "invalid_request_error",
		"'latest' is not accepted when deleting a skill version."+addressIt)
	status, obj = s.do("GET", "/v1/skills/"+id+"/versions/latest/content", nil)
	wantErrMsg(t, status, obj, http.StatusBadRequest, "invalid_request_error",
		"'latest' is not accepted when downloading a skill version."+addressIt)

	// The refusal is a refusal: nothing was deleted.
	if status, _ := s.do("GET", "/v1/skills/"+id+"/versions/latest", nil); status != http.StatusOK {
		t.Errorf("the refused delete removed the version: get latest = %d", status)
	}
}

// TestSkillDeleteCascadesOverVersions pins decision 6's first half [79-82]: the
// skill delete takes every version with it in one transaction, running the
// per-version archive cleanup — which is exactly why the schema must not carry
// an ON DELETE CASCADE that would drop the rows behind the handler's back.
func TestSkillDeleteCascadesOverVersions(t *testing.T) {
	s := newTestServer(t)
	created := s.createSkill(t)
	id, _ := created["id"].(string)
	ct, form := skillForm(t, nil, []upFile{
		{name: "financial-skill/SKILL.md", content: testSkillMD},
		{name: "financial-skill/v2.txt", content: "second"},
	})
	if st, obj := s.doForm("POST", "/v1/skills/"+id+"/versions", ct, form); st != http.StatusOK {
		t.Fatalf("second version: %d %v", st, obj)
	}
	if n := s.blobs.Len(); n != 2 {
		t.Fatalf("stored objects = %d, want one per version", n)
	}

	status, obj := s.do("DELETE", "/v1/skills/"+id, nil)
	if status != http.StatusOK || obj["id"] != id || obj["type"] != "skill_deleted" {
		t.Fatalf("delete skill: %d %v", status, obj)
	}
	status, obj = s.do("GET", "/v1/skills/"+id, nil)
	wantErr(t, status, obj, http.StatusNotFound, "not_found_error")
	status, obj = s.do("GET", "/v1/skills/"+id+"/versions", nil)
	wantErr(t, status, obj, http.StatusNotFound, "not_found_error")
	status, obj = s.do("DELETE", "/v1/skills/"+id, nil)
	wantErr(t, status, obj, http.StatusNotFound, "not_found_error")

	var rows int
	if err := s.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM skill_versions WHERE skill_id = $1`, id).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("%d version rows survived the cascade", rows)
	}
	if n := s.blobs.Len(); n != 0 {
		t.Errorf("stored objects after the cascade = %d, want every archive swept", n)
	}
}

// TestSkillVersionDeleteRefusesOnlyVersion pins decision 6's second half [111]:
// a skill can never reach zero versions through the API, which is what makes
// latest_version_id "always set".
func TestSkillVersionDeleteRefusesOnlyVersion(t *testing.T) {
	s := newTestServer(t)
	created := s.createSkill(t)
	id, _ := created["id"].(string)
	v1, _ := created["latest_version_id"].(string)
	const refusal = "cannot delete a Skill's only version. " +
		"Delete the Skill, or create another version first"

	status, obj := s.do("DELETE", "/v1/skills/"+id+"/versions/"+v1, nil)
	wantErrMsg(t, status, obj, http.StatusBadRequest, "invalid_request_error", refusal)
	if status, _ := s.do("GET", "/v1/skills/"+id+"/versions/"+v1, nil); status != http.StatusOK {
		t.Errorf("the refused delete removed the version anyway")
	}
	if n := s.blobs.Len(); n != 1 {
		t.Errorf("stored objects = %d, want the refused version's archive intact", n)
	}

	// With a second version the delete goes through, and the survivor then
	// refuses in its turn.
	ct, form := skillForm(t, nil, []upFile{{name: "financial-skill/SKILL.md", content: testSkillMD}})
	st, v2obj := s.doForm("POST", "/v1/skills/"+id+"/versions", ct, form)
	if st != http.StatusOK {
		t.Fatalf("second version: %d %v", st, v2obj)
	}
	v2, _ := v2obj["id"].(string)
	status, obj = s.do("DELETE", "/v1/skills/"+id+"/versions/"+v1, nil)
	if status != http.StatusOK || obj["id"] != v1 || obj["type"] != "skill_version_deleted" {
		t.Fatalf("delete one of two versions: %d %v", status, obj)
	}
	status, obj = s.do("DELETE", "/v1/skills/"+id+"/versions/"+v2, nil)
	wantErrMsg(t, status, obj, http.StatusBadRequest, "invalid_request_error", refusal)
}

// TestSkillListParams pins decision 10 [21-25]: the list ceiling is 1000, and
// the source filter did not widen when the source object's type set did.
func TestSkillListParams(t *testing.T) {
	s := newTestServer(t)
	s.createSkill(t)

	if status, body := s.do("GET", "/v1/skills?limit=1000", nil); status != http.StatusOK {
		t.Fatalf("limit=1000: %d %v", status, body)
	}
	for _, limit := range []string{"1001", "0", "-1", "nope"} {
		status, body := s.do("GET", "/v1/skills?limit="+limit, nil)
		wantErrMsg(t, status, body, http.StatusBadRequest, "invalid_request_error",
			"limit must be between 1 and 1000")
	}
	for _, src := range []string{"bogus", "plugin", "anthropic_example"} {
		status, body := s.do("GET", "/v1/skills?source="+src, nil)
		wantErrMsg(t, status, body, http.StatusBadRequest, "invalid_request_error",
			"source must be one of custom, anthropic")
	}
}
