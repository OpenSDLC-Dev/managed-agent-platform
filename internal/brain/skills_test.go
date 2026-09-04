package brain

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

func TestRenderSkillsBlock(t *testing.T) {
	if got := renderSkillsBlock(nil); got != "" {
		t.Errorf("empty set = %q, want empty", got)
	}
	one := renderSkillsBlock([]skillMeta{{Name: "alpha", Description: "Alpha skill", Dir: "alpha"}})
	if !strings.Contains(one, "- alpha - Alpha skill (skills/alpha/SKILL.md)") {
		t.Errorf("one-skill block missing the bullet: %q", one)
	}
	if !strings.HasPrefix(one, "Available skills.") {
		t.Errorf("block missing lead line: %q", one)
	}
	// A skill with no description drops the " - description" tail but keeps the path.
	noDesc := renderSkillsBlock([]skillMeta{{Name: "beta", Description: "", Dir: "beta"}})
	if !strings.Contains(noDesc, "- beta (skills/beta/SKILL.md)") || strings.Contains(noDesc, "beta - ") {
		t.Errorf("no-description block wrong: %q", noDesc)
	}
	multi := renderSkillsBlock([]skillMeta{
		{Name: "a", Description: "one", Dir: "a"},
		{Name: "b", Description: "two", Dir: "b"},
	})
	if strings.Count(multi, "\n- ") != 2 {
		t.Errorf("multi block should have two bullets: %q", multi)
	}
}

// seedSkill inserts a skill and one version row directly. Called again for the
// same skill it adds a version and moves latest_version onto it, the way the
// registry does — a skill with several versions is what version addressing is
// about.
func seedSkill(t *testing.T, b *Brain, id, version, name, description string) {
	t.Helper()
	ctx := context.Background()
	if _, err := b.pool.Exec(ctx,
		`INSERT INTO skills (id, source, display_title, latest_version) VALUES ($1, 'custom', $2, $3)
		 ON CONFLICT (id) DO UPDATE SET latest_version = EXCLUDED.latest_version`,
		id, name, version); err != nil {
		t.Fatalf("seed skill %s: %v", id, err)
	}
	if _, err := b.pool.Exec(ctx,
		`INSERT INTO skill_versions (id, skill_id, version, name, description, directory)
		 VALUES ('skillver_'||md5($1||$2), $1, $2, $3, $4, $3)`,
		id, version, name, description); err != nil {
		t.Fatalf("seed version %s@%s: %v", id, version, err)
	}
}

func ref(t *testing.T, id, version string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"type": "skill", "skill_id": id, "version": version})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestResolveSkillsBlock(t *testing.T) {
	pool := pgtest.NewPool(t)
	b := &Brain{pool: pool}
	ctx := context.Background()

	if block, injected, misses := b.resolveSkillsBlock(ctx, domain.ResolvedAgent{}); block != "" || injected != 0 || misses != 0 {
		t.Errorf("no skills = %q,%d,%d", block, injected, misses)
	}

	seedSkill(t, b, "skill_a", "100", "alpha", "Alpha skill")
	seedSkill(t, b, "skill_b", "200", "beta", "") // empty description; resolved via latest

	agent := domain.ResolvedAgent{AgentSpec: domain.AgentSpec{Skills: []json.RawMessage{
		ref(t, "skill_a", "100"),       // pinned digit version
		ref(t, "skill_a", "100"),       // duplicate id -> deduped, not double-injected
		ref(t, "skill_b", "latest"),    // alias resolved against latest_version
		ref(t, "skill_gone", "latest"), // dangling -> miss
		ref(t, "", "100"),              // empty id -> miss
		json.RawMessage(`not json`),    // malformed -> miss
	}}}

	block, injected, misses := b.resolveSkillsBlock(ctx, agent)
	if injected != 2 {
		t.Errorf("injected = %d, want 2", injected)
	}
	if misses != 3 {
		t.Errorf("misses = %d, want 3 (dangling + empty id + malformed)", misses)
	}
	if !strings.Contains(block, "alpha - Alpha skill (skills/alpha/SKILL.md)") {
		t.Errorf("block missing skill_a: %q", block)
	}
	// skill_b resolved through latest_version; empty description omits the tail.
	if !strings.Contains(block, "- beta (skills/beta/SKILL.md)") {
		t.Errorf("block missing skill_b via latest: %q", block)
	}
	if strings.Count(block, "\n- ") != 2 {
		t.Errorf("block should carry exactly the two resolved skills: %q", block)
	}

	// A skill whose every version was deleted (latest_version NULL) is a miss,
	// not a panic.
	if _, err := pool.Exec(ctx, `UPDATE skills SET latest_version = NULL WHERE id = 'skill_b'`); err != nil {
		t.Fatal(err)
	}
	_, injected, misses = b.resolveSkillsBlock(ctx, domain.ResolvedAgent{AgentSpec: domain.AgentSpec{
		Skills: []json.RawMessage{ref(t, "skill_b", "latest")},
	}})
	if injected != 0 || misses != 1 {
		t.Errorf("versionless latest = injected %d misses %d, want 0/1", injected, misses)
	}
}

