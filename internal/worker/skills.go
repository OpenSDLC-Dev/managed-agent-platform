package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// no database, writing through the sandbox file API instead of the host
// filesystem. Per-skill failure is logged and skipped, never fatal; only the
// session read fails the call, mirroring the reference. A sentinel under
// {workdir}/skills/ records the resolved set so a reclaiming pass over a
// live sandbox skips rewriting unchanged skills (the reference re-extracts
// every time, but its workdir is host-shared across sessions and cleaned per
// item; this sandbox is per-session, so skipping is safe and cheaper).
func SetupSkills(ctx context.Context, client sdk.Client, sessionID string, sb sandbox.Sandbox, workdir string, progress func()) error {
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

	// Resolve every reference to {version, trusted directory} before any write:
	// the sentinel records a resolved set, and each entry's directory comes from
	// the version object's TRUSTED name, so the skip probe can never be
	// redirected by an agent-rewritten marker.
	resolved := make([]skills.Resolved, 0, len(refs))
	seen := map[string]bool{}
	misses := 0
	for _, ref := range refs {
		// Resolution is per-reference work too — round trips each, and the
		// dangling ones leave by the continue below without ever reaching the
		// write loop. A set of them is a run that is moving (#383).
		progress()
		if seen[ref.SkillID] {
			continue
		}
		seen[ref.SkillID] = true
		r, err := resolveSkill(ctx, client, ref, progress)
		if err != nil {
			skipSkill(ctx, sessionID, ref.SkillID, ref.Version, err)
			misses++
			continue
		}
		resolved = append(resolved, r)
	}
	span.SetAttributes(attribute.Int("skills.referenced", len(refs)))

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
			return nil
		}
	}
	progress()

	var landed []skills.Resolved
	for _, r := range resolved {
		// Per skill, at the top of the iteration — the files.go rule, so a set
		// of large archives reports as it goes rather than once at the end
		// (#383).
		progress()
		if err := materializeSkill(ctx, client, sb, workdir, r); err != nil {
			skipSkill(ctx, sessionID, r.ID, r.Version, err)
			continue
		}
		landed = append(landed, r)
		recordSkillMaterialized(ctx, skillOutcomeOK)
		slog.InfoContext(ctx, "skill materialized", "session_id", sessionID, "skill_id", r.ID, "version", r.Version)
	}
	// The pass boundary: the report at the top of the loop covers every item but
	// the last one, whose landing would otherwise share a silent interval with
	// the sentinel write behind it — a 500 MB mount and a slow sandbox write are
	// each well inside the budget and together need not be (#383).
	progress()
	span.SetAttributes(attribute.Int("skills.materialized", len(landed)))
	if err := sb.WriteFile(ctx, sentinelPath, skills.Sentinel(landed)); err != nil {
		slog.WarnContext(ctx, "skills sentinel not written", "session_id", sessionID, "err", err)
	}
	return nil
}

// skipSkill is the per-skill tolerance path: log, count, continue. A wire
// 404 (missing skill, version, or archive) classifies as not_found — the
// late-bound surfacing of a dangling reference. A digest mismatch takes the
// same tolerant path — one corrupt archive must not fail every session that
// references it — under its own outcome, so integrity failures are alertable
// apart from ordinary dangling references.
func skipSkill(ctx context.Context, sessionID, skillID, version string, err error) {
	outcome := skillOutcomeFailed
	var apierr *sdk.Error
	switch {
	case errors.As(err, &apierr) && apierr.StatusCode == 404:
		outcome = skillOutcomeNotFound
	case errors.Is(err, skills.ErrDigestMismatch):
		outcome = skillOutcomeCorrupt
	}
	recordSkillMaterialized(ctx, outcome)
	slog.WarnContext(ctx, "skill not materialized",
		"session_id", sessionID, "skill_id", skillID, "version", version, "err", err)
}

// resolveSkillVersion resolves one reference the reference worker's way: an
// all-digits version is already concrete; anything else ("latest") lists the
// skill's versions and picks the newest numeric one client-side.
func resolveSkillVersion(ctx context.Context, client sdk.Client, ref skillRef, progress func()) (string, error) {
	if skillDigitsRe.MatchString(ref.Version) {
		return ref.Version, nil
	}
	iter := client.Beta.Skills.Versions.ListAutoPaging(ctx, ref.SkillID, sdk.BetaSkillVersionListParams{})
	best := ""
	for iter.Next() {
		// One reference can be many wire round trips: the alias is resolved by
		// walking every version, and the pager fetches a page per Next. Reporting
		// only around the whole resolution would put an unbounded number of them
		// inside one silent interval — the rule this change is built on, applied
		// to the one loop in this file that is not over items (#383).
		progress()
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

// resolveSkill resolves one reference to {version, trusted directory}: the
// concrete version (digits verbatim, or the newest for an alias) and a version
// GET for the name, from which the landing directory is derived. The name
// comes from the version object, never the sandbox, so it is safe to drive the
// skip probe with.
func resolveSkill(ctx context.Context, client sdk.Client, ref skillRef, progress func()) (skills.Resolved, error) {
	version, err := resolveSkillVersion(ctx, client, ref, progress)
	if err != nil {
		return skills.Resolved{}, err
	}
	// The version GET is a second round trip behind whatever the line above
	// cost, so it gets its own interval rather than sharing that one.
	progress()
	v, err := client.Beta.Skills.Versions.Get(ctx, version, sdk.BetaSkillVersionGetParams{SkillID: ref.SkillID})
	if err != nil {
		return skills.Resolved{}, err
	}
	return skills.Resolved{
		ID: ref.SkillID, Version: version, Dir: skills.TargetDir(v.Name, ref.SkillID),
	}, nil
}

// materializeSkill lands one already-resolved skill version: download, extract,
// write. The download is read under a byte cap and verified against the digest
// the response advertises (skills.ReadArchive), so neither a corrupt or
// oversized served archive can OOM the worker nor a bit-rotted or substituted
// one reach the sandbox. The digest arrives as a response header because the
// SDK's version object carries no checksum field; a control plane that sends
// none leaves the archive unverified rather than unusable.
func materializeSkill(ctx context.Context, client sdk.Client, sb sandbox.Sandbox, workdir string, r skills.Resolved) error {
	resp, err := client.Beta.Skills.Versions.Download(ctx, r.Version, sdk.BetaSkillVersionDownloadParams{SkillID: r.ID})
	if err != nil {
		return err
	}
	want := resp.Header.Get(skills.ArchiveDigestHeader)
	if want == "" {
		slog.WarnContext(ctx, "skill archive download advertises no digest; extracting unverified",
			"skill_id", r.ID, "version", r.Version)
	}
	data, err := skills.ReadArchive(resp.Body, want)
	resp.Body.Close()
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
	// One call for the whole tree, for the reason the executor's twin gives:
	// a file at a time costs one sandbox exec per member (#206).
	return sb.WriteFiles(ctx, batch)
}
