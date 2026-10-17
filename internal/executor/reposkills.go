package executor

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/skills"
	"github.com/jackc/pgx/v5"
)

// maxRepoSkills bounds what one session's repositories announce: the
// attached-skills cap (ours; the reference's bound is unrecorded).
const maxRepoSkills = 500

// repoSkillsListTimeout bounds the listing of one checkout's skills.
const repoSkillsListTimeout = 30 * time.Second

// unscannedRepos is the checkouts this pass found present whose skills the
// session has no entry for yet: each repository is scanned once, when its
// checkout first lands (plan 65 decision 2).
func unscannedRepos(present []repoRef, recorded map[string][]skills.RepoSkill) []repoRef {
	var out []repoRef
	for _, r := range present {
		if _, ok := recorded[r.ID]; !ok {
			out = append(out, r)
		}
	}
	return out
}

// scanRepoSkills lists the skills the given checkouts hold, each by the
// resource's id: each directory under a checkout's .claude/skills whose
// SKILL.md is a regular file, by that exact name and one level down, as the
// reference finds them. A skill is named by its frontmatter where that gives a
// usable name (skills.RepoName), else by its directory, and its description is
// read leniently (skills.RepoDescription). Skills come in their directories'
// sorted order, and room is what the session's cap leaves (maxRepoSkills).
//
// Only a whole scan of a checkout is returned. One whose listing failed, or
// whose run was cancelled under it, or a SKILL.md read that failed for a
// reason the next run may not meet, leaves that checkout out, so a later run
// scans it again. A SKILL.md the sandbox refuses to read for good, too large,
// not a regular file, a directory or gone since the listing, is listed with an
// empty description, as the agent's own read of it would fail.
func (e *Executor) scanRepoSkills(ctx context.Context, sb sandbox.Sandbox, sid domain.ID, refs []repoRef, room int, progress func()) map[string][]skills.RepoSkill {
	found := map[string][]skills.RepoSkill{}
	for _, r := range refs {
		progress()
		logger := slog.With("session_id", sid, "resource_id", r.ID)
		mount := path.Clean(r.MountPath)
		// A listing the output cap cut short keeps its first names, in
		// listing order, as the cap keeps the first.
		names, _, ok := listSkills(ctx, sb, logger, []string{mount})
		if !ok {
			continue
		}
		list, ok := readSkills(ctx, sb, logger, path.Join(mount, ".claude", "skills"), names[0], nil, room, progress)
		if !ok || ctx.Err() != nil {
			continue
		}
		found[r.ID] = list
		room -= len(list)
	}
	return found
}

// listSkills lists, for each of dirs, the directories under its
// .claude/skills whose SKILL.md is a regular file (listSkillsScript), each
// one's names sorted. dirs is a chain, nearest first, each after the first
// its predecessor's parent. It answers cut when the output cap cut the
// listing short, so that only its first names came back, and false when the
// listing did not come back, which the caller reads as a scan to take again
// later.
func listSkills(ctx context.Context, sb sandbox.Sandbox, logger *slog.Logger, dirs []string) (names [][]string, cut, ok bool) {
	// Framed (sandbox.ExecFramed), so what an image's startup prints around
	// the listing, a BASH_ENV file's banner, is no name of it.
	res, framed, err := sandbox.ExecFramed(ctx, sb, "reposkills",
		sandbox.ExecRequest{Command: listSkillsScript(dirs[0], dirs[len(dirs)-1]), Timeout: repoSkillsListTimeout})
	if err != nil || !framed || res.ExitCode != 0 || res.TimedOut {
		logger.WarnContext(ctx, "repository skills not listed; a later scan lists them again",
			"dirs", dirs, "err", err, "framed", framed, "exit_code", res.ExitCode, "timed_out", res.TimedOut)
		return nil, false, false
	}
	// Each entry ends in a NUL, so whatever follows the last one is an entry
	// the output cap cut short, and is dropped. The listing is in byte order
	// (LC_ALL=C), directory by directory, so what the cap keeps is the first
	// names in the order sorted here.
	entries := strings.Split(res.Stdout, "\x00")
	entries = entries[:len(entries)-1]
	names = make([][]string, len(dirs))
	for _, entry := range entries {
		// An entry is the directory's index, a slash and the name: a name
		// holds no slash, so the first one ends the index.
		at, name, ok := strings.Cut(entry, "/")
		i, err := strconv.Atoi(at)
		if !ok || err != nil || i < 0 || i >= len(dirs) {
			continue
		}
		names[i] = append(names[i], name)
	}
	for _, n := range names {
		sort.Strings(n)
	}
	return names, res.StdoutTruncated, true
}

