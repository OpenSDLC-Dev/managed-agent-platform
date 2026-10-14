package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/skills"
)

// TestNestedSkillDirs pins which directories a read scans (plan 65 decision
// 5): each between the file's and its checkout's root, nearest first, the
// root left out; none for a file at the root or in no checkout. A mount whose
// path only begins the file's is not its checkout, and of two mounts that
// hold the file, the innermost is.
func TestNestedSkillDirs(t *testing.T) {
	// One inner mount comes before its outer one, the other after it, so
	// neither the first match nor the last is the innermost by order alone.
	repos := []repoRef{
		{ID: "sesrsc_inner", MountPath: "/workspace/r/inner/"},
		{ID: "sesrsc_r", MountPath: "/workspace/r"},
		{ID: "sesrsc_rx", MountPath: "/workspace/rx"},
		{ID: "sesrsc_s", MountPath: "/workspace/s"},
		{ID: "sesrsc_s_inner", MountPath: "/workspace/s/inner"},
	}
	for _, c := range []struct {
		file string
		want []string
	}{
		{"/workspace/r/a/b/c.txt", []string{"/workspace/r/a/b", "/workspace/r/a"}},
		{"/workspace/r/a/c.txt", []string{"/workspace/r/a"}},
		{"/workspace/r/c.txt", nil},
		{"/workspace/rx/a/c.txt", []string{"/workspace/rx/a"}},
		{"/workspace/ry/a/c.txt", nil},
		{"/workspace/r/inner/a/c.txt", []string{"/workspace/r/inner/a"}},
		{"/workspace/r/innerx/c.txt", []string{"/workspace/r/innerx"}},
		{"/workspace/s/inner/a/c.txt", []string{"/workspace/s/inner/a"}},
		{"/etc/hosts", nil},
	} {
		if got := nestedSkillDirs(c.file, repos); !reflect.DeepEqual(got, c.want) {
			t.Errorf("nestedSkillDirs(%s) = %v, want %v", c.file, got, c.want)
		}
	}
}

