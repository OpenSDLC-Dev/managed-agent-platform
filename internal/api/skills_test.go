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
)

const testSkillMD = `---
name: financial-skill
description: Reads and explains financial statements.
---

# Financial skill
`

type upFile struct{ name, content string }

// skillForm builds a multipart body with one files[] part per file and an
// optional display_title field, the exact shape every reference client emits.
func skillForm(t *testing.T, displayTitle *string, files []upFile) (contentType string, body string) {
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
	if displayTitle != nil {
		if err := w.WriteField("display_title", *displayTitle); err != nil {
			t.Fatalf("write display_title: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close form: %v", err)
	}
	return w.FormDataContentType(), buf.String()
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

func TestSkillCreateLooseFiles(t *testing.T) {
	s := newTestServer(t)
	obj := s.createSkill(t)
	wantFields(t, obj, "id", "created_at", "display_title", "latest_version", "source", "type", "updated_at")
	id, _ := obj["id"].(string)
	if !strings.HasPrefix(id, "skill_") {
		t.Errorf("id = %q, want skill_ prefix", id)
	}
	if obj["type"] != "skill" || obj["source"] != "custom" {
		t.Errorf("type/source = %v/%v", obj["type"], obj["source"])
	}
	// display_title defaults from the SKILL.md name.
	if obj["display_title"] != "financial-skill" {
		t.Errorf("display_title = %v, want the frontmatter name", obj["display_title"])
	}
	lv, _ := obj["latest_version"].(string)
	if lv == "" || strings.Trim(lv, "0123456789") != "" {
		t.Errorf("latest_version = %q, want an epoch-timestamp string", lv)
	}

	// The create also minted version 1: it lists and resolves.
	status, body := s.do("GET", "/v1/skills/"+id+"/versions", nil)
	if status != http.StatusOK {
		t.Fatalf("list versions: %d %v", status, body)
	}
	versions := listData(t, body)
	if len(versions) != 1 {
		t.Fatalf("versions = %v, want exactly the minted one", versions)
	}
	v := versions[0]
	wantFields(t, v, "id", "created_at", "description", "directory", "name", "skill_id", "type", "version")
	if vid, _ := v["id"].(string); !strings.HasPrefix(vid, "skillver_") {
		t.Errorf("version id = %v, want skillver_ prefix", v["id"])
	}
	if v["type"] != "skill_version" || v["skill_id"] != id || v["version"] != lv ||
		v["name"] != "financial-skill" || v["directory"] != "financial-skill" ||
		v["description"] != "Reads and explains financial statements." {
		t.Errorf("version object = %v", v)
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
	version, _ := obj["latest_version"].(string)

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

func TestSkillCreateExplicitDisplayTitleAndUniqueness(t *testing.T) {
	s := newTestServer(t)
	title := "Quarterly Reports"
	ct, body := skillForm(t, &title, []upFile{{name: "financial-skill/SKILL.md", content: testSkillMD}})
	status, obj := s.doForm("POST", "/v1/skills", ct, body)
	if status != http.StatusOK || obj["display_title"] != title {
		t.Fatalf("create with display_title: %d %v", status, obj)
	}
	// Same title again: unique among the workspace's custom skills.
	ct2, body2 := skillForm(t, &title, []upFile{{name: "financial-skill/SKILL.md", content: testSkillMD}})
	status, obj = s.doForm("POST", "/v1/skills", ct2, body2)
	wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")
	// A rejected create leaves no orphaned archive behind.
	if n := s.blobs.Len(); n != 1 {
		t.Errorf("stored objects = %d, want only the first skill's archive", n)
	}
}

func TestSkillUploadRejects(t *testing.T) {
	s := newTestServer(t)
	empty := ""
	cases := map[string]struct {
		displayTitle *string
		files        []upFile
	}{
		"NoFiles":           {nil, nil},
		"FlatBasename":      {nil, []upFile{{name: "SKILL.md", content: testSkillMD}}},
		"MissingSkillMD":    {nil, []upFile{{name: "dir/notes.md", content: "x"}}},
		"BadName":           {nil, []upFile{{name: "Financial/SKILL.md", content: "---\nname: Financial\ndescription: d\n---\n"}}},
		"DirMismatch":       {nil, []upFile{{name: "other/SKILL.md", content: testSkillMD}}},
		"EmptyDisplayTitle": {&empty, []upFile{{name: "financial-skill/SKILL.md", content: testSkillMD}}},
		"BadZip":            {nil, []upFile{{name: "s.zip", content: "PK\x03\x04 corrupt"}}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ct, body := skillForm(t, tc.displayTitle, tc.files)
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
		status, obj := s.do("POST", "/v1/skills", map[string]any{"display_title": "x"})
		wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")
	})
	t.Run("UnknownFormField", func(t *testing.T) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		_ = w.WriteField("nope", "x")
		_ = w.Close()
		status, obj := s.doForm("POST", "/v1/skills", w.FormDataContentType(), buf.String())
		wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")
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
	t.Run("DisplayTitleWithNUL", func(t *testing.T) {
		title := "bad\x00title"
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
	for _, k := range []string{"id", "display_title", "latest_version", "source", "type"} {
		if obj[k] != created[k] {
			t.Errorf("get %s = %v, create returned %v", k, obj[k], created[k])
		}
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

	status, body = s.do("GET", "/v1/skills?source=bogus", nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	status, body = s.do("GET", "/v1/skills?limit=101", nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
}

func TestSkillVersionCreateAndLatestTracking(t *testing.T) {
	s := newTestServer(t)
	created := s.createSkill(t)
	id, _ := created["id"].(string)
	v1, _ := created["latest_version"].(string)

	ct, body := skillForm(t, nil, []upFile{
		{name: "financial-skill/SKILL.md", content: testSkillMD},
		{name: "financial-skill/v2.txt", content: "second"},
	})
	status, obj := s.doForm("POST", "/v1/skills/"+id+"/versions", ct, body)
	if status != http.StatusOK {
		t.Fatalf("create version: %d %v", status, obj)
	}
	v2, _ := obj["version"].(string)
	if v2 == "" || v2 == v1 {
		t.Fatalf("second version = %q (first %q)", v2, v1)
	}
	if obj["skill_id"] != id || obj["type"] != "skill_version" {
		t.Errorf("version object = %v", obj)
	}

	// The skill's latest_version follows, and updated_at moves.
	status, skill := s.do("GET", "/v1/skills/"+id, nil)
	if status != http.StatusOK || skill["latest_version"] != v2 {
		t.Errorf("skill after version create = %v, want latest_version %q", skill, v2)
	}

	// display_title is a create-form field only.
	title := "nope"
	ct, body = skillForm(t, &title, []upFile{{name: "financial-skill/SKILL.md", content: testSkillMD}})
	status, obj = s.doForm("POST", "/v1/skills/"+id+"/versions", ct, body)
	wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")

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
	if status, skill := s.do("GET", "/v1/skills/xlsx", nil); status != http.StatusOK || skill["latest_version"] != "20250929" {
		t.Errorf("anthropic skill after refused deletes = %d %v", status, skill)
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

	// The versions list accepts limit up to 1000, unlike the skills list's 100.
	status, body := s.do("GET", "/v1/skills/"+id+"/versions?limit=1000", nil)
	if status != http.StatusOK {
		t.Fatalf("limit=1000: %d %v", status, body)
	}
	status, body = s.do("GET", "/v1/skills/"+id+"/versions?limit=1001", nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")

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
	version, _ := created["latest_version"].(string)

	status, obj := s.do("GET", "/v1/skills/"+id+"/versions/"+version, nil)
	if status != http.StatusOK || obj["version"] != version || obj["skill_id"] != id {
		t.Fatalf("get version: %d %v", status, obj)
	}

	// The {version} slot takes the timestamp string only: aliases are rejected.
	status, obj = s.do("GET", "/v1/skills/"+id+"/versions/latest", nil)
	wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")
	status, obj = s.do("GET", "/v1/skills/"+id+"/versions/999", nil)
	wantErr(t, status, obj, http.StatusNotFound, "not_found_error")
}

func TestSkillVersionDeleteRecomputesLatest(t *testing.T) {
	s := newTestServer(t)
	created := s.createSkill(t)
	id, _ := created["id"].(string)
	v1, _ := created["latest_version"].(string)
	ct, form := skillForm(t, nil, []upFile{{name: "financial-skill/SKILL.md", content: testSkillMD}})
	st, obj := s.doForm("POST", "/v1/skills/"+id+"/versions", ct, form)
	if st != http.StatusOK {
		t.Fatalf("second version: %d %v", st, obj)
	}
	v2, _ := obj["version"].(string)

	// Deleting the newest version rolls latest_version back; the response
	// echoes the version timestamp as its id (the wire's asymmetry).
	status, del := s.do("DELETE", "/v1/skills/"+id+"/versions/"+v2, nil)
	if status != http.StatusOK || del["id"] != v2 || del["type"] != "skill_version_deleted" {
		t.Fatalf("delete version: %d %v", status, del)
	}
	status, skill := s.do("GET", "/v1/skills/"+id, nil)
	if status != http.StatusOK || skill["latest_version"] != v1 {
		t.Errorf("after deleting v2, skill = %v, want latest_version %q", skill, v1)
	}
	// Its archive left object storage with it.
	if n := s.blobs.Len(); n != 1 {
		t.Errorf("stored objects = %d, want 1 after deleting one of two versions", n)
	}

	status, del = s.do("DELETE", "/v1/skills/"+id+"/versions/"+v2, nil)
	wantErr(t, status, del, http.StatusNotFound, "not_found_error")

	// Deleting the last version leaves the skill with no latest_version.
	if status, del = s.do("DELETE", "/v1/skills/"+id+"/versions/"+v1, nil); status != http.StatusOK {
		t.Fatalf("delete last version: %d %v", status, del)
	}
	status, skill = s.do("GET", "/v1/skills/"+id, nil)
	if status != http.StatusOK || skill["latest_version"] != "" {
		t.Errorf("after deleting every version, skill = %v, want empty latest_version", skill)
	}
}

func TestSkillDeleteRequiresVersionsGone(t *testing.T) {
	s := newTestServer(t)
	created := s.createSkill(t)
	id, _ := created["id"].(string)
	version, _ := created["latest_version"].(string)

	status, obj := s.do("DELETE", "/v1/skills/"+id, nil)
	wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")

	if status, obj = s.do("DELETE", "/v1/skills/"+id+"/versions/"+version, nil); status != http.StatusOK {
		t.Fatalf("delete version: %d %v", status, obj)
	}
	status, obj = s.do("DELETE", "/v1/skills/"+id, nil)
	if status != http.StatusOK || obj["id"] != id || obj["type"] != "skill_deleted" {
		t.Fatalf("delete skill: %d %v", status, obj)
	}
	status, obj = s.do("GET", "/v1/skills/"+id, nil)
	wantErr(t, status, obj, http.StatusNotFound, "not_found_error")
	status, obj = s.do("DELETE", "/v1/skills/"+id, nil)
	wantErr(t, status, obj, http.StatusNotFound, "not_found_error")
}

func TestSkillDownloadErrors(t *testing.T) {
	s := newTestServer(t)
	created := s.createSkill(t)
	id, _ := created["id"].(string)
	version, _ := created["latest_version"].(string)

	status, obj := s.do("GET", "/v1/skills/"+id+"/versions/999/content", nil)
	wantErr(t, status, obj, http.StatusNotFound, "not_found_error")

	// A version row whose archive vanished from object storage is an operator
	// incident, not a 404: the resource exists.
	if err := s.blobs.Delete(t.Context(), "skills/"+id+"/"+version+".zip"); err != nil {
		t.Fatalf("delete object: %v", err)
	}
	status, obj = s.do("GET", "/v1/skills/"+id+"/versions/"+version+"/content", nil)
	wantErr(t, status, obj, http.StatusInternalServerError, "api_error")
}

func TestSkillsUnavailableWithoutObjectStorage(t *testing.T) {
	// A deployment without object storage keeps serving everything else;
	// the skills upload/download surface reports its absence cleanly.
	pool := newPoolWithKey(t)
	srv := httptest.NewServer(api.NewHandler(pool, nil))
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
	okSrv := httptest.NewServer(api.NewHandler(pool, working))
	t.Cleanup(okSrv.Close)
	badSrv := httptest.NewServer(api.NewHandler(pool, failingStore{working}))
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
	v1, _ := created["latest_version"].(string)
	ct, body = skillForm(t, nil, []upFile{{name: "financial-skill/SKILL.md", content: testSkillMD}})
	status, obj = bad.doForm("POST", "/v1/skills/"+id+"/versions", ct, body)
	wantErr(t, status, obj, http.StatusInternalServerError, "api_error")
	status, versions := ok.do("GET", "/v1/skills/"+id+"/versions", nil)
	if status != http.StatusOK || len(listData(t, versions)) != 1 {
		t.Errorf("after failed version create: %d %v, want only the original version", status, versions)
	}
	if _, skill := ok.do("GET", "/v1/skills/"+id, nil); skill["latest_version"] != v1 {
		t.Errorf("latest_version = %v, want unchanged %q", skill["latest_version"], v1)
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
