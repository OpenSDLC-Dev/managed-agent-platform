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

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
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

// skillDigitsRe matches the pre-GA numeric pin this platform still accepts
// (plan 39 decision 4). The other two forms a stored pin holds need no pattern:
// a version id is domain.ID.HasPrefix, which recognizes both spellings, and the
// alias "latest" is what is left.
var skillDigitsRe = regexp.MustCompile(`^[0-9]+$`)

// SetupSkills materializes the session's skills into the sandbox — its agent's
// own, and on a coordinator session every roster member's beside them — the
// BYOC twin of the executor's materialization and a re-expression of the
// reference worker's SetupSkills (anthropic-sdk-go tools/agenttoolset):
// session GET with the environment key, per skill a version GET that resolves
// the pin and carries the name, the /content download, and extraction under
// the reference guards — all wire, no database, writing through the sandbox
// file API instead of the host filesystem. Per-skill failure is logged and
// skipped, never fatal; only the session read fails the call, mirroring the
// reference. A sentinel under
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
			// The roster snapshot, absent on a single-agent session. Its
			// members' skills join the coordinator's own: the session's threads
			// share one sandbox, so the set materialized into it is the union
			// (plan 35 decision 11). It costs no extra wire read — the roster
			// travels in the session this call already loads.
			Multiagent struct {
				Agents []struct {
					Skills []skillRef `json:"skills"`
				} `json:"agents"`
			} `json:"multiagent"`
		} `json:"agent"`
	}
	if err := json.Unmarshal([]byte(sess.RawJSON()), &snapshot); err != nil {
		return fmt.Errorf("parse session for skills: %w", err)
	}
	// Concatenated in roster order behind the coordinator's own, not
	// deduplicated here: the resolution loop below already collapses repeats by
	// skill id with the first occurrence winning, and a skill's landing
	// directory carries no version — so two members pinning different versions
	// of one skill share a tree and the earlier reference is what lands.
	refs := snapshot.Agent.Skills
	for _, m := range snapshot.Agent.Multiagent.Agents {
		refs = append(refs, m.Skills...)
	}
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
	seen := map[string]string{} // skill id -> the version that won
	misses := 0
	for _, ref := range refs {
		// Resolution is per-reference work too — one round trip each, and the
		// dangling ones leave by the continue below without ever reaching the
		// write loop. A set of them is a run that is moving (#383).
		progress()
		if won, dup := seen[ref.SkillID]; dup {
			// Same version twice is the roster agreeing with itself and worth
			// nothing to anyone. Two versions is a member running against a
			// skill it did not pin, and the materialize log below reports only
			// the winner — so without this line the collision is invisible in
			// logs and metrics alike, and the operator's first sign of it is a
			// member behaving as though its own pin did not exist.
			if won != ref.Version {
				slog.WarnContext(ctx, "roster skill version collision; the first reference wins",
					"session_id", sessionID, "skill_id", ref.SkillID,
					"materialized_version", won, "dropped_version", ref.Version)
			}
			continue
		}
		seen[ref.SkillID] = ref.Version
		r, err := resolveSkill(ctx, client, ref)
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

// resolveSkillVersion is the three-way classification of one stored pin (plan
// 39 decision 5): it returns the token the /content download is addressed by,
// given the pin and the version the retrieve answered it with.
//
// It replaces a two-way "digits or else" test that read anything non-numeric as
// the alias "latest" — so a pin by version id, the GA way to pin one, was
// served the NEWEST version instead of the pinned one. That is a wrong answer
// rather than a refusal, which is why it is a resolution rule and not a
// validation one.
func resolveSkillVersion(pinned string, retrieved *sdk.BetaSkillVersion) string {
	switch {
	case skillDigitsRe.MatchString(pinned):
		// The pre-GA numeric pin this platform still accepts (decision 4). It
		// is not an id, so the retrieve's answer cannot stand in for it; both
		// this platform's /content route and the reference's take the numeric
		// verbatim, so it is carried through.
		return pinned
	case domain.ID(pinned).HasPrefix(domain.PrefixSkillVersion):
		// Already concrete and already the addressing token, in either
		// spelling. Taken from the pin rather than the retrieve's echo, so what
		// lands is the version the agent named whatever answers for it.
		return pinned
	default:
		// The alias "latest", which is all that is left: a pin naming no
		// version at all never reaches here, the retrieve having refused it.
		// Only the retrieve resolves the alias, so the download rides the
		// concrete id it answered with — the reference worker's own rule
		// (anthropic-sdk-go tools/agenttoolset/skills.go).
		return retrieved.ID
	}
}

// resolveSkill resolves one reference to {addressing token, trusted directory}
// in a single round trip: the version retrieve takes every accepted form of a
// pin — the alias, an id, the legacy numeric — and answers with the name the
// landing directory is derived from. The name comes from the version object,
// never the sandbox, so it is safe to drive the skip probe with.
func resolveSkill(ctx context.Context, client sdk.Client, ref skillRef) (skills.Resolved, error) {
	v, err := client.Beta.Skills.Versions.Get(ctx, ref.Version, sdk.BetaSkillVersionGetParams{SkillID: ref.SkillID})
	if err != nil {
		return skills.Resolved{}, err
	}
	return skills.Resolved{
		ID:      ref.SkillID,
		Version: resolveSkillVersion(ref.Version, v),
		Dir:     skills.TargetDir(v.Name, ref.SkillID),
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
