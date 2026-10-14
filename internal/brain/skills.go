package brain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"path"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/skills"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// MetricSkillResolveMisses counts skill references the brain could not resolve
// to name/description at Level-1 injection time — a dangling id or version
// (surfaced late-bound, plan design decision 7) or a transient store error.
// Exported so the telemetry contract test can assert the exact name.
const MetricSkillResolveMisses = "skills.resolve.misses"

// skillRef is the minimal shape of one resolved-agent skills[] entry — the
// normalized {type, skill_id, version} the API stores, of which injection needs
// only the id and version.
type skillRef struct {
	SkillID string `json:"skill_id"`
	Version string `json:"version"`
}

// skillMeta is one resolved skill's Level-1 metadata: the name and description
// of the resolved version, plus the sandbox directory the executor/worker
// materialized it into (skills.TargetDir), so the injected path matches where
// the files actually land.
type skillMeta struct {
	Name        string
	Description string
	Dir         string
}

// resolveSkillsBlock resolves the agent's skills[] into the Level-1 system-prompt
// block: per skill, resolve the version at request-assembly time (see
// resolveSkillMeta for the three addressing forms) and read name+description
// from the resolved version, then render the block under the session's skills
// root (skillsRoot), with the repository skills after them. A reference
// that cannot be resolved — a dangling id/version, or a store blip — is logged
// and skipped, never fatal to the turn (the same late-bound tolerance
// materialization applies, plan design decision 7). Returns the rendered block,
// the number of attached skills injected, and the number of misses.
func (b *Brain) resolveSkillsBlock(ctx context.Context, agent domain.ResolvedAgent, envKind string, repo []repoSkill) (string, int, int) {
	if len(agent.Skills) == 0 {
		return renderSkillsBlock(nil, "", repo), 0, 0
	}
	var metas []skillMeta
	misses := 0
	seen := map[string]bool{}
	for _, raw := range agent.Skills {
		var ref skillRef
		if err := json.Unmarshal(raw, &ref); err != nil {
			slog.WarnContext(ctx, "skill reference not injected", "err", err)
			misses++
			continue
		}
		if ref.SkillID == "" {
			// A reference with no id cannot resolve — like a malformed one, it is
			// a logged miss, not a silent drop (the API rejects it, so this is
			// defensive; the counted-miss invariant still holds here).
			slog.WarnContext(ctx, "skill reference not injected", "version", ref.Version)
			misses++
			continue
		}
		if seen[ref.SkillID] {
			continue
		}
		seen[ref.SkillID] = true
		name, desc, err := b.resolveSkillMeta(ctx, ref.SkillID, ref.Version)
		if err != nil {
			slog.WarnContext(ctx, "skill reference not injected",
				"skill_id", ref.SkillID, "version", ref.Version, "err", err)
			misses++
			continue
		}
		metas = append(metas, skillMeta{
			Name: name, Description: desc, Dir: skills.TargetDir(name, ref.SkillID),
		})
	}
	return renderSkillsBlock(metas, b.skillsRoot(envKind), repo), len(metas), misses
}

// skillsRoot is the directory the block says holds the skills. On a cloud
// session it is absolute, the executor's workdir (Config.Workdir) joined with
// "skills", as the reference names /workspace/skills/. A self-hosted worker's
// workdir is the customer's (WORKER_WORKDIR), so no value here can name it:
// that root stays relative. The file tools resolve it against the workdir, and
// so does bash until the agent changes directory (plan 65 decision 1).
func (b *Brain) skillsRoot(envKind string) string {
	if envKind != string(domain.EnvCloud) {
		return "skills"
	}
	workdir := b.cfg.Workdir
	if workdir == "" {
		workdir = sandbox.DefaultWorkdir
	}
	return path.Join(workdir, "skills")
}

