package executor

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/skills"
)

// skillArchive builds an archive shaped like the one the registry stores: every
// file under the skill's single top-level directory.
func skillArchive(t *testing.T, name string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for p, content := range files {
		fw, err := w.Create(name + "/" + p)
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// seedSkill plants a registry skill the way the API does: rows — including the
// archive digest recorded at upload — plus the archive in object storage.
func (h *harness) seedSkill(t *testing.T, id, version, name string, files map[string]string) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO skills (id, source, display_title, latest_version) VALUES ($1, 'custom', $1, $2)
		 ON CONFLICT (id) DO UPDATE SET latest_version = $2`, id, version); err != nil {
		t.Fatalf("seed skill: %v", err)
	}
	archive := skillArchive(t, name, files)
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO skill_versions (id, skill_id, version, name, description, directory, sha256)
		 VALUES ('skillver_'||md5($1||$2), $1, $2, $3, 'test skill', $3, $4)`,
		id, version, name, skills.Digest(archive)); err != nil {
		t.Fatalf("seed skill version: %v", err)
	}
	if err := h.blobs.Put(ctx, skills.BlobKey(id, version),
		bytes.NewReader(archive), int64(len(archive)), "application/zip"); err != nil {
		t.Fatalf("seed archive: %v", err)
	}
}

// swapArchive replaces a version's stored object while leaving its row — and so
// its recorded digest — untouched: the storage-layer substitution the digest
// exists to catch, and one zip's own per-member CRC cannot see.
func (h *harness) swapArchive(t *testing.T, id, version, name string, files map[string]string) {
	t.Helper()
	archive := skillArchive(t, name, files)
	if err := h.blobs.Put(context.Background(), skills.BlobKey(id, version),
		bytes.NewReader(archive), int64(len(archive)), "application/zip"); err != nil {
		t.Fatalf("swap archive: %v", err)
	}
}

