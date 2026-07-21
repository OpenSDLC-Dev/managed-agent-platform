package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/skills"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// skillRef is the minimal shape of one resolved-agent skills[] entry — the
// normalized {type, skill_id, version} the API stores.
type skillRef struct {
	SkillID string `json:"skill_id"`
	Version string `json:"version"`
}

// errSkillNotFound classifies a dangling reference: existence is deliberately
// not validated at agent create (docs/plan/06_skills.md design decision 7),
// so a missing skill or version surfaces here as a logged skip.
var errSkillNotFound = errors.New("skill not found")

var skillDigitsRe = regexp.MustCompile(`^[0-9]+$`)

// materializeSkills lands the session agent's skills under {workdir}/skills/
// in the provisioned sandbox — the reference worker's SetupSkills semantics
// at the platform-managed deployment point: versions resolved at use time
// (an alias like "latest" against the registry's latest_version), archives
// read from object storage, extraction under the reference guards, and
// per-skill failure logged and skipped, never fatal to the tool run. A
// sentinel file records the resolved set so re-entrant provisioning of a
// live sandbox skips rewriting unchanged skills. refs come from the same
// locked session read that gated the run (sessionForRun) — the reference's
// one hard failure, the session read, faults there, so nothing here does.
func (e *Executor) materializeSkills(ctx context.Context, sb sandbox.Sandbox, sid domain.ID, refs []skillRef) {
	if len(refs) == 0 {
		return
	}
	if e.blobs == nil {
		slog.WarnContext(ctx, "session references skills but object storage is not configured",
			"session_id", sid, "skills", len(refs))
		return
	}

	ctx, span := otel.GetTracerProvider().Tracer(tracerName).Start(ctx, "skills_materialize")
	defer span.End()
	start := time.Now()
	defer func() { recordSkillsMaterializeDuration(ctx, time.Since(start)) }()

	// Resolve every reference first: the sentinel records a resolved set, so
	// the skip decision needs the whole picture before any write. Each entry's
	// target directory is derived from the version row's TRUSTED name here, so
	// the skip probe can never be redirected by an agent-rewritten marker.
	resolved := make([]skills.Resolved, 0, len(refs))
	seen := map[string]bool{}
	misses := 0
	for _, ref := range refs {
		if seen[ref.SkillID] {
			continue
		}
		seen[ref.SkillID] = true
		version, err := e.resolveSkillVersion(ctx, ref)
		if err != nil {
			e.skipSkill(ctx, sid, ref.SkillID, ref.Version, err)
			misses++
			continue
		}
		name, err := e.skillName(ctx, ref.SkillID, version)
		if err != nil {
			e.skipSkill(ctx, sid, ref.SkillID, version, err)
			misses++
			continue
		}
		resolved = append(resolved, skills.Resolved{
			ID: ref.SkillID, Version: version, Dir: skills.TargetDir(name, ref.SkillID),
		})
	}
	span.SetAttributes(attribute.Int("skills.referenced", len(refs)))

	workdir := e.cfg.Workdir
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
			return
		}
	}

	var landed []skills.Resolved
	for _, r := range resolved {
		if err := e.materializeSkill(ctx, sb, workdir, r); err != nil {
			e.skipSkill(ctx, sid, r.ID, r.Version, err)
			continue
		}
		landed = append(landed, r)
		recordSkillMaterialized(ctx, skillOutcomeOK)
		slog.InfoContext(ctx, "skill materialized", "session_id", sid, "skill_id", r.ID, "version", r.Version)
	}
	span.SetAttributes(attribute.Int("skills.materialized", len(landed)))
	// The sentinel records only what landed: a partial pass re-runs next time.
	if err := sb.WriteFile(ctx, sentinelPath, skills.Sentinel(landed)); err != nil {
		slog.WarnContext(ctx, "skills sentinel not written", "session_id", sid, "err", err)
	}
}

// skipSkill is the per-skill tolerance path: log, count, continue.
func (e *Executor) skipSkill(ctx context.Context, sid domain.ID, skillID, version string, err error) {
	outcome := skillOutcomeFailed
	if errors.Is(err, errSkillNotFound) {
		outcome = skillOutcomeNotFound
	}
	recordSkillMaterialized(ctx, outcome)
	slog.WarnContext(ctx, "skill not materialized",
		"session_id", sid, "skill_id", skillID, "version", version, "err", err)
}

// resolveSkillVersion resolves one reference to a concrete version: an
// all-digits version is already concrete; anything else ("latest" — the
// snapshot keeps the alias verbatim) resolves against the registry's
// latest_version at use time, the reference's late-binding semantics.
func (e *Executor) resolveSkillVersion(ctx context.Context, ref skillRef) (string, error) {
	if skillDigitsRe.MatchString(ref.Version) {
		return ref.Version, nil
	}
	var latest *string
	err := e.pool.QueryRow(ctx,
		`SELECT latest_version FROM skills WHERE id = $1`, ref.SkillID).Scan(&latest)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errSkillNotFound
	}
	if err != nil {
		return "", err
	}
	if latest == nil {
		return "", fmt.Errorf("%w: no versions to resolve %q against", errSkillNotFound, ref.Version)
	}
	return *latest, nil
}

// skillName reads a version row's materialization name from trusted storage.
// It is also the existence check for an already-concrete (all-digits) version,
// which resolveSkillVersion does not verify: a missing row is a dangling
// reference, surfaced as errSkillNotFound and skipped.
func (e *Executor) skillName(ctx context.Context, skillID, version string) (string, error) {
	var name string
	err := e.pool.QueryRow(ctx,
		`SELECT name FROM skill_versions WHERE skill_id = $1 AND version = $2`,
		skillID, version).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: version %s", errSkillNotFound, version)
	}
	if err != nil {
		return "", err
	}
	return name, nil
}

// materializeSkill extracts one already-resolved skill version's archive into
// the sandbox at {workdir}/skills/{r.Dir}/. The archive is read under a byte
// cap (skills.ReadArchive) so a corrupt or oversized stored object cannot OOM
// the executor.
func (e *Executor) materializeSkill(ctx context.Context, sb sandbox.Sandbox, workdir string, r skills.Resolved) error {
	rc, size, err := e.blobs.Get(ctx, skills.BlobKey(r.ID, r.Version))
	if errors.Is(err, blob.ErrNotFound) {
		return fmt.Errorf("%w: archive missing from object storage", errSkillNotFound)
	}
	if err != nil {
		return err
	}
	data, err := skills.ReadArchive(rc, size)
	rc.Close()
	if err != nil {
		return err
	}
	files, err := skills.Extract(data)
	if err != nil {
		return err
	}
	root := path.Join(workdir, "skills", r.Dir)
	for _, f := range files {
		if err := sb.WriteFile(ctx, path.Join(root, f.Path), f.Data); err != nil {
			return err
		}
	}
	return nil
}