// readSkills reads the description of each named skill under root and lists
// it, passing over a name already known by its location and stopping at room.
// It answers false when a read failed for a reason a later scan may not meet.
func readSkills(ctx context.Context, sb sandbox.Sandbox, logger *slog.Logger, root string, names []string, known map[string]bool, room int, progress func()) ([]skills.RepoSkill, bool) {
	list := []skills.RepoSkill{}
	for _, name := range names {
		// A name that is not UTF-8 cannot be announced as the path it is, and
		// one holding a control character, a newline among them, would break
		// the list it is announced in.
		if name == "" || !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) {
			continue
		}
		loc := path.Join(root, name, "SKILL.md")
		if known[loc] {
			continue
		}
		if len(list) >= room {
			logger.WarnContext(ctx, "repository skills over the cap are not announced",
				"root", root, "cap", maxRepoSkills)
			break
		}
		progress()
		announced, description := name, ""
		md, err := sb.ReadFile(ctx, loc)
		switch {
		case err == nil:
			if n := skills.RepoName(md); n != "" {
				announced = n
			}
			description = skills.RepoDescription(md)
		case errors.Is(err, sandbox.ErrFileTooLarge), errors.Is(err, sandbox.ErrNotRegularFile),
			errors.Is(err, sandbox.ErrIsDirectory), errors.Is(err, sandbox.ErrFileNotExist):
			logger.WarnContext(ctx, "repository skill listed without its description",
				"location", loc, "err", err)
		default:
			logger.WarnContext(ctx, "repository skill not read; a later scan reads it again",
				"location", loc, "err", err)
			return nil, false
		}
		list = append(list, skills.RepoSkill{Name: announced, Description: description, Location: loc})
	}
	return list, true
}

// listSkillsScript prints, each followed by a NUL, an index, a slash and the
// name of each directory under <dir>/.claude/skills whose SKILL.md is a
// regular file, for dir from first up to last, one parent at a time, the
// index counting the steps. The command names only the two ends, so it grows
// with the path, not with the path times its depth. A SKILL.md that is a
// symbolic link is passed over, since a read refuses one, and so neither the
// scan nor the agent could load it; a linked directory is followed. Hidden
// directories are not listed, and an absent .claude/skills lists nothing. The
// glob runs in byte order (LC_ALL=C), and the listing's frame undoes what an
// image's startup may set that changes what it matches, or its order
// (sandbox.ScriptPreamble).
func listSkillsScript(first, last string) string {
	return "LC_ALL=C; export LC_ALL\n" +
		"d=" + shellQuote(first) + "; i=0\n" +
		"while :; do\n" +
		"  if cd -- \"$d/.claude/skills\" 2>/dev/null; then\n" +
		"    for s in *; do\n" +
		"      if [ -f \"$s/SKILL.md\" ] && [ ! -h \"$s/SKILL.md\" ]; then printf '%s/%s\\000' \"$i\" \"$s\"; fi\n" +
		"    done\n" +
		"  fi\n" +
		"  if [ \"$d\" = " + shellQuote(last) + " ]; then break; fi\n" +
		"  d=${d%/*}; i=$((i+1))\n" +
		"  if [ -z \"$d\" ]; then break; fi\n" +
		"done\n"
}

// recordRepoSkills adds a run's scans to the session's repository skills,
// inside the transaction that settles the run: it holds the session's row
// lock and proves the claimant's lease (settleDrain), so a claimant that lost
// its item, and may have read a sandbox the reclaim has since replaced,
// records nothing. An entry already recorded for a repository stands.
func recordRepoSkills(ctx context.Context, tx pgx.Tx, sid domain.ID, found map[string][]skills.RepoSkill) error {
	if len(found) == 0 {
		return nil
	}
	b, err := json.Marshal(found)
	if err != nil {
		return err
	}
	// In `a || b` the right operand's keys win, so an existing entry does.
	_, err = tx.Exec(ctx,
		`UPDATE sessions SET repo_skills = $2::jsonb || COALESCE(repo_skills, '{}'::jsonb) WHERE id = $1`,
		sid.String(), b)
	return err
}

// recordedRepoSkills decodes sessions.repo_skills as sessionForRun read it.
func recordedRepoSkills(raw []byte) (map[string][]skills.RepoSkill, error) {
	recorded := map[string][]skills.RepoSkill{}
	if raw == nil {
		return recorded, nil
	}
	if err := json.Unmarshal(raw, &recorded); err != nil {
		return nil, err
	}
	return recorded, nil
}

// repoSkillsRoom is what the session's cap leaves for repositories not yet
// scanned.
func repoSkillsRoom(recorded map[string][]skills.RepoSkill) int {
	return max(maxRepoSkills-countRepoSkills(recorded), 0)
}

// countRepoSkills is how many skills the record holds.
func countRepoSkills(recorded map[string][]skills.RepoSkill) int {
	n := 0
	for _, list := range recorded {
		n += len(list)
	}
	return n
}
