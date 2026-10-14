package executor

import (
	"context"
	"encoding/json"
	"log/slog"
	"path"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/skills"
	"github.com/jackc/pgx/v5"
)

// nestedSkills finds the skills a run's reads reach below a repository's root
// (plan 65 decision 5): a read of a file in a mounted repository scans each
// directory between the file's and the root, the root left out, for
// .claude/skills/<name>/SKILL.md. What a read finds is announced to its thread
// after its result, so it is kept by the result event's id, which the run
// allocates for it, and recorded when the run settles.
type nestedSkills struct {
	sb       sandbox.Sandbox
	sid      domain.ID
	repos    []repoRef
	recorded int // the skills sessions.repo_skills holds, which the cap counts
	// known is, by thread, the location of every nested skill the thread was
	// told of, and room what the cap leaves it; both are read from the log
	// at the thread's first read in the run.
	known map[domain.ID]map[string]bool
	room  map[domain.ID]int
	// listed is the skill names under each directory the run listed, so a
	// batch of reads in one directory lists it once.
	listed map[string][]string
	found  map[domain.ID][]skills.RepoSkill // by result event id
}

// newNestedSkills is a run's collector; recorded is how many skills the
// session's repositories announce from their roots.
func newNestedSkills(sb sandbox.Sandbox, sid domain.ID, repos []repoRef, recorded int) *nestedSkills {
	return &nestedSkills{sb: sb, sid: sid, repos: repos, recorded: recorded,
		known: map[domain.ID]map[string]bool{}, room: map[domain.ID]int{},
		listed: map[string][]string{}, found: map[domain.ID][]skills.RepoSkill{}}
}

// afterRead scans what a read of file reached, for the thread that read it,
// and answers the id its result event must carry when the scan found skills,
// or "" when it found none. Best effort, like the root scan: a scan that did
// not come back whole announces nothing, so a later read below the same
// directories scans them again, and never fails the read.
func (e *Executor) afterRead(ctx context.Context, n *nestedSkills, thread domain.ID, file string, progress func()) domain.ID {
	roots := nestedSkillRoots(file, n.repos)
	if len(roots) == 0 {
		return ""
	}
	if _, ok := n.known[thread]; !ok {
		known, err := e.announcedNestedSkills(ctx, n.sid, thread)
		if err != nil {
			slog.WarnContext(ctx, "nested repository skills not scanned",
				"session_id", n.sid, "thread_id", thread, "err", err)
			return ""
		}
		n.known[thread] = known
		n.room[thread] = max(maxRepoSkills-n.recorded-len(known), 0)
	}
	known := n.known[thread]
	var unlisted []string
	for _, root := range roots {
		if _, ok := n.listed[root]; !ok {
			unlisted = append(unlisted, root)
		}
	}
	if len(unlisted) > 0 {
		names, ok := listSkills(ctx, n.sb, n.sid, unlisted)
		if !ok {
			return ""
		}
		for i, root := range unlisted {
			n.listed[root] = names[i]
		}
	}
	var found []skills.RepoSkill
	room := n.room[thread]
	for _, root := range roots {
		list, ok := readSkills(ctx, n.sb, n.sid, root, n.listed[root], known, room, progress)
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
	n.room[thread] = room
	id := domain.NewID(domain.PrefixEvent)
	n.found[id] = found
	return id
}

// nestedSkillRoots is the .claude/skills directory of each directory between
// file and the root of the mounted repository holding it, nearest first, the
// root's own left out: that one is scanned when the checkout lands. It is
// empty for a file in no mounted repository, or at a repository's root.
func nestedSkillRoots(file string, repos []repoRef) []string {
	for _, r := range repos {
		if r.Type != "github_repository" || r.MountPath == "" {
			continue
		}
		mount := path.Clean(r.MountPath)
		if !strings.HasPrefix(file, mount+"/") {
			continue
		}
		var roots []string
		for d := path.Dir(file); strings.HasPrefix(d, mount+"/"); d = path.Dir(d) {
			roots = append(roots, path.Join(d, ".claude", "skills"))
		}
		return roots
	}
	return nil
}

// announcedNestedSkills is the location of every nested skill the thread was
// told of already, so a later read below the same directories does not tell
// it again.
func (e *Executor) announcedNestedSkills(ctx context.Context, sid, thread domain.ID) (map[string]bool, error) {
	rows, err := e.pool.Query(ctx,
		`SELECT n.skills FROM session_nested_skills n JOIN events ev ON ev.id = n.result_event_id
		  WHERE n.session_id = $1 AND ev.thread_id IS NOT DISTINCT FROM $2`,
		sid.String(), events.NullableThread(thread))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	known := map[string]bool{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var list []skills.RepoSkill
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, err
		}
		for _, s := range list {
			known[s.Location] = true
		}
	}
	return known, rows.Err()
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