// resolveSkillMeta resolves one reference to the resolved version's name and
// description. Version addressing mirrors the executor's, in three explicit
// forms (plan 39 decision 5): the literal "latest" resolves against the skill's
// latest_version column at request-assembly time; a version id — the GA skver_
// spelling or the legacy skillver_ one — names that row directly, scoped to the
// skill so another skill's version id does not resolve; and an all-digits
// string is the legacy numeric pin, taken verbatim. Anything else addresses no
// version and is a miss. Asking only "is it digits?" is what made a pinned id
// resolve silently to the newest version instead of the pinned one.
//
// The id arm asks for a well-formed id, not merely a prefixed one: nothing
// validates a stored pin's shape on the way in, so skver_ followed by an
// unstorable byte would otherwise bind into the query and fail in the driver
// (SQLSTATE 22021) where the miss below was intended.
func (b *Brain) resolveSkillMeta(ctx context.Context, skillID, version string) (string, string, error) {
	var row pgx.Row
	switch skills.ClassifyPin(version) {
	case skills.PinLatest:
		var latest *string
		err := b.pool.QueryRow(ctx, `SELECT latest_version FROM skills WHERE id = $1`, skillID).Scan(&latest)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", fmt.Errorf("skill %s not found", skillID)
		}
		if err != nil {
			return "", "", err
		}
		if latest == nil {
			return "", "", fmt.Errorf("skill %s has no version to resolve %q against", skillID, version)
		}
		row = b.pool.QueryRow(ctx,
			`SELECT name, description FROM skill_versions WHERE skill_id = $1 AND version = $2`,
			skillID, *latest)
	case skills.PinID:
		row = b.pool.QueryRow(ctx,
			`SELECT name, description FROM skill_versions WHERE skill_id = $1 AND id = $2`,
			skillID, version)
	case skills.PinNumber:
		row = b.pool.QueryRow(ctx,
			`SELECT name, description FROM skill_versions WHERE skill_id = $1 AND version = $2`,
			skillID, version)
	default:
		return "", "", fmt.Errorf("skill %s version %q is neither %q, a version id nor a version number",
			skillID, version, skills.LatestAlias)
	}
	var name, description string
	err := row.Scan(&name, &description)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", fmt.Errorf("skill %s version %s not found", skillID, version)
	}
	if err != nil {
		return "", "", err
	}
	return name, description, nil
}

// skillsLead opens the skills block: what a skill is, and the order the agent
// works in when one applies.
const skillsLead = "**Using skills**\n\n" +
	"A skill is a set of instructions, and often code, for one kind of task. " +
	"Before you write code or run a tool for a task:\n" +
	"- check the task against each skill's description below;\n" +
	"- when one matches, read that skill's SKILL.md first;\n" +
	"- then follow its instructions and use its code, rather than writing your own from scratch.\n\n" +
	"<system-reminder>Read the matching SKILL.md before you act. " +
	"A skill's instructions and code take precedence over a generic approach.</system-reminder>\n"

// renderSkillsBlock formats the resolved skills into the Level-1 block appended
// to the system prompt, in the template a 2026-09-03 recording captured (#581):
// a lead, then an <available_skills> list with one <skill> per skill, carrying
// its name, its description and the location of its SKILL.md under root. The
// list and the sentence naming the root are the reference's. The lead gives the
// recorded lead's directions and its reminder in our own words, not its text.
// A name and a description are escaped as html.EscapeString does, which writes
// `"` as &#34;, as the recording shows. Paths are written as they are: the
// model copies them into its tools, and an escaped path names no file. They
// hold only the root, which is operator configuration, and a materialization
// directory, which upload validation keeps to [a-z0-9-] or a skill id.
//
// The skills a mounted repository holds follow in their own list,
// <available_skills source="repo">, as the reference was recorded announcing
// them (plan 65 decision 4). Everything in that list is the repository's, so
// its locations are escaped too: a directory name that needed it names a path
// the model cannot copy, which is the cost of keeping the list's shape. With no
// attached skill the lead precedes the repository list alone, without the
// sentence naming the root (ours: the reference's text there is unrecorded).
// Empty input yields an empty block, so nothing is appended.
func renderSkillsBlock(metas []skillMeta, root string, repo []repoSkill) string {
	if len(metas) == 0 && len(repo) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(skillsLead)
	if len(metas) > 0 {
		b.WriteString("- Skills are available under `" + root + "/`.\n\n")
		b.WriteString("<available_skills>\n")
		for _, m := range metas {
			writeSkill(&b, m.Name, m.Description, path.Join(root, m.Dir, "SKILL.md"))
		}
		b.WriteString("</available_skills>")
	}
	if len(repo) > 0 {
		if len(metas) > 0 {
			b.WriteString("\n")
		}
		b.WriteString("\n<available_skills source=\"repo\">\n")
		for _, s := range repo {
			writeSkill(&b, s.Name, s.Description, html.EscapeString(s.Location))
		}
		b.WriteString("</available_skills>")
	}
	return b.String()
}

// writeSkill writes one <skill> entry, escaping its name and description and
// writing location as given.
func writeSkill(b *strings.Builder, name, description, location string) {
	b.WriteString("<skill>\n  <name>")
	b.WriteString(html.EscapeString(name))
	b.WriteString("</name>\n  <description>")
	b.WriteString(html.EscapeString(description))
	b.WriteString("</description>\n  <location>")
	b.WriteString(location)
	b.WriteString("</location>\n</skill>\n")
}

// recordResolveMisses adds to the resolve-miss counter. Like the brain's other
// metrics it resolves the meter per call and never fails the turn: a telemetry
// error just drops the reading.
func recordResolveMisses(ctx context.Context, n int) {
	if n <= 0 {
		return
	}
	c, err := otel.GetMeterProvider().Meter(meterName).Int64Counter(
		MetricSkillResolveMisses,
		metric.WithDescription("Skill references the brain could not resolve for Level-1 injection."))
	if err != nil {
		return
	}
	c.Add(ctx, int64(n))
}
