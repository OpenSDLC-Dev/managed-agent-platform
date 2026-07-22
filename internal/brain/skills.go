package brain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
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

var skillDigitsRe = regexp.MustCompile(`^[0-9]+$`)

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
// block: per skill, resolve the version at request-assembly time (a digit string
// verbatim, else "latest" against the registry's latest_version) and read
// name+description from the resolved version, then render the block. A reference
// that cannot be resolved — a dangling id/version, or a store blip — is logged
// and skipped, never fatal to the turn (the same late-bound tolerance
// materialization applies, plan design decision 7). Returns the rendered block,
// the number of skills injected, and the number of misses.
func (b *Brain) resolveSkillsBlock(ctx context.Context, agent domain.ResolvedAgent) (string, int, int) {
	if len(agent.Skills) == 0 {
		return "", 0, 0
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
	return renderSkillsBlock(metas), len(metas), misses
}

// resolveSkillMeta resolves one reference to the resolved version's name and
// description. Version resolution mirrors the executor: a digit string is
// already concrete; anything else ("latest") resolves against the skill's
// latest_version column.
func (b *Brain) resolveSkillMeta(ctx context.Context, skillID, version string) (string, string, error) {
	if !skillDigitsRe.MatchString(version) {
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
		version = *latest
	}
	var name, description string
	err := b.pool.QueryRow(ctx,
		`SELECT name, description FROM skill_versions WHERE skill_id = $1 AND version = $2`,
		skillID, version).Scan(&name, &description)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", fmt.Errorf("skill %s version %s not found", skillID, version)
	}
	if err != nil {
		return "", "", err
	}
	return name, description, nil
}

// renderSkillsBlock formats the resolved skills into the Level-1 block appended
// to the system prompt. The exact reference template is captured by no source;
// this format — a short lead line plus one "name - description (path)" bullet
// per skill, the path being where materialization lands the files — is an
// inference recorded in docs/DIVERGENCES.md. Empty input yields an empty block,
// so nothing is appended.
func renderSkillsBlock(metas []skillMeta) string {
	if len(metas) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Available skills. Each skill's full instructions live in its SKILL.md file; read it with your file tools before using the skill.\n")
	for _, m := range metas {
		b.WriteString("\n- ")
		b.WriteString(m.Name)
		if m.Description != "" {
			b.WriteString(" - ")
			b.WriteString(m.Description)
		}
		b.WriteString(" (skills/")
		b.WriteString(m.Dir)
		b.WriteString("/SKILL.md)")
	}
	return b.String()
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
