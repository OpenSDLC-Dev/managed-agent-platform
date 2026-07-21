package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"regexp"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/skills"
	sdk "github.com/anthropics/anthropic-sdk-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// skillRef is the minimal wire shape of one session-agent skills[] entry,
// decoded from the session's raw JSON for the same drift rationale as
// unansweredToolUses.
type skillRef struct {
	SkillID string `json:"skill_id"`
	Version string `json:"version"`
}

var skillDigitsRe = regexp.MustCompile(`^[0-9]+$`)

// SetupSkills materializes the session agent's skills into the sandbox — the
// BYOC twin of the executor's materialization and a re-expression of the
// reference worker's SetupSkills (anthropic-sdk-go tools/agenttoolset):
// session GET with the environment key, per skill an alias resolution over
// the versions list (newest numeric wins), a version GET for the name, the
// /content download, and extraction under the reference guards — all wire,
// no database, writing through sandbox.WriteFile instead of the host
// filesystem. Per-skill failure is logged and skipped, never fatal; only the
// session read fails the call, mirroring the reference. A sentinel under
// {workdir}/skills/ records the resolved set so a reclaiming pass over a
// live sandbox skips rewriting unchanged skills (the reference re-extracts
// every time, but its workdir is host-shared across sessions and cleaned per
// item; this sandbox is per-session, so skipping is safe and cheaper).
func SetupSkills(ctx context.Context, client sdk.Client, sessionID string, sb sandbox.Sandbox, workdir string) error {
	sess, err := client.Beta.Sessions.Get(ctx, sessionID, sdk.BetaSessionGetParams{})
	if err != nil {
		return fmt.Errorf("read session for skills: %w", err)
	}
	var snapshot struct {
		Agent struct {
			Skills []skillRef `json:"skills"`
		} `json:"agent"`
	}
	if err := json.Unmarshal([]byte(sess.RawJSON()), &snapshot); err != nil {
		return fmt.Errorf("parse session for skills: %w", err)
	}
	refs := snapshot.Agent.Skills
	if len(refs) == 0 {
		return nil
	}

	ctx, span := otel.GetTracerProvider().Tracer(tracerName).Start(ctx, "skills_materialize")
	defer span.End()
	start := time.Now()
	defer func() { recordSkillsMaterializeDuration(ctx, time.Since(start)) }()

	order := make([]string, 0, len(refs))
	resolved := map[string]string{}
	misses := 0
	for _, ref := range refs {
		if _, dup := resolved[ref.SkillID]; dup {
			continue
		}
		version, err := resolveSkillVersion(ctx, client, ref)
		if err != nil {
			skipSkill(ctx, sessionID, ref.SkillID, ref.Version, err)
			misses++
			continue
		}
		order = append(order, ref.SkillID)
		resolved[ref.SkillID] = version
	}
	span.SetAttributes(attribute.Int("skills.referenced", len(refs)))

	if workdir == "" {
		workdir = sandbox.DefaultWorkdir
	}
	// The skip needs the marker to match the resolved set AND the recorded
	// trees to still hold their SKILL.md — the workdir is agent-writable, so
	// a tool call may have deleted skills the marker still claims.
	sentinelPath := path.Join(workdir, "skills", skills.SentinelName)
	if misses == 0 {
		if prev, err := sb.ReadFile(ctx, sentinelPath); err == nil &&
			skills.SentinelMatches(ctx, sb.ReadFile, workdir, prev, resolved) {
			span.SetAttributes(attribute.Bool("skills.unchanged", true))
			return nil
		}
	}

	var landed []skills.SentinelEntry
	for _, id := range order {
		version := resolved[id]
		dir, err := materializeSkill(ctx, client, sb, workdir, id, version)
		if err != nil {
			skipSkill(ctx, sessionID, id, version, err)
			continue
		}
		landed = append(landed, skills.SentinelEntry{ID: id, Version: version, Dir: dir})
		recordSkillMaterialized(ctx, skillOutcomeOK)
		slog.InfoContext(ctx, "skill materialized", "session_id", sessionID, "skill_id", id, "version", version)
	}
	span.SetAttributes(attribute.Int("skills.materialized", len(landed)))
	if err := sb.WriteFile(ctx, sentinelPath, skills.Sentinel(landed)); err != nil {
		slog.WarnContext(ctx, "skills sentinel not written", "session_id", sessionID, "err", err)
	}
	return nil
}

// skipSkill is the per-skill tolerance path: log, count, continue. A wire
// 404 (missing skill, version, or archive) classifies as not_found — the
// late-bound surfacing of a dangling reference.
func skipSkill(ctx context.Context, sessionID, skillID, version string, err error) {
	outcome := skillOutcomeFailed
	var apierr *sdk.Error
	if errors.As(err, &apierr) && apierr.StatusCode == 404 {
		outcome = skillOutcomeNotFound
	}
	recordSkillMaterialized(ctx, outcome)
	slog.WarnContext(ctx, "skill not materialized",
		"session_id", sessionID, "skill_id", skillID, "version", version, "err", err)
}

// resolveSkillVersion resolves one reference the reference worker's way: an
// all-digits version is already concrete; anything else ("latest") lists the
// skill's versions and picks the newest numeric one client-side.
func resolveSkillVersion(ctx context.Context, client sdk.Client, ref skillRef) (string, error) {
	if skillDigitsRe.MatchString(ref.Version) {
		return ref.Version, nil
	}
	iter := client.Beta.Skills.Versions.ListAutoPaging(ctx, ref.SkillID, sdk.BetaSkillVersionListParams{})
	best := ""
	for iter.Next() {
		if v := iter.Current().Version; skillDigitsRe.MatchString(v) && numericGreater(v, best) {
			best = v
		}
	}
	if err := iter.Err(); err != nil {
		return "", err
	}
	if best == "" {
		return "", fmt.Errorf("skill %q has no concrete version to resolve %q against", ref.SkillID, ref.Version)
	}
	return best, nil
}

// numericGreater orders decimal version strings without overflow:
// length-then-lexical (the reference's rule — versions are epoch or date
// digit strings, so this equals numeric order).
func numericGreater(a, b string) bool {
	if len(a) != len(b) {
		return len(a) > len(b)
	}
	return a > b
}

// materializeSkill lands one skill version: version GET (for the directory
// name), download, extract, write. Returns the directory it landed in.
func materializeSkill(ctx context.Context, client sdk.Client, sb sandbox.Sandbox, workdir, skillID, version string) (string, error) {
	v, err := client.Beta.Skills.Versions.Get(ctx, version, sdk.BetaSkillVersionGetParams{SkillID: skillID})
	if err != nil {
		return "", err
	}
	resp, err := client.Beta.Skills.Versions.Download(ctx, version, sdk.BetaSkillVersionDownloadParams{SkillID: skillID})
	if err != nil {
		return "", err
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return "", err
	}
	files, err := skills.Extract(data)
	if err != nil {
		return "", err
	}
	dir := skills.TargetDir(v.Name, skillID)
	root := path.Join(workdir, "skills", dir)
	for _, f := range files {
		if err := sb.WriteFile(ctx, path.Join(root, f.Path), f.Data); err != nil {
			return "", err
		}
	}
	return dir, nil
}
