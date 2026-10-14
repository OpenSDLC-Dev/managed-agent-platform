package executor

import (
	"context"
	"encoding/json"
	"log/slog"
	"path"
	"slices"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/skills"
	"github.com/jackc/pgx/v5"
)

// nestedSkills finds the skills a run's reads reach below a repository's root
// (plan 65 decision 5): a read of a file in a checkout scans each directory
// between the file's and the root, the root left out, for
// .claude/skills/<name>/SKILL.md. What a read finds is announced to its thread
// after its result, so it is kept by the result event's id, which the run
// allocates for it, and recorded when the run settles.
type nestedSkills struct {
	sb       sandbox.Sandbox
	sid      domain.ID
	repos    []repoRef // the checkouts this run found present
	recorded int       // the skills the root scans announce, which the cap counts
	// known is, by thread, the location of every nested skill the thread was
	// told of, read from the log at the run's first read (toldNestedSkills).
	known map[domain.ID]map[string]bool
	// listed is the skill names under each directory listed since the run's
	// last tool other than a read, so a batch of reads in one directory lists
	// it once, and a skill another tool made is listed.
	listed map[string][]string
	found  map[domain.ID][]skills.RepoSkill // by result event id
}

// newNestedSkills is a run's collector; recorded is how many skills the
// session's repositories announce from their roots.
func newNestedSkills(sb sandbox.Sandbox, sid domain.ID, repos []repoRef, recorded int) *nestedSkills {
	return &nestedSkills{sb: sb, sid: sid, repos: repos, recorded: recorded,
		listed: map[string][]string{}, found: map[domain.ID][]skills.RepoSkill{}}
}

// forgetListings drops the run's listings, after a tool that may have made
// or removed a skill.
func (n *nestedSkills) forgetListings() { clear(n.listed) }

// afterRead scans what a read of file reached, for the thread that read it,
// and answers the id its result event must carry when the scan found skills,
// or "" when it found none. Best effort, like the root scan: a scan that did
// not come back whole announces nothing, so a later read below the same
// directories scans them again, and never fails the read.
func (e *Executor) afterRead(ctx context.Context, n *nestedSkills, thread domain.ID, file string, progress func()) domain.ID {
	dirs := nestedSkillDirs(file, n.repos)
	if len(dirs) == 0 {
		return ""
	}
	logger := slog.With("session_id", n.sid, "thread_id", thread)
	if n.known == nil {
		told, err := e.toldNestedSkills(ctx, n.sid)
		if err != nil {
			logger.WarnContext(ctx, "nested repository skills not scanned", "err", err)
			return ""
		}
		n.known = told
	}
	known := n.known[thread]
	if known == nil {
		known = map[string]bool{}
		n.known[thread] = known
	}
	// The cap counts the root scans' skills and the thread's nested ones.
	room := maxRepoSkills - n.recorded - len(known)
	if room <= 0 {
		return ""
	}
	if slices.ContainsFunc(dirs, func(d string) bool { _, ok := n.listed[d]; return !ok }) {
		names, cut, ok := listSkills(ctx, n.sb, logger, dirs)
		if !ok {
			return ""
		}
		if cut {
			// The output cap cut the listing short, so the farther
			// directories' skills are missing from it, not absent.
			logger.WarnContext(ctx, "nested repository skills not announced: the listing was cut short", "dirs", dirs)
			return ""
		}
		for i, d := range dirs {
			n.listed[d] = names[i]
		}
	}
	var found []skills.RepoSkill
	for _, d := range dirs {
		if room <= 0 {
			break
		}
		list, ok := readSkills(ctx, n.sb, logger, path.Join(d, ".claude", "skills"), n.listed[d], known, room, progress)
		if !ok || ctx.Err() != nil {
			return ""
		}
		found = append(found, list...)
		room -= len(list)
	}
	if len(found) == 0 {
		return ""
	}
	for _, s := range found {
		known[s.Location] = true
	}
	id := domain.NewID(domain.PrefixEvent)
	n.found[id] = found
	return id
}

// nestedSkillDirs is each directory between file and the root of the
// checkout holding it, nearest first, the root left out: its skills are the
// root scan's. The checkout is the innermost whose mount holds the file, so a
// repository mounted inside another is the one a read in it reaches. It is
// empty for a file in no checkout, or at a checkout's root.
func nestedSkillDirs(file string, repos []repoRef) []string {
	mount := ""
	for _, r := range repos {
		if m := path.Clean(r.MountPath); strings.HasPrefix(file, m+"/") && len(m) > len(mount) {
			mount = m
		}
	}
	if mount == "" {
		return nil
	}
	var dirs []string
	for d := path.Dir(file); strings.HasPrefix(d, mount+"/"); d = path.Dir(d) {
		dirs = append(dirs, d)
	}
	return dirs
}

// toldNestedSkills is, by thread (the primary's id empty), the location of
// every nested skill the thread was told of, so a later read below the same
// directories does not tell it again. A row whose skills do not decode was
// not written by the executor, and is passed over, as the brain passes it
// over: it tells no thread anything, so it counts for nothing.
func (e *Executor) toldNestedSkills(ctx context.Context, sid domain.ID) (map[domain.ID]map[string]bool, error) {
	rows, err := e.pool.Query(ctx,
		`SELECT n.result_event_id, COALESCE(ev.thread_id, ''), n.skills
		   FROM session_nested_skills n JOIN events ev ON ev.id = n.result_event_id
		  WHERE n.session_id = $1`,
		sid.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	told := map[domain.ID]map[string]bool{}
	for rows.Next() {
		var id, thread string
		var raw []byte
		if err := rows.Scan(&id, &thread, &raw); err != nil {
			return nil, err
		}
		var list []skills.RepoSkill
		if err := json.Unmarshal(raw, &list); err != nil {
			slog.WarnContext(ctx, "nested repository skills record not read",
				"session_id", sid, "result_event_id", id, "err", err)
			continue
		}
		known := told[domain.ID(thread)]
		if known == nil {
			known = map[string]bool{}
			told[domain.ID(thread)] = known
		}
		for _, s := range list {
			known[s.Location] = true
		}
	}
	return told, rows.Err()
}

// mostNestedSkills is how many nested skills the thread told of the most was
// told of, for the room a root scan leaves them.
func (e *Executor) mostNestedSkills(ctx context.Context, sid domain.ID) (int, error) {
	told, err := e.toldNestedSkills(ctx, sid)
	if err != nil {
		return 0, err
	}
	most := 0
	for _, known := range told {
		most = max(most, len(known))
	}
	return most, nil
}

// recordNestedSkills stores what the run's reads found, inside the
// transaction that settles the run, each by its result event. A result the
// settlement dropped, its call answered already, committed no event, so
// nothing is recorded for it.
func recordNestedSkills(ctx context.Context, tx pgx.Tx, sid domain.ID, found map[domain.ID][]skills.RepoSkill) error {
	for id, list := range found {
		b, err := json.Marshal(list)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO session_nested_skills (result_event_id, session_id, skills)
			 SELECT $1, $2, $3 WHERE EXISTS (SELECT 1 FROM events WHERE id = $1)
			 ON CONFLICT DO NOTHING`,
			id.String(), sid.String(), b); err != nil {
			return err
		}
	}
	return nil
}
