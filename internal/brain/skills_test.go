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

// seedSkill inserts a skill and one version row directly.
func seedSkill(t *testing.T, b *Brain, id, version, name, description string) {
	t.Helper()
	ctx := context.Background()
	if _, err := b.pool.Exec(ctx,
		`INSERT INTO skills (id, source, display_title, latest_version) VALUES ($1, 'custom', $2, $3)`,
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
		json.RawMessage(`not json`),    // malformed -> miss
	}}}

	block, injected, misses := b.resolveSkillsBlock(ctx, agent)
	if injected != 2 {
		t.Errorf("injected = %d, want 2", injected)
	}
	if misses != 2 {
		t.Errorf("misses = %d, want 2 (dangling + malformed)", misses)
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
