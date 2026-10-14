package executor

import (
	"context"
	"encoding/json"
	"log/slog"
	"path"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/skills"
)

// repoSkill is one skill found in a mounted repository, as the brain announces
// it (plan 65 decision 3). It is the element shape of sessions.repo_skills,
// which internal/brain reads.
type repoSkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Location    string `json:"location"`
}

// maxRepoSkills bounds what one session's repositories announce: the
// attached-skills cap (ours; the reference's bound is unrecorded).
const maxRepoSkills = 500

// repoSkillsListTimeout bounds the listing of one checkout's skills.
const repoSkillsListTimeout = 30 * time.Second

// scanRepoSkills lists the skills the session's repositories hold: each
// directory under a checkout's .claude/skills whose SKILL.md is a file, by
// that exact name and one level down, as the reference finds them. A skill is
// named by its directory, and its description is read leniently
// (skills.RepoDescription). Entries come in resource order, then by name.
//
// Best effort, like the clone before it: a checkout that is absent lists
// nothing, one that cannot be listed is logged and passed over, and a SKILL.md
// that cannot be read is logged and listed with an empty description, so a
// broken repository costs the session its skills, never its run.
func (e *Executor) scanRepoSkills(ctx context.Context, sb sandbox.Sandbox, sid domain.ID, refs []repoRef, progress func()) []repoSkill {
	found := []repoSkill{}
	for _, r := range refs {
		if r.Type != "github_repository" || r.MountPath == "" {
			continue
		}
		progress()
		root := path.Join(r.MountPath, ".claude", "skills")
		// Framed (sandbox.ExecFramed), so what an image's startup prints
		// around the listing, a BASH_ENV file's banner, is no name of it.
		res, framed, err := sandbox.ExecFramed(ctx, sb, "reposkills",
			sandbox.ExecRequest{Command: listRepoSkillsScript(root), Timeout: repoSkillsListTimeout})
		if err != nil || !framed || res.ExitCode != 0 || res.TimedOut {
			slog.WarnContext(ctx, "repository skills not listed",
				"session_id", sid, "resource_id", r.ID, "err", err, "framed", framed,
				"exit_code", res.ExitCode, "timed_out", res.TimedOut)
			continue
		}
		// Each name ends in a NUL, so whatever follows the last one is a name
		// the output cap cut short, and is dropped.
		names := strings.Split(res.Stdout, "\x00")
		names = names[:len(names)-1]
		sort.Strings(names)
		for _, name := range names {
			// A name that is not UTF-8 cannot be announced as the path it is,
			// and one holding a control character, a newline among them, would
			// break the list it is announced in.
			if name == "" || !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) {
				continue
			}
			if len(found) == maxRepoSkills {
				slog.WarnContext(ctx, "repository skills over the cap are not announced",
					"session_id", sid, "cap", maxRepoSkills)
				return found
			}
			progress()
			loc := path.Join(root, name, "SKILL.md")
			description := ""
			if md, err := sb.ReadFile(ctx, loc); err != nil {
				slog.WarnContext(ctx, "repository skill read without its description",
					"session_id", sid, "resource_id", r.ID, "location", loc, "err", err)
			} else {
				description = skills.RepoDescription(md)
			}
			found = append(found, repoSkill{Name: name, Description: description, Location: loc})
		}
	}
	return found
}

// listRepoSkillsScript prints, each followed by a NUL, the name of every
// directory under root whose SKILL.md is a regular file. A SKILL.md that is a
// symbolic link is passed over, since a read refuses one, and so neither the
// scan nor the agent could load it; a linked directory is followed. Hidden
// directories are not listed, and an absent root prints nothing.
func listRepoSkillsScript(root string) string {
	return "cd -- " + shellQuote(root) + " 2>/dev/null || exit 0\n" +
		"for d in *; do\n" +
		"  if [ -f \"$d/SKILL.md\" ] && [ ! -h \"$d/SKILL.md\" ]; then printf '%s\\000' \"$d\"; fi\n" +
		"done\n"
}

// recordRepoSkills stores a scan as the session's repository skills, once:
// the first scan to land wins. The write proves the claimant still holds the
// item (queue.Assert), as every state write of a run does: a claimant that
// lost its lease may have read a sandbox the reclaim has since replaced, and
// its scan must not become the session's for good. A session with no
// repository records an empty scan, which keeps the brain from waiting on one.
func (e *Executor) recordRepoSkills(ctx context.Context, item *queue.Item, found []repoSkill) error {
	b, err := json.Marshal(found)
	if err != nil {
		return err
	}
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := e.queue.Assert(ctx, tx, item); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE sessions SET repo_skills = $2 WHERE id = $1 AND repo_skills IS NULL`,
		item.SessionID.String(), b); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
