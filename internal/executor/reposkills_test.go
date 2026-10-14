package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/sandboxtest"
)

// repoSkillsRecord reads the session's recorded repository skills; ok is false
// while the scan has not run (the column is NULL).
func (h *harness) repoSkillsRecord(t *testing.T) (skills []repoSkill, ok bool) {
	t.Helper()
	var raw []byte
	if err := h.pool.QueryRow(context.Background(),
		`SELECT repo_skills FROM sessions WHERE id = $1`, h.sid.String()).Scan(&raw); err != nil {
		t.Fatalf("read repo_skills: %v", err)
	}
	if raw == nil {
		return nil, false
	}
	if err := json.Unmarshal(raw, &skills); err != nil {
		t.Fatalf("decode repo_skills %s: %v", raw, err)
	}
	return skills, true
}

// TestRepoSkillsScannedBeforeTheFirstTurn is plan 65 decision 2 and 3 on a real
// checkout: a tool_exec with no tool to run, the shape the brain enqueues when
// it holds a session's first turn, clones the repository, records the skills
// its .claude/skills holds, and wakes the turn. The fixture is recording #23's:
// a skill with no frontmatter and one with no description are listed with an
// empty description, while a lowercase skill.md, a SKILL.md two levels down
// and a hidden directory are not found. Of the two links, a linked skill
// directory is followed and a SKILL.md that is itself a link, which a read
// refuses, is not listed.
func TestRepoSkillsScannedBeforeTheFirstTurn(t *testing.T) {
	fx := newGitFixture(t, map[string]string{
		"README.md":                          "fixture\n",
		".claude/skills/alpha/SKILL.md":      "---\nname: alpha\ndescription: Alpha marker.\n---\nbody\n",
		".claude/skills/broken/SKILL.md":     "# no frontmatter\n",
		".claude/skills/nodesc/SKILL.md":     "---\nname: nodesc\n---\n",
		".claude/skills/lower/skill.md":      "---\ndescription: lowercase\n---\n",
		".claude/skills/deep/inner/SKILL.md": "---\ndescription: two levels\n---\n",
		".claude/skills/.hidden/SKILL.md":    "---\ndescription: hidden\n---\n",
	})
	fx.links(t, map[string]string{
		".claude/skills/viadir":          "alpha",
		".claude/skills/linked/SKILL.md": "../alpha/SKILL.md",
	})
	counter := &countListProvider{}
	h, _ := wrappedRepoHarness(t, func(p sandbox.Provider) sandbox.Provider { counter.Provider = p; return counter })
	h.seedRepoResource(t, "sesrsc_skills", fx.url(), repoMount, "ghp_fixture", nil)

	h.suspend(t)
	h.stepOnce(t)

	got, ok := h.repoSkillsRecord(t)
	if !ok {
		t.Fatal("repo_skills is NULL after the run; the brain would wait on it forever")
	}
	root := repoMount + "/.claude/skills/"
	want := []repoSkill{
		{Name: "alpha", Description: "Alpha marker.", Location: root + "alpha/SKILL.md"},
		{Name: "broken", Description: "", Location: root + "broken/SKILL.md"},
		{Name: "nodesc", Description: "", Location: root + "nodesc/SKILL.md"},
		{Name: "viadir", Description: "Alpha marker.", Location: root + "viadir/SKILL.md"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("repo_skills = %+v\nwant %+v", got, want)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 1 {
		t.Errorf("live model turns after the run = %d, want 1: the held turn must wake", n)
	}

	// Once per session: a recorded scan is not taken again, even when it found
	// nothing and the checkout now holds skills.
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET repo_skills = '[]' WHERE id = $1`, h.sid.String()); err != nil {
		t.Fatal(err)
	}
	h.suspend(t)
	h.stepOnce(t)
	if got, _ := h.repoSkillsRecord(t); len(got) != 0 {
		t.Errorf("a second run overwrote the record: %+v", got)
	}
	if n := counter.listings.Load(); n != 1 {
		t.Errorf("listings = %d over two runs, want 1: a recorded scan is not taken again", n)
	}
}

// TestRepoSkillsRecordedWithoutARepository pins the empty record: a session
// with no repository still stores a scan at its first run, so no brain that
// looks for one can wait on it.
func TestRepoSkillsRecordedWithoutARepository(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	h.suspend(t)
	h.stepOnce(t)
	got, ok := h.repoSkillsRecord(t)
	if !ok || len(got) != 0 {
		t.Errorf("repo_skills = %+v (set %v), want an empty scan", got, ok)
	}
}

// TestRepoSkillsListingFailureStillRecords: a checkout whose listing fails
// costs the session that checkout's skills, not the others' and not its first
// turn — the scan is recorded with what the healthy checkout holds, and the
// turn wakes.
func TestRepoSkillsListingFailureStillRecords(t *testing.T) {
	broken := newGitFixture(t, map[string]string{
		".claude/skills/alpha/SKILL.md": "---\ndescription: Alpha marker.\n---\n",
	})
	healthy := newGitFixture(t, map[string]string{
		".claude/skills/beta/SKILL.md": "---\ndescription: Beta marker.\n---\n",
	})
	h, _ := wrappedRepoHarness(t, func(p sandbox.Provider) sandbox.Provider { return &failListProvider{Provider: p} })
	h.seedRepoResource(t, "sesrsc_broken", broken.url(), repoMount, "ghp_fixture", nil)
	h.seedRepoResource(t, "sesrsc_healthy", healthy.url(), "/workspace/healthy", "ghp_fixture", nil)

	h.suspend(t)
	h.stepOnce(t)

	got, ok := h.repoSkillsRecord(t)
	want := []repoSkill{{Name: "beta", Description: "Beta marker.", Location: "/workspace/healthy/.claude/skills/beta/SKILL.md"}}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("repo_skills = %+v (set %v), want %+v", got, ok, want)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 1 {
		t.Errorf("live model turns = %d, want 1", n)
	}
}

// TestRepoSkillsRecordFailureWakesNothing: a run that cannot store its scan
// stops for a retry with nothing committed, so the held turn stays held rather
// than waking to find no record.
func TestRepoSkillsRecordFailureWakesNothing(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	if _, err := h.pool.Exec(context.Background(),
		`ALTER TABLE sessions ADD CONSTRAINT test_no_repo_skills CHECK (repo_skills IS NULL)`); err != nil {
		t.Fatal(err)
	}
	h.suspend(t)
	h.stepOnce(t)
	if _, ok := h.repoSkillsRecord(t); ok {
		t.Fatal("the record landed despite the constraint")
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 0 {
		t.Errorf("live model turns = %d, want 0: a run that stored no scan must not wake the turn", n)
	}
}

// TestRecordRepoSkillsFirstWins pins the write's two guards: a claimant that
// has lost its lease stores nothing, and of two that hold one in turn, the
// first record stands.
func TestRecordRepoSkillsFirstWins(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	ctx := context.Background()
	h.suspend(t)
	item, err := h.queue.Claim(ctx, queue.ToolExec, time.Minute)
	if err != nil || item == nil {
		t.Fatalf("claim: %v, %v", item, err)
	}

	lost := *item
	lost.Lease = lost.Lease.Add(-time.Hour)
	if err := h.exec.recordRepoSkills(ctx, &lost, []repoSkill{{Name: "stale"}}); err == nil {
		t.Error("a claimant without the lease stored its scan")
	}
	if _, ok := h.repoSkillsRecord(t); ok {
		t.Fatal("a claimant without the lease stored its scan")
	}

	first := []repoSkill{{Name: "first", Location: "/workspace/r/.claude/skills/first/SKILL.md"}}
	if err := h.exec.recordRepoSkills(ctx, item, first); err != nil {
		t.Fatal(err)
	}
	if err := h.exec.recordRepoSkills(ctx, item, []repoSkill{{Name: "second"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.repoSkillsRecord(t); !reflect.DeepEqual(got, first) {
		t.Errorf("repo_skills = %+v, want the first record", got)
	}
}

// countListProvider counts the repository-skills listings its sandboxes run.
type countListProvider struct {
	sandbox.Provider
	listings atomic.Int32
}

func (p *countListProvider) Provision(ctx context.Context, spec sandbox.Spec) (sandbox.Sandbox, error) {
	sb, err := p.Provider.Provision(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &countListSandbox{Sandbox: sb, p: p}, nil
}

type countListSandbox struct {
	sandbox.Sandbox
	p *countListProvider
}

func (s *countListSandbox) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	if strings.Contains(req.Command, "/.claude/skills") {
		s.p.listings.Add(1)
	}
	return s.Sandbox.Exec(ctx, req)
}

// failListProvider fails the repository-skills listing of the checkout at
// repoMount and nothing else.
type failListProvider struct{ sandbox.Provider }

func (p *failListProvider) Provision(ctx context.Context, spec sandbox.Spec) (sandbox.Sandbox, error) {
	sb, err := p.Provider.Provision(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &failListSandbox{Sandbox: sb}, nil
}

type failListSandbox struct{ sandbox.Sandbox }

func (s *failListSandbox) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	if strings.Contains(req.Command, repoMount+"/.claude/skills") {
		return sandbox.ExecResult{}, errors.New("exec stream lost")
	}
	return s.Sandbox.Exec(ctx, req)
}

// TestScanRepoSkillsListsOnlyAnnounceableNames pins the scan's own rules on a
// stub listing a real checkout cannot produce: an image's startup banner is no
// name, since the listing is framed; names come back sorted; one
// holding a control character, a newline among them, or one that is not UTF-8
// is passed over, since it would break the list or name no path; the entry the
// output cap cut short is dropped; and at most maxRepoSkills are kept.
func TestScanRepoSkillsListsOnlyAnnounceableNames(t *testing.T) {
	refs := []repoRef{{ID: "sesrsc_a", Type: "github_repository", URL: "https://github.com/o/r", MountPath: "/workspace/r"}}
	sb := &listingSandbox{stdout: "zeta\x00alpha\x00new\nline\x00bad\xff\x00cut-sho", truncated: true}
	got := (&Executor{}).scanRepoSkills(context.Background(), sb, "sesn_x", refs, func() {})
	want := []repoSkill{
		{Name: "alpha", Description: "From alpha.", Location: "/workspace/r/.claude/skills/alpha/SKILL.md"},
		{Name: "zeta", Description: "From zeta.", Location: "/workspace/r/.claude/skills/zeta/SKILL.md"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("scan = %+v\nwant %+v", got, want)
	}

	var many strings.Builder
	for i := 0; i < maxRepoSkills+5; i++ {
		fmt.Fprintf(&many, "s%04d\x00", i)
	}
	if got := (&Executor{}).scanRepoSkills(context.Background(), &listingSandbox{stdout: many.String()}, "sesn_x", refs, func() {}); len(got) != maxRepoSkills {
		t.Errorf("scan kept %d skills, want the cap of %d", len(got), maxRepoSkills)
	}
}

// listingSandbox answers the skills listing with a fixed output, behind an
// image's startup banner that ends no line, and each SKILL.md read with a
// description naming its directory. A framed listing gets its answer framed,
// so the banner falls outside it; an unframed one gets the banner glued to the
// first name, as a real image would print it.
type listingSandbox struct {
	sandbox.Sandbox
	stdout    string
	truncated bool
}

func (s *listingSandbox) Exec(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	const banner = "welcome to the image "
	res := sandbox.ExecResult{Stdout: s.stdout, StdoutTruncated: s.truncated}
	if _, f, ok := sandboxtest.Unwrap(req.Command); ok {
		res = sandboxtest.Framed(f, res)
	}
	res.Stdout = banner + res.Stdout
	return res, nil
}

func (s *listingSandbox) ReadFile(_ context.Context, p string) ([]byte, error) {
	dir := path.Base(path.Dir(p))
	return []byte("---\ndescription: From " + dir + ".\n---\n"), nil
}