// nestedSkillsRecord reads what the session's reads found, by result event id.
func (h *harness) nestedSkillsRecord(t *testing.T) map[string][]skills.RepoSkill {
	t.Helper()
	rows, err := h.pool.Query(context.Background(),
		`SELECT result_event_id, skills FROM session_nested_skills WHERE session_id = $1 AND jsonb_typeof(skills) = 'array'`,
		h.sid.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string][]skills.RepoSkill{}
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			t.Fatal(err)
		}
		var list []skills.RepoSkill
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatal(err)
		}
		out[id] = list
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// resultEventID is the id of the agent.tool_result event answering use.
func (h *harness) resultEventID(t *testing.T, use domain.ID) string {
	t.Helper()
	var id string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT id FROM events WHERE session_id = $1 AND type = $2 AND payload->>'tool_use_id' = $3`,
		h.sid.String(), string(domain.EventAgentToolResult), use.String()).Scan(&id); err != nil {
		t.Fatalf("the result for %s: %v", use, err)
	}
	return id
}

// told plants a result on thread's log and records skills as what it found:
// a JSON array of skills, or, to stand for a row the executor did not write,
// anything else.
func (h *harness) told(t *testing.T, thread domain.ID, skillsJSON string) {
	t.Helper()
	ctx := context.Background()
	id := domain.NewID(domain.PrefixEvent)
	if _, err := h.log.Append(ctx, h.sid, []events.NewEvent{{ID: id, Type: domain.EventAgentToolResult, ThreadID: thread,
		Payload: json.RawMessage(`{"tool_use_id":"sevt_x","content":[{"type":"text","text":"ok"}]}`)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx, `INSERT INTO session_nested_skills (result_event_id, session_id, skills) VALUES ($1, $2, $3)`,
		id.String(), h.sid.String(), skillsJSON); err != nil {
		t.Fatal(err)
	}
}

// resultIsError is whether the result answering use is an error.
func (h *harness) resultIsError(t *testing.T, use domain.ID) bool {
	t.Helper()
	var isError bool
	if err := h.pool.QueryRow(context.Background(),
		`SELECT COALESCE((payload->>'is_error')::boolean, false) FROM events WHERE id = $1`, h.resultEventID(t, use)).Scan(&isError); err != nil {
		t.Fatal(err)
	}
	return isError
}

// TestNestedSkillsFoundByARead is decision 5 on a real checkout, after the
// recording's subtree read: a read of a file deep in the repository records
// the skills of the directories between it and the root, nearest first, by
// its result event, and leaves the root's own to the root scan. A write, and
// a read that failed, scan nothing, so the read after them is the one told. A
// thread is told of a skill once: a later read below the same directories,
// in the same run or a later one, records only what is new to it, a skill
// another tool made in the run among it, and another thread is told on its
// own.
func TestNestedSkillsFoundByARead(t *testing.T) {
	fx := newGitFixture(t, map[string]string{
		".claude/skills/alpha/SKILL.md":                  "---\ndescription: Root marker.\n---\n",
		"packages/.claude/skills/mid/SKILL.md":           "---\ndescription: Mid marker.\n---\n",
		"packages/x/.claude/skills/inner/SKILL.md":       "---\ndescription: Inner marker.\n---\n",
		"packages/x/.claude/skills/inner-lower/skill.md": "---\ndescription: lowercase\n---\n",
		"packages/x/probe.txt":                           "probe\n",
		"packages/x/other.txt":                           "other\n",
		"packages/y/.claude/skills/why/SKILL.md":         "---\ndescription: Why marker.\n---\n",
		"packages/y/z.txt":                               "z\n",
	})
	h, _ := dockerRepoHarness(t)
	h.seedRepoResource(t, "sesrsc_skills", fx.url(), repoMount, "ghp_fixture", nil)
	inner := skills.RepoSkill{Name: "inner", Description: "Inner marker.", Location: repoMount + "/packages/x/.claude/skills/inner/SKILL.md"}
	mid := skills.RepoSkill{Name: "mid", Description: "Mid marker.", Location: repoMount + "/packages/.claude/skills/mid/SKILL.md"}
	fresh := skills.RepoSkill{Name: "fresh", Description: "Fresh marker.", Location: repoMount + "/packages/x/.claude/skills/fresh/SKILL.md"}
	why := skills.RepoSkill{Name: "why", Description: "Why marker.", Location: repoMount + "/packages/y/.claude/skills/why/SKILL.md"}

	evs := h.suspend(t, writeUse(repoMount+"/packages/x/new.txt", "new"),
		readUse(repoMount+"/packages/x/missing.txt"), readUse(repoMount+"/packages/x/probe.txt"),
		writeUse(fresh.Location, "---\ndescription: Fresh marker.\n---\n"),
		readUse(repoMount+"/packages/x/other.txt"))
	h.stepOnce(t)
	want := map[string][]skills.RepoSkill{
		h.resultEventID(t, evs[2].ID): {inner, mid},
		h.resultEventID(t, evs[4].ID): {fresh},
	}
	if got := h.nestedSkillsRecord(t); !reflect.DeepEqual(got, want) {
		t.Errorf("first run: nested skills = %+v\nwant %+v", got, want)
	}

	// Relative, as a model may name it: it resolves against the workdir.
	later := h.suspend(t, readUse("fixture/packages/y/z.txt"))
	h.stepOnce(t)
	want[h.resultEventID(t, later[0].ID)] = []skills.RepoSkill{why}
	if got := h.nestedSkillsRecord(t); !reflect.DeepEqual(got, want) {
		t.Errorf("a later run: nested skills = %+v\nwant %+v", got, want)
	}

	child := h.childThread(t, "running")
	use := h.appendOn(t, child, domain.EventAgentToolUse, withPerm(readUse(repoMount+"/packages/x/probe.txt"), "allow"))
	h.enqueueToolExec(t)
	h.stepOnce(t)
	want[h.resultEventID(t, use)] = []skills.RepoSkill{fresh, inner, mid}
	if got := h.nestedSkillsRecord(t); !reflect.DeepEqual(got, want) {
		t.Errorf("another thread: nested skills = %+v\nwant %+v", got, want)
	}
}

// TestNestedSkillsOnlyInAPresentCheckout: a mount whose clone failed holds no
// repository, so skills the agent wrote under its path are none of its, and a
// read there announces nothing.
func TestNestedSkillsOnlyInAPresentCheckout(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"README.md": "readme\n"})
	fx.status.Store(http.StatusUnauthorized)
	h, _ := dockerRepoHarness(t)
	h.seedRepoResource(t, "sesrsc_fail", fx.url(), repoMount, "ghp_wrong", nil)

	evs := h.suspend(t, writeUse(repoMount+"/x/.claude/skills/foo/SKILL.md", "---\ndescription: Foo.\n---\n"),
		writeUse(repoMount+"/x/f.txt", "f"), readUse(repoMount+"/x/f.txt"))
	h.stepOnce(t)
	if h.resultIsError(t, evs[2].ID) {
		t.Fatal("the read failed; it must reach the file for the scan to be in question")
	}
	if got := h.nestedSkillsRecord(t); len(got) != 0 {
		t.Errorf("nested skills = %+v, want none: no repository is checked out there", got)
	}
}

// TestNestedSkillsCountTheRootScans pins the run's wiring of the cap: what
// the session's root scans announce, recorded before the run and scanned in
// it, counts against what a read may add. 498 recorded and the checkout's one
// root skill leave room for one, the nearest.
func TestNestedSkillsCountTheRootScans(t *testing.T) {
	fx := newGitFixture(t, map[string]string{
		".claude/skills/alpha/SKILL.md":            "---\ndescription: Root marker.\n---\n",
		"packages/.claude/skills/mid/SKILL.md":     "---\ndescription: Mid marker.\n---\n",
		"packages/x/.claude/skills/inner/SKILL.md": "---\ndescription: Inner marker.\n---\n",
		"packages/x/probe.txt":                     "probe\n",
	})
	h, _ := dockerRepoHarness(t)
	h.seedRepoResource(t, "sesrsc_skills", fx.url(), repoMount, "ghp_fixture", nil)
	recorded := make([]skills.RepoSkill, maxRepoSkills-2)
	for i := range recorded {
		recorded[i] = skills.RepoSkill{Name: fmt.Sprintf("s%03d", i), Location: fmt.Sprintf("/workspace/o/.claude/skills/s%03d/SKILL.md", i)}
	}
	raw, err := json.Marshal(map[string][]skills.RepoSkill{"sesrsc_other": recorded})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(context.Background(), `UPDATE sessions SET repo_skills = $2 WHERE id = $1`, h.sid.String(), raw); err != nil {
		t.Fatal(err)
	}

	evs := h.suspend(t, readUse(repoMount+"/packages/x/probe.txt"))
	h.stepOnce(t)
	want := map[string][]skills.RepoSkill{h.resultEventID(t, evs[0].ID): {
		{Name: "inner", Description: "Inner marker.", Location: repoMount + "/packages/x/.claude/skills/inner/SKILL.md"},
	}}
	if got := h.nestedSkillsRecord(t); !reflect.DeepEqual(got, want) {
		t.Errorf("nested skills = %+v\nwant %+v", got, want)
	}
}

// TestRootScanLeavesRoomForNestedSkills is the cap the other way: the root
// list reaches every thread, so a checkout scanned after a thread was told of
// nested skills is scanned into what the cap leaves that thread, the one told
// of the most. A row the executor did not write counts for nothing.
func TestRootScanLeavesRoomForNestedSkills(t *testing.T) {
	fx := newGitFixture(t, map[string]string{
		".claude/skills/alpha/SKILL.md": "---\ndescription: Alpha marker.\n---\n",
		".claude/skills/beta/SKILL.md":  "---\ndescription: Beta marker.\n---\n",
		"README.md":                     "readme\n",
	})
	h, _ := dockerRepoHarness(t)
	h.seedRepoResource(t, "sesrsc_skills", fx.url(), repoMount, "ghp_fixture", nil)
	nested := make([]skills.RepoSkill, maxRepoSkills-1)
	for i := range nested {
		nested[i] = skills.RepoSkill{Name: fmt.Sprintf("n%03d", i), Location: fmt.Sprintf(repoMount+"/n/.claude/skills/n%03d/SKILL.md", i)}
	}
	raw, err := json.Marshal(nested)
	if err != nil {
		t.Fatal(err)
	}
	h.told(t, "", string(raw))
	h.told(t, "", `"not a list"`)
	h.told(t, "sthr_corrupt", "["+strings.Repeat("1,", maxRepoSkills)+"1]")
	h.told(t, "sthr_small", `[{"name":"s","description":"","location":"`+repoMount+`/s/.claude/skills/s/SKILL.md"}]`)

	h.suspend(t, readUse(repoMount+"/README.md"))
	h.stepOnce(t)
	got, ok := h.repoSkillsRecord(t)
	want := map[string][]skills.RepoSkill{"sesrsc_skills": {
		{Name: "alpha", Description: "Alpha marker.", Location: repoMount + "/.claude/skills/alpha/SKILL.md"},
	}}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("repository skills = %+v (recorded %v)\nwant %+v", got, ok, want)
	}
}

// TestNestedSkillsRoomAndWholeScans pins what a thread's room is and when a
// scan counts. Room is the cap less the root scans' skills and what the
// thread was told already, another thread's not counted, and a read's
// findings use it up for the reads after it. A row the executor did not
// write counts for nothing. A scan whose listing failed or was cut short, or
// one of whose reads failed for a reason a later read may not meet, records
// nothing, so the next read below the same directories records all of it.
func TestNestedSkillsRoomAndWholeScans(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	ctx := context.Background()
	repos := []repoRef{{ID: "sesrsc_r", Type: "github_repository", MountPath: "/workspace/r"}}
	const file = "/workspace/r/x/y/f.txt"
	a := skills.RepoSkill{Name: "a", Description: "From a.", Location: "/workspace/r/x/y/.claude/skills/a/SKILL.md"}
	b := skills.RepoSkill{Name: "b", Description: "From b.", Location: "/workspace/r/x/.claude/skills/b/SKILL.md"}
	h.told(t, "", `[{"name":"z","description":"","location":"/workspace/r/q/.claude/skills/z/SKILL.md"}]`)
	h.told(t, "", `{"not":"a list"}`)
	h.told(t, "", `[1,2,3]`)
	h.told(t, "sthr_other", `[{"name":"a","description":"","location":"`+a.Location+`"}]`)

	sb := &listingSandbox{stdout: "0/a\x001/b\x00"}
	n := newNestedSkills(sb, h.sid, repos, maxRepoSkills-2)
	id := h.exec.afterRead(ctx, n, "", file, func() {})
	if want := map[domain.ID][]skills.RepoSkill{id: {a}}; id == "" || !reflect.DeepEqual(n.found, want) {
		t.Errorf("found = %+v, want a alone: the cap less two root skills and one told", n.found)
	}
	execs := sb.execs
	if again := h.exec.afterRead(ctx, n, "", "/workspace/r/q/f.txt", func() {}); again != "" || sb.execs != execs {
		t.Errorf("a read after the room was used recorded %+v, listing %d times", n.found[again], sb.execs-execs)
	}

	sb = &listingSandbox{stdout: "0/a\x00", truncated: true, failExecs: 1,
		readErr: map[string]error{b.Location: errors.New("transient")}}
	n = newNestedSkills(sb, h.sid, repos, 0)
	if id := h.exec.afterRead(ctx, n, "sthr_fresh", file, func() {}); id != "" || len(n.found) != 0 {
		t.Fatalf("a scan whose listing failed recorded %+v", n.found)
	}
	if id := h.exec.afterRead(ctx, n, "sthr_fresh", file, func() {}); id != "" || len(n.found) != 0 {
		t.Fatalf("a scan whose listing was cut short recorded %+v", n.found)
	}
	sb.stdout, sb.truncated = "0/a\x001/b\x00", false
	if id := h.exec.afterRead(ctx, n, "sthr_fresh", file, func() {}); id != "" || len(n.found) != 0 {
		t.Fatalf("a scan with a failed read recorded %+v", n.found)
	}
	delete(sb.readErr, b.Location)
	id = h.exec.afterRead(ctx, n, "sthr_fresh", file, func() {})
	if want := map[domain.ID][]skills.RepoSkill{id: {a, b}}; id == "" || !reflect.DeepEqual(n.found, want) {
		t.Errorf("after the failed scans, found = %+v, want a and b", n.found)
	}
}

// TestRecordNestedSkillsNeedsItsResult pins the write's guard: what a read
// found is recorded only beside the result event the settlement committed,
// so a result it dropped, its call answered already, leaves nothing.
func TestRecordNestedSkillsNeedsItsResult(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	ctx := context.Background()
	committed := domain.NewID(domain.PrefixEvent)
	if _, err := h.log.Append(ctx, h.sid, []events.NewEvent{{ID: committed, Type: domain.EventAgentToolResult,
		Payload: json.RawMessage(`{"tool_use_id":"sevt_x","content":[{"type":"text","text":"ok"}]}`)}}); err != nil {
		t.Fatal(err)
	}
	inner := []skills.RepoSkill{{Name: "inner", Location: "/workspace/r/x/.claude/skills/inner/SKILL.md"}}
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := recordNestedSkills(ctx, tx, h.sid, map[domain.ID][]skills.RepoSkill{
		committed: inner, domain.NewID(domain.PrefixEvent): inner,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := h.nestedSkillsRecord(t), map[string][]skills.RepoSkill{committed.String(): inner}; !reflect.DeepEqual(got, want) {
		t.Errorf("nested skills = %+v, want only the committed result's", got)
	}
}
