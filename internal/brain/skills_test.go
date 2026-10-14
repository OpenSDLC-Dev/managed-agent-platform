package brain

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestRenderSkillsBlock(t *testing.T) {
	if got := renderSkillsBlock(nil, "/workspace/skills"); got != "" {
		t.Errorf("empty set = %q, want empty", got)
	}
	// The whole block, so the template is pinned rather than sampled (#581):
	// the lead, the root, then one <skill> per skill in order. The description
	// is escaped as recorded (`"` is &#34;), an empty one keeps its tags, and a
	// location follows the materialization directory, not the name.
	got := renderSkillsBlock([]skillMeta{
		{Name: "alpha", Description: `Use for "alpha" <tasks> & more, it's fine`, Dir: "alpha"},
		{Name: "beta", Description: "", Dir: "beta-skill_b"},
	}, "/workspace/skills")
	want := "**Using skills**\n\n" +
		"A skill is a set of instructions, and often code, for one kind of task. " +
		"Before you write code or run a tool for a task:\n" +
		"- check the task against each skill's description below;\n" +
		"- when one matches, read that skill's SKILL.md first;\n" +
		"- then follow its instructions and use its code, rather than writing your own from scratch.\n\n" +
		"<system-reminder>Read the matching SKILL.md before you act. " +
		"A skill's instructions and code take precedence over a generic approach.</system-reminder>\n" +
		"- Skills are available under `/workspace/skills/`.\n\n" +
		"<available_skills>\n" +
		"<skill>\n  <name>alpha</name>\n" +
		"  <description>Use for &#34;alpha&#34; &lt;tasks&gt; &amp; more, it&#39;s fine</description>\n" +
		"  <location>/workspace/skills/alpha/SKILL.md</location>\n</skill>\n" +
		"<skill>\n  <name>beta</name>\n  <description></description>\n" +
		"  <location>/workspace/skills/beta-skill_b/SKILL.md</location>\n</skill>\n" +
		"</available_skills>"
	if got != want {
		t.Errorf("block =\n%s\nwant\n%s", got, want)
	}

	// A self-hosted root is relative, in the sentence and in every location.
	rel := renderSkillsBlock([]skillMeta{{Name: "alpha", Description: "a", Dir: "alpha"}}, "skills")
	if !strings.Contains(rel, "available under `skills/`.") ||
		!strings.Contains(rel, "<location>skills/alpha/SKILL.md</location>") {
		t.Errorf("relative-root block = %q", rel)
	}

	// Paths are written as they are, so the model can copy them into its tools:
	// an escaped root would name a directory that does not exist.
	odd := renderSkillsBlock([]skillMeta{{Name: "alpha", Description: "a", Dir: "alpha"}}, "/home/o'brien&co/skills")
	if !strings.Contains(odd, "available under `/home/o'brien&co/skills/`.") ||
		!strings.Contains(odd, "<location>/home/o'brien&co/skills/alpha/SKILL.md</location>") {
		t.Errorf("a root with characters an escaper touches is not verbatim: %q", odd)
	}

	// A name is data, escaped like the description. Upload validation keeps it
	// to [a-z0-9-], so this guards the block's shape, not a reachable name.
	if named := renderSkillsBlock([]skillMeta{{Name: "a<b", Dir: "x"}}, "skills"); !strings.Contains(named, "<name>a&lt;b</name>") {
		t.Errorf("a name is not escaped: %q", named)
	}
}

