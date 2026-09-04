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

// skillVersionAlias is the one alias the wire admits for a skill version: the
// newest one at use time. Everything else a stored pin can hold is concrete —
// a version id, or the legacy numeric.
const skillVersionAlias = "latest"

var skillDigitsRe = regexp.MustCompile(`^[0-9]+$`)

// materializeSkills lands the session agent's skills under {workdir}/skills/
// in the provisioned sandbox — the reference worker's SetupSkills semantics
// at the platform-managed deployment point: versions resolved at use time
// (see resolveSkillVersion for the three addressing forms), archives
// read from object storage, extraction under the reference guards, and
// per-skill failure logged and skipped, never fatal to the tool run. A
// sentinel file records the resolved set so re-entrant provisioning of a
// live sandbox skips rewriting unchanged skills. refs come from the same
// locked session read that gated the run (sessionForRun) — the reference's
// one hard failure, the session read, faults there, so nothing here does.
func (e *Executor) materializeSkills(ctx context.Context, sb sandbox.Sandbox, sid domain.ID, refs []skillRef, progress func()) {
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
		// Resolution is per-reference work too — two round trips each, and the
		// dangling ones leave by the continue below without ever reaching the
		// write loop. A set of them is a run that is moving (#383).
		progress()
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
		name, sha, err := e.skillVersionMeta(ctx, ref.SkillID, version)
		if err != nil {
			e.skipSkill(ctx, sid, ref.SkillID, version, err)
			misses++
			continue
		}
		resolved = append(resolved, skills.Resolved{
			ID: ref.SkillID, Version: version, Dir: skills.TargetDir(name, ref.SkillID),
			SHA256: sha,
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
	// The probe is one sandbox read per recorded tree — a session may reference
	// hundreds — and the skip returns without ever entering the write loop, so
	// the reads report as they land rather than the whole probe counting as one
	// silent step (#383).
	probeRead := func(ctx context.Context, p string) ([]byte, error) {
		progress()
		return sb.ReadFile(ctx, p)
	}
	if misses == 0 {
		if prev, err := sb.ReadFile(ctx, sentinelPath); err == nil &&
			skills.SentinelMatches(ctx, probeRead, workdir, prev, resolved) {
			span.SetAttributes(attribute.Bool("skills.unchanged", true))
			return
		}
	}
	progress()

	var landed []skills.Resolved
	for _, r := range resolved {
		// Per skill, at the top of the iteration — the files.go rule, applied
		// here so a set of large archives reports as it goes rather than once at
		// the end (#383).
		progress()
		if err := e.materializeSkill(ctx, sb, workdir, r); err != nil {
			e.skipSkill(ctx, sid, r.ID, r.Version, err)
			continue
		}
		landed = append(landed, r)
		recordSkillMaterialized(ctx, skillOutcomeOK)
		slog.InfoContext(ctx, "skill materialized", "session_id", sid, "skill_id", r.ID, "version", r.Version)
	}
	// The pass boundary: the report at the top of the loop covers every item but
	// the last one, whose landing would otherwise share a silent interval with
	// the sentinel write behind it — a 500 MB mount and a slow sandbox write are
	// each well inside the budget and together need not be (#383).
	progress()
	span.SetAttributes(attribute.Int("skills.materialized", len(landed)))
	// The sentinel records only what landed: a partial pass re-runs next time.
	if err := sb.WriteFile(ctx, sentinelPath, skills.Sentinel(landed)); err != nil {
		slog.WarnContext(ctx, "skills sentinel not written", "session_id", sid, "err", err)
	}
}

// skipSkill is the per-skill tolerance path: log, count, continue. A digest
// mismatch takes the same path — one corrupt archive must not fail every
// session that references it — under its own outcome, so integrity failures
// are alertable apart from ordinary dangling references.
func (e *Executor) skipSkill(ctx context.Context, sid domain.ID, skillID, version string, err error) {
	outcome := skillOutcomeFailed
	switch {
	case errors.Is(err, errSkillNotFound):
		outcome = skillOutcomeNotFound
	case errors.Is(err, skills.ErrDigestMismatch):
		outcome = skillOutcomeCorrupt
	}
	recordSkillMaterialized(ctx, outcome)
	slog.WarnContext(ctx, "skill not materialized",
		"session_id", sid, "skill_id", skillID, "version", version, "err", err)
}

// resolveSkillVersion resolves one reference to the concrete numeric version
// that names the archive in object storage. A stored pin takes three forms
// (plan 39 decision 5): the literal "latest" — the snapshot keeps the alias
// verbatim — resolves against the registry's latest_version at use time, the
// reference's late-binding semantics; a version id, in the GA skver_ spelling
// or the legacy skillver_ one, is translated to its row's numeric version,
// scoped to the skill so another skill's version id does not resolve; and an
// all-digits version is already concrete. Anything else addresses no version
// and is a dangling reference, skipped like any other. Asking only "is it
// digits?" is what made a pinned id resolve silently to the newest version and
// materialize the wrong archive.
//
// The numeric stays the internal identity (decision 3), so the blob key, the
// sentinel and skills.BlobKey are untouched by id addressing.
func (e *Executor) resolveSkillVersion(ctx context.Context, ref skillRef) (string, error) {
	switch {
	case ref.Version == skillVersionAlias:
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
	case domain.ID(ref.Version).HasPrefix(domain.PrefixSkillVersion):
		var version string
		err := e.pool.QueryRow(ctx,
			`SELECT version FROM skill_versions WHERE skill_id = $1 AND id = $2`,
			ref.SkillID, ref.Version).Scan(&version)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("%w: version id %s", errSkillNotFound, ref.Version)
		}
		if err != nil {
			return "", err
		}
		return version, nil
	case skillDigitsRe.MatchString(ref.Version):
		return ref.Version, nil
	default:
		return "", fmt.Errorf("%w: version %q is neither %q, a version id nor a version number",
			errSkillNotFound, ref.Version, skillVersionAlias)
	}
}

// skillVersionMeta reads a version row's materialization name and the archive
// digest recorded at upload, both from trusted storage. It is also the
// existence check for an already-concrete (all-digits) version, which
// resolveSkillVersion does not verify: a missing row is a dangling reference,
// surfaced as errSkillNotFound and skipped. sha256 is empty for a row written
// before migration 0010 — nothing to verify against, not a failure.
func (e *Executor) skillVersionMeta(ctx context.Context, skillID, version string) (name, sha256 string, err error) {
	var stored *string
	err = e.pool.QueryRow(ctx,
		`SELECT name, sha256 FROM skill_versions WHERE skill_id = $1 AND version = $2`,
		skillID, version).Scan(&name, &stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", fmt.Errorf("%w: version %s", errSkillNotFound, version)
	}
	if err != nil {
		return "", "", err
	}
	if stored != nil {
		sha256 = *stored
	}
	return name, sha256, nil
}

// materializeSkill extracts one already-resolved skill version's archive into
// the sandbox at {workdir}/skills/{r.Dir}/. The archive is read under a byte
// cap and verified against the digest the version row recorded at upload
// (skills.ReadArchive), so neither a corrupt or oversized stored object can OOM
// the executor nor a bit-rotted or substituted one reach the sandbox.
func (e *Executor) materializeSkill(ctx context.Context, sb sandbox.Sandbox, workdir string, r skills.Resolved) error {
	rc, _, err := e.blobs.Get(ctx, skills.BlobKey(r.ID, r.Version))
	if errors.Is(err, blob.ErrNotFound) {
		return fmt.Errorf("%w: archive missing from object storage", errSkillNotFound)
	}
	if err != nil {
		return err
	}
	if r.SHA256 == "" {
		slog.WarnContext(ctx, "skill version records no archive digest; extracting unverified",
			"skill_id", r.ID, "version", r.Version)
	}
	data, err := skills.ReadArchive(rc, r.SHA256)
	rc.Close()
	if err != nil {
		return err
	}
	files, err := skills.Extract(data)
	if err != nil {
		return err
	}
	root := path.Join(workdir, "skills", r.Dir)
	batch := make([]sandbox.FileWrite, len(files))
	for i, f := range files {
		batch[i] = sandbox.FileWrite{Path: path.Join(root, f.Path), Data: f.Data}
	}
	// One call for the whole tree: written a file at a time, a skill costs one
	// sandbox exec per member — about 14ms each, and up to 10,000 members
	// (#206) — where the batch costs a fixed couple for all of them. The failure semantics
	// are the same either way (the first failure stops the run and what landed
	// stays), so this skill is skipped and re-materialized on the next pass.
	return sb.WriteFiles(ctx, batch)
}
