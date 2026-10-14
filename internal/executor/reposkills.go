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
// reference finds them. A skill is named by its directory, and its
// description is read leniently (skills.RepoDescription). Names come sorted,
// and room is what the session's cap leaves (maxRepoSkills).
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
		root := path.Join(r.MountPath, ".claude", "skills")
		names, ok := listSkills(ctx, sb, sid, []string{root})
		if !ok {
			continue
		}
		list, ok := readSkills(ctx, sb, sid, root, names[0], nil, room, progress)
		if !ok || ctx.Err() != nil {
			continue
		}
		found[r.ID] = list
		room -= len(list)
	}
	return found
}

// listSkills lists, for each of roots, the directories under it whose
// SKILL.md is a regular file (listSkillsScript), each root's names sorted.
// It answers false when the listing did not come back whole, which the
// caller reads as a scan to take again later.
func listSkills(ctx context.Context, sb sandbox.Sandbox, sid domain.ID, roots []string) ([][]string, bool) {
	// Framed (sandbox.ExecFramed), so what an image's startup prints around
	// the listing, a BASH_ENV file's banner, is no name of it.
	res, framed, err := sandbox.ExecFramed(ctx, sb, "reposkills",
		sandbox.ExecRequest{Command: listSkillsScript(roots), Timeout: repoSkillsListTimeout})
	if err != nil || !framed || res.ExitCode != 0 || res.TimedOut {
		slog.WarnContext(ctx, "repository skills not listed; a later scan lists them again",
			"session_id", sid, "roots", roots, "err", err, "framed", framed,
			"exit_code", res.ExitCode, "timed_out", res.TimedOut)
		return nil, false
	}
	// Each entry ends in a NUL, so whatever follows the last one is an entry
	// the output cap cut short, and is dropped. The listing is in byte order
	// (LC_ALL=C), root by root, so what the cap keeps is the first names in
	// the order sorted here.
	entries := strings.Split(res.Stdout, "\x00")
	entries = entries[:len(entries)-1]
	names := make([][]string, len(roots))
	for _, entry := range entries {
		// An entry is the root's index, a slash and the name: a name holds no
		// slash, so the first one ends the index.
		at, name, ok := strings.Cut(entry, "/")
		i, err := strconv.Atoi(at)
		if !ok || err != nil || i < 0 || i >= len(roots) {
			continue
		}
		names[i] = append(names[i], name)
	}
	for _, n := range names {
		sort.Strings(n)
	}
	return names, true
}

// readSkills reads the description of each named skill under root and lists
// it, passing over a name already known by its location and stopping at room.
// It answers false when a read failed for a reason a later scan may not meet.
func readSkills(ctx context.Context, sb sandbox.Sandbox, sid domain.ID, root string, names []string, known map[string]bool, room int, progress func()) ([]skills.RepoSkill, bool) {
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
			slog.WarnContext(ctx, "repository skills over the cap are not announced",
				"session_id", sid, "root", root, "cap", maxRepoSkills)
			break
		}
		progress()
		description := ""
		md, err := sb.ReadFile(ctx, loc)
		switch {
		case err == nil:
			description = skills.RepoDescription(md)
		case errors.Is(err, sandbox.ErrFileTooLarge), errors.Is(err, sandbox.ErrNotRegularFile),
			errors.Is(err, sandbox.ErrIsDirectory), errors.Is(err, sandbox.ErrFileNotExist):
			slog.WarnContext(ctx, "repository skill listed without its description",
				"session_id", sid, "location", loc, "err", err)
		default:
			slog.WarnContext(ctx, "repository skill not read; a later scan reads it again",
				"session_id", sid, "location", loc, "err", err)
			return nil, false
		}
		list = append(list, skills.RepoSkill{Name: name, Description: description, Location: loc})
	}
	return list, true
}

// listSkillsScript prints, each followed by a NUL, the index of a root, a
// slash and the name of each directory under that root whose SKILL.md is a
// regular file. A SKILL.md that is a symbolic link is passed over, since a
// read refuses one, and so neither the scan nor the agent could load it; a
// linked directory is followed. Hidden directories are not listed, and an
// absent root lists nothing. The glob runs in byte order, and what an image's
// startup may set that changes what it matches, or its order, is undone:
// noglob (set -f), GLOBIGNORE, GLOBSORT (bash 5.3), and the shopt options,
// dotglob among them.
func listSkillsScript(roots []string) string {
	quoted := make([]string, len(roots))
	for i, r := range roots {
		quoted[i] = shellQuote(r)
	}
	return "LC_ALL=C; export LC_ALL\n" +
		"set +f\n" +
		"unset GLOBIGNORE GLOBSORT\n" +
		"shopt -u dotglob failglob nullglob nocaseglob 2>/dev/null\n" +
		"i=0\n" +
		"for r in " + strings.Join(quoted, " ") + "; do\n" +
		"  if cd -- \"$r\" 2>/dev/null; then\n" +
		"    for d in *; do\n" +
		"      if [ -f \"$d/SKILL.md\" ] && [ ! -h \"$d/SKILL.md\" ]; then printf '%s/%s\\000' \"$i\" \"$d\"; fi\n" +
		"    done\n" +
		"  fi\n" +
		"  i=$((i+1))\n" +
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