// TestSkillsRoot pins where the block says the skills are: under the
// configured executor workdir on a cloud session, the default when none is
// configured, and relative on a self-hosted one, whose workdir the brain cannot
// know.
func TestSkillsRoot(t *testing.T) {
	for _, c := range []struct {
		workdir, envKind, want string
	}{
		{"", "cloud", "/workspace/skills"},
		{"/srv/agent", "cloud", "/srv/agent/skills"},
		{"/srv/agent/", "cloud", "/srv/agent/skills"},
		{"/srv/agent", "self_hosted", "skills"},
		{"", "self_hosted", "skills"},
	} {
		b := &Brain{cfg: Config{Workdir: c.workdir}}
		if got := b.skillsRoot(c.envKind); got != c.want {
			t.Errorf("skillsRoot(workdir %q, %s) = %q, want %q", c.workdir, c.envKind, got, c.want)
		}
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

	if block, injected, misses := b.resolveSkillsBlock(ctx, domain.ResolvedAgent{}, "cloud"); block != "" || injected != 0 || misses != 0 {
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

	block, injected, misses := b.resolveSkillsBlock(ctx, agent, "cloud")
	if injected != 2 {
		t.Errorf("injected = %d, want 2", injected)
	}
	if misses != 3 {
		t.Errorf("misses = %d, want 3 (dangling + empty id + malformed)", misses)
	}
	if !strings.Contains(block, "<name>alpha</name>\n  <description>Alpha skill</description>\n"+
		"  <location>/workspace/skills/alpha/SKILL.md</location>") {
		t.Errorf("block missing skill_a: %q", block)
	}
	// skill_b resolved through latest_version; its empty description keeps the tags.
	if !strings.Contains(block, "<name>beta</name>\n  <description></description>\n"+
		"  <location>/workspace/skills/beta/SKILL.md</location>") {
		t.Errorf("block missing skill_b via latest: %q", block)
	}
	if strings.Count(block, "<skill>") != 2 {
		t.Errorf("block should carry exactly the two resolved skills: %q", block)
	}

	// A skill whose every version was deleted (latest_version NULL) is a miss,
	// not a panic.
	if _, err := pool.Exec(ctx, `UPDATE skills SET latest_version = NULL WHERE id = 'skill_b'`); err != nil {
		t.Fatal(err)
	}
	_, injected, misses = b.resolveSkillsBlock(ctx, domain.ResolvedAgent{AgentSpec: domain.AgentSpec{
		Skills: []json.RawMessage{ref(t, "skill_b", "latest")},
	}}, "cloud")
	if injected != 0 || misses != 1 {
		t.Errorf("versionless latest = injected %d misses %d, want 0/1", injected, misses)
	}
}

// setVersionID spells out a seeded version row's id. seedSkill derives one from
// a hash, which no test can name, and the whole point of the three-way resolver
// is that a client addresses a version BY that id.
// setVersionID plants an id on a seeded version row. The tokens are drawn from
// the alphabet NewID emits, because the resolver now asks for a well-formed id
// rather than a merely prefixed one.
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
	setVersionID(t, b, "skill_pin", "100", "skver_pn100")
	seedSkill(t, b, "skill_pin", "200", "pinned", "second")
	setVersionID(t, b, "skill_pin", "200", "skillver_pn200")
	seedSkill(t, b, "skill_pin", "300", "pinned", "third")
	setVersionID(t, b, "skill_pin", "300", "skver_pn300")
	// A second skill, so a version id belonging to another skill can be shown
	// not to resolve — the reference refuses one (plan 39 §2, recording 105).
	seedSkill(t, b, "skill_other", "100", "other", "elsewhere")
	setVersionID(t, b, "skill_other", "100", "skver_extra100")

	cases := []struct {
		name, version, wantDesc string
		wantErr                 bool
	}{
		{name: "latest alias", version: "latest", wantDesc: "third"},
		{name: "minted skver_ id", version: "skver_pn100", wantDesc: "first"},
		{name: "legacy skillver_ id", version: "skillver_pn200", wantDesc: "second"},
		{name: "id naming the latest version", version: "skver_pn300", wantDesc: "third"},
		{name: "legacy numeric pin", version: "100", wantDesc: "first"},
		{name: "another skill's version id", version: "skver_extra100", wantErr: true},
		{name: "unknown version id", version: "skver_absent", wantErr: true},
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

// TestResolveSkillMetaRejectsMalformedIDBeforeItBinds: a stored pin is whatever
// the agent's skills[] carried — parseSkills checks no shape on `version` — so
// a value that merely starts skver_ can hold bytes no id ever has. Rejecting it
// on shape is what keeps it out of a bind parameter, where an unstorable byte
// answers SQLSTATE 22021 (a driver failure, reported as a store blip) in place
// of the miss a pin naming no version was always meant to be.
func TestResolveSkillMetaRejectsMalformedIDBeforeItBinds(t *testing.T) {
	pool := pgtest.NewPool(t)
	b := &Brain{pool: pool}
	seedSkill(t, b, "skill_malformed", "100", "malformed", "first")

	_, _, err := b.resolveSkillMeta(context.Background(), "skill_malformed", "skver_\x00")
	if err == nil {
		t.Fatal("a NUL-bearing version id resolved")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		t.Fatalf("the malformed pin reached the database: %v", err)
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
	setVersionID(t, b, "skill_inj", "100", "skver_ver100")
	seedSkill(t, b, "skill_inj", "200", "injected", "the newest one")
	setVersionID(t, b, "skill_inj", "200", "skver_ver200")

	block, injected, misses := b.resolveSkillsBlock(ctx, domain.ResolvedAgent{
		AgentSpec: domain.AgentSpec{Skills: []json.RawMessage{ref(t, "skill_inj", "skver_ver100")}},
	}, "cloud")
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
		AgentSpec: domain.AgentSpec{Skills: []json.RawMessage{ref(t, "skill_inj", "skver_absent")}},
	}, "cloud")
	if injected != 0 || misses != 1 {
		t.Errorf("dangling id pin = injected %d misses %d, want 0/1", injected, misses)
	}
}
