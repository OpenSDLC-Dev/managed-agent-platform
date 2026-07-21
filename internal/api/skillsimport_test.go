package api_test

import (
	"net/http"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
)

// The fixture skills under testdata/skillsimport are self-authored for this
// repository (Apache-2.0) — never copies of anthropics/skills content, whose
// document skills are source-available and must not be vendored (the plan's
// license red lines).
const importFixtures = "testdata/skillsimport"

func importDirs(names ...string) []string {
	dirs := make([]string, len(names))
	for i, n := range names {
		dirs[i] = importFixtures + "/" + n
	}
	return dirs
}

func TestImportAnthropicSkills(t *testing.T) {
	s := newTestServer(t)
	ctx := t.Context()

	sum, err := api.ImportAnthropicSkills(ctx, s.pool, s.blobs, importDirs("alpha-notes", "beta-notes"), "20260101")
	if err != nil {
		t.Fatalf("import: %v (summary %+v)", err, sum)
	}
	if len(sum.Imported) != 2 || len(sum.Skipped) != 0 || len(sum.Failed) != 0 {
		t.Fatalf("summary = %+v, want 2 imported", sum)
	}

	// The imported rows serve through the wire API as anthropic-source skills
	// with short-name ids and the date version.
	status, body := s.do("GET", "/v1/skills?source=anthropic", nil)
	if status != http.StatusOK {
		t.Fatalf("list: %d %v", status, body)
	}
	byID := map[string]map[string]any{}
	for _, e := range listData(t, body) {
		byID[e["id"].(string)] = e
	}
	alpha := byID["alpha-notes"]
	if len(byID) != 2 || alpha == nil || byID["beta-notes"] == nil {
		t.Fatalf("imported ids = %v", byID)
	}
	if alpha["source"] != "anthropic" || alpha["latest_version"] != "20260101" ||
		alpha["display_title"] != "alpha-notes" {
		t.Errorf("alpha-notes = %v", alpha)
	}

	// The version object carries the frontmatter extraction, and its archive
	// downloads like any uploaded skill's.
	status, v := s.do("GET", "/v1/skills/alpha-notes/versions/20260101", nil)
	if status != http.StatusOK || v["name"] != "alpha-notes" || v["directory"] != "alpha-notes" {
		t.Fatalf("version get: %d %v", status, v)
	}
	res := s.doRaw("GET", "/v1/skills/alpha-notes/versions/20260101/content", nil,
		map[string]string{"x-api-key": testKey})
	res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/zip" {
		t.Errorf("download: %d %q", res.StatusCode, res.Header.Get("Content-Type"))
	}

	// Idempotent per version: a rerun imports nothing and stores nothing new.
	objects := s.blobs.Len()
	sum, err = api.ImportAnthropicSkills(ctx, s.pool, s.blobs, importDirs("alpha-notes", "beta-notes"), "20260101")
	if err != nil || len(sum.Imported) != 0 || len(sum.Skipped) != 2 {
		t.Fatalf("rerun: %v %+v, want 2 skipped", err, sum)
	}
	if s.blobs.Len() != objects {
		t.Errorf("rerun changed stored objects: %d -> %d", objects, s.blobs.Len())
	}

	// A newer date imports as a second version and latest_version follows.
	sum, err = api.ImportAnthropicSkills(ctx, s.pool, s.blobs, importDirs("alpha-notes"), "20260102")
	if err != nil || len(sum.Imported) != 1 {
		t.Fatalf("newer version: %v %+v", err, sum)
	}
	status, skill := s.do("GET", "/v1/skills/alpha-notes", nil)
	if status != http.StatusOK || skill["latest_version"] != "20260102" {
		t.Errorf("after newer import: %v", skill)
	}
	status, versions := s.do("GET", "/v1/skills/alpha-notes/versions", nil)
	if status != http.StatusOK || len(listData(t, versions)) != 2 {
		t.Errorf("versions after both imports: %v", versions)
	}

	// Backfilling an older date lands the version but never regresses
	// latest_version — "latest" stays the numerically newest, the same rule
	// the reference worker applies client-side over the versions list.
	sum, err = api.ImportAnthropicSkills(ctx, s.pool, s.blobs, importDirs("alpha-notes"), "20251231")
	if err != nil || len(sum.Imported) != 1 {
		t.Fatalf("older backfill: %v %+v", err, sum)
	}
	status, skill = s.do("GET", "/v1/skills/alpha-notes", nil)
	if status != http.StatusOK || skill["latest_version"] != "20260102" {
		t.Errorf("older backfill regressed latest_version: %v", skill)
	}
}

func TestImportAnthropicSkillsFailures(t *testing.T) {
	s := newTestServer(t)
	ctx := t.Context()

	// One broken directory fails; the healthy one still lands; the run errors.
	sum, err := api.ImportAnthropicSkills(ctx, s.pool, s.blobs, importDirs("broken-skill", "alpha-notes"), "20260101")
	if err == nil {
		t.Error("import with a broken dir reported no error")
	}
	if len(sum.Imported) != 1 || len(sum.Failed) != 1 {
		t.Fatalf("summary = %+v, want 1 imported + 1 failed", sum)
	}
	if status, skill := s.do("GET", "/v1/skills/alpha-notes", nil); status != http.StatusOK {
		t.Errorf("healthy dir did not import: %d %v", status, skill)
	}

	// A malformed version string is rejected up front.
	if _, err := api.ImportAnthropicSkills(ctx, s.pool, s.blobs, importDirs("alpha-notes"), "latest"); err == nil {
		t.Error("accepted a non-digit version")
	}
	// A missing directory is a per-dir failure, not a panic.
	sum, err = api.ImportAnthropicSkills(ctx, s.pool, s.blobs, importDirs("no-such-dir"), "20260103")
	if err == nil || len(sum.Failed) != 1 {
		t.Errorf("missing dir: err=%v summary=%+v", err, sum)
	}
	// No object storage: refused outright.
	if _, err := api.ImportAnthropicSkills(ctx, s.pool, nil, importDirs("alpha-notes"), "20260104"); err == nil {
		t.Error("accepted a nil blob store")
	}

	// A row already holding the id with a different source is refused — the
	// defensive guard for state the API itself can never mint (custom ids are
	// skill_-prefixed), planted here directly.
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO skills (id, source, display_title) VALUES ('beta-notes', 'custom', 'squatter')`); err != nil {
		t.Fatalf("plant conflicting row: %v", err)
	}
	sum, err = api.ImportAnthropicSkills(ctx, s.pool, s.blobs, importDirs("beta-notes"), "20260105")
	if err == nil || len(sum.Failed) != 1 {
		t.Errorf("import over a custom-source id: err=%v summary=%+v, want a per-dir failure", err, sum)
	}
}