// setVersionID spells out a seeded version row's id. seedSkill derives one from
// a hash, which no test can name, and the whole point of the three-way resolver
// is that a client addresses a version BY that id.
func setVersionID(t *testing.T, b *Brain, skillID, version, versionID string) {
	t.Helper()
	if _, err := b.pool.Exec(context.Background(),
		`UPDATE skill_versions SET id = $3 WHERE skill_id = $1 AND version = $2`,
		skillID, version, versionID); err != nil {
		t.Fatalf("set version id %s: %v", versionID, err)
	}
}

// TestResolveSkillMetaAddressingForms pins the three-way resolution the GA
// shape needs (plan 39 decision 5): the literal "latest", a version id in
// either the minted skver_ or the legacy skillver_ spelling, and the legacy
// numeric pin. Everything else is a miss.
//
// It is a mutation test for the silent-wrong-answer bug. The resolver this
// replaces asked one question — is the version all digits? — so every id form
// below fell through to the "latest" branch and was served the NEWEST version
// under the name of the pinned one. Each id row here names a version that is
// deliberately not the latest, so the old resolver answers "third" where the
// client asked for "first", and the unresolvable rows answer "third" where the
// client asked for something that does not exist at all.
func TestResolveSkillMetaAddressingForms(t *testing.T) {
	pool := pgtest.NewPool(t)
	b := &Brain{pool: pool}
	ctx := context.Background()

	// Three versions of one skill; latest_version lands on 300. Descriptions,
	// not names, carry the version identity: the reference requires the
	// frontmatter name to be identical across a skill's versions.
	seedSkill(t, b, "skill_pin", "100", "pinned", "first")
	setVersionID(t, b, "skill_pin", "100", "skver_pin100")
	seedSkill(t, b, "skill_pin", "200", "pinned", "second")
	setVersionID(t, b, "skill_pin", "200", "skillver_pin200")
	seedSkill(t, b, "skill_pin", "300", "pinned", "third")
	setVersionID(t, b, "skill_pin", "300", "skver_pin300")
	// A second skill, so a version id belonging to another skill can be shown
	// not to resolve — the reference refuses one (plan 39 §2, recording 105).
	seedSkill(t, b, "skill_other", "100", "other", "elsewhere")
	setVersionID(t, b, "skill_other", "100", "skver_oth100")

	cases := []struct {
		name, version, wantDesc string
		wantErr                 bool
	}{
		{name: "latest alias", version: "latest", wantDesc: "third"},
		{name: "minted skver_ id", version: "skver_pin100", wantDesc: "first"},
		{name: "legacy skillver_ id", version: "skillver_pin200", wantDesc: "second"},
		{name: "id naming the latest version", version: "skver_pin300", wantDesc: "third"},
		{name: "legacy numeric pin", version: "100", wantDesc: "first"},
		{name: "another skill's version id", version: "skver_oth100", wantErr: true},
		{name: "unknown version id", version: "skver_nosuchversion", wantErr: true},
		{name: "unknown numeric pin", version: "999", wantErr: true},
		{name: "neither alias, id nor digits", version: "bogus", wantErr: true},
		{name: "empty version", version: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, desc, err := b.resolveSkillMeta(ctx, "skill_pin", tc.version)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("version %q resolved to %q/%q, want an unresolved miss",
						tc.version, name, desc)
				}
				return
			}
			if err != nil {
				t.Fatalf("version %q: %v", tc.version, err)
			}
			if name != "pinned" {
				t.Errorf("name = %q, want %q", name, "pinned")
			}
			if desc != tc.wantDesc {
				t.Errorf("version %q resolved to description %q, want %q",
					tc.version, desc, tc.wantDesc)
			}
		})
	}
}

// TestResolveSkillsBlockInjectsIDPinnedVersion is the same fix seen from the
// caller: an agent pinning a version by its id gets THAT version's metadata in
// the Level-1 block, and an unresolvable pin is one counted miss rather than a
// quietly substituted newest version.
func TestResolveSkillsBlockInjectsIDPinnedVersion(t *testing.T) {
	pool := pgtest.NewPool(t)
	b := &Brain{pool: pool}
	ctx := context.Background()

	seedSkill(t, b, "skill_inj", "100", "injected", "the pinned one")
	setVersionID(t, b, "skill_inj", "100", "skver_inj100")
	seedSkill(t, b, "skill_inj", "200", "injected", "the newest one")
	setVersionID(t, b, "skill_inj", "200", "skver_inj200")

	block, injected, misses := b.resolveSkillsBlock(ctx, domain.ResolvedAgent{
		AgentSpec: domain.AgentSpec{Skills: []json.RawMessage{ref(t, "skill_inj", "skver_inj100")}},
	})
	if injected != 1 || misses != 0 {
		t.Fatalf("id pin = injected %d misses %d, want 1/0", injected, misses)
	}
	if !strings.Contains(block, "the pinned one") {
		t.Errorf("block = %q, want the pinned version's description", block)
	}
	if strings.Contains(block, "the newest one") {
		t.Errorf("an id pin was served the newest version: %q", block)
	}

	_, injected, misses = b.resolveSkillsBlock(ctx, domain.ResolvedAgent{
		AgentSpec: domain.AgentSpec{Skills: []json.RawMessage{ref(t, "skill_inj", "skver_dangling")}},
	})
	if injected != 0 || misses != 1 {
		t.Errorf("dangling id pin = injected %d misses %d, want 0/1", injected, misses)
	}
}
