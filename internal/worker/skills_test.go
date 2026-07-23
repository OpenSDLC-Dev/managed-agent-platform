package worker

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

// seedSkill plants a registry skill server-side: rows — including the archive
// digest recorded at upload — plus the archive in the control plane's object
// store. The worker only ever sees it over the wire.
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
// the digest the download advertises — untouched: the storage-layer
// substitution the digest exists to catch.
func (h *harness) swapArchive(t *testing.T, id, version, name string, files map[string]string) {
	t.Helper()
	archive := skillArchive(t, name, files)
	if err := h.blobs.Put(context.Background(), skills.BlobKey(id, version),
		bytes.NewReader(archive), int64(len(archive)), "application/zip"); err != nil {
		t.Fatalf("swap archive: %v", err)
	}
}

// refSkills points the session snapshot's skills[] at the given
// {skill_id, version} references.
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

func TestSetupSkillsOverTheWire(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedSkill(t, "wire-one", "100", "wire-notes", map[string]string{
		"SKILL.md":  "# wire",
		"ref/a.txt": "aaa",
	})
	h.refSkills(t, [2]string{"wire-one", "latest"})
	h.suspend(t, writeUse("out.txt", "hello"))

	if err := h.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	// The skill landed through the environment-key wire path — session GET,
	// versions list (alias resolution), version get, /content download.
	if got := sb.files["/workspace/skills/wire-notes/SKILL.md"]; got != "# wire" {
		t.Errorf("SKILL.md = %q", got)
	}
	if got := sb.files["/workspace/skills/wire-notes/ref/a.txt"]; got != "aaa" {
		t.Errorf("nested = %q", got)
	}
	if sb.files["/workspace/out.txt"] != "hello" {
		t.Errorf("tool write = %q", sb.files["/workspace/out.txt"])
	}
	// "latest" resolved client-side to the newest numeric version.
	sentinel := sb.files["/workspace/skills/"+skills.SentinelName]
	if !strings.Contains(sentinel, `"100"`) {
		t.Errorf("sentinel = %q", sentinel)
	}

	// An unchanged resolved set skips rewrites on a reclaiming pass.
	sb.files["/workspace/skills/wire-notes/SKILL.md"] = "mutated"
	h.suspend(t, writeUse("out2.txt", "again"))
	if err := h.run(); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := sb.files["/workspace/skills/wire-notes/SKILL.md"]; got != "mutated" {
		t.Errorf("unchanged set was rewritten: %q", got)
	}

	// The workdir is agent-writable: a tool call deleting a skill tree while
	// the marker survives must not be trusted — the next pass restores it.
	delete(sb.files, "/workspace/skills/wire-notes/SKILL.md")
	h.suspend(t, writeUse("out3.txt", "thrice"))
	if err := h.run(); err != nil {
		t.Fatalf("third run: %v", err)
	}
	if got := sb.files["/workspace/skills/wire-notes/SKILL.md"]; got != "# wire" {
		t.Errorf("deleted skill not restored: %q", got)
	}
}

func TestSetupSkillsRefusesSubstitutedArchive(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedSkill(t, "wire-swap", "100", "swap-wire", map[string]string{"SKILL.md": "genuine"})
	h.seedSkill(t, "wire-kept", "100", "kept-wire", map[string]string{"SKILL.md": "ok"})
	// A different but perfectly valid archive replaces the stored object; the
	// version row — and so the digest the /content download advertises — is
	// untouched. The worker reads that header, never the database.
	h.swapArchive(t, "wire-swap", "100", "swap-wire", map[string]string{"SKILL.md": "tampered"})
	h.refSkills(t, [2]string{"wire-swap", "100"}, [2]string{"wire-kept", "100"})
	h.suspend(t, writeUse("out.txt", "hello"))

	if err := h.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, ok := sb.files["/workspace/skills/swap-wire/SKILL.md"]; ok {
		t.Errorf("substituted archive reached the sandbox: %q", got)
	}
	if got := sb.files["/workspace/skills/kept-wire/SKILL.md"]; got != "ok" {
		t.Errorf("healthy skill = %q", got)
	}
	if sb.files["/workspace/out.txt"] != "hello" {
		t.Error("a corrupt archive blocked the tool run")
	}
	if sentinel := sb.files["/workspace/skills/"+skills.SentinelName]; strings.Contains(sentinel, "wire-swap") {
		t.Errorf("sentinel recorded a refused skill: %q", sentinel)
	}
}

func TestSetupSkillsToleratesVersionWithoutDigest(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedSkill(t, "wire-legacy", "100", "legacy-wire", map[string]string{"SKILL.md": "v1"})
	// No recorded digest: the download advertises no header, so there is
	// nothing to verify against and the skill must still materialize.
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE skill_versions SET sha256 = NULL WHERE skill_id = $1`, "wire-legacy"); err != nil {
		t.Fatal(err)
	}
	h.refSkills(t, [2]string{"wire-legacy", "100"})
	h.suspend(t, writeUse("out.txt", "x"))

	if err := h.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := sb.files["/workspace/skills/legacy-wire/SKILL.md"]; got != "v1" {
		t.Errorf("SKILL.md = %q, want the archive extracted unverified", got)
	}
}

func TestSetupSkillsTolerance(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedSkill(t, "wire-good", "100", "good-wire", map[string]string{"SKILL.md": "ok"})
	h.refSkills(t,
		[2]string{"wire-gone", "latest"},
		[2]string{"wire-good", "100"},
	)
	h.suspend(t, writeUse("out.txt", "hello"))

	// A dangling reference (wire 404s) skips; the healthy skill and the tool
	// run both proceed.
	if err := h.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := sb.files["/workspace/skills/good-wire/SKILL.md"]; got != "ok" {
		t.Errorf("good skill = %q", got)
	}
	if sb.files["/workspace/out.txt"] != "hello" {
		t.Error("per-skill failure blocked the tool run")
	}
	sentinel := sb.files["/workspace/skills/"+skills.SentinelName]
	if strings.Contains(sentinel, "wire-gone") {
		t.Errorf("sentinel recorded a failed skill: %q", sentinel)
	}
}