// refSkills points the session snapshot's skills[] at the given
// {skill_id, version} references, the normalized shape the API stores.
func (h *harness) refSkills(t *testing.T, refs ...[2]string) {
	t.Helper()
	entries := make([]map[string]string, len(refs))
	for i, r := range refs {
		entries[i] = map[string]string{"type": "custom", "skill_id": r[0], "version": r[1]}
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET resolved_agent = jsonb_set(resolved_agent, '{skills}', $2::jsonb) WHERE id = $1`,
		h.sid.String(), raw); err != nil {
		t.Fatalf("set session skills: %v", err)
	}
}

func TestMaterializesSkills(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedSkill(t, "skill_mat_one", "100", "research-notes", map[string]string{
		"SKILL.md":       "# research",
		"scripts/run.sh": "echo hi",
	})
	h.refSkills(t, [2]string{"skill_mat_one", "latest"})
	h.suspend(t, writeUse("out.txt", "hello"))

	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}

	// The archive landed under {workdir}/skills/{name}/ — reachable by bash
	// as a relative path — and "latest" resolved to the registry's latest.
	if got := sb.files["/workspace/skills/research-notes/SKILL.md"]; got != "# research" {
		t.Errorf("SKILL.md = %q", got)
	}
	if got := sb.files["/workspace/skills/research-notes/scripts/run.sh"]; got != "echo hi" {
		t.Errorf("nested file = %q", got)
	}
	// The tool itself still ran.
	if sb.files["/workspace/out.txt"] != "hello" {
		t.Errorf("tool write = %q", sb.files["/workspace/out.txt"])
	}
	// The sentinel records the resolved concrete version.
	sentinel := sb.files["/workspace/skills/"+skills.SentinelName]
	if !strings.Contains(sentinel, `"skill_mat_one"`) || !strings.Contains(sentinel, `"100"`) {
		t.Errorf("sentinel = %q", sentinel)
	}

	// Re-entrant provisioning with an unchanged resolved set skips rewrites:
	// mutate a materialized file, run another item, and see it untouched.
	sb.files["/workspace/skills/research-notes/SKILL.md"] = "mutated"
	h.suspend(t, writeUse("out2.txt", "again"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("second step: %v", err)
	}
	if got := sb.files["/workspace/skills/research-notes/SKILL.md"]; got != "mutated" {
		t.Errorf("unchanged set was rewritten: %q", got)
	}

	// A newer version moves latest: the sentinel no longer matches, so the
	// next provisioning pass rewrites.
	h.seedSkill(t, "skill_mat_one", "200", "research-notes", map[string]string{
		"SKILL.md": "# v2",
	})
	h.suspend(t, writeUse("out3.txt", "thrice"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("third step: %v", err)
	}
	if got := sb.files["/workspace/skills/research-notes/SKILL.md"]; got != "# v2" {
		t.Errorf("latest bump not rematerialized: %q", got)
	}

	// The workdir is agent-writable: a tool call deleting a skill tree while
	// the marker survives must not be trusted — the next pass restores it.
	delete(sb.files, "/workspace/skills/research-notes/SKILL.md")
	h.suspend(t, writeUse("out4.txt", "fourth"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("fourth step: %v", err)
	}
	if got := sb.files["/workspace/skills/research-notes/SKILL.md"]; got != "# v2" {
		t.Errorf("deleted skill not restored: %q", got)
	}
}

func TestMaterializeToleratesPerSkillFailure(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedSkill(t, "skill_mat_good", "100", "good-skill", map[string]string{"SKILL.md": "ok"})
	// A dangling reference (create-time existence is deliberately not
	// validated) and a corrupt archive both skip, never fault the run.
	h.seedSkill(t, "skill_mat_bad", "100", "bad-skill", map[string]string{"SKILL.md": "x"})
	if err := h.blobs.Put(context.Background(), skills.BlobKey("skill_mat_bad", "100"),
		strings.NewReader("not a zip"), 9, "application/zip"); err != nil {
		t.Fatal(err)
	}
	h.refSkills(t,
		[2]string{"skill_mat_missing", "latest"},
		[2]string{"skill_mat_bad", "100"},
		[2]string{"skill_mat_good", "latest"},
	)
	h.suspend(t, writeUse("out.txt", "hello"))

	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}
	if got := sb.files["/workspace/skills/good-skill/SKILL.md"]; got != "ok" {
		t.Errorf("good skill = %q", got)
	}
	if sb.files["/workspace/out.txt"] != "hello" {
		t.Error("per-skill failures blocked the tool run")
	}
	// The sentinel records only what landed, so the next pass retries.
	sentinel := sb.files["/workspace/skills/"+skills.SentinelName]
	if strings.Contains(sentinel, "skill_mat_bad") || strings.Contains(sentinel, "skill_mat_missing") {
		t.Errorf("sentinel recorded a failed skill: %q", sentinel)
	}
	// The tool result on the log is the model's, unpolluted by skill skips.
	if results := h.types(t, "agent.tool_result"); len(results) != 1 {
		t.Errorf("results = %d, want 1", len(results))
	}
}

func TestMaterializeRefusesSubstitutedArchive(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedSkill(t, "skill_mat_swap", "100", "swapped-skill", map[string]string{
		"SKILL.md": "genuine instructions",
	})
	h.seedSkill(t, "skill_mat_kept", "100", "kept-skill", map[string]string{"SKILL.md": "ok"})
	// The stored object is replaced by a different but perfectly valid archive
	// — bit-rot, truncation and substitution all present here as bytes that
	// extract cleanly and are simply not what was uploaded.
	h.swapArchive(t, "skill_mat_swap", "100", "swapped-skill", map[string]string{
		"SKILL.md": "tampered instructions",
	})
	h.refSkills(t, [2]string{"skill_mat_swap", "100"}, [2]string{"skill_mat_kept", "100"})
	h.suspend(t, writeUse("out.txt", "hello"))

	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}
	if got, ok := sb.files["/workspace/skills/swapped-skill/SKILL.md"]; ok {
		t.Errorf("substituted archive reached the sandbox: %q", got)
	}
	// A failed digest is the same per-skill tolerance as any other miss: the
	// healthy skill and the tool call both proceed.
	if got := sb.files["/workspace/skills/kept-skill/SKILL.md"]; got != "ok" {
		t.Errorf("healthy skill = %q", got)
	}
	if sb.files["/workspace/out.txt"] != "hello" {
		t.Error("a corrupt archive blocked the tool run")
	}
	// Nothing landed for it, so the sentinel must not claim it — the next pass
	// retries, and a repaired object materializes then.
	if sentinel := sb.files["/workspace/skills/"+skills.SentinelName]; strings.Contains(sentinel, "skill_mat_swap") {
		t.Errorf("sentinel recorded a refused skill: %q", sentinel)
	}
}

func TestMaterializeToleratesVersionWithoutDigest(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedSkill(t, "skill_mat_legacy", "100", "legacy-skill", map[string]string{"SKILL.md": "v1"})
	// A row written before the sha256 column exists records no digest; there is
	// nothing to verify against, and that must not make the skill unusable.
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE skill_versions SET sha256 = NULL WHERE skill_id = $1`, "skill_mat_legacy"); err != nil {
		t.Fatal(err)
	}
	h.refSkills(t, [2]string{"skill_mat_legacy", "100"})
	h.suspend(t, writeUse("out.txt", "x"))

	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}
	if got := sb.files["/workspace/skills/legacy-skill/SKILL.md"]; got != "v1" {
		t.Errorf("SKILL.md = %q, want the archive extracted unverified", got)
	}
}

func TestMaterializeWithoutStorage(t *testing.T) {
	sb := &fakeSandbox{}
	prov := &fakeProvider{sb: sb}
	h := newHarnessWith(t, prov, Config{})
	h.exec = New(h.pool, h.log, h.queue, prov, nil, Config{})
	h.refSkills(t, [2]string{"skill_mat_any", "latest"})
	h.suspend(t, writeUse("out.txt", "hello"))

	// No object storage: materialization is skipped with a log line; the
	// tool run proceeds.
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}
	if sb.files["/workspace/out.txt"] != "hello" {
		t.Error("tool did not run without storage")
	}
	for p := range sb.files {
		if strings.Contains(p, "/skills/") {
			t.Errorf("unexpected skills write %q", p)
		}
	}
}

func TestMaterializePinnedVersion(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedSkill(t, "skill_mat_pin", "100", "pinned-skill", map[string]string{"SKILL.md": "v1"})
	h.seedSkill(t, "skill_mat_pin", "200", "pinned-skill", map[string]string{"SKILL.md": "v2"})
	h.refSkills(t, [2]string{"skill_mat_pin", "100"})
	h.suspend(t, writeUse("out.txt", "x"))

	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}
	if got := sb.files["/workspace/skills/pinned-skill/SKILL.md"]; got != "v1" {
		t.Errorf("pinned version = %q, want the pinned v1", got)
	}
}
